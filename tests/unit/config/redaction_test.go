package config_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// logKey is the attribute key of the logged settings.
const logKey = "settings"

func TestSettingsNeverPrintTheSecretsKeyThroughAnyVerbOrLogger(t *testing.T) {
	t.Parallel()
	key := testkit.Key("redaction")
	settings, err := config.Load(testkit.LookupEnv(map[string]string{"HOME": "/home/u", "STACKPUDDLE_SECRETS_KEY": key}), "linux")
	if err != nil {
		t.Fatal(err)
	}
	// holder reaches the settings through an unexported field, where fmt cannot call a method.
	type holder struct {
		inner config.Settings
		ptr   *config.Settings
	}
	core, err := operation.New(operation.Deps{Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"deps": operation.Deps{Settings: settings}, "operation engine": core, "value": settings, "pointer": &settings, "holder": holder{inner: settings, ptr: &settings}, "holder pointer": &holder{inner: settings, ptr: &settings}}
	for name, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%T %v"} {
			if out := fmt.Sprintf(verb, value); strings.Contains(out, key) || strings.Contains(out, fmt.Sprintf("%x", key)) {
				t.Errorf("%s printed with %s holds the key: %s", name, verb, out)
			}
		}
		for handlerName, newHandler := range map[string]func(*bytes.Buffer) slog.Handler{
			"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var out bytes.Buffer
			slog.New(newHandler(&out)).Info("settings", slog.Any(logKey, value))
			if strings.Contains(out.String(), key) {
				t.Errorf("%s logged by the %s handler holds the key: %s", name, handlerName, out.String())
			}
		}
	}
	if got, err := settings.SecretsKey(); err != nil || got == [config.KeySize]byte{} {
		t.Errorf("redaction must not break the key: %v", err)
	}
}
