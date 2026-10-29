package operation

import (
	"context"
	"errors"
	"io/fs"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// EnvironmentView is the lock-free picture of one environment.
type EnvironmentView struct {
	Scope        state.Scope
	HasScope     bool
	Generation   int64
	Revision     int
	Realizations map[string]state.Realization
	Open         *state.Operation
}

// InspectEnvironment reads scope.json and state.json without the lock and writes nothing, so it
// works while another operation holds the environment and needs no secrets key. An environment
// that was never created gives an empty view.
func (e *Engine) InspectEnvironment(_ context.Context, scope Scope) (EnvironmentView, error) {
	env, err := state.OpenEnv(e.d.Settings.StateDir(), scope.Project.String(), scope.Environment.String(), e.d.State)
	if errors.Is(err, fs.ErrNotExist) {
		return EnvironmentView{}, nil
	}
	if err != nil {
		return EnvironmentView{}, err
	}
	defer env.Close()
	sc, hasScope, err := env.LoadScope()
	if err != nil {
		return EnvironmentView{}, err
	}
	st, _, err := env.Load()
	if err != nil {
		return EnvironmentView{}, err
	}
	view := EnvironmentView{Scope: sc, HasScope: hasScope, Generation: st.Generation, Realizations: st.Realizations, Open: st.Operation}
	if st.Accepted != nil {
		view.Revision = st.Accepted.Revision
	}
	return view, nil
}

// InspectOperation returns the open operation or an archived one by ID.
func (e *Engine) InspectOperation(ctx context.Context, scope Scope, id state.OperationID) (state.Operation, string, error) {
	op, uid, err := e.inspectOperation(ctx, scope, id)
	return op, uid, err
}

func (e *Engine) inspectOperation(_ context.Context, scope Scope, id state.OperationID) (state.Operation, string, error) {
	env, err := state.OpenEnv(e.d.Settings.StateDir(), scope.Project.String(), scope.Environment.String(), e.d.State)
	if errors.Is(err, fs.ErrNotExist) {
		return state.Operation{}, "", fail(state.CodeOperationNotFound, state.CategoryNotFound, "operation %s is not recorded for %s.%s", id, scope.Project, scope.Environment)
	}
	if err != nil {
		return state.Operation{}, "", err
	}
	defer env.Close()
	st, _, err := env.Load()
	if err != nil {
		return state.Operation{}, "", err
	}
	if st.Operation != nil && st.Operation.ID == id {
		return *st.Operation, st.ScopeUID.String(), nil
	}
	op, ok, err := env.LoadOperation(id)
	if err != nil {
		return state.Operation{}, "", err
	}
	if !ok {
		return state.Operation{}, "", fail(state.CodeOperationNotFound, state.CategoryNotFound, "operation %s is not recorded for %s.%s", id, scope.Project, scope.Environment)
	}
	return op, st.ScopeUID.String(), nil
}

// Cancel asks the running operation to stop between steps by writing cancel/<id>.json, and writes
// nothing else. It takes no lock: the operation notices the request before its next step.
func (e *Engine) Cancel(_ context.Context, scope Scope, id state.OperationID) error {
	env, err := state.OpenEnv(e.d.Settings.StateDir(), scope.Project.String(), scope.Environment.String(), e.d.State)
	if errors.Is(err, fs.ErrNotExist) {
		return fail(state.CodeOperationNotFound, state.CategoryNotFound, "operation %s is not recorded for %s.%s", id, scope.Project, scope.Environment)
	}
	if err != nil {
		return err
	}
	defer env.Close()
	st, _, err := env.Load()
	if err != nil {
		return err
	}
	if st.Operation == nil || st.Operation.ID != id {
		if _, archived, err := env.LoadOperation(id); err == nil && archived {
			return fail(state.CodeOperationNotRunning, state.CategoryConflict, "operation %s is not running", id)
		}
		return fail(state.CodeOperationNotFound, state.CategoryNotFound, "operation %s is not recorded for %s.%s", id, scope.Project, scope.Environment)
	}
	return env.WriteCancel(state.CancelRequest{Operation: id, Actor: state.Actor{Kind: state.ActorOSUser}, RequestedAt: e.now()})
}
