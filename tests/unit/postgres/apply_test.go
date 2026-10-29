package postgres_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
)

const verifierText = "SCRAM-SHA-256$4096:c2FsdA==$c3RvcmVk:c2VydmVy"

func step(t *testing.T, action, object string, params any, secret *module.SecretNeed) module.Step {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return module.Step{Action: action, Object: object, Params: raw, Secret: secret}
}

func apply(t *testing.T, s module.Step, results ...module.ProbeResult) (module.ApplyResponse, error) {
	t.Helper()
	return postgres.Driver{}.Apply(t.Context(), module.ApplyRequest{Instance: instance(optionsText), Step: s, Results: results})
}

func ok(id, stdout string) module.ProbeResult { return module.ProbeResult{ID: id, Stdout: stdout} }

type roleParams = map[string]any

func userStep(t *testing.T) module.Step {
	return step(t, "postgres.role", "role/app-runtime",
		roleParams{"name": "app_runtime", "login": true, "memberOf": []string{"app_writer"}, "declared": []string{"app_migrator", "app_runtime", "app_writer"}},
		&module.SecretNeed{Ref: "primary/app-runtime-password", Size: 32})
}

func onlyProbe(t *testing.T, resp module.ApplyResponse) module.Probe {
	t.Helper()
	if len(resp.Probes) != 1 || resp.Outcome != nil {
		t.Fatalf("response = %+v, want exactly one probe", resp)
	}
	return resp.Probes[0]
}

func TestApplyRole_ACreatedUserCarriesItsMarkerAndOnlyTheVerifierBinding(t *testing.T) {
	t.Parallel()
	check := onlyProbe(t, mustApply(t, userStep(t)))
	if check.User != "postgres" || check.Kind != module.ProbeExec || check.Target != "primary--server" {
		t.Errorf("admin probe = %+v, want exec as postgres in the instance's server", check)
	}
	if got := strings.Join(check.Argv, " "); !strings.HasPrefix(got, "psql -X -v ON_ERROR_STOP=1") {
		t.Errorf("argv = %q, want psql with ON_ERROR_STOP", got)
	}
	create := onlyProbe(t, mustApply(t, userStep(t), ok("check", `[false,""]`)))
	if create.ID != "mutate" {
		t.Fatalf("probe id = %q", create.ID)
	}
	wantBinding := module.SecretBinding{Name: "SP_PG_VERIFIER", Ref: "primary/app-runtime-password", Form: module.FormSCRAMSHA256}
	if len(create.SecretEnv) != 1 || create.SecretEnv[0] != wantBinding {
		t.Errorf("secret binding = %+v, want the verifier form only", create.SecretEnv)
	}
	sql := create.Stdin
	order := []string{
		"BEGIN;", "SET LOCAL log_statement = 'none';", "SET LOCAL log_min_error_statement = 'panic';",
		`CREATE ROLE "app_runtime" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT PASSWORD :'verifier';`,
		`COMMENT ON ROLE "app_runtime" IS :'marker';`,
		`GRANT "app_writer" TO "app_runtime" WITH INHERIT TRUE, SET FALSE, ADMIN FALSE;`, "COMMIT;",
	}
	at := 0
	for _, want := range order {
		i := strings.Index(sql[at:], want)
		if i < 0 {
			t.Fatalf("script lacks %q after offset %d:\n%s", want, at, sql)
		}
		at += i + len(want)
	}
	if !strings.Contains(sql, `\set marker 'stackpuddle:scope-1/primary'`) || strings.Contains(sql, "SCRAM-SHA-256") {
		t.Errorf("the marker must be set once and no verifier may be in the script:\n%s", sql)
	}
}

func mustApply(t *testing.T, s module.Step, results ...module.ProbeResult) module.ApplyResponse {
	t.Helper()
	resp, err := apply(t, s, results...)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return resp
}

func TestApplyRole_APermissionRoleCannotLoginAndNeedsNoSecret(t *testing.T) {
	t.Parallel()
	s := step(t, "postgres.role", "role/app-writer", roleParams{"name": "app_writer", "declared": []string{"app_writer"}}, nil)
	create := onlyProbe(t, mustApply(t, s, ok("check", `[false,""]`)))
	if !strings.Contains(create.Stdin, `CREATE ROLE "app_writer" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS INHERIT;`) || len(create.SecretEnv) != 0 {
		t.Errorf("permission role script = %q with %v", create.Stdin, create.SecretEnv)
	}
}

