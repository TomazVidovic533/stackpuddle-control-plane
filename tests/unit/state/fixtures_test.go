package state_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

var epoch = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

// validState builds a state that exercises every member: accepted intent, credentials, a realization
// with a mapping and an open operation.
func validState(t testing.TB, actor state.ActorKind) state.State {
	t.Helper()
	random := testkit.NewRandom("state-fixture")
	uid := must(t, func() (state.ScopeUID, error) { return state.NewScopeUID(epoch, random) })
	op := must(t, func() (state.OperationID, error) { return state.NewOperationID(epoch, random) })
	rz := must(t, func() (state.RealizationID, error) { return state.NewRealizationID(epoch, random) })
	sv := must(t, func() (state.SecretVersionID, error) { return state.NewSecretVersionID(epoch, random) })
	started := epoch.Add(time.Second)
	return state.State{
		StateVersion: state.StateVersion,
		Generation:   3,
		ScopeUID:     uid,
		Accepted: &state.Accepted{
			Revision:    2,
			Model:       json.RawMessage(`{"kind":"Model"}`),
			Topology:    json.RawMessage(`{"workloads":[]}`),
			Inputs:      map[string]string{"region": "eu"},
			Credentials: map[string]state.Credential{"input/db-password": {Provider: "local", Origin: state.OriginSupplied, Version: sv}},
			Intent:      map[string]state.Intent{"api": state.IntentRunning},
		},
		Realizations: map[string]state.Realization{
			"volume/store--data": {
				UID: rz, AppliedRevision: 2, State: state.RealizationActive, SpecHash: "abc",
				Artifact: &state.Artifact{ImageID: "sha256:1"},
				Mappings: []state.Mapping{{Kind: "compose.volume", Name: "p_store--data", ID: "p_store--data", CreatedAt: epoch, Ports: []state.PortMapping{{Container: 5432, Host: 5432, HostIP: "127.0.0.1"}}}},
			},
		},
		Operation: &state.Operation{
			ID: op, Command: state.CommandUp, Selection: state.Selection{Refs: []string{"workload/api"}}, Revision: 2, BasedOnGeneration: 2,
			Status: state.OpRunning, Actor: state.Actor{Kind: actor}, CreatedAt: epoch, UpdatedAt: started,
			Steps: []state.Step{{ID: "s1", Action: "compose.volume", Object: "volume/store--data", Status: state.StepStarted, StartedAt: &started}},
		},
	}
}

func must[T any](t testing.TB, f func() (T, error)) T {
	t.Helper()
	v, err := f()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func marshal(t testing.TB, v any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutate decodes data as a generic document, applies change and encodes it again.
func mutate(t testing.TB, data []byte, change func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	return marshal(t, doc)
}

// diff returns "" when want and got encode to the same compact JSON and both documents otherwise. The
// encoding is the contract of a stored file, it reaches the identifier types through their text
// form, and it compacts raw members so layout never counts as a difference.
func diff(want, got any) string {
	wantJSON, wantErr := json.Marshal(want)
	gotJSON, gotErr := json.Marshal(got)
	if wantErr != nil || gotErr != nil {
		return fmt.Sprintf("cannot encode: %v, %v", wantErr, gotErr)
	}
	if bytes.Equal(wantJSON, gotJSON) {
		return ""
	}
	return fmt.Sprintf("want: %s\n got: %s", wantJSON, gotJSON)
}
