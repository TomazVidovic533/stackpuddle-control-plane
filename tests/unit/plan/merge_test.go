package plan_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func snapshot(t testing.TB, o testkit.ShopOptions) plan.Snapshot {
	t.Helper()
	project := testkit.Shop(t, o)
	top, err := plan.BuildTopology(project, env(t), plan.Inputs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Snapshot{Model: model.Encode(project), Topology: top}
}

func selectNames(t testing.TB, top plan.Topology, refs ...string) plan.Selection {
	t.Helper()
	var parsed []model.ObjectRef
	for _, r := range refs {
		ref, err := model.ParseObjectRef(r)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, ref)
	}
	sel, err := plan.ResolveSelection(top, parsed, false)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func objectKeys(t testing.TB, raw json.RawMessage) []string {
	t.Helper()
	var doc struct {
		Spec struct {
			Objects []struct{ Kind, Key string } `json:"objects"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range doc.Spec.Objects {
		keys = append(keys, o.Kind+"/"+o.Key)
	}
	return keys
}

func TestMerge_FirstRevisionAdmitsOnlyTheSelectionAndItsPrerequisites(t *testing.T) {
	t.Parallel()
	candidate := snapshot(t, testkit.ShopOptions{})
	merged, err := plan.Merge(nil, candidate, selectNames(t, candidate.Topology, "workload/api"))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(merged.Topology); !slices.Equal(got, []string{"api", "migrate"}) {
		t.Fatalf("workloads = %v, want the api and the job it requires", got)
	}
	if got := objectKeys(t, merged.Model); !slices.Equal(got, []string{"Workload/api", "Workload/migrate"}) {
		t.Errorf("model objects = %v", got)
	}
	if len(merged.Topology.Instances) != 0 {
		t.Errorf("instances = %v although no module was selected", merged.Topology.Instances)
	}
}

func TestMerge_UnselectedSourceEditsStayUnapplied(t *testing.T) {
	t.Parallel()
	first := snapshot(t, testkit.ShopOptions{})
	accepted, err := plan.Merge(nil, first, plan.Selection{All: true})
	if err != nil {
		t.Fatal(err)
	}
	// Source edits: the api command changes (selected), a worker appears and the primary note is the
	// same; the worker was never accepted and must stay out.
	edited := snapshot(t, testkit.ShopOptions{APICommand: []string{"sleep", "7200"}, ExtraWorkload: true})
	merged, err := plan.Merge(&accepted, edited, selectNames(t, edited.Topology, "workload/api"))
	if err != nil {
		t.Fatal(err)
	}
	api, _ := merged.Topology.Find("api")
	if !slices.Equal(api.Command, []string{"sleep", "7200"}) {
		t.Errorf("the selected edit was not taken: %v", api.Command)
	}
	if _, ok := merged.Topology.Find("worker"); ok {
		t.Error("an unselected new workload entered the accepted revision")
	}
	if got := objectKeys(t, merged.Model); slices.Contains(got, "Workload/worker") {
		t.Errorf("model objects %v include the unselected worker", got)
	}
	for _, name := range []string{"migrate", "primary--store", "reporting--store"} {
		before, _ := accepted.Topology.Find(name)
		after, ok := merged.Topology.Find(name)
		if !ok || !equalWorkload(before, after) {
			t.Errorf("%s changed although it was not selected", name)
		}
	}
	if string(accepted.Model) == string(merged.Model) {
		t.Error("the model document did not take the selected edit")
	}
}

func equalWorkload(a, b plan.Workload) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestMerge_ASharedDefinitionChangeNeedsTheExpandedScope(t *testing.T) {
	t.Parallel()
	accepted, err := plan.Merge(nil, snapshot(t, testkit.ShopOptions{}), plan.Selection{All: true})
	if err != nil {
		t.Fatal(err)
	}
	edited := snapshot(t, testkit.ShopOptions{StoreImage: "mirror.gcr.io/library/busybox:1.37"})
	sel := selectNames(t, edited.Topology, "module/primary")

	_, err = plan.Merge(&accepted, edited, sel)
	if !errors.Is(err, plan.ErrExpandedScopeRequired) || !strings.Contains(err.Error(), "definitions/kv-store") {
		t.Fatalf("error = %v, want ErrExpandedScopeRequired naming the definition", err)
	}

	sel.ExpandedScope = true
	merged, err := plan.Merge(&accepted, edited, sel)
	if err != nil {
		t.Fatalf("with the expanded scope: %v", err)
	}
	for _, name := range []string{"primary--store", "reporting--store"} {
		w, _ := merged.Topology.Find(name)
		if w.Image != "mirror.gcr.io/library/busybox:1.37" {
			t.Errorf("%s image = %q, want the edited one on every instance", name, w.Image)
		}
	}
}

func TestMerge_ASectionChangeNeedsAllOrTheExpandedScope(t *testing.T) {
	t.Parallel()
	accepted, err := plan.Merge(nil, snapshot(t, testkit.ShopOptions{}), plan.Selection{All: true})
	if err != nil {
		t.Fatal(err)
	}
	edited := snapshot(t, testkit.ShopOptions{ExposePort: 18080})
	sel := selectNames(t, edited.Topology, "workload/api")
	if _, err := plan.Merge(&accepted, edited, sel); !errors.Is(err, plan.ErrExpandedScopeRequired) || !strings.Contains(err.Error(), "environments") {
		t.Fatalf("error = %v, want ErrExpandedScopeRequired naming the environments section", err)
	}
	sel.ExpandedScope = true
	if _, err := plan.Merge(&accepted, edited, sel); err != nil {
		t.Errorf("expanded scope: %v", err)
	}
	if _, err := plan.Merge(&accepted, edited, plan.Selection{All: true}); err != nil {
		t.Errorf("all: %v", err)
	}
}

func TestWithout_RemovesWorkloadsObjectsAndEmptyInstances(t *testing.T) {
	t.Parallel()
	all, err := plan.Merge(nil, snapshot(t, testkit.ShopOptions{}), plan.Selection{All: true})
	if err != nil {
		t.Fatal(err)
	}
	left, err := plan.Without(all, []string{"api", "primary--store"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(left.Topology); !slices.Equal(got, []string{"migrate", "reporting--store"}) {
		t.Errorf("workloads = %v", got)
	}
	if len(left.Topology.Instances) != 1 || left.Topology.Instances[0].Key != "reporting" {
		t.Errorf("instances = %+v", left.Topology.Instances)
	}
	if got := objectKeys(t, left.Model); !slices.Equal(got, []string{"ModuleInstance/reporting", "Workload/migrate"}) {
		t.Errorf("model objects = %v", got)
	}
}
