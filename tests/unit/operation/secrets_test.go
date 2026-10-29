package operation_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

const canary = "canary-supplied-secret-3b1f77"

func (h *harness) store() secrets.Store {
	h.t.Helper()
	sc := h.scopeFile()
	store, err := h.provider.Open(h.t.Context(), secrets.ScopeRef{UID: sc.ScopeUID, KeyID: sc.KeyID})
	if err != nil {
		h.t.Fatal(err)
	}
	return store
}

func (h *harness) secretValue(ref string) string {
	h.t.Helper()
	cred, ok := h.state().Accepted.Credentials[ref]
	if !ok {
		h.t.Fatalf("no credential for %s", ref)
	}
	parsed, _ := secrets.ParseRef(ref)
	value, err := h.store().Resolve(h.t.Context(), parsed, cred.Version)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(value.Reveal())
}

func TestSecrets_ADriverStepGetsAGeneratedSecretThroughTheProbeEnvironmentOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, driverCatalog())
	if _, err := h.up(opts); err != nil {
		t.Fatalf("up: %v", err)
	}
	cred := h.state().Accepted.Credentials["primary/admin"]
	if cred.Provider != "local" || cred.Origin != state.OriginGenerated {
		t.Fatalf("credential = %+v", cred)
	}
	value := h.secretValue("primary/admin")
	if len(value) < 24 {
		t.Fatalf("generated value is %d characters", len(value))
	}
	h.assertAbsent(value)

	var carried bool
	for _, refs := range h.rt.Secrets {
		for _, ref := range refs {
			carried = carried || ref == "primary/admin"
		}
	}
	if !carried {
		t.Error("no probe carried the secret in its environment")
	}
	for _, op := range h.progress {
		data, _ := json.Marshal(op)
		if strings.Contains(string(data), value) {
			t.Error("the journal carries the secret value")
		}
	}
	op := h.archived(h.progress[len(h.progress)-1].ID)
	order := map[string]int{}
	for i, step := range op.Steps {
		order[step.Object] = i
	}
	if op.Steps[0].Action != "secret.create" || order["module/primary/key/admin"] < order["service/primary--store"] || order["module/primary/key/admin"] > order["job/migrate"] {
		t.Errorf("step order = %v: secret steps come first, the driver step after its provider and before the jobs and consumers", order)
	}
}

func TestSecrets_ASuppliedValueIsKeptAndNeverWrittenInPlaintext(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	withInput := testkit.ShopOptions{Instances: []string{"primary"}, SecretInput: true}
	supply := func(r *operation.Request) {
		r.SecretInputs = map[string]secrets.Value{"db-password": secrets.NewValue([]byte(canary))}
	}
	if _, err := h.up(withInput, supply); err != nil {
		t.Fatalf("up: %v", err)
	}
	cred := h.state().Accepted.Credentials["input/db-password"]
	if cred.Origin != state.OriginSupplied || cred.Provider != "local" {
		t.Fatalf("credential = %+v", cred)
	}
	if got := h.secretValue("input/db-password"); got != canary {
		t.Errorf("stored value = %q", got)
	}
	h.assertAbsent(canary)
	for _, op := range h.progress {
		data, _ := json.Marshal(op)
		if strings.Contains(string(data), canary) {
			t.Error("progress carries the secret value")
		}
	}
	// A kill and rerun keep the value: repeating the same supply stores no second version.
	if _, err := h.up(withInput, supply, func(r *operation.Request) { r.ExpandedScope = true }); err != nil {
		t.Fatal(err)
	}
	parsed, _ := secrets.ParseRef("input/db-password")
	if entry, _ := h.store().Entry(parsed); len(entry.Versions) != 1 {
		t.Errorf("the supplied value has %d versions after a repeat", len(entry.Versions))
	}
}

