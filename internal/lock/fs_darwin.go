package lock

import (
	"fmt"
	"syscall"
)

// CheckFilesystem refuses a folder on NFS, SMB, AFP or FUSE with an error wrapping
// ErrFilesystemUnsupported that names dir and the setting that moves the state root.
func CheckFilesystem(dir, setting string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return fmt.Errorf("lock: statfs %s: %w", dir, err)
	}
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	if kind, bad := ClassifyFstype(string(name)); bad {
		return unsupported(dir, kind, setting)
	}
	return nil
}
