package operation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// begin writes the new operation: revision N+1 when the request is an up, the plan as pending
// steps, and the fenced commit that prints the operation ID.
func (s *session) begin(pl plan.Plan, continues *state.OperationID) error {
	id := s.nextID
	if id.IsZero() {
		var err error
		if id, err = state.NewOperationID(s.e.now(), s.e.d.Random); err != nil {
			return err
		}
	}
	revision, err := s.acceptRequest(pl)
	if err != nil {
		return err
	}
	op := s.newOperation(id, continues, pl, revision)
	for _, key := range pl.Lost {
		s.update(key, func(r *state.Realization) { r.State = state.RealizationLost })
	}
	if err := s.archiveRecoveredBefore(id); err != nil {
		return err
	}
	s.st.Operation = op
	if err := s.commit(); err != nil {
		return err
	}
	s.noteChild(0)
	return nil
}

// acceptRequest records what the request accepts when nothing blocks it: revision N+1 for an up, the
// stopped intent for a stop. It returns the revision the operation submits.
func (s *session) acceptRequest(pl plan.Plan) (int, error) {
	revision := 0
	if s.st.Accepted != nil {
		revision = s.st.Accepted.Revision
	}
	if len(pl.Findings) > 0 {
		return revision, nil
	}
	switch s.req.Command {
	case state.CommandUp, state.CommandRun:
		revision++
		return revision, s.accept(revision)
	case state.CommandStop:
		s.recordStopIntent()
	case state.CommandDestroy:
	}
	return revision, nil
}

func (s *session) newOperation(id state.OperationID, continues *state.OperationID, pl plan.Plan, revision int) *state.Operation {
	now := s.e.now()
	op := &state.Operation{
		ID: id, Command: s.req.Command, Selection: s.req.selectionRecord(), Revision: max(revision, 1),
		BasedOnGeneration: s.st.Generation, Status: state.OpRunning, Continues: continues,
		Actor: state.Actor{Kind: state.ActorOSUser}, CreatedAt: now, UpdatedAt: now,
	}
	for i, ps := range pl.Steps {
		op.Steps = append(op.Steps, state.Step{ID: fmt.Sprintf("s%d", i+1), Action: ps.Action, Object: ps.Object, Status: state.StepPending})
	}
	return op
}

// stopTargets lists the services a stop selects whose accepted intent is not yet stopped. The intent is
// derived from the accepted request, never from whether an effect ran: a service that already exited
// still must not be restarted by a later up that does not select it.
func (s *session) stopTargets() []string {
	if s.req.Command != state.CommandStop || s.st.Accepted == nil {
		return nil
	}
	var names []string
	for _, w := range s.top.Workloads {
		selected := s.prep.selection.All || slices.Contains(s.prep.selection.Names, w.Name)
		if w.Type == plan.TypeService && selected && s.st.Accepted.Intent[w.Name] != state.IntentStopped {
			names = append(names, w.Name)
		}
	}
	return names
}

// recordStopIntent writes the stopped intent of every selected service into the accepted state, which
// the next commit persists with the operation.
func (s *session) recordStopIntent() {
	for _, name := range s.stopTargets() {
		s.setIntent(name, state.IntentStopped)
	}
}

// archiveRecoveredBefore writes the interrupted operation s.recovered with continuedBy set to id. It
// runs just before the commit that writes the continuing operation, so the archive names an operation
// that is about to exist. A crash between the two leaves the old operation open, and the next run
// archives it again naming the ID it mints.
func (s *session) archiveRecoveredBefore(id state.OperationID) error {
	if s.recovered == nil {
		return nil
	}
	s.recovered.ContinuedBy = &id
	if err := s.env.Archive(s.st.ScopeUID, *s.recovered); err != nil {
		return err
	}
	s.recovered = nil
	return nil
}

// accept records revision N+1: the merged snapshot, the public inputs and what was already known.
func (s *session) accept(revision int) error {
	topology, err := jsonOf(s.top)
	if err != nil {
		return err
	}
	next := &state.Accepted{Revision: revision, Model: s.mergedModel, Topology: topology, Inputs: map[string]string{}}
	for k, v := range s.req.PublicInputs {
		next.Inputs[k] = v
	}
	if prev := s.st.Accepted; prev != nil {
		next.Credentials, next.Intent = prev.Credentials, prev.Intent
	}
	if next.Credentials == nil {
		next.Credentials = map[string]state.Credential{}
	}
	if next.Intent == nil {
		next.Intent = map[string]state.Intent{}
	}
	s.st.Accepted = next
	return nil
}

