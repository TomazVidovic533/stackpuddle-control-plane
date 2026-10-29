package postgres

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/TomazVidovic533/stackpuddle/model"
	validation "github.com/TomazVidovic533/stackpuddle/validation/postgres"
)

// ErrInvalidOptions marks options the module's pure rules refuse. The error names pointers and map keys,
// never a value written in the options.
var ErrInvalidOptions = errors.New("postgres options are invalid")

// Purposes of a user.
const (
	purposeRuntime    = "runtime"
	purposeMigrations = "migrations"
)

// defaultStartupSeconds is the startup bound when the effective options carry none.
const defaultStartupSeconds = 60

// The server the engine runs is fixed by the definition's workload. These are the values an option
// must equal; one that asks for anything else has no effect, so Plan refuses it. The memory and CPU
// values are schema defaults only: the engine applies no memory or CPU limit, and the container runs
// unbounded.
const (
	fixedMaxConnections = 100
	fixedSharedBuffers  = 128
	fixedMemoryMiB      = 1024
	fixedCPUs           = 1
)

// options is the part of the module's effective options the driver acts on. Server sizing is not
// read: the engine runs the server from the definition's workload.
type options struct {
	StartupTimeoutSeconds int                   `json:"startupTimeoutSeconds"`
	Users                 map[string]user       `json:"users"`
	Roles                 map[string]permission `json:"roles"`
	Databases             map[string]database   `json:"databases"`

	// Members the driver does not act on decode into raw messages, so a schema property never fails
	// the strict decode.
	Resources  json.RawMessage `json:"resources"`
	HostAccess json.RawMessage `json:"hostAccess"`
	Settings   json.RawMessage `json:"settings"`
}

// permission is a role that cannot log in; its schema fixes login to false.
type permission struct {
	Login bool `json:"login"`
}

type user struct {
	Purpose           string   `json:"purpose"`
	PasswordSecretRef string   `json:"passwordSecretRef"`
	MemberOf          []string `json:"memberOf"`
}

type database struct {
	ConnectionLimit *int                `json:"connectionLimit"`
	Settings        timeouts            `json:"settings"`
	Owner           string              `json:"owner"`
	ConnectRoles    []string            `json:"connectRoles"`
	Schemas         map[string]schema   `json:"schemas"`
	UserSettings    map[string]timeouts `json:"userSettings"`
}

type timeouts struct {
	StatementTimeoutMs *int `json:"statementTimeoutMs"`
	LockTimeoutMs      *int `json:"lockTimeoutMs"`
}

type schema struct {
	Grants map[string]grant `json:"grants"`
}

type grant struct {
	Tables    []string `json:"tables"`
	Sequences []string `json:"sequences"`
}

// parseOptions decodes the effective options and runs the module's pure rules from the stackpuddle
// validation package before anything is planned.
func parseOptions(raw json.RawMessage) (options, error) {
	var o options
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&o); err != nil {
		return options{}, fmt.Errorf("%w: decode: %w", ErrInvalidOptions, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return options{}, fmt.Errorf("%w: trailing data after the options object", ErrInvalidOptions)
	}
	diagnostics, err := validation.Validate(raw)
	if err != nil {
		return options{}, fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	if len(diagnostics) > 0 {
		return options{}, fmt.Errorf("%w: %s", ErrInvalidOptions, describe(diagnostics))
	}
	if o.StartupTimeoutSeconds == 0 {
		o.StartupTimeoutSeconds = defaultStartupSeconds
	}
	return o, nil
}

func describe(diagnostics model.Diagnostics) string {
	var out []string
	for _, d := range diagnostics {
		out = append(out, string(d.Code())+" at "+d.Location().Pointer())
	}
	return fmt.Sprint(out)
}

// ErrUnwiredOption marks an option the definition's fixed server does not act on.
var ErrUnwiredOption = errors.New("postgres option is not wired to the server yet")

// parsePlanOptions parses the options and refuses those the fixed server would ignore.
func parsePlanOptions(raw json.RawMessage) (options, error) {
	o, err := parseOptions(raw)
	if err != nil {
		return options{}, err
	}
	return o, o.refuseUnwired()
}

// refuseUnwired refuses every option whose value differs from what the fixed server runs with. The
// error names the pointer, never the value. A schema default (or an absent option) passes.
func (o options) refuseUnwired() error {
	var res struct {
		MemoryMiB *float64 `json:"memoryMiB"`
		CPUs      *float64 `json:"cpus"`
	}
	var set struct {
		MaxConnections   *float64 `json:"maxConnections"`
		SharedBuffersMiB *float64 `json:"sharedBuffersMiB"`
	}
	var host struct {
		Enabled bool `json:"enabled"`
	}
	for _, d := range []struct {
		raw json.RawMessage
		out any
	}{{o.Resources, &res}, {o.Settings, &set}, {o.HostAccess, &host}} {
		if err := decodeLoose(d.raw, d.out); err != nil {
			return fmt.Errorf("%w: decode: %w", ErrInvalidOptions, err)
		}
	}
	checks := []struct {
		pointer string
		differs bool
	}{
		{"/resources/memoryMiB", differs(res.MemoryMiB, fixedMemoryMiB)},
		{"/resources/cpus", differs(res.CPUs, fixedCPUs)},
		{"/settings/maxConnections", differs(set.MaxConnections, fixedMaxConnections)},
		{"/settings/sharedBuffersMiB", differs(set.SharedBuffersMiB, fixedSharedBuffers)},
		{"/hostAccess/enabled", host.Enabled},
		{"/startupTimeoutSeconds", o.StartupTimeoutSeconds > probeBudgetSeconds},
	}
	var bad []string
	for _, c := range checks {
		if c.differs {
			bad = append(bad, c.pointer)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %w at %v", ErrInvalidOptions, ErrUnwiredOption, bad)
	}
	return nil
}

func differs(set *float64, fixed float64) bool { return set != nil && *set != fixed }

// decodeLoose reads the members of an option the schema already validated; an absent option leaves out unchanged.
func decodeLoose(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// declaredRoles returns every role name the options declare, users and permission roles, sorted.
func (o options) declaredRoles() []string {
	return slices.Sorted(maps.Keys(o.allRoles()))
}

func (o options) allRoles() map[string]bool {
	all := map[string]bool{}
	for name := range o.Users {
		all[name] = true
	}
	for name := range o.Roles {
		all[name] = true
	}
	return all
}

func (o options) databaseNames() []string { return slices.Sorted(maps.Keys(o.Databases)) }
func (o options) userNames() []string     { return slices.Sorted(maps.Keys(o.Users)) }
func (o options) roleNames() []string     { return slices.Sorted(maps.Keys(o.Roles)) }

// declaredMembership returns the roles the options make name a member of.
func (o options) declaredMembership(name string) []string {
	if u, ok := o.Users[name]; ok {
		return slices.Clone(u.MemberOf)
	}
	return nil
}
