package operation

import (
	"context"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// RunRequest is one explicit invocation of a job.
type RunRequest struct {
	Scope      Scope
	Project    model.Project
	ProjectDir string
	// Job is the job to invoke, as a workload reference.
	Job model.ObjectRef
	// PublicInputs and SecretInputs are resolved by the caller, as for Submit.
	PublicInputs map[string]string
	SecretInputs map[string]secrets.Value
	DryRun       bool
	// Wait is how long to wait for a busy environment; zero fails at once.
	Wait time.Duration
	// Progress is called after every persisted change, on Run's goroutine.
	Progress func(op state.Operation, scopeUID string)
}

// Run records an operation with the command run: the provider and access steps the job needs, then
// exactly one job step. The job runs even when its spec is unchanged, and it is the only job that
// does; its own prerequisites run only when their spec changed. The step records the job's native
// exit code; a non-zero code fails the operation with job.exited_nonzero, never with
// runtime.effect_failed, which stays the engine's own failure. Each invocation is a new operation, so
// it has its own invocation ID, "<operation id>/<step id>". A dry run writes nothing. The result and
// error follow Submit.
func (e *Engine) Run(ctx context.Context, r RunRequest) (Result, error) {
	return e.Submit(ctx, Request{
		Scope: r.Scope, Project: r.Project, ProjectDir: r.ProjectDir, Command: state.CommandRun,
		Refs: []model.ObjectRef{r.Job}, PublicInputs: r.PublicInputs, SecretInputs: r.SecretInputs,
		DryRun: r.DryRun, Wait: r.Wait, Progress: r.Progress,
	})
}

// invokedJob checks that a run names exactly one job of the project and returns its workload name.
// Anything else is validation.unknown_object, before any lock or effect.
func invokedJob(r Request, top plan.Topology) (string, error) {
	if len(r.Refs) != 1 || r.Refs[0].Kind() != model.ObjectWorkload {
		return "", fail(state.CodeUnknownObject, state.CategoryValidation, "a run names exactly one job")
	}
	name := r.Refs[0].Key().String()
	w, ok := top.Find(name)
	if !ok || w.Type != plan.TypeJob || w.Instance != "" {
		return "", fail(state.CodeUnknownObject, state.CategoryValidation, "%s is not a job of the project", r.Refs[0])
	}
	return name, nil
}
