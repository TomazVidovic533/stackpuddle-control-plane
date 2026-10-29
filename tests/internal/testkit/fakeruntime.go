package testkit

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// FakeRuntime is a registered test double: it stands in for the Docker daemon behind the Compose
// adapter, a boundary this repository does not own, so that the operation tests run with no Docker.
// The double is checked against the real adapter by RuntimeConformance, which tests/unit/operation
// runs on this fake and tests/integration runs on compose.Adapter against Docker; see doubles.md.
//
// It is an in-memory runtime for the operation tests that need no Docker. It keeps a
// labelled inventory, applies each effect to it, and lets a test fail an effect before or after it
// happens, which stands for a crash at that point. It renders with the real renderer.
type FakeRuntime struct {
	mu sync.Mutex
	// EngineID is what Target reports.
	EngineID string
	// TargetErr, if set, makes Target fail.
	TargetErr error
	// BeforeApply, if set, is called before an effect changes anything; an error aborts the effect.
	BeforeApply func(plan.Effect) error
	// AfterApply, if set, is called after an effect changed the inventory; an error is returned
	// to the caller as is, like a crash between the effect and the journal.
	AfterApply func(plan.Effect) error
	// Probe, if set, answers probes; the default answers exit 0 with "ok".
	ProbeFunc func(call port.Call, p module.Probe) (module.ProbeResult, error)
	// RefuseCancelled makes Observe, Probe and Apply refuse a call whose context has ended, as the Compose
	// adapter does ("compose: not started"): a cancelled context stops new children, never a running one.
	RefuseCancelled bool

	containers map[string]plan.Container
	volumes    map[string]plan.Object
	// observeOK counts the Observe calls that still succeed before observeErr is returned.
	observeOK  int
	observeErr error
	// Calls lists every call in order, such as "apply start api" or "observe".
	Calls []string
	// Secrets records, per apply and probe call, the secret references the call carried.
	Secrets [][]string
	// Pulls and Builds count image effects.
	Pulls, Builds int
	// JobExit maps a job workload to the exit code its container ends with; a missing job exits 0.
	JobExit map[string]int
	// DeliveredFiles records, per apply call, the keys of the public material the call carried.
	DeliveredFiles [][]string
}

// NewFakeRuntime returns a fake runtime reaching engine "engine-1".
func NewFakeRuntime() *FakeRuntime {
	return &FakeRuntime{EngineID: "engine-1", containers: map[string]plan.Container{}, volumes: map[string]plan.Object{}}
}

func (f *FakeRuntime) record(format string, args ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, args...))
}

// Target reports the configured engine.
func (f *FakeRuntime) Target(_ context.Context, _ port.Call) (plan.TargetInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("target")
	if f.TargetErr != nil {
		return plan.TargetInfo{}, f.TargetErr
	}
	return plan.TargetInfo{EngineID: f.EngineID, ServerVersion: "fake"}, nil
}

// Render uses the real renderer.
func (*FakeRuntime) Render(scope plan.ScopeNames, top plan.Topology) (plan.Rendered, error) {
	return compose.Render(scope, top)
}

// Observe returns the objects carrying the call's scope label.
func (f *FakeRuntime) Observe(ctx context.Context, call port.Call) (plan.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RefuseCancelled && ctx.Err() != nil {
		return plan.Inventory{}, fmt.Errorf("compose: not started: %w", context.Cause(ctx))
	}
	f.record("observe")
	if f.observeErr != nil {
		if f.observeOK == 0 {
			return plan.Inventory{}, f.observeErr
		}
		f.observeOK--
	}
	var inv plan.Inventory
	for _, c := range f.containers {
		if c.Object.Kind == call.Scope.ScopeUID {
			inv.Containers = append(inv.Containers, c)
		}
	}
	for _, v := range f.volumes {
		if v.Kind == call.Scope.ScopeUID {
			inv.Volumes = append(inv.Volumes, v)
		}
	}
	sort.Slice(inv.Containers, func(i, j int) bool { return inv.Containers[i].Name < inv.Containers[j].Name })
	sort.Slice(inv.Volumes, func(i, j int) bool { return inv.Volumes[i].Name < inv.Volumes[j].Name })
	return inv, nil
}

// Apply changes the in-memory inventory the way Docker would.
func (f *FakeRuntime) Apply(ctx context.Context, call port.Call, e plan.Effect) (plan.Evidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RefuseCancelled && ctx.Err() != nil {
		return plan.Evidence{}, fmt.Errorf("compose: not started: %w", context.Cause(ctx))
	}
	f.record("apply %T %s", e, e.Target())
	f.Secrets = append(f.Secrets, secretRefs(call))
	f.DeliveredFiles = append(f.DeliveredFiles, fileKeys(call))
	if f.BeforeApply != nil {
		if err := f.BeforeApply(e); err != nil {
			return plan.Evidence{}, err
		}
	}
	evidence, err := f.apply(call, e)
	if err != nil {
		return evidence, err
	}
	if f.AfterApply != nil {
		if err := f.AfterApply(e); err != nil {
			return plan.Evidence{}, err
		}
	}
	return evidence, nil
}

