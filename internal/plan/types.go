// Package plan is the pure core of an operation: it turns a project into a runtime-neutral
// Topology, merges a selection into the accepted intent, and diffs the intent against what the
// state and the runtime inventory say exists.
//
// Nothing here reads the clock, the environment, the filesystem or a process. Every runtime adapter
// translates the same Topology, so adding a runtime never changes this package.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Workload kinds.
const (
	// TypeService runs until it is stopped.
	TypeService = "service"
	// TypeJob runs to completion.
	TypeJob = "job"
)

// Build is a declared image build. Context is relative to the project directory.
type Build struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Target     string `json:"target,omitempty"`
}

// EnvVar is one environment variable with its literal text. Secret values never appear here.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Port is one published port; Host 0 means the runtime assigns it.
type Port struct {
	Container int `json:"container"`
	Host      int `json:"host,omitempty"`
}

// VolumeMount mounts a named volume into a workload.
type VolumeMount struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SourceKind says where the bytes of a delivered file come from. The set is closed.
type SourceKind int

// The source kinds.
const (
	// SourceSecret is a secret of the sealed store, named by its reference.
	SourceSecret SourceKind = iota + 1
	// SourceText is public literal text.
	SourceText
	// SourceMaterial is engine material, such as a certificate, named by its reference.
	SourceMaterial
)

func sourceKindNames() []string { return []string{"secret", "text", "material"} }

// String returns the wire name, or "" for an invalid value.
func (k SourceKind) String() string {
	if k < 1 || int(k) > len(sourceKindNames()) {
		return ""
	}
	return sourceKindNames()[k-1]
}

