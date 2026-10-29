package local_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets/local"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// logKey is the attribute key of the logged secret.
const logKey = "value"

const canary = "canary-secret-value-7f3a9c"

var epoch = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

func keyOf(seed string) [config.KeySize]byte {
	var key [config.KeySize]byte
	copy(key[:], testkit.NewRandomBytes(seed, config.KeySize))
	return key
}

func provider(t testing.TB, dir string, key [config.KeySize]byte) *local.Provider {
	t.Helper()
	return local.New(local.Options{
		Dir:       dir,
		KeySource: func() ([config.KeySize]byte, error) { return key, nil },
		Now:       func() time.Time { return epoch },
		Random:    testkit.NewRandom("local"),
	})
}

func scopeUID(t testing.TB, seed string) state.ScopeUID {
	t.Helper()
	uid, err := state.NewScopeUID(epoch, testkit.NewRandom(seed))
	if err != nil {
		t.Fatal(err)
	}
	return uid
}

func ref(t testing.TB, text string) secrets.Ref {
	t.Helper()
	r, err := secrets.ParseRef(text)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func open(t testing.TB, p *local.Provider, uid state.ScopeUID, keyID string) secrets.Store {
	t.Helper()
	store, err := p.Open(context.Background(), secrets.ScopeRef{UID: uid, KeyID: keyID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func TestStore_RoundTripAcrossReopen(t *testing.T) {
	t.Parallel()
	dir, uid, key := testkit.Root(t), scopeUID(t, "rt"), keyOf("rt")
	store := open(t, provider(t, dir, key), uid, "")
	supplied, err := store.Import(t.Context(), ref(t, "input/db-password"), secrets.NewValue([]byte(canary)), "op-x/s1")
	if err != nil {
		t.Fatal(err)
	}
	generated, err := store.CreateIfAbsent(t.Context(), ref(t, "primary/admin"), 24, "op-x/s2")
	if err != nil {
		t.Fatal(err)
	}
	if supplied.Origin != state.OriginSupplied || generated.Origin != state.OriginGenerated {
		t.Errorf("origins = %v, %v", supplied.Origin, generated.Origin)
	}

	reopened := open(t, provider(t, dir, key), uid, store.KeyID())
	got, err := reopened.Resolve(t.Context(), ref(t, "input/db-password"), supplied.ID)
	if err != nil || string(got.Reveal()) != canary {
		t.Fatalf("Resolve after reopen = %q, %v", got.Reveal(), err)
	}
	value, err := reopened.Resolve(t.Context(), ref(t, "primary/admin"), generated.ID)
	if err != nil || value.Len() == 0 {
		t.Fatalf("generated value = %d bytes, %v", value.Len(), err)
	}
	entry, ok := reopened.Entry(ref(t, "primary/admin"))
	if !ok || len(entry.Versions) != 1 || entry.Versions[0].CreatedBy != "op-x/s2" {
		t.Errorf("entry = %+v, %v", entry, ok)
	}
}

func TestStore_IsCreateOnly(t *testing.T) {
	t.Parallel()
	store := open(t, provider(t, testkit.Root(t), keyOf("co")), scopeUID(t, "co"), "")
	first, _ := store.CreateIfAbsent(t.Context(), ref(t, "a/b"), 16, "op/s1")
	second, err := store.CreateIfAbsent(t.Context(), ref(t, "a/b"), 16, "op/s2")
	if err != nil || second.ID != first.ID {
		t.Fatalf("CreateIfAbsent regenerated an existing secret: %v then %v (%v)", first.ID, second.ID, err)
	}
	same, _ := store.Import(t.Context(), ref(t, "a/c"), secrets.NewValue([]byte("v1")), "op/s3")
	again, _ := store.Import(t.Context(), ref(t, "a/c"), secrets.NewValue([]byte("v1")), "op/s4")
	if again.ID != same.ID {
		t.Error("importing the same value made a new version")
	}
	changed, _ := store.Import(t.Context(), ref(t, "a/c"), secrets.NewValue([]byte("v2")), "op/s5")
	if changed.ID == same.ID {
		t.Error("importing a different value kept the old version")
	}
	old, err := store.Resolve(t.Context(), ref(t, "a/c"), same.ID)
	if err != nil || string(old.Reveal()) != "v1" {
		t.Errorf("the old version was overwritten: %q, %v", old.Reveal(), err)
	}
	if _, err := store.CreateIfAbsent(t.Context(), ref(t, "a/zero"), 0, "op/s6"); err == nil {
		t.Error("CreateIfAbsent accepted size 0")
	}
}

func TestStore_CreateValueIfAbsentKeepsTheFirstValueAndRecordsItAsGenerated(t *testing.T) {
	t.Parallel()
	dir, uid, key := testkit.Root(t), scopeUID(t, "cv"), keyOf("cv")
	store := open(t, provider(t, dir, key), uid, "")
	first, err := store.CreateValueIfAbsent(t.Context(), ref(t, "tls/ca-key"), secrets.NewValue([]byte("first-key-bytes")), "op/s1")
	if err != nil || first.Origin != state.OriginGenerated || first.CreatedBy != "op/s1" {
		t.Fatalf("first = %+v, %v: want a generated version made by op/s1", first, err)
	}
	second, err := store.CreateValueIfAbsent(t.Context(), ref(t, "tls/ca-key"), secrets.NewValue([]byte("second-key-bytes")), "op/s2")
	if err != nil || second.ID != first.ID {
		t.Fatalf("a second call made a new version: %v then %v (%v)", first.ID, second.ID, err)
	}
	value, err := store.Resolve(t.Context(), ref(t, "tls/ca-key"), first.ID)
	if err != nil || string(value.Reveal()) != "first-key-bytes" {
		t.Errorf("the stored value is %q, %v: the first value must stay", value.Reveal(), err)
	}
	reopened := open(t, provider(t, dir, key), uid, "")
	if entry, ok := reopened.Entry(ref(t, "tls/ca-key")); !ok || len(entry.Versions) != 1 {
		t.Errorf("after reopening: %+v, %v", entry, ok)
	}
	raw, err := os.ReadFile(filepath.Join(dir, uid.String()+".sealed"))
	if err != nil || bytes.Contains(raw, []byte("first-key-bytes")) {
		t.Errorf("the sealed file holds the value in plaintext (%v)", err)
	}
	if _, err := store.CreateValueIfAbsent(t.Context(), ref(t, "tls/empty"), secrets.NewValue(nil), "op/s3"); err == nil {
		t.Error("an empty value was stored")
	}
}

func TestStore_MissingVersionIsSecretMissing(t *testing.T) {
	t.Parallel()
	store := open(t, provider(t, testkit.Root(t), keyOf("miss")), scopeUID(t, "miss"), "")
	id, _ := state.NewSecretVersionID(epoch, testkit.NewRandom("v"))
	if _, err := store.Resolve(t.Context(), ref(t, "a/b"), id); !errors.Is(err, secrets.ErrSecretMissing) {
		t.Fatalf("Resolve error = %v", err)
	}
}

func TestOpen_KeyFailures(t *testing.T) {
	t.Parallel()
	dir, uid := testkit.Root(t), scopeUID(t, "key")
	right := provider(t, dir, keyOf("right"))
	store := open(t, right, uid, "")
	if _, err := store.CreateIfAbsent(t.Context(), ref(t, "a/b"), 8, "op/s1"); err != nil {
		t.Fatal(err)
	}

	wrong := provider(t, dir, keyOf("wrong"))
	if _, err := wrong.Open(t.Context(), secrets.ScopeRef{UID: uid, KeyID: store.KeyID()}); !errors.Is(err, secrets.ErrKeyMismatch) {
		t.Errorf("a different key with a recorded key ID: %v, want ErrKeyMismatch", err)
	}
	if _, err := wrong.Open(t.Context(), secrets.ScopeRef{UID: uid}); !errors.Is(err, secrets.ErrStoreUnreadable) {
		t.Errorf("a different key with no recorded key ID: %v, want ErrStoreUnreadable", err)
	}
	missing := local.New(local.Options{Dir: dir, KeySource: func() ([config.KeySize]byte, error) {
		return [config.KeySize]byte{}, fmt.Errorf("%w: setting", secrets.ErrKeyMissing)
	}})
	if _, err := missing.Open(t.Context(), secrets.ScopeRef{UID: uid}); !errors.Is(err, secrets.ErrKeyMissing) {
		t.Errorf("an unset key: %v, want ErrKeyMissing", err)
	}
}

func TestOpen_AStoreCopiedIntoAnotherScopeFails(t *testing.T) {
	t.Parallel()
	dir, key := testkit.Root(t), keyOf("copy")
	uid, other := scopeUID(t, "one"), scopeUID(t, "two")
	store := open(t, provider(t, dir, key), uid, "")
	if _, err := store.Import(t.Context(), ref(t, "a/b"), secrets.NewValue([]byte(canary)), "op/s1"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, uid.String()+".sealed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, other.String()+".sealed"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider(t, dir, key).Open(t.Context(), secrets.ScopeRef{UID: other}); !errors.Is(err, secrets.ErrStoreUnreadable) {
		t.Fatalf("a store copied into another scope opened: %v", err)
	}
}

func TestOpen_RefusesTamperedAndMalformedFiles(t *testing.T) {
	t.Parallel()
	dir, key, uid := testkit.Root(t), keyOf("tamper"), scopeUID(t, "tamper")
	store := open(t, provider(t, dir, key), uid, "")
	if _, err := store.CreateIfAbsent(t.Context(), ref(t, "a/b"), 8, "op/s1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uid.String()+".sealed")
	good, _ := os.ReadFile(path)
	flipped := append([]byte(nil), good...)
	flipped[len(flipped)-1] ^= 0xff
	for name, data := range map[string][]byte{
		"flipped bit":  flipped,
		"truncated":    good[:len(good)/2],
		"empty":        {},
		"wrong magic":  append([]byte("NOTSEALD"), good[8:]...),
		"wrong format": append(append([]byte(nil), good[:8]...), append([]byte{2}, good[9:]...)...),
		"plain JSON":   []byte(`{"storeVersion":1,"entries":{}}`),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := provider(t, dir, key).Open(t.Context(), secrets.ScopeRef{UID: uid}); !errors.Is(err, secrets.ErrStoreUnreadable) {
			t.Errorf("%s: Open error = %v", name, err)
		}
	}
}

func TestStore_FileHoldsNoPlaintextAndIsPrivate(t *testing.T) {
	// The umask is process-wide, so this test does not run in parallel with the others.
	dir, uid := testkit.Root(t), scopeUID(t, "mode")
	old := syscall.Umask(0)
	defer syscall.Umask(old)
	store := open(t, provider(t, dir, keyOf("mode")), uid, "")
	if _, err := store.Import(t.Context(), ref(t, "a/b"), secrets.NewValue([]byte(canary)), "op/s1"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, uid.String()+".sealed")
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte(canary)) || bytes.Contains(data, []byte("a/b")) {
		t.Error("the sealed file holds plaintext")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v", info.Mode().Perm())
	}
	dirInfo, _ := os.Stat(dir)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("folder mode = %v", dirInfo.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the folder holds %d entries, want only the sealed file", len(entries))
	}
}

func TestValue_RedactsOnEveryPrintPath(t *testing.T) {
	t.Parallel()
	value := secrets.NewValue([]byte(canary))
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("resolved", slog.Any(logKey, value))
	text, _ := value.MarshalText()
	encoded, _ := json.Marshal(struct{ V secrets.Value }{value})
	outputs := []string{
		fmt.Sprint(value), fmt.Sprintf("%v", value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value),
		fmt.Sprintf("%s", value), fmt.Sprintf("%q", value), fmt.Sprintf("%x", value), fmt.Sprintf("%d", value),
		fmt.Sprintf("%v", []secrets.Value{value}), fmt.Sprintf("%+v", struct{ V secrets.Value }{value}),
		string(text), string(encoded), logged.String(), value.String(), value.GoString(),
	}
	for _, out := range outputs {
		if strings.Contains(out, canary) {
			t.Errorf("a print path leaked the value: %q", out)
		}
	}
	if !strings.Contains(fmt.Sprint(value), "[redacted]") {
		t.Error("the redaction marker is missing")
	}
}

func TestOpen_StopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := provider(t, testkit.Root(t), keyOf("ctx")).Open(ctx, secrets.ScopeRef{UID: scopeUID(t, "ctx")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open error = %v", err)
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	for _, good := range []string{"input/db-password", "primary/admin", "a"} {
		if _, err := secrets.ParseRef(good); err != nil {
			t.Errorf("ParseRef(%q): %v", good, err)
		}
	}
	for _, bad := range []string{"", "/a", "a/", "A/b", "a//b", "a b", strings.Repeat("a", 129), "../x"} {
		if _, err := secrets.ParseRef(bad); !errors.Is(err, secrets.ErrInvalidRef) {
			t.Errorf("ParseRef(%q) error = %v", bad, err)
		}
	}
}

func FuzzOpenStore(f *testing.F) {
	dir, key, uid := f.TempDir(), keyOf("fuzz"), scopeUID(f, "fuzz")
	store := open(f, provider(f, dir, key), uid, "")
	if _, err := store.Import(f.Context(), ref(f, "a/b"), secrets.NewValue([]byte("v")), "op/s1"); err != nil {
		f.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join(dir, uid.String()+".sealed"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte("SPSEALED"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, uid.String()+".sealed"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := provider(t, root, key).Open(t.Context(), secrets.ScopeRef{UID: uid})
		if err != nil && !errors.Is(err, secrets.ErrStoreUnreadable) {
			t.Fatalf("a refusal must be ErrStoreUnreadable, got %v", err)
		}
	})
}

// zeroReader is a deterministic reader that never varies, the worst case an injected Random can be.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestStore_NoncesAndGeneratedValuesNeverComeFromTheInjectedReader(t *testing.T) {
	t.Parallel()
	dir, uid := testkit.Root(t), scopeUID(t, "nonce")
	p := local.New(local.Options{
		Dir:       dir,
		KeySource: func() ([config.KeySize]byte, error) { return keyOf("nonce"), nil },
		Now:       func() time.Time { return epoch },
		Random:    zeroReader{},
	})
	store := open(t, p, uid, "")
	path := filepath.Join(dir, uid.String()+".sealed")
	nonceOf := func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		const header = len("SPSEALED") + 1
		return string(data[header : header+12])
	}
	var nonces, values []string
	for _, name := range []string{"a/one", "a/two", "a/three"} {
		version, err := store.CreateIfAbsent(t.Context(), ref(t, name), 24, "op/s1")
		if err != nil {
			t.Fatal(err)
		}
		value, err := store.Resolve(t.Context(), ref(t, name), version.ID)
		if err != nil {
			t.Fatal(err)
		}
		nonces, values = append(nonces, nonceOf()), append(values, string(value.Reveal()))
	}
	for i := range nonces {
		for j := i + 1; j < len(nonces); j++ {
			if nonces[i] == nonces[j] {
				t.Errorf("writes %d and %d reused the GCM nonce under one key", i, j)
			}
			if values[i] == values[j] {
				t.Errorf("generated secrets %d and %d are equal: they follow the injected reader", i, j)
			}
		}
	}
}
