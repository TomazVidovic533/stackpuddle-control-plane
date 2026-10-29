package engine

import (
	"context"
	"sort"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
)

// InspectRequest says what to inspect. Build it with InspectEnvironment or InspectOperation.
type InspectRequest struct {
	scope Scope
	id    OperationID
	op    bool
}

// InspectEnvironment inspects the state of one environment.
func InspectEnvironment(s Scope) InspectRequest { return InspectRequest{scope: s} }

// InspectOperation inspects one operation of an environment, open or archived.
func InspectOperation(s Scope, id OperationID) InspectRequest {
	return InspectRequest{scope: s, id: id, op: true}
}

// Mapping is one runtime object that carries a realized object.
type Mapping struct {
	Kind string
	Name string
	ID   string
}

// Object is one realized object of an environment.
type Object struct {
	Ref      string
	State    string
	Mappings []Mapping
}

// EnvironmentView is the lock-free picture of one environment.
type EnvironmentView struct {
	scopeUID       string
	composeProject string
	generation     int64
	revision       int
	objects        []Object
	open           *Operation
}

// ScopeUID returns the scope uid, "" before the first mutating operation.
func (v EnvironmentView) ScopeUID() string { return v.scopeUID }

// ComposeProject returns the Compose project name, "" before the first mutating operation.
func (v EnvironmentView) ComposeProject() string { return v.composeProject }

// Generation returns the state generation.
func (v EnvironmentView) Generation() int64 { return v.generation }

// Revision returns the accepted revision, 0 before the first.
func (v EnvironmentView) Revision() int { return v.revision }

// Objects returns the realized objects sorted by reference. The slice is a copy.
func (v EnvironmentView) Objects() []Object { return append([]Object(nil), v.objects...) }

// OpenOperation returns the operation that is running or was left open.
func (v EnvironmentView) OpenOperation() (Operation, bool) {
	if v.open == nil {
		return Operation{}, false
	}
	return *v.open, true
}

// Inspection is the result of Inspect.
type Inspection struct {
	env *EnvironmentView
	op  *Operation
}

// Environment returns the environment view; ok is false for an operation inspection.
func (i Inspection) Environment() (EnvironmentView, bool) {
	if i.env == nil {
		return EnvironmentView{}, false
	}
	return *i.env, true
}

// Operation returns the inspected operation; ok is false for an environment inspection.
func (i Inspection) Operation() (Operation, bool) {
	if i.op == nil {
		return Operation{}, false
	}
	return *i.op, true
}

// Inspect reads state without the lock and writes nothing, so it works while another operation runs
// and needs no secrets key.
func (e *Engine) Inspect(ctx context.Context, req InspectRequest) (Inspection, error) {
	scope := operation.Scope(req.scope)
	if req.op {
		op, uid, err := e.core.InspectOperation(ctx, scope, req.id)
		if err != nil {
			return Inspection{}, toPublic(err)
		}
		view := withScopeUID(newOperation(req.scope, op), uid)
		return Inspection{op: &view}, nil
	}
	view, err := e.core.InspectEnvironment(ctx, scope)
	if err != nil {
		return Inspection{}, toPublic(err)
	}
	out := EnvironmentView{
		scopeUID: view.Scope.ScopeUID.String(), composeProject: view.Scope.ComposeProject,
		generation: view.Generation, revision: view.Revision,
	}
	for ref, r := range view.Realizations {
		object := Object{Ref: ref, State: r.State.String()}
		for _, m := range r.Mappings {
			object.Mappings = append(object.Mappings, Mapping{Kind: m.Kind, Name: m.Name, ID: m.ID})
		}
		out.objects = append(out.objects, object)
	}
	sort.Slice(out.objects, func(i, j int) bool { return out.objects[i].Ref < out.objects[j].Ref })
	if view.Open != nil {
		op := withScopeUID(newOperation(req.scope, *view.Open), out.scopeUID)
		out.open = &op
	}
	return Inspection{env: &out}, nil
}
