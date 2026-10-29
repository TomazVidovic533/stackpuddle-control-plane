package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle/model"
)

// Snapshot is one accepted view of a project: the canonical model document and the topology that
// was derived from it for the environment.
type Snapshot struct {
	Model    json.RawMessage
	Topology Topology
}

// ResolveSelection turns object references into instance-scoped workload names. A workload
// reference names that workload; a module reference names every workload its instance contributes;
// a client reference names nothing a runtime runs.
func ResolveSelection(top Topology, refs []model.ObjectRef, expanded bool) (Selection, error) {
	sel := Selection{ExpandedScope: expanded}
	for _, ref := range refs {
		key := ref.Key().String()
		var matched bool
		switch ref.Kind() {
		case model.ObjectWorkload:
			if _, ok := top.Find(key); ok {
				sel.Names = append(sel.Names, key)
				matched = true
			}
		case model.ObjectModule:
			for _, w := range top.Workloads {
				if w.Instance == key {
					sel.Names = append(sel.Names, w.Name)
					matched = true
				}
			}
		case model.ObjectClient:
			matched = true
		}
		if !matched {
			return Selection{}, fmt.Errorf("%w: %s", ErrUnknownObject, ref)
		}
	}
	slices.Sort(sel.Names)
	sel.Names = slices.Compact(sel.Names)
	return sel, nil
}

// document is the part of a canonical model that Merge reads: the sections and the object list.
type document struct {
	rest       map[string]json.RawMessage
	spec       map[string]json.RawMessage
	objects    []json.RawMessage
	provenance map[string]json.RawMessage
}

func parseDocument(raw json.RawMessage) (document, error) {
	var d document
	if err := json.Unmarshal(raw, &d.rest); err != nil {
		return d, fmt.Errorf("plan: read model document: %w", err)
	}
	if err := json.Unmarshal(d.rest["spec"], &d.spec); err != nil {
		return d, fmt.Errorf("plan: read model spec: %w", err)
	}
	if o, ok := d.spec["objects"]; ok {
		if err := json.Unmarshal(o, &d.objects); err != nil {
			return d, fmt.Errorf("plan: read model objects: %w", err)
		}
	}
	if p, ok := d.rest["provenance"]; ok {
		if err := json.Unmarshal(p, &d.provenance); err != nil {
			return d, fmt.Errorf("plan: read model provenance: %w", err)
		}
	}
	return d, nil
}

type objectHead struct {
	Kind string `json:"kind"`
	Key  string `json:"key"`
}

func head(raw json.RawMessage) (objectHead, error) {
	var h objectHead
	if err := json.Unmarshal(raw, &h); err != nil {
		return h, fmt.Errorf("plan: read model object: %w", err)
	}
	return h, nil
}

// Kinds of declared objects in the canonical model.
const (
	kindWorkload = "Workload"
	kindInstance = "ModuleInstance"
)

func provenanceKey(h objectHead) string {
	switch h.Kind {
	case kindWorkload:
		return "workload/" + h.Key
	case kindInstance:
		return "module/" + h.Key
	}
	return "client/" + h.Key
}

// sectionsRequiringScope are the model sections whose change reaches every consumer.
func sectionsRequiringScope() []string {
	return []string{"environments", "inputs", "applications", "definitions", "bindings", "manifest", "lock"}
}

// Merge builds revision N+1 from the accepted snapshot, the candidate loaded from source and the
// selection. Only the selected objects and the prerequisites they need come from the candidate; the
// rest stays as accepted, so unselected source edits stay unapplied. A change to a shared section,
// or to a definition with unselected consumers, is refused with ErrExpandedScopeRequired unless the
// selection is All or carries the expanded scope. accepted is nil for the first revision.
func Merge(accepted *Snapshot, candidate Snapshot, sel Selection) (Snapshot, error) {
	if sel.All {
		return candidate, nil
	}
	selected, err := expandSelection(accepted, candidate, sel)
	if err != nil {
		return Snapshot{}, err
	}
	top := mergeTopology(accepted, candidate.Topology, selected)
	doc, err := mergeDocument(accepted, candidate, selected, sel)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Model: doc, Topology: top}, nil
}

