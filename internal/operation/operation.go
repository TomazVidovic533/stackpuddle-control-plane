// Package operation is the engine core: Submit, Inspect and Cancel over one environment.
//
// It owns the order of a Submit (capability check, lock, load, secret store, target check, recovery,
// observation, merge, plan, fenced write, steps, verification) and declares the Runtime port the
// Compose adapter implements. It imports no adapter, so the catalog, the runtime and the secret
// providers arrive as parameters.
package operation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// Runtime is the port the Compose adapter implements. All calls target the declared context in
// Call.DockerContext and nothing else.
type Runtime interface {
	// Target returns the identity of the engine the declared context reaches.
	Target(ctx context.Context, call port.Call) (plan.TargetInfo, error)
	// Observe lists the objects that carry the scope's label.
	Observe(ctx context.Context, call port.Call) (plan.Inventory, error)
	// Render translates the topology; it is pure.
	Render(scope plan.ScopeNames, top plan.Topology) (plan.Rendered, error)
	// Apply performs one effect.
	Apply(ctx context.Context, call port.Call, e plan.Effect) (plan.Evidence, error)
	// Probe runs one driver probe inside the scope.
	Probe(ctx context.Context, call port.Call, p module.Probe) (module.ProbeResult, error)
}

// Failure is a classified failure the operation raises itself: a stable code and a category.
type Failure struct {
	Code     string
	Category state.Category
	Detail   string
	Cause    error
}

// Error returns the detail, which names settings and objects and never a secret value.
func (f *Failure) Error() string {
	if f.Cause != nil {
		return f.Detail + ": " + f.Cause.Error()
	}
	return f.Detail
}

// Unwrap returns the underlying cause, if any.
func (f *Failure) Unwrap() error { return f.Cause }

func fail(code string, category state.Category, format string, args ...any) *Failure {
	return &Failure{Code: code, Category: category, Detail: fmt.Sprintf(format, args...)}
}

// wrap records the underlying cause, which errors.Is and errors.As see through.
func (f *Failure) wrap(cause error) *Failure {
	f.Cause = cause
	return f
}

// Deps are the parts the engine is composed of.
type Deps struct {
	// Catalog is the explicit list of module drivers.
	Catalog []module.Driver
	// Runtime is the runtime adapter.
	Runtime Runtime
	// Providers maps a provider ID to its implementation.
	Providers map[string]secrets.Provider
	// Settings are the typed settings.
	Settings config.Settings
	// State configures the state store; only tests set its fault seam.
	State state.Options
	// Now supplies the clock; nil uses time.Now. Values are converted to UTC.
	Now func() time.Time
	// Random supplies IDs, nonces and jitter; it must not be nil.
	Random io.Reader
	// Version is the engine version Advertise reports.
	Version string
	// RuntimeCapability is the capability the runtime adapter advertises, such as "runtime.compose/1".
	RuntimeCapability string
}

// Engine runs operations. It holds no state between calls and is safe for concurrent use by
// operations on different environments.
type Engine struct {
	d       Deps
	drivers map[string]module.Driver
}

// New returns an Engine. Two drivers for one module are refused.
func New(d Deps) (*Engine, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	drivers := map[string]module.Driver{}
	for _, driver := range d.Catalog {
		name := driver.Describe().Module
		if _, dup := drivers[name]; dup {
			return nil, fmt.Errorf("operation: two drivers for module %q", name)
		}
		drivers[name] = driver
	}
	return &Engine{d: d, drivers: drivers}, nil
}

func (e *Engine) now() time.Time { return e.d.Now().UTC() }

