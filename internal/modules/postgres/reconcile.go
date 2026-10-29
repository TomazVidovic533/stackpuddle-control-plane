package postgres

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// Action kinds, namespaced by the module.
const (
	actionAdmin   = "postgres.admin"
	actionRole    = "postgres.role"
	actionDB      = "postgres.database"
	actionSchema  = "postgres.schema"
	actionGrant   = "postgres.grant"
	actionDefault = "postgres.default-privilege"
)

// secretSize is the size in bytes of a generated password.
const secretSize = 32

type roleParams struct {
	Name     string   `json:"name"`
	Login    bool     `json:"login"`
	MemberOf []string `json:"memberOf,omitempty"`
	// Declared lists every declared role, so a reconcile can remove memberships the declaration lacks.
	Declared []string `json:"declared,omitempty"`
}

type databaseParams struct {
	Name     string   `json:"name"`
	Database database `json:"database"`
}

type schemaParams struct {
	Database string `json:"database"`
	Name     string `json:"name"`
	Owner    string `json:"owner"`
}

type grantParams struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Role     string `json:"role"`
	Creator  string `json:"creator,omitempty"`
	Grant    grant  `json:"grant"`
}

// objectName turns identifiers into the path segments the engine's step objects allow.
func objectName(kind string, parts ...string) string {
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		segs = append(segs, strings.ReplaceAll(p, "_", "-"))
	}
	return kind + "/" + strings.Join(segs, "/")
}

func newStep(action, object string, params any, secret *module.SecretNeed) (module.Step, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return module.Step{}, fmt.Errorf("encode step params: %w", err)
	}
	return module.Step{Action: action, Object: object, Params: raw, Secret: secret}, nil
}

// buildSteps turns the declaration and the observed facts into ordered steps: the admin step, roles
// (permission roles before users), databases, schemas, existing-object grants, then default privileges.
// With fresh facts nothing exists yet and every step is planned. A step is skipped only when the
// observation shows it done and ours.
func buildSteps(inst module.Instance, o options, facts catalogFacts, marker string) ([]module.Step, error) {
	b := stepBuilder{inst: inst, o: o, facts: facts, marker: marker}
	b.add(actionAdmin, "admin/server", struct{}{}, nil)
	b.roles()
	b.databases()
	b.schemas()
	b.grants()
	return b.steps, b.err
}

type stepBuilder struct {
	inst   module.Instance
	o      options
	facts  catalogFacts
	marker string
	steps  []module.Step
	err    error
}

func (b *stepBuilder) add(action, object string, params any, secret *module.SecretNeed) {
	step, err := newStep(action, object, params, secret)
	if err != nil {
		b.err = err
		return
	}
	b.steps = append(b.steps, step)
}

func (b *stepBuilder) roles() {
	declared := b.o.declaredRoles()
	for _, name := range b.o.roleNames() {
		b.role(roleParams{Name: name, Declared: declared}, nil)
	}
	for _, name := range b.o.userNames() {
		u := b.o.Users[name]
		secret := &module.SecretNeed{Ref: b.inst.Key + "/" + u.PasswordSecretRef, Size: secretSize}
		b.role(roleParams{Name: name, Login: true, MemberOf: u.MemberOf, Declared: declared}, secret)
	}
}

func (b *stepBuilder) role(p roleParams, secret *module.SecretNeed) {
	r, exists := b.facts.role(p.Name)
	if exists && !b.roleDrift(r, p) {
		return
	}
	if exists {
		secret = nil
	}
	b.add(actionRole, objectName("role", p.Name), p, secret)
}

// roleDrift reports whether an existing role of ours differs from its declaration.
func (b *stepBuilder) roleDrift(r roleFact, p roleParams) bool {
	return r.Comment != b.marker || attributesDrift(r, p) || b.membershipDrift(p)
}

func attributesDrift(r roleFact, p roleParams) bool {
	dangerous := r.Super || r.CreateDB || r.CreateRole || r.Replication || r.BypassRLS
	return dangerous || !r.Inherit || r.Login != p.Login
}

func (b *stepBuilder) membershipDrift(p roleParams) bool {
	have := map[string]memberFact{}
	for _, m := range b.facts.Members {
		if m.Member == p.Name {
			have[m.Role] = m
		}
	}
	return missingOrWrongMembership(have, p) || undeclaredMembership(have, p)
}

// missingOrWrongMembership reports a declared membership that is absent or has options beyond
// inheritance.
func missingOrWrongMembership(have map[string]memberFact, p roleParams) bool {
	for _, parent := range p.MemberOf {
		m, ok := have[parent]
		if !ok || !m.Inherit || m.Set || m.Admin {
			return true
		}
	}
	return false
}

