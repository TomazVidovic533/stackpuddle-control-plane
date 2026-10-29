// Package config is the one place the control plane reads its environment.
//
// The caller passes a Lookup (the process passes os.LookupEnv, tests pass a map) and Load turns
// the settings below into typed values. Nothing else in the module reads the environment.
//
// Settings catalogue, one source each, no aliases:
//
//	STACKPUDDLE_STATE_DIR    absolute path that replaces the per-user state root
//	STACKPUDDLE_SECRETS_DIR  absolute path that replaces <state root>/secrets
//	STACKPUDDLE_SECRETS_KEY  32 random bytes, standard base64; never written by StackPuddle
//	XDG_STATE_HOME, HOME     platform inputs for the default state root
//	PATH                     searched once for the docker binary
//
// Every other variable is ignored, and a child process receives only the allowlist of ChildEnv.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Names of the settings in the catalogue above.
const (
	KeyStateDir   = "STACKPUDDLE_STATE_DIR"
	KeySecretsDir = "STACKPUDDLE_SECRETS_DIR"
	KeySecretsKey = "STACKPUDDLE_SECRETS_KEY"
	// KeyHome and KeyXDGStateHome are platform inputs for the default state root; KeyPath is searched
	// once for the docker binary.
	KeyHome         = "HOME"
	KeyXDGStateHome = "XDG_STATE_HOME"
	KeyPath         = "PATH"
)

// KeySize is the length in bytes of the secrets key.
const KeySize = 32

// Sentinels for the failures a caller classifies. Each wrapped error names the setting, never a value.
var (
	// ErrPathNotAbsolute marks a path setting that is not absolute.
	ErrPathNotAbsolute = errors.New("setting must be an absolute path")
	// ErrNoStateRoot marks an environment from which no default state root can be derived.
	ErrNoStateRoot = errors.New("no state root can be derived")
	// ErrKeyMissing marks an unset secrets key.
	ErrKeyMissing = errors.New("secrets key is not set")
	// ErrKeyInvalid marks a secrets key that is not 32 bytes of standard base64.
	ErrKeyInvalid = errors.New("secrets key is not 32 bytes of standard base64")
)

// Lookup reads one environment variable the way os.LookupEnv does.
type Lookup func(key string) (string, bool)

// Settings is the typed configuration. It is immutable after Load and safe for concurrent use.
type Settings struct {
	stateDir   string
	secretsDir string
	path       string
	// rawKey returns the key text. It is a closure because fmt cannot call methods on an unexported
	// field and prints the contents of a string or struct there, but only the address of a function.
	rawKey   func() string
	childEnv []string
}

const redacted = "[redacted]"

// String, GoString, Format and LogValue print the settings without the secrets key, through every
// fmt verb and log/slog.
func (Settings) String() string   { return "config.Settings{" + redacted + "}" }
func (Settings) GoString() string { return "config.Settings{" + redacted + "}" }

// Format prints the redaction for every verb.
func (s Settings) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// LogValue redacts the settings in a structured log.
func (Settings) LogValue() slog.Value { return slog.StringValue(redacted) }

// Load parses the settings from lookup for the operating system goos ("darwin" or any other name,
// which uses the Linux layout). It fails with ErrPathNotAbsolute or ErrNoStateRoot, naming the
// setting. A missing or malformed secrets key is not a Load error: Inspect needs no key, so
// SecretsKey reports it when a Submit opens the store.
func Load(lookup Lookup, goos string) (Settings, error) {
	stateDir, err := stateRoot(lookup, goos)
	if err != nil {
		return Settings{}, err
	}
	secretsDir := filepath.Join(stateDir, "secrets")
	if v, ok := lookup(KeySecretsDir); ok && v != "" {
		if !filepath.IsAbs(v) {
			return Settings{}, fmt.Errorf("%s: %w", KeySecretsDir, ErrPathNotAbsolute)
		}
		secretsDir = filepath.Clean(v)
	}
	path, _ := lookup(KeyPath)
	rawKey, _ := lookup(KeySecretsKey)
	return Settings{
		stateDir:   stateDir,
		secretsDir: secretsDir,
		path:       path,
		rawKey:     func() string { return rawKey },
		childEnv:   allowlisted(lookup),
	}, nil
}

func stateRoot(lookup Lookup, goos string) (string, error) {
	if v, ok := lookup(KeyStateDir); ok && v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s: %w", KeyStateDir, ErrPathNotAbsolute)
		}
		return filepath.Clean(v), nil
	}
	home, _ := lookup(KeyHome)
	if goos == "darwin" {
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("HOME is unset or relative, set %s: %w", KeyStateDir, ErrNoStateRoot)
		}
		return filepath.Join(home, "Library", "Application Support", "stackpuddle"), nil
	}
	if xdg, ok := lookup(KeyXDGStateHome); ok && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "stackpuddle"), nil
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("HOME is unset or relative, set %s: %w", KeyStateDir, ErrNoStateRoot)
	}
	return filepath.Join(home, ".local", "state", "stackpuddle"), nil
}

// childEnvAllowlist is every variable a child process may inherit. DOCKER_HOST, DOCKER_CONTEXT,
// DOCKER_TLS_VERIFY, DOCKER_CERT_PATH and COMPOSE_* are absent on purpose: the declared Docker
// context decides the target, never the caller's shell.
func childEnvAllowlist() []string {
	return []string{
		"PATH", "HOME", "USER", "LANG", "LC_ALL", "TMPDIR", "XDG_RUNTIME_DIR",
		"XDG_CONFIG_HOME", "DOCKER_CONFIG", "SSH_AUTH_SOCK",
	}
}

// ChildEnvNames lists the variable names a child process may inherit, which a driver may not rebind.
func ChildEnvNames() []string { return childEnvAllowlist() }

func allowlisted(lookup Lookup) []string {
	var env []string
	for _, name := range childEnvAllowlist() {
		if v, ok := lookup(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// StateDir returns the absolute per-user state root.
func (s Settings) StateDir() string { return s.stateDir }

// SecretsDir returns the absolute folder of the sealed secret stores.
func (s Settings) SecretsDir() string { return s.secretsDir }

// ChildEnv returns the allowlisted environment for a child process, as "NAME=value" entries. Each
// call returns a new slice.
func (s Settings) ChildEnv() []string { return append([]string(nil), s.childEnv...) }

// SecretsKey returns the 32-byte secrets key. It fails with ErrKeyMissing when the setting is unset
// and ErrKeyInvalid when it does not decode to exactly 32 bytes; both name KeySecretsKey and never
// the value.
func (s Settings) SecretsKey() ([KeySize]byte, error) {
	var key [KeySize]byte
	if s.rawKey == nil || s.rawKey() == "" {
		return key, fmt.Errorf("%s: %w", KeySecretsKey, ErrKeyMissing)
	}
	decoded, err := base64.StdEncoding.DecodeString(s.rawKey())
	if err != nil || len(decoded) != KeySize {
		return key, fmt.Errorf("%s: %w", KeySecretsKey, ErrKeyInvalid)
	}
	copy(key[:], decoded)
	return key, nil
}

// DockerPath searches the injected PATH for an executable named docker and reports whether it found one.
func (s Settings) DockerPath() (string, bool) {
	for _, dir := range filepath.SplitList(s.path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, "docker")
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}
