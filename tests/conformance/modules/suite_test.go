//go:build integration

// Package modules_test is the driver conformance suite (DECISIONS D29). It runs one set of properties
// over every entry of the engine's catalog; a driver brings a fixture, never its own copy of a
// property. A catalog entry without a fixture fails by name. PostgreSQL is the first fixture.
package modules_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// fixture is everything the suite needs to know about one module.
type fixture struct {
	// prepare makes the module's runtime image present, failing by name otherwise.
	prepare func(testing.TB)
	// project returns a project named name with one instance of the module.
	project func(name string) (model.Project, error)
	// instance is the key of that instance.
	instance string
	// workloads are the module's workloads of the instance, as the runtime adapter may reach them.
	workloads []plan.Workload
	// options are the instance's effective options.
	options json.RawMessage
	// absent, denied and unknown are the results the first probe of Observe receives in each case.
	absent, denied, unknown module.ProbeResult
	// status reads the module's own vocabulary out of an observation.
	status func(module.Observation) string
	// wantStatus is what status must say in each case.
	wantAbsent, wantDenied, wantUnknown string
	// killAfterEffect wraps a driver so the process dies with SIGKILL after a native effect and before
	// the engine journals its outcome.
	killAfterEffect func(module.Driver) module.Driver
	// snapshot lists one line per object the module provisioned, carrying its identity and credential,
	// so a retry that recreates or rotates one changes its line and one that duplicates adds a line.
	snapshot func(r *testkit.Rig) []string
	// secretRefs are the credentials the module generates.
	secretRefs []string
	// export is an Export of the module and parameters its binding accepts.
	export     string
	parameters json.RawMessage
}

func fixtures() map[string]fixture {
	return map[string]fixture{"postgres": postgresFixture()}
}

const namePrefix = "sp-m01-050-"

func TestMain(m *testing.M) {
	testkit.Dispatch(map[string]testkit.Helper{"submit": helperSubmit})
	os.Exit(m.Run())
}

// TestConformance runs the properties over every catalog entry.
func TestConformance(t *testing.T) {
	entries := modules.Catalog()
	if len(entries) == 0 {
		t.Fatal("the catalog is empty; the suite has nothing to run over")
	}
	known := fixtures()
	for _, driver := range entries {
		name := driver.Describe().Module
		t.Run(name, func(t *testing.T) {
			fx, ok := known[name]
			if !ok {
				t.Fatalf("catalog entry %q has no conformance fixture; add one to tests/conformance/modules", name)
			}
			runProperties(t, driver, fx)
		})
	}
}

func runProperties(t *testing.T, driver module.Driver, fx fixture) {
	t.Run("Observe tells absent, denied and unknown apart", func(t *testing.T) { observeProperty(t, driver, fx) })
	t.Run("Plan leaves the Docker inventory unchanged", func(t *testing.T) { planProperty(t, driver, fx) })
	t.Run("Apply after kill -9 duplicates nothing and rotates no credential", func(t *testing.T) { killProperty(t, driver, fx) })
	t.Run("Verify fails on a stopped runtime", func(t *testing.T) { verifyProperty(t, driver, fx) })
	t.Run("Bind refuses unknown parameters and makes no probe", func(t *testing.T) { bindProperty(t, driver, fx) })
	t.Run("Delete is refused without owned intent", func(t *testing.T) { deleteProperty(t, driver, fx) })
}

// drive runs one driver method's request and probe loop with a scripted prober.
func drive(driver module.Driver, inst module.Instance, answer func(module.Probe) module.ProbeResult) (module.Observation, error) {
	var results []module.ProbeResult
	for range module.MaxProbeRounds {
		resp, err := driver.Observe(context.Background(), module.ObserveRequest{Instance: inst, Results: results})
		if err != nil {
			return module.Observation{}, err
		}
		if resp.Observation != nil {
			return *resp.Observation, nil
		}
		results = nil
		for _, p := range resp.Probes {
			res := answer(p)
			res.ID = p.ID
			results = append(results, res)
		}
	}
	return module.Observation{}, errors.New("the driver asked for more than MaxProbeRounds rounds")
}

