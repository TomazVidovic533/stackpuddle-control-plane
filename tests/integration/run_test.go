//go:build integration

package integration_test

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/engine"
)

type scenarioRun struct{ cmd *exec.Cmd }

// killed runs the helper to its end and requires that a signal killed it.
func (r *scenarioRun) killed(t *testing.T) {
	t.Helper()
	err := r.cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 {
		t.Fatalf("the helper was not killed by a signal: %v", err)
	}
}

func noKill() struct{ Effect, Target, Phase string } { return struct{ Effect, Target, Phase string }{} }

func killAt(phase, effect, target string) struct{ Effect, Target, Phase string } {
	return struct{ Effect, Target, Phase string }{Effect: effect, Target: target, Phase: phase}
}

func codeOf(t testing.TB, err error) engine.Code {
	t.Helper()
	failure, ok := errors.AsType[*engine.Error](err)
	if !ok {
		t.Fatalf("error %v is not an *engine.Error", err)
	}
	return failure.Code()
}

var _ = syscall.SIGINT
