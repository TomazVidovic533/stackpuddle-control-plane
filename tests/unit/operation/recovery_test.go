package operation_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

var opts = testkit.ShopOptions{Instances: []string{"primary"}}

// crashOnce makes the fake runtime report an unknown outcome the first time match accepts an effect,
// after (true) or before (false) the effect changed the inventory.
func crashOnce(h *harness, after bool, match func(plan.Effect) bool) {
	crashed := false
	hook := func(e plan.Effect) error {
		if !crashed && match(e) {
			crashed = true
			return plan.ErrOutcomeUnknown
		}
		return nil
	}
	if after {
		h.rt.AfterApply = hook
	} else {
		h.rt.BeforeApply = hook
	}
}

func isVolume(name string) func(plan.Effect) bool {
	return func(e plan.Effect) bool {
		v, ok := e.(plan.CreateVolume)
		return ok && v.Volume == name
	}
}

func stepFor(t *testing.T, op state.Operation, object string) state.Step {
	t.Helper()
	for _, s := range op.Steps {
		if s.Object == object {
			return s
		}
	}
	t.Fatalf("operation %s has no step for %s", op.ID, object)
	return state.Step{}
}

func TestRecovery_AStepKilledAfterItsEffectIsSettledByObservationWithNoDuplicate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	crashOnce(h, true, isVolume("api--uploads"))
	if _, err := h.up(opts); !errors.Is(err, plan.ErrOutcomeUnknown) {
		t.Fatalf("the crashed run error = %v", err)
	}
	open := h.state().Operation
	if open == nil || open.Status != state.OpRunning || stepFor(t, *open, "volume/api--uploads").Status != state.StepStarted {
		t.Fatalf("the journal does not show the open started step: %+v", open)
	}

	result, err := h.up(opts)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	old := h.archived(open.ID)
	if old.Status != state.OpInterrupted || old.ContinuedBy == nil || *old.ContinuedBy != result.Operation.ID {
		t.Errorf("old operation = %v continuedBy %v, new %v", old.Status, old.ContinuedBy, result.Operation.ID)
	}
	if result.Operation.Continues == nil || *result.Operation.Continues != open.ID {
		t.Errorf("the new operation does not name the one it continues: %v", result.Operation.Continues)
	}
	if got := stepFor(t, old, "volume/api--uploads").Status; got != state.StepCompletedByObservation {
		t.Errorf("settled step = %v, want completed-by-observation", got)
	}
	if n := h.rt.CallCount("apply plan.CreateVolume api--uploads"); n != 1 {
		t.Errorf("the volume was created %d times, want once", n)
	}
	if h.state().Operation != nil {
		t.Error("an operation is still open after the rerun")
	}
}

func TestRecovery_AStepKilledBeforeItsEffectIsNotApplied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	crashOnce(h, false, isVolume("api--uploads"))
	if _, err := h.up(opts); err == nil {
		t.Fatal("the crashed run succeeded")
	}
	open := h.state().Operation
	if _, err := h.up(opts); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got := stepFor(t, h.archived(open.ID), "volume/api--uploads").Status; got != state.StepNotApplied {
		t.Errorf("settled step = %v, want not-applied", got)
	}
	if n := h.rt.CallCount("apply plan.CreateVolume api--uploads"); n != 2 {
		t.Errorf("CreateVolume ran %d times, want the lost attempt and the retry", n)
	}
}

func TestRecovery_ACancelFileMakesTheOperationCancelledAndItIsNotContinued(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	crashOnce(h, true, isVolume("api--uploads"))
	_, _ = h.up(opts)
	open := h.state().Operation
	if err := h.eng.Cancel(t.Context(), h.scope(), open.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "environments", "shop.local", "cancel", open.ID.String()+".json")); err != nil {
		t.Fatalf("no cancel file: %v", err)
	}
	result, err := h.up(opts)
	if err != nil {
		t.Fatalf("a request after a cancel is a new operation: %v", err)
	}
	old := h.archived(open.ID)
	if old.Status != state.OpCancelled || old.ContinuedBy != nil || old.Error == nil || old.Error.Code != state.CodeOperationCancelled {
		t.Errorf("old operation = %v continuedBy %v error %v", old.Status, old.ContinuedBy, old.Error)
	}
	if result.Operation.Continues != nil {
		t.Errorf("a cancelled operation was continued by %s", result.Operation.ID)
	}
}

