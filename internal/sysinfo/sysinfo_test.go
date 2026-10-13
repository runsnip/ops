package sysinfo

import "testing"

func TestHumanBytesAndCPU(t *testing.T) {
	for in, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 8 << 30: "8 GiB", 3*(1<<40) + (1 << 39): "3.5 TiB"} {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if p := CPUPercent(Snapshot{cpuBusy: 100, cpuTotal: 1000}, Snapshot{cpuBusy: 150, cpuTotal: 1100}); p != 50 {
		t.Fatal(p)
	}
	if CPUPercent(Snapshot{}, Snapshot{cpuBusy: 1, cpuTotal: 2}) != 0 {
		t.Fatal("first sample must be 0")
	}
	if s := Read("."); s.CPUs < 1 || s.DiskTotal == 0 {
		t.Fatalf("%+v", s)
	}
}
