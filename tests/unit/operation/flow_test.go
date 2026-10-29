package operation_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func TestUp_RunsEveryStepInOrderAndArchivesTheOperation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	result, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}})
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	op := result.Operation
	if op == nil || op.Status != state.OpSucceeded || op.Actor.Kind != state.ActorOSUser || op.Revision != 1 || op.FinishedAt == nil {
		t.Fatalf("operation = %+v", op)
	}
	for _, step := range op.Steps {
		if step.Status != state.StepCompleted || step.StartedAt == nil || step.FinishedAt == nil {
			t.Errorf("step %s %s is %v", step.ID, step.Object, step.Status)
		}
	}
	if got := h.archived(op.ID); got.Status != state.OpSucceeded {
		t.Errorf("archived status = %v", got.Status)
	}
	st := h.state()
	if st.Operation != nil || st.Accepted == nil || st.Accepted.Revision != 1 {
		t.Fatalf("state after up: operation=%v accepted=%v", st.Operation, st.Accepted)
	}
	for _, key := range []string{"service/api", "job/migrate", "service/primary--store", "volume/api--uploads", "volume/primary--store--data"} {
		r, ok := st.Realizations[key]
		if !ok || r.State != state.RealizationActive || len(r.Mappings) == 0 && !strings.HasPrefix(key, "job/") && r.Artifact == nil {
			t.Errorf("realization %s = %+v (%v)", key, r, ok)
		}
	}
	if st.Accepted.Intent["api"] != state.IntentRunning {
		t.Errorf("intent = %v", st.Accepted.Intent)
	}
	if len(h.progress) < len(op.Steps)*2 {
		t.Errorf("Progress fired %d times for %d steps", len(h.progress), len(op.Steps))
	}
	scope := h.scopeFile()
	if scope.ComposeProject != "shop-local-"+scope.ScopeUID.Suffix() || scope.EngineID != "engine-1" {
		t.Errorf("scope = %+v", scope)
	}
}

func TestUp_ARepeatedUpDoesNothingAndPullsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}}); err != nil {
		t.Fatal(err)
	}
	pulls, applies := h.rt.Pulls, h.rt.CallCount("apply")
	before := h.state()

	result, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}})
	if err != nil || result.Operation != nil {
		t.Fatalf("a repeated up = %v, %v; want no operation", result.Operation, err)
	}
	if h.rt.Pulls != pulls || h.rt.CallCount("apply") != applies {
		t.Errorf("a repeated up changed the runtime: pulls %d->%d applies %d->%d", pulls, h.rt.Pulls, applies, h.rt.CallCount("apply"))
	}
	if after := h.state(); after.Generation != before.Generation {
		t.Errorf("a repeated up wrote state: generation %d -> %d", before.Generation, after.Generation)
	}
}

func TestUp_ABuildIsRecordedAndNeverRepeated(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	req := h.request(testkit.ShopOptions{NoJob: true, Instances: []string{"primary"}}, state.CommandUp)
	if _, err := h.submit(req); err != nil {
		t.Fatal(err)
	}
	if h.rt.Pulls != 2 || h.rt.Builds != 0 {
		t.Errorf("pulls %d builds %d, want 2 pulls (api and the store) and no builds", h.rt.Pulls, h.rt.Builds)
	}
	r := h.state().Realizations["service/api"]
	if r.Artifact == nil || !strings.HasSuffix(r.Artifact.Image, "@sha256:fake") {
		t.Errorf("recorded artifact = %+v", r.Artifact)
	}
}

func TestDryRun_TakesNoLockWritesNothingAndLeavesTheInventoryAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}}); err != nil {
		t.Fatal(err)
	}
	// Edit the source so the preview has something to show, and hold the lock so a dry run that took
	// it would be refused.
	held, err := lock.Acquire(t.Context(), h.open().LockPath(), lock.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	before, inventory := h.fileBytes("state.json"), h.rt.Snapshot()
	generation := h.state().Generation

	req := h.request(testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "7200"}}, state.CommandUp)
	req.DryRun = true
	result, err := h.eng.Submit(t.Context(), req)
	if err != nil {
		t.Fatalf("a dry run while the environment is held: %v", err)
	}
	if result.Preview == nil || result.Preview.BasedOnGeneration != generation || len(result.Preview.Actions) != 1 || result.Preview.Actions[0].Object != "service/api" {
		t.Fatalf("preview = %+v", result.Preview)
	}
	if result.Operation != nil {
		t.Error("a dry run returned an operation")
	}
	if string(h.fileBytes("state.json")) != string(before) {
		t.Error("a dry run wrote state")
	}
	if h.rt.Snapshot() != inventory {
		t.Error("a dry run changed the Docker inventory")
	}
}

