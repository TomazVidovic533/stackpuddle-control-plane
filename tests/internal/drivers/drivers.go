// Package drivers holds the module drivers the tests use. No test driver ships in the engine: the
// engine core takes its catalog as a parameter and the tests pass this one.
package drivers

import (
	"context"
	"errors"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// Module is the module name of the test driver; it matches the kv-store definition of the sample project.
const Module = "kv-store"

// KV drives instances of the kv-store definition: it observes through an exec probe, plans one step
// that needs a generated secret, applies it through an exec probe whose secret travels in the
// probe's environment, and verifies through an exec probe.
type KV struct{}

// Catalog returns the test catalog: one KV driver.
func Catalog() []module.Driver { return []module.Driver{KV{}} }

// Describe returns the static description.
func (KV) Describe() module.Descriptor {
	return module.Descriptor{
		Module: Module, Version: "0.1.0", Capabilities: []string{"module.kv-store/1"},
		StateKinds: []string{"kv-store.key"}, ActionKinds: []string{"kv-store.key"}, Deletes: false,
	}
}

func target(i module.Instance) string { return i.Key + "--store" }

// Observe asks whether the store answers.
func (KV) Observe(_ context.Context, r module.ObserveRequest) (module.ObserveResponse, error) {
	if len(r.Results) == 0 {
		return module.ObserveResponse{Probes: []module.Probe{{ID: "ping", Kind: module.ProbeExec, Target: target(r.Instance), Argv: []string{"true"}}}}, nil
	}
	return module.ObserveResponse{Observation: &module.Observation{Present: r.Results[0].ExitCode == 0}}, nil
}

// Plan plans the key step unless a mapping shows it exists.
func (KV) Plan(_ context.Context, r module.PlanRequest) (module.PlanResponse, error) {
	for _, m := range r.Mappings {
		if m.Kind == "kv-store.key" {
			return module.PlanResponse{}, nil
		}
	}
	return module.PlanResponse{Steps: []module.Step{{
		Action: "kv-store.key", Object: "key/admin", Secret: &module.SecretNeed{Ref: r.Instance.Key + "/admin", Size: 24},
	}}}, nil
}

// Apply writes the generated secret into the store through an exec probe, by environment only.
func (KV) Apply(_ context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	if len(r.Results) == 0 {
		return module.ApplyResponse{Probes: []module.Probe{{
			ID: "set", Kind: module.ProbeExec, Target: target(r.Instance),
			Argv:      []string{"sh", "-c", "printf %s \"$KV_SECRET\" > /tmp/admin-key"},
			SecretEnv: []module.SecretBinding{{Name: "KV_SECRET", Ref: r.Step.Secret.Ref}},
		}}}, nil
	}
	if r.Results[0].ExitCode != 0 {
		return module.ApplyResponse{Outcome: &module.Outcome{Applied: false, Evidence: "probe failed"}}, nil
	}
	return module.ApplyResponse{Outcome: &module.Outcome{Applied: true, Evidence: "key stored", Mappings: []module.Mapping{{Kind: "kv-store.key", Name: "admin"}}}}, nil
}

// Verify checks the key file exists.
func (KV) Verify(_ context.Context, r module.VerifyRequest) (module.VerifyResponse, error) {
	if len(r.Results) == 0 {
		return module.VerifyResponse{Probes: []module.Probe{{ID: "check", Kind: module.ProbeExec, Target: target(r.Instance), Argv: []string{"test", "-s", "/tmp/admin-key"}}}}, nil
	}
	ok := r.Results[0].ExitCode == 0
	verdict := module.Verification{OK: ok}
	if !ok {
		verdict.Problems = []string{"key file is missing"}
	}
	return module.VerifyResponse{Verification: &verdict}, nil
}

// Bind returns no values: the kv-store export declares none, and parameters are not accepted.
func (KV) Bind(_ context.Context, r module.BindRequest) (module.BindResponse, error) {
	if len(r.Parameters) > 0 {
		return module.BindResponse{}, errors.New("kv-store takes no binding parameters")
	}
	return module.BindResponse{}, nil
}

// Rogue is a KV driver whose observation asks for the probe Make returns for the instance, so a test
// can check what the engine refuses a driver to do.
type Rogue struct {
	KV
	Make func(i module.Instance) module.Probe
}

// Observe asks for the rogue probe.
func (r Rogue) Observe(_ context.Context, req module.ObserveRequest) (module.ObserveResponse, error) {
	if len(req.Results) == 0 {
		return module.ObserveResponse{Probes: []module.Probe{r.Make(req.Instance)}}, nil
	}
	return module.ObserveResponse{Observation: &module.Observation{Present: true}}, nil
}

// RogueSecret is a KV driver whose plan needs the secret Ref returns for the instance, so a test can
// check that the engine refuses a step that reaches outside its instance's namespace.
type RogueSecret struct {
	KV
	Ref func(i module.Instance) string
}

// Plan plans one step that needs the rogue secret.
func (r RogueSecret) Plan(_ context.Context, req module.PlanRequest) (module.PlanResponse, error) {
	return module.PlanResponse{Steps: []module.Step{{
		Action: "kv-store.key", Object: "key/admin", Secret: &module.SecretNeed{Ref: r.Ref(req.Instance), Size: 24},
	}}}, nil
}

// TwoRound is a KV driver whose apply takes two probe rounds, "mutate" then "mark", like a database
// that is created and then marked as ours: a step stopped between the rounds leaves an unmarked object.
type TwoRound struct{ KV }

// Apply asks for the mutate probe, then the mark probe, then reports the key.
func (TwoRound) Apply(_ context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	probe := func(id string) module.ApplyResponse {
		return module.ApplyResponse{Probes: []module.Probe{{ID: id, Kind: module.ProbeExec, Target: target(r.Instance), Argv: []string{"true"}}}}
	}
	switch {
	case len(r.Results) == 0:
		return probe("mutate"), nil
	case r.Results[0].ID == "mutate":
		return probe("mark"), nil
	}
	return module.ApplyResponse{Outcome: &module.Outcome{Applied: true, Evidence: "marked", Mappings: []module.Mapping{{Kind: "kv-store.key", Name: "admin"}}}}, nil
}
