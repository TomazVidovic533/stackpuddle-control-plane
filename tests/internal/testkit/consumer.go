package testkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing/fstest"

	"github.com/TomazVidovic533/stackpuddle/model"
)

// ErrConsumerFixtureMissing marks a missing shared consumer fixture. Tests fail by name rather than
// skip, because skipping would hide the loss of the coverage.
var ErrConsumerFixtureMissing = errors.New("testkit: the shared consumer fixture is missing")

// ConsumerFixtureRoot is the fixture's folder in the sibling stackpuddle tree, tests/fixtures/consumer.
// The tree is found as the module archives are (SiblingTree); a tree without the folder fails by name.
func ConsumerFixtureRoot() (string, error) {
	tree, err := SiblingTree()
	if err != nil {
		return "", err
	}
	root := filepath.Join(tree, "tests", "fixtures", "consumer")
	if info, err := os.Stat(filepath.Join(root, "stackpuddle.yaml")); err != nil || info.IsDir() {
		return "", fmt.Errorf("%w: %q has no stackpuddle.yaml; the delivery tests read the fixture from the sibling tree (set %s to its path) and fail rather than skip", ErrConsumerFixtureMissing, root, SiblingTreeEnv)
	}
	return root, nil
}

// Edit changes the fixture's text before it loads. Old must occur exactly once, so a test that edits a
// fixture which has since changed fails by name instead of testing something else.
type Edit struct {
	Old, New string
}

// ConsumerProject loads the shared consumer fixture as a project named name, with the packaged
// postgres definition built from the sibling tree, after the edits. The fixture's own name is a
// placeholder that every test replaces with its disposable project name.
func ConsumerProject(name string, edits ...Edit) (model.Project, error) {
	root, err := ConsumerFixtureRoot()
	if err != nil {
		return model.Project{}, err
	}
	data, err := os.ReadFile(filepath.Join(root, "stackpuddle.yaml"))
	if err != nil {
		return model.Project{}, fmt.Errorf("%w: %w", ErrConsumerFixtureMissing, err)
	}
	text := string(data)
	all := append([]Edit{{Old: "metadata:\n  name: consumer\n", New: "metadata:\n  name: " + name + "\n"}}, edits...)
	for _, e := range all {
		if n := strings.Count(text, e.Old); n != 1 {
			return model.Project{}, fmt.Errorf("testkit: the consumer fixture holds %d occurrences of %q, want exactly one; the fixture changed", n, e.Old)
		}
		text = strings.Replace(text, e.Old, e.New, 1)
	}
	definition, _, err := BuildPackagedModule("postgres")
	if err != nil {
		return model.Project{}, err
	}
	fsys := fstest.MapFS{"stackpuddle.yaml": &fstest.MapFile{Data: []byte(text)}}
	project, diagnostics := model.Load(fsys, "stackpuddle.yaml", []model.ModuleDefinition{definition})
	if len(diagnostics) > 0 {
		return model.Project{}, fmt.Errorf("testkit: the consumer fixture does not load: %v", diagnostics)
	}
	return project, nil
}

// ConsumerExposure is the edit that publishes the database's sql export on the host's loopback
// address, which is what puts host names into the server certificate.
func ConsumerExposure() Edit {
	const local = "    local:\n      runtime:\n        type: compose\n        context: default\n      secrets:\n        provider: local\n"
	return Edit{Old: local, New: local + "      exposures:\n        - source:\n            object: module/db\n            export: sql\n"}
}
