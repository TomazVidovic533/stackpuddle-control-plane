// Package postgres is the built-in driver of the postgres module: it provisions roles, databases,
// schemas, grants and default privileges inside the server the engine runs from the module's
// workload.
//
// The driver never holds a Docker handle. It asks for exec probes, which run psql as the postgres
// superuser over the in-container socket with peer authentication and the SQL on stdin. No admin port
// is published and no admin password is stored. Role passwords arrive as SCRAM verifiers the engine
// computes, in a probe's environment only.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// ModuleName is the module the driver serves.
const ModuleName = "postgres"

// Driver is the postgres module driver. It holds no state.
type Driver struct{}

// Describe returns the static description. Delete is not offered in m01: a vanished declaration leaves
// its objects retained.
func (Driver) Describe() module.Descriptor {
	return module.Descriptor{
		Module: ModuleName, Version: "0.1.0", Capabilities: []string{"module.postgres/1"},
		StateKinds:  []string{actionAdmin, actionRole, actionDB, actionSchema, actionGrant, actionDefault},
		ActionKinds: []string{actionAdmin, actionRole, actionDB, actionSchema, actionGrant, actionDefault},
		Deletes:     false,
		// The server workload serves encrypted connections as the postgres user (uid and gid 999 in the
		// pinned image); the engine delivers its certificate and key to the fixed module targets.
		ServerTLS: []module.ServerTLS{{Workload: "server", Listener: "sql", Owner: serverOwner}},
	}
}

// serverOwner is the uid and gid of the postgres user inside the pinned image.
const serverOwner = "999:999"

// Fact keys of an Observation.
const (
	// FactStatus is "present", "absent", "denied" or "unknown".
	FactStatus = "status"
	// FactCatalog is the JSON of the catalog facts, set when the status is present.
	FactCatalog = "catalog"
	// FactReason says why the status is unknown or denied, without echoing a value.
	FactReason = "reason"
)

// inspection is the result of the catalog inspection state machine.
type inspection struct {
	probes []module.Probe
	facts  catalogFacts
	status access
	reason string
	done   bool
}

// inspect advances the two-round catalog inspection. Round one reads the cluster; round two reads the
// cluster again and each connectable database, so the final results hold everything.
func inspect(i module.Instance, results []module.ProbeResult) inspection {
	if len(results) == 0 {
		return inspection{probes: []module.Probe{adminProbe(i, adminCall{ID: probeCluster1, Database: adminDatabase, SQL: clusterSQL()})}}
	}
	if first, ok := resultFor(results, probeCluster1); ok {
		return inspectRoundTwo(i, first)
	}
	cluster, okCluster := resultFor(results, probeCluster2)
	dbs, okDBs := resultFor(results, probeDatabases)
	if !okCluster || !okDBs {
		return inspection{done: true, status: accessUnknown, reason: "the inspection results are incomplete"}
	}
	for _, res := range []module.ProbeResult{cluster, dbs} {
		if a := classify(res); a != accessOK {
			return inspection{done: true, status: a, reason: reasonOf(res)}
		}
	}
	facts, err := parseCluster(cluster.Stdout)
	if err != nil {
		return inspection{done: true, status: accessUnknown, reason: err.Error()}
	}
	facts, err = facts.withDatabases(dbs.Stdout)
	if err != nil {
		return inspection{done: true, status: accessUnknown, reason: err.Error()}
	}
	return inspection{done: true, status: accessOK, facts: facts}
}

func inspectRoundTwo(i module.Instance, first module.ProbeResult) inspection {
	if a := classify(first); a != accessOK {
		return inspection{done: true, status: a, reason: reasonOf(first)}
	}
	facts, err := parseCluster(first.Stdout)
	if err != nil {
		return inspection{done: true, status: accessUnknown, reason: err.Error()}
	}
	dbScript, err := inspectDatabasesScript(facts.connectable())
	if err != nil {
		return inspection{done: true, status: accessUnknown, reason: err.Error()}
	}
	return inspection{probes: []module.Probe{
		adminProbe(i, adminCall{ID: probeCluster2, Database: adminDatabase, SQL: clusterSQL()}),
		adminProbe(i, adminCall{ID: probeDatabases, Database: adminDatabase, SQL: dbScript}),
	}}
}

