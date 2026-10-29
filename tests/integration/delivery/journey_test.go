//go:build integration

package delivery_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

const (
	runtimeUser   = "app_runtime"
	runtimeRef    = "db/app-runtime-password"
	migratorUser  = "app_migrator"
	migratorRef   = "db/app-migrator-password"
	serverKeyRef  = "tls/server/db--server"
	authorityRef  = "tls/ca-key"
	deniedMessage = "42501"
)

// TestDelivery_TheConsumerJourneyRunsTheMigrationAsItsOwnIdentityThenTheRuntime runs the fixture once
// and checks the Done lines of the delivery on that one stack: the order, the two identities, the
// files and the keys, and that nothing secret is visible to the host, to docker inspect or to a log.
func TestDelivery_TheConsumerJourneyRunsTheMigrationAsItsOwnIdentityThenTheRuntime(t *testing.T) {
	s := testkit.NewConsumerStack(t, namePrefix, "")
	result := s.MustUp()
	op := *result.Operation
	app, migrate, server := s.Container("app"), s.Container("migrate"), s.Server()

	t.Run("the migration runs after the access steps and before the final check and the rollout", func(t *testing.T) {
		job := stepIndex(op, "compose.job", "job/migrate")
		verify := stepIndex(op, "module.verify", "module/db")
		start := stepIndex(op, "compose.service", "service/app")
		serverStart := stepIndex(op, "compose.service", "service/db--server")
		access := -1
		for i, step := range op.Steps {
			if strings.HasPrefix(step.Action, "postgres.") {
				access = i
			}
		}
		if serverStart < 0 || access < serverStart || job < access || verify < job || start < verify {
			t.Errorf("order: server %d, last access step %d, migration %d, verification %d, rollout %d in %v", serverStart, access, job, verify, start, op.Steps)
		}
		if got := op.Steps[job]; got.Job == nil || got.Job.ExitCode != 0 || got.Status != state.StepCompleted {
			t.Errorf("job step = %+v, want completed with exit code 0", got)
		}
		if got := s.Admin("app", "SELECT string_agg(applied_by, ',') FROM schema_history"); got != migratorUser {
			t.Errorf("the migration ran as %q, want %s", got, migratorUser)
		}
	})

	t.Run("runtime and migration use different credentials and the runtime cannot migrate or administer", func(t *testing.T) {
		if s.Secret(runtimeRef) == s.Secret(migratorRef) {
			t.Fatal("the two bindings share a password")
		}
		runtime := func(sql string) (string, error) {
			return s.Connect(runtimeUser, runtimeRef, "db--server", "verify-full").Run(t.Context(), sql)
		}
		if out, err := runtime("SELECT count(*) FROM schema_history"); err != nil || out != "1" {
			t.Fatalf("the runtime cannot read what the migration made: %q, %v", out, err)
		}
		if out, err := runtime("INSERT INTO items (note) VALUES ('kept'); SELECT count(*) FROM items"); err != nil || !strings.HasSuffix(out, "1") {
			t.Fatalf("the runtime cannot use its granted tables: %q, %v", out, err)
		}
		for name, sql := range map[string]string{
			"create a table":              "CREATE TABLE denied (id int)",
			"drop the migration's table":  "DROP TABLE items",
			"assume the migration role":   "SET ROLE app_migrator",
			"create a database":           "CREATE DATABASE denied",
			"create a role":               "CREATE ROLE sneaky",
			"change a role":               "ALTER ROLE app_runtime SUPERUSER",
			"grant itself the other role": "GRANT app_migrator TO app_runtime",
		} {
			if out, err := runtime(sql); err == nil || !strings.Contains(out, deniedMessage) {
				t.Errorf("the runtime binding tried to %s: err=%v out=%q, want a %s rejection", name, err, out, deniedMessage)
			}
		}
		migration := func(sql string) (string, error) {
			return s.Connect(migratorUser, migratorRef, "db--server", "verify-full").Run(t.Context(), sql)
		}
		if out, err := migration("CREATE TABLE migrated_later (id int); DROP TABLE migrated_later"); err != nil {
			t.Errorf("the migration binding cannot change the schema it owns: %v %s", err, out)
		}
		for name, sql := range map[string]string{
			"create a role":     "CREATE ROLE sneaky",
			"create a database": "CREATE DATABASE denied",
			"become superuser":  "ALTER ROLE app_migrator SUPERUSER",
		} {
			if out, err := migration(sql); err == nil || !strings.Contains(out, deniedMessage) {
				t.Errorf("the migration binding tried to %s: err=%v out=%q, want a %s rejection", name, err, out, deniedMessage)
			}
		}
	})

	t.Run("credentials and the CA arrive as files with the declared owner and mode and the password is the runtime's", func(t *testing.T) {
		if got, want := execIn(t, app, "stat", "-c", "%a %u:%g %n", "/run/sp/password", "/run/sp/ca.crt"), "400 999:999 /run/sp/password\n444 0:0 /run/sp/ca.crt"; got != want {
			t.Errorf("files =\n%s\nwant\n%s", got, want)
		}
		if got := execIn(t, app, "cat", "/run/sp/password"); got != s.Secret(runtimeRef) {
			t.Error("the delivered password is not the runtime user's")
		}
		if got := execIn(t, app, "cat", "/run/sp/ca.crt"); got != strings.TrimSpace(string(s.CA())) {
			t.Error("the delivered CA is not the environment's")
		}
		if got := testkit.Docker(t, "logs", app); !strings.Contains(got, "0") {
			t.Errorf("the app's startup query printed %q: it did not read the migrated table over verify-full", got)
		}
	})

	t.Run("an environment key exists only where the binding declares one and no password is among them", func(t *testing.T) {
		env := containerEnv(t, app)
		for key, want := range map[string]string{
			"PGHOST": "db--server", "PGPORT": "5432", "PGDATABASE": "app", "PGUSER": runtimeUser,
			"PGSSLMODE": "verify-full", "PGSSLROOTCERT": "/run/sp/ca.crt",
		} {
			if env[key] != want {
				t.Errorf("%s = %q, want %q", key, env[key], want)
			}
		}
		image := map[string]bool{"PATH": true, "GOSU_VERSION": true, "LANG": true, "PG_MAJOR": true, "PG_VERSION": true, "PGDATA": true}
		declared := map[string]bool{"PGHOST": true, "PGPORT": true, "PGDATABASE": true, "PGUSER": true, "PGSSLMODE": true, "PGSSLROOTCERT": true}
		for key := range env {
			if !declared[key] && !image[key] {
				t.Errorf("the container has the undeclared environment key %s", key)
			}
		}
		if _, ok := env["PGPASSWORD"]; ok {
			t.Error("the password is an environment key")
		}
	})

	t.Run("nothing is a host file or a bind mount; only the server's named data volume is mounted", func(t *testing.T) {
		for _, container := range []string{app, server, migrate} {
			kinds, binds := mounts(t, container)
			if slices.Contains(kinds, "bind") || slices.ContainsFunc(binds, func(b string) bool { return strings.HasPrefix(b, "/") }) {
				t.Errorf("%s has host mounts: binds %v kinds %v", container, binds, kinds)
			}
		}
	})

	t.Run("no secret value or private key is visible on the host, in docker inspect, in a log or in the record", func(t *testing.T) {
		values := []string{s.Secret(runtimeRef), s.Secret(migratorRef), s.Secret(serverKeyRef), s.Secret(authorityRef)}
		absentFromHost(t, s.Root, values...)
		absentFromDocker(t, s.Name, values[:2]...)
		record, err := json.Marshal(result.Operation)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range values {
			if strings.Contains(string(record), v) {
				t.Error("the operation record holds a secret value")
			}
		}
		// The server's key is delivered into its container at the fixed target, and exists nowhere on
		// the host but the sealed store.
		if got := execIn(t, server, "stat", "-c", "%a %u:%g", "/run/stackpuddle/tls/server.key"); got != "600 999:999" {
			t.Errorf("server key = %s", got)
		}
	})

	t.Run("an ordinary restart or a repeated up does not run the migration again", func(t *testing.T) {
		before := testkit.Docker(t, "inspect", "-f", "{{.Id}} {{.State.FinishedAt}}", migrate)
		testkit.Docker(t, "restart", app)
		testkit.Docker(t, "restart", server)
		if again, err := s.Up(); err != nil || again.Operation != nil && again.Operation.Status != state.OpSucceeded {
			t.Fatalf("up after restarts = %+v, %v", again.Operation, err)
		}
		if after := testkit.Docker(t, "inspect", "-f", "{{.Id}} {{.State.FinishedAt}}", migrate); after != before {
			t.Errorf("the migration container changed: %q -> %q", before, after)
		}
		if got := s.Admin("app", "SELECT count(*) FROM schema_history"); got != "1" {
			t.Errorf("schema_history holds %s rows, want the one migration", got)
		}
		if again, err := s.Up(); err != nil {
			t.Errorf("repeat up: %v", err)
		} else if again.Operation != nil {
			for _, step := range again.Operation.Steps {
				if step.Action == "compose.job" {
					t.Errorf("a repeat up planned %s", step.Object)
				}
			}
		}
	})
}

