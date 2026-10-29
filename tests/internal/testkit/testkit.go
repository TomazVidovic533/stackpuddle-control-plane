// Package testkit holds the helpers every test layer shares: scratch roots, a fixed clock and
// random source, and the helper-process launcher the process tests use to kill a real process at a
// chosen point.
package testkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperEnv names the helper a re-executed test binary should run instead of its tests.
const helperEnv = "STACKPUDDLE_TEST_HELPER"

// Helper is a function a test binary can run as a separate process. It returns the exit code.
type Helper func(args []string) int

// Dispatch runs the helper the process was launched for and exits, or returns when the process is an
// ordinary test run. Call it first in TestMain.
func Dispatch(helpers map[string]Helper) {
	name := os.Getenv(helperEnv)
	if name == "" {
		return
	}
	helper, ok := helpers[name]
	if !ok {
		os.Stderr.WriteString("unknown test helper " + name + "\n")
		os.Exit(2)
	}
	os.Exit(helper(os.Args[1:]))
}

// Command returns a command that re-executes the running test binary as the named helper with args.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), helperEnv+"="+name)
	return cmd
}

// Root returns a fresh scratch directory for the test, removed afterwards, under TMPDIR.
func Root(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Clock returns a deterministic clock that advances one second per call from a fixed start.
func Clock() func() time.Time {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	return func() time.Time {
		now = now.Add(time.Second)
		return now
	}
}

// Random is a deterministic byte stream: SHA-256 blocks of a counter and a seed.
type Random struct {
	seed    string
	counter uint64
	buf     bytes.Buffer
}

// NewRandom returns a deterministic source for the seed.
func NewRandom(seed string) *Random { return &Random{seed: seed} }

// Read fills p with the next bytes of the stream.
func (r *Random) Read(p []byte) (int, error) {
	for r.buf.Len() < len(p) {
		var counter [8]byte
		binary.BigEndian.PutUint64(counter[:], r.counter)
		r.counter++
		sum := sha256.Sum256(append([]byte(r.seed), counter[:]...))
		r.buf.Write(sum[:])
	}
	return r.buf.Read(p)
}

var _ io.Reader = (*Random)(nil)

// LookupEnv returns a lookup over a fixed environment.
func LookupEnv(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// Key returns a valid secrets key setting: 32 bytes derived from the seed, standard base64.
func Key(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return strings.TrimSpace(base64Std(sum[:]))
}

// NewRandomBytes returns n deterministic bytes for the seed.
func NewRandomBytes(seed string, n int) []byte {
	out := make([]byte, n)
	if _, err := NewRandom(seed).Read(out); err != nil {
		panic(err)
	}
	return out
}

// Epoch is the fixed start time of the deterministic clock.
var Epoch = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
