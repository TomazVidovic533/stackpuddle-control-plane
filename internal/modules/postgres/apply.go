package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// probeReconcile names the round that brings an existing object of ours back to its declaration.
const probeReconcile = "reconcile"

// Apply performs one planned step. Each step first observes its own object, so a rerun after a crash
// settles by observation: what is ours and done is skipped, and an object that exists without our
// marker is refused, never adopted by name.
func (Driver) Apply(_ context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	marker, err := markerFor(r.Instance.Scope, r.Instance.Key)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	a := applier{inst: r.Instance, step: r.Step, results: r.Results, marker: marker}
	switch r.Step.Action {
	case actionAdmin:
		return a.admin()
	case actionRole:
		return a.role()
	case actionDB:
		return a.database()
	case actionSchema:
		return a.schema()
	case actionGrant:
		return a.grant()
	case actionDefault:
		return a.defaultPrivilege()
	}
	return module.ApplyResponse{}, fmt.Errorf("postgres: unknown action %q", r.Step.Action)
}

type applier struct {
	inst    module.Instance
	step    module.Step
	results []module.ProbeResult
	marker  string
}

func (a applier) probe(p module.Probe) module.ApplyResponse {
	return module.ApplyResponse{Probes: []module.Probe{p}}
}

func (a applier) done(evidence string, mappings ...module.Mapping) module.ApplyResponse {
	return module.ApplyResponse{Outcome: &module.Outcome{Applied: true, Evidence: evidence, Mappings: mappings}}
}

// last returns the single result of the previous round.
func (a applier) last() (module.ProbeResult, bool) {
	if len(a.results) == 0 {
		return module.ProbeResult{}, false
	}
	return a.results[0], true
}

func decodeParams[T any](step module.Step) (T, error) {
	var p T
	if err := decodeStrict(string(step.Params), &p); err != nil {
		return p, fmt.Errorf("postgres: step %s params: %w", step.Object, err)
	}
	return p, nil
}

// admin waits for the server, runs the live preflight and, only if it allows mutation, clears the
// superuser password.
func (a applier) admin() (module.ApplyResponse, error) {
	o, err := parseOptions(a.inst.Options)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	first, ok := a.last()
	if !ok {
		return a.probe(readyProbe(a.inst, o.StartupTimeoutSeconds)), nil
	}
	if first.ID == probeLock {
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("clearing the superuser password", first)
		}
		return a.done("preflight passed; the superuser password is cleared"), nil
	}
	if first.ID == probeReady {
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("waiting for the server", first)
		}
		first = module.ProbeResult{}
	}
	ins := inspect(a.inst, a.inspectionResults(first))
	if !ins.done {
		return module.ApplyResponse{Probes: ins.probes}, nil
	}
	if ins.status != accessOK {
		return module.ApplyResponse{}, fmt.Errorf("%w: the server is %s: %s", ErrPreflight, ins.status, ins.reason)
	}
	if err := refusalError(evaluate(ins.facts, o, a.marker)); err != nil {
		return module.ApplyResponse{}, err
	}
	return a.probe(adminProbe(a.inst, adminCall{ID: probeLock, Database: adminDatabase, SQL: lockAdminScript()})), nil
}

// inspectionResults passes the results of the inspection rounds on; the readiness round has none.
func (a applier) inspectionResults(first module.ProbeResult) []module.ProbeResult {
	if first.ID == "" {
		return nil
	}
	return a.results
}

type presence struct {
	exists  bool
	owner   string
	comment string
	dbMark  string
}

// parsePresence reads the JSON array a check query printed: [exists, comment] for a role,
// [exists, owner, comment] for a database, [exists, owner, comment, databaseComment] for a schema.
func parsePresence(kind, out string) (presence, error) {
	var raw []json.RawMessage
	if err := decodeStrict(out, &raw); err != nil {
		return presence{}, fmt.Errorf("postgres: %s check output: %w", kind, err)
	}
	var p presence
	fields := []any{&p.exists}
	switch kind {
	case "role":
		fields = append(fields, &p.comment)
	case "database":
		fields = append(fields, &p.owner, &p.comment)
	default:
		fields = append(fields, &p.owner, &p.comment, &p.dbMark)
	}
	if len(raw) != len(fields) {
		return presence{}, fmt.Errorf("postgres: %s check output has %d fields", kind, len(raw))
	}
	for i, f := range fields {
		if err := json.Unmarshal(raw[i], f); err != nil {
			return presence{}, fmt.Errorf("postgres: %s check output: %w", kind, err)
		}
	}
	return p, nil
}