func TestApplyRole_ARoleOfOursIsReconciledAndNeverGetsANewPassword(t *testing.T) {
	t.Parallel()
	probe := onlyProbe(t, mustApply(t, userStep(t), ok("check", `[true,"`+marker+`"]`)))
	if probe.ID != "reconcile" || len(probe.SecretEnv) != 0 || strings.Contains(probe.Stdin, "PASSWORD") || strings.Contains(probe.Stdin, "CREATE ROLE") {
		t.Errorf("reconcile probe %q with %v and script:\n%s", probe.ID, probe.SecretEnv, probe.Stdin)
	}
	if !strings.Contains(probe.Stdin, `REVOKE "app_migrator" FROM "app_runtime";`) || !strings.Contains(probe.Stdin, `NOSUPERUSER`) {
		t.Errorf("reconcile must remove undeclared memberships and reset attributes:\n%s", probe.Stdin)
	}
}

func TestApplyRole_ARoleWithoutOurMarkerIsNeverAdopted(t *testing.T) {
	t.Parallel()
	for _, comment := range []string{"", "stackpuddle:scope-9/other", "someone else"} {
		resp, err := apply(t, userStep(t), ok("check", `[true,"`+comment+`"]`))
		if !errors.Is(err, postgres.ErrPreflight) || len(resp.Probes) != 0 || resp.Outcome != nil {
			t.Errorf("comment %q: response %+v, error %v; want a refusal", comment, resp, err)
		}
	}
}

func TestApplyRole_AMissingSecretFailsBeforeAnythingIsCreated(t *testing.T) {
	t.Parallel()
	s := userStep(t)
	s.Secret = nil
	resp, err := apply(t, s, ok("check", `[false,""]`))
	if err == nil || len(resp.Probes) != 0 {
		t.Fatalf("response %+v, error %v; want a failure with no create probe", resp, err)
	}
}

func TestApplyRole_NamesThatCouldBreakOutOfAnIdentifierAreRefused(t *testing.T) {
	t.Parallel()
	for _, name := range []string{`a"b`, "a\x00b", "", "Upper", "a;drop", "-d"} {
		s := step(t, "postgres.role", "role/x", roleParams{"name": name, "declared": []string{}}, nil)
		resp, err := apply(t, s)
		if err == nil || len(resp.Probes) != 0 {
			t.Errorf("name %q: response %+v, error %v; want a refusal before any probe", name, resp, err)
		}
		if _, err = apply(t, s, ok("check", `[false,""]`)); err == nil {
			t.Errorf("name %q reached a create script", name)
		}
	}
}

func TestApply_AVerifierInProbeOutputNeverReachesAnError(t *testing.T) {
	t.Parallel()
	failed := module.ProbeResult{ID: "mutate", ExitCode: 3, Stderr: "ERROR:  syntax error\nSTATEMENT:  CREATE ROLE x PASSWORD '" + verifierText + "'"}
	_, err := apply(t, userStep(t), failed)
	if err == nil {
		t.Fatal("a failed create succeeded")
	}
	if strings.Contains(err.Error(), "SCRAM-SHA-256") || strings.Contains(err.Error(), "c3RvcmVk") {
		t.Errorf("the error echoes the verifier: %v", err)
	}
	check := module.ProbeResult{ID: "check", ExitCode: 2, Stderr: "LINE 1: " + verifierText}
	if _, err = apply(t, userStep(t), check); err == nil || strings.Contains(err.Error(), "SCRAM-SHA-256") {
		t.Errorf("the check error echoes the verifier: %v", err)
	}
}

func dbParams() map[string]any {
	return map[string]any{"name": "catalog", "database": map[string]any{
		"owner": "app_migrator", "connectRoles": []string{"app_writer"}, "connectionLimit": 25,
		"settings": map[string]any{"statementTimeoutMs": 30000}, "schemas": map[string]any{},
		"userSettings": map[string]any{"app_runtime": map[string]any{"lockTimeoutMs": 2000}},
	}}
}

func TestApplyDatabase_CreateRunsOutsideATransactionAndTheMarkerFollows(t *testing.T) {
	t.Parallel()
	s := step(t, "postgres.database", "database/catalog", dbParams(), nil)
	create := onlyProbe(t, mustApply(t, s, ok("check", `[false,"",""]`)))
	if !strings.Contains(create.Stdin, `CREATE DATABASE "catalog" OWNER "app_migrator" TEMPLATE template0;`) || strings.Contains(create.Stdin, "BEGIN") {
		t.Errorf("create script must be one statement outside a transaction:\n%s", create.Stdin)
	}
	mark := onlyProbe(t, mustApply(t, s, ok("mutate", "")))
	for _, want := range []string{
		"BEGIN;", `COMMENT ON DATABASE "catalog" IS :'marker';`, `REVOKE ALL ON DATABASE "catalog" FROM PUBLIC;`,
		`GRANT CONNECT ON DATABASE "catalog" TO "app_writer";`, `ALTER DATABASE "catalog" CONNECTION LIMIT 25;`,
		`ALTER DATABASE "catalog" SET statement_timeout = 30000;`, `ALTER ROLE "app_runtime" IN DATABASE "catalog" SET lock_timeout = 2000;`, "COMMIT;",
	} {
		if !strings.Contains(mark.Stdin, want) {
			t.Errorf("mark script lacks %q:\n%s", want, mark.Stdin)
		}
	}
	resp := mustApply(t, s, ok("mark", ""))
	if resp.Outcome == nil || !resp.Outcome.Applied || len(resp.Outcome.Mappings) != 1 {
		t.Errorf("outcome = %+v", resp.Outcome)
	}
}

