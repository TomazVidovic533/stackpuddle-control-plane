package operation_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// newBoundHarness is a harness whose catalog holds the Bound driver, a database-shaped driver.
func newBoundHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, drivers.BoundCatalog())
}

func (h *harness) boundRequest(o testkit.BoundOptions, command state.Command) operation.Request {
	h.t.Helper()
	return operation.Request{
		Scope: h.scope(), Project: testkit.Bound(h.t, o), ProjectDir: h.root, Command: command, All: true,
		Progress: func(op state.Operation, _ string) { h.progress = append(h.progress, op) },
	}
}

func (h *harness) upBound(o testkit.BoundOptions, change ...func(*operation.Request)) (operation.Result, error) {
	h.t.Helper()
	return h.submit(h.boundRequest(o, state.CommandUp), change...)
}

// acceptedTopology decodes the topology the last up accepted.
func (h *harness) acceptedTopology() plan.Topology {
	h.t.Helper()
	var top plan.Topology
	mustJSON(h.t, h.state().Accepted.Topology, &top)
	return top
}

func (h *harness) workload(name string) plan.Workload {
	h.t.Helper()
	w, ok := h.acceptedTopology().Find(name)
	if !ok {
		h.t.Fatalf("the accepted topology has no workload %s", name)
	}
	return w
}

func steps(op state.Operation) []string {
	var out []string
	for _, s := range op.Steps {
		out = append(out, s.Action+" "+s.Object)
	}
	return out
}

func TestDelivery_UpRunsDecision0024InOrder(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	result, err := h.upBound(testkit.BoundOptions{})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	got := steps(*result.Operation)
	order := []string{
		"tls.issue tls/ca", "tls.issue tls/db--store",
		"compose.service service/db--store", "bound-store.user module/db/user/migrator", "compose.job job/migrate",
		"module.verify module/db", "compose.service service/app",
	}
	last := -1
	for _, step := range order {
		i := slices.Index(got, step)
		if i <= last {
			t.Errorf("%q is at %d after %d: want the order %v, got %v", step, i, last, order, got)
		}
		last = max(last, i)
	}
	if first := got[0]; !strings.HasPrefix(first, "secret.create") {
		t.Errorf("the first step is %q: secrets come before certificates and workloads", first)
	}
}

func TestDelivery_RuntimeAndMigrationBindingsDifferAndDeliverDeclaredFilesAndKeys(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatalf("up: %v", err)
	}
	migrate, app := h.workload("migrate"), h.workload("app")
	wantFiles := func(secret string) []plan.File {
		return []plan.File{
			{Path: "/run/sp/ca.crt", Mode: 0o444, Source: plan.Source{Kind: plan.SourceMaterial, Ref: "tls/ca"}},
			{Path: "/run/sp/password", Mode: 0o400, Owner: "999:999", Source: plan.Source{Kind: plan.SourceSecret, Ref: secret}},
		}
	}
	if want := wantFiles("db/migrator-password"); !reflect.DeepEqual(want, migrate.Files) {
		t.Errorf("migration files:\nwant %+v\n got %+v", want, migrate.Files)
	}
	if want := wantFiles("db/app-password"); !reflect.DeepEqual(want, app.Files) {
		t.Errorf("runtime files:\nwant %+v\n got %+v", want, app.Files)
	}
	env := func(user string) []plan.EnvVar {
		return []plan.EnvVar{{Name: "DB_HOST", Value: "db--store"}, {Name: "DB_NAME", Value: "app"}, {Name: "DB_PORT", Value: "5432"}, {Name: "DB_USER", Value: user}}
	}
	if want := env("migrator"); !reflect.DeepEqual(want, migrate.Env) {
		t.Errorf("migration environment:\nwant %+v\n got %+v", want, migrate.Env)
	}
	if want := env("app"); !reflect.DeepEqual(want, app.Env) {
		t.Errorf("runtime environment:\nwant %+v\n got %+v", want, app.Env)
	}
	for _, w := range []plan.Workload{migrate, app} {
		for _, e := range w.Env {
			if strings.EqualFold(e.Name, "DB_PASSWORD") {
				t.Errorf("%s has an environment key for the password", w.Name)
			}
		}
	}
	server := h.workload("db--store")
	if want := plan.ServerFiles("999:999", "db--store"); !reflect.DeepEqual(want, server.Files) {
		t.Errorf("server files:\nwant %+v\n got %+v", want, server.Files)
	}
}

func TestDelivery_ARepeatedUpRestartsAndReRunsNothing(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatalf("up: %v", err)
	}
	applies, ca := h.rt.CallCount("apply"), h.fileBytes("tls/ca.crt")
	result, err := h.upBound(testkit.BoundOptions{})
	if err != nil || result.Operation != nil {
		t.Fatalf("a repeated up = %v, %v: want no operation", result.Operation, err)
	}
	if h.rt.CallCount("apply") != applies || !bytes.Equal(ca, h.fileBytes("tls/ca.crt")) {
		t.Error("a repeated up changed the runtime or reissued the CA")
	}
}

func TestDelivery_NoSecretValueOrPrivateKeyIsWrittenOutsideTheSealedStore(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatalf("up: %v", err)
	}
	values := []string{h.secretValue("db/app-password"), h.secretValue("db/migrator-password"), h.secretValue("tls/ca-key"), h.secretValue("tls/server/db--store")}
	for _, v := range values {
		h.assertAbsent(v)
		for _, op := range h.progress {
			data, _ := json.Marshal(op)
			if strings.Contains(string(data), v) {
				t.Error("the journal carries a secret value")
			}
		}
	}
	h.walkFiles(func(path string, data []byte) {
		if bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Errorf("%s holds private key material", path)
		}
	})
	for _, call := range h.rt.DeliveredFiles {
		for _, key := range call {
			if key != "material:tls/ca" && key != "material:tls/db--store" {
				t.Errorf("a call carried %q as public material", key)
			}
		}
	}
}

