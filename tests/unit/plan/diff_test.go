package plan_test

import (
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func actions(p plan.Plan) []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Action+" "+s.Object)
	}
	return out
}

func hashes(top plan.Topology) map[string]string {
	out := map[string]string{}
	for _, w := range top.Workloads {
		out[w.Name] = "hash-" + w.Name
	}
	return out
}

func diffInput(top plan.Topology, command state.Command) plan.DiffInput {
	return plan.DiffInput{
		Desired: top, State: state.State{StateVersion: 1}, Command: command,
		Selection: plan.Selection{All: true}, SpecHashes: hashes(top),
	}
}

func TestDiff_FreshUpOrdersProvidersThenJobsThenServices(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	got := actions(plan.Diff(diffInput(top, state.CommandUp)))
	want := []string{
		"compose.image image/primary--store", "compose.volume volume/primary--store--data", "compose.service service/primary--store",
		"compose.image image/migrate", "compose.job job/migrate",
		"compose.image image/api", "compose.volume volume/api--uploads", "compose.service service/api",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("steps =\n%v\nwant\n%v", got, want)
	}
}

func realized(top plan.Topology, running bool) (state.State, plan.Inventory) {
	st := state.State{StateVersion: 1, Realizations: map[string]state.Realization{}, Accepted: &state.Accepted{Revision: 1, Intent: map[string]state.Intent{}}}
	var inv plan.Inventory
	for i, w := range top.Workloads {
		key := plan.RealizationKey(w.Type, w.Name)
		uid, _ := state.NewRealizationID(testkit.Epoch, testkit.NewRandom(key))
		st.Realizations[key] = state.Realization{UID: uid, State: state.RealizationActive, SpecHash: "hash-" + w.Name, Artifact: &state.Artifact{ImageID: "sha256:x"}, Mappings: []state.Mapping{{Kind: "compose.service", Name: "c" + w.Name}}}
		inv.Containers = append(inv.Containers, plan.Container{Object: plan.Object{Name: "c" + w.Name, Realization: uid.String()}, Service: w.Name, Running: running && w.Type == plan.TypeService, ExitCode: 0, SpecHash: "hash-" + w.Name})
		for _, m := range w.Volumes {
			vid, _ := state.NewRealizationID(testkit.Epoch, testkit.NewRandom(m.Name))
			st.Realizations[plan.RealizationKey("volume", m.Name)] = state.Realization{UID: vid, State: state.RealizationActive, Mappings: []state.Mapping{{Kind: "compose.volume", Name: m.Name}}}
			inv.Volumes = append(inv.Volumes, plan.Object{Kind: "volume", Name: m.Name, Realization: vid.String()})
		}
		_ = i
	}
	return st, inv
}

func TestDiff_ARepeatedUpChangesNothing(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	in := diffInput(top, state.CommandUp)
	in.State, in.Inventory = realized(top, true)
	if got := plan.Diff(in); len(got.Steps) != 0 || len(got.Findings) != 0 {
		t.Fatalf("a second up planned %v / %v", actions(got), got.Findings)
	}
}

func TestDiff_AChangedSpecRestartsOnlyThatService(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	in := diffInput(top, state.CommandUp)
	in.State, in.Inventory = realized(top, true)
	in.SpecHashes["api"] = "hash-api-edited"
	if got := actions(plan.Diff(in)); !slices.Equal(got, []string{"compose.service service/api"}) {
		t.Fatalf("steps = %v", got)
	}
}

func TestDiff_NeverStartsAStoppedObjectUnlessSelected(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	in := diffInput(top, state.CommandUp)
	in.State, in.Inventory = realized(top, false)
	in.State.Accepted.Intent["api"] = state.IntentStopped
	in.Selection = plan.Selection{Names: []string{"primary--store"}}

	for _, step := range actions(plan.Diff(in)) {
		if step == "compose.service service/api" {
			t.Fatalf("a later up without the selection restarted the stopped api: %v", actions(plan.Diff(in)))
		}
	}
	in.Selection = plan.Selection{Names: []string{"api"}}
	if got := actions(plan.Diff(in)); !slices.Contains(got, "compose.service service/api") {
		t.Errorf("selecting the stopped api must start it: %v", got)
	}
}