func TestSecrets_ASecretWrittenBeforeTheStateOutcomeIsSettledByItsCreatedBy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, driverCatalog())
	fault := errors.New("killed after the store write")
	h.hook = func(s durable.Stage) error {
		if s != durable.TempSynced {
			return nil
		}
		old, err := os.ReadFile(filepath.Join(h.root, "environments", "shop.local", "state.json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil // the first write has no old state
		}
		if err != nil {
			return fmt.Errorf("the hook cannot read the old state: %w", err)
		}
		// The write that would record the secret step as completed: the old file still has it started.
		if strings.Contains(string(old), `"action": "secret.create"`) && strings.Contains(string(old), `"status": "started"`) {
			return fault
		}
		return nil
	}
	if _, err := h.up(opts); !errors.Is(err, fault) {
		t.Fatalf("the faulted run error = %v", err)
	}
	h.hook = nil
	open := h.state().Operation
	if open == nil {
		t.Fatal("no open operation after the fault")
	}
	parsed, _ := secrets.ParseRef("primary/admin")
	store := h.store()
	entry, ok := store.Entry(parsed)
	if !ok || len(entry.Versions) != 1 {
		t.Fatalf("the secret was not written before the fault: %+v %v", entry, ok)
	}
	written := entry.Versions[0].ID

	if _, err := h.up(opts); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got := stepFor(t, h.archived(open.ID), "secret/primary/admin").Status; got != state.StepCompletedByObservation {
		t.Errorf("settled secret step = %v, want completed-by-observation", got)
	}
	if cred := h.state().Accepted.Credentials["primary/admin"]; cred.Version != written || cred.Origin != state.OriginGenerated {
		t.Errorf("credential = %+v, want the version written before the fault (%s)", cred, written)
	}
	if entry, _ := h.store().Entry(parsed); len(entry.Versions) != 1 {
		t.Errorf("the rerun generated a second value: %d versions", len(entry.Versions))
	}
}

func TestSecrets_ADeletedStoreAgainstExistingStateIsSecretMissingAndNeverRegenerated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, driverCatalog())
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(h.settings.SecretsDir(), h.scopeFile().ScopeUID.String()+".sealed")
	if err := os.Remove(sealed); err != nil {
		t.Fatal(err)
	}
	calls := len(h.rt.Calls)
	_, err := h.up(opts, func(r *operation.Request) { r.ExpandedScope = true })
	if !errors.Is(err, secrets.ErrSecretMissing) {
		t.Fatalf("error = %v, want ErrSecretMissing", err)
	}
	if _, statErr := os.Stat(sealed); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a missing store was regenerated")
	}
	if len(h.rt.Calls) != calls {
		t.Errorf("the runtime was called after the secret check failed: %v", h.rt.Calls[calls:])
	}
}

func TestSecrets_AMissingKeyFailsBeforeTheRuntime(t *testing.T) {
	t.Parallel()
	h := newHarnessWithoutKey(t)
	_, err := h.up(opts)
	if !errors.Is(err, secrets.ErrKeyMissing) {
		t.Fatalf("error = %v", err)
	}
	if len(h.rt.Calls) != 0 {
		t.Errorf("the runtime was called: %v", h.rt.Calls)
	}
}

func TestSecrets_AnInputSuppliedThroughTheWrongChannelIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   testkit.ShopOptions
		change func(*operation.Request)
	}{
		{"a secret input as public text", testkit.ShopOptions{SecretInput: true}, func(r *operation.Request) { r.PublicInputs = map[string]string{"db-password": canary} }},
		{"an undeclared public input", testkit.ShopOptions{}, func(r *operation.Request) { r.PublicInputs = map[string]string{"nobody-declared-this": canary} }},
		{"a public input as a secret", testkit.ShopOptions{PublicInput: true}, func(r *operation.Request) {
			r.SecretInputs = map[string]secrets.Value{"log-level": secrets.NewValue([]byte(canary))}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			_, err := h.up(test.opts, test.change)
			if codeOf(err) != state.CodeInputInvalid {
				t.Fatalf("error = %v, want %s", err, state.CodeInputInvalid)
			}
			if strings.Contains(err.Error(), canary) {
				t.Error("the refusal names the value")
			}
			if _, statErr := os.Stat(filepath.Join(h.root, "environments")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("a refused request created state: %v", statErr)
			}
			h.assertAbsent(canary)
		})
	}
}

