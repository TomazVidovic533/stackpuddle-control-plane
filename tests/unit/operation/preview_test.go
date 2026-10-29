package operation_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func (h *harness) dryRun(o testkit.ShopOptions, command state.Command, change ...func(*operation.Request)) operation.Preview {
	h.t.Helper()
	req := h.request(o, command)
	req.DryRun = true
	result, err := h.submit(req, change...)
	if err != nil || result.Preview == nil {
		h.t.Fatalf("dry run: %+v, %v", result.Preview, err)
	}
	return *result.Preview
}

func actionFor(t *testing.T, p operation.Preview, object string) operation.Action {
	t.Helper()
	for _, a := range p.Actions {
		if a.Object == object {
			return a
		}
	}
	t.Fatalf("the preview has no row for %s: %+v", object, p.Actions)
	return operation.Action{}
}

func TestPreview_RowsCarryTheirImpactAndAKnownOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	edited := testkit.ShopOptions{Instances: []string{"primary"}, APICommand: []string{"sleep", "7200"}}
	if row := actionFor(t, h.dryRun(edited, state.CommandUp), "service/api"); row.Impact != plan.ImpactApply || row.Unknown || row.Problem != "" {
		t.Errorf("up row = %+v", row)
	}
	if row := actionFor(t, h.dryRun(opts, state.CommandStop, selecting("workload/api")), "service/api"); row.Impact != plan.ImpactStop {
		t.Errorf("stop row = %+v", row)
	}
	if row := actionFor(t, h.dryRun(opts, state.CommandDestroy), "service/api"); row.Impact != plan.ImpactDelete {
		t.Errorf("destroy row = %+v", row)
	}
}

func TestPreview_ABlockingFindingIsABlockedRowNotAnApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if _, err := h.up(opts); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.root, "environments", "shop.local", "state.json")); err != nil {
		t.Fatal(err)
	}
	var blocked int
	for _, row := range h.dryRun(opts, state.CommandUp).Actions {
		if row.Impact == plan.ImpactBlocked {
			blocked++
			if row.Problem != state.CodeUnmappedOwnedObject || row.Action != "" || row.Unknown {
				t.Errorf("blocked row = %+v", row)
			}
		}
	}
	if blocked == 0 {
		t.Error("the unmapped objects were not reported as blocked rows")
	}
}

func TestPreview_AStepAnOpenOperationStartedAndADriverStepAreUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, driverCatalog())
	crashOnce(h, true, isVolume("primary--store--data"))
	if _, err := h.up(opts); err == nil {
		t.Fatal("the crashed run succeeded")
	}
	preview := h.dryRun(opts, state.CommandUp)
	if row := actionFor(t, preview, "volume/primary--store--data"); !row.Unknown {
		t.Errorf("a step the open operation started must be unknown: %+v", row)
	}
	var drivers int
	for _, row := range preview.Actions {
		if row.Action != "" && row.Impact == plan.ImpactApply && len(row.Object) > 7 && row.Object[:7] == "module/" {
			drivers++
			if !row.Unknown {
				t.Errorf("a driver step must be unknown: %+v", row)
			}
		}
	}
	if drivers == 0 {
		t.Error("the preview holds no driver step to check")
	}
}
