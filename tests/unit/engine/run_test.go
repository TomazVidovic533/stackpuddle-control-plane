package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func (h *harness) runRequest(job string, o testkit.ShopOptions) engine.RunRequest {
	h.t.Helper()
	ref, err := model.ParseObjectRef("workload/" + job)
	if err != nil {
		h.t.Fatal(err)
	}
	return engine.RunRequest{Scope: scope(h.t), Project: testkit.Shop(h.t, o), ProjectDir: h.root, Job: ref}
}

func TestRun_RefusesAReferenceThatIsNotAJobBeforeAnyDockerCall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "k")
	for _, job := range []string{"api", "nothing"} {
		_, err := h.eng.Run(t.Context(), h.runRequest(job, testkit.ShopOptions{Instances: []string{"primary"}}))
		if code, category := codeOf(t, err); code != engine.CodeUnknownObject || category != engine.CategoryValidation {
			t.Errorf("run %s = %v / %v, want unknown_object / validation", job, code, category)
		}
	}
	h.wantNoSideEffects()
}

func TestRun_ADryRunPlansTheOneJobTakesNoLockAndWritesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	req := h.runRequest("migrate", testkit.ShopOptions{Instances: []string{"primary"}})
	req.DryRun = true
	result, err := h.eng.Run(t.Context(), req)
	if err != nil {
		t.Fatalf("a dry run needs neither a key nor Docker: %v", err)
	}
	var jobs int
	for _, action := range result.Preview().Actions() {
		if action.Action == "compose.job" && action.Object == "job/migrate" && action.Effect == engine.EffectApply {
			jobs++
		}
		if action.Object == "service/api" {
			t.Errorf("a run previews the consumer: %+v", action)
		}
	}
	if jobs != 1 {
		t.Errorf("the preview holds %d job rows, want 1: %+v", jobs, result.Preview().Actions())
	}
	if _, ok := result.Operation(); ok {
		t.Error("a dry run returned an operation")
	}
	if _, err := os.Stat(filepath.Join(h.root, "environments")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dry run created a state folder: %v", err)
	}
	h.wantNoSideEffects()
}

func TestStep_AJobResultIsAbsentUntilTheJobHasExited(t *testing.T) {
	t.Parallel()
	if result, ok := (engine.Step{}).Job(); ok || result != (engine.JobResult{}) {
		t.Errorf("a step with no job = %+v, %v", result, ok)
	}
	if engine.CommandRun.String() != "run" || engine.CodeJobExitedNonzero != "job.exited_nonzero" {
		t.Errorf("run command %q, code %q", engine.CommandRun, engine.CodeJobExitedNonzero)
	}
}