func TestDelivery_ASecretValueMappedToAnEnvironmentKeyIsRefusedBeforeAnyEffect(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	_, err := h.upBound(testkit.BoundOptions{PasswordInEnvironment: true})
	if codeOf(err) != state.CodeSecretInEnvironment {
		t.Fatalf("error = %v, want %s", err, state.CodeSecretInEnvironment)
	}
	if len(h.rt.Calls) != 0 {
		t.Errorf("the runtime was called before the refusal: %v", h.rt.Calls)
	}
}

func TestDelivery_APlainWorkloadDeliversHostAndPortThroughTheSamePath(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{Plain: true}); err != nil {
		t.Fatalf("up: %v", err)
	}
	reader := h.workload("reader")
	if want := []plan.EnvVar{{Name: "CACHE_HOST", Value: "cache"}, {Name: "CACHE_PORT", Value: "6379"}}; !reflect.DeepEqual(want, reader.Env) {
		t.Errorf("environment:\nwant %+v\n got %+v", want, reader.Env)
	}
	want := []plan.File{{Path: "/run/sp/cache-host", Mode: 0o444, Source: plan.Source{Kind: plan.SourceText, Text: "cache"}}}
	if want := want; !reflect.DeepEqual(want, reader.Files) {
		t.Errorf("files:\nwant %+v\n got %+v", want, reader.Files)
	}
	if !h.workload("cache").Provider {
		t.Error("a workload other workloads bind to is not marked a provider")
	}
}

func TestDelivery_ADeclaredValueOfAPlainWorkloadHasNoSourceAndIsRefused(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	_, err := h.upBound(testkit.BoundOptions{Plain: true, PlainSecretValue: true})
	if codeOf(err) != state.CodeValueUnavailable {
		t.Fatalf("error = %v, want %s", err, state.CodeValueUnavailable)
	}
	if len(h.rt.Calls) != 0 {
		t.Errorf("the runtime was called before the refusal: %v", h.rt.Calls)
	}
}

func TestDelivery_ADriverResponseTheEngineCannotDeliverIsRefusedBeforeAnyEffect(t *testing.T) {
	t.Parallel()
	good := drivers.Bound{}
	response := func(change func(values []module.BoundValue) []module.BoundValue) func(module.BindRequest) module.BindResponse {
		return func(r module.BindRequest) module.BindResponse {
			base, _ := good.Bind(t.Context(), r)
			return module.BindResponse{Values: change(base.Values)}
		}
	}
	tests := []struct {
		name string
		make func(module.BindRequest) module.BindResponse
		code string
	}{
		{"an undeclared value", response(func(v []module.BoundValue) []module.BoundValue {
			return append(v, module.BoundValue{Name: "extra", Text: "x"})
		}), state.CodeValueUnavailable},
		{"a missing value", response(func(v []module.BoundValue) []module.BoundValue { return v[:len(v)-1] }), state.CodeValueUnavailable},
		{"two kinds in one value", response(func(v []module.BoundValue) []module.BoundValue {
			v[1].SecretRef = "db/app-password"
			return v
		}), state.CodeValueUnavailable},
		{"no kind in a value", response(func(v []module.BoundValue) []module.BoundValue {
			v[1].Text = ""
			return v
		}), state.CodeValueUnavailable},
		{"an unknown material", response(func(v []module.BoundValue) []module.BoundValue {
			v[0].Material = "host-file"
			return v
		}), state.CodeValueUnavailable},
		{"a secret outside the instance namespace", response(func(v []module.BoundValue) []module.BoundValue {
			v[2].SecretRef = "input/db-password"
			return v
		}), state.CodeOutOfScope},
		{"a secret value bound as text", response(func(v []module.BoundValue) []module.BoundValue {
			v[2] = module.BoundValue{Name: "password", Text: "hunter2"}
			return v
		}), state.CodeValueUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, []module.Driver{drivers.BadBind{Make: tc.make}})
			_, err := h.upBound(testkit.BoundOptions{})
			if codeOf(err) != tc.code {
				t.Fatalf("error = %v, want %s", err, tc.code)
			}
			if len(h.rt.Calls) != 0 {
				t.Errorf("the runtime was called before the refusal: %v", h.rt.Calls)
			}
		})
	}
}

func TestDelivery_BindingParametersTheDriverRefusesFailAtPlanTimeWithTheirOwnCode(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	_, err := h.upBound(testkit.BoundOptions{RuntimeUser: "postgres"})
	if codeOf(err) != state.CodeParametersInvalid {
		t.Fatalf("error = %v, want %s", err, state.CodeParametersInvalid)
	}
	if len(h.rt.Calls) != 0 {
		t.Errorf("the runtime was called before the refusal: %v", h.rt.Calls)
	}
	dry := h.boundRequest(testkit.BoundOptions{RuntimeUser: "postgres"}, state.CommandUp)
	dry.DryRun = true
	if _, err := h.eng.Submit(t.Context(), dry); codeOf(err) != state.CodeParametersInvalid {
		t.Errorf("a dry run reports %v, want %s", err, state.CodeParametersInvalid)
	}
}
