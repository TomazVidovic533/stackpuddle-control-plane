package lock

import "fmt"

// Linux statfs magic numbers of filesystems whose flock is unreliable or only advisory per host.
const (
	magicNFS  = 0x6969
	magicSMB  = 0x517B
	magicCIFS = 0xFF534D42
	magicSMB2 = 0xFE534D42
	magicFUSE = 0x65735546
)

// ClassifyMagic names the network or FUSE filesystem a Linux statfs magic number identifies. ok is
// false for any other filesystem.
func ClassifyMagic(magic uint32) (name string, ok bool) {
	switch magic {
	case magicNFS:
		return "NFS", true
	case magicSMB, magicSMB2, magicCIFS:
		return "SMB/CIFS", true
	case magicFUSE:
		return "FUSE", true
	}
	return "", false
}

// ClassifyFstype names the network or FUSE filesystem a macOS filesystem type name identifies. ok is
// false for any other name.
func ClassifyFstype(fstype string) (name string, ok bool) {
	switch fstype {
	case "nfs":
		return "NFS", true
	case "smbfs":
		return "SMB", true
	case "afpfs":
		return "AFP", true
	case "macfuse", "osxfuse":
		return "FUSE", true
	}
	return "", false
}

func unsupported(dir, kind, setting string) error {
	return fmt.Errorf("%w: %s is on %s; set %s to a folder on a local disk", ErrFilesystemUnsupported, dir, kind, setting)
}
