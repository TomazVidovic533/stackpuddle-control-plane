package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// harness is one engine over a scratch state root with a docker shim that records every call.
type harness struct {
	t     *testing.T
	root  string
	eng   *engine.Engine
	calls func() []string
	env   map[string]string
}

func newHarness(t *testing.T, key string) *harness {
	t.Helper()
	root := testkit.Root(t)
	shim, calls := testkit.DockerShim(t)
	h := &harness{t: t, root: root, calls: calls, env: map[string]string{
		"STACKPUDDLE_STATE_DIR": root, "HOME": root, "PATH": shim,
	}}
	if key != "" {
		h.env["STACKPUDDLE_SECRETS_KEY"] = testkit.Key(key)
	}
	h.eng = h.newEngine()
	return h
}

func (h *harness) newEngine() *engine.Engine {
	h.t.Helper()
	eng, err := engine.New(engine.Options{LookupEnv: testkit.LookupEnv(h.env), Now: testkit.Clock(), Random: testkit.NewRandom("engine")})
	if err != nil {
		h.t.Fatal(err)
	}
	return eng
}

func scope(t testing.TB) engine.Scope {
	t.Helper()
	project, err := model.ParseProjectName("shop")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := model.ParseEnvironmentName("local")
	if err != nil {
		t.Fatal(err)
	}
	return engine.Scope{Project: project, Environment: environment}
}

func (h *harness) request(o testkit.ShopOptions) engine.SubmitRequest {
	h.t.Helper()
	return engine.SubmitRequest{
		Scope: scope(h.t), Project: testkit.Shop(h.t, o), ProjectDir: h.root,
		Command: engine.CommandUp, Selection: engine.SelectAll(),
	}
}

func codeOf(t testing.TB, err error) (engine.Code, engine.Category) {
	t.Helper()
	failure, ok := errors.AsType[*engine.Error](err)
	if !ok {
		t.Fatalf("error %v is not an *engine.Error", err)
	}
	return failure.Code(), failure.Category()
}

func (h *harness) wantNoSideEffects() {
	h.t.Helper()
	if calls := h.calls(); len(calls) != 0 {
		h.t.Errorf("docker was called: %v", calls)
	}
}

func TestAdvertise_ListsTheRuntimeAndTheSecretProvider(t *testing.T) {
	t.Parallel()
	info := newHarness(t, "k").eng.Advertise()
	var got []string
	for _, c := range info.Capabilities {
		got = append(got, c.String())
	}
	if want := []string{"module.postgres/1", "runtime.compose/1", "secrets.local/1"}; !slices.Equal(got, want) {
		t.Errorf("capabilities = %v, want %v (the catalog holds the postgres driver)", got, want)
	}
	if !slices.Equal(info.StateRead, []int{1}) || info.StateWrite != 1 || !slices.Equal(info.StoreRead, []int{1}) || info.StoreWrite != 1 {
		t.Errorf("versions = %+v", info)
	}
	info.Capabilities[0] = info.Capabilities[1]
	if again := newHarness(t, "k").eng.Advertise(); again.Capabilities[0].String() != "module.postgres/1" {
		t.Error("Advertise shares its slice with the caller")
	}
}

func TestSubmit_RefusesAMissingCapabilityBeforeAnyLockStateReadOrDockerCall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "k")
	_, err := h.eng.Submit(t.Context(), h.request(testkit.ShopOptions{RequireCapability: "module.mysql/1"}))
	code, category := codeOf(t, err)
	if code != engine.CodeCapabilityUnsupported || category != engine.CategoryValidation {
		t.Fatalf("error = %v / %v", code, category)
	}
	if _, err := os.Stat(filepath.Join(h.root, "environments")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a state folder was created before the capability check: %v", err)
	}
	h.wantNoSideEffects()
}

func TestSubmit_RefusesASecretInputBoundToTheEnvironment(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "k")
	req := h.request(testkit.ShopOptions{SecretInput: true, APISecretInput: true})
	req.Inputs = []engine.InputValue{engine.SecretInput("db-password", engine.NewSecretValue([]byte("canary")))}
	_, err := h.eng.Submit(t.Context(), req)
	if code, _ := codeOf(t, err); code != engine.CodeSecretInEnvironment {
		t.Fatalf("error = %v", err)
	}
	h.wantNoSideEffects()
}

