// Package module is the driver port: the contract a module's built-in driver implements today and a
// staged container runner implements later.
//
// Only plain data crosses it: strings, numbers, json.RawMessage, slices and structs with JSON tags.
// No function, channel, interface value or handle crosses, so a driver never holds a Docker handle
// and an out-of-process runner can speak the same contract. A driver asks for runtime work as
// Probes; the engine runs each probe through its runtime adapter and calls the driver again with
// the Results, at most MaxProbeRounds times.
package module

import (
	"context"
	"encoding/json"
)

// MaxProbeRounds bounds the request/probe loop of one driver call.
const MaxProbeRounds = 8

// Probe kinds.
const (
	// ProbeExec runs a command inside a running container of the scope.
	ProbeExec = "exec"
	// ProbeOneShot runs a throwaway container on the scope network.
	ProbeOneShot = "one-shot"
)

// Driver implements a module's responsibilities. Delete is a capability of Describe, not a method.
type Driver interface {
	// Describe returns the static description of the module.
	Describe() Descriptor
	// Observe reports what exists for one instance.
	Observe(ctx context.Context, r ObserveRequest) (ObserveResponse, error)
	// Plan returns the steps that bring one instance to its options.
	Plan(ctx context.Context, r PlanRequest) (PlanResponse, error)
	// Apply performs one step the driver planned.
	Apply(ctx context.Context, r ApplyRequest) (ApplyResponse, error)
	// Verify checks that one instance meets its options.
	Verify(ctx context.Context, r VerifyRequest) (VerifyResponse, error)
	// Bind returns the values one binding of an Export delivers. It runs no probe and has no effect,
	// so it is safe at plan time; parameters a driver does not accept are refused here, before any
	// effect.
	Bind(ctx context.Context, r BindRequest) (BindResponse, error)
}

// Descriptor describes a module's driver.
type Descriptor struct {
	Module       string   `json:"module"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	StateKinds   []string `json:"stateKinds"`
	ActionKinds  []string `json:"actionKinds"`
	Deletes      bool     `json:"deletes"`
	// ServerTLS lists the workloads that need a server certificate and a private key delivered to them.
	ServerTLS []ServerTLS `json:"serverTls,omitempty"`
}

// ServerTLS names a workload of the module that serves encrypted connections: the engine issues its
// certificate and delivers the certificate and the key to the fixed targets ServerCertPath and
// ServerKeyPath, owned by Owner.
type ServerTLS struct {
	// Workload is the module's workload key, such as "server".
	Workload string `json:"workload"`
	// Listener is the workload's listener the certificate protects.
	Listener string `json:"listener"`
	// Owner is the "<uid>:<gid>" that owns both files inside the container.
	Owner string `json:"owner"`
}

// The fixed in-container targets of a server certificate and key. Module content refers to them.
const (
	ServerCertPath = "/run/stackpuddle/tls/server.crt"
	ServerKeyPath  = "/run/stackpuddle/tls/server.key"
)

// Instance is one declared use of a module with its effective options.
type Instance struct {
	Key        string          `json:"key"`
	Definition string          `json:"definition"`
	Version    string          `json:"version"`
	Options    json.RawMessage `json:"options"`
	// Scope is the uid of the scope the instance runs in; a driver puts it in the ownership markers
	// it writes, so an object created by another scope is never taken for its own.
	Scope string `json:"scope"`
}

// Mapping ties an instance to one runtime object it owns.
type Mapping struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

// Secret binding forms.
const (
	// FormValue binds the secret's own bytes. It is the default, written as an empty form.
	FormValue = ""
	// FormSCRAMSHA256 binds the RFC 5802 SCRAM-SHA-256 verifier of the secret, computed by the engine,
	// so a driver can set a password without ever holding the plaintext.
	FormSCRAMSHA256 = "scram-sha-256"
)

// SecretBinding names a secret the engine puts into a probe's child environment only.
type SecretBinding struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	// Form is FormValue or FormSCRAMSHA256; any other form is refused before the probe runs.
	Form string `json:"form,omitempty"`
}

// Probe is one runtime primitive as data.
type Probe struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target"`
	User      string          `json:"user,omitempty"`
	Argv      []string        `json:"argv"`
	Stdin     string          `json:"stdin,omitempty"`
	SecretEnv []SecretBinding `json:"secretEnv,omitempty"`
}

