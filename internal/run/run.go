// Package run is the one door to external programs (docker, git, gh). Every package takes a Runner, so the
// logic around those programs is tested with Fake instead of a real Docker.
package run

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/runsnip/ops/internal/proc"
)

// Call is one program run.
type Call struct {
	Dir    string
	Env    []string // added to this process's environment (KEY=VALUE)
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Name   string
	Args   []string
	// Attach runs the program on this terminal (psql, a shell, logs -f): no new session, the terminal's own
	// stdin/stdout/stderr when the fields above are nil.
	Attach bool
}

// Line is the command line, for messages and for Fake's matching.
func (c Call) Line() string { return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " ")) }

// Runner runs a program.
type Runner interface {
	Run(ctx context.Context, c Call) error
}

// Exec runs real programs.
type Exec struct{}

// Run starts c and waits for it.
func (Exec) Run(ctx context.Context, c Call) error {
	var cmd *exec.Cmd
	if c.Attach {
		cmd = exec.CommandContext(ctx, c.Name, c.Args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd = proc.Command(ctx, c.Name, c.Args...)
	}
	if c.Stdin != nil {
		cmd.Stdin = c.Stdin
	}
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		if cmd.Env == nil {
			cmd.Env = os.Environ()
		}
		cmd.Env = append(cmd.Env, c.Env...)
	}
	return cmd.Run()
}

// Output runs c and returns its stdout; stderr goes into the error.
func Output(ctx context.Context, r Runner, c Call) (string, error) {
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := r.Run(ctx, c); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("%s: %s", c.Line(), msg)
	}
	return out.String(), nil
}

// Fake answers calls from a table: the first rule whose Match is contained in the command line wins. Calls are
// recorded in order.
type Fake struct {
	Rules []Rule
	Calls []Call
}

// Rule is one answer of Fake.
type Rule struct {
	Match  string
	Stdout string
	Stderr string
	Fail   bool
	// Fn, when set, answers instead (it may read the call's stdin).
	Fn func(c Call) (string, error)
}

// On adds a rule answering out.
func (f *Fake) On(match, out string) *Fake {
	f.Rules = append(f.Rules, Rule{Match: match, Stdout: out})
	return f
}

// Failing adds a rule that fails with stderr msg.
func (f *Fake) Failing(match, msg string) *Fake {
	f.Rules = append(f.Rules, Rule{Match: match, Stderr: msg, Fail: true})
	return f
}

// Run answers c.
func (f *Fake) Run(_ context.Context, c Call) error {
	f.Calls = append(f.Calls, c)
	line := c.Line()
	for _, r := range f.Rules {
		if !strings.Contains(line, r.Match) {
			continue
		}
		if r.Fn != nil {
			out, err := r.Fn(c)
			if c.Stdout != nil {
				io.WriteString(c.Stdout, out)
			}
			return err
		}
		if c.Stdout != nil {
			io.WriteString(c.Stdout, r.Stdout)
		}
		if c.Stderr != nil {
			io.WriteString(c.Stderr, r.Stderr)
		}
		if r.Fail {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}
	return fmt.Errorf("fake: no rule for %q", line)
}

// Lines returns every recorded command line.
func (f *Fake) Lines() []string {
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.Line()
	}
	return out
}

// Called reports whether some recorded command line contains s.
func (f *Fake) Called(s string) bool {
	for _, l := range f.Lines() {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}
