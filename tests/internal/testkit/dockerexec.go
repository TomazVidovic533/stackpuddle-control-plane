package testkit

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Docker runs the real docker CLI against the default context and returns trimmed output, failing
// the test on error.
func Docker(t testing.TB, args ...string) string {
	t.Helper()
	out, err := DockerErr(args...)
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// DockerErr runs docker and returns its combined trimmed output and error.
func DockerErr(args ...string) (string, error) { return DockerStdin("", args...) }

// DockerStdin runs docker with stdin and returns its combined trimmed output and error.
func DockerStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	err := cmd.Run()
	return strings.TrimSpace(stdout.String() + stderr.String()), err
}

// Lines splits output into lines; empty output is no lines.
func Lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
