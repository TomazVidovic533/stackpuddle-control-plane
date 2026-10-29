package operation

import (
	"context"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// recoverOpen settles an operation a dead process left open. Each started or blocked step is observed
// and settled as completed-by-observation, not-applied or blocked. Then:
//
//   - a cancelled operation is archived and continued by nothing, even with a step left blocked: the
//     caller asked to stop, and the plan that follows observes the runtime itself;
//   - an operation with a step observation cannot settle stays open as blocked, with its problem and no
//     continuation, so every later Submit observes again before it plans;
//   - any other operation is continued: the returned ID is the old one and the continuing operation's
//     ID is minted, but nothing is written until begin, which archives the old record naming it in the
//     same step that commits the continuing operation.
func (s *session) recoverOpen(ctx context.Context) (*state.OperationID, error) {
	op := s.st.Operation
	if op == nil {
		return nil, nil //nolint:nilnil // reason: no open operation means nothing continues
	}
	cancelled, err := s.env.CancelRequested(op.ID)
	if err != nil {
		return nil, err
	}
	inv, err := s.observeAccepted(ctx)
	if err != nil {
		return nil, err
	}
	blocked := s.settleSteps(op, inv)
	switch {
	case closedBeforeArchive(op.Status):
		return nil, s.archiveRecovered(op)
	case cancelled || op.Status == state.OpCancelled:
		s.closeCancelled(op)
		return nil, s.archiveRecovered(op)
	case blocked != "":
		return nil, s.holdBlocked(op, blocked)
	}
	return s.continuation(op)
}

// closedBeforeArchive reports a status that is final and was recorded by the run itself, so only the
// archive file is missing.
func closedBeforeArchive(status state.OpStatus) bool {
	return status.Finished() && status != state.OpInterrupted && status != state.OpCancelled && status != state.OpBlocked
}

func blockedFailure(op *state.Operation, step string) error {
	if step == "" {
		return nil
	}
	return fail(state.CodeEffectFailed, state.CategoryUnexpected, "step %s of operation %s could not be settled by observation; inspect the environment, then cancel the operation or run again", step, op.ID)
}

// archiveRecovered writes the recovered operation to its archive and clears it from state.
func (s *session) archiveRecovered(op *state.Operation) error {
	if err := s.env.Archive(s.st.ScopeUID, *op); err != nil {
		return err
	}
	s.st.Operation = nil
	return s.commit()
}

// holdBlocked records that observation could not settle a step. The operation stays open with its
// problem and no continuation.
func (s *session) holdBlocked(op *state.Operation, step string) error {
	op.Status, op.UpdatedAt = state.OpBlocked, s.e.now()
	op.Error = &state.Problem{Code: state.CodeEffectFailed, Category: state.CategoryUnexpected}
	if err := s.commit(); err != nil {
		return err
	}
	held := *op
	s.last = &held
	return blockedFailure(op, step)
}

func (s *session) closeCancelled(op *state.Operation) {
	now := s.e.now()
	op.UpdatedAt, op.FinishedAt = now, &now
	op.Status = state.OpCancelled
	op.Error = &state.Problem{Code: state.CodeOperationCancelled, Category: state.CategoryConflict}
}

// continuation marks the recovered operation interrupted and mints the ID of its continuation. It
// writes nothing: s.recovered is archived by begin.
func (s *session) continuation(op *state.Operation) (*state.OperationID, error) {
	now := s.e.now()
	op.UpdatedAt = now
	if op.FinishedAt == nil {
		op.FinishedAt = &now
	}
	op.Status = state.OpInterrupted
	op.Error = &state.Problem{Code: state.CodeOperationInterrupted, Category: state.CategoryTransient}
	next, err := state.NewOperationID(now, s.e.d.Random)
	if err != nil {
		return nil, err
	}
	old := op.ID
	recovered := *op
	s.nextID, s.recovered = next, &recovered
	return &old, nil
}

// observeAccepted observes the runtime against the accepted topology, which is what recovery plans from.
func (s *session) observeAccepted(ctx context.Context) (plan.Inventory, error) {
	accepted, err := s.accepted()
	if err != nil {
		return plan.Inventory{}, err
	}
	if accepted != nil {
		s.top = accepted.Topology
	}
	inv, err := s.observe(ctx)
	s.inv = inv
	return inv, err
}

// settleSteps observes every started or blocked step and settles it. It returns the ID of a step left
// blocked; when the topology cannot be rendered no step can be settled, so every one of them is.
func (s *session) settleSteps(op *state.Operation, inv plan.Inventory) string {
	blocked := ""
	hashes, err := s.currentHashes()
	for i := range op.Steps {
		step := &op.Steps[i]
		switch step.Status {
		case state.StepPending:
			step.Status = state.StepNotApplied
		case state.StepStarted, state.StepBlocked:
			step.Status = state.StepBlocked
			if err == nil {
				step.Status = s.settle(op, *step, inv, hashes)
			}
			if step.Status == state.StepBlocked && blocked == "" {
				blocked = step.ID
			}
			finished := s.e.now()
			step.FinishedAt = &finished
		case state.StepCompleted, state.StepCompletedByObservation, state.StepNotApplied, state.StepFailed:
		}
	}
	return blocked
}

func (s *session) settle(op *state.Operation, step state.Step, inv plan.Inventory, hashes map[string]string) state.StepStatus {
	switch {
	case step.Action == plan.ActionSecretResolve || step.Action == plan.ActionSecretCreate:
		return s.settleSecret(op, step)
	case step.Action == plan.ActionTLSIssue || step.Action == plan.ActionModuleVerify:
		// Issuing reuses the stored key and verifying only reads, so repeating either is safe.
		return state.StepNotApplied
	case !strings.HasPrefix(step.Action, "compose."):
		return state.StepBlocked
	}
	effect, ok := s.effectOf(step)
	if !ok {
		return state.StepBlocked
	}
	applied, known := plan.Observed(effect, inv, hashes)
	if !known || !applied {
		return state.StepNotApplied
	}
	if err := s.fold(effect, nil, inv); err != nil {
		return state.StepBlocked
	}
	return state.StepCompletedByObservation
}

// settleSecret decides a secret step by the store entry's createdBy, which names the operation and
// step that wrote it.
func (s *session) settleSecret(op *state.Operation, step state.Step) state.StepStatus {
	refText := strings.TrimPrefix(step.Object, "secret/")
	ref, err := secrets.ParseRef(refText)
	if err != nil {
		return state.StepBlocked
	}
	entry, ok := s.store.Entry(ref)
	if !ok {
		return state.StepNotApplied
	}
	createdBy := op.ID.String() + "/" + step.ID
	for _, v := range entry.Versions {
		if v.CreatedBy == createdBy {
			if s.st.Accepted != nil {
				s.recordCredential(refText, v)
			}
			return state.StepCompletedByObservation
		}
	}
	return state.StepNotApplied
}

// effectOf rebuilds the effect of a persisted step from its action and object.
func (s *session) effectOf(step state.Step) (plan.Effect, bool) {
	_, name, ok := strings.Cut(step.Object, "/")
	if !ok {
		return nil, false
	}
	if step.Action == plan.ActionImage {
		if w, found := s.top.Find(name); found && w.Build != nil {
			return plan.BuildImage{Workload: name}, true
		}
		return plan.ResolveImage{Workload: name}, true
	}
	return runtimeEffect(step.Action, name)
}

func runtimeEffect(action, name string) (plan.Effect, bool) {
	switch action {
	case plan.ActionVolume:
		return plan.CreateVolume{Volume: name}, true
	case plan.ActionJob:
		return plan.RunJob{Workload: name}, true
	case plan.ActionService:
		return plan.StartService{Workload: name}, true
	case plan.ActionStop:
		return plan.StopService{Workload: name}, true
	case plan.ActionRemove:
		return plan.RemoveService{Workload: name}, true
	case plan.ActionVolumeDelete:
		return plan.DeleteVolume{Volume: name}, true
	}
	return nil, false
}