func TestRecovery_AModuleStepThatCannotBeObservedIsBlocked(t *testing.T) {
	t.Parallel()
	h := newHarness(t, driverCatalog())
	h.rt.ProbeFunc = func(call port.Call, p moduleProbe) (moduleResult, error) {
		if p.ID == "set" && len(h.rt.Secrets) > 0 {
			return moduleResult{}, plan.ErrOutcomeUnknown
		}
		return moduleResult{ID: p.ID, Stdout: "ok"}, nil
	}
	if _, err := h.up(opts); err == nil {
		t.Fatal("the crashed run succeeded")
	}
	open := h.state().Operation
	h.rt.ProbeFunc = nil
	result, err := h.up(opts)
	if codeOf(err) != state.CodeEffectFailed {
		t.Fatalf("a rerun that cannot settle a step must say so, got %v", err)
	}
	if result.Operation == nil || result.Operation.ID != open.ID || result.Operation.Status != state.OpBlocked {
		t.Fatalf("the caller must get the blocked operation to cancel, got %+v", result.Operation)
	}
	held := h.state().Operation
	if held == nil || held.ID != open.ID || held.Status != state.OpBlocked || held.ContinuedBy != nil || held.Error == nil || held.Error.Code != state.CodeEffectFailed {
		t.Fatalf("a blocked recovery must keep the operation open with its problem and no continuation: %+v", held)
	}
	if _, ok, err := h.open().LoadOperation(open.ID); ok || err != nil {
		t.Fatalf("a blocked operation was archived: %v %v", ok, err)
	}
	blocked := 0
	for _, s := range held.Steps {
		if s.Status == state.StepBlocked {
			blocked++
		}
	}
	if blocked != 1 {
		t.Errorf("%d blocked steps, want 1", blocked)
	}

	// Every later Submit observes again before it plans; none writes a continuation or applies anything.
	applies := h.rt.CallCount("apply")
	observes := h.rt.CallCount("observe")
	if _, err := h.up(opts); codeOf(err) != state.CodeEffectFailed {
		t.Fatalf("a second rerun = %v, want the same refusal", err)
	}
	if h.rt.CallCount("observe") == observes || h.rt.CallCount("apply") != applies {
		t.Error("the second rerun did not observe, or it applied an effect")
	}

	// Cancelling the blocked operation is the explicit way out.
	if err := h.eng.Cancel(t.Context(), h.scope(), open.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	result, err = h.up(opts)
	if err != nil {
		t.Fatalf("up after cancelling the blocked operation: %v", err)
	}
	if old := h.archived(open.ID); old.Status != state.OpCancelled || old.ContinuedBy != nil {
		t.Errorf("cancelled blocked operation = %v continuedBy %v", old.Status, old.ContinuedBy)
	}
	if result.Operation.Continues != nil {
		t.Errorf("a cancelled operation was continued by %s", result.Operation.ID)
	}
}

func TestRecovery_AFailedPlanAfterACleanRecoveryNamesNoContinuation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	crashOnce(h, true, isVolume("api--uploads"))
	if _, err := h.up(opts); err == nil {
		t.Fatal("the crashed run succeeded")
	}
	open := h.state().Operation
	failing := errors.New("observation is down")
	h.rt.FailObserveAfter(1, failing)
	if _, err := h.up(opts); err == nil {
		t.Fatal("the rerun succeeded with the runtime down")
	}
	if _, ok, err := h.open().LoadOperation(open.ID); ok || err != nil {
		t.Fatalf("the interrupted operation was archived naming a continuation that does not exist: %v %v", ok, err)
	}
	if got := h.state().Operation; got == nil || got.ID != open.ID {
		t.Fatalf("the open operation was cleared: %+v", got)
	}
	h.rt.FailObserveAfter(0, nil)
	result, err := h.up(opts)
	if err != nil {
		t.Fatalf("rerun after the runtime came back: %v", err)
	}
	if old := h.archived(open.ID); old.ContinuedBy == nil || *old.ContinuedBy != result.Operation.ID {
		t.Errorf("old operation continuedBy %v, new %v", old.ContinuedBy, result.Operation.ID)
	}
}

