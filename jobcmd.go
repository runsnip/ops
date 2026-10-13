package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
	"github.com/runsnip/ops/internal/stack"
)

// globals are the options every command takes before its name.
type globals struct {
	stack string        // --stack <name>: a registered stack
	wait  time.Duration // --wait <d>: how long to wait for a stack's lock (default 0: fail at once, saying who holds it)
	state string        // the state folder (RUNSNIP_STATE, else /var/lib/runsnip)
}

func parseGlobals(args []string) (globals, []string, error) {
	g := globals{state: host.Dir()}
	for len(args) > 0 {
		switch {
		case args[0] == "--stack" && len(args) > 1:
			g.stack, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--stack="):
			g.stack, args = strings.TrimPrefix(args[0], "--stack="), args[1:]
		case args[0] == "--wait" && len(args) > 1:
			d, err := time.ParseDuration(args[1])
			if err != nil {
				return g, nil, fmt.Errorf("--wait %s: %v", args[1], err)
			}
			g.wait, args = d, args[2:]
		default:
			return g, args, nil
		}
	}
	return g, args, nil
}

// serverMode: runsnip keeps jobs, locks and the audit log only where its state folder exists (install.sh creates it).
func serverMode(g globals) bool {
	st, err := os.Stat(g.state)
	return err == nil && st.IsDir()
}

// openStack opens --stack from the registry, else the stack of this folder (RUNSNIP_HOME, the program's, the current).
func openStack(g globals) (*stack.Stack, string, error) {
	if g.stack != "" {
		e, ok, err := host.Find(g.state, g.stack)
		if err != nil {
			return nil, "", err
		}
		if !ok {
			names := []string{}
			all, _ := host.Stacks(g.state)
			for _, s := range all {
				names = append(names, s.Name)
			}
			return nil, "", fmt.Errorf("no stack %q registered (runsnip stack list): %s", g.stack, strings.Join(names, " "))
		}
		st, err := stack.Open(e.Dir)
		if err == nil {
			st.GitEnv = gitSSH(g.state, e.KeyFile)
		}
		return st, e.Name, err
	}
	dir := stackDir()
	st, err := stack.Open(dir)
	if err != nil {
		return nil, "", err
	}
	name := st.Name()
	if all, err := host.Stacks(g.state); err == nil {
		for _, e := range all {
			if same(e.Dir, dir) {
				name = e.Name
				st.GitEnv = gitSSH(g.state, e.KeyFile)
			}
		}
	}
	return st, name, nil
}

func same(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// isJob: the commands that change something run as jobs (recorded, locked); reading commands do not.
func isJob(cmd string, args []string) bool {
	switch cmd {
	case "start", "deploy", "update", "up", "restart", "redeploy", "reload", "stop", "down", "pin", "rollback",
		"backup", "prune", "login", "init":
		return true
	case "infra":
		return len(args) > 0 && args[0] == "pull"
	case "env":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "set", "generate", "services":
			return true
		case "combine":
			for _, a := range args[1:] {
				if a == "--apply" {
					return true
				}
			}
		}
	}
	return false
}

// who ran a command: the web admin (RUNSNIP_JOB_USER, set by serve), else the person behind sudo, else the user.
func who() string {
	if u := os.Getenv("RUNSNIP_JOB_USER"); u != "" {
		return u
	}
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func via() string {
	switch {
	case os.Getenv("RUNSNIP_JOB_ID") != "":
		return "web"
	case os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "":
		return "ssh"
	}
	return "cli"
}

// runJob records a command as a job — its log beside the terminal's output — under the stack's lock, and audits it.
// A command serve starts arrives with RUNSNIP_JOB_ID: serve created the record and streams the log as it grows.
func runJob(ctx context.Context, g globals, st *stack.Stack, name, cmd string, args []string, out io.Writer, fn func(io.Writer) (int, error)) (int, error) {
	if !serverMode(g) {
		return fn(out)
	}
	argv := append([]string{cmd}, args...)
	var j jobs.Job
	var err error
	if id := os.Getenv("RUNSNIP_JOB_ID"); id != "" {
		if j, err = jobs.Get(g.state, id); err != nil {
			return 1, fmt.Errorf("job %s: %v", id, err)
		}
	} else if j, err = jobs.Create(g.state, jobs.Job{Stack: name, Command: argv, User: who(), Via: via()}); err != nil {
		return 1, err
	}
	j.PID = os.Getpid()
	_ = jobs.Update(g.state, j)
	// A job serve started already writes everything into its log (main); one started here tees into it.
	w := out
	var log io.Writer = io.Discard
	if os.Getenv("RUNSNIP_JOB_ID") == "" {
		f, err := jobs.OpenLog(g.state, j.ID)
		if err != nil {
			return 1, err
		}
		defer f.Close()
		w, log = jobs.Tee(out, f), f
	}
	release, err := jobs.Lock(g.state, name, jobs.Holder{Job: j.ID, Command: strings.Join(argv, " "), User: j.User}, g.wait)
	if err != nil {
		fmt.Fprintln(log, "✗ "+err.Error())
		_ = jobs.Finish(g.state, j, 1)
		audit(g, j, 1)
		return 1, err
	}
	defer release()
	code, err := fn(w)
	if err != nil {
		fmt.Fprintln(log, "✗ "+err.Error())
		if code == 0 {
			code = 1
		}
	}
	_ = jobs.Finish(g.state, j, code)
	audit(g, j, code)
	return code, err
}

func audit(g globals, j jobs.Job, exit int) {
	_ = host.Audit(g.state, map[string]any{"event": "job", "job": j.ID, "stack": j.Stack, "command": j.Command, "user": j.User, "via": j.Via, "exit": exit})
}

const jobsUsage = `runsnip jobs [--stack name] [-n N]     the latest jobs (every command that changed something, from anywhere)
runsnip jobs show <id>                  a job's record and its whole log`

func jobsCmd(g globals, args []string, out io.Writer) (int, error) {
	if len(args) > 0 && args[0] == "show" {
		if len(args) < 2 {
			return 2, errors.New(jobsUsage)
		}
		j, err := jobs.Get(g.state, args[1])
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "%s  %s  runsnip %s\n  by %s via %s, %s", j.ID, j.Stack, strings.Join(j.Command, " "), j.User, j.Via, j.Status)
		if j.Exit != nil {
			fmt.Fprintf(out, " (exit %d)", *j.Exit)
		}
		fmt.Fprintln(out)
		b, _ := os.ReadFile(jobs.LogPath(g.state, j.ID))
		out.Write(b)
		return 0, nil
	}
	n := 20
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			fmt.Sscan(args[i+1], &n)
			i++
		}
	}
	list, err := jobs.List(g.state, g.stack, n)
	if err != nil {
		return 1, err
	}
	for _, j := range list {
		exit := " "
		if j.Exit != nil {
			exit = fmt.Sprint(*j.Exit)
		}
		fmt.Fprintf(out, "%s  %-8s %-3s %-12s %-10s %-4s runsnip %s\n", j.ID, j.Status, exit, j.Stack, j.User, j.Via, strings.Join(j.Command, " "))
	}
	return 0, nil
}
