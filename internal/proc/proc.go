// Package proc starts the external programs the TUIs drive (release scripts, docker compose).
package proc

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Command runs name in its own session. Without a controlling terminal it cannot switch the
// TUI's terminal out of raw mode (docker, turbo and ssh open /dev/tty), and cancelling ctx
// stops the whole group, so no grandchild (`docker buildx`, the compose plugin) is left
// behind holding the output pipe open.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		Stop(pid)
		go func() {
			time.Sleep(5 * time.Second)
			Kill(pid)
		}()
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// Stop asks the group led by pid (a Command) to end.
func Stop(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
}

// Kill ends the group led by pid at once.
func Kill(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