// undeclaredMembership reports a membership in a declared role that the declaration does not make.
func undeclaredMembership(have map[string]memberFact, p roleParams) bool {
	for parent := range have {
		if slices.Contains(p.Declared, parent) && !slices.Contains(p.MemberOf, parent) {
			return true
		}
	}
	return false
}

func (b *stepBuilder) databases() {
	for _, name := range b.o.databaseNames() {
		d := b.o.Databases[name]
		if existing, ok := b.facts.database(name); ok && !b.databaseDrift(existing, d) {
			continue
		}
		b.add(actionDB, objectName("database", name), databaseParams{Name: name, Database: d}, nil)
	}
}

func (b *stepBuilder) databaseDrift(existing databaseFact, d database) bool {
	if existing.Comment != b.marker {
		return true
	}
	connect := map[string]bool{}
	for _, a := range existing.ACL {
		if a.Priv == "CONNECT" {
			connect[a.Grantee] = true
		}
	}
	if connect[""] {
		return true
	}
	for _, role := range d.ConnectRoles {
		if !connect[role] {
			return true
		}
	}
	return false
}

func (b *stepBuilder) schemas() {
	for _, db := range b.o.databaseNames() {
		d := b.o.Databases[db]
		for _, name := range sortedKeys(d.Schemas) {
			if b.schemaDone(db, name, d.Owner) {
				continue
			}
			b.add(actionSchema, objectName("schema", db, name), schemaParams{Database: db, Name: name, Owner: d.Owner}, nil)
		}
	}
}

func (b *stepBuilder) schemaDone(db, name, owner string) bool {
	existing, ok := b.facts.database(db)
	if !ok {
		return false
	}
	s, found := existing.schema(name)
	return found && s.Comment == b.marker && s.Owner == owner
}

func (b *stepBuilder) grants() {
	for _, db := range b.o.databaseNames() {
		d := b.o.Databases[db]
		for _, sname := range sortedKeys(d.Schemas) {
			for _, role := range sortedKeys(d.Schemas[sname].Grants) {
				g := d.Schemas[sname].Grants[role]
				p := grantParams{Database: db, Schema: sname, Role: role, Creator: d.Owner, Grant: g}
				b.add(actionGrant, objectName("grant", db, sname, role), p, nil)
			}
		}
	}
	for _, db := range b.o.databaseNames() {
		b.defaults(db)
	}
}

func (b *stepBuilder) defaults(db string) {
	d := b.o.Databases[db]
	if !b.routinesDone(db, d.Owner) {
		b.add(actionDefault, objectName("default-privilege", db, "routines"), grantParams{Database: db, Creator: d.Owner}, nil)
	}
	for _, sname := range sortedKeys(d.Schemas) {
		for _, role := range sortedKeys(d.Schemas[sname].Grants) {
			p := grantParams{Database: db, Schema: sname, Role: role, Creator: d.Owner, Grant: d.Schemas[sname].Grants[role]}
			if b.defaultDone(db, p) {
				continue
			}
			b.add(actionDefault, objectName("default-privilege", db, sname, role), p, nil)
		}
	}
}

func (b *stepBuilder) defaultsOf(db, creator, schema, typ string) []defaultsFact {
	existing, ok := b.facts.database(db)
	if !ok {
		return nil
	}
	var out []defaultsFact
	for _, d := range existing.Defaults {
		if d.Role == creator && d.Schema == schema && d.Type == typ {
			out = append(out, d)
		}
	}
	return out
}

// routinesDone reports whether the creator's future routines already lack PUBLIC execute.
func (b *stepBuilder) routinesDone(db, creator string) bool {
	for _, d := range b.defaultsOf(db, creator, "", "f") {
		if !slices.ContainsFunc(d.ACL, func(a aclFact) bool { return a.Grantee == "" }) {
			return true
		}
	}
	return false
}

func (b *stepBuilder) defaultDone(db string, p grantParams) bool {
	return b.privilegesPresent(b.defaultsOf(db, p.Creator, p.Schema, "r"), p.Role, p.Grant.Tables) &&
		b.privilegesPresent(b.defaultsOf(db, p.Creator, p.Schema, "S"), p.Role, p.Grant.Sequences)
}

func (b *stepBuilder) privilegesPresent(rows []defaultsFact, role string, want []string) bool {
	if len(want) == 0 {
		return true
	}
	have := map[string]bool{}
	for _, row := range rows {
		for _, a := range row.ACL {
			if a.Grantee == role {
				have[a.Priv] = true
			}
		}
	}
	for _, p := range want {
		if !have[p] {
			return false
		}
	}
	return true
}