func TestApplyDatabase_ADatabaseWithoutItsMarkerBlocksAndIsNeverAdopted(t *testing.T) {
	t.Parallel()
	s := step(t, "postgres.database", "database/catalog", dbParams(), nil)
	for _, found := range []string{`[true,"app_migrator",""]`, `[true,"app_migrator","stackpuddle:scope-9/other"]`, `[true,"postgres","` + marker + `"]`} {
		resp, err := apply(t, s, ok("check", found))
		if !errors.Is(err, postgres.ErrPreflight) || len(resp.Probes) != 0 {
			t.Errorf("found %s: response %+v, error %v; want a refusal", found, resp, err)
		}
	}
	probe := onlyProbe(t, mustApply(t, s, ok("check", `[true,"app_migrator","`+marker+`"]`)))
	if probe.ID != "mark" || strings.Contains(probe.Stdin, "CREATE DATABASE") {
		t.Errorf("an existing database of ours must only be re-marked, got %q", probe.ID)
	}
}

func TestApplySchema_PublicIsClaimedOnlyInADatabaseOfOurs(t *testing.T) {
	t.Parallel()
	s := step(t, "postgres.schema", "schema/catalog/public", map[string]any{"database": "catalog", "name": "public", "owner": "app_migrator"}, nil)
	claim := onlyProbe(t, mustApply(t, s, ok("check", `[true,"pg_database_owner","standard public schema","`+marker+`"]`)))
	if claim.Argv[len(claim.Argv)-1] != "catalog" || !strings.Contains(claim.Stdin, `ALTER SCHEMA "public" OWNER TO "app_migrator";`) ||
		!strings.Contains(claim.Stdin, `COMMENT ON SCHEMA "public" IS :'marker';`) || !strings.Contains(claim.Stdin, `REVOKE EXECUTE ON ALL ROUTINES IN SCHEMA "public" FROM PUBLIC;`) {
		t.Errorf("claim script in %v:\n%s", claim.Argv, claim.Stdin)
	}
	if _, err := apply(t, s, ok("check", `[true,"pg_database_owner","standard public schema","not ours"]`)); !errors.Is(err, postgres.ErrPreflight) {
		t.Errorf("public of a foreign database was claimed: %v", err)
	}
	other := step(t, "postgres.schema", "schema/catalog/app", map[string]any{"database": "catalog", "name": "app", "owner": "app_migrator"}, nil)
	if _, err := apply(t, other, ok("check", `[true,"app_migrator","","`+marker+`"]`)); !errors.Is(err, postgres.ErrPreflight) {
		t.Errorf("an unmarked schema other than public was adopted: %v", err)
	}
	create := onlyProbe(t, mustApply(t, other, ok("check", `[false,"","",""]`)))
	if !strings.Contains(create.Stdin, `CREATE SCHEMA "app" AUTHORIZATION "app_migrator";`) {
		t.Errorf("create script:\n%s", create.Stdin)
	}
}

func TestApplyGrants_ExistingObjectsAndFutureObjectsAreSeparateStepsThatNameTheCreator(t *testing.T) {
	t.Parallel()
	params := map[string]any{
		"database": "catalog", "schema": "public", "role": "app_writer", "creator": "app_migrator",
		"grant": map[string]any{"tables": []string{"SELECT", "INSERT"}, "sequences": []string{"USAGE"}},
	}
	existing := onlyProbe(t, mustApply(t, step(t, "postgres.grant", "grant/catalog/public/app-writer", params, nil)))
	for _, want := range []string{`GRANT USAGE ON SCHEMA "public" TO "app_writer";`, `GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA "public" TO "app_writer";`, `GRANT USAGE ON ALL SEQUENCES IN SCHEMA "public" TO "app_writer";`} {
		if !strings.Contains(existing.Stdin, want) {
			t.Errorf("existing-object script lacks %q", want)
		}
	}
	if strings.Contains(existing.Stdin, "DEFAULT PRIVILEGES") {
		t.Error("the existing-object step also sets default privileges")
	}
	future := onlyProbe(t, mustApply(t, step(t, "postgres.default-privilege", "default-privilege/catalog/public/app-writer", params, nil)))
	if !strings.Contains(future.Stdin, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_migrator" IN SCHEMA "public" GRANT SELECT, INSERT ON TABLES TO "app_writer";`) || strings.Contains(future.Stdin, "ON ALL TABLES") {
		t.Errorf("default-privilege script:\n%s", future.Stdin)
	}
	routines := onlyProbe(t, mustApply(t, step(t, "postgres.default-privilege", "default-privilege/catalog/routines", map[string]any{"database": "catalog", "creator": "app_migrator", "grant": map[string]any{}}, nil)))
	if !strings.Contains(routines.Stdin, `ALTER DEFAULT PRIVILEGES FOR ROLE "app_migrator" REVOKE EXECUTE ON ROUTINES FROM PUBLIC;`) {
		t.Errorf("routines script:\n%s", routines.Stdin)
	}
}

