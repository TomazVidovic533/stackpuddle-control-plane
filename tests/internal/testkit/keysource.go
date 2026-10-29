package testkit

import (
	"errors"
	"fmt"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
)

// keySource adapts the settings' key errors to the provider's, as engine.New does.
func keySource(settings config.Settings) func() ([config.KeySize]byte, error) {
	return func() ([config.KeySize]byte, error) {
		key, err := settings.SecretsKey()
		switch {
		case err == nil:
			return key, nil
		case errors.Is(err, config.ErrKeyMissing):
			return key, fmt.Errorf("%w: %w", secrets.ErrKeyMissing, err)
		}
		return key, fmt.Errorf("%w: %w", secrets.ErrKeyInvalid, err)
	}
}