// TestDelivery_AFailingMigrationBlocksTheRolloutAndReportsItsExitCodeApartFromAnEngineFailure runs the
// fixture's failing-migration environment.
func TestDelivery_AFailingMigrationBlocksTheRolloutAndReportsItsExitCodeApartFromAnEngineFailure(t *testing.T) {
	s := testkit.NewConsumerStack(t, namePrefix, "failing-migration")
	result, err := s.Up()
	if err == nil {
		t.Fatal("up succeeded although the migration exits non-zero")
	}
	if f := failure(t, err); f.Code != state.CodeJobExitedNonzero || f.Category != state.CategoryConflict {
		t.Fatalf("failure = %s / %v, want %s / conflict", f.Code, f.Category, state.CodeJobExitedNonzero)
	}
	op := *result.Operation
	if op.Status != state.OpFailed {
		t.Errorf("operation status = %v", op.Status)
	}
	job := op.Steps[stepIndex(op, "compose.job", "job/migrate")]
	if job.Status != state.StepFailed || job.Job == nil || job.Job.ExitCode != 3 || job.Error == nil || job.Error.Code != state.CodeJobExitedNonzero {
		t.Errorf("job step = %+v, want a failed step holding the job's exit code 3", job)
	}
	if verify := op.Steps[stepIndex(op, "module.verify", "module/db")]; verify.Status != state.StepNotApplied {
		t.Errorf("the final check after a failed migration is %v, want not-applied", verify.Status)
	}
	if start := op.Steps[stepIndex(op, "compose.service", "service/app")]; start.Status != state.StepNotApplied {
		t.Errorf("the rollout after a failed migration is %v, want not-applied", start.Status)
	}
	if services := s.Services(); slices.Contains(services, "app") {
		t.Errorf("the consumer exists although its migration failed: %v", services)
	}
	if got := s.Admin("app", "SELECT to_regclass('schema_history') IS NULL"); got != "t" {
		t.Errorf("the failed migration changed the schema (to_regclass is null = %s)", got)
	}
}

