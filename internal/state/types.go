// Package state is the durable record of one environment: scope.json written once, state.json
// advanced under a generation fence, archived operations and cancel requests.
//
// State files are internal under stateVersion; they never become a published contract. Loading is
// strict: a newer version, an unknown field or an unknown enum value is ErrUnsupported and the
// bytes on disk are untouched.
package state

import (
	"encoding/json"
	"time"
)

// StateVersion is the only state and scope version this engine reads and writes.
const StateVersion = 1

// Problem is the stable part of a failure: a code and a category, never a message or a value.
type Problem struct {
	Code     string   `json:"code"`
	Category Category `json:"category"`
}

// Actor says who submitted an operation.
type Actor struct {
	Kind ActorKind `json:"kind"`
}

// Selection says which objects an operation changes.
type Selection struct {
	All           bool     `json:"all,omitempty"`
	Refs          []string `json:"refs,omitempty"`
	ExpandedScope bool     `json:"expandedScope,omitempty"`
}

// JobResult is the native result of a job invocation: the exit code of the job's own container, which
// is told apart from a failure of the engine to run the job.
type JobResult struct {
	ExitCode int `json:"exitCode"`
}

// Step is one planned or executed action of an operation.
type Step struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"`
	Object     string     `json:"object"`
	Status     StepStatus `json:"status"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Evidence   string     `json:"evidence,omitempty"`
	Error      *Problem   `json:"error,omitempty"`
	// Job is the native result of a job step; it is set once the job's container has exited.
	Job *JobResult `json:"job,omitempty"`
}

// Operation is the journal of one operation. While it sits in State it is the open operation.
type Operation struct {
	ID                OperationID  `json:"id"`
	Command           Command      `json:"command"`
	Selection         Selection    `json:"selection"`
	Revision          int          `json:"revision"`
	BasedOnGeneration int64        `json:"basedOnGeneration"`
	Status            OpStatus     `json:"status"`
	Continues         *OperationID `json:"continues,omitempty"`
	ContinuedBy       *OperationID `json:"continuedBy,omitempty"`
	Actor             Actor        `json:"actor"`
	CreatedAt         time.Time    `json:"createdAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
	FinishedAt        *time.Time   `json:"finishedAt,omitempty"`
	Steps             []Step       `json:"steps"`
	Error             *Problem     `json:"error,omitempty"`
}

// Credential records where a secret input lives, never its value.
type Credential struct {
	Provider string          `json:"provider"`
	Origin   Origin          `json:"origin"`
	Version  SecretVersionID `json:"version"`
}

// Accepted is the intent the last successful merge accepted.
type Accepted struct {
	Revision    int                   `json:"revision"`
	Model       json.RawMessage       `json:"model"`
	Topology    json.RawMessage       `json:"topology"`
	Inputs      map[string]string     `json:"inputs,omitempty"`
	Credentials map[string]Credential `json:"credentials,omitempty"`
	Intent      map[string]Intent     `json:"intent,omitempty"`
}

// PortMapping is one published port of a realized service.
type PortMapping struct {
	Container int    `json:"container"`
	Host      int    `json:"host"`
	HostIP    string `json:"hostIp"`
}

// Mapping ties a realization to one runtime object by name and ID.
type Mapping struct {
	Kind      string        `json:"kind"`
	Name      string        `json:"name"`
	ID        string        `json:"id,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
	Ports     []PortMapping `json:"ports,omitempty"`
}

// Artifact is the image a realization runs, recorded at first use.
type Artifact struct {
	Image   string `json:"image,omitempty"`
	ImageID string `json:"imageId,omitempty"`
}

// Realization is one object this scope created and the runtime objects that carry it.
type Realization struct {
	UID             RealizationID    `json:"uid"`
	AppliedRevision int              `json:"appliedRevision,omitempty"`
	SpecHash        string           `json:"specHash,omitempty"`
	State           RealizationState `json:"state"`
	Artifact        *Artifact        `json:"artifact,omitempty"`
	Mappings        []Mapping        `json:"mappings,omitempty"`
}

// State is the content of state.json.
type State struct {
	StateVersion int                    `json:"stateVersion"`
	Generation   int64                  `json:"generation"`
	ScopeUID     ScopeUID               `json:"scopeUid"`
	Accepted     *Accepted              `json:"accepted,omitempty"`
	Realizations map[string]Realization `json:"realizations,omitempty"`
	Operation    *Operation             `json:"operation,omitempty"`
}

// Scope is the content of scope.json, written once at the first mutating operation.
type Scope struct {
	StateVersion   int       `json:"stateVersion"`
	ScopeUID       ScopeUID  `json:"scopeUid"`
	Project        string    `json:"project"`
	Environment    string    `json:"environment"`
	DockerContext  string    `json:"dockerContext"`
	EngineID       string    `json:"engineId"`
	ComposeProject string    `json:"composeProject"`
	KeyID          string    `json:"keyId"`
	CreatedAt      time.Time `json:"createdAt"`
}

// CancelRequest is the content of cancel/<op-id>.json, written only by Cancel.
type CancelRequest struct {
	StateVersion int         `json:"stateVersion"`
	Operation    OperationID `json:"operation"`
	Actor        Actor       `json:"actor"`
	RequestedAt  time.Time   `json:"requestedAt"`
}

// Archived is the content of operations/<op-id>.json.
type Archived struct {
	StateVersion int       `json:"stateVersion"`
	ScopeUID     ScopeUID  `json:"scopeUid"`
	Operation    Operation `json:"operation"`
}
