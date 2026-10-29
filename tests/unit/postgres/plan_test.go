package postgres_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
)

func TestPlan_AFreshServerIsPlannedInOrderWithSecretsOnUsersOnly(t *testing.T) {
	t.Parallel()
	resp, err := plan(t, optionsText, absent())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"postgres.admin admin/server",
		"postgres.role role/app-writer",
		"postgres.role role/app-migrator",
		"postgres.role role/app-runtime",
		"postgres.database database/catalog",
		"postgres.schema schema/catalog/public",
		"postgres.grant grant/catalog/public/app-writer",
		"postgres.default-privilege default-privilege/catalog/routines",
		"postgres.default-privilege default-privilege/catalog/public/app-writer",
	}
	sameLines(t, "plan order", want, actions(resp.Steps))
	for _, s := range resp.Steps {
		wantSecret := s.Action == "postgres.role" && !strings.HasSuffix(s.Object, "app-writer")
		if (s.Secret != nil) != wantSecret {
			t.Errorf("%s: secret = %v, want a secret only for login users", s.Object, s.Secret)
		}
		if s.Secret != nil && !strings.HasPrefix(s.Secret.Ref, "primary/") {
			t.Errorf("%s: secret ref %q is outside the instance namespace", s.Object, s.Secret.Ref)
		}
		if s.Destructive {
			t.Errorf("%s is destructive, but the module deletes nothing", s.Object)
		}
	}
}

func TestPlan_AConvergedServerPlansOnlyTheAdminAndTheIdempotentGrants(t *testing.T) {
	t.Parallel()
	resp, err := plan(t, optionsText, present(t, converged()))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"postgres.admin admin/server", "postgres.grant grant/catalog/public/app-writer"}
	sameLines(t, "a repeat run plans", want, actions(resp.Steps))
	for _, s := range resp.Steps {
		if s.Secret != nil {
			t.Errorf("%s asks for a secret on a repeat run", s.Object)
		}
	}
}

func TestPlan_AnExistingRoleOfOursIsReconciledWithoutASecret(t *testing.T) {
	t.Parallel()
	c := converged()
	for i := range c.Roles {
		if c.Roles[i].Name == "app_runtime" {
			c.Roles[i].CreateDB = true
		}
	}
	resp, err := plan(t, optionsText, present(t, c))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(resp.Steps, func(s module.Step) bool { return s.Object == "role/app-runtime" })
	if i < 0 || resp.Steps[i].Secret != nil {
		t.Fatalf("drifted role step = %v, want a step without a secret", resp.Steps)
	}
}

func TestPlan_PreflightRefusesWhatTheDeclarationDoesNotExplain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(c *catalog)
		want   string
	}{
		{"an unmanaged database open to PUBLIC (prospective access for new users)", func(c *catalog) {
			c.Databases = append(c.Databases, database{Name: "legacy", Owner: "postgres", AllowConn: true, Inspected: true, ACL: publicConnect()})
		}, "PUBLIC"},
		{"an unmanaged database that grants a declared role directly", func(c *catalog) {
			*c = converged()
			c.Databases = append(c.Databases, database{Name: "legacy", Owner: "postgres", AllowConn: true, Inspected: true, ACL: []acl{{Grantee: "app_migrator", Priv: "CONNECT"}}})
		}, "direct"},
		{"an unmanaged database reached through a declared membership", func(c *catalog) {
			*c = converged()
			c.Databases = append(c.Databases, database{Name: "legacy", Owner: "postgres", AllowConn: true, Inspected: true, ACL: []acl{{Grantee: "app_writer", Priv: "CONNECT"}}})
		}, "inherited"},
		{"an unmanaged database owned by a declared role", func(c *catalog) {
			*c = converged()
			c.Databases = append(c.Databases, database{Name: "legacy", Owner: "app_migrator", AllowConn: true, Inspected: true})
		}, "ownership"},
		{"a declared role that exists without our marker", func(c *catalog) {
			c.Roles = append(c.Roles, role{Name: "app_writer", Inherit: true})
		}, "never adopted by name"},
		{"a declared role marked by another instance", func(c *catalog) {
			c.Roles = append(c.Roles, role{Name: "app_writer", Inherit: true, Comment: "stackpuddle:scope-9/other"})
		}, "never adopted by name"},
		{"a declared database without its marker", func(c *catalog) {
			c.Databases = append(c.Databases, database{Name: "catalog", Owner: "app_migrator", AllowConn: true, Inspected: true})
		}, "never adopted by name"},
		{"a managed role that is a member of an undeclared role", func(c *catalog) {
			*c = converged()
			c.Roles = append(c.Roles, role{Name: "pg_read_all_data", Inherit: true})
			c.Members = append(c.Members, member{Member: "app_runtime", Role: "pg_read_all_data", Inherit: true})
		}, "undeclared role"},
		{"a schema of a managed database owned by a runtime identity", func(c *catalog) {
			*c = converged()
			c.Databases[len(c.Databases)-1].Schemas[0].Owner = "app_runtime"
		}, "runtime user"},
		{"a connectable database that was not inspected (incomplete inspection)", func(c *catalog) {
			c.Databases[0].Inspected = false
		}, "incomplete"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c := fresh()
			test.mutate(&c)
			resp, err := plan(t, optionsText, present(t, c))
			if !errors.Is(err, postgres.ErrPreflight) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want a preflight refusal mentioning %q", err, test.want)
			}
			if len(resp.Steps) != 0 {
				t.Errorf("a refused preflight still planned %v", actions(resp.Steps))
			}
		})
	}
}