func checkScript(name, query string) (string, error) {
	var s script
	s.setName("name", name)
	s.raw(query + "\n")
	return s.String()
}

func (a applier) role() (module.ApplyResponse, error) {
	p, err := decodeParams[roleParams](a.step)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	if _, err := managedName(p.Name); err != nil {
		return module.ApplyResponse{}, err
	}
	first, ok := a.last()
	if !ok {
		sql, err := checkScript(p.Name, roleCheckSQL)
		if err != nil {
			return module.ApplyResponse{}, err
		}
		return a.probe(adminProbe(a.inst, adminCall{ID: probeCheck, Database: adminDatabase, SQL: sql})), nil
	}
	mapping := module.Mapping{Kind: actionRole, Name: p.Name}
	switch first.ID {
	case probeCheck:
		return a.roleDecision(p, first)
	case probeMutate, probeReconcile:
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("provisioning role "+p.Name, first)
		}
		return a.done("role "+p.Name+" provisioned", mapping), nil
	}
	return module.ApplyResponse{}, fmt.Errorf("postgres: unexpected result %q", first.ID)
}

func (a applier) roleDecision(p roleParams, check module.ProbeResult) (module.ApplyResponse, error) {
	if check.ExitCode != 0 {
		return module.ApplyResponse{}, probeError("checking role "+p.Name, check)
	}
	found, err := parsePresence("role", check.Stdout)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	switch {
	case !found.exists:
		return a.createRole(p)
	case found.comment == a.marker:
		sql, err := reconcileRoleScript(p)
		if err != nil {
			return module.ApplyResponse{}, err
		}
		return a.probe(adminProbe(a.inst, adminCall{ID: probeReconcile, Database: adminDatabase, SQL: sql})), nil
	}
	return module.ApplyResponse{}, fmt.Errorf("%w: role %q exists and is not marked as created by this instance; it is not adopted by name", ErrPreflight, p.Name)
}

func (a applier) createRole(p roleParams) (module.ApplyResponse, error) {
	sql, err := createRoleScript(p, a.marker)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	var env []module.SecretBinding
	if p.Login {
		if a.step.Secret == nil {
			return module.ApplyResponse{}, fmt.Errorf("postgres: user %q has no secret in its step; nothing was created", p.Name)
		}
		env = append(env, module.SecretBinding{Name: verifierEnv, Ref: a.step.Secret.Ref, Form: module.FormSCRAMSHA256})
	}
	return a.probe(adminProbe(a.inst, adminCall{ID: probeMutate, Database: adminDatabase, SQL: sql, Env: env})), nil
}

func (a applier) database() (module.ApplyResponse, error) {
	p, err := decodeParams[databaseParams](a.step)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	if _, err := managedName(p.Name); err != nil {
		return module.ApplyResponse{}, err
	}
	first, ok := a.last()
	if !ok {
		sql, err := checkScript(p.Name, databaseCheckSQL)
		if err != nil {
			return module.ApplyResponse{}, err
		}
		return a.probe(adminProbe(a.inst, adminCall{ID: probeCheck, Database: adminDatabase, SQL: sql})), nil
	}
	switch first.ID {
	case probeCheck:
		return a.databaseDecision(p, first)
	case probeMutate:
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("creating database "+p.Name, first)
		}
		return a.markDatabase(p)
	case probeMark:
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("marking database "+p.Name, first)
		}
		return a.done("database "+p.Name+" provisioned", module.Mapping{Kind: actionDB, Name: p.Name}), nil
	}
	return module.ApplyResponse{}, fmt.Errorf("postgres: unexpected result %q", first.ID)
}

func (a applier) databaseDecision(p databaseParams, check module.ProbeResult) (module.ApplyResponse, error) {
	if check.ExitCode != 0 {
		return module.ApplyResponse{}, probeError("checking database "+p.Name, check)
	}
	found, err := parsePresence("database", check.Stdout)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	switch {
	case !found.exists:
		sql, err := createDatabaseScript(p.Name, p.Database.Owner)
		if err != nil {
			return module.ApplyResponse{}, err
		}
		return a.probe(adminProbe(a.inst, adminCall{ID: probeMutate, Database: adminDatabase, SQL: sql})), nil
	case found.comment == a.marker && found.owner == p.Database.Owner:
		return a.markDatabase(p)
	}
	return module.ApplyResponse{}, fmt.Errorf("%w: database %q exists without this instance's marker or with another owner; it is an unexplained object and is not adopted by name", ErrPreflight, p.Name)
}