// TestDelivery_ABindingTheDriverRefusesStopsBeforeAnyContainer covers the refusals that happen at plan
// time: the administrator role, an undeclared database and a secret mapped to an environment key.
func TestDelivery_ABindingTheDriverRefusesStopsBeforeAnyContainer(t *testing.T) {
	const runtimeBinding = "        user: app_runtime\n        database: app\n"
	const migrationEnvironment = "      parameters:\n        user: app_migrator\n        database: app\n      delivery:\n        environment:\n"
	tests := []struct {
		name string
		edit testkit.Edit
		code string
	}{
		{"the administrator role", testkit.Edit{Old: runtimeBinding, New: "        user: postgres\n        database: app\n"}, state.CodeParametersInvalid},
		{"an undeclared database", testkit.Edit{Old: runtimeBinding, New: "        user: app_runtime\n        database: postgres\n"}, state.CodeParametersInvalid},
		{"an undeclared user", testkit.Edit{Old: runtimeBinding, New: "        user: sneaky\n        database: app\n"}, state.CodeParametersInvalid},
		{"a password mapped to an environment key", testkit.Edit{Old: migrationEnvironment, New: migrationEnvironment + "          PGPASSWORD: password\n"}, state.CodeSecretInEnvironment},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testkit.NewConsumerStack(t, namePrefix, "", tc.edit)
			_, err := s.Up()
			if err == nil {
				t.Fatal("up succeeded")
			}
			if f := failure(t, err); f.Code != tc.code {
				t.Errorf("failure = %s, want %s", f.Code, tc.code)
			}
			if inventory := s.Inventory(); len(inventory) != 0 {
				t.Errorf("the refused up created objects: %v", inventory)
			}
		})
	}
}
