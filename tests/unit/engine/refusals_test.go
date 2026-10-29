package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets/local"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// seedScope writes scope.json (and no state) for the harness environment, recording the key ID of the
// key seed, as the first mutating Submit would have.
func seedScope(t *testing.T, h *harness, keySeed string) (*state.Env, state.Scope) {
	t.Helper()
	uid, err := state.NewScopeUID(testkit.Epoch, testkit.NewRandom("seeded"))
	if err != nil {
		t.Fatal(err)
	}
	var key [config.KeySize]byte
	copy(key[:], testkit.NewRandomBytes(keySeed, config.KeySize))
	store, err := local.New(local.Options{
		Dir: filepath.Join(h.root, "secrets"), KeySource: func() ([config.KeySize]byte, error) { return key, nil },
	}).Open(t.Context(), secrets.ScopeRef{UID: uid})
	if err != nil {
		t.Fatal(err)
	}
	env, err := state.CreateEnv(h.root, "shop", "local", state.Options{Random: testkit.NewRandom("seed-env")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	scope := state.Scope{
		ScopeUID: uid, Project: "shop", Environment: "local", DockerContext: "default", EngineID: "engine-1",
		ComposeProject: "shop-local-" + uid.Suffix(), KeyID: store.KeyID(), CreatedAt: testkit.Epoch,
	}
	if err := env.WriteScope(scope); err != nil {
		t.Fatal(err)
	}
	return env, scope
}

func TestSubmit_KeyFailuresHappenBeforeAnyDockerCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  func(h *harness)
		want engine.Code
		cat  engine.Category
	}{
		{"missing", func(h *harness) {}, engine.CodeKeyMissing, engine.CategoryValidation},
		{"not base64", func(h *harness) { h.env["STACKPUDDLE_SECRETS_KEY"] = "not a key" }, engine.CodeKeyInvalid, engine.CategoryValidation},
		{"wrong length", func(h *harness) { h.env["STACKPUDDLE_SECRETS_KEY"] = "AAAA" }, engine.CodeKeyInvalid, engine.CategoryValidation},
		{"different key", func(h *harness) { h.env["STACKPUDDLE_SECRETS_KEY"] = testkit.Key("other") }, engine.CodeKeyMismatch, engine.CategoryForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "")
			seedScope(t, h, "seed-key")
			test.key(h)
			h.eng = h.newEngine()
			_, err := h.eng.Submit(t.Context(), h.request(testkit.ShopOptions{DockerContext: "dead-socket"}))
			code, category := codeOf(t, err)
			if code != test.want || category != test.cat {
				t.Fatalf("error = %v / %v: %v", code, category, err)
			}
			if got := err.Error(); containsAny(got, testkit.Key("seed-key"), testkit.Key("other")) {
				t.Errorf("the message leaks a key: %q", got)
			}
			h.wantNoSideEffects()
		})
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func TestSubmit_RefusedStateFilesKeepTheirBytesAndHappenBeforeAnyDockerCall(t *testing.T) {
	t.Parallel()
	good := func(t *testing.T, scope state.Scope) []byte {
		data, err := json.Marshal(state.State{StateVersion: 1, ScopeUID: scope.ScopeUID})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	mutate := func(t *testing.T, data []byte, change func(map[string]any)) []byte {
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		change(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	tests := map[string]func(t *testing.T, scope state.Scope) []byte{
		"stateVersion 2": func(t *testing.T, scope state.Scope) []byte {
			return mutate(t, good(t, scope), func(d map[string]any) { d["stateVersion"] = 2 })
		},
		"an unknown field": func(t *testing.T, scope state.Scope) []byte {
			return mutate(t, good(t, scope), func(d map[string]any) { d["newThing"] = true })
		},
		"an unknown enum": func(t *testing.T, scope state.Scope) []byte {
			return mutate(t, good(t, scope), func(d map[string]any) {
				d["operation"] = map[string]any{
					"id": "op-01J9Z8K3M4N5P6Q7R8S9T0V1W2", "command": "up", "status": "paused", "revision": 1,
					"actor": map[string]any{"kind": "os-user"}, "createdAt": "2026-10-29T10:00:00Z", "updatedAt": "2026-10-29T10:00:00Z",
				}
			})
		},
	}
	for name, make := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, "k")
			env, scope := seedScope(t, h, "k")
			path := filepath.Join(env.Dir(), "state.json")
			data := make(t, scope)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			// The declared Docker context sits on a dead socket: a refusal that reached Docker first
			// would fail differently, and the shim would have logged a call.
			_, err := h.eng.Submit(t.Context(), h.request(testkit.ShopOptions{DockerContext: "dead-socket"}))
			if code, category := codeOf(t, err); code != engine.CodeStateVersionUnsupported || category != engine.CategoryValidation {
				t.Fatalf("error = %v / %v: %v", code, category, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(data) {
				t.Errorf("state.json changed (%v)", err)
			}
			h.wantNoSideEffects()
		})
	}
}

func TestInspect_ReadsAStateWhoseActorKindIsToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	env, stored := seedScope(t, h, "k")
	started := testkit.Epoch
	written, err := env.Commit(0, state.State{StateVersion: 1, ScopeUID: stored.ScopeUID, Operation: &state.Operation{
		ID: opID(t), Command: state.CommandUp, Revision: 1, Status: state.OpRunning, Actor: state.Actor{Kind: state.ActorToken},
		CreatedAt: started, UpdatedAt: started.Add(time.Second),
	}})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := h.eng.Inspect(t.Context(), engine.InspectOperation(scope(t), written.Operation.ID))
	if err != nil {
		t.Fatalf("a token actor must load although m01 never writes one: %v", err)
	}
	op, ok := inspection.Operation()
	if !ok || op.Actor() != engine.ActorToken || op.ScopeUID() != stored.ScopeUID.String() {
		t.Errorf("operation = actor %v scope %q", op.Actor(), op.ScopeUID())
	}
}

func opID(t *testing.T) state.OperationID {
	t.Helper()
	id, err := state.ParseOperationID("op-01J9Z8K3M4N5P6Q7R8S9T0V1W2")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