func TestRecovery_AContextInterruptedOperationIsContinuedByTheNextUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	req := h.request(opts, state.CommandUp)
	req.Progress = func(op state.Operation, _ string) {
		for _, s := range op.Steps {
			if s.Status == state.StepCompleted {
				cancel()
			}
		}
	}
	first, err := h.eng.Submit(ctx, req)
	if codeOf(err) != state.CodeOperationInterrupted || first.Operation == nil {
		t.Fatalf("first run: %v %v", first.Operation, err)
	}
	open := h.state().Operation
	if open == nil || open.ID != first.Operation.ID || open.Status != state.OpInterrupted {
		t.Fatalf("the interrupted operation is not left discoverable: %+v", open)
	}
	second, err := h.up(opts)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if second.Operation.Continues == nil || *second.Operation.Continues != first.Operation.ID {
		t.Errorf("the second up does not record continues: %v", second.Operation.Continues)
	}
	if old := h.archived(first.Operation.ID); old.Status != state.OpInterrupted || old.ContinuedBy == nil || *old.ContinuedBy != second.Operation.ID {
		t.Errorf("old operation = %v continuedBy %v", old.Status, old.ContinuedBy)
	}
}

func TestCancel_WhileRunningStopsBetweenSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	cancelled := false
	req := h.request(opts, state.CommandUp)
	req.Progress = func(op state.Operation, _ string) {
		done := 0
		for _, s := range op.Steps {
			if s.Status == state.StepCompleted {
				done++
			}
		}
		if done >= 2 && !cancelled {
			cancelled = true
			if err := h.eng.Cancel(t.Context(), h.scope(), op.ID); err != nil {
				t.Errorf("Cancel: %v", err)
			}
		}
	}
	result, err := h.submit(req)
	if codeOf(err) != state.CodeOperationCancelled {
		t.Fatalf("error = %v, want cancelled", err)
	}
	op := result.Operation
	if op.Status != state.OpCancelled {
		t.Fatalf("status = %v", op.Status)
	}
	var completed, notApplied int
	for _, s := range op.Steps {
		if s.Status == state.StepCompleted {
			completed++
		}
		if s.Status == state.StepNotApplied {
			notApplied++
		}
	}
	if completed != 2 || notApplied != len(op.Steps)-2 {
		t.Errorf("completed %d, not-applied %d of %d", completed, notApplied, len(op.Steps))
	}
	if h.archived(op.ID).Status != state.OpCancelled || h.state().Operation != nil {
		t.Error("the cancelled operation is not archived once and cleared")
	}
	// The work is not continued by itself; an explicit up finishes it.
	if _, err := h.up(opts); err != nil {
		t.Errorf("explicit up after a cancel: %v", err)
	}
}

func TestSubmit_ACancelledContextInterruptsBetweenSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	req := h.request(opts, state.CommandUp)
	req.Progress = func(op state.Operation, _ string) {
		for _, s := range op.Steps {
			if s.Status == state.StepCompleted {
				cancel()
			}
		}
	}
	result, err := h.eng.Submit(ctx, req)
	if codeOf(err) != state.CodeOperationInterrupted || result.Operation == nil || result.Operation.Status != state.OpInterrupted {
		t.Fatalf("result %+v error %v", result.Operation, err)
	}
}

func TestCancel_Errors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	result, err := h.up(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.eng.Cancel(t.Context(), h.scope(), result.Operation.ID); codeOf(err) != state.CodeOperationNotRunning {
		t.Errorf("cancelling a finished operation = %v", err)
	}
	other, _ := state.ParseOperationID("op-01J9Z8K3M4N5P6Q7R8S9T0V1W9")
	if err := h.eng.Cancel(t.Context(), h.scope(), other); codeOf(err) != state.CodeOperationNotFound {
		t.Errorf("cancelling an unknown operation = %v", err)
	}
}

func TestOwnership_EmptyStateBesideLabelledObjectsBlocks(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.root, "environments", "shop.local", "state.json")); err != nil {
		t.Fatal(err)
	}
	applies := h.rt.CallCount("apply")
	result, err := h.up(opts)
	if codeOf(err) != state.CodeUnmappedOwnedObject {
		t.Fatalf("error = %v, want unmapped_owned_object", err)
	}
	if result.Operation == nil || result.Operation.Status != state.OpBlocked || result.Operation.Error.Code != state.CodeUnmappedOwnedObject {
		t.Errorf("operation = %+v", result.Operation)
	}
	if h.rt.CallCount("apply") != applies {
		t.Error("a blocked operation changed the runtime")
	}
}

