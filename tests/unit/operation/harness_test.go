package operation_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets/local"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// harness is one operation engine over a real state folder, the real sealed secret store and a fake
// runtime, so every test is Docker-free but exercises the real files.
type harness struct {
	t        *testing.T
	root     string
	rt       *testkit.FakeRuntime
	settings config.Settings
	provider *local.Provider
	eng      *operation.Engine
	// hook, if set, is the fault-injection seam of every state write.
	hook     func(durable.Stage) error
	progress []state.Operation
}

func newHarness(t *testing.T, catalog []module.Driver) *harness {
	t.Helper()
	return newHarnessWithEnv(t, catalog, map[string]string{"STACKPUDDLE_SECRETS_KEY": testkit.Key("operation")})
}

func newHarnessWithoutKey(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithEnv(t, nil, map[string]string{})
}

func newHarnessWithEnv(t *testing.T, catalog []module.Driver, extra map[string]string) *harness {
	t.Helper()
	root := testkit.Root(t)
	env := map[string]string{"STACKPUDDLE_STATE_DIR": root, "HOME": root}
	for k, v := range extra {
		env[k] = v
	}
	settings, err := config.Load(testkit.LookupEnv(env), "linux")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: root, rt: testkit.NewFakeRuntime(), settings: settings}
	random := testkit.NewRandom("operation")
	h.provider = local.New(local.Options{
		Dir: settings.SecretsDir(), Now: testkit.Clock(), Random: random,
		KeySource: func() ([config.KeySize]byte, error) {
			key, err := settings.SecretsKey()
			if errors.Is(err, config.ErrKeyMissing) {
				return key, fmt.Errorf("%w: %w", secrets.ErrKeyMissing, err)
			}
			return key, err
		},
	})
	h.eng, err = operation.New(operation.Deps{
		Catalog: catalog, Runtime: h.rt, Providers: map[string]secrets.Provider{local.ProviderID: h.provider},
		Settings: settings, State: state.Options{Random: random, AfterStage: h.stage}, Now: testkit.Clock(), Random: random,
		Version: "test", RuntimeCapability: "runtime.compose/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) stage(s durable.Stage) error {
	if h.hook != nil {
		return h.hook(s)
	}
	return nil
}

func (h *harness) scope() operation.Scope {
	project, err := model.ParseProjectName("shop")
	if err != nil {
		h.t.Fatal(err)
	}
	environment, err := model.ParseEnvironmentName("local")
	if err != nil {
		h.t.Fatal(err)
	}
	return operation.Scope{Project: project, Environment: environment}
}

func (h *harness) request(o testkit.ShopOptions, command state.Command) operation.Request {
	h.t.Helper()
	return operation.Request{
		Scope: h.scope(), Project: testkit.Shop(h.t, o), ProjectDir: h.root, Command: command, All: true,
		Progress: func(op state.Operation, _ string) { h.progress = append(h.progress, op) },
	}
}

func (h *harness) up(o testkit.ShopOptions, change ...func(*operation.Request)) (operation.Result, error) {
	h.t.Helper()
	return h.submit(h.request(o, state.CommandUp), change...)
}

func (h *harness) submit(req operation.Request, change ...func(*operation.Request)) (operation.Result, error) {
	h.t.Helper()
	for _, c := range change {
		c(&req)
	}
	return h.eng.Submit(h.t.Context(), req)
}

func selecting(refs ...string) func(*operation.Request) {
	return func(r *operation.Request) {
		r.All = false
		for _, text := range refs {
			ref, err := model.ParseObjectRef(text)
			if err != nil {
				panic(err)
			}
			r.Refs = append(r.Refs, ref)
		}
	}
}

func (h *harness) open() *state.Env {
	h.t.Helper()
	env, err := state.OpenEnv(h.root, "shop", "local", state.Options{})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { env.Close() })
	return env
}

func (h *harness) state() state.State {
	h.t.Helper()
	st, _, err := h.open().Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) scopeFile() state.Scope {
	h.t.Helper()
	sc, ok, err := h.open().LoadScope()
	if err != nil || !ok {
		h.t.Fatalf("scope.json: %v %v", ok, err)
	}
	return sc
}

func (h *harness) archived(id state.OperationID) state.Operation {
	h.t.Helper()
	op, ok, err := h.open().LoadOperation(id)
	if err != nil || !ok {
		h.t.Fatalf("operation %s is not archived: %v %v", id, ok, err)
	}
	return op
}

func (h *harness) fileBytes(name string) []byte {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, "environments", "shop.local", name))
	if err != nil {
		h.t.Fatal(err)
	}
	return data
}

func codeOf(err error) string {
	if f, ok := errors.AsType[*operation.Failure](err); ok {
		return f.Code
	}
	return ""
}

// walkFiles calls fn for every regular file under the state root.
func (h *harness) walkFiles(fn func(path string, data []byte)) {
	h.t.Helper()
	err := filepath.WalkDir(h.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fn(path, data)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) assertAbsent(value string) {
	h.t.Helper()
	h.walkFiles(func(path string, data []byte) {
		if bytes.Contains(data, []byte(value)) {
			h.t.Errorf("%s contains the secret value", path)
		}
	})
}