func TestApplyGrants_APrivilegeOutsideTheSchemaEnumIsRefused(t *testing.T) {
	t.Parallel()
	params := map[string]any{
		"database": "catalog", "schema": "public", "role": "app_writer", "creator": "app_migrator",
		"grant": map[string]any{"tables": []string{"SELECT; DROP"}},
	}
	if resp, err := apply(t, step(t, "postgres.grant", "grant/catalog/public/app-writer", params, nil)); err == nil || len(resp.Probes) != 0 {
		t.Errorf("response %+v, error %v; want a refusal", resp, err)
	}
}

func TestApplyAdmin_TheSuperuserPasswordIsClearedOnlyAfterTheLivePreflightAllowsIt(t *testing.T) {
	t.Parallel()
	s := step(t, "postgres.admin", "admin/server", struct{}{}, nil)
	ready := onlyProbe(t, mustApply(t, s))
	if ready.ID != "ready" || !strings.Contains(strings.Join(ready.Argv, " "), "pg_isready") {
		t.Errorf("first probe = %+v, want the readiness wait", ready)
	}
	first := onlyProbe(t, mustApply(t, s, ok("ready", "")))
	if first.ID != "catalog-1" || strings.Contains(first.Stdin, "PASSWORD") {
		t.Errorf("after readiness the driver must read the catalog, got %q", first.ID)
	}
	raw, err := json.Marshal(fresh())
	if err != nil {
		t.Fatal(err)
	}
	round2 := mustApply(t, s, ok("catalog-1", string(raw)))
	if len(round2.Probes) != 2 || round2.Probes[1].ID != "catalog-dbs" || !strings.Contains(round2.Probes[1].Stdin, `\connect 'postgres'`) || strings.Contains(round2.Probes[1].Stdin, "template0") {
		t.Fatalf("round two = %+v, want the cluster and the connectable databases only", round2.Probes)
	}
	dbLine := func(name string) string { return `{"db":"` + name + `","schemas":[],"defaults":[]}` }
	lock := onlyProbe(t, mustApply(t, s, ok("catalog-2", string(raw)), ok("catalog-dbs", dbLine("postgres")+"\n"+dbLine("template1")+"\n")))
	if lock.ID != "lock" || !strings.Contains(lock.Stdin, "ALTER ROLE postgres PASSWORD NULL;") || !strings.Contains(lock.Stdin, "SET LOCAL log_statement = 'none';") {
		t.Errorf("lock probe = %+v", lock)
	}
	if resp := mustApply(t, s, ok("lock", "")); resp.Outcome == nil || !resp.Outcome.Applied {
		t.Errorf("outcome = %+v", resp.Outcome)
	}

	dirty := fresh()
	dirty.Databases = append(dirty.Databases, database{Name: "legacy", Owner: "postgres", AllowConn: true, Inspected: true, ACL: publicConnect()})
	rawDirty, err := json.Marshal(dirty)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := apply(t, s, ok("catalog-2", string(rawDirty)), ok("catalog-dbs", dbLine("postgres")+"\n"+dbLine("template1")+"\n"+dbLine("legacy")+"\n"))
	if !errors.Is(err, postgres.ErrPreflight) || len(refused.Probes) != 0 {
		t.Errorf("a cluster with undeclared access: response %+v, error %v; want a refusal before the lock", refused, err)
	}
	if _, err = apply(t, s, ok("catalog-2", string(raw)), ok("catalog-dbs", dbLine("postgres")+"\n")); !errors.Is(err, postgres.ErrPreflight) {
		t.Errorf("a missing database line (incomplete inspection) was accepted: %v", err)
	}
	if _, err = apply(t, s, module.ProbeResult{ID: "ready", ExitCode: 1}); err == nil {
		t.Error("a server that never became ready was accepted")
	}
}