// finishRun runs the steps, verifies, and closes the operation.
func (s *session) finishRun(ctx context.Context, pl plan.Plan) (Result, error) {
	if len(pl.Findings) > 0 {
		first := pl.Findings[0]
		err := s.finish(state.OpBlocked, &first.Problem, &Failure{Code: first.Problem.Code, Category: first.Problem.Category, Detail: first.Detail})
		return s.result(), err
	}
	s.steps = pl.Steps
	for i, ps := range pl.Steps {
		if err := s.checkContinue(ctx); err != nil {
			return s.result(), err
		}
		if err := s.runStep(ctx, i, ps); err != nil {
			return s.result(), err
		}
	}
	if s.req.Command == state.CommandUp {
		if err := s.verifyModules(ctx); err != nil {
			problem := problemOf(err)
			err = s.finish(state.OpFailed, &problem, err)
			return s.result(), err
		}
	}
	err := s.finish(state.OpSucceeded, nil, nil)
	return s.result(), err
}

func (s *session) result() Result {
	if s.last == nil {
		return Result{}
	}
	op := *s.last
	return Result{Operation: &op, ScopeUID: s.uid().String()}
}

// checkContinue stops between steps when the caller's context ended or a cancel request exists.
func (s *session) checkContinue(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return s.leaveInterrupted(fail(state.CodeOperationInterrupted, state.CategoryTransient, "operation %s was interrupted", s.st.Operation.ID).wrap(context.Cause(ctx)))
	}
	cancelled, err := s.env.CancelRequested(s.st.Operation.ID)
	if err != nil {
		return err
	}
	if cancelled {
		return s.finish(state.OpCancelled, &state.Problem{Code: state.CodeOperationCancelled, Category: state.CategoryConflict},
			fail(state.CodeOperationCancelled, state.CategoryConflict, "operation %s was cancelled", s.st.Operation.ID))
	}
	return nil
}

// leaveInterrupted records that the caller's context ended between steps. The operation is not
// archived: it stays open with status interrupted and its pending steps, exactly as a killed process
// would leave it, so the next Submit continues it and records continues.
func (s *session) leaveInterrupted(failure error) error {
	op := s.st.Operation
	now := s.e.now()
	op.Status, op.UpdatedAt, op.FinishedAt = state.OpInterrupted, now, &now
	op.Error = &state.Problem{Code: state.CodeOperationInterrupted, Category: state.CategoryTransient}
	if err := s.commit(); err != nil {
		return err
	}
	closed := *op
	s.last = &closed
	return failure
}

// finish closes the open operation: it archives it once, then clears it from state. When failure is
// non-nil it is returned after the state is settled.
func (s *session) finish(status state.OpStatus, problem *state.Problem, failure error) error {
	op := s.st.Operation
	now := s.e.now()
	op.Status, op.UpdatedAt, op.FinishedAt, op.Error = status, now, &now, problem
	for i := range op.Steps {
		if op.Steps[i].Status == state.StepPending {
			op.Steps[i].Status = state.StepNotApplied
		}
	}
	if status == state.OpSucceeded && s.req.Command == state.CommandDestroy {
		if err := s.forgetDestroyed(); err != nil {
			return err
		}
	}
	if err := s.env.Archive(s.st.ScopeUID, *op); err != nil {
		return err
	}
	closed := *op
	s.last = &closed
	if s.req.Progress != nil {
		s.req.Progress(closed, s.uid().String())
	}
	s.st.Operation = nil
	if err := s.commit(); err != nil {
		return err
	}
	return failure
}

