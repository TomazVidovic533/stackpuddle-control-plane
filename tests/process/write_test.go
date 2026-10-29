package process_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func seeded(t *testing.T) (root string, env *state.Env) {
	t.Helper()
	root = testkit.Root(t)
	env, err := state.CreateEnv(root, "shop", "dev", state.Options{Random: testkit.NewRandom("seed")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	if _, err := env.Commit(0, emptyState(fixedUID())); err != nil {
		t.Fatal(err)
	}
	return root, env
}

// TestAHelperKilledAtEachWritePointLeavesTheOldOrTheNewGeneration kills a real process with SIGKILL
// at every stage of the atomic write. The file is never torn: before the rename it is the old
// generation, from the rename on it is the new one.
func TestAHelperKilledAtEachWritePointLeavesTheOldOrTheNewGeneration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		stage         durable.Stage
		wantGen       int64
		wantAccepted  bool
		stageNameText string
	}{
		{durable.TempWritten, 1, false, "temp-written"},
		{durable.TempSynced, 1, false, "temp-synced"},
		{durable.Renamed, 2, true, "renamed"},
		{durable.DirSynced, 2, true, "dir-synced"},
	} {
		t.Run(test.stageNameText, func(t *testing.T) {
			t.Parallel()
			root, env := seeded(t)
			cmd := testkit.Command("commit", root, "1", test.stageNameText, "kill")
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != -1 {
				t.Fatalf("the helper was not killed by a signal: %v", err)
			}
			loaded, ok, err := env.Load()
			if err != nil || !ok {
				t.Fatalf("state.json is not readable after SIGKILL at %s: %v", test.stageNameText, err)
			}
			if loaded.Generation != test.wantGen || (loaded.Accepted != nil) != test.wantAccepted {
				t.Errorf("after SIGKILL at %s: generation %d accepted=%v, want %d accepted=%v",
					test.stageNameText, loaded.Generation, loaded.Accepted != nil, test.wantGen, test.wantAccepted)
			}
		})
	}
}

func TestAFaultBetweenTempWriteAndRenameLeavesTheOldStateReadable(t *testing.T) {
	t.Parallel()
	root, env := seeded(t)
	err := testkit.Command("commit", root, "1", "temp-synced", "fail").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the helper must report the injected fault with exit 3: %v", err)
	}
	loaded, ok, err := env.Load()
	if err != nil || !ok || loaded.Generation != 1 || loaded.Accepted != nil {
		t.Fatalf("the old state is not intact: %+v %v %v", loaded, ok, err)
	}
	entries, _ := os.ReadDir(env.Dir())
	for _, e := range entries {
		if e.Name() != "state.json" && e.Name() != "operations" && e.Name() != "cancel" {
			t.Errorf("unexpected leftover %q after a faulted write", e.Name())
		}
	}
}

func TestAReplacedLockFileLetsASecondWriterAdvanceAndRefusesTheFirst(t *testing.T) {
	t.Parallel()
	root, first := seeded(t)
	// The first writer holds the lock on the original env.flock inode.
	staleHold := lockOf(t, first)

	// The file is replaced: removed and created again, so a second writer locks a different inode.
	if err := os.Remove(first.LockPath()); err != nil {
		t.Fatal(err)
	}
	second, err := state.OpenEnv(root, "shop", "dev", state.Options{Random: testkit.NewRandom("second")})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondHold := lockOf(t, second)
	defer secondHold.Release()
	loaded, _, err := second.Load()
	if err != nil {
		t.Fatal(err)
	}
	advanced, err := second.Commit(loaded.Generation, loaded)
	if err != nil {
		t.Fatalf("the second writer could not advance: %v", err)
	}

	// The first writer still believes it is alone, but its commit is fenced by the generation.
	if _, err := first.Commit(loaded.Generation, loaded); !errors.Is(err, state.ErrStaleGeneration) {
		t.Fatalf("the first writer's commit error = %v, want ErrStaleGeneration", err)
	}
	_ = staleHold.Release()
	after, _, err := first.Load()
	if err != nil || after.Generation != advanced.Generation {
		t.Fatalf("the newer state must be kept: generation %d, %v", after.Generation, err)
	}
}

func TestStateFilesArePrivateUnderUmask000AndUnreadableToAnotherUser(t *testing.T) {
	root := testkit.Root(t)
	if err := testkit.Command("umask", root).Run(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	env := filepath.Join(root, "environments", "shop.dev")
	for path, want := range map[string]os.FileMode{
		root: 0o700, filepath.Join(root, "environments"): 0o700, env: 0o700,
		filepath.Join(env, "operations"): 0o700, filepath.Join(env, "cancel"): 0o700,
		filepath.Join(env, "scope.json"): 0o600, filepath.Join(env, "state.json"): 0o600, filepath.Join(env, "env.flock"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			continue
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
		}
	}
	probeUnprivileged(t, env)
}

// probeUnprivileged opens files of the environment folder as an unprivileged uid, relative to a
// folder descriptor that root opened, so the probe does not depend on whether the scratch root's own
// parents are reachable. The build host runs as root, which ignores mode bits; when the test cannot
// switch uid it reports INCOMPLETE by name.
func probeUnprivileged(t *testing.T, envDir string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("INCOMPLETE: the unprivileged-user probe needs the test to start as root so it can switch uid; the mode bits were checked above")
	}
	dir, err := os.Open(envDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	control := filepath.Join(envDir, "control.txt")
	if err := os.WriteFile(control, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cat opens the file through /proc/self/fd/3, the inherited folder descriptor, so only the
	// modes of the environment folder and the file decide the outcome.
	readAs := func(name string) int {
		var stderr bytes.Buffer
		cmd := exec.Command("cat", "/proc/self/fd/3/"+name)
		cmd.ExtraFiles = []*os.File{dir}
		cmd.Stderr = &stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: nobody, Gid: nobody}}
		err := cmd.Run()
		switch {
		case err == nil:
			return 1
		case strings.Contains(stderr.String(), "Permission denied"):
			return 0
		}
		t.Logf("probe of %s: %v: %s", name, err, stderr.String())
		return -1
	}
	if got := readAs("state.json"); got == -1 {
		t.Skip("INCOMPLETE: this host does not let the test switch to an unprivileged uid")
	} else if got != 0 {
		t.Errorf("an unprivileged user reaching state.json through the 0700 folder got exit %d (0 means denied)", got)
	}
	// Open the folder up and the files must still deny: 0600 is its own protection. A world-readable
	// control file proves the probe can succeed at all.
	if err := os.Chmod(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(envDir, 0o700)
	for _, name := range []string{"state.json", "scope.json", "env.flock"} {
		if got := readAs(name); got != 0 {
			t.Errorf("an unprivileged user reading %s from a readable folder got exit %d (0 means denied)", name, got)
		}
	}
	if got := readAs("control.txt"); got != 1 {
		t.Errorf("the control file was not readable (exit %d): the probe proves nothing", got)
	}
}

// nobody is the unprivileged uid and gid the probe runs as.
const nobody = 65534
