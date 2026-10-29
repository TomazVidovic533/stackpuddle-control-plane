package testkit

import (
	"context"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// ConformanceScope is the scope a runtime conformance run acts on: a disposable one the caller owns
// and cleans up. The suffix keeps two runs apart.
type ConformanceScope struct {
	ScopeUID       string
	ComposeProject string
	DockerContext  string
}

// RuntimeConformance is the one scenario set every implementation of operation.Runtime must satisfy:
// the real Compose adapter (tests/integration, needs Docker) and FakeRuntime (tests/unit/operation).
// It is the contract fixture that keeps the registered double honest: a behaviour the fake gains or
// loses without the adapter doing the same fails here.
//
// The scenario walks one scope through create volume, resolve image, start, probe, stop, remove and
// delete, and checks what Observe reports after each step, the only thing the engine reads back.
func RuntimeConformance(t *testing.T, rt operation.Runtime, cs ConformanceScope) {
	t.Helper()
	call := conformanceCall(t, rt, cs)
	rendered, err := rt.Render(call.Scope, call.Topology)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	conformanceTarget(t, rt, call)
	if inv := observe(t, rt, call); len(inv.Containers) != 0 || len(inv.Volumes) != 0 {
		t.Fatalf("a fresh scope observes %+v, want nothing", inv)
	}

	conformanceCancelled(t, rt, call)

	apply(t, rt, call, plan.CreateVolume{Volume: "data"})
	if inv := observe(t, rt, call); len(inv.Volumes) != 1 || inv.Volumes[0].Name != "data" || len(inv.Containers) != 0 {
		t.Fatalf("after CreateVolume the scope observes %+v", inv)
	}

	evidence := apply(t, rt, call, plan.ResolveImage{Workload: "api"})
	if evidence.Artifact == nil || (evidence.Artifact.Image == "" && evidence.Artifact.ImageID == "") {
		t.Fatalf("ResolveImage recorded no image identity: %+v", evidence)
	}

	apply(t, rt, call, plan.StartService{Workload: "api"})
	api := serviceOf(t, observe(t, rt, call), "api")
	if !api.Running || api.SpecHash != rendered.SpecHashes["api"] || api.Realization != call.Scope.Realizations["service/api"] {
		t.Fatalf("after StartService the container is %+v, want running with spec hash %q", api, rendered.SpecHashes["api"])
	}

	conformanceProbes(t, rt, call)

	apply(t, rt, call, plan.StopService{Workload: "api"})
	if api = serviceOf(t, observe(t, rt, call), "api"); api.Running {
		t.Fatalf("after StopService the container still runs: %+v", api)
	}

	apply(t, rt, call, plan.RemoveService{Workload: "api"})
	conformanceJobs(t, rt, call)
	if inv := observe(t, rt, call); len(inv.Containers) != 0 || len(inv.Volumes) != 1 {
		t.Fatalf("after RemoveService the scope observes %+v, want only the volume", inv)
	}
	apply(t, rt, call, plan.DeleteVolume{Volume: "data"})
	if inv := observe(t, rt, call); len(inv.Containers) != 0 || len(inv.Volumes) != 0 {
		t.Fatalf("after DeleteVolume the scope observes %+v, want nothing", inv)
	}
}

func conformanceCall(t *testing.T, rt operation.Runtime, cs ConformanceScope) port.Call {
	t.Helper()
	random := NewRandom("conformance-" + cs.ScopeUID)
	service, err := state.NewRealizationID(Clock()(), random)
	if err != nil {
		t.Fatal(err)
	}
	volume, err := state.NewRealizationID(Clock()(), random)
	if err != nil {
		t.Fatal(err)
	}
	_ = rt
	return port.Call{
		Scope: plan.ScopeNames{
			ScopeUID: cs.ScopeUID, ComposeProject: cs.ComposeProject, ProjectDir: t.TempDir(),
			Realizations: map[string]string{"service/api": service.String(), "volume/data": volume.String()},
			Artifacts:    map[string]state.Artifact{},
		},
		DockerContext: cs.DockerContext,
		Topology: plan.Topology{Workloads: []plan.Workload{{
			Name: "api", Type: plan.TypeService, Image: BusyboxImage, Command: []string{"sleep", "3600"},
		}}},
		Secrets: map[string]port.SecretValue{},
	}
}

// plainSecret is a secret value for a conformance call.
type plainSecret []byte

// Reveal returns the bytes.
func (p plainSecret) Reveal() []byte { return p }

// ConformanceBadJob is the conformance job that exits with ConformanceBadExit.
const (
	ConformanceBadJob  = "bad-job"
	ConformanceBadExit = 3
)

// conformanceJobs runs jobs through the runtime port. A job's own exit code comes back as a native
// result, zero or not, and is never an error of the engine; files delivered into a job arrive with the
// declared owner and mode (the fake cannot check that and answers with the exit code it was told).
func conformanceJobs(t *testing.T, rt operation.Runtime, call port.Call) {
	t.Helper()
	job := func(name, script string, files ...plan.File) plan.Workload {
		return plan.Workload{Name: name, Type: plan.TypeJob, Image: BusyboxImage, Command: []string{"sh", "-c", script}, Files: files}
	}
	const check = `test "$(cat /run/sp/hello)" = hello && test "$(stat -c %a:%u:%g /run/sp/hello)" = 440:1234:5678 && test "$(cat /run/sp/pw)" = conformance-secret && test "$(stat -c %a /run/sp/pw)" = 400`
	jobs := []struct {
		workload plan.Workload
		exit     int
	}{
		{job("ok-job", "exit 0"), 0},
		{job(ConformanceBadJob, "exit 3"), ConformanceBadExit},
		{job("files-job", check,
			plan.File{Path: "/run/sp/hello", Mode: 0o440, Owner: "1234:5678", Source: plan.Source{Kind: plan.SourceText, Text: "hello"}},
			plan.File{Path: "/run/sp/pw", Mode: 0o400, Source: plan.Source{Kind: plan.SourceSecret, Ref: "conformance/pw"}}), 0},
	}
	call.Secrets = map[string]port.SecretValue{"conformance/pw": plainSecret("conformance-secret")}
	for _, j := range jobs {
		call.Topology.Workloads = append(call.Topology.Workloads, j.workload)
	}
	for _, j := range jobs {
		evidence, err := rt.Apply(t.Context(), call, plan.RunJob{Workload: j.workload.Name})
		if err != nil {
			t.Fatalf("RunJob %s: %v: a job's own exit code is a result, not an error", j.workload.Name, err)
		}
		if evidence.Job == nil || evidence.Job.ExitCode != j.exit {
			t.Errorf("RunJob %s = %+v, want the native result with exit code %d", j.workload.Name, evidence.Job, j.exit)
		}
		if got := serviceOf(t, observe(t, rt, call), j.workload.Name); got.Running || got.ExitCode != j.exit {
			t.Errorf("after RunJob %s the container is %+v, want exited with %d", j.workload.Name, got, j.exit)
		}
	}
	for _, j := range jobs {
		apply(t, rt, call, plan.RemoveService{Workload: j.workload.Name})
	}
}

func conformanceTarget(t *testing.T, rt operation.Runtime, call port.Call) {
	t.Helper()
	first, err := rt.Target(t.Context(), call)
	if err != nil || first.EngineID == "" {
		t.Fatalf("Target = %+v, %v", first, err)
	}
	if again, err := rt.Target(t.Context(), call); err != nil || again.EngineID != first.EngineID {
		t.Fatalf("the engine identity is not stable: %+v then %+v (%v)", first, again, err)
	}
}

// conformanceCancelled checks that a call made with an ended context fails and starts no child: nothing
// is created, and a later call with a live context still sees a fresh scope. A fake must be built to
// refuse cancelled calls (FakeRuntime.RefuseCancelled) to pass.
func conformanceCancelled(t *testing.T, rt operation.Runtime, call port.Call) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := rt.Observe(ctx, call); err == nil {
		t.Error("Observe with a cancelled context succeeded")
	}
	if _, err := rt.Apply(ctx, call, plan.CreateVolume{Volume: "data"}); err == nil {
		t.Error("Apply with a cancelled context succeeded")
	}
	if _, err := rt.Probe(ctx, call, module.Probe{ID: "ok", Kind: module.ProbeExec, Target: "api", Argv: []string{"true"}}); err == nil {
		t.Error("Probe with a cancelled context succeeded")
	}
	if inv := observe(t, rt, call); len(inv.Containers) != 0 || len(inv.Volumes) != 0 {
		t.Fatalf("a cancelled call changed the scope: %+v", inv)
	}
}