func TestSubmit_AnotherWriterGetsBusyNamingTheHolderAndAWaitingOneProceeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}}); err != nil {
		t.Fatal(err)
	}
	held, err := lock.Acquire(t.Context(), h.open().LockPath(), lock.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.SetOwner(lock.Owner{Operation: "op-01J9Z8K3M4N5P6Q7R8S9T0V1W2", PID: 4242}); err != nil {
		t.Fatal(err)
	}
	_, err = h.up(testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "1"}})
	if !errors.Is(err, lock.ErrBusy) || !strings.Contains(err.Error(), "op-01J9Z8K3M4N5P6Q7R8S9T0V1W2") {
		t.Fatalf("error = %v, want busy naming the holder", err)
	}
	_ = held.Release()
	if _, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "1"}}); err != nil {
		t.Errorf("after the release: %v", err)
	}
}

func TestSubmit_ATargetThatChangedEngineStopsAndCreatesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}}); err != nil {
		t.Fatal(err)
	}
	h.rt.EngineID = "engine-2"
	calls := len(h.rt.Calls)
	_, err := h.up(testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "1"}})
	if codeOf(err) != state.CodeTargetReplaced {
		t.Fatalf("error = %v, want target_replaced", err)
	}
	if rest := h.rt.Calls[calls:]; !slices.Equal(rest, []string{"target"}) {
		t.Errorf("after the engine check the runtime was called: %v", rest)
	}
}

func TestSubmit_AnUnreachableTargetIsClassified(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.rt.TargetErr = errors.New("cannot connect")
	_, err := h.up(testkit.ShopOptions{})
	if codeOf(err) != state.CodeTargetUnreachable {
		t.Fatalf("error = %v", err)
	}
}

func TestStopAndAnUpWithoutTheSelectionNeverRestartTheStoppedObject(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	opts := testkit.ShopOptions{Instances: []string{"primary"}}
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := h.submit(h.request(opts, state.CommandStop), selecting("workload/api"), func(r *operation.Request) { r.Command = state.CommandStop }); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := h.state().Accepted.Intent["api"]; got != state.IntentStopped {
		t.Fatalf("intent after stop = %v", got)
	}
	// A later up of something else, even with the api's source edited, leaves the api stopped.
	edited := testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "7200"}}
	if _, err := h.up(edited, selecting("module/primary")); err != nil {
		t.Fatalf("up of the store: %v", err)
	}
	if h.rt.Running("api") {
		t.Fatalf("the stopped api was restarted:\n%s", h.rt.Snapshot())
	}
	// Selecting it starts it.
	if _, err := h.up(edited, selecting("workload/api")); err != nil {
		t.Fatalf("up of the api: %v", err)
	}
	if !h.rt.Running("api") {
		t.Errorf("an explicit up did not start the api:\n%s", h.rt.Snapshot())
	}
}

func TestStopOfAServiceThatAlreadyExitedStillRecordsTheStoppedIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	opts := testkit.ShopOptions{Instances: []string{"primary"}}
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	h.rt.ExitContainer("api")
	applies := h.rt.CallCount("apply")
	result, err := h.submit(h.request(opts, state.CommandStop), selecting("workload/api"))
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if h.rt.CallCount("apply") != applies {
		t.Error("stopping an exited service ran an effect")
	}
	if result.Operation == nil || result.Operation.Status != state.OpSucceeded || len(result.Operation.Steps) != 0 {
		t.Errorf("the stop must be recorded as an operation with no steps: %+v", result.Operation)
	}
	if got := h.state().Accepted.Intent["api"]; got != state.IntentStopped {
		t.Fatalf("intent after stopping an exited service = %v", got)
	}
	// An up of something else must not restart it.
	if _, err := h.up(opts, selecting("module/primary")); err != nil {
		t.Fatalf("up of the store: %v", err)
	}
	if h.rt.Running("api") {
		t.Errorf("the stopped api was restarted:\n%s", h.rt.Snapshot())
	}
	// A second stop has nothing left to record.
	if again, err := h.submit(h.request(opts, state.CommandStop), selecting("workload/api")); err != nil || again.Operation != nil {
		t.Errorf("a repeated stop = %+v, %v; want nothing to do", again.Operation, err)
	}
}

