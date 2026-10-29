//go:build linux || darwin

// Package lock excludes a second writer from one environment with an advisory flock on env.flock.
//
// The file is created once and never unlinked: unlinking would let a second writer lock a new file
// while the first still holds the old one. Release closes the descriptor. The body of the file is a
// display-only owner record; the kernel lock is the only truth.
package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// Sentinels for the failures a caller classifies.
var (
	// ErrBusy marks an environment whose lock another operation holds.
	ErrBusy = errors.New("environment is busy")
	// ErrFilesystemUnsupported marks a state folder on a filesystem with unreliable locks.
	ErrFilesystemUnsupported = errors.New("filesystem does not support reliable locks")
)

const heldByOther = "held by another operation"

const (
	ownerLimit     = 4 << 10
	initialBackoff = 50 * time.Millisecond
	maxBackoff     = time.Second
	lockFileMode   = 0o600
)

// Owner is the display-only record of who holds the lock.
type Owner struct {
	Operation string `json:"operation"`
	PID       int    `json:"pid"`
	ChildPID  int    `json:"childPid,omitempty"`
	Host      string `json:"host,omitempty"`
}

// AcquireOptions are the optional seams of Acquire.
type AcquireOptions struct {
	// Wait is how long to keep retrying a busy lock; zero fails at once.
	Wait time.Duration
	// Random supplies the retry jitter; it must not be nil when Wait is positive.
	Random io.Reader
	// Now supplies the time for the wait deadline; nil uses time.Now.
	Now func() time.Time
}

// Held is a held environment lock.
type Held struct {
	file *os.File
}

// File returns the descriptor children inherit so that a live effect keeps the environment busy
// after its parent dies. It stays valid until Release.
func (h *Held) File() *os.File { return h.file }

// SetOwner replaces the display-only owner record.
func (h *Held) SetOwner(owner Owner) error {
	body, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("lock: encode owner: %w", err)
	}
	if err := h.file.Truncate(0); err != nil {
		return fmt.Errorf("lock: truncate owner record: %w", err)
	}
	if _, err := h.file.WriteAt(body, 0); err != nil {
		return fmt.Errorf("lock: write owner record: %w", err)
	}
	return nil
}

// Release closes the descriptor, which drops the lock unless a child still holds a copy. It never
// unlinks the file.
func (h *Held) Release() error {
	if err := h.file.Close(); err != nil {
		return fmt.Errorf("lock: release: %w", err)
	}
	return nil
}

// Acquire takes the exclusive lock on the file at path, creating it with mode 0600 when absent.
// A busy lock fails at once with an error wrapping ErrBusy that names the holder; with
// opts.Wait it retries at 50 ms doubling to 1 s with full jitter until the deadline, and stops when
// ctx ends.
func Acquire(ctx context.Context, path string, opts AcquireOptions) (*Held, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, lockFileMode)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	if err := file.Chmod(lockFileMode); err != nil {
		return nil, errors.Join(fmt.Errorf("lock: chmod %s: %w", path, err), file.Close())
	}
	deadline := now().Add(opts.Wait)
	backoff := initialBackoff
	for {
		err := tryLock(file)
		if err == nil {
			return &Held{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.Join(fmt.Errorf("lock: flock %s: %w", path, err), file.Close())
		}
		if !now().Before(deadline) {
			busy := fmt.Errorf("%w: %s", ErrBusy, describeHolder(path))
			return nil, errors.Join(busy, file.Close())
		}
		pause, jitterErr := jitter(opts.Random, backoff)
		if jitterErr != nil {
			return nil, errors.Join(jitterErr, file.Close())
		}
		if err := sleep(ctx, pause); err != nil {
			return nil, errors.Join(fmt.Errorf("lock: waiting for %s: %w", path, err), file.Close())
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func tryLock(file *os.File) error {
	conn, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := conn.Control(func(fd uintptr) {
		lockErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	}); err != nil {
		return err
	}
	return lockErr
}

func jitter(random io.Reader, ceiling time.Duration) (time.Duration, error) {
	var raw [8]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return 0, fmt.Errorf("lock: jitter: %w", err)
	}
	var n uint64
	for _, b := range raw {
		n = n<<8 | uint64(b)
	}
	return time.Duration(n % uint64(ceiling)), nil
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// describeHolder reads the owner record for a message; it never decides anything.
func describeHolder(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return heldByOther
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, ownerLimit))
	if err != nil || len(body) == 0 {
		return heldByOther
	}
	var owner Owner
	if err := json.Unmarshal(body, &owner); err != nil {
		return heldByOther
	}
	text := fmt.Sprintf("held by operation %s (process %d", owner.Operation, owner.PID)
	if owner.ChildPID != 0 {
		text += fmt.Sprintf(", effect process %d", owner.ChildPID)
	}
	return text + ")"
}