func TestOwnership_AVanishedMappedVolumeIsLostAndNotRecreated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	h.rt.RemoveVolume("api--uploads")
	for range 2 {
		result, err := h.up(opts)
		if codeOf(err) != state.CodeLostObject || result.Operation.Status != state.OpBlocked {
			t.Fatalf("error = %v, operation %+v", err, result.Operation)
		}
	}
	if got := h.state().Realizations["volume/api--uploads"].State; got != state.RealizationLost {
		t.Errorf("realization state = %v, want lost", got)
	}
	if n := h.rt.CallCount("apply plan.CreateVolume api--uploads"); n != 1 {
		t.Errorf("the lost volume was created %d times, want only the original", n)
	}
}

func TestSelection_UnselectedSourceEditsStayUnappliedThroughAKillAndRerun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	edited := testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "7200"}, ExtraWorkload: true}
	crashOnce(h, true, func(e plan.Effect) bool { _, ok := e.(plan.StartService); return ok })
	if _, err := h.up(edited, selecting("workload/api")); err == nil {
		t.Fatal("the crashed run succeeded")
	}
	if _, err := h.up(edited, selecting("workload/api")); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	st := h.state()
	var top plan.Topology
	mustJSON(t, st.Accepted.Topology, &top)
	if _, ok := top.Find("worker"); ok {
		t.Error("an unselected new workload entered the accepted revision")
	}
	if _, ok := st.Realizations["service/worker"]; ok || h.rt.Running("worker") {
		t.Error("the unselected workload was applied")
	}
	api, _ := top.Find("api")
	if api.Command[1] != "7200" {
		t.Errorf("the selected edit was not applied: %v", api.Command)
	}
}

func TestSelection_ASharedResourceChangeWithUnselectedConsumersIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	both := testkit.ShopOptions{}
	if _, err := h.up(both); err != nil {
		t.Fatal(err)
	}
	edited := testkit.ShopOptions{StoreImage: "mirror.gcr.io/library/busybox:1.37"}
	before := h.state()
	_, err := h.up(edited, selecting("module/primary"))
	if codeOf(err) != state.CodeExpandedScopeRequired {
		t.Fatalf("error = %v, want expanded_scope_required", err)
	}
	if after := h.state(); after.Generation != before.Generation {
		t.Error("a refused request wrote state")
	}
	if _, err := h.up(edited, selecting("module/primary"), func(r *operation.Request) { r.ExpandedScope = true }); err != nil {
		t.Errorf("with the expanded scope: %v", err)
	}
}

func TestSubmit_ASignalBetweenARoundsOfAStartedModuleStepLetsTheStepFinish(t *testing.T) {
	t.Parallel()
	h := newHarness(t, []module.Driver{drivers.TwoRound{}})
	h.rt.RefuseCancelled = true
	ctx, cancel := context.WithCancel(t.Context())
	var ran []string
	h.rt.ProbeFunc = func(_ port.Call, p module.Probe) (module.ProbeResult, error) {
		ran = append(ran, p.ID)
		if p.ID == "mutate" {
			cancel() // the signal lands between the mutation and its marker
		}
		return module.ProbeResult{ID: p.ID, ExitCode: 0}, nil
	}
	two := testkit.ShopOptions{Instances: []string{"primary", "reporting"}}
	first, err := h.eng.Submit(ctx, h.request(two, state.CommandUp))
	if codeOf(err) != state.CodeOperationInterrupted || first.Operation == nil || first.Operation.Status != state.OpInterrupted {
		t.Fatalf("result %+v error %v, want interrupted", first.Operation, err)
	}
	if !slices.Contains(ran, "mark") {
		t.Fatalf("probes %v: the marker probe never ran after the mutation", ran)
	}
	second, err := h.up(two)
	if err != nil {
		t.Fatalf("the next up does not continue: %v", err)
	}
	if second.Operation.Continues == nil || *second.Operation.Continues != first.Operation.ID {
		t.Errorf("continues = %v, want %v", second.Operation.Continues, first.Operation.ID)
	}
}

func TestSubmit_ASignalDuringAnEffectEndsInterruptedNotUnreachable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.rt.RefuseCancelled = true
	ctx, cancel := context.WithCancel(t.Context())
	once := false
	h.rt.AfterApply = func(plan.Effect) error {
		if !once {
			once = true
			cancel() // the signal lands inside Apply; the observation that follows must still run
		}
		return nil
	}
	result, err := h.eng.Submit(ctx, h.request(opts, state.CommandUp))
	if codeOf(err) != state.CodeOperationInterrupted || result.Operation == nil || result.Operation.Status != state.OpInterrupted {
		t.Fatalf("result %+v error %v, want interrupted and not %s", result.Operation, err, state.CodeTargetUnreachable)
	}
}
