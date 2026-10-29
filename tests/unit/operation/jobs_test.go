package operation_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func (h *harness) run(job string, change ...func(*operation.RunRequest)) (operation.Result, error) {
	h.t.Helper()
	ref, err := model.ParseObjectRef("workload/" + job)
	if err != nil {
		h.t.Fatal(err)
	}
	req := operation.RunRequest{
		Scope: h.scope(), Project: testkit.Bound(h.t, testkit.BoundOptions{}), ProjectDir: h.root, Job: ref,
		Progress: func(op state.Operation, _ string) { h.progress = append(h.progress, op) },
	}
	for _, c := range change {
		c(&req)
	}
	return h.eng.Run(h.t.Context(), req)
}

func stepOf(t *testing.T, op state.Operation, action string) state.Step {
	t.Helper()
	for _, s := range op.Steps {
		if s.Action == action {
			return s
		}
	}
	t.Fatalf("operation %s has no %s step: %v", op.ID, action, steps(op))
	return state.Step{}
}

func TestJobs_AFailedMigrationBlocksTheRolloutAndKeepsItsNativeExitCode(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	h.rt.JobExit = map[string]int{"migrate": 3}
	result, err := h.upBound(testkit.BoundOptions{})
	failure, ok := errors.AsType[*operation.Failure](err)
	if !ok || failure.Code != state.CodeJobExitedNonzero || failure.Category != state.CategoryConflict {
		t.Fatalf("error = %v, want %s with the conflict category", err, state.CodeJobExitedNonzero)
	}
	op := h.archived(result.Operation.ID)
	job := stepOf(t, op, "compose.job")
	if op.Status != state.OpFailed || job.Status != state.StepFailed || job.Job == nil || job.Job.ExitCode != 3 {
		t.Errorf("status %v, job step %+v: want a failed step holding exit code 3", op.Status, job)
	}
	if job.Error == nil || job.Error.Code != state.CodeJobExitedNonzero {
		t.Errorf("job problem = %+v", job.Error)
	}
	if h.rt.Running("app") || h.rt.CallCount("apply plan.StartService app") != 0 {
		t.Error("the consumer started although its migration failed")
	}
	if verify := stepOf(t, op, "module.verify"); verify.Status != state.StepNotApplied {
		t.Errorf("the access check after a failed migration is %v, want not-applied", verify.Status)
	}

	// Once the migration is fixed the next up runs it again and then the rollout.
	h.rt.JobExit = nil
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatalf("up after the fix: %v", err)
	}
	if !h.rt.Running("app") || h.rt.CallCount("apply plan.RunJob migrate") != 2 {
		t.Errorf("app running = %v, migrations run = %d", h.rt.Running("app"), h.rt.CallCount("apply plan.RunJob migrate"))
	}
}

func TestJobs_AnEngineFailureIsNotAJobResult(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	h.rt.BeforeApply = func(e plan.Effect) error {
		if _, isJob := e.(plan.RunJob); isJob {
			return errors.New("docker could not start the container")
		}
		return nil
	}
	result, err := h.upBound(testkit.BoundOptions{})
	if err == nil || codeOf(err) == state.CodeJobExitedNonzero {
		t.Fatalf("error = %v, want an engine failure", err)
	}
	job := stepOf(t, h.archived(result.Operation.ID), "compose.job")
	if job.Job != nil || job.Error == nil || job.Error.Code != state.CodeEffectFailed || job.Error.Category != state.CategoryUnexpected {
		t.Errorf("job step = %+v: an engine failure carries no job result and a runtime code", job)
	}
}

func TestJobs_AnOrdinaryRestartDoesNotRunTheMigrationAgain(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.submit(h.boundRequest(testkit.BoundOptions{}, state.CommandStop), selecting("workload/app")); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := h.upBound(testkit.BoundOptions{}, selecting("workload/app")); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !h.rt.Running("app") {
		t.Error("the app did not restart")
	}
	if n := h.rt.CallCount("apply plan.RunJob migrate"); n != 1 {
		t.Errorf("the migration ran %d times, want once", n)
	}
}

