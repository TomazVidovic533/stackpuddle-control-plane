//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func TestAMinimalLabelledServiceStartsInTheScopesComposeProject(t *testing.T) {
	s := newStack(t)
	op := s.mustUp(testkit.ShopOptions{Instances: []string{}, NoJob: true})
	if op.Status() != engine.StatusSucceeded {
		t.Fatalf("status = %v", op.Status())
	}
	scope, st := s.scopeFile(), s.stateFile()
	if want := s.name + "-local-" + scope.ScopeUID.Suffix(); scope.ComposeProject != want {
		t.Fatalf("compose project = %q, want %q", scope.ComposeProject, want)
	}
	names := s.containers()
	if len(names) != 1 {
		t.Fatalf("containers = %v", names)
	}
	info := inspectContainer(t, names[0])
	labels := info.Config.Labels
	if !info.State.Running || labels["com.docker.compose.project"] != scope.ComposeProject || labels["io.stackpuddle.scope"] != scope.ScopeUID.String() {
		t.Errorf("container %s running=%v labels=%v", names[0], info.State.Running, labels)
	}
	if labels["io.stackpuddle.realization"] != st.Realizations["service/api"].UID.String() {
		t.Errorf("realization label %q, state says %q", labels["io.stackpuddle.realization"], st.Realizations["service/api"].UID)
	}
	if got := st.Realizations["service/api"].Mappings; len(got) != 1 || got[0].ID != info.ID {
		t.Errorf("mappings = %+v, container %s", got, info.ID)
	}
}

func TestTwoInstancesOfOneProjectLocalDefinitionStartWithoutACollision(t *testing.T) {
	s := newStack(t)
	s.mustUp(testkit.ShopOptions{NoJob: true})
	scope := s.scopeFile()
	want := []string{
		scope.ComposeProject + "-api-1",
		scope.ComposeProject + "-primary--store-1",
		scope.ComposeProject + "-reporting--store-1",
	}
	got := s.containers()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("containers = %v, want %v", got, want)
	}
	for _, name := range got {
		if !inspectContainer(t, name).State.Running {
			t.Errorf("%s is not running", name)
		}
	}
	volumes := s.volumes()
	slices.Sort(volumes)
	wantVolumes := []string{scope.ComposeProject + "_api--uploads", scope.ComposeProject + "_primary--store--data", scope.ComposeProject + "_reporting--store--data"}
	if !slices.Equal(volumes, wantVolumes) {
		t.Errorf("volumes = %v, want %v", volumes, wantVolumes)
	}
	mounts := map[string]string{}
	for _, name := range []string{want[1], want[2]} {
		for _, m := range inspectContainer(t, name).Mounts {
			mounts[name] = m.Source
		}
	}
	if mounts[want[1]] == mounts[want[2]] {
		t.Errorf("both instances mount the same volume: %v", mounts)
	}
}

func TestTwoScopesCreatedInTheSameSecondGetDifferentProjectNames(t *testing.T) {
	first, second := newStack(t), newStack(t)
	second.name = first.name // the same declared project and environment, in two state roots
	instant := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, s := range []*stack{first, second} {
		eng, err := engine.New(engine.Options{LookupEnv: testkit.LookupEnv(s.env), Now: func() time.Time { return instant }})
		if err != nil {
			t.Fatal(err)
		}
		s.eng = eng
		s.mustUp(testkit.ShopOptions{Instances: []string{}, NoJob: true})
	}
	a, b := first.scopeFile(), second.scopeFile()
	if a.ComposeProject == b.ComposeProject {
		t.Fatalf("two scopes share the compose project %q", a.ComposeProject)
	}
	if a.ScopeUID == b.ScopeUID {
		t.Fatal("two scopes share a scope uid")
	}
	for _, s := range []*stack{first, second} {
		if got := s.containers(); len(got) != 1 || !inspectContainer(t, got[0]).State.Running {
			t.Errorf("containers of %s = %v", s.scopeFile().ComposeProject, got)
		}
	}
}

func TestAmbientDockerAndComposeSettingsAreIgnored(t *testing.T) {
	s := newStack(t)
	ambient := map[string]string{
		"DOCKER_HOST": "unix:///nonexistent/docker.sock", "DOCKER_CONTEXT": "does-not-exist", "DOCKER_TLS_VERIFY": "1",
		"DOCKER_CERT_PATH": "/nonexistent", "COMPOSE_PROJECT_NAME": "hijacked", "COMPOSE_FILE": "/nonexistent/compose.yaml",
	}
	for k, v := range ambient {
		s.env[k] = v
	}
	s.rebuild()
	// The test process itself carries them too, while the engine runs, so a child that inherited the
	// caller's environment would be caught.
	for k, v := range ambient {
		t.Setenv(k, v)
	}
	op := s.mustUp(testkit.ShopOptions{Instances: []string{}, NoJob: true})
	for k := range ambient {
		os.Unsetenv(k)
	}
	if op.Status() != engine.StatusSucceeded {
		t.Fatalf("status = %v", op.Status())
	}
	if got := s.containers(); len(got) != 1 || !strings.HasPrefix(got[0], s.scopeFile().ComposeProject) {
		t.Errorf("containers = %v", got)
	}
	if hijacked := docker(t, "ps", "-a", "--filter", "name=hijacked", "--format", "{{.Names}}"); hijacked != "" {
		t.Errorf("the ambient COMPOSE_PROJECT_NAME was honoured: %s", hijacked)
	}
}

