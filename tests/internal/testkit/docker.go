package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// DockerShim installs a stand-in docker binary in a fresh PATH directory. It logs every invocation
// and fails, so a test can prove that no Docker call happened before a refusal. It returns the PATH
// to inject and the function that reads the log.
func DockerShim(t testing.TB) (path string, calls func() []string) {
	t.Helper()
	dir := Root(t)
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\nexit 1\n"
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() []string {
		data, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}