// ProbeResult is what the engine learned from running one probe.
type ProbeResult struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// SecretNeed asks the engine to resolve or create a secret before the step runs.
type SecretNeed struct {
	Ref  string `json:"ref"`
	Size int    `json:"size"`
}

// Step is one planned action. Its action is namespaced by the module, such as "<module>.role".
type Step struct {
	Action      string          `json:"action"`
	Object      string          `json:"object"`
	Destructive bool            `json:"destructive,omitempty"`
	Secret      *SecretNeed     `json:"secret,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
}

// Observation is what a driver found for one instance.
type Observation struct {
	Present bool              `json:"present"`
	Facts   map[string]string `json:"facts,omitempty"`
}

// Outcome is what a driver did for one step.
type Outcome struct {
	Applied  bool      `json:"applied"`
	Evidence string    `json:"evidence,omitempty"`
	Mappings []Mapping `json:"mappings,omitempty"`
}

// Verification is a driver's verdict on one instance.
type Verification struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems,omitempty"`
}

// ObserveRequest asks a driver what exists.
type ObserveRequest struct {
	Instance Instance      `json:"instance"`
	Mappings []Mapping     `json:"mappings,omitempty"`
	Results  []ProbeResult `json:"results,omitempty"`
}

// ObserveResponse either asks for Probes or carries the final Observation.
type ObserveResponse struct {
	Probes      []Probe      `json:"probes,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
}

// PlanRequest asks a driver for the steps of one instance.
type PlanRequest struct {
	Instance    Instance      `json:"instance"`
	Mappings    []Mapping     `json:"mappings,omitempty"`
	Observation Observation   `json:"observation"`
	Results     []ProbeResult `json:"results,omitempty"`
}

// PlanResponse either asks for Probes or carries the final Steps.
type PlanResponse struct {
	Probes []Probe `json:"probes,omitempty"`
	Steps  []Step  `json:"steps,omitempty"`
}

// ApplyRequest asks a driver to perform one step.
type ApplyRequest struct {
	Instance Instance      `json:"instance"`
	Step     Step          `json:"step"`
	Results  []ProbeResult `json:"results,omitempty"`
}

// ApplyResponse either asks for Probes or carries the final Outcome.
type ApplyResponse struct {
	Probes  []Probe  `json:"probes,omitempty"`
	Outcome *Outcome `json:"outcome,omitempty"`
}

// VerifyRequest asks a driver to check one instance.
type VerifyRequest struct {
	Instance Instance      `json:"instance"`
	Mappings []Mapping     `json:"mappings,omitempty"`
	Results  []ProbeResult `json:"results,omitempty"`
}

// VerifyResponse either asks for Probes or carries the final Verification.
type VerifyResponse struct {
	Probes       []Probe       `json:"probes,omitempty"`
	Verification *Verification `json:"verification,omitempty"`
}

// MaterialTLSCA is the material a bound value names for the CA certificate of the environment; the
// engine delivers its bytes, the driver never holds them.
const MaterialTLSCA = "tls-ca"

// BindRequest asks a driver for the values of one Export of an instance. Parameters are the
// module-defined binding parameters, untrusted input the driver validates.
type BindRequest struct {
	Instance   Instance        `json:"instance"`
	Export     string          `json:"export"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// BoundValue is one declared value of an Export. Exactly one of Text, SecretRef or Material is set:
// Text is public text, SecretRef names a secret in the instance's namespace, Material names engine
// material such as MaterialTLSCA.
type BoundValue struct {
	Name      string `json:"name"`
	Text      string `json:"text,omitempty"`
	SecretRef string `json:"secretRef,omitempty"`
	Material  string `json:"material,omitempty"`
}

// BindResponse carries the values of the Export; "host" and "port" are never among them.
type BindResponse struct {
	Values []BoundValue `json:"values"`
}
