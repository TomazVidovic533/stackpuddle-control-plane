package engine

import (
	"context"
	"errors"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Code is a stable error code, "<area>.<name>". Callers match on codes, never on message text.
type Code string

// The codes re-export the one internal table, internal/state/codes.go. Each is a published name:
// it is kept or retired explicitly, never renamed.
const (
	CodeCapabilityUnsupported   Code = state.CodeCapabilityUnsupported
	CodeProjectUnresolved       Code = state.CodeProjectUnresolved
	CodeProjectInvalid          Code = state.CodeProjectInvalid
	CodeStateVersionUnsupported Code = state.CodeStateVersionUnsupported
	CodeExpandedScopeRequired   Code = state.CodeExpandedScopeRequired
	CodeSecretInEnvironment     Code = state.CodeSecretInEnvironment
	CodeDeliveryConflict        Code = state.CodeDeliveryConflict
	CodeValueUnavailable        Code = state.CodeValueUnavailable
	CodeJobExitedNonzero        Code = state.CodeJobExitedNonzero
	CodeParametersInvalid       Code = state.CodeParametersInvalid
	CodeEnvironmentBusy         Code = state.CodeEnvironmentBusy
	CodeStaleGeneration         Code = state.CodeStaleGeneration
	CodeFilesystemUnsupported   Code = state.CodeFilesystemUnsupported
	CodeKeyMissing              Code = state.CodeKeyMissing
	CodeKeyInvalid              Code = state.CodeKeyInvalid
	CodeKeyMismatch             Code = state.CodeKeyMismatch
	CodeStoreUnreadable         Code = state.CodeStoreUnreadable
	CodeSecretMissing           Code = state.CodeSecretMissing
	CodeTargetUnreachable       Code = state.CodeTargetUnreachable
	CodeTargetReplaced          Code = state.CodeTargetReplaced
	CodeEffectFailed            Code = state.CodeEffectFailed
	CodeImageMissing            Code = state.CodeImageMissing
	CodeOutOfScope              Code = state.CodeOutOfScope
	CodeUnmappedOwnedObject     Code = state.CodeUnmappedOwnedObject
	CodeLostObject              Code = state.CodeLostObject
	CodeOperationNotFound       Code = state.CodeOperationNotFound
	CodeOperationNotRunning     Code = state.CodeOperationNotRunning
	CodeOperationInterrupted    Code = state.CodeOperationInterrupted
	CodeOperationCancelled      Code = state.CodeOperationCancelled
	CodeInvalidSetting          Code = state.CodeInvalidSetting
	CodeInputMissing            Code = state.CodeInputMissing
	CodeInputInvalid            Code = state.CodeInputInvalid
	CodeUnknownObject           Code = state.CodeUnknownObject
	CodeUnexpected              Code = state.CodeUnexpected
)

// Error is a classified failure. Match it with errors.AsType[*Error] and branch on Category or Code.
type Error struct {
	category Category
	code     Code
	message  string
	cause    error
}

// Category returns the class of the failure, which decides how a caller reacts.
func (e *Error) Category() Category { return e.category }

// Code returns the stable code.
func (e *Error) Code() Code { return e.code }

// Error returns a message for people. It names settings and objects, never a secret value.
func (e *Error) Error() string { return e.message }

// Unwrap returns the underlying cause for errors.Is.
func (e *Error) Unwrap() error { return e.cause }

// rule maps one internal sentinel to a public code and category.
type rule struct {
	sentinel error
	code     Code
	category Category
}

func rules() []rule {
	return []rule{
		{lock.ErrBusy, CodeEnvironmentBusy, CategoryConflict},
		{lock.ErrFilesystemUnsupported, CodeFilesystemUnsupported, CategoryValidation},
		{state.ErrUnsupported, CodeStateVersionUnsupported, CategoryValidation},
		{state.ErrStaleGeneration, CodeStaleGeneration, CategoryConflict},
		{secrets.ErrKeyMissing, CodeKeyMissing, CategoryValidation},
		{secrets.ErrKeyInvalid, CodeKeyInvalid, CategoryValidation},
		{secrets.ErrKeyMismatch, CodeKeyMismatch, CategoryForbidden},
		{secrets.ErrStoreUnreadable, CodeStoreUnreadable, CategoryForbidden},
		{secrets.ErrSecretMissing, CodeSecretMissing, CategoryNotFound},
		{config.ErrPathNotAbsolute, CodeInvalidSetting, CategoryValidation},
		{config.ErrNoStateRoot, CodeInvalidSetting, CategoryValidation},
		{compose.ErrNoDocker, CodeTargetUnreachable, CategoryTransient},
		{compose.ErrUnreachable, CodeTargetUnreachable, CategoryTransient},
		{compose.ErrOutOfScope, CodeOutOfScope, CategoryForbidden},
		{compose.ErrImageMissing, CodeImageMissing, CategoryNotFound},
		{compose.ErrBuildContext, CodeProjectInvalid, CategoryValidation},
		{compose.ErrCommand, CodeEffectFailed, CategoryUnexpected},
		{context.Canceled, CodeOperationInterrupted, CategoryTransient},
		{context.DeadlineExceeded, CodeOperationInterrupted, CategoryTransient},
	}
}

func isKeyMissing(err error) bool { return errors.Is(err, config.ErrKeyMissing) }

// toPublic is the one place internal errors become public ones. A failure the operation classified
// keeps its code; a known sentinel gets its row; anything else is unexpected.
func toPublic(err error) error {
	if err == nil {
		return nil
	}
	if existing, ok := errors.AsType[*Error](err); ok {
		return existing
	}
	if f, ok := errors.AsType[*operation.Failure](err); ok {
		return &Error{category: f.Category, code: Code(f.Code), message: f.Error(), cause: err}
	}
	for _, r := range rules() {
		if errors.Is(err, r.sentinel) {
			return &Error{category: r.category, code: r.code, message: err.Error(), cause: err}
		}
	}
	return &Error{category: CategoryUnexpected, code: CodeUnexpected, message: err.Error(), cause: err}
}