// MarshalText implements encoding.TextMarshaler.
func (k SourceKind) MarshalText() ([]byte, error) {
	if k.String() == "" {
		return nil, fmt.Errorf("plan: unknown source kind %d", int(k))
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler and refuses an unknown name.
func (k *SourceKind) UnmarshalText(text []byte) error {
	for i, name := range sourceKindNames() {
		if name == string(text) {
			*k = SourceKind(i + 1)
			return nil
		}
	}
	return fmt.Errorf("plan: unknown source kind %q", text)
}

// Source is where a delivered file's bytes come from. The topology stores the source and its
// reference only, never a secret value: it is persisted in the accepted state.
type Source struct {
	Kind SourceKind `json:"kind"`
	// Ref names the secret or the material; empty for text.
	Ref string `json:"ref,omitempty"`
	// Text is the public literal of a text source.
	Text string `json:"text,omitempty"`
}

// Key identifies the source inside one rendered project and one call: the reference of a secret, a
// prefixed reference for material, and a digest of the text for a literal.
func (s Source) Key() string {
	switch s.Kind {
	case SourceSecret:
		return s.Ref
	case SourceMaterial:
		return "material:" + s.Ref
	case SourceText:
		sum := sha256.Sum256([]byte(s.Text))
		return "text:" + hex.EncodeToString(sum[:])[:sourceDigestChars]
	}
	return ""
}

const sourceDigestChars = 16

// File delivers one value into a workload as a file the runtime copies in with the declared owner and
// mode, never from a host file or a bind mount.
type File struct {
	Path string `json:"path"`
	// Mode is the file mode in the container; zero means owner read-only (0400).
	Mode uint32 `json:"mode,omitempty"`
	// Owner is "<uid>:<gid>"; empty leaves the runtime default.
	Owner  string `json:"owner,omitempty"`
	Source Source `json:"source"`
}

// Workload is one container workload under its instance-scoped name.
type Workload struct {
	Name          string        `json:"name"`
	Instance      string        `json:"instance,omitempty"`
	Definition    string        `json:"definition,omitempty"`
	Type          string        `json:"type"`
	Image         string        `json:"image,omitempty"`
	Build         *Build        `json:"build,omitempty"`
	Command       []string      `json:"command,omitempty"`
	Env           []EnvVar      `json:"env,omitempty"`
	Ports         []Port        `json:"ports,omitempty"`
	Volumes       []VolumeMount `json:"volumes,omitempty"`
	Files         []File        `json:"files,omitempty"`
	Prerequisites []string      `json:"prerequisites,omitempty"`
	// Provider is true for a workload other workloads bind to or that a module contributes: it comes
	// up before the jobs and services that consume it.
	Provider bool `json:"provider,omitempty"`
}

// Instance is one declared module instance with its effective options, for the module's driver.
type Instance struct {
	Key        string          `json:"key"`
	Definition string          `json:"definition"`
	Version    string          `json:"version"`
	Options    json.RawMessage `json:"options"`
}

// Topology is the runtime-neutral desired state of one environment.
type Topology struct {
	Workloads []Workload `json:"workloads"`
	Instances []Instance `json:"instances,omitempty"`
}

// Find returns the workload with the given instance-scoped name.
func (t Topology) Find(name string) (Workload, bool) {
	for _, w := range t.Workloads {
		if w.Name == name {
			return w, true
		}
	}
	return Workload{}, false
}

// Selection says which workloads an operation targets.
type Selection struct {
	// All selects every workload.
	All bool
	// Names are the instance-scoped workload names selected by reference (applications and module
	// instances already expanded).
	Names []string
	// ExpandedScope permits a change to a shared resource with unselected consumers.
	ExpandedScope bool
}

// ScopeNames are the identifiers every runtime object of one scope carries.
type ScopeNames struct {
	// ScopeUID labels every object; it is the ownership marker.
	ScopeUID string
	// ComposeProject is <project>-<environment>-<suffix>.
	ComposeProject string
	// ProjectDir is the absolute project directory builds resolve against.
	ProjectDir string
	// Realizations maps a realization key such as "service/api" to its realization ID.
	Realizations map[string]string
	// Artifacts maps a workload name to the recorded image, which replaces its declared image.
	Artifacts map[string]state.Artifact
	// Delivered maps a source key to the version token of what it currently delivers: the version ID
	// of a secret, the fingerprint of a certificate. A source with no token yet is absent. Tokens are
	// never values, so a new version changes the rendered service without the value entering it.
	Delivered map[string]string
}

// Labels every runtime object of a scope carries.
const (
	// LabelScope holds the scope UID.
	LabelScope = "io.stackpuddle.scope"
	// LabelRealization holds the realization ID.
	LabelRealization = "io.stackpuddle.realization"
	// LabelSpec holds the hash of the rendered service, so a changed spec is visible in observation.
	LabelSpec = "io.stackpuddle.spec"
	// LabelFiles holds the hash of the versions of the files delivered into the workload, so a rotated
	// secret or a reissued certificate changes the spec hash and restarts it.
	LabelFiles = "io.stackpuddle.files"
)

// Object is one runtime object found by observation.
type Object struct {
	Kind        string
	Name        string
	ID          string
	Realization string
	CreatedAt   string
}

// Container is an observed container.
type Container struct {
	Object
	Service  string
	Running  bool
	ExitCode int
	SpecHash string
	Ports    []state.PortMapping
}

// Inventory is what observation found carrying one scope's label.
type Inventory struct {
	Volumes    []Object
	Containers []Container
}

// Rendered is the runtime-specific text of one topology, plus the secret variable each reference uses.
type Rendered struct {
	// Compose is the rendered project file.
	Compose []byte
	// SecretVars maps a source key to the child environment variable that carries it.
	SecretVars map[string]string
	// Literals maps the key of a text source to its public text.
	Literals map[string]string
	// SpecHashes maps a workload name to the hash of its rendered service.
	SpecHashes map[string]string
}

// Evidence is what an effect proved it did.
type Evidence struct {
	// Text is a short, secret-free line for the journal.
	Text string
	// Artifact is the image an image effect recorded.
	Artifact *state.Artifact
	// Job is the native result of a job effect: set whenever the job's own container ran to its end,
	// whatever its exit code. A job that could not be run is an error instead.
	Job *state.JobResult
}

// TargetInfo identifies the engine a Docker context reaches.
type TargetInfo struct {
	EngineID      string
	ServerVersion string
}

// ErrOutcomeUnknown marks an effect that outlived its budget or lost its child: it may or may not
// have happened, so the step stays started and the next run settles it by observation.
var ErrOutcomeUnknown = errors.New("effect outcome is unknown")
