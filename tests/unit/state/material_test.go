package state_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

func TestMaterial_WrittenThroughTheEnvironmentRootAndReadBack(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	if _, ok, err := env.ReadMaterial("ca.crt"); ok || err != nil {
		t.Fatalf("before any write: ok=%v err=%v, want absent", ok, err)
	}
	want := []byte("-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----\n")
	if err := env.WriteMaterial("ca.crt", want); err != nil {
		t.Fatalf("WriteMaterial: %v", err)
	}
	got, ok, err := env.ReadMaterial("ca.crt")
	if err != nil || !ok || !bytes.Equal(got, want) {
		t.Fatalf("ReadMaterial = %q, %v, %v", got, ok, err)
	}
	if err := env.WriteMaterial("ca.crt", []byte("second")); err != nil {
		t.Fatalf("a second write replaces the file: %v", err)
	}
	if got, _, _ := env.ReadMaterial("ca.crt"); string(got) != "second" {
		t.Errorf("after replace: %q", got)
	}
	dir, err := os.Stat(filepath.Join(env.Dir(), "tls"))
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("tls folder = %v, %v: want mode 0700", dir, err)
	}
	entries, _ := os.ReadDir(filepath.Join(env.Dir(), "tls"))
	if len(entries) != 1 {
		t.Errorf("the tls folder holds %d entries, want only ca.crt (no temporary file left)", len(entries))
	}
}

func TestMaterial_ANameThatCouldLeaveTheFolderIsRefused(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	for _, name := range []string{"", ".", "..", "../state.json", "a/b", `a\b`} {
		if err := env.WriteMaterial(name, []byte("x")); !errors.Is(err, state.ErrInvalidMaterialName) {
			t.Errorf("WriteMaterial(%q) = %v", name, err)
		}
		if _, _, err := env.ReadMaterial(name); !errors.Is(err, state.ErrInvalidMaterialName) {
			t.Errorf("ReadMaterial(%q) = %v", name, err)
		}
	}
}

func TestMaterial_ASymlinkOutOfTheFolderIsNotFollowed(t *testing.T) {
	t.Parallel()
	env := newEnv(t, state.Options{})
	if err := env.WriteMaterial("ca.crt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(env.Dir(), "tls", "link.crt")); err != nil {
		t.Fatal(err)
	}
	if data, ok, err := env.ReadMaterial("link.crt"); err == nil {
		t.Errorf("a symlink out of the folder was followed: %q, %v", data, ok)
	}
}
