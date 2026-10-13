//go:build !linux

package paths

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func fsInfo(path string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return FSInfo{}, err
	}
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return FSInfo{Type: string(name), FreeBytes: uint64(st.Bavail) * uint64(st.Bsize)}, nil
}

func writable(path string) bool { return unix.Access(path, unix.W_OK) == nil }
