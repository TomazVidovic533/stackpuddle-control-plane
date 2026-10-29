package operation

import (
	"fmt"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// fold records in state what an applied effect did, from the evidence it returned (nil when the
// effect is settled by observation) and the inventory observed afterwards.
func (s *session) fold(e plan.Effect, ev *plan.Evidence, inv plan.Inventory) error {
	key := s.effectKey(e)
	switch effect := e.(type) {
	case plan.ResolveImage, plan.BuildImage:
		s.foldImage(key, ev)
	case plan.CreateVolume:
		s.foldVolume(key, effect.Volume, inv)
	case plan.StartService:
		return s.foldWorkload(workloadFold{key: key, workload: effect.Workload, kind: "compose.service", intent: state.IntentRunning}, inv)
	case plan.RunJob:
		return s.foldWorkload(workloadFold{key: key, workload: effect.Workload, kind: "compose.job"}, inv)
	case plan.StopService:
		s.setIntent(effect.Workload, state.IntentStopped)
	case plan.RemoveService:
		delete(s.st.Realizations, key)
		if s.st.Accepted != nil {
			delete(s.st.Accepted.Intent, effect.Workload)
		}
	case plan.DeleteVolume:
		delete(s.st.Realizations, key)
	}
	return nil
}

func (s *session) revision() int {
	if s.st.Accepted == nil {
		return 0
	}
	return s.st.Accepted.Revision
}

func (s *session) foldImage(key string, ev *plan.Evidence) {
	if ev == nil || ev.Artifact == nil {
		return
	}
	artifact := *ev.Artifact
	s.update(key, func(r *state.Realization) { r.Artifact = &artifact })
}

func (s *session) foldVolume(key, volume string, inv plan.Inventory) {
	revision := s.revision()
	s.update(key, func(r *state.Realization) {
		r.AppliedRevision = revision
		for _, o := range inv.Volumes {
			if o.Name == volume {
				r.Mappings = []state.Mapping{{Kind: "compose.volume", Name: o.ID, ID: o.ID, CreatedAt: s.e.now()}}
			}
		}
	})
}

// workloadFold names what a started service or finished job leaves in state.
type workloadFold struct {
	key      string
	workload string
	kind     string
	// intent is the accepted intent to record; zero records none (a job has no intent).
	intent state.Intent
}

func (s *session) foldWorkload(f workloadFold, inv plan.Inventory) error {
	key, workload, kind, intent := f.key, f.workload, f.kind, f.intent
	hashes, err := s.currentHashes()
	if err != nil {
		return err
	}
	revision := s.revision()
	s.update(key, func(r *state.Realization) {
		r.AppliedRevision, r.SpecHash, r.Mappings = revision, hashes[workload], nil
		for _, c := range inv.Containers {
			if c.Service == workload {
				r.Mappings = append(r.Mappings, state.Mapping{Kind: kind, Name: c.Name, ID: c.ID, CreatedAt: s.e.now(), Ports: c.Ports})
			}
		}
		slices.SortFunc(r.Mappings, func(a, b state.Mapping) int { return strings.Compare(a.Name, b.Name) })
	})
	if intent != 0 {
		s.setIntent(workload, intent)
	}
	return nil
}

func (s *session) setIntent(workload string, intent state.Intent) {
	if s.st.Accepted == nil {
		return
	}
	if s.st.Accepted.Intent == nil {
		s.st.Accepted.Intent = map[string]state.Intent{}
	}
	s.st.Accepted.Intent[workload] = intent
}

// currentHashes renders the topology with the realizations and artifacts recorded so far, so the hash
// stored with a realization is the one observation will see on the container.
func (s *session) currentHashes() (map[string]string, error) {
	rendered, err := s.render()
	if err != nil {
		return nil, fmt.Errorf("operation: render the topology: %w", err)
	}
	s.hashes = rendered.SpecHashes
	return rendered.SpecHashes, nil
}