// runStep runs one step: started, effect, observe, completed.
func (s *session) runStep(ctx context.Context, i int, ps plan.Step) error {
	op := s.st.Operation
	started := s.e.now()
	op.Steps[i].Status, op.Steps[i].StartedAt, op.UpdatedAt = state.StepStarted, &started, started
	if key, ok := s.realizationKeyOf(ps); ok {
		if err := s.ensureRealization(key); err != nil {
			return err
		}
	}
	if err := s.commit(); err != nil {
		return err
	}
	evidence, err := s.execute(ctx, op, i, ps)
	if err != nil {
		return s.stepFailed(i, err)
	}
	finished := s.e.now()
	op.Steps[i].Status, op.Steps[i].FinishedAt, op.Steps[i].Evidence, op.UpdatedAt = state.StepCompleted, &finished, evidence, finished
	return s.commit()
}

func (s *session) stepFailed(i int, err error) error {
	if errors.Is(err, plan.ErrOutcomeUnknown) {
		// The effect may have happened. The step stays started and the operation stays open, so the
		// next run observes before it plans. The caller still learns the operation ID.
		open := *s.st.Operation
		s.last = &open
		return fail(state.CodeEffectFailed, state.CategoryUnexpected, "step %s outcome is unknown; run again to settle it", s.st.Operation.Steps[i].ID).wrap(err)
	}
	op := s.st.Operation
	finished := s.e.now()
	problem := problemOf(err)
	op.Steps[i].Status, op.Steps[i].FinishedAt, op.Steps[i].Error = state.StepFailed, &finished, &problem
	return s.finish(state.OpFailed, &problem, err)
}

// problemOf is the stable part of an error: the code and category of a Failure, else an unexpected
// effect failure. It never carries the message, which could name a path or a command.
func problemOf(err error) state.Problem {
	var f *Failure
	if errors.As(err, &f) {
		return state.Problem{Code: f.Code, Category: f.Category}
	}
	return state.Problem{Code: state.CodeEffectFailed, Category: state.CategoryUnexpected}
}

func (s *session) execute(ctx context.Context, op *state.Operation, i int, ps plan.Step) (string, error) {
	switch {
	case ps.Action == plan.ActionSecretResolve || ps.Action == plan.ActionSecretCreate:
		return s.runSecretStep(ctx, op, i, ps)
	case ps.Module != nil:
		return s.applyModuleStep(ctx, ps)
	case ps.Action == plan.ActionTLSIssue:
		return s.runTLSStep(ctx, op, i)
	case ps.Verify != nil:
		return s.runVerifyStep(ctx, ps.Verify.Instance)
	case ps.Effect != nil:
		return s.runEffect(ctx, op, i, ps)
	}
	return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "step %s has nothing to run", ps.Action)
}

func (s *session) runEffect(ctx context.Context, op *state.Operation, i int, ps plan.Step) (string, error) {
	call, err := s.call(ctx, workloadSecretRefs(s.top))
	if err != nil {
		return "", err
	}
	call.Files = s.materialFiles()
	ev, err := s.e.d.Runtime.Apply(ctx, call, ps.Effect)
	if err != nil {
		return "", err
	}
	if ev.Job != nil {
		op.Steps[i].Job = ev.Job
	}
	// A signal cancels ctx after Apply; observing with the live context would
	// mislabel an interrupted step as target_unreachable.
	inv, err := s.observe(context.WithoutCancel(ctx))
	if err != nil {
		return "", err
	}
	s.inv = inv
	if ev.Job != nil && ev.Job.ExitCode != 0 {
		// The job ran and its own container failed: that is its result, not a failure of the engine.
		return "", fail(state.CodeJobExitedNonzero, state.CategoryConflict, "job %s exited with code %d", ps.Effect.Target(), ev.Job.ExitCode)
	}
	if err := s.fold(ps.Effect, &ev, inv); err != nil {
		return "", err
	}
	return ev.Text, nil
}

// workloadSecretRefs lists the secrets delivered as files into the topology's workloads, once each.
func workloadSecretRefs(top plan.Topology) []string {
	var refs []string
	for _, w := range top.Workloads {
		for _, f := range w.Files {
			if f.Source.Kind == plan.SourceSecret && !slices.Contains(refs, f.Source.Ref) {
				refs = append(refs, f.Source.Ref)
			}
		}
	}
	return refs
}

// materialFiles reads the public material the topology's files deliver, by source key. A certificate
// that does not exist yet is left out; the call then fails to resolve it when it matters.
func (s *session) materialFiles() map[string][]byte {
	files := map[string][]byte{}
	for _, w := range s.top.Workloads {
		for _, f := range w.Files {
			if f.Source.Kind != plan.SourceMaterial {
				continue
			}
			if data, ok := s.material(f.Source.Ref); ok {
				files[f.Source.Key()] = data
			}
		}
	}
	return files
}

