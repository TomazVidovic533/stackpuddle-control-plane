package engine

import (
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Problem is the stable part of a failure recorded on an operation or a step: a code and a category.
type Problem struct {
	Code     Code
	Category Category
}

// Step is one action of an operation.
type Step struct {
	ID      string
	Action  string
	Object  string
	Status  StepStatus
	Problem *Problem
	// job is the native result of a job step, nil for every other step and until the job has exited.
	job *JobResult
	// started and finished are nil while the step has not started or finished.
	started, finished *time.Time
}

// JobResult is the native result of a job: the exit code of the job's own container. It is told
// apart from an engine failure, which is a Problem with a runtime code.
type JobResult struct {
	ExitCode int
}

// Job returns the native result of a job step; ok is false for any other step and for a job that has
// not exited.
func (s Step) Job() (JobResult, bool) {
	if s.job == nil {
		return JobResult{}, false
	}
	return *s.job, true
}

// StartedAt returns when the step started; ok is false while it has not.
func (s Step) StartedAt() (time.Time, bool) { return timeOf(s.started) }

// FinishedAt returns when the step finished; ok is false while it has not.
func (s Step) FinishedAt() (time.Time, bool) { return timeOf(s.finished) }

func timeOf(t *time.Time) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	return *t, true
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// Operation is the public record of an engine operation. It carries no JSON tags: the command-line
// interface maps it to the published Operation schema.
type Operation struct {
	scope Scope
	op    state.Operation
	uid   string
}

func newOperation(scope Scope, op state.Operation) Operation {
	return Operation{scope: scope, op: op}
}

func withScopeUID(o Operation, uid string) Operation {
	o.uid = uid
	return o
}

// ID returns the operation ID.
func (o Operation) ID() OperationID { return o.op.ID }

// Scope returns the project and environment.
func (o Operation) Scope() Scope { return o.scope }

// ScopeUID returns the scope uid, or "" when the operation was not read from a scope.
func (o Operation) ScopeUID() string { return o.uid }

// Command returns the command.
func (o Operation) Command() Command { return o.op.Command }

// SelectionRefs returns the selected references, or nil when everything was selected.
func (o Operation) SelectionRefs() []string { return append([]string(nil), o.op.Selection.Refs...) }

// SelectedAll reports whether everything was selected.
func (o Operation) SelectedAll() bool { return o.op.Selection.All }

// ExpandedScope reports whether the request carried the expanded scope.
func (o Operation) ExpandedScope() bool { return o.op.Selection.ExpandedScope }

// Revision returns the accepted revision the operation submits.
func (o Operation) Revision() int { return o.op.Revision }

// Status returns the lifecycle status.
func (o Operation) Status() Status { return o.op.Status }

// Steps returns the steps in order. The slice is a copy.
func (o Operation) Steps() []Step {
	steps := make([]Step, 0, len(o.op.Steps))
	for _, s := range o.op.Steps {
		step := Step{ID: s.ID, Action: s.Action, Object: s.Object, Status: s.Status, started: copyTime(s.StartedAt), finished: copyTime(s.FinishedAt)}
		if s.Job != nil {
			result := JobResult{ExitCode: s.Job.ExitCode}
			step.job = &result
		}
		if s.Error != nil {
			step.Problem = &Problem{Code: Code(s.Error.Code), Category: s.Error.Category}
		}
		steps = append(steps, step)
	}
	return steps
}

// Continues returns the operation this one continues, if any.
func (o Operation) Continues() (OperationID, bool) {
	if o.op.Continues == nil {
		return OperationID{}, false
	}
	return *o.op.Continues, true
}

// ContinuedBy returns the operation that continues this one, if any.
func (o Operation) ContinuedBy() (OperationID, bool) {
	if o.op.ContinuedBy == nil {
		return OperationID{}, false
	}
	return *o.op.ContinuedBy, true
}

// Actor returns who submitted the operation.
func (o Operation) Actor() ActorKind { return o.op.Actor.Kind }

// CreatedAt returns when the operation was recorded, in UTC.
func (o Operation) CreatedAt() time.Time { return o.op.CreatedAt }

// UpdatedAt returns when the operation last changed, in UTC.
func (o Operation) UpdatedAt() time.Time { return o.op.UpdatedAt }

// FinishedAt returns when the operation ended; ok is false while it runs.
func (o Operation) FinishedAt() (time.Time, bool) {
	if o.op.FinishedAt == nil {
		return time.Time{}, false
	}
	return *o.op.FinishedAt, true
}

// Problem returns why the operation did not succeed, if it did not.
func (o Operation) Problem() (Problem, bool) {
	if o.op.Error == nil {
		return Problem{}, false
	}
	return Problem{Code: Code(o.op.Error.Code), Category: o.op.Error.Category}, true
}
