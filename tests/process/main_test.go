package process_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func TestMain(m *testing.M) {
	testkit.Dispatch(map[string]testkit.Helper{
		"commit":           helperCommit,
		"hold":             helperHold,
		"parent-and-child": helperParentAndChild,
		"umask":            helperUmask,
	})
	os.Exit(m.Run())
}

func emptyState(uid state.ScopeUID) state.State {
	return state.State{StateVersion: state.StateVersion, ScopeUID: uid}
}

func fixedUID() state.ScopeUID {
	uid, err := state.NewScopeUID(testkit.Epoch, testkit.NewRandom("process"))
	if err != nil {
		panic(err)
	}
	return uid
}

// helperCommit commits generation expected+1 and dies or fails at the named write stage.
// Arguments: state root, expected generation, stage name, mode ("kill" or "fail").
func helperCommit(args []string) int {
	root, stageName, mode := args[0], args[2], args[3]
	expected, _ := strconv.ParseInt(args[1], 10, 64)
	hook := func(s durable.Stage) error {
		if s.String() != stageName {
			return nil
		}
		if mode == "kill" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			select {}
		}
		return fmt.Errorf("injected fault at %s", s)
	}
	env, err := state.CreateEnv(root, "shop", "dev", state.Options{AfterStage: hook, Random: testkit.NewRandom("helper")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	next := emptyState(fixedUID())
	next.Accepted = &state.Accepted{Revision: 9, Model: []byte(`{"kind":"Model"}`), Topology: []byte(`{"workloads":[]}`)}
	if _, err := env.Commit(expected, next); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	return 0
}

// helperHold takes the lock, records an owner, announces "ready" and holds for the given
// milliseconds (0: until killed). Arguments: lock path, operation ID, hold milliseconds.
func helperHold(args []string) int {
	held, err := lock.Acquire(context.Background(), args[0], lock.AcquireOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := held.SetOwner(lock.Owner{Operation: args[1], PID: os.Getpid()}); err != nil {
		return 1
	}
	fmt.Println("ready")
	millis, _ := strconv.Atoi(args[2])
	if millis == 0 {
		select {}
	}
	time.Sleep(time.Duration(millis) * time.Millisecond)
	_ = held.Release()
	return 0
}

// helperParentAndChild takes the lock and starts a child that inherits it, then announces both PIDs
// and waits to be killed. Arguments: lock path, operation ID.
func helperParentAndChild(args []string) int {
	held, err := lock.Acquire(context.Background(), args[0], lock.AcquireOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	child := exec.Command("sleep", "120")
	child.ExtraFiles = []*os.File{held.File()}
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := held.SetOwner(lock.Owner{Operation: args[1], PID: os.Getpid(), ChildPID: child.Process.Pid}); err != nil {
		return 1
	}
	fmt.Printf("ready %d\n", child.Process.Pid)
	select {}
}

// helperUmask creates an environment, a state, a scope and the lock file under umask 000.
func helperUmask(args []string) int {
	syscall.Umask(0)
	env, err := state.CreateEnv(args[0], "shop", "dev", state.Options{Random: testkit.NewRandom("umask")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	uid := fixedUID()
	if err := env.WriteScope(state.Scope{ScopeUID: uid, Project: "shop", Environment: "dev", DockerContext: "default", EngineID: "e", ComposeProject: "p", KeyID: "k", CreatedAt: testkit.Epoch}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := env.Commit(0, emptyState(uid)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	held, err := lock.Acquire(context.Background(), env.LockPath(), lock.AcquireOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return exitCode(held.Release())
}

func exitCode(err error) int {
	if err != nil {
		return 1
	}
	return 0
}

// start launches a helper and waits for its first stdout line.
func start(t *testing.T, name string, args ...string) (cmd *exec.Cmd, firstLine string) {
	t.Helper()
	cmd = testkit.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("helper %s did not announce itself: %v", name, err)
	}
	return cmd, line
}