func conformanceProbes(t *testing.T, rt operation.Runtime, call port.Call) {
	t.Helper()
	ok, err := rt.Probe(t.Context(), call, module.Probe{ID: "ok", Kind: module.ProbeExec, Target: "api", Argv: []string{"true"}})
	if err != nil || ok.ID != "ok" || ok.ExitCode != 0 {
		t.Fatalf("a probe of a running target = %+v, %v", ok, err)
	}
	if _, err := rt.Probe(t.Context(), call, module.Probe{ID: "away", Kind: module.ProbeExec, Target: "elsewhere", Argv: []string{"true"}}); err == nil {
		t.Fatal("a probe of a target outside the topology must fail")
	}
}

func observe(t *testing.T, rt operation.Runtime, call port.Call) plan.Inventory {
	t.Helper()
	inv, err := rt.Observe(t.Context(), call)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return inv
}

func apply(t *testing.T, rt operation.Runtime, call port.Call, effect plan.Effect) plan.Evidence {
	t.Helper()
	evidence, err := rt.Apply(t.Context(), call, effect)
	if err != nil {
		t.Fatalf("Apply %T: %v", effect, err)
	}
	return evidence
}

func serviceOf(t *testing.T, inv plan.Inventory, service string) plan.Container {
	t.Helper()
	i := slices.IndexFunc(inv.Containers, func(c plan.Container) bool { return c.Service == service })
	if i < 0 {
		t.Fatalf("the scope observes no container of %s: %+v", service, inv)
	}
	return inv.Containers[i]
}
