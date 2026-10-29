//go:build integration

package provisioning_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// secretRefs are the instance secrets of the sample project ("db" is the instance key).
func refs() []string {
	return []string{"db/catalog-app-password", "db/catalog-ro-password", "db/catalog-migration-password"}
}

func TestProvisioning_StartsAuthenticatesPersistsAndReconcilesSafely(t *testing.T) {
	s := newStack(t)
	s.MustUp()

	// Startup, administrator authentication over the in-container socket, and no stored admin password.
	if got := s.admin("postgres", "SELECT 1"); got != "1" {
		t.Fatalf("admin over the socket: %q", got)
	}
	if got := s.admin("postgres", "SELECT rolpassword IS NULL FROM pg_authid WHERE rolname = 'postgres'"); got != "t" {
		t.Errorf("the superuser still has a password (rolpassword IS NULL = %q)", got)
	}
	if _, err := s.asUser("postgres", refs()[0], "postgres", "SELECT 1"); err == nil {
		t.Error("the superuser logged in over TCP")
	}

	// Safe attributes, markers and membership options.
	attrs := s.admin("postgres", `SELECT string_agg(rolname || ':' || rolsuper || rolcreatedb || rolcreaterole || rolreplication || rolbypassrls || ':' || coalesce(shobj_description(oid, 'pg_authid'), ''), ',' ORDER BY rolname) FROM pg_roles WHERE rolname LIKE 'catalog\_%'`)
	scope := s.ScopeFile().ScopeUID.String()
	for _, name := range []string{"catalog_app", "catalog_migrator", "catalog_ro", "catalog_writer", "catalog_reader"} {
		want := name + ":falsefalsefalsefalsefalse:stackpuddle:" + scope + "/db"
		if !strings.Contains(attrs, want) {
			t.Errorf("role %s lacks safe attributes and its marker; catalog: %s", name, attrs)
		}
	}
	if got := s.admin("postgres", `SELECT string_agg(m.rolname || '>' || r.rolname || ':' || a.inherit_option || a.set_option || a.admin_option, ',' ORDER BY m.rolname) FROM pg_auth_members a JOIN pg_roles m ON m.oid = a.member JOIN pg_roles r ON r.oid = a.roleid WHERE m.rolname LIKE 'catalog\_%'`); got != "catalog_app>catalog_writer:truefalsefalse,catalog_ro>catalog_reader:truefalsefalse" {
		t.Errorf("memberships = %q, want inherit-only grants", got)
	}
	if got := s.admin("postgres", `SELECT pg_get_userbyid(datdba) || ':' || datconnlimit || ':' || coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname = 'catalog'`); got != "catalog_migrator:25:stackpuddle:"+scope+"/db" {
		t.Errorf("database = %q", got)
	}
	if got := s.admin("catalog", `SELECT pg_get_userbyid(nspowner) || ':' || coalesce(obj_description(oid, 'pg_namespace'), '') FROM pg_namespace WHERE nspname = 'public'`); got != "catalog_migrator:stackpuddle:"+scope+"/db" {
		t.Errorf("schema = %q", got)
	}
	if got := s.admin("postgres", `SELECT has_database_privilege('catalog_app', 'catalog', 'CONNECT')::text || has_database_privilege('catalog_app', 'catalog', 'CREATE')::text`); got != "truefalse" {
		t.Errorf("connect/create on catalog = %q", got)
	}

	// No published admin port and no POSTGRES_PASSWORD in the container's configuration.
	var infos []struct {
		Config          struct{ Env []string }
		NetworkSettings struct {
			Ports map[string][]struct{ HostPort string }
		}
	}
	if err := json.Unmarshal([]byte(docker(t, "inspect", s.server())), &infos); err != nil || len(infos) != 1 {
		t.Fatalf("inspect: %v", err)
	}
	for port, bindings := range infos[0].NetworkSettings.Ports {
		if len(bindings) != 0 {
			t.Errorf("port %s is published: %v", port, bindings)
		}
	}
	for _, env := range infos[0].Config.Env {
		if strings.HasPrefix(env, "POSTGRES_PASSWORD") {
			t.Errorf("the container configuration holds %s", strings.SplitN(env, "=", 2)[0])
		}
	}

	// Intended grants on future objects; denied actions observed as SQLSTATE 42501 rejections.
	migrator, ro, app := refs()[2], refs()[1], refs()[0]
	if out, err := s.asUser("catalog_migrator", migrator, "catalog", "CREATE TABLE items (id serial PRIMARY KEY, note text); CREATE TABLE secrets_t (v text); REVOKE ALL ON secrets_t FROM catalog_writer, catalog_reader;"); err != nil {
		t.Fatalf("migrator DDL: %v\n%s", err, out)
	}
	if out, err := s.asUser("catalog_app", app, "catalog", "INSERT INTO items (note) VALUES ('kept'); SELECT count(*) FROM items;"); err != nil || !strings.HasSuffix(out, "1") {
		t.Errorf("the writer cannot use a future table: %v\n%s", err, out)
	}
	if out, err := s.asUser("catalog_ro", ro, "catalog", "SELECT count(*) FROM items"); err != nil || out != "1" {
		t.Errorf("the reader cannot read a future table: %v\n%s", err, out)
	}
	denied := []struct{ name, user, ref, sql string }{
		{"the reader writing", "catalog_ro", ro, "INSERT INTO items (note) VALUES ('x')"},
		{"the writer creating an object", "catalog_app", app, "CREATE TABLE mine (v int)"},
		{"the writer dropping the migrator's table", "catalog_app", app, "DROP TABLE items"},
		{"the writer assuming a role", "catalog_app", app, "SET ROLE catalog_writer"},
		{"the writer assuming the migrator", "catalog_app", app, "SET ROLE catalog_migrator"},
		{"the writer granting its own role", "catalog_app", app, "GRANT catalog_writer TO catalog_ro"},
		{"the writer creating a role", "catalog_app", app, "CREATE ROLE sneaky"},
	}
	for _, d := range denied {
		out, err := s.asUser(d.user, d.ref, "catalog", d.sql)
		if err == nil || !strings.Contains(out, "42501") {
			t.Errorf("%s: want a 42501 rejection, got err=%v out=%q", d.name, err, out)
		}
	}

	// An existing object the migrator made without the default grant stays denied until the declared
	// grant is applied to existing objects again.
	if out, err := s.asUser("catalog_ro", ro, "catalog", "SELECT 1 FROM secrets_t"); err == nil || !strings.Contains(out, "42501") {
		t.Errorf("the reader read an object it was never granted: %v %q", err, out)
	}

	// Repeat execution preserves credentials, role identity and data.
	before, versions := s.snapshot(), map[string]string{}
	for _, ref := range refs() {
		versions[ref] = s.CredentialVersion(ref)
	}
	second := s.MustUp()
	if after := s.snapshot(); after != before {
		t.Errorf("a repeat up changed the server's identity or credentials:\nbefore %s\nafter  %s", before, after)
	}
	for _, ref := range refs() {
		if got := s.CredentialVersion(ref); got != versions[ref] {
			t.Errorf("credential %s rotated from %s to %s", ref, versions[ref], got)
		}
	}
	if got := s.admin("catalog", "SELECT count(*) FROM items"); got != "1" {
		t.Errorf("data after the repeat up: %s", got)
	}
	if out, err := s.asUser("catalog_ro", ro, "catalog", "SELECT count(*) FROM secrets_t"); err != nil || out != "0" {
		t.Errorf("the declared grant did not reach the existing object: %v %q", err, out)
	}
	for _, step := range second.Operation.Steps {
		if step.Action == "postgres.role" || step.Action == "postgres.database" || step.Action == "postgres.schema" {
			t.Errorf("a repeat up planned %s %s", step.Action, step.Object)
		}
	}

	// Restart persistence: the server restarts and a further up finds everything in place.
	docker(t, "restart", s.server())
	s.MustUp()
	if after := s.snapshot(); after != before {
		t.Errorf("a restart changed the server's identity or credentials:\nbefore %s\nafter  %s", before, after)
	}
	if got := s.admin("catalog", "SELECT note FROM items"); got != "kept" {
		t.Errorf("data after the restart: %q", got)
	}

	// Leak test: no password and no verifier in the state, inspect, logs or the operation record.
	record, err := json.Marshal(second.Operation)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs() {
		password := s.Secret(ref)
		s.absentEverywhere(password)
		if strings.Contains(string(record), password) {
			t.Errorf("the operation record holds the password of %s", ref)
		}
	}
	for _, verifier := range lines(s.admin("postgres", "SELECT rolpassword FROM pg_authid WHERE rolname LIKE 'catalog\\_%' AND rolpassword IS NOT NULL")) {
		s.absentEverywhere(verifier)
		if strings.Contains(string(record), verifier) {
			t.Error("the operation record holds a SCRAM verifier")
		}
	}
}

