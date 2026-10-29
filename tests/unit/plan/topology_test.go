package plan_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func env(t testing.TB) model.EnvironmentName {
	t.Helper()
	name, err := model.ParseEnvironmentName("local")
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func build(t testing.TB, o testkit.ShopOptions, inputs plan.Inputs) plan.Topology {
	t.Helper()
	top, err := plan.BuildTopology(testkit.Shop(t, o), env(t), inputs, nil)
	if err != nil {
		t.Fatalf("BuildTopology: %v", err)
	}
	return top
}

func names(top plan.Topology) []string {
	out := make([]string, 0, len(top.Workloads))
	for _, w := range top.Workloads {
		out = append(out, w.Name)
	}
	return out
}

func TestBuildTopology_TwoInstancesOfOneDefinitionNeverCollide(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{}, plan.Inputs{})
	want := []string{"api", "migrate", "primary--store", "reporting--store"}
	if got := names(top); !slices.Equal(got, want) {
		t.Fatalf("workload names = %v, want %v", got, want)
	}
	volumes := map[string]bool{}
	for _, w := range top.Workloads {
		for _, v := range w.Volumes {
			if volumes[v.Name] {
				t.Errorf("volume name %q is used twice", v.Name)
			}
			volumes[v.Name] = true
		}
		if len(w.Name) > 62 {
			t.Errorf("name %q is longer than a DNS label", w.Name)
		}
	}
	for _, want := range []string{"api--uploads", "primary--store--data", "reporting--store--data"} {
		if !volumes[want] {
			t.Errorf("volume %q is missing from %v", want, volumes)
		}
	}
	if len(top.Instances) != 2 || top.Instances[0].Definition != "kv-store" {
		t.Errorf("instances = %+v", top.Instances)
	}
}

func TestBuildTopology_PrerequisitesAndImages(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{}, plan.Inputs{})
	api, ok := top.Find("api")
	if !ok || !slices.Equal(api.Prerequisites, []string{"migrate"}) || api.Image != testkit.BusyboxImage {
		t.Fatalf("api = %+v", api)
	}
	migrate, _ := top.Find("migrate")
	if migrate.Type != plan.TypeJob {
		t.Errorf("migrate type = %q", migrate.Type)
	}
}

func TestBuildTopology_PublishesOnlyDeclaredExposures(t *testing.T) {
	t.Parallel()
	without := build(t, testkit.ShopOptions{}, plan.Inputs{})
	for _, w := range without.Workloads {
		if len(w.Ports) != 0 {
			t.Errorf("%s publishes %v although nothing is exposed", w.Name, w.Ports)
		}
	}
	with := build(t, testkit.ShopOptions{ExposePort: 18080}, plan.Inputs{})
	api, _ := with.Find("api")
	if len(api.Ports) != 1 || api.Ports[0] != (plan.Port{Container: 8080, Host: 18080}) {
		t.Errorf("api ports = %v", api.Ports)
	}
}

func TestBuildTopology_RefusesASecretInputInTheEnvironment(t *testing.T) {
	t.Parallel()
	_, err := plan.BuildTopology(testkit.Shop(t, testkit.ShopOptions{SecretInput: true, APISecretInput: true}), env(t), plan.Inputs{Secret: map[string]bool{"db-password": true}}, nil)
	if !errors.Is(err, plan.ErrSecretInEnvironment) {
		t.Fatalf("error = %v, want ErrSecretInEnvironment", err)
	}
}

func TestBuildTopology_EnvironmentOverridesAndInputs(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{APIEnv: map[string]string{"EXTRA": "1"}}, plan.Inputs{})
	api, _ := top.Find("api")
	got := map[string]string{}
	for _, e := range api.Env {
		got[e.Name] = e.Value
	}
	if got["MODE"] != "standard" || got["EXTRA"] != "1" || got["LITERAL"] != "${SP_SECRET_1}" {
		t.Errorf("api environment = %v", got)
	}
}

func TestResolveSelection(t *testing.T) {
	t.Parallel()
	top := build(t, testkit.ShopOptions{}, plan.Inputs{})
	ref := func(s string) model.ObjectRef {
		r, err := model.ParseObjectRef(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	sel, err := plan.ResolveSelection(top, []model.ObjectRef{ref("workload/api"), ref("module/primary")}, false)
	if err != nil || !slices.Equal(sel.Names, []string{"api", "primary--store"}) {
		t.Fatalf("selection = %+v, %v", sel, err)
	}
	if _, err := plan.ResolveSelection(top, []model.ObjectRef{ref("workload/ghost")}, false); !errors.Is(err, plan.ErrUnknownObject) {
		t.Errorf("unknown ref error = %v", err)
	}
}
