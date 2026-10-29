package postgres_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
)

const marker = "stackpuddle:scope-1/primary"

// The fact types mirror the JSON the driver's catalog queries print; the driver's own types are
// unexported, and the wire shape is the contract the tests hold.
type (
	acl struct {
		Grantee string `json:"grantee"`
		Priv    string `json:"priv"`
	}
	role struct {
		Name        string `json:"name"`
		Super       bool   `json:"super"`
		CreateDB    bool   `json:"createdb"`
		CreateRole  bool   `json:"createrole"`
		Replication bool   `json:"replication"`
		BypassRLS   bool   `json:"bypassrls"`
		Login       bool   `json:"login"`
		Inherit     bool   `json:"inherit"`
		Comment     string `json:"comment"`
	}
	member struct {
		Member  string `json:"member"`
		Role    string `json:"role"`
		Inherit bool   `json:"inherit"`
		Set     bool   `json:"set"`
		Admin   bool   `json:"admin"`
	}
	schemaFact struct {
		Name    string `json:"name"`
		Owner   string `json:"owner"`
		Comment string `json:"comment"`
		ACL     []acl  `json:"acl"`
	}
	defaults struct {
		Role   string `json:"role"`
		Schema string `json:"schema"`
		Type   string `json:"type"`
		ACL    []acl  `json:"acl"`
	}
	database struct {
		Name       string       `json:"name"`
		Owner      string       `json:"owner"`
		AllowConn  bool         `json:"allowConn"`
		IsTemplate bool         `json:"isTemplate"`
		Comment    string       `json:"comment"`
		ACL        []acl        `json:"acl"`
		Inspected  bool         `json:"inspected"`
		Schemas    []schemaFact `json:"schemas"`
		Defaults   []defaults   `json:"defaults"`
	}
	catalog struct {
		Roles     []role     `json:"roles"`
		Members   []member   `json:"members"`
		Databases []database `json:"databases"`
	}
)

func publicConnect() []acl {
	return []acl{{Grantee: "", Priv: "CONNECT"}, {Grantee: "", Priv: "TEMPORARY"}}
}

// fresh is the catalog of a cluster the image just initialised: the superuser and the built-in databases.
func fresh() catalog {
	return catalog{
		Roles: []role{{Name: "postgres", Super: true, CreateDB: true, CreateRole: true, Replication: true, BypassRLS: true, Login: true, Inherit: true}},
		Databases: []database{
			{Name: "postgres", Owner: "postgres", AllowConn: true, ACL: publicConnect(), Inspected: true},
			{Name: "template0", Owner: "postgres", IsTemplate: true, ACL: publicConnect()},
			{Name: "template1", Owner: "postgres", AllowConn: true, IsTemplate: true, ACL: publicConnect(), Inspected: true},
		},
	}
}

const optionsText = `{
  "startupTimeoutSeconds": 60,
  "users": {
    "app_runtime": {"purpose": "runtime", "passwordSecretRef": "app-runtime-password", "memberOf": ["app_writer"]},
    "app_migrator": {"purpose": "migrations", "passwordSecretRef": "app-migration-password"}
  },
  "roles": {"app_writer": {"login": false}},
  "databases": {
    "catalog": {
      "owner": "app_migrator", "connectRoles": ["app_writer"], "connectionLimit": 25,
      "settings": {"statementTimeoutMs": 30000},
      "schemas": {"public": {"grants": {"app_writer": {"tables": ["SELECT", "INSERT"], "sequences": ["USAGE"]}}}}
    }
  }
}`

func instance(optionsJSON string) module.Instance {
	return module.Instance{Key: "primary", Definition: "postgres", Version: "0.1.0", Scope: "scope-1", Options: json.RawMessage(optionsJSON)}
}

func present(t *testing.T, c catalog) module.Observation {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return module.Observation{Present: true, Facts: map[string]string{postgres.FactStatus: "present", postgres.FactCatalog: string(raw)}}
}

func absent() module.Observation {
	return module.Observation{Facts: map[string]string{postgres.FactStatus: "absent"}}
}

// converged is a catalog in which everything the options declare exists, is ours and is configured.
func converged() catalog {
	c := fresh()
	attrs := role{Inherit: true, Comment: marker}
	for _, name := range []string{"app_migrator", "app_runtime"} {
		r := attrs
		r.Name, r.Login = name, true
		c.Roles = append(c.Roles, r)
	}
	writer := attrs
	writer.Name = "app_writer"
	c.Roles = append(c.Roles, writer)
	c.Members = []member{{Member: "app_runtime", Role: "app_writer", Inherit: true}}
	c.Databases = append(c.Databases, database{
		Name: "catalog", Owner: "app_migrator", AllowConn: true, Comment: marker, Inspected: true,
		ACL:     []acl{{Grantee: "app_migrator", Priv: "CONNECT"}, {Grantee: "app_writer", Priv: "CONNECT"}},
		Schemas: []schemaFact{{Name: "public", Owner: "app_migrator", Comment: marker}},
		Defaults: []defaults{
			{Role: "app_migrator", Schema: "public", Type: "r", ACL: []acl{{Grantee: "app_writer", Priv: "SELECT"}, {Grantee: "app_writer", Priv: "INSERT"}}},
			{Role: "app_migrator", Schema: "public", Type: "S", ACL: []acl{{Grantee: "app_writer", Priv: "USAGE"}}},
			{Role: "app_migrator", Schema: "", Type: "f", ACL: []acl{{Grantee: "app_migrator", Priv: "EXECUTE"}}},
		},
	})
	return c
}

func plan(t *testing.T, optionsJSON string, obs module.Observation) (module.PlanResponse, error) {
	t.Helper()
	return postgres.Driver{}.Plan(t.Context(), module.PlanRequest{Instance: instance(optionsJSON), Observation: obs})
}

func actions(steps []module.Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Action+" "+s.Object)
	}
	return out
}

func sameLines(t *testing.T, what string, want, got []string) {
	t.Helper()
	if !slices.Equal(want, got) {
		t.Errorf("%s:\nwant %q\ngot  %q", what, want, got)
	}
}
