// Package engine is the only public package of the control plane: a Go library the command-line
// interface runs in-process. It submits, inspects and cancels operations on one environment of a
// loaded project and advertises what it can run.
//
// There is no listener, transport or login here. Everything else lives under internal/.
package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets/local"
	"github.com/TomazVidovic533/stackpuddle/model"
)

const modulePath = "github.com/TomazVidovic533/stackpuddle-control-plane"

// RuntimeCapability is the capability of the Docker Compose runtime this engine runs.
const RuntimeCapability = "runtime.compose/1"

// Options configure New.
type Options struct {
	// LookupEnv reads one environment variable; the process passes os.LookupEnv. It is required and is
	// the only way the engine reads its environment.
	LookupEnv func(key string) (string, bool)
	// Now supplies the clock; nil uses time.Now. Values are converted to UTC.
	Now func() time.Time
	// Random supplies identifiers, temporary-file suffixes and jitter; nil uses crypto/rand. Secret
	// values and encryption nonces never come from it: the local secret provider always draws them
	// from crypto/rand, so a deterministic reader cannot repeat a nonce or make a secret predictable.
	Random io.Reader
}

// Engine runs operations. It is safe for concurrent use by operations on different environments.
type Engine struct {
	core *operation.Engine
	info model.EngineInfo
}

// New composes the engine: settings from the environment, the explicit module catalog, the local
// secret provider and the Compose runtime adapter. It reads no file and starts no process.
func New(opts Options) (*Engine, error) {
	if opts.LookupEnv == nil {
		return nil, &Error{category: CategoryUnexpected, code: CodeUnexpected, message: "engine: Options.LookupEnv is required"}
	}
	settings, err := config.Load(opts.LookupEnv, runtime.GOOS)
	if err != nil {
		return nil, toPublic(err)
	}
	random := opts.Random
	if random == nil {
		random = rand.Reader
	}
	provider := local.New(local.Options{Dir: settings.SecretsDir(), KeySource: keySource(settings), Now: opts.Now, Random: random})
	docker, _ := settings.DockerPath()
	core, err := operation.New(operation.Deps{
		Catalog:           modules.Catalog(),
		Runtime:           compose.New(compose.Options{Docker: docker, ChildEnv: settings.ChildEnv}),
		Providers:         map[string]secrets.Provider{provider.ID(): provider},
		Settings:          settings,
		Now:               opts.Now,
		Random:            random,
		Version:           version(),
		RuntimeCapability: RuntimeCapability,
	})
	if err != nil {
		return nil, toPublic(err)
	}
	info, err := core.Advertise()
	if err != nil {
		return nil, toPublic(err)
	}
	return &Engine{core: core, info: info}, nil
}

// keySource adapts the settings' key errors to the provider's.
func keySource(settings config.Settings) func() ([config.KeySize]byte, error) {
	return func() ([config.KeySize]byte, error) {
		key, err := settings.SecretsKey()
		switch {
		case err == nil:
			return key, nil
		case isKeyMissing(err):
			return key, fmt.Errorf("%w: %w", secrets.ErrKeyMissing, err)
		}
		return key, fmt.Errorf("%w: %w", secrets.ErrKeyInvalid, err)
	}
}

// version reports the control-plane module's version as the build recorded it, "(devel)" when the
// build does not know it.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	if info.Main.Path == modulePath && info.Main.Version != "" {
		return info.Main.Version
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			return dep.Version
		}
	}
	return "(devel)"
}

// Advertise reports the version, the capabilities and the state and store versions this engine
// reads and writes. The capabilities are the runtime, the secret providers and the module drivers.
func (e *Engine) Advertise() model.EngineInfo {
	info := e.info
	info.Capabilities = append([]model.Capability(nil), e.info.Capabilities...)
	info.StateRead = append([]int(nil), e.info.StateRead...)
	info.StoreRead = append([]int(nil), e.info.StoreRead...)
	return info
}

// Cancel asks the running operation to stop between steps. It writes cancel/<id>.json and nothing
// else, and takes no lock.
func (e *Engine) Cancel(ctx context.Context, req CancelRequest) error {
	if err := e.core.Cancel(ctx, operation.Scope(req.Scope), req.Operation); err != nil {
		return toPublic(err)
	}
	return nil
}
