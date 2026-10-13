//go:build linux

package sysinfo

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Read takes a snapshot; dir selects the filesystem for the disk figures.
func Read(dir string) Snapshot {
	s := Snapshot{CPUs: runtime.NumCPU(), Supported: true}
	if f, err := os.Open("/proc/stat"); err == nil {
		sc := bufio.NewScanner(f)
		if sc.Scan() {
			fields := strings.Fields(sc.Text()) // cpu user nice system idle iowait irq softirq steal …
			var vals []uint64
			for _, x := range fields[1:] {
				v, _ := strconv.ParseUint(x, 10, 64)
				vals = append(vals, v)
			}
			for i, v := range vals {
				if i < 8 { // guest time is already counted in user/nice
					s.cpuTotal += v
				}
			}
			if len(vals) > 4 {
				s.cpuBusy = s.cpuTotal - vals[3] - vals[4] // minus idle and iowait
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(b))
		if len(f) >= 3 {
			s.Load1, _ = strconv.ParseFloat(f[0], 64)
			s.Load5, _ = strconv.ParseFloat(f[1], 64)
			s.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			n, _ := strconv.ParseUint(strings.Fields(v)[0], 10, 64)
			n *= 1024
			switch k {
			case "MemTotal":
				s.MemTotal = n
			case "MemAvailable":
				s.MemAvail = n
			case "SwapTotal":
				s.SwapTotal = n
			case "SwapFree":
				s.SwapFree = n
			}
		}
		f.Close()
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		s.Uptime, _ = strconv.ParseFloat(strings.Fields(string(b))[0], 64)
	}
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) == nil {
		s.DiskTotal = st.Blocks * uint64(st.Bsize)
		s.DiskFree = st.Bavail * uint64(st.Bsize)
	}
	return s
}