func TestPlan_TheBuiltinDefaultAccessOfAFreshClusterIsNotUndeclaredAccess(t *testing.T) {
	t.Parallel()
	if _, err := plan(t, optionsText, present(t, fresh())); err != nil {
		t.Fatalf("a fresh cluster is refused: %v", err)
	}
}

func TestPlan_DeniedAndUnknownObservationsRefuseMutation(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"denied", "unknown", ""} {
		obs := module.Observation{Facts: map[string]string{postgres.FactStatus: status, postgres.FactReason: "why"}}
		resp, err := plan(t, optionsText, obs)
		if !errors.Is(err, postgres.ErrObservationUnusable) || len(resp.Steps) != 0 {
			t.Errorf("status %q: steps %v, error %v", status, actions(resp.Steps), err)
		}
	}
}

func TestPlan_OptionsTheModuleRulesRefuseStopBeforePlanning(t *testing.T) {
	t.Parallel()
	bad := strings.Replace(optionsText, `"owner": "app_migrator"`, `"owner": "app_runtime"`, 1)
	resp, err := plan(t, bad, absent())
	if !errors.Is(err, postgres.ErrInvalidOptions) || len(resp.Steps) != 0 {
		t.Fatalf("steps %v, error %v; want the module's own rules to refuse", actions(resp.Steps), err)
	}
	if strings.Contains(err.Error(), "app_runtime") && strings.Contains(err.Error(), "value") {
		t.Errorf("the refusal echoes a value: %v", err)
	}
}

func TestPlan_OptionsTheFixedServerIgnoresAreRefusedByPointer(t *testing.T) {
	t.Parallel()
	with := func(member string) string {
		return strings.Replace(optionsText, `"startupTimeoutSeconds": 60,`, `"startupTimeoutSeconds": 60, `+member+`,`, 1)
	}
	tests := []struct{ name, text, pointer string }{
		{"memory", with(`"resources": {"memoryMiB": 2048}`), "/resources/memoryMiB"},
		{"cpus", with(`"resources": {"cpus": 2}`), "/resources/cpus"},
		{"max connections", with(`"settings": {"maxConnections": 200}`), "/settings/maxConnections"},
		{"shared buffers", with(`"settings": {"sharedBuffersMiB": 256}`), "/settings/sharedBuffersMiB"},
		{"host access", with(`"hostAccess": {"enabled": true, "port": 5433}`), "/hostAccess/enabled"},
		{"startup beyond the probe budget", strings.Replace(optionsText, `"startupTimeoutSeconds": 60`, `"startupTimeoutSeconds": 300`, 1), "/startupTimeoutSeconds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resp, err := plan(t, test.text, absent())
			if !errors.Is(err, postgres.ErrInvalidOptions) || !errors.Is(err, postgres.ErrUnwiredOption) || !strings.Contains(err.Error(), test.pointer) || len(resp.Steps) != 0 {
				t.Fatalf("steps %v, error %v; want a refusal naming %s", actions(resp.Steps), err, test.pointer)
			}
		})
	}
}

func TestPlan_TheSchemaDefaultsOfTheFixedServerAreAccepted(t *testing.T) {
	t.Parallel()
	text := strings.Replace(optionsText, `"startupTimeoutSeconds": 60,`, `"startupTimeoutSeconds": 100, "resources": {"memoryMiB": 1024, "cpus": 1}, "hostAccess": {"enabled": false, "port": 5432}, "settings": {"maxConnections": 100, "sharedBuffersMiB": 128},`, 1)
	if _, err := plan(t, text, absent()); err != nil {
		t.Fatalf("the defaults are refused: %v", err)
	}
}