// runSecretStep stores or resolves one secret and records its credential. The store write happens
// before the state commit; recovery settles a kill between them by the entry's createdBy.
func (s *session) runSecretStep(ctx context.Context, op *state.Operation, i int, ps plan.Step) (string, error) {
	refText := strings.TrimPrefix(ps.Object, "secret/")
	ref, err := secrets.ParseRef(refText)
	if err != nil {
		return "", err
	}
	createdBy := op.ID.String() + "/" + op.Steps[i].ID
	var version secrets.Version
	switch ps.Action {
	case plan.ActionSecretResolve:
		key := strings.TrimPrefix(refText, "input/")
		version, err = s.store.Import(ctx, ref, s.req.SecretInputs[key], createdBy)
	default:
		if cred, ok := s.st.Accepted.Credentials[refText]; ok {
			if _, err = s.store.Resolve(ctx, ref, cred.Version); err != nil {
				return "", err
			}
			return "kept the recorded version of " + refText, nil
		}
		size := 0
		if ps.Secret != nil {
			size = ps.Secret.Size
		}
		version, err = s.store.CreateIfAbsent(ctx, ref, size, createdBy)
	}
	if err != nil {
		return "", err
	}
	s.recordCredential(refText, version)
	return "stored version of " + refText, nil
}

func (s *session) recordCredential(ref string, v secrets.Version) {
	if s.st.Accepted.Credentials == nil {
		s.st.Accepted.Credentials = map[string]state.Credential{}
	}
	s.st.Accepted.Credentials[ref] = state.Credential{Provider: s.prep.providerID, Origin: v.Origin, Version: v.ID}
}

// realizationKeyOf is the realization an effect step creates or changes before it runs.
func (s *session) realizationKeyOf(ps plan.Step) (string, bool) {
	switch {
	case ps.Module != nil:
		return plan.RealizationKey("module", ps.Module.Instance), true
	case ps.Effect != nil:
		return s.effectKey(ps.Effect), true
	}
	return "", false
}

// effectKey is the realization key an effect acts on: a volume's own, or its workload's by type.
func (s *session) effectKey(e plan.Effect) string {
	switch effect := e.(type) {
	case plan.CreateVolume:
		return plan.RealizationKey("volume", effect.Volume)
	case plan.DeleteVolume:
		return plan.RealizationKey("volume", effect.Volume)
	case plan.ResolveImage, plan.BuildImage, plan.StartService, plan.RunJob, plan.StopService, plan.RemoveService:
		kind := plan.TypeService
		if w, ok := s.top.Find(e.Target()); ok {
			kind = w.Type
		}
		return plan.RealizationKey(kind, e.Target())
	}
	return ""
}

// forgetDestroyed removes what a destroy ended from the accepted intent: everything after a destroy of
// all, else the selected workloads. Data volumes that still exist stay mapped, so a later up adopts
// them instead of reporting them as unmapped.
func (s *session) forgetDestroyed() error {
	if s.st.Accepted == nil {
		return nil
	}
	names := s.prep.selection.Names
	if s.req.All {
		names = nil
		for _, w := range s.top.Workloads {
			names = append(names, w.Name)
		}
	}
	for _, name := range names {
		delete(s.st.Realizations, plan.RealizationKey(plan.TypeService, name))
		delete(s.st.Realizations, plan.RealizationKey(plan.TypeJob, name))
		delete(s.st.Accepted.Intent, name)
	}
	if s.req.All {
		for key := range s.st.Realizations {
			if !strings.HasPrefix(key, "volume/") {
				delete(s.st.Realizations, key)
			}
		}
		s.st.Accepted = nil
		return nil
	}
	remaining, err := plan.Without(plan.Snapshot{Model: s.st.Accepted.Model, Topology: s.top}, names)
	if err != nil {
		return err
	}
	topology, err := jsonOf(remaining.Topology)
	if err != nil {
		return err
	}
	s.st.Accepted.Model, s.st.Accepted.Topology = remaining.Model, topology
	return nil
}
