// Package sysinfo reads host CPU, memory, swap, load and disk usage (Linux /proc; other systems report what they can).
package sysinfo

// Snapshot is the host state at one moment. CPU needs two snapshots (see CPUPercent).
type Snapshot struct {
	CPUs                 int
	cpuBusy, cpuTotal    uint64
	Load1, Load5, Load15 float64
	MemTotal, MemAvail   uint64 // bytes
	SwapTotal, SwapFree  uint64
	DiskTotal, DiskFree  uint64  // of the stack directory's filesystem
	Uptime               float64 // seconds
	Supported            bool    // false where /proc is missing (development on macOS)
}

// CPUPercent is the busy share between two snapshots (0 when unknown).
func CPUPercent(prev, cur Snapshot) float64 {
	dt := float64(cur.cpuTotal - prev.cpuTotal)
	if prev.cpuTotal == 0 || dt <= 0 {
		return 0
	}
	return 100 * float64(cur.cpuBusy-prev.cpuBusy) / dt
}

// HumanBytes formats bytes with binary units (1.5 GiB).
func HumanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return itoa(b) + " B"
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	v := float64(b) / float64(div)
	return trim(v) + " " + string("KMGTPE"[exp]) + "iB"
}

func itoa(b uint64) string {
	if b == 0 {
		return "0"
	}
	var s []byte
	for ; b > 0; b /= 10 {
		s = append([]byte{byte('0' + b%10)}, s...)
	}
	return string(s)
}

func trim(v float64) string {
	i := uint64(v*10 + 0.5)
	if i%10 == 0 {
		return itoa(i / 10)
	}
	return itoa(i/10) + "." + itoa(i%10)
}
