package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Sentinels for the failures a caller classifies.
var (
	// ErrUnsupported marks a file this engine cannot read: over its size cap, a newer version, an
	// unknown field, an unknown enum value or malformed content. The bytes on disk stay untouched.
	ErrUnsupported = errors.New("state is not readable by this engine")
	// ErrStaleGeneration marks a commit that lost to a newer generation; the newer file is kept.
	ErrStaleGeneration = errors.New("state generation is stale")
	// ErrScopeExists marks a second write of scope.json, which is written once.
	ErrScopeExists = errors.New("scope.json already exists")
)

// Size caps for the files read from disk (untrusted bytes).
const (
	stateFileLimit  = 16 << 20
	scopeFileLimit  = 64 << 10
	cancelFileLimit = 4 << 10
	archiveLimit    = 16 << 20
)

// ParseState decodes state.json strictly.
func ParseState(data []byte) (State, error) {
	var s State
	if err := decodeStrict(data, stateFileLimit, &s); err != nil {
		return State{}, err
	}
	if err := s.validate(); err != nil {
		return State{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return s, nil
}

// ParseScope decodes scope.json strictly.
func ParseScope(data []byte) (Scope, error) {
	var s Scope
	if err := decodeStrict(data, scopeFileLimit, &s); err != nil {
		return Scope{}, err
	}
	if s.ScopeUID.IsZero() || s.EngineID == "" || s.ComposeProject == "" || s.KeyID == "" {
		return Scope{}, fmt.Errorf("%w: scope.json lacks a required member", ErrUnsupported)
	}
	return s, nil
}

// ParseCancel decodes a cancel request strictly.
func ParseCancel(data []byte) (CancelRequest, error) {
	var c CancelRequest
	if err := decodeStrict(data, cancelFileLimit, &c); err != nil {
		return CancelRequest{}, err
	}
	if c.Operation.IsZero() || c.Actor.Kind.String() == "" {
		return CancelRequest{}, fmt.Errorf("%w: cancel request lacks a required member", ErrUnsupported)
	}
	return c, nil
}

// ParseArchived decodes an archived operation strictly.
func ParseArchived(data []byte) (Archived, error) {
	var a Archived
	if err := decodeStrict(data, archiveLimit, &a); err != nil {
		return Archived{}, err
	}
	if err := a.Operation.validate(); err != nil {
		return Archived{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return a, nil
}

func decodeStrict[T any](data []byte, limit int, out *T) error {
	if len(data) > limit {
		return fmt.Errorf("%w: file is larger than %d bytes", ErrUnsupported, limit)
	}
	var probe struct {
		StateVersion int `json:"stateVersion"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && probe.StateVersion != StateVersion {
		return fmt.Errorf("%w: stateVersion %d, this engine reads %d", ErrUnsupported, probe.StateVersion, StateVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the document", ErrUnsupported)
	}
	return nil
}

func kindPattern() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*$`)
}

// engineKinds are the engine-owned mapping kinds; a driver's kinds carry its module's namespace.
func engineKind(kind string) bool {
	switch kind {
	case "compose.image", "compose.volume", "compose.service", "compose.job", "compose.network":
		return true
	}
	return false
}

func validKind(kind string) bool {
	if !kindPattern().MatchString(kind) {
		return false
	}
	if strings.HasPrefix(kind, "compose.") {
		return engineKind(kind)
	}
	return true
}

func (s State) validate() error {
	if s.StateVersion != StateVersion {
		return fmt.Errorf("stateVersion %d", s.StateVersion)
	}
	if s.ScopeUID.IsZero() || s.Generation < 0 {
		return errors.New("scopeUid or generation is missing")
	}
	if s.Accepted != nil {
		if err := s.Accepted.validate(); err != nil {
			return err
		}
	}
	for ref, r := range s.Realizations {
		if err := r.validate(); err != nil {
			return fmt.Errorf("realization %q: %w", ref, err)
		}
	}
	if s.Operation != nil {
		return s.Operation.validate()
	}
	return nil
}

func (a Accepted) validate() error {
	if a.Revision < 1 || len(a.Model) == 0 || len(a.Topology) == 0 {
		return errors.New("accepted revision, model or topology is missing")
	}
	for ref, c := range a.Credentials {
		if c.Provider == "" || c.Origin.String() == "" || c.Version.String() == "" {
			return fmt.Errorf("credential %q is incomplete", ref)
		}
	}
	for ref, i := range a.Intent {
		if i.String() == "" {
			return fmt.Errorf("intent %q is unknown", ref)
		}
	}
	return nil
}

func (r Realization) validate() error {
	if r.UID.String() == "" || r.State.String() == "" {
		return errors.New("uid or state is missing")
	}
	for _, m := range r.Mappings {
		if !validKind(m.Kind) || m.Name == "" {
			return fmt.Errorf("mapping kind %q or name is invalid", m.Kind)
		}
	}
	return nil
}

func (o Operation) validate() error {
	if o.ID.IsZero() || o.Command.String() == "" || o.Status.String() == "" || o.Actor.Kind.String() == "" {
		return errors.New("operation lacks a required member")
	}
	if o.Revision < 1 || o.CreatedAt.IsZero() || o.UpdatedAt.IsZero() {
		return errors.New("operation revision or times are missing")
	}
	for _, step := range o.Steps {
		if err := step.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s Step) validate() error {
	if s.ID == "" || s.Action == "" || s.Object == "" || s.Status.String() == "" {
		return fmt.Errorf("step %q is incomplete", s.ID)
	}
	return nil
}
