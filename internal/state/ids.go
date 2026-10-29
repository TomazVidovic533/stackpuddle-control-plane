package state

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrInvalidID marks text that is not a prefixed ULID of the requested type.
var ErrInvalidID = errors.New("invalid identifier")

const (
	crockford   = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ulidChars   = 26
	ulidBytes   = 16
	timeBytes   = 6
	randomChars = 16
	groupBits   = 5
	leadPadBits = 2
	byteBits    = 8
	suffixChars = 6
)

func newULID(now time.Time, random io.Reader) (string, error) {
	var raw [ulidBytes]byte
	millis := uint64(now.UnixMilli()) //nolint:gosec // reason: UnixMilli is non-negative for any time after 1970
	for i := range timeBytes {
		raw[i] = byte(millis >> (byteBits * (timeBytes - 1 - i)))
	}
	if _, err := io.ReadFull(random, raw[timeBytes:]); err != nil {
		return "", fmt.Errorf("state: random bytes for an identifier: %w", err)
	}
	out := make([]byte, ulidChars)
	for i := range out {
		start := i*groupBits - leadPadBits
		var group byte
		for j := range groupBits {
			bit := byte(0)
			if p := start + j; p >= 0 {
				bit = (raw[p/byteBits] >> (byteBits - 1 - p%byteBits)) & 1
			}
			group = group<<1 | bit
		}
		out[i] = crockford[group]
	}
	return string(out), nil
}

func parseULID(prefix, text string) (string, error) {
	body, ok := strings.CutPrefix(text, prefix+"-")
	if !ok || len(body) != ulidChars {
		return "", fmt.Errorf("%w: want %s- and %d characters", ErrInvalidID, prefix, ulidChars)
	}
	for _, c := range body {
		if !strings.ContainsRune(crockford, c) {
			return "", fmt.Errorf("%w: %q is not a Crockford base32 character", ErrInvalidID, c)
		}
	}
	return text, nil
}

// OperationID identifies one operation: "op-" and a ULID. It is comparable.
type OperationID struct{ v string }

// NewOperationID returns a fresh OperationID from now and random.
func NewOperationID(now time.Time, random io.Reader) (OperationID, error) {
	body, err := newULID(now, random)
	return OperationID{"op-" + body}, err
}

// ParseOperationID parses text such as "op-01J9Z8K3M4N5P6Q7R8S9T0V1W2".
func ParseOperationID(text string) (OperationID, error) {
	v, err := parseULID("op", text)
	return OperationID{v}, err
}

// String returns the identifier, or "" for the zero value.
func (id OperationID) String() string { return id.v }

// IsZero reports whether id is the zero value.
func (id OperationID) IsZero() bool { return id.v == "" }

// MarshalText implements encoding.TextMarshaler.
func (id OperationID) MarshalText() ([]byte, error) { return []byte(id.v), nil }

// UnmarshalText implements encoding.TextUnmarshaler and validates the text.
func (id *OperationID) UnmarshalText(text []byte) error {
	parsed, err := ParseOperationID(string(text))
	*id = parsed
	return err
}

// ScopeUID identifies one scope on one machine: "sc-" and a ULID. It is comparable.
type ScopeUID struct{ v string }

// NewScopeUID returns a fresh ScopeUID from now and random.
func NewScopeUID(now time.Time, random io.Reader) (ScopeUID, error) {
	body, err := newULID(now, random)
	return ScopeUID{"sc-" + body}, err
}

// ParseScopeUID parses text such as "sc-01J9Z8K3M4N5P6Q7R8S9T0V1W3".
func ParseScopeUID(text string) (ScopeUID, error) {
	v, err := parseULID("sc", text)
	return ScopeUID{v}, err
}

// String returns the identifier, or "" for the zero value.
func (id ScopeUID) String() string { return id.v }

// IsZero reports whether id is the zero value.
func (id ScopeUID) IsZero() bool { return id.v == "" }

// Suffix returns six lowercase characters of the random part of the ULID, never of its time
// prefix, so two scopes created in the same second still differ.
func (id ScopeUID) Suffix() string {
	const randomStart = len("sc-") + ulidChars - randomChars
	if len(id.v) < randomStart+suffixChars {
		return ""
	}
	return strings.ToLower(id.v[randomStart : randomStart+suffixChars])
}

// MarshalText implements encoding.TextMarshaler.
func (id ScopeUID) MarshalText() ([]byte, error) { return []byte(id.v), nil }

// UnmarshalText implements encoding.TextUnmarshaler and validates the text.
func (id *ScopeUID) UnmarshalText(text []byte) error {
	parsed, err := ParseScopeUID(string(text))
	*id = parsed
	return err
}

// RealizationID identifies one realized object: "rz-" and a ULID. It is comparable.
type RealizationID struct{ v string }

// NewRealizationID returns a fresh RealizationID from now and random.
func NewRealizationID(now time.Time, random io.Reader) (RealizationID, error) {
	body, err := newULID(now, random)
	return RealizationID{"rz-" + body}, err
}

// ParseRealizationID parses text such as "rz-01J9Z8K3M4N5P6Q7R8S9T0V1W3".
func ParseRealizationID(text string) (RealizationID, error) {
	v, err := parseULID("rz", text)
	return RealizationID{v}, err
}

// String returns the identifier, or "" for the zero value.
func (id RealizationID) String() string { return id.v }

// MarshalText implements encoding.TextMarshaler.
func (id RealizationID) MarshalText() ([]byte, error) { return []byte(id.v), nil }

// UnmarshalText implements encoding.TextUnmarshaler and validates the text.
func (id *RealizationID) UnmarshalText(text []byte) error {
	parsed, err := ParseRealizationID(string(text))
	*id = parsed
	return err
}

// SecretVersionID identifies one stored secret version: "sv-" and a ULID. It is comparable.
type SecretVersionID struct{ v string }

// NewSecretVersionID returns a fresh SecretVersionID from now and random.
func NewSecretVersionID(now time.Time, random io.Reader) (SecretVersionID, error) {
	body, err := newULID(now, random)
	return SecretVersionID{"sv-" + body}, err
}

// ParseSecretVersionID parses text such as "sv-01J9Z8K3M4N5P6Q7R8S9T0V1W3".
func ParseSecretVersionID(text string) (SecretVersionID, error) {
	v, err := parseULID("sv", text)
	return SecretVersionID{v}, err
}

// String returns the identifier, or "" for the zero value.
func (id SecretVersionID) String() string { return id.v }

// MarshalText implements encoding.TextMarshaler.
func (id SecretVersionID) MarshalText() ([]byte, error) { return []byte(id.v), nil }

// UnmarshalText implements encoding.TextUnmarshaler and validates the text.
func (id *SecretVersionID) UnmarshalText(text []byte) error {
	parsed, err := ParseSecretVersionID(string(text))
	*id = parsed
	return err
}