func TestDiff_BlocksOnLabelledObjectsNoRealizationMaps(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	in := diffInput(top, state.CommandUp)
	in.Inventory = plan.Inventory{
		Volumes:    []plan.Object{{Kind: "volume", Name: "api--uploads", Realization: "rz-OLD"}},
		Containers: []plan.Container{{Object: plan.Object{Name: "old-api-1", Realization: "rz-OLD"}, Service: "api"}},
	}
	got := plan.Diff(in)
	if len(got.Findings) != 2 {
		t.Fatalf("findings = %+v", got.Findings)
	}
	for _, f := range got.Findings {
		if f.Problem.Code != state.CodeUnmappedOwnedObject || f.Problem.Category != state.CategoryConflict {
			t.Errorf("finding = %+v", f)
		}
	}
}

func TestDiff_AVanishedMappedVolumeIsLostAndNeverRecreated(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	in := diffInput(top, state.CommandUp)
	in.State, in.Inventory = realized(top, true)
	in.Inventory.Volumes = slices.DeleteFunc(in.Inventory.Volumes, func(o plan.Object) bool { return o.Name == "api--uploads" })

	got := plan.Diff(in)
	if !slices.Equal(got.Lost, []string{"volume/api--uploads"}) {
		t.Errorf("lost = %v", got.Lost)
	}
	var found bool
	for _, f := range got.Findings {
		found = found || f.Problem.Code == state.CodeLostObject
	}
	if !found {
		t.Errorf("findings = %+v, want a lost object", got.Findings)
	}
	for _, step := range actions(got) {
		if step == "compose.volume volume/api--uploads" {
			t.Fatal("the planner recreates a vanished data volume")
		}
	}
}

func TestDiff_StopAndDestroy(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{Instances: []string{"primary"}}, plan.Inputs{})
	in := diffInput(top, state.CommandStop)
	in.State, in.Inventory = realized(top, true)
	if got := actions(plan.Diff(in)); !slices.Equal(got, []string{"compose.stop service/api", "compose.stop service/primary--store"}) {
		t.Errorf("stop steps = %v", got)
	}

	in.Command = state.CommandDestroy
	removes := actions(plan.Diff(in))
	for _, step := range removes {
		if step[:len("compose.volume-delete")] == "compose.volume-delete" {
			t.Errorf("destroy without DeleteData deleted a volume: %v", removes)
		}
	}
	in.DeleteData = true
	if got := actions(plan.Diff(in)); !slices.Contains(got, "compose.volume-delete volume/api--uploads") {
		t.Errorf("destroy with DeleteData did not delete the volume: %v", got)
	}
}

func TestObserved(t *testing.T) {
	t.Parallel()
	inv := plan.Inventory{
		Volumes: []plan.Object{{Name: "vol"}},
		Containers: []plan.Container{
			{Service: "up", Running: true, SpecHash: "h"},
			{Service: "job", Running: false, ExitCode: 0, SpecHash: "h"},
			{Service: "failed", Running: false, ExitCode: 1, SpecHash: "h"},
		},
	}
	h := map[string]string{"up": "h", "job": "h", "failed": "h"}
	tests := []struct {
		name        string
		effect      plan.Effect
		applied, ok bool
	}{
		{"volume exists", plan.CreateVolume{Volume: "vol"}, true, true},
		{"volume absent", plan.CreateVolume{Volume: "other"}, false, true},
		{"delete done", plan.DeleteVolume{Volume: "other"}, true, true},
		{"delete not done", plan.DeleteVolume{Volume: "vol"}, false, true},
		{"service running with the spec", plan.StartService{Workload: "up"}, true, true},
		{"service absent", plan.StartService{Workload: "ghost"}, false, true},
		{"job finished", plan.RunJob{Workload: "job"}, true, true},
		{"job failed", plan.RunJob{Workload: "failed"}, false, true},
		{"stopped", plan.StopService{Workload: "job"}, true, true},
		{"not stopped", plan.StopService{Workload: "up"}, false, true},
		{"removed", plan.RemoveService{Workload: "ghost"}, true, true},
		{"not removed", plan.RemoveService{Workload: "up"}, false, true},
		{"image pull is unknowable", plan.ResolveImage{Workload: "up"}, false, false},
		{"image build is unknowable", plan.BuildImage{Workload: "up"}, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			applied, ok := plan.Observed(test.effect, inv, h)
			if applied != test.applied || ok != test.ok {
				t.Errorf("Observed = %v, %v, want %v, %v", applied, ok, test.applied, test.ok)
			}
		})
	}
}

