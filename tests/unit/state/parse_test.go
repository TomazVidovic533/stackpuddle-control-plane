package state_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

func TestParseState_RoundTrip(t *testing.T) {
	t.Parallel()
	want := validState(t, state.ActorOSUser)
	got, err := state.ParseState(marshal(t, want))
	if err != nil {
		t.Fatalf("ParseState: %v", err)
	}
	if diff := diff(want, got); diff != "" {
		t.Errorf("state changed in a round trip (-want +got):\n%s", diff)
	}
}

func TestParseState_AcceptsATokenActor(t *testing.T) {
	t.Parallel()
	got, err := state.ParseState(marshal(t, validState(t, state.ActorToken)))
	if err != nil {
		t.Fatalf("a token actor must load although m01 never writes one: %v", err)
	}
	if got.Operation.Actor.Kind != state.ActorToken {
		t.Errorf("actor kind = %v", got.Operation.Actor.Kind)
	}
}

func TestParseState_Refusals(t *testing.T) {
	t.Parallel()
	good := marshal(t, validState(t, state.ActorOSUser))
	tests := []struct {
		name    string
		data    []byte
		naming  string
		changes func(doc map[string]any)
	}{
		{name: "newer state version", naming: "stateVersion 2", changes: func(d map[string]any) { d["stateVersion"] = 2 }},
		{name: "state version zero", naming: "stateVersion 0", changes: func(d map[string]any) { d["stateVersion"] = 0 }},
		{name: "unknown top-level field", changes: func(d map[string]any) { d["extra"] = true }},
		{name: "unknown field inside the operation", changes: func(d map[string]any) { d["operation"].(map[string]any)["extra"] = 1 }},
		{name: "unknown operation status", changes: func(d map[string]any) { d["operation"].(map[string]any)["status"] = "paused" }},
		{name: "unknown step status", changes: func(d map[string]any) {
			d["operation"].(map[string]any)["steps"].([]any)[0].(map[string]any)["status"] = "weird"
		}},
		{name: "unknown actor kind", changes: func(d map[string]any) { d["operation"].(map[string]any)["actor"] = map[string]any{"kind": "robot"} }},
		{name: "unknown intent", changes: func(d map[string]any) { d["accepted"].(map[string]any)["intent"] = map[string]any{"api": "paused"} }},
		{name: "unknown engine mapping kind", changes: func(d map[string]any) {
			d["realizations"].(map[string]any)["volume/store--data"].(map[string]any)["mappings"].([]any)[0].(map[string]any)["kind"] = "compose.bogus"
		}},
		{name: "malformed scope uid", changes: func(d map[string]any) { d["scopeUid"] = "sc-123" }},
		{name: "missing accepted topology", changes: func(d map[string]any) { delete(d["accepted"].(map[string]any), "topology") }},
		{name: "trailing data", data: append(append([]byte(nil), good...), []byte(`{"x":1}`)...), naming: "after the document"},
		{name: "not JSON", data: []byte("generation: 3"), naming: ""},
		{name: "over the size cap", data: bytes.Repeat([]byte(" "), 16<<20+1), naming: "larger than"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data := test.data
			if test.changes != nil {
				data = mutate(t, good, test.changes)
			}
			_, err := state.ParseState(data)
			if !errors.Is(err, state.ErrUnsupported) {
				t.Fatalf("ParseState error = %v, want ErrUnsupported", err)
			}
			if test.naming != "" && !strings.Contains(err.Error(), test.naming) {
				t.Errorf("error %q does not contain %q", err, test.naming)
			}
		})
	}
}

func TestParseScope_Refusals(t *testing.T) {
	t.Parallel()
	good := marshal(t, validScope(t))
	if _, err := state.ParseScope(good); err != nil {
		t.Fatalf("valid scope: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"newer version":  func(d map[string]any) { d["stateVersion"] = 2 },
		"unknown field":  func(d map[string]any) { d["extra"] = 1 },
		"missing engine": func(d map[string]any) { delete(d, "engineId") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := state.ParseScope(mutate(t, good, change)); !errors.Is(err, state.ErrUnsupported) {
				t.Fatalf("error = %v, want ErrUnsupported", err)
			}
		})
	}
}

func FuzzParseState(f *testing.F) {
	f.Add(marshal(f, validState(f, state.ActorOSUser)))
	f.Add(marshal(f, validState(f, state.ActorToken)))
	f.Add([]byte(`{"stateVersion":1}`))
	f.Add([]byte(`{"stateVersion":2,"generation":1}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, err := state.ParseState(data)
		if err != nil {
			if !errors.Is(err, state.ErrUnsupported) {
				t.Fatalf("a refusal must be ErrUnsupported, got %v", err)
			}
			return
		}
		// What parses must encode and parse again to the same value.
		again, err := state.ParseState(marshal(t, parsed))
		if err != nil {
			t.Fatalf("parsed state does not parse after encoding: %v", err)
		}
		if diff := diff(parsed, again); diff != "" {
			t.Fatalf("state changed after a second round trip:\n%s", diff)
		}
	})
}

func TestParseState_ARunWithAJobResultRoundTripsAndAnUnknownJobMemberIsRefused(t *testing.T) {
	t.Parallel()
	want := validState(t, state.ActorOSUser)
	want.Operation.Command = state.CommandRun
	want.Operation.Steps[0] = state.Step{
		ID: "s1", Action: "compose.job", Object: "job/migrate", Status: state.StepFailed,
		Error: &state.Problem{Code: state.CodeJobExitedNonzero, Category: state.CategoryConflict}, Job: &state.JobResult{ExitCode: 3},
	}
	got, err := state.ParseState(marshal(t, want))
	if err != nil {
		t.Fatalf("ParseState: %v", err)
	}
	if got.Operation.Command != state.CommandRun || got.Operation.Steps[0].Job == nil || got.Operation.Steps[0].Job.ExitCode != 3 {
		t.Errorf("run/job round trip = %+v", got.Operation)
	}
	bad := mutate(t, marshal(t, want), func(d map[string]any) {
		d["operation"].(map[string]any)["steps"].([]any)[0].(map[string]any)["job"] = map[string]any{"exitCode": 3, "signal": "KILL"}
	})
	if _, err := state.ParseState(bad); !errors.Is(err, state.ErrUnsupported) {
		t.Errorf("an unknown job member: %v, want ErrUnsupported", err)
	}
}