func TestAChangedEngineIDStopsWithTargetReplacedAndCreatesNothing(t *testing.T) {
	s := newStack(t)
	s.mustUp(testkit.ShopOptions{Instances: []string{}, NoJob: true})
	before := docker(t, "ps", "-a", "--format", "{{.Names}} {{.ID}}", "--filter", "name="+s.name)

	path := filepath.Join(s.envDir(), "scope.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := s.scopeFile()
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), scope.EngineID, "a-different-engine", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = s.up(testkit.ShopOptions{Instances: []string{}, NoJob: true, APIEnv: map[string]string{"X": "1"}})
	if code := codeOf(t, err); code != engine.CodeTargetReplaced {
		t.Fatalf("error = %v (%v)", code, err)
	}
	if after := docker(t, "ps", "-a", "--format", "{{.Names}} {{.ID}}", "--filter", "name="+s.name); after != before {
		t.Errorf("containers changed:\n%s\n->\n%s", before, after)
	}
}

// deadContext creates a Docker context on a dead socket in a private config folder and returns its name.
func deadContext(t *testing.T, s *stack) string {
	t.Helper()
	config := filepath.Join(s.root, "docker-config")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "dead-" + strings.TrimPrefix(s.name, namePrefix)
	docker(t, "--config", config, "context", "create", name, "--docker", "host=unix:///nonexistent/stackpuddle-dead.sock")
	s.env["DOCKER_CONFIG"] = config
	s.rebuild()
	return name
}

func TestRefusalsHappenBeforeAnyDockerCallWithTheContextOnADeadSocket(t *testing.T) {
	s := newStack(t)
	live := testkit.ShopOptions{Instances: []string{}, NoJob: true}
	s.mustUp(live) // a real scope and state exist, created through the live context
	dead := deadContext(t, s)
	o := live
	o.DockerContext = dead
	o.APIEnv = map[string]string{"EDIT": "1"}

	// Baseline: with everything valid the dead socket is what fails, so every refusal below that is
	// not target_unreachable happened before Docker was asked.
	if _, err := s.up(o); codeOf(t, err) != engine.CodeTargetUnreachable {
		t.Fatalf("baseline error = %v", err)
	}

	statePath := filepath.Join(s.envDir(), "state.json")
	good, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(string) string{
		"stateVersion 2": func(in string) string { return strings.Replace(in, `"stateVersion": 1`, `"stateVersion": 2`, 1) },
		"an unknown field": func(in string) string {
			return strings.Replace(in, `"stateVersion": 1`, `"stateVersion": 1, "surprise": true`, 1)
		},
		"an unknown enum": func(in string) string { return strings.Replace(in, `"state": "active"`, `"state": "melted"`, 1) },
	} {
		bad := edit(string(good))
		if bad == string(good) {
			t.Fatalf("%s: the edit changed nothing", name)
		}
		if err := os.WriteFile(statePath, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := s.up(o)
		if code := codeOf(t, err); code != engine.CodeStateVersionUnsupported {
			t.Errorf("%s: error = %v (%v)", name, code, err)
		}
		if after, _ := os.ReadFile(statePath); string(after) != bad {
			t.Errorf("%s: the bytes changed", name)
		}
	}
	if err := os.WriteFile(statePath, good, 0o600); err != nil {
		t.Fatal(err)
	}

	s.env["STACKPUDDLE_SECRETS_KEY"] = testkit.Key("some other key")
	s.rebuild()
	if _, err := s.up(o); codeOf(t, err) != engine.CodeKeyMismatch {
		t.Errorf("a different key: %v", err)
	}
	delete(s.env, "STACKPUDDLE_SECRETS_KEY")
	s.rebuild()
	if _, err := s.up(o); codeOf(t, err) != engine.CodeKeyMissing {
		t.Errorf("a missing key: %v", err)
	}
	s.env["STACKPUDDLE_SECRETS_KEY"] = "not base64"
	s.rebuild()
	if _, err := s.up(o); codeOf(t, err) != engine.CodeKeyInvalid {
		t.Errorf("an invalid key: %v", err)
	}
	s.env["STACKPUDDLE_SECRETS_KEY"] = testkit.Key("integration-" + s.name)
	s.rebuild()

	withMissing := o
	withMissing.RequireCapability = "module.mysql/1"
	if _, err := s.up(withMissing); codeOf(t, err) != engine.CodeCapabilityUnsupported {
		t.Errorf("a missing capability: %v", err)
	}
}
