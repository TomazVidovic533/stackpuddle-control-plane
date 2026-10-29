package plan_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// bindings returns the delivery project's bindings as BuildDeliveries takes them, with the values the
// store's driver binds.
func bindings(t testing.TB, o testkit.BoundOptions) []plan.BoundBinding {
	t.Helper()
	p := testkit.Bound(t, o)
	def, ok := p.Definition(p.ModuleInstances()[0].Definition())
	if !ok {
		t.Fatal("the definition is not resolved")
	}
	values := def.Exports()[0].Export.Values()
	var out []plan.BoundBinding
	for _, b := range p.Bindings() {
		delivery, declared := b.Delivery()
		if !declared {
			continue
		}
		parameters, _ := b.Parameters()
		var p struct{ User string }
		if err := json.Unmarshal(parameters, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, plan.BoundBinding{
			Consumer: b.Ref().Consumer.Key().String(), Ref: b.Ref(), Delivery: delivery, Endpoint: model.Endpoint{Host: "db--store", Port: 5432}, Values: values,
			Bound: map[string]module.BoundValue{
				"database": {Name: "database", Text: "app"}, "username": {Name: "username", Text: p.User},
				"password": {Name: "password", SecretRef: "db/" + p.User + "-password"}, "ca": {Name: "ca", Material: module.MaterialTLSCA},
			},
		})
	}
	return out
}

func TestBuildDeliveries_FilesCarryTheirDeclaredPathModeAndOwnerAndEnvironmentOnlyWhereDeclared(t *testing.T) {
	t.Parallel()
	got, err := plan.BuildDeliveries(bindings(t, testkit.BoundOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	migrate := got["migrate"]
	wantFiles := []plan.File{
		{Path: "/run/sp/ca.crt", Mode: 0o444, Source: plan.Source{Kind: plan.SourceMaterial, Ref: "tls/ca"}},
		{Path: "/run/sp/password", Mode: 0o400, Owner: "999:999", Source: plan.Source{Kind: plan.SourceSecret, Ref: "db/migrator-password"}},
	}
	if len(migrate.Files) != 2 || migrate.Files[0] != wantFiles[0] || migrate.Files[1] != wantFiles[1] {
		t.Errorf("files = %+v, want %+v", migrate.Files, wantFiles)
	}
	var keys []string
	for _, e := range migrate.Env {
		keys = append(keys, e.Name)
	}
	if !slices.Equal(keys, []string{"DB_HOST", "DB_NAME", "DB_PORT", "DB_USER"}) {
		t.Errorf("environment keys = %v: only the declared keys, none for the password or the CA", keys)
	}
	for _, f := range migrate.Files {
		if f.Source.Text != "" && f.Source.Kind != plan.SourceText {
			t.Errorf("file %s carries text although its source is not text", f.Path)
		}
	}
}

func TestBuildDeliveries_ASecretValueMappedToAnEnvironmentKeyIsRefused(t *testing.T) {
	t.Parallel()
	_, err := plan.BuildDeliveries(bindings(t, testkit.BoundOptions{PasswordInEnvironment: true}))
	if !errors.Is(err, plan.ErrSecretInEnvironment) {
		t.Fatalf("error = %v, want ErrSecretInEnvironment", err)
	}
	if strings.Contains(err.Error(), "migrator-password") {
		t.Errorf("the refusal names a secret reference: %v", err)
	}
}

func TestBuildDeliveries_AValueNoDriverBoundIsUnavailable(t *testing.T) {
	t.Parallel()
	in := bindings(t, testkit.BoundOptions{})
	for i := range in {
		in[i].Bound = nil
	}
	if _, err := plan.BuildDeliveries(in); !errors.Is(err, plan.ErrValueUnavailable) {
		t.Fatalf("error = %v, want ErrValueUnavailable", err)
	}
}

func TestBuildDeliveries_ASecondBindingOfOneConsumerCannotSetTheSameKeyOrPath(t *testing.T) {
	t.Parallel()
	in := bindings(t, testkit.BoundOptions{})
	if _, err := plan.BuildDeliveries(append(in, in[0])); !errors.Is(err, plan.ErrDeliveryConflict) {
		t.Fatalf("error = %v, want ErrDeliveryConflict", err)
	}
}

func TestBuildDeliveries_TheSameBindingsInAnyOrderGiveTheSameBytes(t *testing.T) {
	t.Parallel()
	in := bindings(t, testkit.BoundOptions{})
	forward, err := plan.BuildDeliveries(in)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(in)
	backward, err := plan.BuildDeliveries(in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(forward)
	b, _ := json.Marshal(backward)
	if string(a) != string(b) {
		t.Errorf("order changed the result:\n%s\n%s", a, b)
	}
}

func TestBuildTopology_AKeyOrPathAWorkloadAlreadyHasIsNeverOverwritten(t *testing.T) {
	t.Parallel()
	p := testkit.Shop(t, testkit.ShopOptions{Instances: []string{"primary"}})
	own := plan.Deliveries{"api": {Env: []plan.EnvVar{{Name: "MODE", Value: "bound"}}}}
	if _, err := plan.BuildTopology(p, env(t), plan.Inputs{}, own); !errors.Is(err, plan.ErrDeliveryConflict) {
		t.Errorf("a bound key that the workload sets: %v, want ErrDeliveryConflict", err)
	}
	twice := plan.Deliveries{"api": {Files: []plan.File{
		{Path: "/run/x", Source: plan.Source{Kind: plan.SourceText, Text: "a"}},
		{Path: "/run/x", Source: plan.Source{Kind: plan.SourceText, Text: "b"}},
	}}}
	if _, err := plan.BuildTopology(p, env(t), plan.Inputs{}, twice); !errors.Is(err, plan.ErrDeliveryConflict) {
		t.Errorf("two files at one path: %v, want ErrDeliveryConflict", err)
	}
	if _, err := plan.BuildTopology(p, env(t), plan.Inputs{}, plan.Deliveries{"nobody": {}}); !errors.Is(err, plan.ErrUnknownObject) {
		t.Errorf("a delivery into no workload: %v, want ErrUnknownObject", err)
	}
}

func TestServerNames_HostNamesOnlyWhenTheWorkloadIsExposed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		expose bool
		want   []string
	}{
		{"not exposed", false, []string{"db--store"}},
		{"exposed", true, []string{"db--store", "localhost", "127.0.0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := boundTop(t, testkit.BoundOptions{Expose: tc.expose})
			if got := plan.ServerNames(top, "db--store"); !slices.Equal(got, tc.want) {
				t.Errorf("names = %v, want %v", got, tc.want)
			}
		})
	}
}