// bindProperty holds every driver's Bind to the contract: values that are exactly one kind each and
// never a probe, an error for a parameter the driver does not define and for an export it does not
// offer, and the same answer every time, because it has no effect.
func bindProperty(t *testing.T, driver module.Driver, fx fixture) {
	inst := module.Instance{Key: fx.instance, Definition: driver.Describe().Module, Version: "0.0.0", Options: fx.options, Scope: "sc-conformance"}
	ctx := context.Background()
	first, err := driver.Bind(ctx, module.BindRequest{Instance: inst, Export: fx.export, Parameters: fx.parameters})
	if err != nil {
		t.Fatalf("Bind with the fixture's parameters: %v", err)
	}
	if len(first.Values) == 0 {
		t.Fatal("Bind bound no value")
	}
	for _, v := range first.Values {
		kinds := 0
		for _, set := range []bool{v.Text != "", v.SecretRef != "", v.Material != ""} {
			if set {
				kinds++
			}
		}
		if kinds != 1 {
			t.Errorf("value %q has %d kinds set, want exactly one", v.Name, kinds)
		}
	}
	again, err := driver.Bind(ctx, module.BindRequest{Instance: inst, Export: fx.export, Parameters: fx.parameters})
	if err != nil || !slices.Equal(first.Values, again.Values) {
		t.Errorf("a second Bind answered %+v, %v: it must be repeatable", again, err)
	}
	var unknown map[string]any
	if err := json.Unmarshal(fx.parameters, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["notAParameter"] = true
	raw, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Bind(ctx, module.BindRequest{Instance: inst, Export: fx.export, Parameters: raw}); err == nil {
		t.Error("Bind accepted a parameter the module does not define")
	}
	if _, err := driver.Bind(ctx, module.BindRequest{Instance: inst, Export: "no-such-export", Parameters: fx.parameters}); err == nil {
		t.Error("Bind answered for an export the module does not offer")
	}
}

func observeProperty(t *testing.T, driver module.Driver, fx fixture) {
	inst := module.Instance{Key: fx.instance, Definition: driver.Describe().Module, Version: "0.0.0", Options: fx.options, Scope: "sc-conformance"}
	seen := map[string]string{}
	for _, c := range []struct {
		name   string
		result module.ProbeResult
		want   string
	}{
		{"absent", fx.absent, fx.wantAbsent}, {"denied", fx.denied, fx.wantDenied}, {"unknown", fx.unknown, fx.wantUnknown},
	} {
		obs, err := drive(driver, inst, func(module.Probe) module.ProbeResult { return c.result })
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := fx.status(obs)
		if got != c.want {
			t.Errorf("%s: status %q, want %q", c.name, got, c.want)
		}
		if obs.Present {
			t.Errorf("%s: an unusable observation reports Present", c.name)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s are reported with the same status %q", c.name, other, got)
		}
		seen[got] = c.name
	}
}

func planProperty(t *testing.T, driver module.Driver, fx fixture) {
	fx.prepare(t)
	r := testkit.NewRig(t, namePrefix, fx.project, driver)
	before := r.Inventory()
	if _, err := r.DryRun(); err != nil {
		t.Fatalf("dry run on a fresh scope: %v", err)
	}
	if after := r.Inventory(); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("a dry run on a fresh scope changed the inventory:\nbefore %v\nafter  %v", before, after)
	}
	r.MustUp()
	before = r.Inventory()
	if _, err := r.DryRun(); err != nil {
		t.Fatalf("dry run on a running scope: %v", err)
	}
	if after := r.Inventory(); strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("a dry run on a running scope changed the inventory:\nbefore %v\nafter  %v", before, after)
	}
}

// scenario is what the helper process runs.
type scenario struct {
	Env    map[string]string
	Name   string
	Module string
}

