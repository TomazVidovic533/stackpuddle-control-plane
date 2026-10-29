//go:build integration

package provisioning_test

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// killedRun runs the scenario in a separate process and requires that SIGKILL ended it.
func (s *stack) killedRun(killRole string) {
	s.T.Helper()
	data, err := json.Marshal(scenario{Env: s.Env, Name: s.Name, KillRole: killRole})
	if err != nil {
		s.T.Fatal(err)
	}
	cmd := testkit.Command("submit", string(data))
	out, runErr := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		s.T.Fatalf("the helper was not killed: %v\n%s", runErr, out)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		s.T.Fatalf("the helper did not die from SIGKILL: %v\n%s", runErr, out)
	}
}

func TestProvisioning_AKillAfterCREATEROLEBeforeTheJournalIsObservedOnRerunWithNoDuplicateAndNoRotation(t *testing.T) {
	s := newStack(t)
	s.killedRun("catalog_app")

	// The native effect happened: the role is committed and marked, but the engine never journaled it.
	scope := s.ScopeFile().ScopeUID.String()
	if got := s.admin("postgres", `SELECT coalesce(shobj_description(oid, 'pg_authid'), '') FROM pg_roles WHERE rolname = 'catalog_app'`); got != "stackpuddle:"+scope+"/db" {
		t.Fatalf("the role after the kill carries marker %q, want the committed native effect", got)
	}
	open := s.StateFile().Operation
	if open == nil {
		t.Fatal("the dead process left no open operation")
	}
	var started string
	for _, step := range open.Steps {
		if step.Status == state.StepStarted {
			started = step.Object
		}
	}
	if started != "module/db/role/catalog-app" {
		t.Fatalf("the started step = %q, want the user whose CREATE ROLE was committed", started)
	}
	oidBefore := s.admin("postgres", `SELECT oid FROM pg_authid WHERE rolname = 'catalog_app'`)
	verifierBefore := s.admin("postgres", `SELECT rolpassword FROM pg_authid WHERE rolname = 'catalog_app'`)
	versionBefore := s.CredentialVersion("db/catalog-app-password")

	// The engine blocks a module step it cannot settle itself; the caller cancels it, and the next run
	// observes the server and finishes the rest.
	result, err := s.Up()
	if err == nil || result.Operation == nil || result.Operation.Status != state.OpBlocked || result.Operation.ID != open.ID {
		t.Fatalf("rerun = %+v, %v; want the open operation held blocked", result.Operation, err)
	}
	if err := s.Core.Engine.Cancel(t.Context(), s.Scope(), open.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	s.MustUp()

	if got := s.admin("postgres", `SELECT count(*) FROM pg_roles WHERE rolname = 'catalog_app'`); got != "1" {
		t.Errorf("%s roles named catalog_app, want exactly one", got)
	}
	if got := s.admin("postgres", `SELECT oid FROM pg_authid WHERE rolname = 'catalog_app'`); got != oidBefore {
		t.Errorf("the role was recreated: oid %s, was %s", got, oidBefore)
	}
	if got := s.admin("postgres", `SELECT rolpassword FROM pg_authid WHERE rolname = 'catalog_app'`); got != verifierBefore {
		t.Error("the retry rotated the role's credential")
	}
	if got := s.CredentialVersion("db/catalog-app-password"); got != versionBefore {
		t.Errorf("the secret rotated from %s to %s", versionBefore, got)
	}
	if got := s.admin("postgres", `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'catalog\_%'`); got != "5" {
		t.Errorf("%s managed roles after the retry, want 5", got)
	}
	if out, err := s.asUser("catalog_app", "db/catalog-app-password", "catalog", "SELECT 1"); err != nil || out != "1" {
		t.Errorf("the original password no longer works after the retry: %v %q", err, out)
	}
}

func TestProvisioning_ADatabaseWithoutItsMarkerBlocksAndIsNeverAdoptedByName(t *testing.T) {
	s := newStack(t)
	s.MustUp()
	s.admin("postgres", `COMMENT ON DATABASE catalog IS NULL`)
	ownerBefore := s.admin("postgres", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'catalog'`)
	rolesBefore := s.snapshot()

	_, err := s.Up()
	if err == nil || !strings.Contains(err.Error(), "never adopted by name") {
		t.Fatalf("up = %v, want a refusal naming the unmarked database", err)
	}
	if got := s.admin("postgres", `SELECT coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname = 'catalog'`); got != "" {
		t.Errorf("the database was marked after the refusal: %q", got)
	}
	if got := s.admin("postgres", `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'catalog'`); got != ownerBefore {
		t.Errorf("the refused run changed the owner to %s", got)
	}
	if after := s.snapshot(); after != rolesBefore {
		t.Error("the refused run changed roles")
	}
}
