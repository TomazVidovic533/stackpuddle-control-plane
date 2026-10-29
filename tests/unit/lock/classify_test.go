package lock_test

import (
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/lock"
)

func TestClassifyMagic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		magic uint32
		want  string
		bad   bool
	}{
		{"NFS", 0x6969, "NFS", true},
		{"SMB", 0x517B, "SMB/CIFS", true},
		{"CIFS", 0xFF534D42, "SMB/CIFS", true},
		{"SMB2", 0xFE534D42, "SMB/CIFS", true},
		{"FUSE", 0x65735546, "FUSE", true},
		{"ext4", 0xEF53, "", false},
		{"xfs", 0x58465342, "", false},
		{"btrfs", 0x9123683E, "", false},
		{"tmpfs", 0x01021994, "", false},
		{"overlayfs", 0x794C7630, "", false},
		{"zero", 0, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, bad := lock.ClassifyMagic(test.magic)
			if got != test.want || bad != test.bad {
				t.Errorf("ClassifyMagic(%#x) = %q, %v, want %q, %v", test.magic, got, bad, test.want, test.bad)
			}
		})
	}
}

func TestClassifyFstype(t *testing.T) {
	t.Parallel()
	tests := []struct {
		fstype string
		want   string
		bad    bool
	}{
		{"nfs", "NFS", true},
		{"smbfs", "SMB", true},
		{"afpfs", "AFP", true},
		{"macfuse", "FUSE", true},
		{"osxfuse", "FUSE", true},
		{"apfs", "", false},
		{"hfs", "", false},
		{"", "", false},
	}
	for _, test := range tests {
		t.Run(test.fstype, func(t *testing.T) {
			t.Parallel()
			got, bad := lock.ClassifyFstype(test.fstype)
			if got != test.want || bad != test.bad {
				t.Errorf("ClassifyFstype(%q) = %q, %v, want %q, %v", test.fstype, got, bad, test.want, test.bad)
			}
		})
	}
}
