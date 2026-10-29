package operation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// session is one Submit holding the environment lock. It is used by one goroutine.
type session struct {
	e        *Engine
	req      Request
	prep     prepared
	env      *state.Env
	held     *lock.Held
	store    secrets.Store
	scope    state.Scope
	hasScope bool
	newUID   state.ScopeUID
	st       state.State
	hasState bool
	top      plan.Topology
	steps    []plan.Step
	// mergedModel is the canonical model document of the revision being applied.
	mergedModel json.RawMessage
	// hashes are the spec hashes of the rendered topology.
	hashes map[string]string
	// inv is the latest observed inventory.
	inv plan.Inventory
	// nextID is the ID of the operation that continues a recovered one, minted during recovery.
	nextID state.OperationID
	// recovered is the interrupted operation begin archives, naming nextID, in the commit that writes its continuation.
	recovered *state.Operation
	// last is the closed operation, for the result.
	last *state.Operation
	// verified are the module instances a verify step has already checked in this operation.
	verified map[string]bool
}

func jsonOf(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("operation: encode: %w", err)
	}
	return data, nil
}

func (s *session) uid() state.ScopeUID {
	if s.hasScope {
		return s.scope.ScopeUID
	}
	return s.newUID
}

// names builds the identifiers every runtime object of this scope carries from the scope and the
// realizations recorded so far.
func (s *session) names() plan.ScopeNames {
	n := scopeNames(s.scope, s.uid(), s.req.ProjectDir, s.st)
	for _, w := range s.top.Workloads {
		for _, f := range w.Files {
			if f.Source.Kind != plan.SourceMaterial {
				continue
			}
			if data, ok := s.material(f.Source.Ref); ok {
				n.Delivered[f.Source.Key()] = fingerprint(data)
			}
		}
	}
	return n
}

// fingerprint is the version token of a public certificate: a short digest of its bytes.
func fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:fingerprintChars]
}

const fingerprintChars = 16

// material reads the public file a material reference names from the environment's tls folder. ok is
// false when the environment has no such file yet, or none at all, as in a dry run of a new project.
func (s *session) material(ref string) ([]byte, bool) {
	if s.env == nil {
		return nil, false
	}
	data, ok, err := s.env.ReadMaterial(plan.MaterialFile(ref))
	if err != nil || !ok {
		return nil, false
	}
	return data, true
}

func scopeNames(scope state.Scope, uid state.ScopeUID, projectDir string, st state.State) plan.ScopeNames {
	n := plan.ScopeNames{
		ScopeUID: uid.String(), ComposeProject: scope.ComposeProject, ProjectDir: projectDir,
		Realizations: map[string]string{}, Artifacts: map[string]state.Artifact{}, Delivered: map[string]string{},
	}
	if st.Accepted != nil {
		for ref, cred := range st.Accepted.Credentials {
			n.Delivered[ref] = cred.Version.String()
		}
	}
	for key, r := range st.Realizations {
		n.Realizations[key] = r.UID.String()
		if r.Artifact != nil {
			if _, name, ok := strings.Cut(key, "/"); ok {
				n.Artifacts[name] = *r.Artifact
			}
		}
	}
	return n
}

// call builds the runtime call for the topology the session currently acts on. Secrets named in refs
// are resolved from the store for this call's child environment only.
func (s *session) call(ctx context.Context, refs []string) (port.Call, error) {
	call := port.Call{Scope: s.names(), DockerContext: s.prep.dockerContext, Topology: s.top, Secrets: map[string]port.SecretValue{}}
	if s.held != nil {
		call.LockFile = s.held.File()
		call.OnChild = s.noteChild
	}
	for _, ref := range refs {
		value, err := s.secretValue(ctx, ref)
		if err != nil {
			return port.Call{}, err
		}
		call.Secrets[ref] = value
	}
	return call, nil
}

func (s *session) secretValue(ctx context.Context, ref string) (secrets.Value, error) {
	parsed, err := secrets.ParseRef(ref)
	if err != nil {
		return secrets.Value{}, err
	}
	var cred state.Credential
	if s.st.Accepted != nil {
		cred = s.st.Accepted.Credentials[ref]
	}
	if cred.Version.String() == "" {
		return secrets.Value{}, fmt.Errorf("%w: %s has no recorded version", secrets.ErrSecretMissing, ref)
	}
	return s.store.Resolve(ctx, parsed, cred.Version)
}

func (s *session) noteChild(pid int) {
	if s.held == nil || s.st.Operation == nil {
		return
	}
	host, _ := os.Hostname()
	_ = s.held.SetOwner(lock.Owner{Operation: s.st.Operation.ID.String(), PID: os.Getpid(), ChildPID: pid, Host: host})
}

// commit writes the state as the next generation and tells the caller.
func (s *session) commit() error {
	written, err := s.env.Commit(s.st.Generation, s.st)
	if err != nil {
		return err
	}
	s.st = written
	s.hasState = true
	if s.req.Progress != nil && s.st.Operation != nil {
		s.req.Progress(*s.st.Operation, s.uid().String())
	}
	return nil
}

// accepted returns the accepted snapshot, or nil before the first revision.
func (s *session) accepted() (*plan.Snapshot, error) {
	if s.st.Accepted == nil {
		return nil, nil //nolint:nilnil // reason: nil with no error is the documented "no accepted revision yet"
	}
	var top plan.Topology
	if err := json.Unmarshal(s.st.Accepted.Topology, &top); err != nil {
		return nil, fmt.Errorf("operation: read accepted topology: %w", err)
	}
	return &plan.Snapshot{Model: s.st.Accepted.Model, Topology: top}, nil
}

// ensureRealization mints the realization ID of key before its first effect, so the label an
// effect writes is already in the journal when the effect runs.
func (s *session) ensureRealization(key string) error {
	if _, ok := s.st.Realizations[key]; ok {
		return nil
	}
	id, err := state.NewRealizationID(s.e.now(), s.e.d.Random)
	if err != nil {
		return err
	}
	if s.st.Realizations == nil {
		s.st.Realizations = map[string]state.Realization{}
	}
	s.st.Realizations[key] = state.Realization{UID: id, State: state.RealizationActive}
	return nil
}

func (s *session) update(key string, change func(r *state.Realization)) {
	r := s.st.Realizations[key]
	change(&r)
	s.st.Realizations[key] = r
}

func (s *session) observe(ctx context.Context) (plan.Inventory, error) {
	if s.uid().String() == "" || !s.hasScope {
		return plan.Inventory{}, nil
	}
	call, err := s.call(ctx, nil)
	if err != nil {
		return plan.Inventory{}, err
	}
	inv, err := s.e.d.Runtime.Observe(ctx, call)
	if err != nil {
		return plan.Inventory{}, fail(state.CodeTargetUnreachable, state.CategoryTransient, "observe the runtime").wrap(err)
	}
	return inv, nil
}

func (s *session) render() (plan.Rendered, error) {
	return s.e.d.Runtime.Render(s.names(), s.top)
}
