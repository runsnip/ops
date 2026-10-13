package proc

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCommandHasNoTerminalAndStopsWithItsChildren(t *testing.T) {
	out, err := Command(context.Background(), "sh", "-c", "(exec 3</dev/tty) 2>/dev/null && echo tty || echo none").Output()
	if err != nil || strings.TrimSpace(string(out)) != "none" {
		t.Fatalf("step reached the terminal: %q %v", out, err)
	}

	// A grandchild that keeps stdout open, like `docker buildx` under release.sh.
	ctx, cancel := context.WithCancel(context.Background())
	cmd := Command(ctx, "sh", "-c", "sleep 60 & echo $!; wait")
	pipe, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	n, _ := pipe.Read(buf)
	var grandchild int
	for _, c := range strings.TrimSpace(string(buf[:n])) {
		grandchild = grandchild*10 + int(c-'0')
	}
	cancel()
	done := make(chan error, 1)
	go func() { _, _ = pipe.Read(make([]byte, 1)); done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("stop did not end the step")
	}
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(grandchild, 0) == nil {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatal("grandchild survived the stop")
	}
}

func TestKillEndsAGroupThatIgnoresSIGTERM(t *testing.T) {
	cmd := Command(context.Background(), "sh", "-c", "trap '' TERM; sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	Stop(cmd.Process.Pid)
	time.Sleep(200 * time.Millisecond)
	if syscall.Kill(cmd.Process.Pid, 0) != nil {
		t.Fatal("expected the step to ignore SIGTERM")
	}
	Kill(cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Kill did not end the step")
	}
}
