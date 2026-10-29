// Package secrets is the provider port for secret values: resolve, create-if-absent and import.
//
// State and plans hold only references and versions, never values. A Value redacts itself on every
// print path; its bytes leave through Reveal in exactly two places, the sealed-file writer and the
// child-environment builder of the runtime adapter.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Sentinels for the failures a caller classifies. Wrapped errors name the setting or reference,
// never a value.
var (
	// ErrKeyMissing marks an unset provider key.
	ErrKeyMissing = errors.New("secrets key is missing")
	// ErrKeyInvalid marks a provider key that is malformed.
	ErrKeyInvalid = errors.New("secrets key is invalid")
	// ErrKeyMismatch marks a key that differs from the one the scope was created with.
	ErrKeyMismatch = errors.New("secrets key does not match this scope")
	// ErrStoreUnreadable marks a store that fails authentication: a copy from another scope or tampering.
	ErrStoreUnreadable = errors.New("secrets store is unreadable")
	// ErrSecretMissing marks a version that state references and the store lacks.
	ErrSecretMissing = errors.New("secret version is missing")
	// ErrInvalidRef marks text that is not a secret reference.
	ErrInvalidRef = errors.New("invalid secret reference")
)

const redacted = "[redacted]"

// Value is a secret value. Every print path shows "[redacted]".
type Value struct{ b []byte }

// NewValue wraps b, which the Value then owns.
func NewValue(b []byte) Value { return Value{b: b} }

// Reveal returns the secret bytes. Only the sealed-file writer and the child-environment builder call it.
func (v Value) Reveal() []byte { return v.b }

// Len returns the length of the value in bytes.
func (v Value) Len() int { return len(v.b) }

// String implements fmt.Stringer.
func (Value) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (Value) GoString() string { return redacted }

// Format implements fmt.Formatter so no verb prints the bytes.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// MarshalJSON implements json.Marshaler.
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Value) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LogValue implements slog.LogValuer.
func (Value) LogValue() slog.Value { return slog.StringValue(redacted) }

// Ref names a secret inside one scope, such as "input/db-password".
type Ref struct{ v string }

const maxRefLength = 128

func refPattern() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z][a-z0-9-]*(?:/[a-z][a-z0-9-]*)*$`)
}

// ParseRef parses a secret reference.
func ParseRef(text string) (Ref, error) {
	if len(text) > maxRefLength || !refPattern().MatchString(text) {
		return Ref{}, fmt.Errorf("%w: %q", ErrInvalidRef, text)
	}
	return Ref{v: text}, nil
}

// String returns the reference.
func (r Ref) String() string { return r.v }

// Version describes one stored version of a secret; it carries no value.
type Version struct {
	ID        state.SecretVersionID
	Origin    state.Origin
	CreatedBy string
	CreatedAt time.Time
}

// EntryInfo lists the versions of one reference, oldest first.
type EntryInfo struct {
	Versions []Version
}

// ScopeRef identifies the scope a store belongs to.
type ScopeRef struct {
	// UID binds the store to its scope.
	UID state.ScopeUID
	// KeyID is the key fingerprint recorded in scope.json, or "" for a scope that has none yet.
	KeyID string
}

// Provider opens the secret store of one scope.
type Provider interface {
	// ID returns the provider name an environment declares, such as "local".
	ID() string
	// Open opens the store. It fails with ErrKeyMissing, ErrKeyInvalid, ErrKeyMismatch or ErrStoreUnreadable.
	Open(ctx context.Context, scope ScopeRef) (Store, error)
}

// Store reads and writes the secrets of one scope. Callers hold the environment lock; a Store is not
// safe for concurrent use.
type Store interface {
	// KeyID returns the fingerprint of the key that opened the store.
	KeyID() string
	// Resolve returns the value of one version, or an error wrapping ErrSecretMissing.
	Resolve(ctx context.Context, ref Ref, version state.SecretVersionID) (Value, error)
	// CreateIfAbsent returns the existing latest version of ref, or stores size random bytes as a new one.
	CreateIfAbsent(ctx context.Context, ref Ref, size int, createdBy string) (Version, error)
	// CreateValueIfAbsent returns the existing latest version of ref, or stores value as a new version
	// whose origin is generated: the caller made the bytes (a private key) rather than the user. The
	// value of an existing version is never replaced.
	CreateValueIfAbsent(ctx context.Context, ref Ref, value Value, createdBy string) (Version, error)
	// Import stores a supplied value as a new version unless the latest version already holds it.
	Import(ctx context.Context, ref Ref, value Value, createdBy string) (Version, error)
	// Entry returns the versions of ref, for recovery.
	Entry(ref Ref) (EntryInfo, bool)
}