// boundTop is the delivery project's topology, built with no deliveries: the diff needs only the
// workloads and their roles.
func boundTop(t testing.TB, o testkit.BoundOptions) plan.Topology {
	t.Helper()
	top, err := plan.BuildTopology(testkit.Bound(t, o), env(t), plan.Inputs{}, nil)
	if err != nil {
		t.Fatalf("BuildTopology: %v", err)
	}
	return top
}

func TestDiff_StepsCarryTheirPhaseInDecision0024Order(t *testing.T) {
	t.Parallel()
	top := boundTop(t, testkit.BoundOptions{})
	got := plan.Diff(diffInput(top, state.CommandUp))
	var phases []plan.Phase
	for _, s := range got.Steps {
		phases = append(phases, s.Phase)
	}
	if !slices.IsSorted(phases) || phases[0] != plan.PhaseProvider || phases[len(phases)-1] != plan.PhaseConsumer {
		t.Fatalf("phases = %v for %v: want providers, then jobs, then consumers", phases, actions(got))
	}
	want := []string{
		"compose.image image/db--store", "compose.volume volume/db--store--data", "compose.service service/db--store",
		"compose.image image/migrate", "compose.job job/migrate",
		"compose.image image/app", "compose.service service/app",
	}
	if g := actions(got); !slices.Equal(g, want) {
		t.Errorf("steps =\n%v\nwant\n%v", g, want)
	}
}

func TestDiff_AJobNamedInASelectionOrAGroupButRequiredByNoServiceIsNeverPlanned(t *testing.T) {
	t.Parallel()
	top := boundTop(t, testkit.BoundOptions{NoApp: true})
	for _, sel := range []plan.Selection{{All: true}, {Names: []string{"migrate"}}, {Names: []string{"migrate", "db--store"}}} {
		in := diffInput(top, state.CommandUp)
		in.Selection = sel
		for _, step := range actions(plan.Diff(in)) {
			if step == "compose.job job/migrate" || step == "compose.image image/migrate" {
				t.Errorf("selection %+v planned %s although no service requires the job", sel, step)
			}
		}
	}
}

func TestDiff_AJobRerunsOnlyWhenItsSpecChangedOrARunInvokesIt(t *testing.T) {
	t.Parallel()
	top := boundTop(t, testkit.BoundOptions{})
	in := diffInput(top, state.CommandUp)
	in.State, in.Inventory = realized(top, true)
	if got := actions(plan.Diff(in)); len(got) != 0 {
		t.Fatalf("an unchanged environment planned %v", got)
	}
	in.SpecHashes["migrate"] = "hash-migrate-edited"
	if got := actions(plan.Diff(in)); !slices.Equal(got, []string{"compose.job job/migrate"}) {
		t.Errorf("a changed job planned %v", got)
	}

	run := diffInput(top, state.CommandRun)
	run.State, run.Inventory = realized(top, true)
	run.Invoke, run.Selection = "migrate", plan.Selection{Names: []string{"migrate", "db--store"}}
	if got := actions(plan.Diff(run)); !slices.Equal(got, []string{"compose.job job/migrate"}) {
		t.Errorf("a run of an unchanged job planned %v, want only the job", got)
	}
}

func TestDiff_ARunPlansTheProvidersThenTheJobAndNoConsumer(t *testing.T) {
	t.Parallel()
	top := boundTop(t, testkit.BoundOptions{})
	run := diffInput(top, state.CommandRun)
	run.Invoke, run.Selection = "migrate", plan.Selection{Names: []string{"migrate", "db--store"}}
	want := []string{
		"compose.image image/db--store", "compose.volume volume/db--store--data", "compose.service service/db--store",
		"compose.image image/migrate", "compose.job job/migrate",
	}
	if got := actions(plan.Diff(run)); !slices.Equal(got, want) {
		t.Errorf("run steps =\n%v\nwant\n%v", got, want)
	}
}

func TestDiff_AReissuedCertificateRestartsItsWorkloadAlthoughTheHashIsUnchanged(t *testing.T) {
	t.Parallel()
	top := boundTop(t, testkit.BoundOptions{})
	in := diffInput(top, state.CommandUp)
	in.State, in.Inventory = realized(top, true)
	in.Reissued = map[string]bool{"db--store": true}
	if got := actions(plan.Diff(in)); !slices.Equal(got, []string{"compose.service service/db--store"}) {
		t.Errorf("steps = %v, want only the server restarted", got)
	}
}
