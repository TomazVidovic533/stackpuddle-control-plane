package testkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TomazVidovic533/stackpuddle/artifacts"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// SiblingTreeEnv names the environment variable that overrides the location of the stackpuddle tree.
const SiblingTreeEnv = "STACKPUDDLE_SIBLING_TREE"

// ArchiveRecord says which tree and bytes a test ran against. It is logged by every test that builds
// a module archive, so a reader of the evidence knows the coverage is the developer's: the module
// built from the sibling tree with the public artifacts package, not the published artifact path
// (DECISIONS D12).
type ArchiveRecord struct {
	Module   string
	Tree     string
	Revision string
	Dirty    bool
	Digest   string
}

// String formats the record for a test log.
func (r ArchiveRecord) String() string {
	state := "clean"
	if r.Dirty {
		state = "with uncommitted changes"
	}
	return fmt.Sprintf("module %s archive built from %s at revision %s (%s), digest %s; developer coverage, narrower than the published artifact path",
		r.Module, r.Tree, r.Revision, state, r.Digest)
}

// ErrSiblingTreeMissing marks a missing sibling stackpuddle tree.
var ErrSiblingTreeMissing = errors.New("testkit: the sibling stackpuddle tree is missing")

// SiblingTree returns the directory of the sibling stackpuddle tree: the repository beside this one,
// or the one the environment names. Callers fail by name when it is missing, because skipping would
// hide the loss of the coverage.
func SiblingTree() (string, error) {
	tree := os.Getenv(SiblingTreeEnv)
	if tree == "" {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			return "", errors.New("testkit: cannot locate the control-plane tree to find its sibling stackpuddle tree")
		}
		tree = filepath.Join(filepath.Dir(file), "..", "..", "..", "..", "stackpuddle")
	}
	if info, err := os.Stat(filepath.Join(tree, "modules")); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %q has no modules directory; the provisioning tests build the module archive from it (set %s to its path) and fail rather than skip", ErrSiblingTreeMissing, tree, SiblingTreeEnv)
	}
	return tree, nil
}

// PackagedModule builds the named module's archive from the sibling tree with the public artifacts
// package, inspects it, reads it back into a read-only file system and loads the definition from it,
// exactly as an engine consumer holding the archive would. It logs the record.
func PackagedModule(t testing.TB, name string) (model.ModuleDefinition, ArchiveRecord) {
	t.Helper()
	definition, record, err := BuildPackagedModule(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(record)
	return definition, record
}

// BuildPackagedModule is PackagedModule for a caller that has no testing.TB, such as a helper process.
func BuildPackagedModule(name string) (model.ModuleDefinition, ArchiveRecord, error) {
	tree, err := SiblingTree()
	if err != nil {
		return model.ModuleDefinition{}, ArchiveRecord{}, err
	}
	dir := filepath.Join(tree, "modules", name)
	var archive bytes.Buffer
	digest, err := artifacts.Build(artifacts.Module, dir, &archive)
	if err != nil {
		return model.ModuleDefinition{}, ArchiveRecord{}, fmt.Errorf("testkit: build the %s module archive from %s: %w", name, dir, err)
	}
	if err := artifacts.Inspect(artifacts.Module, bytes.NewReader(archive.Bytes()), artifacts.DefaultLimits()); err != nil {
		return model.ModuleDefinition{}, ArchiveRecord{}, fmt.Errorf("testkit: the %s module archive does not inspect: %w", name, err)
	}
	fsys, err := archiveFS(archive.Bytes())
	if err != nil {
		return model.ModuleDefinition{}, ArchiveRecord{}, fmt.Errorf("testkit: read the %s module archive: %w", name, err)
	}
	definition, diagnostics := model.LoadModuleDefinition(fsys)
	if len(diagnostics) > 0 {
		return model.ModuleDefinition{}, ArchiveRecord{}, fmt.Errorf("testkit: the %s module archive does not load: %v", name, diagnostics)
	}
	revision, dirty := gitRevision(tree, filepath.Join("modules", name))
	return definition, ArchiveRecord{Module: name, Tree: tree, Revision: revision, Dirty: dirty, Digest: digest}, nil
}

// archiveFS reads a verified module archive into memory.
func archiveFS(archive []byte) (fstest.MapFS, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()
	files := fstest.MapFS{}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		files[path.Clean(header.Name)] = &fstest.MapFile{Data: data, Mode: 0o444}
	}
}

// gitRevision returns the tree's HEAD and whether the module directory has uncommitted changes. A
// tree that is not a checkout reports "unknown".
func gitRevision(tree, subdir string) (revision string, dirty bool) {
	head, err := exec.Command("git", "-C", tree, "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown", true
	}
	status, err := exec.Command("git", "-C", tree, "status", "--porcelain", "--", subdir).Output()
	return strings.TrimSpace(string(head)), err != nil || len(bytes.TrimSpace(status)) > 0
}
