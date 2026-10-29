package postgres_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
)

func observe(t *testing.T, results ...module.ProbeResult) module.ObserveResponse {
	t.Helper()
	resp, err := postgres.Driver{}.Observe(t.Context(), module.ObserveRequest{Instance: instance(optionsText), Results: results})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return resp
}

func status(t *testing.T, resp module.ObserveResponse) string {
	t.Helper()
	if resp.Observation == nil {
		t.Fatalf("response %+v carries no observation", resp)
	}
	return resp.Observation.Facts[postgres.FactStatus]
}

func TestObserve_AbsentDeniedAndUnknownAreToldApart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result module.ProbeResult
		want   string
	}{
		{"a server that is not running", module.ProbeResult{ID: "catalog-1", ExitCode: -1, Stderr: "target primary--server is not running"}, "absent"},
		{"a catalog read the server refuses", module.ProbeResult{ID: "catalog-1", ExitCode: 2, Stderr: "ERROR:  42501\n"}, "denied"},
		{"an authentication failure", module.ProbeResult{ID: "catalog-1", ExitCode: 2, Stderr: "FATAL:  28P01\n"}, "denied"},
		{"a refusal worded by a server in another language", module.ProbeResult{ID: "catalog-1", ExitCode: 2, Stderr: "FEHLER:  42501"}, "denied"},
		{"an English refusal without a SQLSTATE is not trusted as one", module.ProbeResult{ID: "catalog-1", ExitCode: 2, Stderr: "ERROR:  permission denied for table pg_authid"}, "unknown"},
		{"a server that is still starting", module.ProbeResult{ID: "catalog-1", ExitCode: 2, Stderr: "FATAL:  57P03"}, "unknown"},
		{"output that is not the catalog", module.ProbeResult{ID: "catalog-1", Stdout: "not json"}, "unknown"},
		{"output with an unexpected member", module.ProbeResult{ID: "catalog-1", Stdout: `{"roles":[],"surprise":1}`}, "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resp := observe(t, test.result)
			if got := status(t, resp); got != test.want {
				t.Errorf("status = %q, want %q", got, test.want)
			}
			if resp.Observation.Present {
				t.Error("an unusable observation reports Present")
			}
		})
	}
}

func TestObserve_ThePresentServerIsReadInTwoRoundsAndTheReasonNeverEchoesAVerifier(t *testing.T) {
	t.Parallel()
	if resp := observe(t); len(resp.Probes) != 1 || resp.Probes[0].ID != "catalog-1" || resp.Probes[0].User != "postgres" {
		t.Fatalf("first round = %+v", resp)
	}
	raw, err := json.Marshal(fresh())
	if err != nil {
		t.Fatal(err)
	}
	second := observe(t, module.ProbeResult{ID: "catalog-1", Stdout: string(raw)})
	if len(second.Probes) != 2 {
		t.Fatalf("second round = %+v", second)
	}
	dbs := `{"db":"postgres","schemas":[],"defaults":[]}` + "\n" + `{"db":"template1","schemas":[],"defaults":[]}` + "\n"
	final := observe(t, module.ProbeResult{ID: "catalog-2", Stdout: string(raw)}, module.ProbeResult{ID: "catalog-dbs", Stdout: dbs})
	if status(t, final) != "present" || !final.Observation.Present || final.Observation.Facts[postgres.FactCatalog] == "" {
		t.Fatalf("final = %+v", final.Observation)
	}
	missing := observe(t, module.ProbeResult{ID: "catalog-2", Stdout: string(raw)}, module.ProbeResult{ID: "catalog-dbs", Stdout: `{"db":"postgres","schemas":[],"defaults":[]}`})
	if status(t, missing) != "unknown" {
		t.Errorf("an uninspected connectable database gave %q, want unknown", status(t, missing))
	}
	leaky := observe(t, module.ProbeResult{ID: "catalog-1", ExitCode: 2, Stderr: "STATEMENT: " + verifierText})
	if reason := leaky.Observation.Facts[postgres.FactReason]; strings.Contains(reason, "SCRAM-SHA-256") {
		t.Errorf("the reason echoes a verifier: %q", reason)
	}
}

func verify(t *testing.T, results ...module.ProbeResult) module.Verification {
	t.Helper()
	resp, err := postgres.Driver{}.Verify(t.Context(), module.VerifyRequest{Instance: instance(optionsText), Results: results})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Verification == nil {
		t.Fatalf("response %+v carries no verdict", resp)
	}
	return *resp.Verification
}

func dbLines(c catalog) string {
	var b strings.Builder
	for _, d := range c.Databases {
		if !d.AllowConn {
			continue
		}
		line, _ := json.Marshal(map[string]any{"db": d.Name, "schemas": d.Schemas, "defaults": d.Defaults})
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func results(t *testing.T, c catalog) []module.ProbeResult {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return []module.ProbeResult{{ID: "catalog-2", Stdout: string(raw)}, {ID: "catalog-dbs", Stdout: dbLines(c)}}
}

func TestVerify_FailsOnAStoppedRuntimeAndOnDriftAndPassesWhenConverged(t *testing.T) {
	t.Parallel()
	if v := verify(t, module.ProbeResult{ID: "catalog-1", ExitCode: -1}); v.OK || len(v.Problems) == 0 {
		t.Errorf("a stopped runtime verified: %+v", v)
	}
	if v := verify(t, results(t, converged())...); !v.OK {
		t.Errorf("a converged server failed: %v", v.Problems)
	}
	drifted := converged()
	drifted.Roles[len(drifted.Roles)-1].BypassRLS = true
	if v := verify(t, results(t, drifted)...); v.OK {
		t.Error("a role with the RLS-bypass attribute verified")
	}
	open := converged()
	open.Databases[len(open.Databases)-1].ACL = append(open.Databases[len(open.Databases)-1].ACL, acl{Grantee: "", Priv: "CONNECT"})
	if v := verify(t, results(t, open)...); v.OK {
		t.Error("a managed database open to PUBLIC verified")
	}
	if v := verify(t, results(t, fresh())...); v.OK {
		t.Error("a server without the declared objects verified")
	}
}

func TestDescribe_NamespacesItsKindsAndDeletesNothing(t *testing.T) {
	t.Parallel()
	d := postgres.Driver{}.Describe()
	if d.Module != "postgres" || d.Deletes {
		t.Errorf("descriptor = %+v", d)
	}
	for _, kind := range append(d.StateKinds, d.ActionKinds...) {
		if !strings.HasPrefix(kind, "postgres.") {
			t.Errorf("kind %q is not namespaced", kind)
		}
	}
}