func TestJobs_EachExplicitRunIsItsOwnOperationWithItsOwnResultAndRunsEvenWhenUnchanged(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatal(err)
	}
	h.rt.JobExit = map[string]int{"migrate": 0}
	first, err := h.run("migrate")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := h.run("migrate")
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first.Operation.ID == second.Operation.ID {
		t.Fatalf("two invocations share the operation %s", first.Operation.ID)
	}
	for _, res := range []operation.Result{first, second} {
		op := h.archived(res.Operation.ID)
		if op.Command != state.CommandRun || !slices.Equal(steps(op), []string{"compose.job job/migrate"}) {
			t.Errorf("run %s = command %v steps %v: want exactly one job step", op.ID, op.Command, steps(op))
		}
		if job := op.Steps[0]; job.Job == nil || job.Job.ExitCode != 0 || job.Status != state.StepCompleted {
			t.Errorf("run %s job step = %+v", op.ID, job)
		}
	}
	if n := h.rt.CallCount("apply plan.RunJob migrate"); n != 3 {
		t.Errorf("the job ran %d times (one up, two runs), want 3", n)
	}
	if h.rt.CallCount("apply plan.StartService app") != 1 {
		t.Error("a run started a consumer")
	}
}

func TestJobs_ARunOfAFailingJobReportsItsExitCodeNotAnEngineFailure(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	h.rt.JobExit = map[string]int{"migrate": 7}
	result, err := h.run("migrate")
	if codeOf(err) != state.CodeJobExitedNonzero {
		t.Fatalf("error = %v, want %s", err, state.CodeJobExitedNonzero)
	}
	if job := stepOf(t, h.archived(result.Operation.ID), "compose.job"); job.Job == nil || job.Job.ExitCode != 7 {
		t.Errorf("job step = %+v", job)
	}
}

func TestJobs_ARunOnANewEnvironmentBringsUpOnlyWhatTheJobBindsThenRunsIt(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	result, err := h.run("migrate")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := steps(h.archived(result.Operation.ID))
	order := []string{"tls.issue tls/ca", "compose.service service/db--store", "bound-store.user module/db/user/migrator", "compose.job job/migrate"}
	last := -1
	for _, step := range order {
		i := slices.Index(got, step)
		if i <= last {
			t.Errorf("%q is at %d after %d in %v", step, i, last, got)
		}
		last = max(last, i)
	}
	if slices.Contains(got, "compose.service service/app") || h.rt.Running("app") {
		t.Errorf("a run started a consumer: %v", got)
	}
	if n := countPrefix(got, "compose.job"); n != 1 {
		t.Errorf("the run holds %d job steps, want exactly one", n)
	}
}

func TestJobs_ARunRefusesAnythingThatIsNotAJob(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	for _, name := range []string{"app", "missing"} {
		if _, err := h.run(name); codeOf(err) != state.CodeUnknownObject {
			t.Errorf("run %s = %v, want %s", name, err, state.CodeUnknownObject)
		}
	}
	if len(h.rt.Calls) != 0 {
		t.Errorf("a refused run called the runtime: %v", h.rt.Calls)
	}
}

func TestJobs_ARunDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatal(err)
	}
	before, snapshot, generation := h.fileBytes("state.json"), h.rt.Snapshot(), h.state().Generation
	result, err := h.run("migrate", func(r *operation.RunRequest) { r.DryRun = true })
	if err != nil || result.Operation != nil || result.Preview == nil {
		t.Fatalf("dry run = %+v, %v", result, err)
	}
	if rows := result.Preview.Actions; len(rows) != 1 || rows[0].Action != "compose.job" || rows[0].Object != "job/migrate" {
		t.Errorf("preview = %+v, want the one job row", rows)
	}
	if string(h.fileBytes("state.json")) != string(before) || h.state().Generation != generation || h.rt.Snapshot() != snapshot {
		t.Error("a dry run changed state or the runtime")
	}
}

func countPrefix(list []string, prefix string) int {
	n := 0
	for _, s := range list {
		if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
