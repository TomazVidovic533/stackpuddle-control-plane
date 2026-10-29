// Package local is the sealed-file secret provider: one AES-256-GCM file per scope.
//
// The file is <dir>/<scopeUid>.sealed with mode 0600. Its associated data is the scope UID and the
// store version, so a store copied into another scope fails to open. The key is 32 random bytes the
// caller supplies through KeySource; StackPuddle never writes it, never regenerates a missing value
// and has no plaintext fallback.
package local

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

const (
	// ProviderID is the name environments declare for this provider.
	ProviderID = "local"

	magic        = "SPSEALED"
	storeVersion = 1
	nonceSize    = 12
	keySize      = 32
	fileLimit    = 4 << 20
	keyIDHex     = 16
	dirMode      = 0o700
	fileMode     = 0o600
	maxCreate    = 1024
)

// Options configure a Provider.
type Options struct {
	// Dir is the absolute folder of the sealed files.
	Dir string
	// KeySource returns the 32-byte key, or an error wrapping secrets.ErrKeyMissing or secrets.ErrKeyInvalid.
	KeySource func() ([keySize]byte, error)
	// Now supplies the time recorded with new versions.
	Now func() time.Time
	// Random supplies version IDs and temporary-file suffixes. Generated secret values and
	// encryption nonces always come from crypto/rand, never from this reader: a deterministic reader
	// would repeat a GCM nonce under one key and make generated secrets predictable.
	Random io.Reader
}

// Provider is the local sealed-file provider.
type Provider struct {
	opts Options
}

// New returns a Provider. A nil Now uses time.Now and a nil Random uses crypto/rand.
func New(opts Options) *Provider {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	return &Provider{opts: opts}
}

// ID returns "local".
func (*Provider) ID() string { return ProviderID }

type plainVersion struct {
	ID        state.SecretVersionID `json:"id"`
	Value     []byte                `json:"value"`
	Origin    state.Origin          `json:"origin"`
	CreatedBy string                `json:"createdBy"`
	CreatedAt time.Time             `json:"createdAt"`
}

type plainEntry struct {
	Versions []plainVersion `json:"versions"`
}

type plainStore struct {
	StoreVersion int                   `json:"storeVersion"`
	Entries      map[string]plainEntry `json:"entries"`
}

// keyID fingerprints the key without revealing it: the first 16 hex characters of an HMAC of a
// fixed label.
func keyID(key [keySize]byte) string {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("stackpuddle secrets key id v1"))
	return hex.EncodeToString(mac.Sum(nil))[:keyIDHex]
}

