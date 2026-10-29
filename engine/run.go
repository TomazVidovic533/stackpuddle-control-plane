package engine

import (
	"context"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// RunRequest is one explicit invocation of a job.
type RunRequest struct {
	Scope Scope
	// Project is loaded by the caller; the engine refuses it while Unresolved() is non-empty.
	Project model.Project
	// ProjectDir is the absolute project directory; build contexts must stay inside it.
	ProjectDir string
	// Job is the job to invoke, as a workload reference. A reference to anything that is not a job
	// is validation.unknown_object.
	Job model.ObjectRef
	// Inputs are resolved by the caller. Job arguments are not accepted in m01.
	Inputs []InputValue
	// DryRun plans the invocation and writes nothing.
	DryRun bool
	// Wait is how long to wait for a busy environment; zero fails at once.
	Wait time.Duration
	// Progress, if set, is called after every persisted change, on Run's goroutine.
	Progress func(Operation)
}

// Run invokes one job and nothing else: it records an operation with the command run holding the
// provider and access steps the job needs and exactly one job step. The job runs even when its spec is
// unchanged, and each invocation is a new operation with its own ID, so the invocation ID is the
// operation ID and the step ID. The job's native exit code is on the step (Step.Job); a non-zero code
// fails the operation with job.exited_nonzero, apart from a runtime failure of the engine. A job runs
// only here or as the prerequisite of a service that is up, never because a group was selected.
// When an operation was recorded but did not succeed, Run returns it with the error.
func (e *Engine) Run(ctx context.Context, req RunRequest) (Result, error) {
	core := operation.RunRequest{
		Scope: operation.Scope(req.Scope), Project: req.Project, ProjectDir: req.ProjectDir, Job: req.Job,
		PublicInputs: map[string]string{}, SecretInputs: map[string]secrets.Value{}, DryRun: req.DryRun, Wait: req.Wait,
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
	res, err := e.core.Run(ctx, core)
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