func secretRefs(call port.Call) []string {
	var refs []string
	for ref := range call.Secrets {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

func fileKeys(call port.Call) []string {
	var keys []string
	for key := range call.Files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (f *FakeRuntime) apply(call port.Call, e plan.Effect) (plan.Evidence, error) {
	scope := call.Scope
	switch effect := e.(type) {
	case plan.ResolveImage:
		f.Pulls++
		w, _ := call.Topology.Find(effect.Workload)
		return plan.Evidence{Text: "resolved", Artifact: &state.Artifact{Image: strings.SplitN(w.Image, "@", 2)[0] + "@sha256:fake"}}, nil
	case plan.BuildImage:
		f.Builds++
		return plan.Evidence{Text: "built", Artifact: &state.Artifact{ImageID: "sha256:built-" + effect.Workload}}, nil
	case plan.CreateVolume:
		f.volumes[effect.Volume] = plan.Object{Kind: scope.ScopeUID, Name: effect.Volume, ID: compose.VolumeName(scope.ComposeProject, effect.Volume), Realization: scope.Realizations["volume/"+effect.Volume]}
	case plan.DeleteVolume:
		delete(f.volumes, effect.Volume)
	case plan.StartService, plan.RunJob:
		return f.start(call, e)
	case plan.StopService:
		c := f.containers[effect.Workload]
		c.Running = false
		f.containers[effect.Workload] = c
	case plan.RemoveService:
		delete(f.containers, effect.Workload)
	}
	return plan.Evidence{Text: "ok"}, nil
}

func (f *FakeRuntime) start(call port.Call, e plan.Effect) (plan.Evidence, error) {
	scope := call.Scope
	name := e.Target()
	w, _ := call.Topology.Find(name)
	rendered, err := compose.Render(scope, call.Topology)
	if err != nil {
		return plan.Evidence{}, err
	}
	c := plan.Container{
		Object:   plan.Object{Kind: scope.ScopeUID, Name: scope.ComposeProject + "-" + name + "-1", ID: "id-" + name, Realization: scope.Realizations[plan.RealizationKey(w.Type, name)]},
		Service:  name,
		Running:  w.Type == plan.TypeService,
		SpecHash: rendered.SpecHashes[name],
	}
	for _, p := range w.Ports {
		host := p.Host
		if host == 0 {
			host = 40000
		}
		c.Ports = append(c.Ports, state.PortMapping{Container: p.Container, Host: host, HostIP: "127.0.0.1"})
	}
	if w.Type == plan.TypeJob {
		c.ExitCode = f.JobExit[name]
	}
	f.containers[name] = c
	if w.Type == plan.TypeJob {
		return plan.Evidence{Text: "ran", Job: &state.JobResult{ExitCode: c.ExitCode}}, nil
	}
	return plan.Evidence{Text: "started"}, nil
}

// Probe answers a driver probe.
func (f *FakeRuntime) Probe(ctx context.Context, call port.Call, p module.Probe) (module.ProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RefuseCancelled && ctx.Err() != nil {
		return module.ProbeResult{}, fmt.Errorf("compose: not started: %w", context.Cause(ctx))
	}
	f.record("probe %s %s", p.Kind, p.Target)
	f.Secrets = append(f.Secrets, secretRefs(call))
	if _, ok := call.Topology.Find(p.Target); !ok {
		return module.ProbeResult{}, fmt.Errorf("target %q is not part of this scope", p.Target)
	}
	if f.ProbeFunc != nil {
		return f.ProbeFunc(call, p)
	}
	if c, found := f.containers[p.Target]; p.Kind == module.ProbeExec && (!found || !c.Running) {
		// The adapter reports an exec probe of a target that is not running as a result, not an error.
		return module.ProbeResult{ID: p.ID, ExitCode: -1, Stderr: "target " + p.Target + " is not running"}, nil
	}
	return module.ProbeResult{ID: p.ID, ExitCode: 0, Stdout: "ok"}, nil
}

// FailObserveAfter lets the next ok Observe calls succeed and then fails every one with err; a nil err
// clears the fault.
func (f *FakeRuntime) FailObserveAfter(ok int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observeOK, f.observeErr = ok, err
}

// Seed puts objects into the inventory, such as labelled leftovers.
func (f *FakeRuntime) Seed(volumes []plan.Object, containers []plan.Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range volumes {
		f.volumes[v.Name] = v
	}
	for _, c := range containers {
		f.containers[c.Service] = c
	}
}

// RemoveVolume deletes a volume behind the engine's back.
func (f *FakeRuntime) RemoveVolume(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, name)
}

// ExitContainer stops a service's container behind the engine's back, as a crash or a manual stop does.
func (f *FakeRuntime) ExitContainer(service string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[service]
	c.Running = false
	f.containers[service] = c
}

// Snapshot returns the sorted names of containers and volumes, for comparing inventories.
func (f *FakeRuntime) Snapshot() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var parts []string
	for name, c := range f.containers {
		parts = append(parts, fmt.Sprintf("container %s running=%v hash=%s", name, c.Running, c.SpecHash))
	}
	for name := range f.volumes {
		parts = append(parts, "volume "+name)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

// Running reports whether the service has a running container.
func (f *FakeRuntime) Running(service string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers[service].Running
}

// CallCount counts the calls that start with prefix.
func (f *FakeRuntime) CallCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}
