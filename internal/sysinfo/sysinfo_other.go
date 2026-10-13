//go:build !linux

package sysinfo

import (
	"runtime"
	"syscall"
)

// Read on non-Linux systems (development): CPU count and disk only.
func Read(dir string) Snapshot {
	s := Snapshot{CPUs: runtime.NumCPU()}
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) == nil {
		s.DiskTotal = uint64(st.Blocks) * uint64(st.Bsize)
		s.DiskFree = uint64(st.Bavail) * uint64(st.Bsize)
	}
	return s
}
