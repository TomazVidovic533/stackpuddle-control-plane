//go:build integration

package integration_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// callOf builds the runtime call of a scope that is already up, the way the engine would.
func (s *stack) callOf(o testkit.ShopOptions) port.Call {
	s.t.Helper()
	env, err := model.ParseEnvironmentName("local")
	if err != nil {
		s.t.Fatal(err)
	}
	top, err := plan.BuildTopology(testkit.Shop(s.t, s.opts(o)), env, plan.Inputs{}, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	scope, st := s.scopeFile(), s.stateFile()
	names := plan.ScopeNames{ScopeUID: scope.ScopeUID.String(), ComposeProject: scope.ComposeProject, ProjectDir: s.projectDir(o), Realizations: map[string]string{}}
	for key, r := range st.Realizations {
		names.Realizations[key] = r.UID.String()
	}
	return port.Call{Scope: names, DockerContext: "default", Topology: top}
}

func (s *stack) adapter() *compose.Adapter {
	s.t.Helper()
	core, err := testkit.NewCore(testkit.CoreOptions{Env: s.env})
	if err != nil {
		s.t.Fatal(err)
	}
	return core.Adapter
}

func TestExecAndOneShotReachOnlyTheirOwnScope(t *testing.T) {
	first, second := newStack(t), newStack(t)
	aOpts := testkit.ShopOptions{Instances: []string{}, NoJob: true, APIEnv: map[string]string{"WHO": "first"}}
	bOpts := testkit.ShopOptions{Instances: []string{}, NoJob: true, APIEnv: map[string]string{"WHO": "second"}, ExtraWorkload: true}
	first.mustUp(aOpts)
	second.mustUp(bOpts)
	a, b := first.callOf(aOpts), second.callOf(bOpts)
	adapter := first.adapter()
	ctx := context.Background()

	probe := func(call port.Call, p module.Probe) module.ProbeResult {
		t.Helper()
		res, err := adapter.Probe(ctx, call, p)
		if err != nil {
			t.Fatalf("probe %s: %v", p.ID, err)
		}
		return res
	}
	exec := func(id, target string, argv ...string) module.Probe {
		return module.Probe{ID: id, Kind: module.ProbeExec, Target: target, Argv: argv}
	}
	oneShot := func(id string, argv ...string) module.Probe {
		return module.Probe{ID: id, Kind: module.ProbeOneShot, Target: "api", Argv: argv}
	}

	// Two scopes both have a service named api: an exec through each call reaches its own container.
	if got := strings.TrimSpace(probe(a, exec("who", "api", "printenv", "WHO")).Stdout); got != "first" {
		t.Errorf("exec through the first scope reached %q", got)
	}
	if got := strings.TrimSpace(probe(b, exec("who", "api", "printenv", "WHO")).Stdout); got != "second" {
		t.Errorf("exec through the second scope reached %q", got)
	}
	// A service the call's topology does not hold is refused, not looked up.
	if _, err := adapter.Probe(ctx, a, exec("x", "worker", "true")); err == nil {
		t.Error("an exec into a target outside the scope's topology was accepted")
	}
	// The second scope's worker is not reachable through the first scope's label, even when the first
	// scope's topology names a worker.
	withWorker := a
	withWorker.Topology.Workloads = append(append([]plan.Workload(nil), a.Topology.Workloads...), plan.Workload{Name: "worker", Type: plan.TypeService, Image: testkit.BusyboxImage})
	if res := probe(withWorker, exec("w", "worker", "true")); res.ExitCode == 0 {
		t.Errorf("the first scope reached the second scope's worker container: %+v", res)
	}
	// A one-shot container runs on its own scope's network: it resolves its own api and not a service
	// that exists only in the other scope.
	if res := probe(a, oneShot("own", "nslookup", "api")); res.ExitCode != 0 {
		t.Errorf("the one-shot cannot see its own scope's api: %+v", res)
	}
	if res := probe(a, oneShot("other", "nslookup", "worker")); res.ExitCode == 0 {
		t.Errorf("the one-shot of the first scope resolved a service of the second: %+v", res)
	}
	if res := probe(b, oneShot("other", "nslookup", "worker")); res.ExitCode != 0 {
		t.Errorf("the one-shot of the second scope cannot see its own worker: %+v", res)
	}
	if left := docker(t, "ps", "-a", "--filter", "label=io.stackpuddle.scope="+a.Scope.ScopeUID, "--format", "{{.Names}}"); strings.Count(left, "\n") != 0 {
		t.Errorf("a one-shot container was left behind: %q", left)
	}
}

func TestABuildRecordsAnImageIDAndARepeatUpNeitherRebuildsNorPullsAgain(t *testing.T) {
	s := newStack(t)
	o := testkit.ShopOptions{Instances: []string{}, NoJob: true, BuildAPI: true}
	first := s.mustUp(o)
	if !hasStep(first, "image/api") {
		t.Fatalf("the first up did not build: %v", first.Steps())
	}
	recorded := s.stateFile().Realizations["service/api"].Artifact
	if recorded == nil || !strings.HasPrefix(recorded.ImageID, "sha256:") {
		t.Fatalf("recorded artifact = %+v", recorded)
	}
	created := docker(t, "image", "inspect", "--format", "{{.Id}} {{.Created}}", recorded.ImageID)

	edited := o
	edited.APIEnv = map[string]string{"EDIT": "1"}
	second := s.mustUp(edited)
	if hasStep(second, "image/api") {
		t.Errorf("a repeat up resolved or built the image again: %v", second.Steps())
	}
	if got := s.stateFile().Realizations["service/api"].Artifact; got == nil || got.ImageID != recorded.ImageID {
		t.Errorf("the recorded image changed: %+v", got)
	}
	if after := docker(t, "image", "inspect", "--format", "{{.Id}} {{.Created}}", recorded.ImageID); after != created {
		t.Errorf("the image was rebuilt: %q -> %q", created, after)
	}
	info := inspectContainer(t, s.containers()[0])
	if !info.State.Running || !strings.Contains(strings.Join(info.Config.Env, " "), "EDIT=1") {
		t.Errorf("the second up did not recreate the service from the recorded image: %+v", info.Config.Env)
	}
}

func TestADeclaredTagIsResolvedToADigestOnceAndNotPulledAgain(t *testing.T) {
	s := newStack(t)
	o := testkit.ShopOptions{Instances: []string{}, NoJob: true, APIImage: "mirror.gcr.io/library/busybox:1.37"}
	first := s.mustUp(o)
	if !hasStep(first, "image/api") {
		t.Fatalf("the first up did not resolve the image: %v", first.Steps())
	}
	recorded := s.stateFile().Realizations["service/api"].Artifact
	if recorded == nil || !strings.Contains(recorded.Image, "@sha256:") {
		t.Fatalf("recorded artifact = %+v", recorded)
	}
	o.APIEnv = map[string]string{"EDIT": "1"}
	if second := s.mustUp(o); hasStep(second, "image/api") {
		t.Errorf("a repeat up pulled again: %v", second.Steps())
	}
}

func hasStep(op engine.Operation, object string) bool {
	for _, step := range op.Steps() {
		if step.Object == object {
			return true
		}
	}
	return false
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestOnlyDeclaredPortsArePublishedAndOnlyOnLoopback(t *testing.T) {
	s := newStack(t)
	port := freePort(t)
	s.mustUp(testkit.ShopOptions{NoJob: true, ExposePort: port})
	var published int
	for _, name := range s.containers() {
		for spec, bindings := range inspectContainer(t, name).NetworkSettings.Ports {
			for _, b := range bindings {
				published++
				if b.HostIp != "127.0.0.1" || b.HostPort != strconv.Itoa(port) || spec != "8080/tcp" || !strings.Contains(name, "-api-") {
					t.Errorf("%s publishes %s on %s:%s", name, spec, b.HostIp, b.HostPort)
				}
			}
		}
	}
	if published != 1 {
		t.Errorf("%d ports are published, want exactly the declared one", published)
	}
}

func TestARequiredEnforcementTheAdapterCannotGiveFailsBeforeAnyMutation(t *testing.T) {
	s := newStack(t)
	_, err := s.up(testkit.ShopOptions{Instances: []string{}, NoJob: true, RequireCapability: "enforce.egress-policy/1"})
	if code := codeOf(t, err); code != engine.CodeCapabilityUnsupported {
		t.Fatalf("error = %v (%v)", code, err)
	}
	if _, statErr := os.Stat(filepath.Join(s.root, "environments")); !os.IsNotExist(statErr) {
		t.Error("state was created before the refusal")
	}
	if left := docker(t, "ps", "-a", "--filter", "name="+s.name, "--format", "{{.Names}}"); left != "" {
		t.Errorf("containers exist: %s", left)
	}
}

func TestADryRunLeavesTheInventoryAndStateAloneAndReportsTheGeneration(t *testing.T) {
	s := newStack(t)
	o := testkit.ShopOptions{Instances: []string{}, NoJob: true}
	s.mustUp(o)
	snapshot := func() string {
		return docker(t, "ps", "-a", "--filter", "name="+s.name, "--format", "{{.Names}} {{.ID}} {{.Status}}") + "|" +
			docker(t, "volume", "ls", "-q", "--filter", "name="+s.name)
	}
	before := snapshot()
	stateBytes, _ := os.ReadFile(filepath.Join(s.envDir(), "state.json"))
	inspection, _ := s.eng.Inspect(t.Context(), engine.InspectEnvironment(s.scope()))
	view, _ := inspection.Environment()

	edited := o
	edited.APIEnv = map[string]string{"EDIT": "1"}
	result, err := s.up(edited, func(r *engine.SubmitRequest) { r.DryRun = true })
	if err != nil {
		t.Fatal(err)
	}
	preview := result.Preview()
	if preview.BasedOnGeneration() != view.Generation() || len(preview.Actions()) != 1 || preview.Actions()[0].Object != "service/api" {
		t.Errorf("preview = generation %d, %+v (state generation %d)", preview.BasedOnGeneration(), preview.Actions(), view.Generation())
	}
	if after := snapshot(); after != before {
		t.Errorf("the Docker inventory changed:\n%s\n->\n%s", before, after)
	}
	if after, _ := os.ReadFile(filepath.Join(s.envDir(), "state.json")); string(after) != string(stateBytes) {
		t.Error("a dry run wrote state")
	}
}

func TestDestroyWithDataRemovesTheObjectsAndKeepsTheFolderScopeAndLock(t *testing.T) {
	s := newStack(t)
	o := testkit.ShopOptions{Instances: []string{"primary"}, NoJob: true}
	s.mustUp(o)
	op, err := s.submit(s.request(o, engine.CommandDestroy), func(r *engine.SubmitRequest) { r.DeleteData = true })
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if done, ok := op.Operation(); !ok || done.Status() != engine.StatusSucceeded {
		t.Fatalf("destroy operation = %+v", done)
	}
	if got := s.containers(); len(got) != 0 {
		t.Errorf("containers remain: %v", got)
	}
	if got := s.volumes(); len(got) != 0 {
		t.Errorf("volumes remain: %v", got)
	}
	for _, name := range []string{"scope.json", "env.flock", "state.json"} {
		if _, err := os.Stat(filepath.Join(s.envDir(), name)); err != nil {
			t.Errorf("destroy removed %s: %v", name, err)
		}
	}
}
