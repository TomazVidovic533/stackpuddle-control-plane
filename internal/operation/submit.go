package operation

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Submit runs one request in the order: checks that need nothing, lock, load, secret store, target
// check, recovery, observation, merge, plan, fenced write, steps, verification. A dry run stops
// after the plan and takes no lock. A request with a result and an error returns both: the
// operation that was recorded and why it did not succeed.
func (e *Engine) Submit(ctx context.Context, r Request) (Result, error) {
	prep, err := e.prepare(ctx, r)
	if err != nil {
		return Result{}, err
	}
	if r.DryRun {
		return e.dryRun(ctx, r, prep)
	}
	return e.execute(ctx, r, prep)
}

func (e *Engine) open(r Request) (*state.Env, error) {
	return state.OpenEnv(e.d.Settings.StateDir(), r.Scope.Project.String(), r.Scope.Environment.String(), e.d.State)
}

func (e *Engine) execute(ctx context.Context, r Request, prep prepared) (Result, error) {
	env, err := state.CreateEnv(e.d.Settings.StateDir(), r.Scope.Project.String(), r.Scope.Environment.String(), e.d.State)
	if err != nil {
		return Result{}, err
	}
	defer env.Close()
	if err := lock.CheckFilesystem(env.Dir(), config.KeyStateDir); err != nil {
		return Result{}, err
	}
	held, err := lock.Acquire(ctx, env.LockPath(), lock.AcquireOptions{Wait: r.Wait, Random: e.d.Random, Now: e.now})
	if err != nil {
		return Result{}, err
	}
	defer held.Release() //nolint:errcheck // reason: closing the descriptor is the release; there is nothing to recover
	s := &session{e: e, req: r, prep: prep, env: env, held: held}
	return s.run(ctx)
}

// run is the locked part of a Submit.
func (s *session) run(ctx context.Context) (Result, error) {
	if err := s.load(); err != nil {
		return Result{}, err
	}
	if err := s.openStore(ctx); err != nil {
		return Result{}, err
	}
	if err := s.checkTarget(ctx); err != nil {
		return Result{}, err
	}
	continues, err := s.recoverOpen(ctx)
	if err != nil {
		return s.result(), err
	}
	pl, err := s.prepareStep(ctx)
	if err != nil {
		return Result{}, err
	}
	if s.nothingToDo(pl, continues) {
		return Result{}, nil
	}
	if err := s.begin(pl, continues); err != nil {
		return Result{}, err
	}
	return s.finishRun(ctx, pl)
}

// nothingToDo reports a request that changes nothing and continues nothing, so no operation is
// recorded. A stop that still has an intent to record is not one of them.
func (s *session) nothingToDo(pl plan.Plan, continues *state.OperationID) bool {
	return len(pl.Steps) == 0 && len(pl.Findings) == 0 && continues == nil && len(s.stopTargets()) == 0
}

func (s *session) load() error {
	scope, hasScope, err := s.env.LoadScope()
	if err != nil {
		return err
	}
	st, hasState, err := s.env.Load()
	if err != nil {
		return err
	}
	s.scope, s.hasScope, s.st, s.hasState = scope, hasScope, st, hasState
	if !hasScope {
		uid, err := state.NewScopeUID(s.e.now(), s.e.d.Random)
		if err != nil {
			return err
		}
		s.newUID = uid
	}
	if !hasState {
		s.st = state.State{StateVersion: state.StateVersion, ScopeUID: s.uid()}
	}
	if hasScope && hasState && st.ScopeUID != scope.ScopeUID {
		return fmt.Errorf("%w: state.json belongs to another scope than scope.json", state.ErrUnsupported)
	}
	return nil
}

