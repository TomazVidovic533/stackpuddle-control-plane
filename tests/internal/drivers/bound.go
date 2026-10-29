package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
)

// BoundModule is the module name of the Bound driver; it matches the bound-store definition of the
// delivery project.
const BoundModule = "bound-store"

// Users the Bound driver knows: it plans one password-bearing user step for each.
func boundUsers() []string { return []string{"app", "migrator"} }

// Bound drives instances of the bound-store definition. It has the shape of a database driver without
// the database: it plans a step with a generated secret for each user, binds the sql export from
// parameters, and asks the engine for a server certificate. Nothing it does needs a probe.
type Bound struct{ KV }

// Describe returns the static description, with the server certificate the store asks for.
func (Bound) Describe() module.Descriptor {
	return module.Descriptor{
		Module: BoundModule, Version: "0.1.0", Capabilities: []string{"module.bound-store/1"},
		StateKinds: []string{"bound-store.user"}, ActionKinds: []string{"bound-store.user"},
		ServerTLS: []module.ServerTLS{{Workload: "store", Listener: "sql", Owner: "999:999"}},
	}
}

// Observe reports the store present without a probe.
func (Bound) Observe(context.Context, module.ObserveRequest) (module.ObserveResponse, error) {
	return module.ObserveResponse{Observation: &module.Observation{Present: true}}, nil
}

// Plan plans one user step for each user without a mapping.
func (Bound) Plan(_ context.Context, r module.PlanRequest) (module.PlanResponse, error) {
	have := map[string]bool{}
	for _, m := range r.Mappings {
		have[m.Name] = true
	}
	var steps []module.Step
	for _, user := range boundUsers() {
		if !have[user] {
			steps = append(steps, module.Step{
				Action: "bound-store.user", Object: "user/" + user,
				Secret: &module.SecretNeed{Ref: r.Instance.Key + "/" + user + "-password", Size: 24},
				Params: json.RawMessage(fmt.Sprintf(`{"name":%q}`, user)),
			})
		}
	}
	return module.PlanResponse{Steps: steps}, nil
}

// Apply records the user as present.
func (Bound) Apply(_ context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	var p struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(r.Step.Params, &p); err != nil {
		return module.ApplyResponse{}, err
	}
	return module.ApplyResponse{Outcome: &module.Outcome{Applied: true, Evidence: "user " + p.Name, Mappings: []module.Mapping{{Kind: "bound-store.user", Name: p.Name}}}}, nil
}

// Verify finds every user present.
func (Bound) Verify(context.Context, module.VerifyRequest) (module.VerifyResponse, error) {
	return module.VerifyResponse{Verification: &module.Verification{OK: true}}, nil
}

// Bind maps {"user": "<known user>"} to the values of the sql export: the user, the database, the
// user's password as a secret reference, and the CA. Any other parameter is refused.
func (Bound) Bind(_ context.Context, r module.BindRequest) (module.BindResponse, error) {
	var p struct {
		User string `json:"user"`
	}
	decoder := json.NewDecoder(bytes.NewReader(r.Parameters))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil && !errors.Is(err, io.EOF) {
		return module.BindResponse{}, fmt.Errorf("bound-store: parameters: %w", err)
	}
	if r.Export != "sql" || p.User == "" || !slices.Contains(boundUsers(), p.User) {
		return module.BindResponse{}, fmt.Errorf("bound-store: export %q user %q is not bindable", r.Export, p.User)
	}
	return module.BindResponse{Values: []module.BoundValue{
		{Name: "ca", Material: module.MaterialTLSCA},
		{Name: "database", Text: "app"},
		{Name: "password", SecretRef: r.Instance.Key + "/" + p.User + "-password"},
		{Name: "username", Text: p.User},
	}}, nil
}

// BadBind is a Bound driver whose Bind answers with Make's response, so a test can check what the
// engine refuses before any effect.
type BadBind struct {
	Bound
	Make func(r module.BindRequest) module.BindResponse
}

// Bind returns the made response.
func (b BadBind) Bind(_ context.Context, r module.BindRequest) (module.BindResponse, error) {
	return b.Make(r), nil
}

// BoundCatalog returns the catalog with the Bound driver.
func BoundCatalog() []module.Driver { return []module.Driver{Bound{}} }
