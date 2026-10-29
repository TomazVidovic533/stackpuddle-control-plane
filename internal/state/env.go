package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/durable"
)

// File and folder modes: owner only, set explicitly so a permissive umask cannot widen them.
const (
	dirMode  = 0o700
	fileMode = 0o600
)

const (
	envFolder        = "environments"
	scopeFile        = "scope.json"
	stateFile        = "state.json"
	lockFile         = "env.flock"
	operationsFolder = "operations"
	cancelFolder     = "cancel"
)

// Options are the seams of an Env.
type Options struct {
	// Random supplies the temporary-file suffix; nil uses crypto/rand.
	Random io.Reader
	// AfterStage is the fault-injection seam for the atomic write: it is called after each
	// durable.Stage of every write and a returned error aborts the write. Only tests set it.
	AfterStage func(durable.Stage) error
}

// Env is the state folder of one declared <project>.<environment>.
type Env struct {
	dir  string
	root *os.Root
	opts Options
}

// EnvironmentDir returns the folder of one environment under the state root. project and
// environment must be single path elements without ".".
func EnvironmentDir(stateRoot, project, environment string) (string, error) {
	for _, name := range []string{project, environment} {
		if name == "" || strings.ContainsAny(name, "./\\") {
			return "", fmt.Errorf("state: %q is not a valid project or environment name", name)
		}
	}
	return filepath.Join(stateRoot, envFolder, project+"."+environment), nil
}

// OpenEnv opens the folder of one environment. It fails with fs.ErrNotExist when the folder is
// absent and creates nothing, so Inspect and a dry run write nothing.
func OpenEnv(stateRoot, project, environment string, opts Options) (*Env, error) {
	dir, err := EnvironmentDir(stateRoot, project, environment)
	if err != nil {
		return nil, err
	}
	return openDir(dir, opts)
}

// CreateEnv is OpenEnv for a writer: it first creates the environment folder and its operations and
// cancel folders with mode 0700 and chmods each, so a permissive umask cannot widen them.
func CreateEnv(stateRoot, project, environment string, opts Options) (*Env, error) {
	dir, err := EnvironmentDir(stateRoot, project, environment)
	if err != nil {
		return nil, err
	}
	for _, folder := range []string{
		stateRoot, filepath.Join(stateRoot, envFolder), dir,
		filepath.Join(dir, operationsFolder), filepath.Join(dir, cancelFolder),
	} {
		if err := ensureDir(folder); err != nil {
			return nil, err
		}
	}
	return openDir(dir, opts)
}

func openDir(dir string, opts Options) (*Env, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("state: open environment folder: %w", err)
	}
	return &Env{dir: dir, root: root, opts: opts}, nil
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, dirMode); err != nil {
		return fmt.Errorf("state: create %s: %w", path, err)
	}
	if err := os.Chmod(path, dirMode); err != nil {
		return fmt.Errorf("state: chmod %s: %w", path, err)
	}
	return nil
}

// Close releases the folder handle.
func (e *Env) Close() error { return e.root.Close() }

// Dir returns the environment folder.
func (e *Env) Dir() string { return e.dir }

// LockPath returns the path of env.flock inside the environment folder.
func (e *Env) LockPath() string { return filepath.Join(e.dir, lockFile) }

func (e *Env) read(root *os.Root, name string, limit int) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("state: read %s: %w", name, err)
	}
	return data, nil
}

func (e *Env) write(root *os.Root, name string, data []byte) error {
	return durable.WriteFile(root, name, data, durable.Options{Perm: fileMode, Random: e.opts.Random, After: e.opts.AfterStage})
}

func (e *Env) sub(folder string) (*os.Root, error) {
	root, err := e.root.OpenRoot(folder)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", folder, err)
	}
	return root, nil
}

// LoadScope reads scope.json. ok is false when the file does not exist yet.
func (e *Env) LoadScope() (scope Scope, ok bool, err error) {
	data, err := e.read(e.root, scopeFile, scopeFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return Scope{}, false, nil
	}
	if err != nil {
		return Scope{}, false, err
	}
	scope, err = ParseScope(data)
	return scope, err == nil, err
}

