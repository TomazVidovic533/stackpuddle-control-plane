//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

const canary = "canary-secret-8d41c2e9a0b7"

// absentEverywhere fails for every file under dir that holds the value, and for the output of docker
// inspect of every container whose name carries the stack's name.
func absentEverywhere(t *testing.T, s *stack, value string, dirs ...string) {
	t.Helper()
	for _, dir := range append([]string{s.root}, dirs...) {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if data, readErr := os.ReadFile(path); readErr == nil && bytes.Contains(data, []byte(value)) {
				t.Errorf("%s holds the secret value", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range lines(docker(t, "ps", "-a", "-q", "--filter", "name="+s.name)) {
		if out := docker(t, "inspect", id); strings.Contains(out, value) {
			t.Errorf("docker inspect of %s holds the secret value", id)
		}
	}
}

func TestComposeReceivesSecretsOnlyThroughTheChildEnvironment(t *testing.T) {
	s := newStack(t)
	project := s.name + "-secrets"
	api := plan.Workload{
		Name: "api", Type: plan.TypeService, Image: testkit.BusyboxImage, Command: []string{"sleep", "3600"},
		// A model value that looks like a reference to the secret variable must arrive literally.
		Env:   []plan.EnvVar{{Name: "LITERAL", Value: "${SP_SECRET_1}"}},
		Files: []plan.File{{Path: "/run/secrets/token", Source: plan.Source{Kind: plan.SourceSecret, Ref: "input/token"}}},
	}
	call := port.Call{
		Scope:         plan.ScopeNames{ScopeUID: "sc-" + strings.ToUpper(strings.TrimPrefix(s.name, namePrefix)), ComposeProject: project, ProjectDir: s.root, Realizations: map[string]string{"service/api": "rz-TEST"}},
		DockerContext: "default",
		Topology:      plan.Topology{Workloads: []plan.Workload{api}},
		Secrets:       map[string]port.SecretValue{"input/token": secrets.NewValue([]byte(canary))},
	}
	rendered, err := compose.Render(call.Scope, call.Topology)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(rendered.Compose, []byte(canary)) {
		t.Fatal("the rendered Compose file holds the secret value")
	}
	if _, err := s.adapter().Apply(context.Background(), call, plan.StartService{Workload: "api"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	container := project + "-api-1"
	info := inspectContainer(t, container)
	for _, m := range info.Mounts {
		if m.Type == "bind" {
			t.Errorf("the container has a host bind mount of %s", m.Source)
		}
	}
	if got := docker(t, "exec", container, "printenv", "LITERAL"); got != "${SP_SECRET_1}" {
		t.Errorf("a model value that names the secret variable reached the container as %q", got)
	}
	if got := docker(t, "exec", container, "cat", "/run/secrets/token"); got != canary {
		t.Errorf("the secret file holds %q", got)
	}
	if mode := docker(t, "exec", container, "stat", "-c", "%a", "/run/secrets/token"); mode != "400" {
		t.Errorf("the secret file mode = %s", mode)
	}
	absentEverywhere(t, s, canary)
}

func TestADriverSecretReachesTheContainerThroughAProbeEnvironmentOnly(t *testing.T) {
	s := newStack(t)
	core, err := testkit.NewCore(testkit.CoreOptions{Env: s.env, Catalog: drivers.Catalog()})
	if err != nil {
		t.Fatal(err)
	}
	o := testkit.ShopOptions{Instances: []string{"primary"}, NoJob: true}
	project := testkit.Shop(t, s.opts(o))
	name, _ := model.ParseProjectName(s.name)
	environment, _ := model.ParseEnvironmentName("local")
	result, err := core.Engine.Submit(t.Context(), operation.Request{
		Scope: operation.Scope{Project: name, Environment: environment}, Project: project, ProjectDir: s.projectDir(o),
		Command: state.CommandUp, All: true,
	})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if result.Operation.Status != state.OpSucceeded {
		t.Fatalf("status = %v", result.Operation.Status)
	}
	var sawSecretStep, sawModuleStep bool
	for _, step := range result.Operation.Steps {
		sawSecretStep = sawSecretStep || step.Action == plan.ActionSecretCreate
		sawModuleStep = sawModuleStep || step.Action == "kv-store.key"
	}
	if !sawSecretStep || !sawModuleStep {
		t.Fatalf("steps = %+v", result.Operation.Steps)
	}

	scope, st := s.scopeFile(), s.stateFile()
	cred := st.Accepted.Credentials["primary/admin"]
	ref, _ := secrets.ParseRef("primary/admin")
	store, err := core.Provider.Open(t.Context(), secrets.ScopeRef{UID: scope.ScopeUID, KeyID: scope.KeyID})
	if err != nil {
		t.Fatal(err)
	}
	value, err := store.Resolve(t.Context(), ref, cred.Version)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(value.Reveal())

	store1 := scope.ComposeProject + "-primary--store-1"
	if got := docker(t, "exec", store1, "cat", "/tmp/admin-key"); got != generated {
		t.Errorf("the container holds %q, want the generated secret", got)
	}
	absentEverywhere(t, s, generated)
}