// expandSelection adds the prerequisites of selected workloads and, when the scope is expanded, every
// instance that shares a changed definition. It refuses a changed shared definition otherwise.
func expandSelection(accepted *Snapshot, candidate Snapshot, sel Selection) (map[string]bool, error) {
	selected := map[string]bool{}
	for _, name := range sel.Names {
		selected[name] = true
	}
	if accepted != nil {
		shared := changedSharedDefinitions(accepted.Topology, candidate.Topology, selected)
		if len(shared) > 0 && !sel.ExpandedScope {
			return nil, fmt.Errorf("%w: definitions/%s is shared with unselected instances; select them or pass the expanded scope", ErrExpandedScopeRequired, shared[0])
		}
		for _, w := range candidate.Topology.Workloads {
			if slices.Contains(shared, w.Definition) {
				selected[w.Name] = true
			}
		}
	}
	addPrerequisites(candidate.Topology, selected)
	return selected, nil
}

// addPrerequisites closes the selection under the jobs that selected workloads require.
func addPrerequisites(top Topology, selected map[string]bool) {
	for changed := true; changed; {
		changed = false
		for _, w := range top.Workloads {
			if !selected[w.Name] {
				continue
			}
			for _, pre := range w.Prerequisites {
				if !selected[pre] {
					selected[pre] = true
					changed = true
				}
			}
		}
	}
}

// changedSharedDefinitions returns the definitions that have a selected instance and an unselected
// instance whose contributed workloads differ between the accepted and the candidate topology.
func changedSharedDefinitions(accepted, candidate Topology, selected map[string]bool) []string {
	var out []string
	for _, w := range candidate.Workloads {
		if w.Definition == "" || selected[w.Name] || slices.Contains(out, w.Definition) {
			continue
		}
		previous, ok := accepted.Find(w.Name)
		if !ok || reflect.DeepEqual(previous, w) {
			continue
		}
		if definitionHasSelected(candidate, w.Definition, selected) {
			out = append(out, w.Definition)
		}
	}
	slices.Sort(out)
	return out
}

func definitionHasSelected(top Topology, definition string, selected map[string]bool) bool {
	return slices.ContainsFunc(top.Workloads, func(w Workload) bool { return w.Definition == definition && selected[w.Name] })
}