// Open opens the store of scope. A missing file is an empty store, so the first write creates it.
// A key that differs from scope.KeyID fails with secrets.ErrKeyMismatch before decryption.
func (p *Provider) Open(ctx context.Context, scope secrets.ScopeRef) (secrets.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("local: open store: %w", context.Cause(ctx))
	}
	key, err := p.opts.KeySource()
	if err != nil {
		return nil, err
	}
	id := keyID(key)
	if scope.KeyID != "" && subtle.ConstantTimeCompare([]byte(scope.KeyID), []byte(id)) != 1 {
		return nil, fmt.Errorf("%w: the key differs from the one this scope was created with", secrets.ErrKeyMismatch)
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	s := &store{provider: p, scope: scope, keyID: id, aead: aead, plain: plainStore{StoreVersion: storeVersion, Entries: map[string]plainEntry{}}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func newAEAD(key [keySize]byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("local: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("local: gcm: %w", err)
	}
	return aead, nil
}

func (s *store) associatedData() []byte {
	return append([]byte(s.scope.UID.String()), storeVersion)
}

func (s *store) path() string {
	return filepath.Join(s.provider.opts.Dir, s.scope.UID.String()+".sealed")
}

var errUnreadable = secrets.ErrStoreUnreadable

type store struct {
	provider *Provider
	scope    secrets.ScopeRef
	keyID    string
	aead     cipher.AEAD
	plain    plainStore
}

func (s *store) KeyID() string { return s.keyID }

func (s *store) Entry(ref secrets.Ref) (secrets.EntryInfo, bool) {
	entry, ok := s.plain.Entries[ref.String()]
	if !ok {
		return secrets.EntryInfo{}, false
	}
	info := secrets.EntryInfo{}
	for _, v := range entry.Versions {
		info.Versions = append(info.Versions, secrets.Version{ID: v.ID, Origin: v.Origin, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt})
	}
	return info, true
}

func (s *store) Resolve(ctx context.Context, ref secrets.Ref, version state.SecretVersionID) (secrets.Value, error) {
	if err := ctx.Err(); err != nil {
		return secrets.Value{}, fmt.Errorf("local: resolve: %w", context.Cause(ctx))
	}
	for _, v := range s.plain.Entries[ref.String()].Versions {
		if v.ID == version {
			return secrets.NewValue(append([]byte(nil), v.Value...)), nil
		}
	}
	return secrets.Value{}, fmt.Errorf("%w: %s %s", secrets.ErrSecretMissing, ref, version)
}

func (s *store) CreateIfAbsent(ctx context.Context, ref secrets.Ref, size int, createdBy string) (secrets.Version, error) {
	if err := ctx.Err(); err != nil {
		return secrets.Version{}, fmt.Errorf("local: create: %w", context.Cause(ctx))
	}
	if existing, ok := s.latest(ref); ok {
		return existing, nil
	}
	if size < 1 || size > maxCreate {
		return secrets.Version{}, fmt.Errorf("local: size %d is outside 1..%d", size, maxCreate)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return secrets.Version{}, fmt.Errorf("local: random value: %w", err)
	}
	text := []byte(base64.RawURLEncoding.EncodeToString(raw))
	return s.add(ref, text, state.OriginGenerated, createdBy)
}

func (s *store) CreateValueIfAbsent(ctx context.Context, ref secrets.Ref, value secrets.Value, createdBy string) (secrets.Version, error) {
	if err := ctx.Err(); err != nil {
		return secrets.Version{}, fmt.Errorf("local: create value: %w", context.Cause(ctx))
	}
	if existing, ok := s.latest(ref); ok {
		return existing, nil
	}
	if value.Len() == 0 || value.Len() > fileLimit {
		return secrets.Version{}, fmt.Errorf("local: value size %d is outside 1..%d", value.Len(), fileLimit)
	}
	return s.add(ref, append([]byte(nil), value.Reveal()...), state.OriginGenerated, createdBy)
}

func (s *store) Import(ctx context.Context, ref secrets.Ref, value secrets.Value, createdBy string) (secrets.Version, error) {
	if err := ctx.Err(); err != nil {
		return secrets.Version{}, fmt.Errorf("local: import: %w", context.Cause(ctx))
	}
	if entry := s.plain.Entries[ref.String()]; len(entry.Versions) > 0 {
		last := entry.Versions[len(entry.Versions)-1]
		if subtle.ConstantTimeCompare(last.Value, value.Reveal()) == 1 {
			return toVersion(last), nil
		}
	}
	return s.add(ref, append([]byte(nil), value.Reveal()...), state.OriginSupplied, createdBy)
}

func (s *store) latest(ref secrets.Ref) (secrets.Version, bool) {
	versions := s.plain.Entries[ref.String()].Versions
	if len(versions) == 0 {
		return secrets.Version{}, false
	}
	return toVersion(versions[len(versions)-1]), true
}

func toVersion(v plainVersion) secrets.Version {
	return secrets.Version{ID: v.ID, Origin: v.Origin, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
}

func (s *store) add(ref secrets.Ref, value []byte, origin state.Origin, createdBy string) (secrets.Version, error) {
	now := s.provider.opts.Now().UTC()
	id, err := state.NewSecretVersionID(now, s.provider.opts.Random)
	if err != nil {
		return secrets.Version{}, err
	}
	entry := s.plain.Entries[ref.String()]
	entry.Versions = append(entry.Versions, plainVersion{ID: id, Value: value, Origin: origin, CreatedBy: createdBy, CreatedAt: now})
	s.plain.Entries[ref.String()] = entry
	if err := s.persist(); err != nil {
		entry.Versions = entry.Versions[:len(entry.Versions)-1]
		s.plain.Entries[ref.String()] = entry
		return secrets.Version{}, err
	}
	return toVersion(entry.Versions[len(entry.Versions)-1]), nil
}

var errMalformed = errors.New("malformed sealed store")

func (s *store) load() error {
	file, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("local: open sealed store: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, fileLimit+1))
	if err != nil {
		return fmt.Errorf("local: read sealed store: %w", err)
	}
	plain, err := s.open(data)
	if err != nil {
		return fmt.Errorf("%w: %w", errUnreadable, err)
	}
	s.plain = plain
	return nil
}

func (s *store) open(data []byte) (plainStore, error) {
	header := len(magic) + 1 + nonceSize
	if len(data) > fileLimit || len(data) < header+s.aead.Overhead() || string(data[:len(magic)]) != magic || data[len(magic)] != storeVersion {
		return plainStore{}, errMalformed
	}
	nonce := data[len(magic)+1 : header]
	plaintext, err := s.aead.Open(nil, nonce, data[header:], s.associatedData())
	if err != nil {
		return plainStore{}, errors.New("authentication failed")
	}
	var plain plainStore
	if err := decodePlain(plaintext, &plain); err != nil {
		return plainStore{}, err
	}
	if plain.Entries == nil {
		plain.Entries = map[string]plainEntry{}
	}
	return plain, nil
}

func (s *store) persist() error {
	plaintext, err := encodePlain(s.plain)
	if err != nil {
		return err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("local: nonce: %w", err)
	}
	out := append([]byte(magic), storeVersion)
	out = append(out, nonce...)
	out = s.aead.Seal(out, nonce, plaintext, s.associatedData())
	if err := os.MkdirAll(s.provider.opts.Dir, dirMode); err != nil {
		return fmt.Errorf("local: create %s: %w", s.provider.opts.Dir, err)
	}
	if err := os.Chmod(s.provider.opts.Dir, dirMode); err != nil {
		return fmt.Errorf("local: chmod %s: %w", s.provider.opts.Dir, err)
	}
	root, err := os.OpenRoot(s.provider.opts.Dir)
	if err != nil {
		return fmt.Errorf("local: open %s: %w", s.provider.opts.Dir, err)
	}
	defer root.Close()
	return durable.WriteFile(root, s.scope.UID.String()+".sealed", out, durable.Options{Perm: fileMode, Random: s.provider.opts.Random})
}