func TestDestroy_KeepsTheFolderScopeAndLockAndOnlyDeletesDataWhenAsked(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	opts := testkit.ShopOptions{Instances: []string{"primary"}}
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := h.submit(h.request(opts, state.CommandDestroy)); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if strings.Contains(h.rt.Snapshot(), "container") || !strings.Contains(h.rt.Snapshot(), "volume api--uploads") {
		t.Fatalf("after destroy without data deletion:\n%s", h.rt.Snapshot())
	}
	for _, name := range []string{"scope.json", "env.flock", "state.json"} {
		if len(h.fileBytes(name)) == 0 && name != "env.flock" {
			t.Errorf("%s is gone", name)
		}
	}
	st := h.state()
	if st.Accepted != nil {
		t.Errorf("destroy all left the accepted intent behind: %+v", st.Accepted)
	}
	for key := range st.Realizations {
		if !strings.HasPrefix(key, "volume/") {
			t.Errorf("destroy all left realization %s behind; only the data volumes stay mapped", key)
		}
	}
	// The data volumes outlive destroy and stay mapped, so a later up adopts them.
	if _, err := h.up(opts); err != nil {
		t.Fatalf("up after destroy: %v", err)
	}
}

func TestDestroyWithData_DeletesTheOwnedVolumes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	opts := testkit.ShopOptions{Instances: []string{"primary"}}
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	if _, err := h.submit(h.request(opts, state.CommandDestroy), func(r *operation.Request) { r.DeleteData = true }); err != nil {
		t.Fatal(err)
	}
	if snapshot := h.rt.Snapshot(); snapshot != "" {
		t.Errorf("objects remain after destroy with data:\n%s", snapshot)
	}
}

func TestPlanErrorsAreClassified(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	_, err := h.up(testkit.ShopOptions{}, selecting("workload/ghost"))
	if codeOf(err) != state.CodeUnknownObject {
		t.Fatalf("unknown ref error = %v", err)
	}
	_ = plan.TypeJob
}

func TestDestroyOfASelectionRemovesItFromTheAcceptedIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	destroy := h.request(opts, state.CommandDestroy)
	if _, err := h.submit(destroy, selecting("workload/api")); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	st := h.state()
	var top plan.Topology
	mustJSON(t, st.Accepted.Topology, &top)
	if _, ok := top.Find("api"); ok {
		t.Error("the destroyed workload is still in the accepted intent")
	}
	if _, ok := st.Realizations["service/api"]; ok {
		t.Error("the destroyed workload is still realized")
	}
	if _, ok := top.Find("primary--store"); !ok || !h.rt.Running("primary--store") {
		t.Error("destroying one workload touched another")
	}
	if _, err := h.up(opts, selecting("module/primary")); err != nil {
		t.Fatal(err)
	}
	if h.rt.Running("api") {
		t.Error("a later up brought back a workload that was destroyed")
	}
}

func TestStopAndDestroyAskNoDriverToVerify(t *testing.T) {
	t.Parallel()
	h := newHarness(t, driverCatalog())
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	probes := h.rt.CallCount("probe")
	if _, err := h.submit(h.request(opts, state.CommandStop)); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := h.submit(h.request(opts, state.CommandDestroy)); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if got := h.rt.CallCount("probe"); got != probes {
		t.Errorf("%d probes ran for stop and destroy; a stopped instance cannot answer one", got-probes)
	}
}

func TestAnUnknownOutcomeStillReturnsTheOpenOperation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	crashOnce(h, true, isVolume("api--uploads"))
	result, err := h.up(opts)
	if !errors.Is(err, plan.ErrOutcomeUnknown) || codeOf(err) != state.CodeEffectFailed {
		t.Fatalf("error = %v", err)
	}
	if result.Operation == nil || result.Operation.Status != state.OpRunning || result.Operation.ID != h.state().Operation.ID {
		t.Errorf("result operation = %+v, want the one left open", result.Operation)
	}
}
