package config_test

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func TestLoad_StateRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
	}{
		{"linux with XDG_STATE_HOME", "linux", map[string]string{"XDG_STATE_HOME": "/xdg/state", "HOME": "/home/u"}, "/xdg/state/stackpuddle"},
		{"linux without XDG_STATE_HOME", "linux", map[string]string{"HOME": "/home/u"}, "/home/u/.local/state/stackpuddle"},
		{"linux ignores a relative XDG_STATE_HOME", "linux", map[string]string{"XDG_STATE_HOME": "state", "HOME": "/home/u"}, "/home/u/.local/state/stackpuddle"},
		{"darwin", "darwin", map[string]string{"HOME": "/Users/u", "XDG_STATE_HOME": "/xdg"}, "/Users/u/Library/Application Support/stackpuddle"},
		{"override replaces linux", "linux", map[string]string{"STACKPUDDLE_STATE_DIR": "/data/sp", "HOME": "/home/u"}, "/data/sp"},
		{"override replaces darwin", "darwin", map[string]string{"STACKPUDDLE_STATE_DIR": "/data/sp/", "HOME": "/Users/u"}, "/data/sp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			settings, err := config.Load(testkit.LookupEnv(test.env), test.goos)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := settings.StateDir(); got != test.want {
				t.Errorf("StateDir = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLoad_SecretsDir(t *testing.T) {
	t.Parallel()
	settings, err := config.Load(testkit.LookupEnv(map[string]string{"HOME": "/home/u"}), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := settings.SecretsDir(), "/home/u/.local/state/stackpuddle/secrets"; got != want {
		t.Errorf("default SecretsDir = %q, want %q", got, want)
	}
	settings, err = config.Load(testkit.LookupEnv(map[string]string{"HOME": "/home/u", "STACKPUDDLE_SECRETS_DIR": "/vault"}), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.SecretsDir(); got != "/vault" {
		t.Errorf("overridden SecretsDir = %q", got)
	}
}

func TestLoad_RefusesBadPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     map[string]string
		wantErr error
		naming  string
	}{
		{"relative state dir", map[string]string{"STACKPUDDLE_STATE_DIR": "state", "HOME": "/h"}, config.ErrPathNotAbsolute, "STACKPUDDLE_STATE_DIR"},
		{"relative secrets dir", map[string]string{"STACKPUDDLE_SECRETS_DIR": "s", "HOME": "/h"}, config.ErrPathNotAbsolute, "STACKPUDDLE_SECRETS_DIR"},
		{"no home", map[string]string{}, config.ErrNoStateRoot, "STACKPUDDLE_STATE_DIR"},
		{"relative home", map[string]string{"HOME": "h"}, config.ErrNoStateRoot, "HOME"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.Load(testkit.LookupEnv(test.env), "linux")
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Load error = %v, want %v", err, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.naming) {
				t.Errorf("error %q does not name %s", err, test.naming)
			}
		})
	}
}

func TestSecretsKey(t *testing.T) {
	t.Parallel()
	valid := testkit.Key("seed")
	tests := []struct {
		name    string
		env     map[string]string
		wantErr error
	}{
		{"valid", map[string]string{"HOME": "/h", "STACKPUDDLE_SECRETS_KEY": valid}, nil},
		{"missing", map[string]string{"HOME": "/h"}, config.ErrKeyMissing},
		{"empty", map[string]string{"HOME": "/h", "STACKPUDDLE_SECRETS_KEY": ""}, config.ErrKeyMissing},
		{"not base64", map[string]string{"HOME": "/h", "STACKPUDDLE_SECRETS_KEY": "not base64!"}, config.ErrKeyInvalid},
		{"31 bytes", map[string]string{"HOME": "/h", "STACKPUDDLE_SECRETS_KEY": base64.StdEncoding.EncodeToString(make([]byte, 31))}, config.ErrKeyInvalid},
		{"33 bytes", map[string]string{"HOME": "/h", "STACKPUDDLE_SECRETS_KEY": base64.StdEncoding.EncodeToString(make([]byte, 33))}, config.ErrKeyInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			settings, err := config.Load(testkit.LookupEnv(test.env), "linux")
			if err != nil {
				t.Fatalf("Load must not fail on the key: %v", err)
			}
			_, err = settings.SecretsKey()
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("SecretsKey error = %v, want %v", err, test.wantErr)
			}
			if err != nil {
				if !strings.Contains(err.Error(), config.KeySecretsKey) {
					t.Errorf("error %q does not name the setting", err)
				}
				if v := test.env["STACKPUDDLE_SECRETS_KEY"]; v != "" && strings.Contains(err.Error(), v) {
					t.Errorf("error %q contains the key value", err)
				}
			}
		})
	}
}

func TestChildEnv_DropsEveryForbiddenVariable(t *testing.T) {
	t.Parallel()
	forbidden := []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "COMPOSE_PROJECT_NAME", "COMPOSE_FILE", "STACKPUDDLE_SECRETS_KEY", "AWS_SECRET_ACCESS_KEY"}
	env := map[string]string{"HOME": "/h", "PATH": "/usr/bin", "USER": "u", "LANG": "C", "DOCKER_CONFIG": "/dc", "SSH_AUTH_SOCK": "/s"}
	for _, name := range forbidden {
		env[name] = "x"
	}
	settings, err := config.Load(testkit.LookupEnv(env), "linux")
	if err != nil {
		t.Fatal(err)
	}
	got := settings.ChildEnv()
	for _, name := range forbidden {
		if slices.ContainsFunc(got, func(entry string) bool { return strings.HasPrefix(entry, name+"=") }) {
			t.Errorf("child environment carries %s", name)
		}
	}
	for _, want := range []string{"HOME=/h", "PATH=/usr/bin", "USER=u", "LANG=C", "DOCKER_CONFIG=/dc", "SSH_AUTH_SOCK=/s"} {
		if !slices.Contains(got, want) {
			t.Errorf("child environment lacks %s: %v", want, got)
		}
	}
}

func TestDockerPath_SearchesTheInjectedPATH(t *testing.T) {
	t.Parallel()
	dir := testkit.Root(t)
	settings, err := config.Load(testkit.LookupEnv(map[string]string{"HOME": "/h", "PATH": dir}), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := settings.DockerPath(); ok {
		t.Fatal("found a docker binary in an empty PATH directory")
	}
}