func helperSubmit(args []string) int {
	var sc scenario
	if err := json.Unmarshal([]byte(args[0]), &sc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fx, ok := fixtures()[sc.Module]
	if !ok {
		fmt.Fprintln(os.Stderr, "no fixture for", sc.Module)
		return 2
	}
	var driver module.Driver
	for _, d := range modules.Catalog() {
		if d.Describe().Module == sc.Module {
			driver = fx.killAfterEffect(d)
		}
	}
	core, err := testkit.NewCore(testkit.CoreOptions{Env: sc.Env, Catalog: []module.Driver{driver}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	loaded, err := fx.project(sc.Name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	name, _ := model.ParseProjectName(sc.Name)
	environment, _ := model.ParseEnvironmentName("local")
	_, err = core.Engine.Submit(context.Background(), operation.Request{
		Scope: operation.Scope{Project: name, Environment: environment}, Project: loaded, ProjectDir: sc.Env["STACKPUDDLE_STATE_DIR"],
		Command: state.CommandUp, All: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func killProperty(t *testing.T, driver module.Driver, fx fixture) {
	fx.prepare(t)
	r := testkit.NewRig(t, namePrefix, fx.project, driver)
	data, err := json.Marshal(scenario{Env: r.Env, Name: r.Name, Module: driver.Describe().Module})
	if err != nil {
		t.Fatal(err)
	}
	out, runErr := testkit.Command("submit", string(data)).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("the helper was not killed: %v\n%s", runErr, out)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the helper did not die from SIGKILL: %v\n%s", runErr, out)
	}
	open := r.StateFile().Operation
	if open == nil {
		t.Fatal("the dead process left no open operation")
	}
	snapshot, inventory := fx.snapshot(r), r.Inventory()
	versions := map[string]string{}
	for _, ref := range fx.secretRefs {
		versions[ref] = r.CredentialVersion(ref)
	}
	// The engine holds a module step it cannot settle by itself; the caller cancels, and the retry observes.
	if _, err := r.Up(); err == nil {
		t.Fatal("the rerun after the kill was not held for the caller")
	}
	if err := r.Core.Engine.Cancel(t.Context(), r.Scope(), open.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	r.MustUp()
	after := fx.snapshot(r)
	for _, line := range snapshot {
		if !slices.Contains(after, line) {
			t.Errorf("the retry changed or removed what the killed run provisioned: %q", line)
		}
	}
	if unique := slices.Compact(slices.Sorted(slices.Values(after))); len(unique) != len(after) {
		t.Errorf("the retry duplicated an object: %v", after)
	}
	for ref, version := range versions {
		if got := r.CredentialVersion(ref); got != version {
			t.Errorf("credential %s rotated from %s to %s", ref, version, got)
		}
	}
	if after := r.Inventory(); len(after) != len(inventory) {
		t.Errorf("the retry changed the number of runtime objects: %v -> %v", inventory, after)
	}
}

// realProber runs probes through the real adapter against the rig's scope, as the engine would.
func realProber(t *testing.T, r *testkit.Rig, fx fixture) func(module.Probe) module.ProbeResult {
	scope := r.ScopeFile()
	call := port.Call{
		Scope:         plan.ScopeNames{ScopeUID: scope.ScopeUID.String(), ComposeProject: scope.ComposeProject, ProjectDir: r.Root},
		DockerContext: "default", Topology: plan.Topology{Workloads: fx.workloads},
	}
	return func(p module.Probe) module.ProbeResult {
		res, err := r.Core.Adapter.Probe(t.Context(), call, p)
		if err != nil {
			t.Fatalf("probe %s: %v", p.ID, err)
		}
		return res
	}
}

func verify(t *testing.T, driver module.Driver, inst module.Instance, answer func(module.Probe) module.ProbeResult) module.Verification {
	var results []module.ProbeResult
	for range module.MaxProbeRounds {
		resp, err := driver.Verify(t.Context(), module.VerifyRequest{Instance: inst, Results: results})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if resp.Verification != nil {
			return *resp.Verification
		}
		results = nil
		for _, p := range resp.Probes {
			res := answer(p)
			res.ID = p.ID
			results = append(results, res)
		}
	}
	t.Fatal("Verify asked for more than MaxProbeRounds rounds")
	return module.Verification{}
}

func verifyProperty(t *testing.T, driver module.Driver, fx fixture) {
	fx.prepare(t)
	r := testkit.NewRig(t, namePrefix, fx.project, driver)
	r.MustUp()
	inst := module.Instance{Key: fx.instance, Definition: driver.Describe().Module, Version: "0.0.0", Options: fx.options, Scope: r.ScopeFile().ScopeUID.String()}
	answer := realProber(t, r, fx)
	if v := verify(t, driver, inst, answer); !v.OK {
		t.Fatalf("a running, provisioned module failed verification: %v", v.Problems)
	}
	for _, id := range testkit.Lines(testkit.Docker(t, "ps", "-q", "--filter", "label=io.stackpuddle.scope="+r.ScopeFile().ScopeUID.String())) {
		testkit.Docker(t, "stop", "-t", "5", id)
	}
	if v := verify(t, driver, inst, answer); v.OK {
		t.Error("a stopped runtime verified")
	}
}

// destructive wraps a driver so its plan carries a delete the module never described.
type destructive struct{ module.Driver }

func (d destructive) Plan(ctx context.Context, r module.PlanRequest) (module.PlanResponse, error) {
	resp, err := d.Driver.Plan(ctx, r)
	if len(resp.Steps) > 0 {
		step := resp.Steps[len(resp.Steps)-1]
		step.Destructive = true
		resp.Steps = append(resp.Steps, step)
	}
	return resp, err
}

func deleteProperty(t *testing.T, driver module.Driver, fx fixture) {
	desc := driver.Describe()
	if desc.Deletes {
		t.Fatalf("module %s describes the delete capability; a fixture must supply the owned-intent refusal check before it may", desc.Module)
	}
	fx.prepare(t)
	r := testkit.NewRig(t, namePrefix, fx.project, destructive{driver})
	_, err := r.DryRun()
	if err == nil || !strings.Contains(err.Error(), "delete capability") {
		t.Errorf("a destructive step from a module without the delete capability was not refused: %v", err)
	}
	if inv := r.Inventory(); len(inv) != 0 {
		t.Errorf("the refused plan left runtime objects: %v", inv)
	}
}