// snapshot is the identity and credentials of the roles: their OIDs and stored verifiers change if a
// role is recreated or a password rotated.
func (s *stack) snapshot() string {
	s.T.Helper()
	return s.admin("postgres", `SELECT string_agg(oid || ':' || rolname || ':' || coalesce(rolpassword, ''), ',' ORDER BY rolname) FROM pg_authid WHERE rolname LIKE 'catalog\_%'`)
}

func TestProvisioning_AMissingSecretFailsSafelyWithNoPartialSetup(t *testing.T) {
	s := newStack(t)
	s.Rebuild(ghost{})
	result, err := s.Up()
	if err == nil {
		t.Fatal("a step whose secret is missing succeeded")
	}
	if result.Operation == nil || result.Operation.Status != state.OpFailed {
		t.Fatalf("operation = %+v, want failed", result.Operation)
	}
	if strings.Contains(err.Error(), "SCRAM-SHA-256") {
		t.Errorf("the error leaks a verifier: %v", err)
	}
	if got := s.admin("postgres", `SELECT count(*) FROM pg_roles WHERE rolname IN ('catalog_app', 'catalog_ro', 'catalog_migrator')`); got != "0" {
		t.Errorf("%s login users exist after a refused secret", got)
	}
	if got := s.admin("postgres", `SELECT count(*) FROM pg_database WHERE datname = 'catalog'`); got != "0" {
		t.Error("the database exists although no user could be created")
	}
	// The same project with the real driver then converges: nothing was half-applied that blocks it.
	s.Rebuild(postgres.Driver{})
	s.MustUp()
	if got := s.admin("postgres", `SELECT count(*) FROM pg_roles WHERE rolname IN ('catalog_app', 'catalog_ro', 'catalog_migrator')`); got != "3" {
		t.Errorf("%s login users exist after the rerun, want 3", got)
	}
}