func TestSubmit_ADryRunTakesNoLockWritesNothingAndReportsTheGeneration(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	req := h.request(testkit.ShopOptions{Instances: []string{"primary"}})
	req.DryRun = true
	result, err := h.eng.Submit(t.Context(), req)
	if err != nil {
		t.Fatalf("a dry run needs neither a key nor Docker: %v", err)
	}
	preview := result.Preview()
	if preview.BasedOnGeneration() != 0 || len(preview.Actions()) == 0 {
		t.Fatalf("preview = generation %d, %d actions", preview.BasedOnGeneration(), len(preview.Actions()))
	}
	if _, ok := result.Operation(); ok {
		t.Error("a dry run returned an operation")
	}
	if _, err := os.Stat(filepath.Join(h.root, "environments")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run created state: %v", err)
	}
	h.wantNoSideEffects()
}

func TestInspect_OfAnUnknownEnvironmentIsEmptyAndWritesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	inspection, err := h.eng.Inspect(t.Context(), engine.InspectEnvironment(scope(t)))
	if err != nil {
		t.Fatal(err)
	}
	view, ok := inspection.Environment()
	if !ok || view.Generation() != 0 || view.Revision() != 0 || len(view.Objects()) != 0 {
		t.Errorf("view = %+v", view)
	}
	if _, err := os.Stat(filepath.Join(h.root, "environments")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Inspect created state: %v", err)
	}
}

func TestCancelAndInspectOperation_OfAnUnknownOperation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	id, err := engine.ParseOperationID("op-01J9Z8K3M4N5P6Q7R8S9T0V1W2")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Cancel(t.Context(), engine.CancelRequest{Scope: scope(t), Operation: id}); err == nil {
		t.Error("Cancel of an unknown operation succeeded")
	} else if code, category := codeOf(t, err); code != engine.CodeOperationNotFound || category != engine.CategoryNotFound {
		t.Errorf("Cancel error = %v / %v", code, category)
	}
	if _, err := h.eng.Inspect(t.Context(), engine.InspectOperation(scope(t), id)); err == nil {
		t.Error("Inspect of an unknown operation succeeded")
	}
}

func TestNew_ClassifiesEverySettingFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		lookup   func(string) (string, bool)
		code     engine.Code
		category engine.Category
	}{
		{"no environment reader", nil, engine.CodeUnexpected, engine.CategoryUnexpected},
		{"a relative state directory", testkit.LookupEnv(map[string]string{"STACKPUDDLE_STATE_DIR": "state", "HOME": "/h"}), engine.CodeInvalidSetting, engine.CategoryValidation},
		{"a relative secrets directory", testkit.LookupEnv(map[string]string{"STACKPUDDLE_SECRETS_DIR": "vault", "HOME": "/h"}), engine.CodeInvalidSetting, engine.CategoryValidation},
		{"no home to derive the state root from", testkit.LookupEnv(map[string]string{}), engine.CodeInvalidSetting, engine.CategoryValidation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			eng, err := engine.New(engine.Options{LookupEnv: test.lookup})
			if eng != nil {
				t.Fatal("New returned an engine with its error")
			}
			if code, category := codeOf(t, err); code != test.code || category != test.category {
				t.Errorf("code %s category %s, want %s %s (%v)", code, category, test.code, test.category, err)
			}
		})
	}
}

// observingDocker replaces the harness's failing docker with one that answers the observation as a
// scope that owns one labelled container no state accounts for.
func (h *harness) observingDocker() {
	h.t.Helper()
	inspect := `{"Id":"c1","Name":"/shop-api-1","Created":"2026-01-01T00:00:00Z","State":{"Running":true},"Config":{"Labels":{"com.docker.compose.service":"api","io.stackpuddle.realization":"r1"}}}`
	script := "#!/bin/sh\ncase \"$3 $4\" in\n\"container ls\") echo c1 ;;\n\"container inspect\") echo '" + inspect + "' ;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(h.env["PATH"], "docker"), []byte(script), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

func TestSubmit_ADryRunReportsUnmappedOwnedObjectsAsBlockedRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	seedScope(t, h, "seed-key")
	h.observingDocker()
	req := h.request(testkit.ShopOptions{Instances: []string{"primary"}})
	req.DryRun = true
	result, err := h.eng.Submit(t.Context(), req)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	blocked := 0
	for _, row := range result.Preview().Actions() {
		if row.Effect == engine.EffectBlocked {
			blocked++
			if row.Problem != engine.CodeUnmappedOwnedObject || row.Action != "" || row.Unknown {
				t.Errorf("blocked row = %+v, want the unmapped_owned_object problem, no action and Unknown false", row)
			}
			continue
		}
		if row.Problem != "" {
			t.Errorf("a row that is not blocked names a problem: %+v", row)
		}
	}
	if blocked == 0 {
		t.Errorf("the unmapped container was not reported as a blocked row: %+v", result.Preview().Actions())
	}
}
