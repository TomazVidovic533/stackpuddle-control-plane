package postgres_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func bind(t *testing.T, parameters string) (module.BindResponse, error) {
	t.Helper()
	i := instance(testkit.PostgresOptions)
	i.Key = testkit.PostgresInstance
	return postgres.Driver{}.Bind(t.Context(), module.BindRequest{Instance: i, Export: postgres.ExportSQL, Parameters: json.RawMessage(parameters)})
}

func valuesOf(r module.BindResponse) map[string]module.BoundValue {
	out := map[string]module.BoundValue{}
	for _, v := range r.Values {
		out[v.Name] = v
	}
	return out
}

func TestBind_RuntimeAndMigrationParametersGiveDifferentUsersAndSecretReferences(t *testing.T) {
	t.Parallel()
	runtime, err := bind(t, `{"user":"catalog_app","database":"catalog"}`)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	migration, err := bind(t, `{"user":"catalog_migrator","database":"catalog"}`)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	r, m := valuesOf(runtime), valuesOf(migration)
	if r["username"].Text != "catalog_app" || m["username"].Text != "catalog_migrator" {
		t.Errorf("usernames = %q / %q", r["username"].Text, m["username"].Text)
	}
	if r["password"].SecretRef != "db/catalog-app-password" || m["password"].SecretRef != "db/catalog-migration-password" || r["password"].SecretRef == m["password"].SecretRef {
		t.Errorf("secret references = %q / %q", r["password"].SecretRef, m["password"].SecretRef)
	}
	if r["database"].Text != "catalog" || r["ca"].Material != module.MaterialTLSCA {
		t.Errorf("database %q, ca %+v", r["database"].Text, r["ca"])
	}
}

func TestBind_EachValueIsExactlyOneKindAndNoTextHoldsASecret(t *testing.T) {
	t.Parallel()
	response, err := bind(t, `{"user":"catalog_app","database":"catalog"}`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range response.Values {
		names = append(names, v.Name)
		set := 0
		for _, present := range []bool{v.Text != "", v.SecretRef != "", v.Material != ""} {
			if present {
				set++
			}
		}
		if set != 1 {
			t.Errorf("value %s has %d kinds set", v.Name, set)
		}
		if v.Name != "password" && v.SecretRef != "" {
			t.Errorf("value %s is a secret reference", v.Name)
		}
		if v.Name == "password" && v.Text != "" {
			t.Error("the password is bound as text")
		}
	}
	if want := []string{"ca", "database", "password", "username"}; !reflect.DeepEqual(names, want) {
		t.Errorf("values = %v, want %v: the contract of the sql export", names, want)
	}
}

func TestBind_RefusesWhatTheDeclarationDoesNotAllow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		parameters string
	}{
		{"the administrator role", `{"user":"postgres","database":"catalog"}`},
		{"an undeclared user", `{"user":"sneaky","database":"catalog"}`},
		{"a permission role that cannot log in", `{"user":"catalog_writer","database":"catalog"}`},
		{"an undeclared database", `{"user":"catalog_app","database":"postgres"}`},
		{"a template database", `{"user":"catalog_app","database":"template1"}`},
		{"an unknown parameter", `{"user":"catalog_app","database":"catalog","superuser":true}`},
		{"a missing database", `{"user":"catalog_app"}`},
		{"a missing user", `{"database":"catalog"}`},
		{"no parameters", ``},
		{"trailing data", `{"user":"catalog_app","database":"catalog"} {}`},
		{"not an object", `["catalog_app"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := bind(t, tc.parameters)
			if !errors.Is(err, postgres.ErrInvalidParameters) {
				t.Fatalf("error = %v, want ErrInvalidParameters", err)
			}
			if strings.Contains(err.Error(), "password") {
				t.Errorf("the refusal names a secret: %v", err)
			}
		})
	}
}

func TestBind_AnExportTheModuleDoesNotOfferIsRefused(t *testing.T) {
	t.Parallel()
	i := instance(testkit.PostgresOptions)
	_, err := postgres.Driver{}.Bind(t.Context(), module.BindRequest{Instance: i, Export: "admin", Parameters: json.RawMessage(`{"user":"catalog_app","database":"catalog"}`)})
	if !errors.Is(err, postgres.ErrInvalidParameters) {
		t.Fatalf("error = %v", err)
	}
}

func TestBind_RunsNoProbeAndTheDescriptorAsksForAServerCertificate(t *testing.T) {
	t.Parallel()
	d := postgres.Driver{}.Describe()
	if want := []module.ServerTLS{{Workload: "server", Listener: "sql", Owner: "999:999"}}; !reflect.DeepEqual(d.ServerTLS, want) {
		t.Errorf("ServerTLS = %+v, want %+v", d.ServerTLS, want)
	}
	// BindResponse has no probe member: a driver cannot ask for runtime work while binding.
	if _, has := reflect.TypeOf(module.BindResponse{}).FieldByName("Probes"); has {
		t.Error("BindResponse can carry probes")
	}
}
