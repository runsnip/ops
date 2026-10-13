// Package dbx moves a stack's database between a server and this machine, for the operator who is about to change
// that server: `db pull` (a verified dump streamed over SSH), `db tunnel` (a local port to the database, nothing
// published on the server) and `db restore` (into a throwaway local container, or — strictly — into a new database
// on a server).
//
// It needs nothing on the server but sshd and docker: the database container is found from the compose labels
// docker keeps, its user and database are read from the container's own environment inside the container, and no
// secret crosses the command line or the log.
package dbx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/runsnip/ops/internal/run"
)

// Quote makes s one word for a POSIX shell: as it is when it holds only safe characters, else single-quoted.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,+%", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes every word and joins them into one command line.
func Join(words ...string) string {
	q := make([]string, len(words))
	for i, w := range words {
		q[i] = Quote(w)
	}
	return strings.Join(q, " ")
}

// Host is where the database runs: a server over SSH, or this machine (--local).
type Host interface {
	// Sh runs a POSIX sh script there. stdin may be nil (then the script reads nothing).
	Sh(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error
	// Name is the server as the operator named it ("local" for this machine).
	Name() string
	Close()
}

// Local runs on this machine.
type Local struct{ Run run.Runner }

func (l Local) Sh(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	return l.Run.Run(ctx, run.Call{Name: "sh", Args: []string{"-c", script}, Stdin: stdin, Stdout: stdout, Stderr: stderr})
}
func (Local) Name() string { return "local" }
func (Local) Close()       {}

// SSH runs on a server through the system ssh: the operator's keys, agent, known_hosts and ~/.ssh/config. One
// master connection is opened first (any passphrase or host-key question is asked then, on the terminal) and every
// later command rides on it, so a pull authenticates once.
type SSH struct {
	Target string
	// Cmd is the ssh program and its options: RUNSNIP_SSH (like GIT_SSH_COMMAND), else "ssh".
	Cmd []string
	Run run.Runner
	// Compress asks ssh to compress the stream (plain SQL dumps; a pg_dump -Fc archive is compressed already).
	Compress bool
	ctl      string
}

// SSHCommand is the ssh program and options from RUNSNIP_SSH.
func SSHCommand() []string {
	if v := strings.Fields(os.Getenv("RUNSNIP_SSH")); len(v) > 0 {
		return v
	}
	return []string{"ssh"}
}

func (s *SSH) base() []string {
	a := append([]string{}, s.Cmd[1:]...)
	a = append(a, "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4")
	if s.ctl != "" {
		a = append(a, "-o", "ControlPath="+filepath.Join(s.ctl, "m"))
	}
	return a
}

// Open starts the master connection. It runs on this terminal so ssh can ask for a passphrase or a host key.
func (s *SSH) Open(ctx context.Context) error {
	if strings.HasPrefix(s.Target, "-") || s.Target == "" {
		return fmt.Errorf("bad ssh target %q", s.Target)
	}
	// A socket path must stay under ~100 bytes: /tmp, not the per-user temp folder of macOS.
	dir, err := os.MkdirTemp("/tmp", "runsnip-ssh-")
	if err != nil {
		return err
	}
	s.ctl = dir
	a := append(s.base(), "-M", "-N", "-f", "-o", "ControlMaster=yes", "-o", "ControlPersist=600", "--", s.Target)
	if err := s.Run.Run(ctx, run.Call{Name: s.Cmd[0], Args: a, Attach: true, Stdout: os.Stderr}); err != nil {
		os.RemoveAll(dir)
		s.ctl = ""
		return fmt.Errorf("ssh %s: %v", s.Target, err)
	}
	return nil
}

// Args is the ssh command line for one remote command (on the master connection).
func (s *SSH) Args(remote string) []string {
	a := append(s.base(), "-T", "-o", "ControlMaster=no")
	if s.Compress {
		a = append(a, "-C")
	}
	return append(a, "--", s.Target, remote)
}

func (s *SSH) Sh(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	// The login shell may be anything: hand the script to sh as one quoted word.
	err := s.Run.Run(ctx, run.Call{Name: s.Cmd[0], Args: s.Args(Join("sh", "-c", script)), Stdin: stdin, Stdout: stdout, Stderr: stderr})
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) && ee.ExitCode() == 255 {
		return fmt.Errorf("ssh %s: the connection failed (exit 255)", s.Target)
	}
	return err
}

func (s *SSH) Name() string { return s.Target }

// Close ends the master connection.
func (s *SSH) Close() {
	if s.ctl == "" {
		return
	}
	_ = s.Run.Run(context.Background(), run.Call{Name: s.Cmd[0], Args: append(s.base(), "-O", "exit", "--", s.Target), Stdout: io.Discard, Stderr: io.Discard})
	os.RemoveAll(s.ctl)
	s.ctl = ""
}

// Docker is how docker is called on the host: "docker", or "sudo -n docker" for a user outside the docker group.
type Docker struct{ Sudo bool }

func (d Docker) Cmd(args ...string) string {
	w := []string{"docker"}
	if d.Sudo {
		w = []string{"sudo", "-n", "docker"}
	}
	return Join(append(w, args...)...)
}

// Exec is a script run by sh inside a container, with stdin attached (nil stdin reads nothing).
func (d Docker) Exec(container, script string) string {
	return d.Cmd("exec", "-i", container, "sh", "-c", script)
}
