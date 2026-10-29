//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func TestEmptyStateBesideTheScopesLabelledObjectsBlocksAsUnmapped(t *testing.T) {
	s := newStack(t)
	s.mustUp(small)
	before := s.containers()
	if err := os.Remove(filepath.Join(s.envDir(), "state.json")); err != nil {
		t.Fatal(err)
	}
	result, err := s.up(small)
	if code := codeOf(t, err); code != engine.CodeUnmappedOwnedObject {
		t.Fatalf("error = %v (%v), want unmapped_owned_object", code, err)
	}
	op, ok := result.Operation()
	if !ok || op.Status() != engine.StatusBlocked {
		t.Fatalf("operation = %+v", op)
	}
	if after := s.containers(); !slices.Equal(after, before) {
		t.Errorf("a blocked operation changed the containers: %v -> %v", before, after)
	}
}

func TestAVanishedMappedVolumeShowsLostAndIsNotRecreatedEmpty(t *testing.T) {
	s := newStack(t)
	s.mustUp(small)
	volumes := s.volumes()
	if len(volumes) != 1 {
		t.Fatalf("volumes = %v", volumes)
	}
	for _, c := range s.containers() {
		docker(t, "rm", "-f", c)
	}
	docker(t, "volume", "rm", "-f", volumes[0])

	result, err := s.up(small)
	if code := codeOf(t, err); code != engine.CodeLostObject {
		t.Fatalf("error = %v (%v), want lost_object", code, err)
	}
	if op, _ := result.Operation(); op.Status() != engine.StatusBlocked {
		t.Errorf("status = %v", op.Status())
	}
	if got := s.volumes(); len(got) != 0 {
		t.Errorf("the lost volume was recreated: %v", got)
	}
	inspection, _ := s.eng.Inspect(t.Context(), engine.InspectEnvironment(s.scope()))
	view, _ := inspection.Environment()
	var state string
	for _, o := range view.Objects() {
		if o.Ref == "volume/api--uploads" {
			state = o.State
		}
	}
	if state != "lost" {
		t.Errorf("the volume shows as %q, want lost", state)
	}
}

func TestAStoppedServiceIsNotRestartedByALaterUpWithoutTheSelection(t *testing.T) {
	s := newStack(t)
	both := testkit.ShopOptions{Instances: []string{"primary"}, NoJob: true}
	s.mustUp(both)
	stop := s.request(both, engine.CommandStop)
	stop.Selection = selectRefs(t, "workload/api")
	if _, err := s.submit(stop); err != nil {
		t.Fatalf("stop: %v", err)
	}
	api := s.scopeFile().ComposeProject + "-api-1"
	if inspectContainer(t, api).State.Running {
		t.Fatal("stop left the api running")
	}
	edited := both
	edited.APIEnv = map[string]string{"EDIT": "1"}
	if _, err := s.up(edited, func(r *engine.SubmitRequest) { r.Selection = selectRefs(t, "module/primary") }); err != nil {
		t.Fatalf("up of the store: %v", err)
	}
	if inspectContainer(t, api).State.Running {
		t.Error("a later up without the selection restarted the stopped api")
	}
	if _, err := s.up(edited, func(r *engine.SubmitRequest) { r.Selection = selectRefs(t, "workload/api") }); err != nil {
		t.Fatalf("up of the api: %v", err)
	}
	if !inspectContainer(t, api).State.Running {
		t.Error("selecting the stopped api did not start it")
	}
}

func selectRefs(t *testing.T, refs ...string) engine.Selection {
	t.Helper()
	var parsed []model.ObjectRef
	for _, text := range refs {
		ref, err := model.ParseObjectRef(text)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, ref)
	}
	return engine.SelectRefs(parsed...)
}

func TestUnselectedSourceEditsStayUnappliedThroughAKillAndRerun(t *testing.T) {
	s := newStack(t)
	s.mustUp(small)
	edited := small
	edited.APICommand = []string{"sleep", "7200"}
	edited.ExtraWorkload = true
	s.helperCommand(edited, killAt("after", "plan.StartService", "api"), "workload/api").killed(t)

	if _, err := s.up(edited, func(r *engine.SubmitRequest) { r.Selection = selectRefs(t, "workload/api") }); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	for _, name := range s.containers() {
		if filepath.Base(name) == s.scopeFile().ComposeProject+"-worker-1" {
			t.Errorf("the unselected worker was started: %s", name)
		}
	}
	api := inspectContainer(t, s.scopeFile().ComposeProject+"-api-1")
	if !api.State.Running {
		t.Error("the selected api is not running")
	}
	inspection, _ := s.eng.Inspect(t.Context(), engine.InspectEnvironment(s.scope()))
	view, _ := inspection.Environment()
	for _, o := range view.Objects() {
		if o.Ref == "service/worker" {
			t.Error("the unselected worker is mapped in state")
		}
	}
}
