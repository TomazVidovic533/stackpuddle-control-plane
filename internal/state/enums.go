package state

import (
	"errors"
	"fmt"
)

// ErrUnknownValue marks text that names no member of a closed set.
var ErrUnknownValue = errors.New("unknown value")

func enumName(names []string, v int) string {
	if v < 1 || v > len(names) {
		return ""
	}
	return names[v-1]
}

func marshalEnum(kind string, names []string, v int) ([]byte, error) {
	name := enumName(names, v)
	if name == "" {
		return nil, fmt.Errorf("%w: %s value %d", ErrUnknownValue, kind, v)
	}
	return []byte(name), nil
}

func parseEnum(kind string, names []string, text string) (int, error) {
	for i, name := range names {
		if name == text {
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("%w: %s %q", ErrUnknownValue, kind, text)
}

// Command is what an operation asks of an environment.
type Command int

// The commands.
const (
	CommandUp Command = iota + 1
	CommandStop
	CommandDestroy
	// CommandRun invokes one job explicitly, after the provider and access steps the job binds.
	CommandRun
)

func commandNames() []string { return []string{"up", "stop", "destroy", "run"} }

// String returns the wire name, or "" for an invalid value.
func (c Command) String() string { return enumName(commandNames(), int(c)) }

// MarshalText implements encoding.TextMarshaler.
func (c Command) MarshalText() ([]byte, error) { return marshalEnum("command", commandNames(), int(c)) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (c *Command) UnmarshalText(text []byte) error {
	v, err := parseEnum("command", commandNames(), string(text))
	*c = Command(v)
	return err
}

// OpStatus is the lifecycle status of an operation.
type OpStatus int

// The operation statuses.
const (
	OpRunning OpStatus = iota + 1
	OpSucceeded
	OpFailed
	OpCancelled
	OpInterrupted
	OpBlocked
)

func opStatusNames() []string {
	return []string{"running", "succeeded", "failed", "cancelled", "interrupted", "blocked"}
}

// String returns the wire name, or "" for an invalid value.
func (s OpStatus) String() string { return enumName(opStatusNames(), int(s)) }

// Finished reports whether the status is final.
func (s OpStatus) Finished() bool { return s != OpRunning }

// MarshalText implements encoding.TextMarshaler.
func (s OpStatus) MarshalText() ([]byte, error) {
	return marshalEnum("status", opStatusNames(), int(s))
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (s *OpStatus) UnmarshalText(text []byte) error {
	v, err := parseEnum("status", opStatusNames(), string(text))
	*s = OpStatus(v)
	return err
}

// StepStatus is the status of one step of an operation.
type StepStatus int

// The step statuses.
const (
	StepPending StepStatus = iota + 1
	StepStarted
	StepCompleted
	StepCompletedByObservation
	StepNotApplied
	StepFailed
	StepBlocked
)

func stepStatusNames() []string {
	return []string{"pending", "started", "completed", "completed-by-observation", "not-applied", "failed", "blocked"}
}

// String returns the wire name, or "" for an invalid value.
func (s StepStatus) String() string { return enumName(stepStatusNames(), int(s)) }

// MarshalText implements encoding.TextMarshaler.
func (s StepStatus) MarshalText() ([]byte, error) {
	return marshalEnum("step status", stepStatusNames(), int(s))
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (s *StepStatus) UnmarshalText(text []byte) error {
	v, err := parseEnum("step status", stepStatusNames(), string(text))
	*s = StepStatus(v)
	return err
}

// ActorKind says who submitted an operation. m01 writes only ActorOSUser; ActorToken is read so a
// later service needs no format change.
type ActorKind int

// The actor kinds.
const (
	ActorOSUser ActorKind = iota + 1
	ActorToken
)

func actorKindNames() []string { return []string{"os-user", "token"} }

// String returns the wire name, or "" for an invalid value.
func (k ActorKind) String() string { return enumName(actorKindNames(), int(k)) }

// MarshalText implements encoding.TextMarshaler.
func (k ActorKind) MarshalText() ([]byte, error) {
	return marshalEnum("actor kind", actorKindNames(), int(k))
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (k *ActorKind) UnmarshalText(text []byte) error {
	v, err := parseEnum("actor kind", actorKindNames(), string(text))
	*k = ActorKind(v)
	return err
}

// Origin says where a credential value came from.
type Origin int

// The credential origins.
const (
	OriginSupplied Origin = iota + 1
	OriginGenerated
)

func originNames() []string { return []string{"supplied", "generated"} }

// String returns the wire name, or "" for an invalid value.
func (o Origin) String() string { return enumName(originNames(), int(o)) }

// MarshalText implements encoding.TextMarshaler.
func (o Origin) MarshalText() ([]byte, error) { return marshalEnum("origin", originNames(), int(o)) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (o *Origin) UnmarshalText(text []byte) error {
	v, err := parseEnum("origin", originNames(), string(text))
	*o = Origin(v)
	return err
}

// RealizationState is whether a realized object still exists.
type RealizationState int

// The realization states.
const (
	RealizationActive RealizationState = iota + 1
	RealizationLost
)

func realizationNames() []string { return []string{"active", "lost"} }

// String returns the wire name, or "" for an invalid value.
func (s RealizationState) String() string { return enumName(realizationNames(), int(s)) }

// MarshalText implements encoding.TextMarshaler.
func (s RealizationState) MarshalText() ([]byte, error) {
	return marshalEnum("realization state", realizationNames(), int(s))
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (s *RealizationState) UnmarshalText(text []byte) error {
	v, err := parseEnum("realization state", realizationNames(), string(text))
	*s = RealizationState(v)
	return err
}

// Intent is what the accepted state wants an object to do.
type Intent int

// The intents.
const (
	IntentRunning Intent = iota + 1
	IntentStopped
)

func intentNames() []string { return []string{"running", "stopped"} }

// String returns the wire name, or "" for an invalid value.
func (i Intent) String() string { return enumName(intentNames(), int(i)) }

// MarshalText implements encoding.TextMarshaler.
func (i Intent) MarshalText() ([]byte, error) { return marshalEnum("intent", intentNames(), int(i)) }

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (i *Intent) UnmarshalText(text []byte) error {
	v, err := parseEnum("intent", intentNames(), string(text))
	*i = Intent(v)
	return err
}

// Category is the class of a problem, which decides how a caller reacts.
type Category int

// The problem categories.
const (
	CategoryValidation Category = iota + 1
	CategoryNotFound
	CategoryConflict
	CategoryForbidden
	CategoryTransient
	CategoryUnexpected
)

func categoryNames() []string {
	return []string{"validation", "not_found", "conflict", "forbidden", "transient", "unexpected"}
}

// String returns the wire name, or "" for an invalid value.
func (c Category) String() string { return enumName(categoryNames(), int(c)) }

// MarshalText implements encoding.TextMarshaler.
func (c Category) MarshalText() ([]byte, error) {
	return marshalEnum("category", categoryNames(), int(c))
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (c *Category) UnmarshalText(text []byte) error {
	v, err := parseEnum("category", categoryNames(), string(text))
	*c = Category(v)
	return err
}
