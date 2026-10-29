//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

var small = testkit.ShopOptions{Instances: []string{}, NoJob: true}

func (s *stack) openOperation() engine.Operation {
	s.t.Helper()
	inspection, err := s.eng.Inspect(s.t.Context(), engine.InspectEnvironment(s.scope()))
	if err != nil {
		s.t.Fatal(err)
	}
	view, _ := inspection.Environment()
	op, ok := view.OpenOperation()
	if !ok {
		s.t.Fatal("no operation was left open")
	}
	return op
}

func (s *stack) inspectOperation(id engine.OperationID) engine.Operation {
	s.t.Helper()
	inspection, err := s.eng.Inspect(s.t.Context(), engine.InspectOperation(s.scope(), id))
	if err != nil {
		s.t.Fatal(err)
	}
	op, _ := inspection.Operation()
	return op
}

func stepStatus(t *testing.T, op engine.Operation, object string) engine.StepStatus {
	t.Helper()
	for _, step := range op.Steps() {
		if step.Object == object {
			return step.Status
		}
	}
	t.Fatalf("operation %s has no step for %s", op.ID(), object)
	return 0
}

func TestAKillAfterTheVolumeStepStartedIsObservedOnTheRerunWithNoDuplicate(t *testing.T) {
	s := newStack(t)
	s.helperCommand(small, killAt("after", "plan.CreateVolume", "api--uploads")).killed(t)

	open := s.openOperation()
	if open.Status() != engine.StatusRunning || stepStatus(t, open, "volume/api--uploads") != engine.StepStarted {
		t.Fatalf("the journal does not show the open started step: %v / %v", open.Status(), stepStatus(t, open, "volume/api--uploads"))
	}
	if got := s.volumes(); len(got) != 1 {
		t.Fatalf("volumes after the kill = %v, want the one the dead process created", got)
	}

	op := s.mustUp(small)
	old := s.inspectOperation(open.ID())
	if old.Status() != engine.StatusInterrupted {
		t.Errorf("old operation = %v", old.Status())
	}
	if next, ok := old.ContinuedBy(); !ok || next != op.ID() {
		t.Errorf("old operation is continued by %v, want %v", next, op.ID())
	}
	if prev, ok := op.Continues(); !ok || prev != open.ID() {
		t.Errorf("new operation continues %v, want %v", prev, open.ID())
	}
	if got := stepStatus(t, old, "volume/api--uploads"); got != engine.StepCompletedByObservation {
		t.Errorf("settled step = %v, want completed-by-observation", got)
	}
	if got := s.volumes(); len(got) != 1 {
		t.Errorf("volumes after the rerun = %v, want no duplicate", got)
	}
	if got := s.containers(); len(got) != 1 || !inspectContainer(t, got[0]).State.Running {
		t.Errorf("containers = %v", got)
	}
}

func TestAKillBeforeTheEffectIsNotAppliedAndTheRerunDoesItOnce(t *testing.T) {
	s := newStack(t)
	s.helperCommand(small, killAt("before", "plan.CreateVolume", "api--uploads")).killed(t)
	open := s.openOperation()
	if got := s.volumes(); len(got) != 0 {
		t.Fatalf("a volume exists although the effect never ran: %v", got)
	}
	s.mustUp(small)
	if got := stepStatus(t, s.inspectOperation(open.ID()), "volume/api--uploads"); got != engine.StepNotApplied {
		t.Errorf("settled step = %v, want not-applied", got)
	}
	if got := s.volumes(); len(got) != 1 {
		t.Errorf("volumes = %v", got)
	}
}

func TestAKillWithACancelFileMakesTheOperationCancelledAndItIsNotContinued(t *testing.T) {
	s := newStack(t)
	s.helperCommand(small, killAt("after", "plan.CreateVolume", "api--uploads")).killed(t)
	open := s.openOperation()
	if err := s.eng.Cancel(t.Context(), engine.CancelRequest{Scope: s.scope(), Operation: open.ID()}); err != nil {
		t.Fatal(err)
	}
	op := s.mustUp(small)
	old := s.inspectOperation(open.ID())
	if old.Status() != engine.StatusCancelled {
		t.Errorf("old operation = %v, want cancelled", old.Status())
	}
	if _, ok := old.ContinuedBy(); ok {
		t.Error("a cancelled operation was continued")
	}
	if _, ok := op.Continues(); ok {
		t.Error("the new operation continues a cancelled one")
	}
}

// startSlowJob runs a helper whose migrate job takes a few seconds, in its own process group, and
// waits until the job's container is running.
func startSlowJob(t *testing.T, s *stack) *exec.Cmd {
	t.Helper()
	slow := testkit.ShopOptions{Instances: []string{}, JobCommand: []string{"sleep", "8"}}
	cmd := s.helperCommand(slow, noKill()).cmd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(s.envDir(), "scope.json")); err == nil && len(data) > 0 {
			uid := s.scopeFile().ScopeUID.String()
			out, _ := dockerErr("ps", "-q", "--filter", "label=io.stackpuddle.scope="+uid, "--filter", "label=com.docker.compose.service=migrate", "--filter", "status=running")
			if out != "" {
				return cmd
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the job never started")
	return nil
}

func waitJobExit(t *testing.T, s *stack) containerInfo {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		uid := s.scopeFile().ScopeUID.String()
		out, _ := dockerErr("ps", "-a", "--filter", "label=io.stackpuddle.scope="+uid, "--filter", "label=com.docker.compose.service=migrate", "--filter", "status=exited", "--format", "{{.Names}}")
		if out != "" {
			return inspectContainer(t, strings.Fields(out)[0])
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the job never finished")
	return containerInfo{}
}

func TestAGroupSignalToTheParentDoesNotKillAnEffectMidWay(t *testing.T) {
	s := newStack(t)
	cmd := startSlowJob(t, s)
	// A terminal's Ctrl-C reaches every process of the foreground group: the helper dies, and the
	// effect, which runs in its own group, must not.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	job := waitJobExit(t, s)
	if job.State.ExitCode != 0 {
		t.Fatalf("the job was cut short: exit code %d", job.State.ExitCode)
	}
	// The next run settles the dead operation by observation and finishes the work.
	slow := testkit.ShopOptions{Instances: []string{}, JobCommand: []string{"sleep", "8"}}
	open := s.openOperation()
	s.mustUp(slow)
	if got := stepStatus(t, s.inspectOperation(open.ID()), "job/migrate"); got != engine.StepCompletedByObservation {
		t.Errorf("job step settled as %v, want completed-by-observation", got)
	}
}

func TestALiveEffectKeepsTheEnvironmentBusyAfterItsParentDiesAndTheMessageNamesIt(t *testing.T) {
	s := newStack(t)
	cmd := startSlowJob(t, s)
	if err := cmd.Process.Kill(); err != nil { // SIGKILL: the parent gets no chance to clean up
		t.Fatal(err)
	}
	_ = cmd.Wait()

	slow := testkit.ShopOptions{Instances: []string{}, JobCommand: []string{"sleep", "8"}}
	_, err := s.up(slow)
	if code := codeOf(t, err); code != engine.CodeEnvironmentBusy {
		t.Fatalf("error = %v (%v), want environment_busy", code, err)
	}
	if !strings.Contains(err.Error(), "effect process") || !strings.Contains(err.Error(), "op-") {
		t.Errorf("the message %q does not name the operation and the effect process", err)
	}
	waitJobExit(t, s)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := s.up(slow)
		if err == nil {
			return
		}
		if codeOf(t, err) != engine.CodeEnvironmentBusy || time.Now().After(deadline) {
			t.Fatalf("the environment stayed busy or failed: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