func mergeTopology(accepted *Snapshot, candidate Topology, selected map[string]bool) Topology {
	instances := selectedInstances(candidate, selected)
	var out Topology
	if accepted != nil {
		out.Workloads = slices.DeleteFunc(slices.Clone(accepted.Topology.Workloads), func(w Workload) bool { return selected[w.Name] })
		out.Instances = slices.DeleteFunc(slices.Clone(accepted.Topology.Instances), func(i Instance) bool { return instances[i.Key] })
	}
	for _, w := range candidate.Workloads {
		if selected[w.Name] {
			out.Workloads = append(out.Workloads, w)
		}
	}
	for _, i := range candidate.Instances {
		if instances[i.Key] {
			out.Instances = append(out.Instances, i)
		}
	}
	slices.SortFunc(out.Workloads, func(a, b Workload) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(out.Instances, func(a, b Instance) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func mergeDocument(accepted *Snapshot, candidate Snapshot, selected map[string]bool, sel Selection) (json.RawMessage, error) {
	cand, err := parseDocument(candidate.Model)
	if err != nil {
		return nil, err
	}
	if accepted == nil {
		return filterDocument(cand, candidate.Topology, selected)
	}
	acc, err := parseDocument(accepted.Model)
	if err != nil {
		return nil, err
	}
	if err := checkSections(acc, cand, sel); err != nil {
		return nil, err
	}
	instanceSelected := selectedInstances(candidate.Topology, selected)
	out := document{rest: maps.Clone(cand.rest), spec: maps.Clone(cand.spec), provenance: map[string]json.RawMessage{}}
	merged, err := mergeObjects(acc, cand, selected, instanceSelected)
	if err != nil {
		return nil, err
	}
	out.objects = merged
	for _, raw := range out.objects {
		h, _ := head(raw)
		key := provenanceKey(h)
		if value, ok := cand.provenance[key]; ok && objectSelected(h, selected, instanceSelected) {
			out.provenance[key] = value
		} else if value, ok := acc.provenance[key]; ok {
			out.provenance[key] = value
		}
	}
	return encodeDocument(out)
}

// checkSections refuses a change to a section every consumer shares unless the scope is expanded.
func checkSections(acc, cand document, sel Selection) error {
	if sel.ExpandedScope {
		return nil
	}
	for _, section := range sectionsRequiringScope() {
		if !sameJSON(acc.spec[section], cand.spec[section]) {
			return fmt.Errorf("%w: the %s section changed; select with all or pass the expanded scope", ErrExpandedScopeRequired, section)
		}
	}
	return nil
}

func selectedInstances(top Topology, selected map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, w := range top.Workloads {
		if w.Instance != "" && selected[w.Name] {
			out[w.Instance] = true
		}
	}
	return out
}

func objectSelected(h objectHead, selected, instances map[string]bool) bool {
	switch h.Kind {
	case kindWorkload:
		return selected[h.Key]
	case kindInstance:
		return instances[h.Key]
	}
	return false
}

func mergeObjects(acc, cand document, selected, instances map[string]bool) ([]json.RawMessage, error) {
	var out []json.RawMessage
	taken := map[string]bool{}
	for _, raw := range acc.objects {
		h, err := head(raw)
		if err != nil {
			return nil, err
		}
		taken[h.Kind+"/"+h.Key] = true
		if !objectSelected(h, selected, instances) {
			out = append(out, raw)
		}
	}
	for _, raw := range cand.objects {
		h, err := head(raw)
		if err != nil {
			return nil, err
		}
		if objectSelected(h, selected, instances) {
			out = append(out, raw)
		}
	}
	return sortObjects(out)
}

// sortObjects orders objects by kind and key so the merged document is deterministic.
func sortObjects(objects []json.RawMessage) ([]json.RawMessage, error) {
	var failure error
	slices.SortFunc(objects, func(a, b json.RawMessage) int {
		ha, err := head(a)
		if err != nil {
			failure = err
		}
		hb, err := head(b)
		if err != nil {
			failure = err
		}
		return strings.Compare(ha.Kind+"/"+ha.Key, hb.Kind+"/"+hb.Key)
	})
	return objects, failure
}

func filterDocument(doc document, top Topology, selected map[string]bool) (json.RawMessage, error) {
	instances := selectedInstances(top, selected)
	if err := doc.keep(func(h objectHead) bool { return objectSelected(h, selected, instances) }); err != nil {
		return nil, err
	}
	return encodeDocument(doc)
}

func encodeDocument(doc document) (json.RawMessage, error) {
	spec := maps.Clone(doc.spec)
	if len(doc.objects) > 0 {
		raw, err := json.Marshal(doc.objects)
		if err != nil {
			return nil, fmt.Errorf("plan: encode objects: %w", err)
		}
		spec["objects"] = raw
	} else {
		delete(spec, "objects")
	}
	rest := maps.Clone(doc.rest)
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("plan: encode spec: %w", err)
	}
	rest["spec"] = rawSpec
	if len(doc.provenance) > 0 {
		raw, err := json.Marshal(doc.provenance)
		if err != nil {
			return nil, fmt.Errorf("plan: encode provenance: %w", err)
		}
		rest["provenance"] = raw
	} else {
		delete(rest, "provenance")
	}
	out, err := json.Marshal(rest)
	if err != nil {
		return nil, fmt.Errorf("plan: encode model: %w", err)
	}
	return out, nil
}

func sameJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

// Without returns the snapshot with the named workloads removed: their topology entries, the
// declared objects that only they carried, and instances left with no workload. It is how a
// destroyed object leaves the accepted intent.
func Without(s Snapshot, names []string) (Snapshot, error) {
	top, live := withoutTopology(s.Topology, names)
	doc, err := parseDocument(s.Model)
	if err != nil {
		return Snapshot{}, err
	}
	if err := doc.keep(func(h objectHead) bool {
		return !(h.Kind == kindWorkload && slices.Contains(names, h.Key)) && !(h.Kind == kindInstance && !live[h.Key])
	}); err != nil {
		return Snapshot{}, err
	}
	encoded, err := encodeDocument(doc)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Model: encoded, Topology: top}, nil
}

// withoutTopology drops the named workloads and the instances left with none, and reports which
// instances remain.
func withoutTopology(t Topology, names []string) (Topology, map[string]bool) {
	var top Topology
	live := map[string]bool{}
	for _, w := range t.Workloads {
		if !slices.Contains(names, w.Name) {
			top.Workloads = append(top.Workloads, w)
			live[w.Instance] = true
		}
	}
	for _, i := range t.Instances {
		if live[i.Key] {
			top.Instances = append(top.Instances, i)
		}
	}
	return top, live
}

// keep drops the objects (and their provenance) for which keep returns false.
func (d *document) keep(keep func(objectHead) bool) error {
	kept := make([]json.RawMessage, 0, len(d.objects))
	provenance := map[string]json.RawMessage{}
	for _, raw := range d.objects {
		h, err := head(raw)
		if err != nil {
			return err
		}
		if !keep(h) {
			continue
		}
		kept = append(kept, raw)
		if value, ok := d.provenance[provenanceKey(h)]; ok {
			provenance[provenanceKey(h)] = value
		}
	}
	d.objects, d.provenance = kept, provenance
	return nil
}
