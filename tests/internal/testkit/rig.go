package testkit

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// Rig is one disposable scope for a Docker-backed test: its own state root, its own project name and
// a cleanup that removes only what carries that name or scope label.
type Rig struct {
	T       testing.TB
	Root    string
	Name    string
	Env     map[string]string
	Core    *Core
	Project func(name string) (model.Project, error)
	// Environment is the environment the rig acts on; "" means local. Set it before the first call.
	Environment string
}

// NewRig builds a rig whose project name starts with prefix and registers its cleanup.
func NewRig(t *testing.T, prefix string, project func(name string) (model.Project, error), catalog ...module.Driver) *Rig {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	r := &Rig{T: t, Root: Root(t), Name: prefix + hex.EncodeToString(b), Project: project}
	r.Env = map[string]string{
		"STACKPUDDLE_STATE_DIR":   r.Root,
		"STACKPUDDLE_SECRETS_KEY": Key("rig-" + r.Name),
		"HOME":                    os.Getenv("HOME"),
		"PATH":                    os.Getenv("PATH"),
	}
	r.Rebuild(catalog...)
	t.Cleanup(r.cleanup)
	return r
}

// Rebuild replaces the engine with one over the catalog, keeping the state root.
func (r *Rig) Rebuild(catalog ...module.Driver) {
	r.T.Helper()
	core, err := NewCore(CoreOptions{Env: r.Env, Catalog: catalog})
	if err != nil {
		r.T.Fatal(err)
	}
	r.Core = core
}

// Scope returns the rig's project and its environment.
func (r *Rig) Scope() operation.Scope {
	r.T.Helper()
	name, err := model.ParseProjectName(r.Name)
	if err != nil {
		r.T.Fatal(err)
	}
	environment, err := model.ParseEnvironmentName(r.environment())
	if err != nil {
		r.T.Fatal(err)
	}
	return operation.Scope{Project: name, Environment: environment}
}

// Request returns an up request for everything.
func (r *Rig) Request() operation.Request {
	r.T.Helper()
	p, err := r.Project(r.Name)
	if err != nil {
		r.T.Fatal(err)
	}
	return operation.Request{Scope: r.Scope(), Project: p, ProjectDir: r.Root, Command: state.CommandUp, All: true}
}

// Up submits an up request.
func (r *Rig) Up() (operation.Result, error) {
	r.T.Helper()
	return r.Core.Engine.Submit(r.T.Context(), r.Request())
}

// DryRun submits an up request that only plans.
func (r *Rig) DryRun() (operation.Result, error) {
	r.T.Helper()
	req := r.Request()
	req.DryRun = true
	return r.Core.Engine.Submit(r.T.Context(), req)
}

// MustUp submits an up request and requires that it succeeds.
func (r *Rig) MustUp() operation.Result {
	r.T.Helper()
	result, err := r.Up()
	if err != nil {
		r.T.Fatalf("up: %v", err)
	}
	if result.Operation == nil || result.Operation.Status != state.OpSucceeded {
		r.T.Fatalf("up did not succeed: %+v", result.Operation)
	}
	return result
}

func (r *Rig) environment() string {
	if r.Environment == "" {
		return "local"
	}
	return r.Environment
}

// EnvDir is the environment's state folder.
func (r *Rig) EnvDir() string {
	return filepath.Join(r.Root, "environments", r.Name+"."+r.environment())
}

// ScopeFile reads scope.json.
func (r *Rig) ScopeFile() state.Scope {
	r.T.Helper()
	data, err := os.ReadFile(filepath.Join(r.EnvDir(), "scope.json"))
	if err != nil {
		r.T.Fatal(err)
	}
	scope, err := state.ParseScope(data)
	if err != nil {
		r.T.Fatal(err)
	}
	return scope
}

// StateFile reads state.json.
func (r *Rig) StateFile() state.State {
	r.T.Helper()
	data, err := os.ReadFile(filepath.Join(r.EnvDir(), "state.json"))
	if err != nil {
		r.T.Fatal(err)
	}
	st, err := state.ParseState(data)
	if err != nil {
		r.T.Fatal(err)
	}
	return st
}

// Secret returns the plaintext of a secret by reading the sealed store.
func (r *Rig) Secret(ref string) string {
	r.T.Helper()
	scope := r.ScopeFile()
	parsed, err := secrets.ParseRef(ref)
	if err != nil {
		r.T.Fatal(err)
	}
	cred, ok := r.StateFile().Accepted.Credentials[ref]
	if !ok {
		r.T.Fatalf("no credential recorded for %s", ref)
	}
	store, err := r.Core.Provider.Open(r.T.Context(), secrets.ScopeRef{UID: scope.ScopeUID, KeyID: scope.KeyID})
	if err != nil {
		r.T.Fatal(err)
	}
	value, err := store.Resolve(r.T.Context(), parsed, cred.Version)
	if err != nil {
		r.T.Fatal(err)
	}
	return string(value.Reveal())
}

// CredentialVersion returns the recorded version of a secret, which a rotation would change.
func (r *Rig) CredentialVersion(ref string) string {
	r.T.Helper()
	cred, ok := r.StateFile().Accepted.Credentials[ref]
	if !ok {
		r.T.Fatalf("no credential recorded for %s", ref)
	}
	return cred.Version.String()
}

// Inventory lists the containers, volumes and networks that belong to the rig, sorted. It is the
// Docker inventory a "leaves the inventory unchanged" check compares.
func (r *Rig) Inventory() []string {
	r.T.Helper()
	var out []string
	for _, kind := range [][]string{
		{"ps", "-a", "--format", "container {{.Names}} {{.ID}} {{.State}}"},
		{"volume", "ls", "--format", "volume {{.Name}}"},
		{"network", "ls", "--format", "network {{.Name}}"},
	} {
		for _, line := range Lines(Docker(r.T, append(kind[:2:2], append([]string{"--filter", "name=" + r.Name}, kind[2:]...)...)...)) {
			out = append(out, line)
		}
	}
	slices.Sort(out)
	return out
}

// cleanup removes only what carries this rig's project name or scope label, with Compose's own down -v first.
func (r *Rig) cleanup() {
	uid, project := "", ""
	if data, err := os.ReadFile(filepath.Join(r.EnvDir(), "scope.json")); err == nil {
		if scope, parseErr := state.ParseScope(data); parseErr == nil {
			uid, project = scope.ScopeUID.String(), scope.ComposeProject
		}
	}
	if project != "" {
		_, _ = DockerErr("compose", "-p", project, "down", "-v", "--remove-orphans")
	}
	filters := [][]string{{"--filter", "name=" + r.Name}}
	if uid != "" {
		filters = append(filters, []string{"--filter", "label=io.stackpuddle.scope=" + uid})
	}
	for _, f := range filters {
		for _, id := range Lines(first(DockerErr(append([]string{"ps", "-a", "-q"}, f...)...))) {
			_, _ = DockerErr("rm", "-f", id)
		}
		for _, v := range Lines(first(DockerErr(append([]string{"volume", "ls", "-q"}, f...)...))) {
			_, _ = DockerErr("volume", "rm", "-f", v)
		}
	}
	for _, n := range Lines(first(DockerErr("network", "ls", "-q", "--filter", "name="+r.Name))) {
		_, _ = DockerErr("network", "rm", n)
	}
}

func first(out string, _ error) string { return out }
