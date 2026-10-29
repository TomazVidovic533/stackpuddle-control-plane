package state_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func validScope(t testing.TB) state.Scope {
	t.Helper()
	uid := must(t, func() (state.ScopeUID, error) { return state.NewScopeUID(epoch, testkit.NewRandom("scope")) })
	return state.Scope{
		StateVersion: state.StateVersion, ScopeUID: uid, Project: "shop", Environment: "dev", DockerContext: "default",
		EngineID: "engine-1", ComposeProject: "shop-dev-" + uid.Suffix(), KeyID: "0123456789abcdef", CreatedAt: epoch,
	}
}

func newEnv(t *testing.T, opts state.Options) *state.Env {
	t.Helper()
	opts.Random = testkit.NewRandom("env")
	env, err := state.CreateEnv(testkit.Root(t), "shop", "dev", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { env.Close() })
	return env
}

func TestEnv_OpenWithoutCreateWritesNothing(t *testing.T) {
	t.Parallel()
	root := testkit.Root(t)
	_, err := state.OpenEnv(root, "shop", "dev", state.Options{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenEnv on a missing folder: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("OpenEnv created %d entries", len(entries))
	}
}

func TestEnv_RejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "a.b", "../x", "a/b", `a\b`} {
		if _, err := state.EnvironmentDir("/root", name, "dev"); err == nil {
			t.Errorf("EnvironmentDir accepted project %q", name)
		}
	}
}

func TestEnv_CommitAdvancesTheGenerationAndRefusesAStaleWriter(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	if _, ok, err := env.Load(); ok || err != nil {
		t.Fatalf("Load on an empty environment = %v, %v", ok, err)
	}
	base := validState(t, state.ActorOSUser)
	base.Operation = nil
	first, err := env.Commit(0, base)
	if err != nil || first.Generation != 1 {
		t.Fatalf("first commit = %d, %v", first.Generation, err)
	}
	second, err := env.Commit(1, first)
	if err != nil || second.Generation != 2 {
		t.Fatalf("second commit = %d, %v", second.Generation, err)
	}
	if _, err := env.Commit(1, first); !errors.Is(err, state.ErrStaleGeneration) {
		t.Fatalf("stale commit error = %v", err)
	}
	loaded, ok, err := env.Load()
	if err != nil || !ok || loaded.Generation != 2 {
		t.Fatalf("the newer state must be kept: %d, %v, %v", loaded.Generation, ok, err)
	}
}

func TestEnv_ScopeIsWrittenOnce(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	if err := env.WriteScope(validScope(t)); err != nil {
		t.Fatal(err)
	}
	if err := env.WriteScope(validScope(t)); !errors.Is(err, state.ErrScopeExists) {
		t.Fatalf("second WriteScope error = %v", err)
	}
	got, ok, err := env.LoadScope()
	if err != nil || !ok {
		t.Fatalf("LoadScope = %v, %v", ok, err)
	}
	if diff := diff(validScope(t), got); diff != "" {
		t.Errorf("scope changed (-want +got):\n%s", diff)
	}
}

func TestEnv_ArchiveCancelAndDroppedOperation(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	st := validState(t, state.ActorOSUser)
	written, err := env.Commit(0, st)
	if err != nil {
		t.Fatal(err)
	}
	op := *written.Operation
	if ok, err := env.CancelRequested(op.ID); ok || err != nil {
		t.Fatalf("CancelRequested before = %v, %v", ok, err)
	}
	if err := env.WriteCancel(state.CancelRequest{Operation: op.ID, Actor: state.Actor{Kind: state.ActorOSUser}, RequestedAt: epoch}); err != nil {
		t.Fatal(err)
	}
	if ok, err := env.CancelRequested(op.ID); !ok || err != nil {
		t.Fatalf("CancelRequested after = %v, %v", ok, err)
	}
	if err := env.Archive(st.ScopeUID, op); err != nil {
		t.Fatal(err)
	}
	if back, ok, err := env.LoadOperation(op.ID); err != nil || !ok || back.ID != op.ID {
		t.Fatalf("LoadOperation = %v, %v, %v", back.ID, ok, err)
	}
	// The archive exists but state.json still lists the operation: Load finishes the archive.
	loaded, _, err := env.Load()
	if err != nil || loaded.Operation != nil {
		t.Fatalf("Load must drop an archived open operation: %v, %v", loaded.Operation, err)
	}
}

func TestEnv_AFaultBetweenTempWriteAndRenameLeavesTheOldStateReadable(t *testing.T) {
	t.Parallel()
	fault := errors.New("injected fault")
	armed := false
	env := newEnv(t, state.Options{AfterStage: func(s durable.Stage) error {
		if armed && s == durable.TempSynced {
			return fault
		}
		return nil
	}})
	base := validState(t, state.ActorOSUser)
	base.Operation = nil
	old, err := env.Commit(0, base)
	if err != nil {
		t.Fatal(err)
	}
	armed = true
	next := old
	next.Accepted = nil
	if _, err := env.Commit(old.Generation, next); !errors.Is(err, fault) {
		t.Fatalf("faulted commit error = %v", err)
	}
	loaded, ok, err := env.Load()
	if err != nil || !ok || loaded.Generation != old.Generation || loaded.Accepted == nil {
		t.Fatalf("old state must stay readable: gen %d accepted=%v ok=%v err=%v", loaded.Generation, loaded.Accepted != nil, ok, err)
	}
	entries, err := os.ReadDir(env.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 5 && e.Name()[:5] == ".stat" {
			t.Errorf("temporary file %s was left behind", e.Name())
		}
	}
}

func TestEnv_RefusesUnreadableFilesAndLeavesTheirBytes(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	path := filepath.Join(env.Dir(), "state.json")
	good := marshal(t, validState(t, state.ActorOSUser))
	for name, data := range map[string][]byte{
		"newer version": mutate(t, good, func(d map[string]any) { d["stateVersion"] = 2 }),
		"unknown field": mutate(t, good, func(d map[string]any) { d["new"] = 1 }),
		"unknown enum":  mutate(t, good, func(d map[string]any) { d["operation"].(map[string]any)["status"] = "x" }),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := env.Load(); !errors.Is(err, state.ErrUnsupported) {
			t.Fatalf("%s: Load error = %v", name, err)
		}
		if _, err := env.Commit(3, validState(t, state.ActorOSUser)); !errors.Is(err, state.ErrUnsupported) {
			t.Fatalf("%s: Commit over an unreadable file must refuse, got %v", name, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(data) {
			t.Errorf("%s: the bytes changed", name)
		}
	}
}
