// Package durable writes files so that a crash leaves the old content or the new, never a mix.
package durable

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// Stage names a point inside WriteFile, for deterministic fault injection in tests.
type Stage int

// The stages of one write, in order.
const (
	// TempWritten is after the temporary file holds all the data and before it is synced.
	TempWritten Stage = iota + 1
	// TempSynced is after the temporary file is synced and closed, before the rename.
	TempSynced
	// Renamed is after the rename, before the folder is synced.
	Renamed
	// DirSynced is after the folder is synced.
	DirSynced
)

// String returns the stage name.
func (s Stage) String() string {
	switch s {
	case TempWritten:
		return "temp-written"
	case TempSynced:
		return "temp-synced"
	case Renamed:
		return "renamed"
	case DirSynced:
		return "dir-synced"
	}
	return "unknown"
}

// Options are the optional seams of WriteFile.
type Options struct {
	// Perm is the mode of the written file; it is applied explicitly, so the umask cannot widen it.
	Perm fs.FileMode
	// Random supplies the temporary file suffix; nil uses crypto/rand.
	Random io.Reader
	// After is called at each Stage; a non-nil error aborts the write, and the temporary file is
	// removed unless the rename already happened.
	After func(Stage) error
}

const tempSuffixBytes = 8

// WriteFile writes data to name inside dir through a temporary file in the same folder: write, fsync,
// close, rename, fsync the folder. The file has mode opts.Perm regardless of the umask. On any error
// before the rename the temporary file is removed and name keeps its old content.
func WriteFile(dir *os.Root, name string, data []byte, opts Options) error {
	random := opts.Random
	if random == nil {
		random = rand.Reader
	}
	suffix := make([]byte, tempSuffixBytes)
	if _, err := io.ReadFull(random, suffix); err != nil {
		return fmt.Errorf("durable: temporary name for %s: %w", name, err)
	}
	temp := "." + name + ".tmp-" + hex.EncodeToString(suffix)

	if err := writeTemp(dir, temp, data, opts); err != nil {
		return errors.Join(err, removeIfExists(dir, temp))
	}
	if err := dir.Rename(temp, name); err != nil {
		return errors.Join(fmt.Errorf("durable: rename into %s: %w", name, err), removeIfExists(dir, temp))
	}
	if err := stage(opts, Renamed); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("durable: sync folder after %s: %w", name, err)
	}
	return stage(opts, DirSynced)
}

func writeTemp(dir *os.Root, temp string, data []byte, opts Options) error {
	file, err := dir.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, opts.Perm)
	if err != nil {
		return fmt.Errorf("durable: create %s: %w", temp, err)
	}
	if err := file.Chmod(opts.Perm); err != nil {
		return errors.Join(fmt.Errorf("durable: chmod %s: %w", temp, err), file.Close())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(fmt.Errorf("durable: write %s: %w", temp, err), file.Close())
	}
	if err := stage(opts, TempWritten); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := file.Sync(); err != nil {
		return errors.Join(fmt.Errorf("durable: sync %s: %w", temp, err), file.Close())
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("durable: close %s: %w", temp, err)
	}
	return stage(opts, TempSynced)
}

func stage(opts Options, s Stage) error {
	if opts.After == nil {
		return nil
	}
	if err := opts.After(s); err != nil {
		return fmt.Errorf("durable: stopped after %s: %w", s, err)
	}
	return nil
}

func syncDir(dir *os.Root) error {
	handle, err := dir.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(handle.Sync(), handle.Close())
}

func removeIfExists(dir *os.Root, name string) error {
	if err := dir.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