// Advertise returns the capabilities and versions of this engine: the runtime, the secret
// providers and every driver's capabilities. It is computed from the parts, never listed twice.
func (e *Engine) Advertise() (model.EngineInfo, error) {
	names := []string{e.d.RuntimeCapability}
	for id := range e.d.Providers {
		names = append(names, "secrets."+id+"/1")
	}
	for _, driver := range e.d.Catalog {
		names = append(names, driver.Describe().Capabilities...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	info := model.EngineInfo{
		Version:    e.d.Version,
		StateRead:  []int{state.StateVersion},
		StateWrite: state.StateVersion,
		StoreRead:  []int{1},
		StoreWrite: 1,
	}
	for _, name := range names {
		c, err := model.ParseCapability(name)
		if err != nil {
			return model.EngineInfo{}, fmt.Errorf("operation: capability %q: %w", name, err)
		}
		info.Capabilities = append(info.Capabilities, c)
	}
	return info, nil
}

// Scope names one environment.
type Scope struct {
	Project     model.ProjectName
	Environment model.EnvironmentName
}

// Request is one Submit.
type Request struct {
	Scope      Scope
	Project    model.Project
	ProjectDir string
	Command    state.Command
	// All selects every object; otherwise Refs does.
	All  bool
	Refs []model.ObjectRef
	// PublicInputs maps a public input key to its text; SecretInputs maps a secret input key to its value.
	PublicInputs  map[string]string
	SecretInputs  map[string]secrets.Value
	DryRun        bool
	ExpandedScope bool
	DeleteData    bool
	// Wait is how long to wait for a busy environment; zero fails at once.
	Wait time.Duration
	// Progress is called after every persisted change, on Submit's goroutine.
	Progress func(op state.Operation, scopeUID string)
}

// Action is one line of a preview: a planned step, or a finding that blocks the operation.
type Action struct {
	// Action is the step's action; it is empty for a blocked row.
	Action string
	Object string
	// Impact says what the row does; plan.ImpactBlocked marks a finding.
	Impact plan.Impact
	// Unknown is true when the plan cannot say whether the step still has anything to do.
	Unknown bool
	// Problem is the code of the finding of a blocked row, empty for a step.
	Problem string
}

// Preview is the dry-run result: what would change, based on which generation.
type Preview struct {
	BasedOnGeneration int64
	Actions           []Action
}

// Result is the outcome of a Submit. Operation is nil for a dry run and for a request with nothing
// to do; Preview is set for a dry run.
type Result struct {
	Preview   *Preview
	Operation *state.Operation
	// ScopeUID identifies the scope the operation belongs to; empty when there is none yet.
	ScopeUID string
}

func (r Request) selectionRecord() state.Selection {
	rec := state.Selection{All: r.All, ExpandedScope: r.ExpandedScope}
	if !r.All {
		for _, ref := range r.Refs {
			rec.Refs = append(rec.Refs, ref.String())
		}
		slices.Sort(rec.Refs)
	}
	return rec
}

// prepared is the validated, source-derived part of a request.
type prepared struct {
	envName       model.EnvironmentName
	dockerContext string
	providerID    string
	candidate     plan.Snapshot
	inputs        plan.Inputs
	selection     plan.Selection
	// invoked is the job a run command invokes; empty for every other command.
	invoked string
}

// prepare runs the checks that need neither a lock, a state file nor Docker (Done: capability_unsupported
// happens before any of them), then derives the candidate snapshot from source.
func (e *Engine) prepare(ctx context.Context, r Request) (prepared, error) {
	env, err := e.checkRequest(r)
	if err != nil {
		return prepared{}, err
	}
	compose, ok := env.Runtime().(model.ComposeRuntime)
	if !ok {
		return prepared{}, fail(state.CodeCapabilityUnsupported, state.CategoryValidation, "environment %s selects a runtime this engine does not run", r.Scope.Environment)
	}
	inputs := plan.Inputs{Public: r.PublicInputs, Secret: map[string]bool{}}
	for key := range r.SecretInputs {
		inputs.Secret[key] = true
	}
	deliveries, err := e.deliveries(ctx, r.Project, r.Scope.Environment)
	if err != nil {
		return prepared{}, err
	}
	top, err := plan.BuildTopology(r.Project, r.Scope.Environment, inputs, deliveries)
	if err != nil {
		return prepared{}, classifyPlanError(err)
	}
	if r.Command == state.CommandUp && !r.All && len(r.Refs) == 0 {
		return prepared{}, fail(state.CodeUnknownObject, state.CategoryValidation, "select objects or ask for all")
	}
	sel, invoked, err := selectionOf(r, top)
	if err != nil {
		return prepared{}, err
	}
	return prepared{
		envName: r.Scope.Environment, dockerContext: compose.Context(), providerID: env.SecretProvider().String(),
		candidate: plan.Snapshot{Model: model.Encode(r.Project), Topology: top}, inputs: inputs, selection: sel, invoked: invoked,
	}, nil
}

// selectionOf resolves what the request selects. A run selects the job and what it binds, and gives
// the job's name; every other command selects its references or everything.
func selectionOf(r Request, top plan.Topology) (sel plan.Selection, invoked string, err error) {
	if r.Command == state.CommandRun {
		if invoked, err = invokedJob(r, top); err != nil {
			return plan.Selection{}, "", err
		}
		return plan.Selection{Names: runSelection(r.Project, top, invoked)}, invoked, nil
	}
	sel, err = plan.ResolveSelection(top, r.Refs, r.ExpandedScope)
	if err != nil {
		return plan.Selection{}, "", classifyPlanError(err)
	}
	sel.All = r.All
	return sel, "", nil
}

// checkRequest refuses a project that is unresolved, invalid, or needs a capability this engine does
// not advertise. It reads no file and starts no process.
func (e *Engine) checkRequest(r Request) (model.Environment, error) {
	p := r.Project
	if len(p.Unresolved()) > 0 {
		return model.Environment{}, fail(state.CodeProjectUnresolved, state.CategoryValidation, "the project has unresolved module definitions: %s", joinNames(p.Unresolved()))
	}
	env, ok := p.Environment(r.Scope.Environment)
	if !ok {
		return model.Environment{}, fail(state.CodeUnknownObject, state.CategoryNotFound, "environment %s is not declared", r.Scope.Environment)
	}
	if p.Name() != r.Scope.Project {
		return model.Environment{}, fail(state.CodeUnknownObject, state.CategoryNotFound, "project %s is not the loaded project %s", r.Scope.Project, p.Name())
	}
	if err := validateProject(p); err != nil {
		return model.Environment{}, err
	}
	if err := e.checkCapabilities(p, r.Scope.Environment); err != nil {
		return model.Environment{}, err
	}
	if err := checkInputs(p, r); err != nil {
		return model.Environment{}, err
	}
	return env, nil
}

func joinNames(names []model.ModuleName) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n.String()
	}
	return strings.Join(parts, ", ")
}

func classifyPlanError(err error) error {
	switch {
	case errors.Is(err, plan.ErrSecretInEnvironment):
		return fail(state.CodeSecretInEnvironment, state.CategoryValidation, "plan refused").wrap(err)
	case errors.Is(err, plan.ErrDeliveryConflict):
		return fail(state.CodeDeliveryConflict, state.CategoryValidation, "plan refused").wrap(err)
	case errors.Is(err, plan.ErrValueUnavailable):
		return fail(state.CodeValueUnavailable, state.CategoryValidation, "plan refused").wrap(err)
	case errors.Is(err, plan.ErrInputMissing):
		return fail(state.CodeInputMissing, state.CategoryValidation, "plan refused").wrap(err)
	case errors.Is(err, plan.ErrExpandedScopeRequired):
		return fail(state.CodeExpandedScopeRequired, state.CategoryConflict, "plan refused").wrap(err)
	case errors.Is(err, plan.ErrUnknownObject), errors.Is(err, plan.ErrInvalidName):
		return fail(state.CodeUnknownObject, state.CategoryValidation, "plan refused").wrap(err)
	}
	return err
}
