// Package port holds the types of one call across the runtime port: what the engine hands a runtime
// adapter besides the plan's pure data. They carry an open file and a callback, so they do not belong
// to the pure plan package.
package port

import (
	"os"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
)

// SecretValue is a secret a runtime puts into a child's environment only. secrets.Value satisfies it.
type SecretValue interface {
	// Reveal returns the secret bytes.
	Reveal() []byte
}

// Call is everything a runtime adapter needs for one call.
type Call struct {
	// Scope names every object of the scope.
	Scope plan.ScopeNames
	// DockerContext is the declared context; every child targets it and nothing else.
	DockerContext string
	// Topology is the desired state the call acts on.
	Topology plan.Topology
	// Secrets maps a secret reference to its value, for this call's child environment only.
	Secrets map[string]SecretValue
	// Files maps the key of each public material source (a certificate) to its bytes, for this call's
	// child environment only. Secrets are never here, so a scrub of Secrets covers every secret.
	Files map[string][]byte
	// LockFile is the held environment lock; children inherit it so a live effect keeps the environment busy.
	LockFile *os.File
	// OnChild is called with the PID of each child as it starts, for the lock owner record.
	OnChild func(pid int)
}

// SecretKey returns the key under which Call.Secrets holds the secret ref in the given binding form:
// the reference itself for the secret's own bytes, the reference and the form for a derived value.
func SecretKey(ref, form string) string {
	if form == "" {
		return ref
	}
	return ref + "#" + form
}
