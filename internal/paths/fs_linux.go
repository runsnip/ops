//go:build linux

package paths

import (
	"syscall"

	"golang.org/x/sys/unix"
)

var fsNames = map[int64]string{0x01021994: "tmpfs", 0x858458f6: "ramfs", 0x9fa0: "proc", 0x62656572: "sysfs", 0x6969: "nfs",
	0xff534d42: "cifs", 0xfe534d42: "smb3", 0x794c7630: "overlay", 0xef53: "ext4", 0x58465342: "xfs", 0x9123683e: "btrfs",
	0x01021997: "9p", 0x65735546: "fuse"}

func fsInfo(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	t := fsNames[int64(st.Type)]
	if t == "" {
		t = "other"
	}
	return FSInfo{Type: t, FreeBytes: st.Bavail * uint64(st.Bsize)}, nil
}

func writable(path string) bool { return unix.Access(path, unix.W_OK) == nil }
