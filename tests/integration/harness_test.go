//go:build integration

package integration_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

const namePrefix = "sp-m01-040-"

// stack is one disposable scope: its own state root, its own project name, and a cleanup that removes
// only what carries that project's name or scope label.
type stack struct {
	t    *testing.T
	root string
	name string
	env  map[string]string
	eng  *engine.Engine
}

func randomName(t testing.TB) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return namePrefix + hex.EncodeToString(b)
}

func newStack(t *testing.T) *stack {
	t.Helper()
	s := &stack{t: t, root: testkit.Root(t), name: randomName(t)}
	s.env = map[string]string{
		"STACKPUDDLE_STATE_DIR":   s.root,
		"STACKPUDDLE_SECRETS_KEY": testkit.Key("integration-" + s.name),
		"HOME":                    os.Getenv("HOME"),
		"PATH":                    os.Getenv("PATH"),
	}
	s.rebuild()
	t.Cleanup(s.cleanup)
	return s
}

func (s *stack) rebuild() {
	s.t.Helper()
	eng, err := engine.New(engine.Options{LookupEnv: testkit.LookupEnv(s.env)})
	if err != nil {
		s.t.Fatal(err)
	}
	s.eng = eng
}

func (s *stack) opts(o testkit.ShopOptions) testkit.ShopOptions {
	o.Name = s.name
	return o
}

func (s *stack) scope() engine.Scope {
	s.t.Helper()
	project, err := model.ParseProjectName(s.name)
	if err != nil {
		s.t.Fatal(err)
	}
	environment, err := model.ParseEnvironmentName("local")
	if err != nil {
		s.t.Fatal(err)
	}
	return engine.Scope{Project: project, Environment: environment}
}

// projectDir writes the sample project's source files (build contexts) and returns the directory.
func (s *stack) projectDir(o testkit.ShopOptions) string {
	s.t.Helper()
	dir := filepath.Join(s.root, "project")
	for name, content := range testkit.ShopFiles(s.opts(o)) {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	return dir
}

func (s *stack) request(o testkit.ShopOptions, command engine.Command) engine.SubmitRequest {
	s.t.Helper()
	return engine.SubmitRequest{
		Scope: s.scope(), Project: testkit.Shop(s.t, s.opts(o)), ProjectDir: s.projectDir(o),
		Command: command, Selection: engine.SelectAll(),
	}
}

func (s *stack) up(o testkit.ShopOptions, change ...func(*engine.SubmitRequest)) (engine.Result, error) {
	s.t.Helper()
	return s.submit(s.request(o, engine.CommandUp), change...)
}

func (s *stack) submit(req engine.SubmitRequest, change ...func(*engine.SubmitRequest)) (engine.Result, error) {
	s.t.Helper()
	for _, c := range change {
		c(&req)
	}
	return s.eng.Submit(s.t.Context(), req)
}

func (s *stack) mustUp(o testkit.ShopOptions, change ...func(*engine.SubmitRequest)) engine.Operation {
	s.t.Helper()
	result, err := s.up(o, change...)
	if err != nil {
		s.t.Fatalf("up: %v", err)
	}
	op, ok := result.Operation()
	if !ok {
		s.t.Fatal("up recorded no operation")
	}
	return op
}

func (s *stack) envDir() string { return filepath.Join(s.root, "environments", s.name+".local") }

func (s *stack) scopeFile() state.Scope {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.envDir(), "scope.json"))
	if err != nil {
		s.t.Fatal(err)
	}
	scope, err := state.ParseScope(data)
	if err != nil {
		s.t.Fatal(err)
	}
	return scope
}

func (s *stack) stateFile() state.State {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.envDir(), "state.json"))
	if err != nil {
		s.t.Fatal(err)
	}
	st, err := state.ParseState(data)
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

// docker runs the real docker CLI against the default context and returns trimmed stdout.
func docker(t testing.TB, args ...string) string {
	t.Helper()
	out, err := dockerErr(args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func dockerErr(args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String() + stderr.String()), err
}

// containers lists the names of every container of this stack's Compose project or scope.
func (s *stack) containers() []string {
	s.t.Helper()
	uid := s.scopeFile().ScopeUID.String()
	return lines(docker(s.t, "ps", "-a", "--filter", "label=io.stackpuddle.scope="+uid, "--format", "{{.Names}}"))
}

func (s *stack) volumes() []string {
	s.t.Helper()
	uid := s.scopeFile().ScopeUID.String()
	return lines(docker(s.t, "volume", "ls", "--filter", "label=io.stackpuddle.scope="+uid, "--format", "{{.Name}}"))
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

type containerInfo struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Env    []string          `json:"Env"`
	} `json:"Config"`
	State struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	} `json:"State"`
	Mounts []struct {
		Type   string `json:"Type"`
		Source string `json:"Source"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Ports map[string][]struct{ HostIp, HostPort string } `json:"Ports"`
	} `json:"NetworkSettings"`
}

func inspectContainer(t testing.TB, name string) containerInfo {
	t.Helper()
	var infos []containerInfo
	if err := json.Unmarshal([]byte(docker(t, "inspect", name)), &infos); err != nil || len(infos) != 1 {
		t.Fatalf("inspect %s: %v", name, err)
	}
	return infos[0]
}

// cleanup removes only objects of this stack: its Compose project, its labelled leftovers and
// anything whose name carries its unique project name.
func (s *stack) cleanup() {
	// Kill the containers first: a busybox sleep ignores SIGTERM, so a graceful stop would wait ten
	// seconds for each. Then let Compose remove what it owns and sweep what only the name or label finds.
	uid := ""
	var project string
	if data, err := os.ReadFile(filepath.Join(s.envDir(), "scope.json")); err == nil {
		if scope, parseErr := state.ParseScope(data); parseErr == nil {
			uid, project = scope.ScopeUID.String(), scope.ComposeProject
		}
	}
	var filters [][]string
	if uid != "" {
		filters = append(filters, []string{"--filter", "label=io.stackpuddle.scope=" + uid})
	}
	filters = append(filters, []string{"--filter", "name=" + s.name})
	for _, f := range filters {
		for _, id := range lines(mustOut(dockerErr(append([]string{"ps", "-a", "-q"}, f...)...))) {
			_, _ = dockerErr("rm", "-f", id)
		}
	}
	if project != "" {
		_, _ = dockerErr("compose", "-p", project, "down", "-v", "--remove-orphans")
	}
	for _, f := range filters {
		for _, v := range lines(mustOut(dockerErr(append([]string{"volume", "ls", "-q"}, f...)...))) {
			_, _ = dockerErr("volume", "rm", "-f", v)
		}
	}
	for _, n := range lines(mustOut(dockerErr("network", "ls", "-q", "--filter", "name="+s.name))) {
		_, _ = dockerErr("network", "rm", n)
	}
}

func mustOut(out string, _ error) string { return out }