func TestSecrets_AProbeStaysInsideItsInstanceAndCannotRebindTheClientEnvironment(t *testing.T) {
	t.Parallel()
	own := func(i module.Instance) string { return i.Key }
	peer := func(i module.Instance) string {
		if i.Key == "primary" {
			return "other"
		}
		return "primary"
	}
	tests := []struct {
		name string
		make func(i module.Instance) module.Probe
	}{
		{"another instance's workload", func(i module.Instance) module.Probe {
			return module.Probe{ID: "p", Kind: module.ProbeExec, Target: peer(i) + "--store", Argv: []string{"true"}}
		}},
		{"a workload of the project", func(i module.Instance) module.Probe {
			return module.Probe{ID: "p", Kind: module.ProbeExec, Target: "api", Argv: []string{"true"}}
		}},
		{"another instance's secret", func(i module.Instance) module.Probe {
			return module.Probe{ID: "p", Kind: module.ProbeExec, Target: own(i) + "--store", Argv: []string{"true"}, SecretEnv: []module.SecretBinding{{Name: "TOKEN", Ref: peer(i) + "/admin"}}}
		}},
		{"the user's input secret", func(i module.Instance) module.Probe {
			return module.Probe{ID: "p", Kind: module.ProbeExec, Target: own(i) + "--store", Argv: []string{"true"}, SecretEnv: []module.SecretBinding{{Name: "TOKEN", Ref: "input/db-password"}}}
		}},
		{"a lower-case name", func(i module.Instance) module.Probe { return bind(i, "token") }},
		{"an allowlisted name", func(i module.Instance) module.Probe { return bind(i, "PATH") }},
		{"a DOCKER_ name", func(i module.Instance) module.Probe { return bind(i, "DOCKER_HOST") }},
		{"a COMPOSE_ name", func(i module.Instance) module.Probe { return bind(i, "COMPOSE_FILE") }},
		{"an engine secret name", func(i module.Instance) module.Probe { return bind(i, "SP_SECRET_X") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, []module.Driver{drivers.Rogue{Make: test.make}})
			_, err := h.up(testkit.ShopOptions{Instances: []string{"primary", "other"}})
			if codeOf(err) != state.CodeOutOfScope {
				t.Fatalf("error = %v, want %s", err, state.CodeOutOfScope)
			}
			if n := h.rt.CallCount("probe"); n != 0 {
				t.Errorf("the refused probe reached the runtime %d times", n)
			}
		})
	}
}

func bind(i module.Instance, name string) module.Probe {
	return module.Probe{ID: "p", Kind: module.ProbeExec, Target: i.Key + "--store", Argv: []string{"true"}, SecretEnv: []module.SecretBinding{{Name: name, Ref: i.Key + "/admin"}}}
}

func TestSecrets_AnInstanceKeyedInputCannotReachTheUsersInputSecrets(t *testing.T) {
	t.Parallel()
	opts := testkit.ShopOptions{Instances: []string{"input"}}
	tests := []struct {
		name   string
		driver module.Driver
	}{
		{"a probe binding input/admin", drivers.Rogue{Make: func(i module.Instance) module.Probe { return bind(i, "TOKEN") }}},
		{"a step generating input/admin", drivers.RogueSecret{Ref: func(i module.Instance) string { return i.Key + "/admin" }}},
		{"a step generating another instance's secret", drivers.RogueSecret{Ref: func(module.Instance) string { return "other/admin" }}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, []module.Driver{test.driver})
			_, err := h.up(opts)
			if codeOf(err) != state.CodeOutOfScope {
				t.Fatalf("error = %v, want a refusal", err)
			}
			if h.state().Accepted != nil {
				if _, ok := h.state().Accepted.Credentials["input/admin"]; ok {
					t.Error("a credential was created in the input namespace")
				}
			}
		})
	}
}
