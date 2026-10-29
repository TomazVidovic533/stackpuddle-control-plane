package lock

import (
	"fmt"
	"syscall"
)

// CheckFilesystem refuses a folder on NFS, SMB, CIFS or FUSE with an error wrapping
// ErrFilesystemUnsupported that names dir and the setting that moves the state root.
func CheckFilesystem(dir, setting string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("lock: statfs %s: %w", dir, err)
	}
	if name, bad := ClassifyMagic(uint32(st.Type)); bad {
		return unsupported(dir, name, setting)
	}
	return nil
}
