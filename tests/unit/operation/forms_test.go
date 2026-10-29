package operation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// formDriver is the KV test driver whose apply probe binds its generated secret in the given form and
// whose observation records the scope the engine put on the instance.
type formDriver struct {
	drivers.KV
	form  string
	scope *string
}

func (d formDriver) Observe(ctx context.Context, r module.ObserveRequest) (module.ObserveResponse, error) {
	*d.scope = r.Instance.Scope
	return d.KV.Observe(ctx, r)
}

func (d formDriver) Apply(ctx context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	resp, err := d.KV.Apply(ctx, r)
	for i := range resp.Probes {
		resp.Probes[i].SecretEnv[0].Form = d.form
	}
	return resp, err
}

func TestSecretForms_TheEngineFillsTheScopeAndBindsASCRAMVerifierInTheProbeEnvironmentOnly(t *testing.T) {
	t.Parallel()
	var scope string
	h := newHarness(t, []module.Driver{formDriver{form: module.FormSCRAMSHA256, scope: &scope}})
	var plain, verifier string
	h.rt.ProbeFunc = func(call port.Call, p module.Probe) (module.ProbeResult, error) {
		if p.ID == "set" {
			plain = string(call.Secrets["primary/admin"].Reveal())
			verifier = string(call.Secrets[port.SecretKey("primary/admin", module.FormSCRAMSHA256)].Reveal())
		}
		return module.ProbeResult{ID: p.ID, Stdout: "ok"}, nil
	}
	if _, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}}); err != nil {
		t.Fatalf("up: %v", err)
	}
	if want := h.state().ScopeUID.String(); scope == "" || scope != want {
		t.Errorf("Instance.Scope = %q, want the scope uid %q", scope, want)
	}
	if !strings.HasPrefix(verifier, "SCRAM-SHA-256$4096:") || plain == "" || strings.Contains(verifier, plain) {
		t.Errorf("verifier %q does not derive from a distinct plaintext", verifier)
	}
}

func TestSecretForms_AnUnknownFormIsRefusedBeforeAnyProbeRuns(t *testing.T) {
	t.Parallel()
	var scope string
	h := newHarness(t, []module.Driver{formDriver{form: "md5", scope: &scope}})
	_, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}})
	if codeOf(err) != state.CodeOutOfScope {
		t.Fatalf("error = %v, want %s", err, state.CodeOutOfScope)
	}
	for _, s := range h.rt.Secrets {
		for _, ref := range s {
			if strings.Contains(ref, "md5") {
				t.Errorf("a refused form reached the runtime: %q", ref)
			}
		}
	}
}
