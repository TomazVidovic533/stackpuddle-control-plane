//go:build linux || darwin

package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
)

// ErrCommand marks a child that exited non-zero or could not run.
var ErrCommand = errors.New("docker command failed")

// exitError is a child that ran and exited non-zero. It is an ErrCommand, and the only kind of
// failure a probe reads as a result instead of an error.
type exitError struct {
	args   string
	code   int
	stderr string
}

func (e *exitError) Error() string {
	return fmt.Sprintf("%s: %s exited %d: %s", ErrCommand, e.args, e.code, e.stderr)
}

// Is makes errors.Is(err, ErrCommand) true.
func (*exitError) Is(target error) bool { return target == ErrCommand }

// ErrTimeout marks a child that outlived its budget; its outcome is unknown and is observed next run.
var ErrTimeout = errors.New("docker command timed out")

const (
	outputLimit = 1 << 20
	killGrace   = 10 * time.Second
	redacted    = "[redacted]"
)

// Timeouts per kind of call (D54).
const (
	timeoutInfo    = 15 * time.Second
	timeoutObserve = 30 * time.Second
	timeoutImage   = 10 * time.Minute
	timeoutEffect  = 2 * time.Minute
	timeoutProbe   = 2 * time.Minute
)

// boundedBuffer keeps at most outputLimit bytes and drops the rest.
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := outputLimit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// child describes one docker invocation.
type child struct {
	args    []string
	stdin   []byte
	env     []string
	timeout time.Duration
}

// result is a finished child's bounded output with every secret scrubbed.
type result struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

// run starts docker against the declared context in its own process group with the allowlisted
// environment, plus env for this call only, and the held environment lock as an inherited file. It
// does not use a cancellable command: a cancelled context never kills an effect mid-way, it only
// stops a new one from starting. On timeout the group gets SIGTERM, then SIGKILL after a grace
// period, and the step outcome is unknown.
func (a *Adapter) run(ctx context.Context, call port.Call, c child) (result, error) {
	if err := ctx.Err(); err != nil {
		return result{}, fmt.Errorf("compose: not started: %w", context.Cause(ctx))
	}
	args := append([]string{"--context", call.DockerContext}, c.args...)
	cmd := exec.Command(a.docker, args...) //nolint:noctx // reason: a cancelled context must never kill an effect mid-way; timeouts are enforced by waitOrKill
	cmd.Env = append(a.childEnv(), c.env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if call.LockFile != nil {
		cmd.ExtraFiles = []*os.File{call.LockFile}
	}
	if c.stdin != nil {
		cmd.Stdin = bytes.NewReader(c.stdin)
	}
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return result{}, fmt.Errorf("%w: start: %w", ErrCommand, err)
	}
	if call.OnChild != nil {
		call.OnChild(cmd.Process.Pid)
	}
	waitErr := waitOrKill(cmd, c.timeout)
	res := result{stdout: scrub(stdout.bytes(), call), stderr: scrub(stderr.bytes(), call)}
	if cmd.ProcessState != nil {
		res.exitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(waitErr, ErrTimeout) {
		return res, fmt.Errorf("%w: %w: %s", ErrTimeout, plan.ErrOutcomeUnknown, firstArgs(c.args))
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return res, &exitError{args: firstArgs(c.args), code: res.exitCode, stderr: tail(res.stderr)}
	}
	if waitErr != nil {
		return res, fmt.Errorf("%w: %s: %w", ErrCommand, firstArgs(c.args), waitErr)
	}
	return res, nil
}

func waitOrKill(cmd *exec.Cmd, timeout time.Duration) error {
	var waitErr error
	finished := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(finished)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-finished:
		return waitErr
	case <-timer.C:
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	grace := time.NewTimer(killGrace)
	defer grace.Stop()
	select {
	case <-finished:
	case <-grace.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-finished
	}
	return ErrTimeout
}

// scrub replaces every secret value of the call in output, so no value reaches an error or evidence.
func scrub(out []byte, call port.Call) []byte {
	for _, v := range call.Secrets {
		if raw := v.Reveal(); len(raw) > 0 {
			out = bytes.ReplaceAll(out, raw, []byte(redacted))
		}
	}
	return out
}

func firstArgs(args []string) string {
	if len(args) > 2 {
		return strings.Join(args[:2], " ")
	}
	return strings.Join(args, " ")
}

func tail(b []byte) string {
	const keep = 400
	s := strings.TrimSpace(string(b))
	if len(s) > keep {
		s = "..." + s[len(s)-keep:]
	}
	return s
}
