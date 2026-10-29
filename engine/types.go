package engine

import (
	"context"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// Command is what an operation asks of an environment.
type Command = state.Command

// The commands.
const (
	CommandUp      = state.CommandUp
	CommandStop    = state.CommandStop
	CommandDestroy = state.CommandDestroy
	CommandRun     = state.CommandRun
)

// Status is the lifecycle status of an operation.
type Status = state.OpStatus

// The operation statuses.
const (
	StatusRunning     = state.OpRunning
	StatusSucceeded   = state.OpSucceeded
	StatusFailed      = state.OpFailed
	StatusCancelled   = state.OpCancelled
	StatusInterrupted = state.OpInterrupted
	StatusBlocked     = state.OpBlocked
)

// StepStatus is the status of one step.
type StepStatus = state.StepStatus

// The step statuses.
const (
	StepPending                = state.StepPending
	StepStarted                = state.StepStarted
	StepCompleted              = state.StepCompleted
	StepCompletedByObservation = state.StepCompletedByObservation
	StepNotApplied             = state.StepNotApplied
	StepFailed                 = state.StepFailed
	StepBlocked                = state.StepBlocked
)

// ActorKind says who submitted an operation.
type ActorKind = state.ActorKind

// The actor kinds. m01 writes only ActorOSUser.
const (
	ActorOSUser = state.ActorOSUser
	ActorToken  = state.ActorToken
)

// Category is the class of an error, which decides how a caller reacts.
type Category = state.Category

// The error categories.
const (
	CategoryValidation = state.CategoryValidation
	CategoryNotFound   = state.CategoryNotFound
	CategoryConflict   = state.CategoryConflict
	CategoryForbidden  = state.CategoryForbidden
	CategoryTransient  = state.CategoryTransient
	CategoryUnexpected = state.CategoryUnexpected
)

// OperationID identifies one operation. It is comparable.
type OperationID = state.OperationID

// ParseOperationID parses text such as "op-01J9Z8K3M4N5P6Q7R8S9T0V1W2".
func ParseOperationID(text string) (OperationID, error) { return state.ParseOperationID(text) }

// SecretValue is a secret. Every print path shows "[redacted]".
type SecretValue = secrets.Value

// NewSecretValue wraps b as a secret value, which then owns b.
func NewSecretValue(b []byte) SecretValue { return secrets.NewValue(b) }

// Scope names one environment of a project.
type Scope struct {
	Project     model.ProjectName
	Environment model.EnvironmentName
}

// Selection says which objects an operation changes.
type Selection struct {
	all  bool
	refs []model.ObjectRef
}

// SelectAll selects every object of the project.
func SelectAll() Selection { return Selection{all: true} }

// SelectRefs selects the named workloads and module instances, plus the jobs they require.
func SelectRefs(refs ...model.ObjectRef) Selection {
	return Selection{refs: append([]model.ObjectRef(nil), refs...)}
}

// InputValue is one resolved input: public text, or a secret value.
type InputValue struct {
	key         string
	text        string
	secret      SecretValue
	secretInput bool
}

// PublicInput supplies the text of a public input.
func PublicInput(key, text string) InputValue { return InputValue{key: key, text: text} }

// SecretInput supplies the value of a secret input.
func SecretInput(key string, value SecretValue) InputValue {
	return InputValue{key: key, secret: value, secretInput: true}
}

// SubmitRequest is one request to change an environment.
type SubmitRequest struct {
	Scope Scope
	// Project is loaded by the caller; the engine refuses it while Unresolved() is non-empty.
	Project model.Project
	// ProjectDir is the absolute project directory; build contexts must stay inside it.
	ProjectDir string
	Command    Command
	Selection  Selection
	// Inputs are resolved by the caller.
	Inputs        []InputValue
	DryRun        bool
	ExpandedScope bool
	// DeleteData makes destroy also delete the owned volumes.
	DeleteData bool
	// Wait is how long to wait for a busy environment; zero fails at once.
	Wait time.Duration
	// Progress, if set, is called after every persisted change, on Submit's goroutine.
	Progress func(Operation)
}

// Submit changes the environment as the request says. A dry run takes no lock and writes nothing.
// When an operation was recorded but did not succeed, Submit returns it with the error.
func (e *Engine) Submit(ctx context.Context, req SubmitRequest) (Result, error) {
	core := operation.Request{
		Scope: operation.Scope(req.Scope), Project: req.Project, ProjectDir: req.ProjectDir, Command: req.Command,
		All: req.Selection.all, Refs: req.Selection.refs, PublicInputs: map[string]string{}, SecretInputs: map[string]secrets.Value{},
		DryRun: req.DryRun, ExpandedScope: req.ExpandedScope, DeleteData: req.DeleteData, Wait: req.Wait,
	}
	for _, in := range req.Inputs {
		if in.secretInput {
			core.SecretInputs[in.key] = in.secret
		} else {
			core.PublicInputs[in.key] = in.text
		}
	}
	if req.Progress != nil {
		scope := req.Scope
		core.Progress = func(op state.Operation, uid string) { req.Progress(withScopeUID(newOperation(scope, op), uid)) }
	}
	res, err := e.core.Submit(ctx, core)
	out := Result{}
	if res.Preview != nil {
		out.preview = newPreview(*res.Preview)
	}
	if res.Operation != nil {
		op := withScopeUID(newOperation(req.Scope, *res.Operation), res.ScopeUID)
		out.operation = &op
	}
	return out, toPublic(err)
}

// Result is the outcome of Submit.
type Result struct {
	preview   Preview
	operation *Operation
}

// Preview returns what a dry run would change; it is empty for a real run.
func (r Result) Preview() Preview { return r.preview }

// Operation returns the recorded operation; ok is false for a dry run and for a request that had
// nothing to do.
func (r Result) Operation() (Operation, bool) {
	if r.operation == nil {
		return Operation{}, false
	}
	return *r.operation, true
}

// Effect is what a preview row does. The set is closed.
type Effect int

// The effects. EffectBlocked marks a finding that blocks the operation and changes nothing.
const (
	EffectApply Effect = iota + 1
	EffectStop
	EffectDelete
	EffectBlocked
)

// String returns the lower-case name of the effect.
func (e Effect) String() string {
	switch e {
	case EffectApply:
		return "apply"
	case EffectStop:
		return "stop"
	case EffectDelete:
		return "delete"
	case EffectBlocked:
		return "blocked"
	}
	return "unknown"
}

// Action is one line of a preview: a planned step, or a finding that blocks the operation.
type Action struct {
	// Action is the step's action, empty for a blocked row.
	Action string
	Object string
	Effect Effect
	// Unknown is true when the plan cannot say whether the step still has anything to do: an open
	// operation already started it, or a module driver owns its effect.
	Unknown bool
	// Problem is the code of the finding of a blocked row, empty for a step.
	Problem Code
}

// Preview is the dry-run result: what would change, based on which generation.
type Preview struct {
	generation int64
	actions    []Action
}

func newPreview(p operation.Preview) Preview {
	out := Preview{generation: p.BasedOnGeneration}
	for _, a := range p.Actions {
		out.actions = append(out.actions, Action{Action: a.Action, Object: a.Object, Effect: effectOf(a.Impact), Unknown: a.Unknown, Problem: Code(a.Problem)})
	}
	return out
}

// effectOf is the one place an internal impact becomes a public effect.
func effectOf(impact plan.Impact) Effect {
	switch impact {
	case plan.ImpactStop:
		return EffectStop
	case plan.ImpactDelete:
		return EffectDelete
	case plan.ImpactBlocked:
		return EffectBlocked
	case plan.ImpactApply:
		return EffectApply
	}
	return EffectApply
}

// BasedOnGeneration returns the state generation the preview was computed from.
func (p Preview) BasedOnGeneration() int64 { return p.generation }

// Actions returns the planned actions in order. The slice is a copy.
func (p Preview) Actions() []Action { return append([]Action(nil), p.actions...) }

// CancelRequest names the operation to cancel.
type CancelRequest struct {
	Scope     Scope
	Operation OperationID
}