func (a applier) markDatabase(p databaseParams) (module.ApplyResponse, error) {
	sql, err := markDatabaseScript(p.Name, a.marker, p.Database, p.Database.Owner)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	return a.probe(adminProbe(a.inst, adminCall{ID: probeMark, Database: adminDatabase, SQL: sql})), nil
}

func (a applier) schema() (module.ApplyResponse, error) {
	p, err := decodeParams[schemaParams](a.step)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	if _, err := managedName(p.Database); err != nil {
		return module.ApplyResponse{}, err
	}
	if _, err := managedName(p.Name); err != nil {
		return module.ApplyResponse{}, err
	}
	first, ok := a.last()
	if !ok {
		sql, err := checkScript(p.Name, schemaCheckSQL)
		if err != nil {
			return module.ApplyResponse{}, err
		}
		return a.probe(adminProbe(a.inst, adminCall{ID: probeCheck, Database: p.Database, SQL: sql})), nil
	}
	switch first.ID {
	case probeCheck:
		return a.schemaDecision(p, first)
	case probeMutate:
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("provisioning schema "+p.Name, first)
		}
		return a.done("schema "+p.Name+" provisioned", module.Mapping{Kind: actionSchema, Name: p.Database + "/" + p.Name}), nil
	}
	return module.ApplyResponse{}, fmt.Errorf("postgres: unexpected result %q", first.ID)
}

func (a applier) schemaDecision(p schemaParams, check module.ProbeResult) (module.ApplyResponse, error) {
	if check.ExitCode != 0 {
		return module.ApplyResponse{}, probeError("checking schema "+p.Name, check)
	}
	found, err := parsePresence("schema", check.Stdout)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	// public exists in every new database; a database this instance created has it claimed.
	claim := found.exists && (found.comment == a.marker || p.Name == "public" && found.dbMark == a.marker)
	if found.exists && !claim {
		return module.ApplyResponse{}, fmt.Errorf("%w: schema %q exists without this instance's marker; it is not adopted by name", ErrPreflight, p.Name)
	}
	build := createSchemaScript
	if found.exists {
		build = claimSchemaScript
	}
	sql, err := build(p.Name, p.Owner, a.marker)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	return a.probe(adminProbe(a.inst, adminCall{ID: probeMutate, Database: p.Database, SQL: sql})), nil
}

func (a applier) grant() (module.ApplyResponse, error) {
	p, err := decodeParams[grantParams](a.step)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	if _, err := managedName(p.Database); err != nil {
		return module.ApplyResponse{}, err
	}
	if first, ok := a.last(); ok {
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("granting to "+p.Role, first)
		}
		return a.done("grants to " + p.Role + " on " + p.Schema + " applied"), nil
	}
	sql, err := grantScript(p.Schema, p.Role, p.Grant)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	return a.probe(adminProbe(a.inst, adminCall{ID: probeMutate, Database: p.Database, SQL: sql})), nil
}

func (a applier) defaultPrivilege() (module.ApplyResponse, error) {
	p, err := decodeParams[grantParams](a.step)
	if err != nil {
		return module.ApplyResponse{}, err
	}
	if _, err := managedName(p.Database); err != nil {
		return module.ApplyResponse{}, err
	}
	if first, ok := a.last(); ok {
		if first.ExitCode != 0 {
			return module.ApplyResponse{}, probeError("default privileges for "+p.Creator, first)
		}
		return a.done("default privileges of " + p.Creator + " applied"), nil
	}
	var sql string
	if p.Schema == "" {
		sql, err = publicRoutinesScript(p.Creator)
	} else {
		sql, err = defaultPrivilegeScript(p.Schema, p.Creator, p.Role, p.Grant)
	}
	if err != nil {
		return module.ApplyResponse{}, err
	}
	return a.probe(adminProbe(a.inst, adminCall{ID: probeMutate, Database: p.Database, SQL: sql})), nil
}
