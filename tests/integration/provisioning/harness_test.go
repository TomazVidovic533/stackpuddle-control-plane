//go:build integration

// Package provisioning_test runs the PostgreSQL driver against a real server in a disposable Compose
// project. The module archive is built from the sibling stackpuddle tree (see testkit.PackagedModule),
// so these tests are developer coverage, narrower than the published artifact path.
package provisioning_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

const namePrefix = "sp-m01-050-"

func docker(t testing.TB, args ...string) string         { return testkit.Docker(t, args...) }
func dockerErr(args ...string) (string, error)           { return testkit.DockerErr(args...) }
func dockerStdin(in string, a ...string) (string, error) { return testkit.DockerStdin(in, a...) }
func lines(s string) []string                            { return testkit.Lines(s) }

func project(name string) (model.Project, error) {
	return testkit.PostgresProject(name, testkit.PostgresOptions)
}

func packaged() (model.ModuleDefinition, testkit.ArchiveRecord, error) {
	return testkit.BuildPackagedModule("postgres")
}

func TestMain(m *testing.M) {
	testkit.Dispatch(map[string]testkit.Helper{"submit": helperSubmit})
	os.Exit(m.Run())
}

// stack is a rig of the postgres project with the driver's SQL helpers.
type stack struct {
	*testkit.Rig
}

func newStack(t *testing.T, catalog ...module.Driver) *stack {
	t.Helper()
	testkit.RequirePostgresImage(t)
	_, record, err := packaged()
	if err != nil {
		t.Fatal(err)
	}
	t.Log(record)
	if len(catalog) == 0 {
		catalog = []module.Driver{postgres.Driver{}}
	}
	return &stack{Rig: testkit.NewRig(t, namePrefix, project, catalog...)}
}

// server returns the name of the instance's server container.
func (s *stack) server() string {
	s.T.Helper()
	return s.ScopeFile().ComposeProject + "-db--server-1"
}

// admin runs SQL in the server as postgres over the socket and returns its trimmed output.
func (s *stack) admin(database, sql string) string {
	s.T.Helper()
	out, err := s.adminErr(database, sql)
	if err != nil {
		s.T.Fatalf("admin sql %q: %v\n%s", sql, err, out)
	}
	return out
}

func (s *stack) adminErr(database, sql string) (string, error) {
	return dockerStdin(sql, "exec", "-i", "-u", "postgres", s.server(), "psql", "-X", "-v", "ON_ERROR_STOP=1", "-At", "-d", database)
}

// asUser runs SQL as a login user over TCP with its password, returning the output and the error
// text, whose SQLSTATE the denial tests read.
func (s *stack) asUser(user, secretRef, database, sql string) (string, error) {
	s.T.Helper()
	password := s.Secret(secretRef)
	return dockerStdin(sql, "exec", "-i", "-e", "PGPASSWORD="+password, "-e", "PGOPTIONS=-c client_min_messages=notice", s.server(),
		"psql", "-X", "-v", "ON_ERROR_STOP=1", "-v", "VERBOSITY=sqlstate", "-At", "-h", "127.0.0.1", "-U", user, "-d", database)
}

// absentEverywhere fails when the value appears in a file of the stack's root, in docker inspect or
// in the logs of any container of the stack.
func (s *stack) absentEverywhere(value string) {
	s.T.Helper()
	if value == "" {
		s.T.Fatal("absentEverywhere needs a value")
	}
	err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if data, readErr := os.ReadFile(path); readErr == nil && bytes.Contains(data, []byte(value)) {
			s.T.Errorf("%s holds a secret value", path)
		}
		return nil
	})
	if err != nil {
		s.T.Fatal(err)
	}
	for _, id := range lines(docker(s.T, "ps", "-a", "-q", "--filter", "name="+s.Name)) {
		if out := docker(s.T, "inspect", id); strings.Contains(out, value) {
			s.T.Errorf("docker inspect of %s holds a secret value", id)
		}
		if out, _ := dockerErr("logs", id); strings.Contains(out, value) {
			s.T.Errorf("the log of %s holds a secret value", id)
		}
	}
}

// scenario is what the helper process runs: one Submit through the real adapter with the real driver
// decorated to die with SIGKILL.
type scenario struct {
	Env  map[string]string
	Name string
	// KillRole names the role whose CREATE ROLE result ends the process before it is journaled.
	KillRole string
}

// killer is the real driver that sends itself SIGKILL once Apply receives the result of the CREATE
// ROLE probe of one role: after the native effect, before the engine journals its outcome.
type killer struct {
	postgres.Driver
	role string
}

func (k killer) Apply(ctx context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	if r.Step.Action == "postgres.role" && len(r.Results) > 0 && r.Results[0].ID == "mutate" && r.Results[0].ExitCode == 0 &&
		strings.Contains(string(r.Step.Params), `"name":"`+k.role+`"`) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return k.Driver.Apply(ctx, r)
}

func helperSubmit(args []string) int {
	var sc scenario
	if err := json.Unmarshal([]byte(args[0]), &sc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	core, err := testkit.NewCore(testkit.CoreOptions{Env: sc.Env, Catalog: []module.Driver{killer{role: sc.KillRole}}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	loaded, err := project(sc.Name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	name, _ := model.ParseProjectName(sc.Name)
	environment, _ := model.ParseEnvironmentName("local")
	_, err = core.Engine.Submit(context.Background(), operation.Request{
		Scope: operation.Scope{Project: name, Environment: environment}, Project: loaded, ProjectDir: sc.Env["STACKPUDDLE_STATE_DIR"],
		Command: state.CommandUp, All: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
