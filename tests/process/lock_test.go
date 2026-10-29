package process_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

const holderOp = "op-01J9Z8K3M4N5P6Q7R8S9T0V1W2"

func lockOf(t *testing.T, env *state.Env) *lock.Held {
	t.Helper()
	held, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return held
}

func TestASecondWriterGetsBusyNamingTheHolder(t *testing.T) {
	t.Parallel()
	_, env := seeded(t)
	holder, _ := start(t, "hold", env.LockPath(), holderOp, "0")

	_, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{})
	if !errors.Is(err, lock.ErrBusy) {
		t.Fatalf("second Acquire error = %v, want ErrBusy", err)
	}
	if !strings.Contains(err.Error(), holderOp) || !strings.Contains(err.Error(), "process "+itoa(holder.Process.Pid)) {
		t.Errorf("the message %q does not name the holder's operation and process", err)
	}
}

func itoa(n int) string {
	var digits []byte
	for ; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

func TestAWriterWithADeadlineWaitsThenProceeds(t *testing.T) {
	t.Parallel()
	_, env := seeded(t)
	start(t, "hold", env.LockPath(), holderOp, "600")

	began := time.Now()
	held, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{Wait: 10 * time.Second, Random: testkit.NewRandom("wait")})
	if err != nil {
		t.Fatalf("a waiting writer must proceed once the holder releases: %v", err)
	}
	defer held.Release()
	if waited := time.Since(began); waited < 400*time.Millisecond {
		t.Errorf("proceeded after %v, before the holder released", waited)
	}
}

func TestAWaitingWriterGivesUpAtTheDeadlineAndStopsOnCancel(t *testing.T) {
	t.Parallel()
	_, env := seeded(t)
	start(t, "hold", env.LockPath(), holderOp, "0")

	began := time.Now()
	_, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{Wait: 300 * time.Millisecond, Random: testkit.NewRandom("deadline")})
	if !errors.Is(err, lock.ErrBusy) {
		t.Fatalf("error at the deadline = %v", err)
	}
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("waited %v for a 300ms deadline", elapsed)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	began = time.Now()
	_, err = lock.Acquire(ctx, env.LockPath(), lock.AcquireOptions{Wait: time.Minute, Random: testkit.NewRandom("cancel")})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cancelled wait error = %v", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Error("a cancelled wait kept waiting")
	}
}

func TestASIGKILLedHolderFreesTheLockWithNoRecoveryCode(t *testing.T) {
	t.Parallel()
	_, env := seeded(t)
	holder, _ := start(t, "hold", env.LockPath(), holderOp, "0")
	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	held, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{})
	if err != nil {
		t.Fatalf("the lock of a killed holder was not freed: %v", err)
	}
	defer held.Release()
}

func TestInspectReadsWhileTheEnvironmentIsHeld(t *testing.T) {
	t.Parallel()
	root, env := seeded(t)
	start(t, "hold", env.LockPath(), holderOp, "0")

	eng, err := engine.New(engine.Options{LookupEnv: testkit.LookupEnv(map[string]string{"STACKPUDDLE_STATE_DIR": root, "HOME": root})})
	if err != nil {
		t.Fatal(err)
	}
	project, environment := parse(t)
	inspection, err := eng.Inspect(t.Context(), engine.InspectEnvironment(engine.Scope{Project: project, Environment: environment}))
	if err != nil {
		t.Fatalf("Inspect while held: %v", err)
	}
	view, ok := inspection.Environment()
	if !ok || view.Generation() != 1 {
		t.Errorf("view = generation %d, ok %v", view.Generation(), ok)
	}
}

func TestALiveChildKeepsTheEnvironmentBusyAfterItsParentDies(t *testing.T) {
	t.Parallel()
	_, env := seeded(t)
	parent, line := start(t, "parent-and-child", env.LockPath(), holderOp)
	childPID := strings.TrimSpace(strings.TrimPrefix(line, "ready "))
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()

	_, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{})
	if !errors.Is(err, lock.ErrBusy) {
		t.Fatalf("with the parent dead and the child alive: %v, want ErrBusy", err)
	}
	if !strings.Contains(err.Error(), "effect process "+childPID) {
		t.Errorf("the message %q does not name the effect process %s", err, childPID)
	}

	pid := 0
	for _, c := range childPID {
		pid = pid*10 + int(c-'0')
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		held, err := lock.Acquire(t.Context(), env.LockPath(), lock.AcquireOptions{})
		if err == nil {
			held.Release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lock was not freed after the child died: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTheLockFileIsNeverUnlinked(t *testing.T) {
	t.Parallel()
	_, env := seeded(t)
	held := lockOf(t, env)
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env.LockPath()); err != nil {
		t.Fatalf("Release removed env.flock: %v", err)
	}
	holder, _ := start(t, "hold", env.LockPath(), holderOp, "0")
	_ = holder.Process.Kill()
	_ = holder.Wait()
	if _, err := os.Stat(env.LockPath()); err != nil {
		t.Fatalf("a killed holder left no env.flock: %v", err)
	}
}
