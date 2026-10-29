package postgres

import (
	"errors"
	"fmt"
	"slices"
)

// ErrPreflight marks a refusal to mutate: the live catalog holds something the declaration does not
// explain, or could not be inspected completely.
var ErrPreflight = errors.New("preflight refuses mutation")

// builtinDatabases are the databases every cluster starts with. Their default PUBLIC access carries no
// application data and is the engine's own, so it is not undeclared access; any other grant on them is.
func builtinDatabases() []string { return []string{adminDatabase, "template0", "template1"} }

// evaluate is the pure preflight. It reads facts the live inspection collected and the declared
// options, and returns every reason mutation is refused, sorted by discovery. No reason means the
// plan may proceed. Incomplete facts always refuse. It never reads the clock, the network or Docker.
func evaluate(facts catalogFacts, o options, marker string) []string {
	if !facts.complete() {
		return []string{"the catalog inspection is incomplete: not every connectable database was inspected"}
	}
	var refusals []string
	refusals = append(refusals, checkRoles(facts, o, marker)...)
	refusals = append(refusals, checkDatabases(facts, o, marker)...)
	refusals = append(refusals, checkAccess(facts, o)...)
	return refusals
}

// checkRoles refuses a declared role that exists and is not ours, and memberships that give a managed
// role access nothing declares.
func checkRoles(facts catalogFacts, o options, marker string) []string {
	var out []string
	declared := o.allRoles()
	for _, name := range o.declaredRoles() {
		r, exists := facts.role(name)
		if !exists {
			continue
		}
		if r.Comment != marker {
			out = append(out, fmt.Sprintf("role %q exists and is not marked as created by this instance; it is never adopted by name", name))
			continue
		}
		for _, m := range facts.Members {
			if m.Member == name && !declared[m.Role] {
				out = append(out, fmt.Sprintf("role %q is a member of undeclared role %q", name, m.Role))
			}
		}
	}
	return out
}

// checkDatabases refuses a declared database that exists without our marker or with another owner, and
// a schema of a database of ours that exists without our marker (public of our own database excepted).
func checkDatabases(facts catalogFacts, o options, marker string) []string {
	var out []string
	for _, name := range o.databaseNames() {
		d, exists := facts.database(name)
		if !exists {
			continue
		}
		if d.Comment != marker {
			out = append(out, fmt.Sprintf("database %q exists without this instance's marker; it is an unexplained object and is never adopted by name", name))
			continue
		}
		if d.Owner != o.Databases[name].Owner {
			out = append(out, fmt.Sprintf("database %q is owned by another role than the declared migration user", name))
		}
		for _, sname := range sortedKeys(o.Databases[name].Schemas) {
			s, found := d.schema(sname)
			if found && s.Comment != marker && sname != "public" {
				out = append(out, fmt.Sprintf("schema %q of database %q exists without this instance's marker", sname, name))
			}
		}
		out = append(out, ownerRisks(d, o, name)...)
	}
	return out
}

// ownerRisks refuses a schema of a managed database that a runtime identity owns.
func ownerRisks(d databaseFact, o options, name string) []string {
	var out []string
	for _, s := range d.Schemas {
		if u, ok := o.Users[s.Owner]; ok && u.Purpose == purposeRuntime {
			out = append(out, fmt.Sprintf("schema %q of database %q is owned by runtime user %q", s.Name, name, s.Owner))
		}
	}
	return out
}

// checkAccess evaluates effective connect access of every declared role, existing or prospective, on
// every connectable database the declaration does not explain: direct, inherited, ownership and PUBLIC.
func checkAccess(facts catalogFacts, o options) []string {
	var out []string
	for _, d := range facts.Databases {
		if !d.AllowConn {
			continue
		}
		if _, managed := o.Databases[d.Name]; managed {
			continue
		}
		for _, role := range o.declaredRoles() {
			if via := connectVia(facts, o, role, d); via != "" && !builtinPublic(d, via) {
				out = append(out, fmt.Sprintf("declared role %q would reach database %q (%s), which the declaration does not grant", role, d.Name, via))
			}
		}
	}
	return out
}

// builtinPublic reports whether the access is only the PUBLIC default of a built-in database.
func builtinPublic(d databaseFact, via string) bool {
	return via == "PUBLIC" && slices.Contains(builtinDatabases(), d.Name)
}

// connectVia returns how role would be able to connect to d, or "". Direct, ownership, PUBLIC and
// inherited access are all checked; membership is the existing one plus the declared one.
func connectVia(facts catalogFacts, o options, role string, d databaseFact) string {
	reach := reachable(facts, o, role)
	if slices.Contains(reach, d.Owner) {
		return "ownership"
	}
	for _, a := range d.ACL {
		if a.Priv != "CONNECT" {
			continue
		}
		switch {
		case a.Grantee == "":
			return "PUBLIC"
		case a.Grantee == role:
			return "direct"
		case slices.Contains(reach, a.Grantee):
			return "inherited"
		}
	}
	return ""
}

// reachable returns the roles whose privileges role inherits: itself, its existing memberships that
// inherit, and its declared memberships, followed transitively.
func reachable(facts catalogFacts, o options, role string) []string {
	seen := []string{role}
	queue := []string{role}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		var next []string
		for _, m := range facts.Members {
			if m.Member == cur && m.Inherit {
				next = append(next, m.Role)
			}
		}
		next = append(next, o.declaredMembership(cur)...)
		for _, n := range next {
			if !slices.Contains(seen, n) {
				seen = append(seen, n)
				queue = append(queue, n)
			}
		}
	}
	return seen
}

// refusalError joins the refusals into one error that names no value.
func refusalError(refusals []string) error {
	if len(refusals) == 0 {
		return nil
	}
	err := ErrPreflight
	for _, r := range refusals {
		err = fmt.Errorf("%w; %s", err, r)
	}
	return err
}
