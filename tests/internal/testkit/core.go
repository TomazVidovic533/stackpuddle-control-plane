package testkit

import (
	"crypto/rand"
	"io"
	"runtime"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets/local"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// CoreOptions compose an operation engine over the real Compose adapter and the real sealed store,
// for the tests that need a catalog or a fault seam that the public engine does not expose.
type CoreOptions struct {
	// Env is the environment the settings are loaded from.
	Env map[string]string
	// Catalog is the driver catalog; nil means none.
	Catalog []module.Driver
	// WrapRuntime, if set, wraps the real adapter, such as to kill the process after an effect.
	WrapRuntime func(operation.Runtime) operation.Runtime
	// State carries the write fault seam.
	State state.Options
	// Now and Random default to the real clock and crypto/rand.
	Now    func() time.Time
	Random io.Reader
}

// Core is an operation engine and the parts a test needs next to it.
type Core struct {
	Engine   *operation.Engine
	Settings config.Settings
	Adapter  *compose.Adapter
	Provider *local.Provider
}

// NewCore builds the engine the way engine.New does, plus the options.
func NewCore(o CoreOptions) (*Core, error) {
	settings, err := config.Load(LookupEnv(o.Env), runtime.GOOS)
	if err != nil {
		return nil, err
	}
	random := o.Random
	if random == nil {
		random = rand.Reader
	}
	docker, _ := settings.DockerPath()
	adapter := compose.New(compose.Options{Docker: docker, ChildEnv: settings.ChildEnv})
	provider := local.New(local.Options{Dir: settings.SecretsDir(), Now: o.Now, Random: random, KeySource: keySource(settings)})
	var rt operation.Runtime = adapter
	if o.WrapRuntime != nil {
		rt = o.WrapRuntime(adapter)
	}
	opts := o.State
	if opts.Random == nil {
		opts.Random = random
	}
	eng, err := operation.New(operation.Deps{
		Catalog: o.Catalog, Runtime: rt, Providers: map[string]secrets.Provider{provider.ID(): provider},
		Settings: settings, State: opts, Now: o.Now, Random: random, Version: "test", RuntimeCapability: "runtime.compose/1",
	})
	if err != nil {
		return nil, err
	}
	return &Core{Engine: eng, Settings: settings, Adapter: adapter, Provider: provider}, nil
}