// ghost is the real driver whose user creation binds a secret that was never created.
type ghost struct{ postgres.Driver }

func (g ghost) Apply(ctx context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	resp, err := g.Driver.Apply(ctx, r)
	for i := range resp.Probes {
		for j := range resp.Probes[i].SecretEnv {
			resp.Probes[i].SecretEnv[j].Ref = "db/ghost-password"
		}
	}
	return resp, err
}

// loud is the real driver with two changes: once the admin step is done it turns full statement
// logging on, and the first login user's CREATE ROLE, which carries the verifier, is made to fail itself:
// the role is created first, in a separate transaction, so the statement that binds the verifier errors.
type loud struct {
	postgres.Driver
	enable func()
	failed *bool
}

func (l loud) Apply(ctx context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	resp, err := l.Driver.Apply(ctx, r)
	if err != nil {
		return resp, err
	}
	if r.Step.Action == "postgres.admin" && resp.Outcome != nil {
		l.enable()
	}
	for i, p := range resp.Probes {
		if p.ID == "mutate" && len(p.SecretEnv) > 0 && !*l.failed {
			*l.failed = true
			resp.Probes[i].Stdin = preCreate(p.Stdin) + p.Stdin
		}
	}
	return resp, nil
}

// preCreate returns a script that, outside the transaction under test, creates the role the script's
// CREATE ROLE statement names, so that statement fails with "already exists" while it carries the verifier.
func preCreate(stdin string) string {
	for _, line := range strings.Split(stdin, "\n") {
		if rest, ok := strings.CutPrefix(line, "CREATE ROLE "); ok {
			name, _, _ := strings.Cut(rest, " ")
			return "CREATE ROLE " + name + " NOLOGIN;\n"
		}
	}
	return ""
}

func TestProvisioning_NoVerifierReachesTheServerLogEvenWithFullStatementLoggingAndAFailingStatement(t *testing.T) {
	var s *stack
	failed := false
	enable := func() {
		s.admin("postgres", `ALTER SYSTEM SET log_statement = 'all'; ALTER SYSTEM SET log_min_error_statement = 'error'; ALTER SYSTEM SET log_min_duration_statement = 0; SELECT pg_reload_conf();`)
	}
	s = newStack(t, loud{enable: enable, failed: &failed})
	if _, err := s.Up(); err == nil {
		t.Fatal("the up with a failing statement succeeded")
	}
	if !failed {
		t.Fatal("no login user was created, so no verifier was bound")
	}
	// The server logs statements now: a canary statement must appear, or the check below proves nothing.
	s.admin("postgres", `SELECT 'logging-is-live'`)
	logs := func() string {
		out, _ := dockerErr("logs", s.server())
		return out
	}
	if !strings.Contains(logs(), "logging-is-live") {
		t.Fatal("full statement logging is not active, so the verifier check proves nothing")
	}
	if !strings.Contains(logs(), "already exists") {
		t.Fatal("the failing CREATE ROLE never reached the server")
	}
	if strings.Contains(logs(), "SCRAM-SHA-256$") {
		t.Error("the server log holds a SCRAM verifier after a failed statement")
	}
	// The role the wrapper created to force the failure is not the engine's; remove it before the rerun.
	s.admin("postgres", `DROP ROLE IF EXISTS catalog_app, catalog_ro, catalog_migrator`)
	// The same project then converges under full logging, and its log still holds no verifier or password.
	s.Rebuild(postgres.Driver{})
	s.MustUp()
	if strings.Contains(logs(), "SCRAM-SHA-256$") {
		t.Error("the server log holds a SCRAM verifier after the converging up")
	}
	for _, ref := range refs() {
		if strings.Contains(logs(), s.Secret(ref)) {
			t.Errorf("the server log holds the password of %s", ref)
		}
	}
}