func (s *session) openStore(ctx context.Context) error {
	provider, ok := s.e.d.Providers[s.prep.providerID]
	if !ok {
		return fail(state.CodeCapabilityUnsupported, state.CategoryValidation, "no secret provider %q", s.prep.providerID)
	}
	ref := secrets.ScopeRef{UID: s.uid()}
	if s.hasScope {
		ref.KeyID = s.scope.KeyID
	}
	store, err := provider.Open(ctx, ref)
	if err != nil {
		return err
	}
	s.store = store
	if s.st.Accepted == nil {
		return nil
	}
	for name, cred := range s.st.Accepted.Credentials {
		parsed, err := secrets.ParseRef(name)
		if err != nil {
			return fmt.Errorf("%w: %w", state.ErrUnsupported, err)
		}
		if _, err := store.Resolve(ctx, parsed, cred.Version); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) checkTarget(ctx context.Context) error {
	info, err := s.e.d.Runtime.Target(ctx, port.Call{DockerContext: s.prep.dockerContext})
	if err != nil {
		return fail(state.CodeTargetUnreachable, state.CategoryTransient, "docker context %q did not answer", s.prep.dockerContext).wrap(err)
	}
	if s.hasScope {
		if s.scope.EngineID != info.EngineID {
			return fail(state.CodeTargetReplaced, state.CategoryConflict, "docker context %q now reaches a different engine than the one this scope was created on; nothing was changed", s.prep.dockerContext)
		}
		return nil
	}
	uid := s.newUID
	s.scope = state.Scope{
		ScopeUID: uid, Project: s.req.Scope.Project.String(), Environment: s.req.Scope.Environment.String(),
		DockerContext: s.prep.dockerContext, EngineID: info.EngineID, KeyID: s.store.KeyID(), CreatedAt: s.e.now(),
		ComposeProject: fmt.Sprintf("%s-%s-%s", s.req.Scope.Project, s.req.Scope.Environment, uid.Suffix()),
	}
	if err := s.env.WriteScope(s.scope); err != nil {
		return err
	}
	s.hasScope = true
	return nil
}

// prepareStep observes the runtime, merges the selection into revision N+1 and plans.
func (s *session) prepareStep(ctx context.Context) (plan.Plan, error) {
	inv, err := s.observe(ctx)
	if err != nil {
		return plan.Plan{}, err
	}
	pl, err := s.buildPlan(ctx, inv)
	if err != nil {
		return plan.Plan{}, err
	}
	return pl, nil
}

// buildPlan merges, renders and diffs. It is shared by the dry run and the real run.
func (s *session) buildPlan(ctx context.Context, inv plan.Inventory) (plan.Plan, error) {
	accepted, err := s.accepted()
	if err != nil {
		return plan.Plan{}, err
	}
	var merged plan.Snapshot
	switch s.req.Command {
	case state.CommandUp, state.CommandRun:
		merged, err = plan.Merge(accepted, s.prep.candidate, s.prep.selection)
		if err != nil {
			return plan.Plan{}, classifyPlanError(err)
		}
	case state.CommandStop, state.CommandDestroy:
		if accepted == nil {
			return plan.Plan{}, nil
		}
		merged = *accepted
	}
	s.top = merged.Topology
	s.mergedModel = merged.Model
	rendered, err := s.render()
	if err != nil {
		return plan.Plan{}, err
	}
	s.hashes = rendered.SpecHashes
	pl, err := s.diff(ctx, inv, rendered)
	if err != nil {
		return plan.Plan{}, err
	}
	s.inv = inv
	return pl, nil
}

// diff plans the merged topology. An up or a run also decides which certificates it issues, which
// restarts the workloads that receive a changed one, and adds the secret, certificate and module steps
// in deployment order.
func (s *session) diff(ctx context.Context, inv plan.Inventory, rendered plan.Rendered) (plan.Plan, error) {
	adds := s.req.Command == state.CommandUp || s.req.Command == state.CommandRun
	var tlsSteps []plan.Step
	var reissued map[string]bool
	if adds {
		tlsSteps, reissued = s.tlsPlan()
	}
	pl := plan.Diff(plan.DiffInput{
		Desired: s.top, State: s.st, Inventory: inv, Command: s.req.Command,
		Selection: s.prep.selection, DeleteData: s.req.DeleteData, SpecHashes: rendered.SpecHashes,
		Invoke: s.prep.invoked, Reissued: reissued,
	})
	if !adds {
		return pl, nil
	}
	return s.addSecretAndModuleSteps(ctx, pl, tlsSteps)
}

// dryRun plans without a lock and writes nothing, not even the environment folder.
func (e *Engine) dryRun(ctx context.Context, r Request, prep prepared) (Result, error) {
	env, err := e.open(r)
	s := &session{e: e, req: r, prep: prep}
	switch {
	case err == nil:
		defer env.Close()
		s.env = env
		if err := s.load(); err != nil {
			return Result{}, err
		}
	case errors.Is(err, fs.ErrNotExist):
		uid, err := state.NewScopeUID(e.now(), e.d.Random)
		if err != nil {
			return Result{}, err
		}
		s.newUID = uid
		s.st = state.State{StateVersion: state.StateVersion, ScopeUID: uid}
	default:
		return Result{}, err
	}
	inv, err := s.observe(ctx)
	if err != nil {
		return Result{}, err
	}
	pl, err := s.buildPlan(ctx, inv)
	if err != nil {
		return Result{}, err
	}
	preview := &Preview{BasedOnGeneration: s.st.Generation}
	for _, step := range pl.Steps {
		preview.Actions = append(preview.Actions, Action{Action: step.Action, Object: step.Object, Impact: step.Impact(), Unknown: step.Unknown})
	}
	for _, f := range pl.Findings {
		preview.Actions = append(preview.Actions, Action{Object: f.Object, Impact: plan.ImpactBlocked, Problem: f.Problem.Code})
	}
	return Result{Preview: preview}, nil
}
