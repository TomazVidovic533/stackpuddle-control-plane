//go:build integration

// Package delivery_test runs the consumer journey of the shared consumer fixture and the generic
// delivery path against Docker, in disposable Compose projects. The module archive and the fixture are
// read from the sibling stackpuddle tree, so these tests are developer coverage, narrower than the
// published artifact path.
package delivery_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

const namePrefix = "sp-m01-060-"

// inspect returns the raw docker inspect JSON of a container.
func inspect(t *testing.T, container string) string {
	t.Helper()
	return testkit.Docker(t, "inspect", container)
}

// exec runs a command in a running container and returns its trimmed output.
func execIn(t *testing.T, container string, argv ...string) string {
	t.Helper()
	return testkit.Docker(t, append([]string{"exec", container}, argv...)...)
}

// stepIndex returns the position of the step with the action and object, or -1.
func stepIndex(op state.Operation, action, object string) int {
	return slices.IndexFunc(op.Steps, func(s state.Step) bool { return s.Action == action && s.Object == object })
}

// failure returns the classified failure of an operation error.
func failure(t *testing.T, err error) *operation.Failure {
	t.Helper()
	f, ok := errors.AsType[*operation.Failure](err)
	if !ok {
		t.Fatalf("error %v is not a classified failure", err)
	}
	return f
}

// absentFromHost fails when a value appears in any file under the rig's state, cache or secrets
// roots, or when private key material does.
func absentFromHost(t *testing.T, root string, values ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Errorf("%s holds private key material", path)
		}
		for _, v := range values {
			if v != "" && bytes.Contains(data, []byte(v)) {
				t.Errorf("%s holds a secret value", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// absentFromDocker fails when a value appears in docker inspect or the logs of any container of the stack.
func absentFromDocker(t *testing.T, name string, values ...string) {
	t.Helper()
	for _, id := range testkit.Lines(testkit.Docker(t, "ps", "-a", "-q", "--filter", "name="+name)) {
		logs, _ := testkit.DockerErr("logs", id)
		info := inspect(t, id)
		for _, v := range values {
			if strings.Contains(info, v) {
				t.Errorf("docker inspect of %s holds a secret value", id)
			}
			if strings.Contains(logs, v) {
				t.Errorf("the log of %s holds a secret value", id)
			}
		}
		if strings.Contains(info, "PRIVATE KEY") || strings.Contains(logs, "PRIVATE KEY") {
			t.Errorf("%s exposes private key material", id)
		}
	}
}

// containerEnv decodes the environment of a container into a map.
func containerEnv(t *testing.T, container string) map[string]string {
	t.Helper()
	var infos []struct {
		Config struct{ Env []string }
	}
	if err := json.Unmarshal([]byte(inspect(t, container)), &infos); err != nil || len(infos) != 1 {
		t.Fatalf("inspect %s: %v", container, err)
	}
	out := map[string]string{}
	for _, kv := range infos[0].Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// mounts decodes the mounts and bind list of a container.
func mounts(t *testing.T, container string) (kinds []string, binds []string) {
	t.Helper()
	var infos []struct {
		Mounts     []struct{ Type string }
		HostConfig struct{ Binds []string }
	}
	if err := json.Unmarshal([]byte(inspect(t, container)), &infos); err != nil || len(infos) != 1 {
		t.Fatalf("inspect %s: %v", container, err)
	}
	for _, m := range infos[0].Mounts {
		kinds = append(kinds, m.Type)
	}
	return kinds, infos[0].HostConfig.Binds
}
