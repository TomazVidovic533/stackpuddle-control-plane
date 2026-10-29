package postgres

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrIncompleteFacts marks catalog facts that do not cover every connectable database. A decision never
// rests on them.
var ErrIncompleteFacts = errors.New("the catalog inspection is incomplete")

// catalogFacts is what the live inspection found: cluster-wide roles, memberships and databases, and
// per database its schemas and default privileges.
type catalogFacts struct {
	Roles     []roleFact     `json:"roles"`
	Members   []memberFact   `json:"members"`
	Databases []databaseFact `json:"databases"`
}

type roleFact struct {
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

type memberFact struct {
	Member  string `json:"member"`
	Role    string `json:"role"`
	Inherit bool   `json:"inherit"`
	Set     bool   `json:"set"`
	Admin   bool   `json:"admin"`
}

// aclFact is one privilege. Grantee "" is PUBLIC.
type aclFact struct {
	Grantee string `json:"grantee"`
	Priv    string `json:"priv"`
}

type databaseFact struct {
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	AllowConn  bool      `json:"allowConn"`
	IsTemplate bool      `json:"isTemplate"`
	Comment    string    `json:"comment"`
	ACL        []aclFact `json:"acl"`

	// Inspected is true once the per-database queries ran; Schemas and Defaults are valid only then.
	Inspected bool           `json:"inspected"`
	Schemas   []schemaFact   `json:"schemas"`
	Defaults  []defaultsFact `json:"defaults"`
}

type schemaFact struct {
	Name    string    `json:"name"`
	Owner   string    `json:"owner"`
	Comment string    `json:"comment"`
	ACL     []aclFact `json:"acl"`
}

type defaultsFact struct {
	Role   string    `json:"role"`
	Schema string    `json:"schema"`
	Type   string    `json:"type"`
	ACL    []aclFact `json:"acl"`
}

// perDatabase is one line of the per-database inspection.
type perDatabase struct {
	DB       string         `json:"db"`
	Schemas  []schemaFact   `json:"schemas"`
	Defaults []defaultsFact `json:"defaults"`
}

const aclSQL = `(SELECT coalesce(json_agg(json_build_object('grantee', CASE WHEN x.grantee = 0 THEN '' ELSE pg_get_userbyid(x.grantee) END, 'priv', x.privilege_type)), '[]'::json) FROM aclexplode(%s) x)`

// clusterSQL reads the cluster-wide catalog as one JSON document.
func clusterSQL() string {
	dbACL := fmt.Sprintf(aclSQL, "coalesce(d.datacl, acldefault('d', d.datdba))")
	return `SELECT json_build_object(
 'roles', (SELECT coalesce(json_agg(json_build_object('name', rolname, 'super', rolsuper, 'createdb', rolcreatedb, 'createrole', rolcreaterole, 'replication', rolreplication, 'bypassrls', rolbypassrls, 'login', rolcanlogin, 'inherit', rolinherit, 'comment', coalesce(shobj_description(oid, 'pg_authid'), '')) ORDER BY rolname), '[]'::json) FROM pg_roles),
 'members', (SELECT coalesce(json_agg(json_build_object('member', m.rolname, 'role', r.rolname, 'inherit', a.inherit_option, 'set', a.set_option, 'admin', a.admin_option) ORDER BY m.rolname, r.rolname), '[]'::json) FROM pg_auth_members a JOIN pg_roles m ON m.oid = a.member JOIN pg_roles r ON r.oid = a.roleid),
 'databases', (SELECT coalesce(json_agg(json_build_object('name', d.datname, 'owner', pg_get_userbyid(d.datdba), 'allowConn', d.datallowconn, 'isTemplate', d.datistemplate, 'comment', coalesce(shobj_description(d.oid, 'pg_database'), ''), 'acl', ` + dbACL + `) ORDER BY d.datname), '[]'::json) FROM pg_database d)
);
`
}

// databaseSQL reads one database's schemas and default privileges as one JSON line.
func databaseSQL() string {
	nsACL := fmt.Sprintf(aclSQL, "coalesce(n.nspacl, acldefault('n', n.nspowner))")
	defACL := fmt.Sprintf(aclSQL, "d.defaclacl")
	return `SELECT json_build_object('db', current_database(),
 'schemas', (SELECT coalesce(json_agg(json_build_object('name', n.nspname, 'owner', pg_get_userbyid(n.nspowner), 'comment', coalesce(obj_description(n.oid, 'pg_namespace'), ''), 'acl', ` + nsACL + `) ORDER BY n.nspname), '[]'::json) FROM pg_namespace n WHERE n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'),
 'defaults', (SELECT coalesce(json_agg(json_build_object('role', pg_get_userbyid(d.defaclrole), 'schema', CASE WHEN d.defaclnamespace = 0 THEN '' ELSE d.defaclnamespace::regnamespace::text END, 'type', d.defaclobjtype::text, 'acl', ` + defACL + `) ORDER BY d.defaclrole, d.defaclnamespace, d.defaclobjtype), '[]'::json) FROM pg_default_acl d)
);
`
}

// inspectDatabasesScript connects to each named database in turn and reads its catalog. A database
// name is passed as a single-quoted psql argument, so any name an unmanaged database carries is safe.
func inspectDatabasesScript(names []string) (string, error) {
	var b strings.Builder
	for _, name := range names {
		lit, err := psqlLiteral(name)
		if err != nil {
			return "", fmt.Errorf("inspect %w", err)
		}
		fmt.Fprintf(&b, "\\connect %s\n%s", lit, databaseSQL())
	}
	return b.String(), nil
}

// parseCluster decodes the cluster document.
func parseCluster(out string) (catalogFacts, error) {
	var facts catalogFacts
	if err := decodeStrict(strings.TrimSpace(out), &facts); err != nil {
		return catalogFacts{}, fmt.Errorf("cluster catalog: %w", err)
	}
	return facts, nil
}

// connectable returns the databases the inspection must cover.
func (f catalogFacts) connectable() []string {
	var names []string
	for _, d := range f.Databases {
		if d.AllowConn {
			names = append(names, d.Name)
		}
	}
	return names
}

// withDatabases adds the per-database lines to the cluster facts. Every connectable database must be
// covered once, or the facts are incomplete.
func (f catalogFacts) withDatabases(out string) (catalogFacts, error) {
	seen := map[string]perDatabase{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var p perDatabase
		if err := decodeStrict(line, &p); err != nil {
			return catalogFacts{}, fmt.Errorf("%w: a database line: %w", ErrIncompleteFacts, err)
		}
		seen[p.DB] = p
	}
	done := catalogFacts{Roles: f.Roles, Members: f.Members, Databases: make([]databaseFact, len(f.Databases))}
	copy(done.Databases, f.Databases)
	for i, d := range done.Databases {
		if !d.AllowConn {
			continue
		}
		p, ok := seen[d.Name]
		if !ok {
			return catalogFacts{}, fmt.Errorf("%w: a connectable database was not inspected", ErrIncompleteFacts)
		}
		done.Databases[i].Inspected, done.Databases[i].Schemas, done.Databases[i].Defaults = true, p.Schemas, p.Defaults
	}
	return done, nil
}

// decodeStrict decodes one JSON document and refuses unknown members and trailing data.
func decodeStrict(text string, out any) error {
	decoder := json.NewDecoder(bytes.NewReader([]byte(text)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the document")
	}
	return nil
}

func (f catalogFacts) role(name string) (roleFact, bool) {
	for _, r := range f.Roles {
		if r.Name == name {
			return r, true
		}
	}
	return roleFact{}, false
}

func (f catalogFacts) database(name string) (databaseFact, bool) {
	for _, d := range f.Databases {
		if d.Name == name {
			return d, true
		}
	}
	return databaseFact{}, false
}

func (d databaseFact) schema(name string) (schemaFact, bool) {
	for _, s := range d.Schemas {
		if s.Name == name {
			return s, true
		}
	}
	return schemaFact{}, false
}

// complete reports whether every connectable database was inspected.
func (f catalogFacts) complete() bool {
	for _, d := range f.Databases {
		if d.AllowConn && !d.Inspected {
			return false
		}
	}
	return len(f.Roles) > 0 && len(f.Databases) > 0
}