func reasonOf(res module.ProbeResult) string {
	if res.ExitCode == notRunning {
		return "the server is not running"
	}
	return fmt.Sprintf("exit code %d: %s", res.ExitCode, strings.TrimSpace(redact(res.Stderr)))
}

// Observe reports whether the server answers and what its catalog holds. Absent (not running), denied
// (the catalog refused the read) and unknown (anything else) are told apart in Facts[FactStatus].
func (Driver) Observe(_ context.Context, r module.ObserveRequest) (module.ObserveResponse, error) {
	ins := inspect(r.Instance, r.Results)
	if !ins.done {
		return module.ObserveResponse{Probes: ins.probes}, nil
	}
	obs := module.Observation{Present: ins.status == accessOK, Facts: map[string]string{FactStatus: string(ins.status)}}
	if ins.reason != "" {
		obs.Facts[FactReason] = ins.reason
	}
	if ins.status == accessOK {
		raw, err := json.Marshal(ins.facts)
		if err != nil {
			return module.ObserveResponse{}, fmt.Errorf("encode catalog facts: %w", err)
		}
		obs.Facts[FactCatalog] = string(raw)
	}
	return module.ObserveResponse{Observation: &obs}, nil
}

// ErrObservationUnusable marks an observation a plan cannot rest on.
var ErrObservationUnusable = errors.New("the observation cannot be planned from")

// Plan validates the options, evaluates the live preflight when the server answers, and returns the
// ordered steps. A server that is not running yet is planned in full; the admin step runs the
// preflight before anything mutates. Denied, unknown and unsafe observations refuse the plan.
func (Driver) Plan(_ context.Context, r module.PlanRequest) (module.PlanResponse, error) {
	o, err := parsePlanOptions(r.Instance.Options)
	if err != nil {
		return module.PlanResponse{}, err
	}
	marker, err := markerFor(r.Instance.Scope, r.Instance.Key)
	if err != nil {
		return module.PlanResponse{}, err
	}
	var facts catalogFacts
	switch status := access(r.Observation.Facts[FactStatus]); status {
	case accessAbsent:
	case accessOK:
		if err := decodeStrict(r.Observation.Facts[FactCatalog], &facts); err != nil {
			return module.PlanResponse{}, fmt.Errorf("%w: the catalog facts do not decode", ErrObservationUnusable)
		}
		if err := refusalError(evaluate(facts, o, marker)); err != nil {
			return module.PlanResponse{}, err
		}
	case accessDenied, accessUnknown:
		return module.PlanResponse{}, unusable(r.Observation)
	default:
		return module.PlanResponse{}, unusable(r.Observation)
	}
	steps, err := buildSteps(r.Instance, o, facts, marker)
	if err != nil {
		return module.PlanResponse{}, err
	}
	return module.PlanResponse{Steps: steps}, nil
}

func unusable(o module.Observation) error {
	return fmt.Errorf("%w: the server is %q (%s); mutation is refused", ErrObservationUnusable, o.Facts[FactStatus], o.Facts[FactReason])
}

// Verify checks that the server answers and that its catalog equals the declaration: every planned
// object but the idempotent grants is already in place, and nothing undeclared is reachable.
func (Driver) Verify(_ context.Context, r module.VerifyRequest) (module.VerifyResponse, error) {
	ins := inspect(r.Instance, r.Results)
	if !ins.done {
		return module.VerifyResponse{Probes: ins.probes}, nil
	}
	if ins.status != accessOK {
		return verdict(fmt.Sprintf("the server is %s: %s", ins.status, ins.reason)), nil
	}
	o, err := parseOptions(r.Instance.Options)
	if err != nil {
		return module.VerifyResponse{}, err
	}
	marker, err := markerFor(r.Instance.Scope, r.Instance.Key)
	if err != nil {
		return module.VerifyResponse{}, err
	}
	problems := evaluate(ins.facts, o, marker)
	steps, err := buildSteps(r.Instance, o, ins.facts, marker)
	if err != nil {
		return module.VerifyResponse{}, err
	}
	for _, s := range steps {
		if s.Action != actionAdmin && s.Action != actionGrant {
			problems = append(problems, s.Object+" is not in the declared state")
		}
	}
	return verdict(problems...), nil
}

func verdict(problems ...string) module.VerifyResponse {
	v := module.Verification{OK: len(problems) == 0, Problems: problems}
	return module.VerifyResponse{Verification: &v}
}
