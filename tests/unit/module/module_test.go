package module_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

func instance() module.Instance {
	return module.Instance{Key: "primary", Definition: "postgres", Version: "0.1.0", Options: json.RawMessage(`{"version":"17"}`), Scope: "scope-uid-1"}
}

func probe() module.Probe {
	return module.Probe{
		ID: "p1", Kind: module.ProbeExec, Target: "primary--store", User: "postgres", Argv: []string{"psql", "-c", "select 1"},
		Stdin: "select 1;", SecretEnv: []module.SecretBinding{{Name: "PGPASSWORD", Ref: "primary/admin", Form: module.FormSCRAMSHA256}},
	}
}

func results() []module.ProbeResult {
	return []module.ProbeResult{{ID: "p1", ExitCode: 0, Stdout: "1", Stderr: "warn"}}
}

func mappings() []module.Mapping {
	return []module.Mapping{{Kind: "postgres.role", Name: "app", ID: "42"}}
}

func step() module.Step {
	return module.Step{Action: "postgres.role", Object: "role/app", Destructive: true, Secret: &module.SecretNeed{Ref: "primary/app", Size: 24}, Params: json.RawMessage(`{"login":true}`)}
}

// TestEveryExportedTypeSurvivesAJSONRoundTrip fills each exported data type of the port from a
// fixture and requires that marshalling and unmarshalling gives the value back unchanged, so a staged
// container runner can speak the same contract.
func TestEveryExportedTypeSurvivesAJSONRoundTrip(t *testing.T) {
	t.Parallel()
	for name, value := range roundTripCases() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			back := newOf(value)
			if err := json.Unmarshal(data, back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got := deref(back); !reflect.DeepEqual(value, got) {
				t.Errorf("value changed in a round trip:\n want %+v\n  got %+v", value, got)
			}
		})
	}
}

// TestRoundTripCasesCoverEveryExportedStruct fails when the port gains a data type no case fills.
func TestRoundTripCasesCoverEveryExportedStruct(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "../../../internal/module/module.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := roundTripCases()
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			if _, isStruct := ts.Type.(*ast.StructType); isStruct {
				if _, covered := cases[ts.Name.Name]; !covered {
					t.Errorf("exported struct %s has no JSON round-trip case", ts.Name.Name)
				}
			}
		}
	}
}

func roundTripCases() map[string]any {
	observation := module.Observation{Present: true, Facts: map[string]string{"version": "17"}}
	outcome := module.Outcome{Applied: true, Evidence: "created", Mappings: mappings()}
	verification := module.Verification{OK: false, Problems: []string{"role missing"}}
	return map[string]any{
		"Descriptor":      module.Descriptor{Module: "postgres", Version: "0.1.0", Capabilities: []string{"module.postgres/1"}, StateKinds: []string{"postgres.role"}, ActionKinds: []string{"postgres.role"}, Deletes: true, ServerTLS: []module.ServerTLS{{Workload: "server", Listener: "sql", Owner: "999:999"}}},
		"ServerTLS":       module.ServerTLS{Workload: "server", Listener: "sql", Owner: "999:999"},
		"BindRequest":     module.BindRequest{Instance: instance(), Export: "sql", Parameters: json.RawMessage(`{"user":"app","database":"app"}`)},
		"BoundValue":      module.BoundValue{Name: "password", SecretRef: "primary/app-password"},
		"BindResponse":    module.BindResponse{Values: []module.BoundValue{{Name: "database", Text: "app"}, {Name: "ca", Material: module.MaterialTLSCA}}},
		"Instance":        instance(),
		"Mapping":         mappings()[0],
		"SecretBinding":   module.SecretBinding{Name: "PGPASSWORD", Ref: "primary/admin", Form: module.FormSCRAMSHA256},
		"Probe":           probe(),
		"ProbeResult":     results()[0],
		"SecretNeed":      module.SecretNeed{Ref: "primary/app", Size: 24},
		"Step":            step(),
		"Observation":     observation,
		"Outcome":         outcome,
		"Verification":    verification,
		"ObserveRequest":  module.ObserveRequest{Instance: instance(), Mappings: mappings(), Results: results()},
		"ObserveResponse": module.ObserveResponse{Probes: []module.Probe{probe()}, Observation: &observation},
		"PlanRequest":     module.PlanRequest{Instance: instance(), Mappings: mappings(), Observation: observation, Results: results()},
		"PlanResponse":    module.PlanResponse{Probes: []module.Probe{probe()}, Steps: []module.Step{step()}},
		"ApplyRequest":    module.ApplyRequest{Instance: instance(), Step: step(), Results: results()},
		"ApplyResponse":   module.ApplyResponse{Probes: []module.Probe{probe()}, Outcome: &outcome},
		"VerifyRequest":   module.VerifyRequest{Instance: instance(), Mappings: mappings(), Results: results()},
		"VerifyResponse":  module.VerifyResponse{Probes: []module.Probe{probe()}, Verification: &verification},
	}
}