// WriteScope writes scope.json once. A second call fails with ErrScopeExists.
func (e *Env) WriteScope(scope Scope) error {
	if _, err := e.root.Stat(scopeFile); err == nil {
		return ErrScopeExists
	}
	scope.StateVersion = StateVersion
	data, err := json.MarshalIndent(scope, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode scope: %w", err)
	}
	return e.write(e.root, scopeFile, append(data, '\n'))
}

// Load reads state.json. ok is false when the file does not exist yet. An open operation whose
// archive already exists is dropped from the returned state, which finishes an interrupted archive.
func (e *Env) Load() (st State, ok bool, err error) {
	data, err := e.read(e.root, stateFile, stateFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	st, err = ParseState(data)
	if err != nil {
		return State{}, false, err
	}
	if st.Operation != nil && e.archiveExists(st.Operation.ID) {
		st.Operation = nil
	}
	return st, true, nil
}

func (e *Env) archiveExists(id OperationID) bool {
	_, err := e.root.Stat(filepath.Join(operationsFolder, id.String()+".json"))
	return err == nil
}

// Commit writes next as the new generation if the file on disk is still at generation expected;
// otherwise it fails with ErrStaleGeneration and leaves the newer file in place. It returns the
// state as written. The environment lock excludes other writers; the generation detects a lock
// that was bypassed or replaced.
func (e *Env) Commit(expected int64, next State) (State, error) {
	current, _, err := e.rawGeneration()
	if err != nil {
		return State{}, err
	}
	if current != expected {
		return State{}, fmt.Errorf("%w: expected generation %d, found %d", ErrStaleGeneration, expected, current)
	}
	next.StateVersion = StateVersion
	next.Generation = expected + 1
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return State{}, fmt.Errorf("state: encode state: %w", err)
	}
	if err := e.write(e.root, stateFile, append(data, '\n')); err != nil {
		return State{}, err
	}
	return next, nil
}

// rawGeneration reads the generation on disk, 0 when no file exists, without dropping archives.
func (e *Env) rawGeneration() (generation int64, ok bool, err error) {
	data, err := e.read(e.root, stateFile, stateFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	st, err := ParseState(data)
	if err != nil {
		return 0, false, err
	}
	return st.Generation, true, nil
}

// Archive writes a finished operation to operations/<id>.json once.
func (e *Env) Archive(scope ScopeUID, op Operation) error {
	dir, err := e.sub(operationsFolder)
	if err != nil {
		return err
	}
	defer dir.Close()
	data, err := json.MarshalIndent(Archived{StateVersion: StateVersion, ScopeUID: scope, Operation: op}, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode archived operation: %w", err)
	}
	return e.write(dir, op.ID.String()+".json", append(data, '\n'))
}

// LoadOperation reads an archived operation. ok is false when it is not archived.
func (e *Env) LoadOperation(id OperationID) (op Operation, ok bool, err error) {
	dir, err := e.sub(operationsFolder)
	if errors.Is(err, fs.ErrNotExist) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	defer dir.Close()
	data, err := e.read(dir, id.String()+".json", archiveLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return Operation{}, false, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	archived, err := ParseArchived(data)
	if err != nil {
		return Operation{}, false, err
	}
	return archived.Operation, true, nil
}

// WriteCancel writes cancel/<op-id>.json atomically. Only Cancel calls it.
func (e *Env) WriteCancel(request CancelRequest) error {
	dir, err := e.sub(cancelFolder)
	if err != nil {
		return err
	}
	defer dir.Close()
	request.StateVersion = StateVersion
	data, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode cancel request: %w", err)
	}
	return e.write(dir, request.Operation.String()+".json", append(data, '\n'))
}

// CancelRequested reports whether a cancel request exists for id. A request that exists is parsed
// strictly, so a damaged one is an error rather than a silent "no".
func (e *Env) CancelRequested(id OperationID) (bool, error) {
	dir, err := e.sub(cancelFolder)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer dir.Close()
	data, err := e.read(dir, id.String()+".json", cancelFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	request, err := ParseCancel(data)
	if err != nil {
		return false, err
	}
	return request.Operation == id, nil
}
