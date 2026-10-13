package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/envmodel"
	"github.com/runsnip/ops/internal/expand"
	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
	"github.com/runsnip/ops/internal/paths"
	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/stack"
)

const stackUsage = `runsnip stack — the stacks this server runs (several per server)

  runsnip stack list [--json]
  runsnip stack check-paths --name N --dir PATH --data PATH [--json]
                       whether PATH may hold the checkout / the data, every refusal with its reason
  runsnip stack keygen <name>        a read-only deploy key for a private repository: prints the public key
  runsnip stack add <name> --repo URL [--branch B] --dir PATH --data PATH
                       check the paths, clone the infra branch, read stack.yaml, register it
  runsnip stack remove <name> [--down]   forget it (--down first brings it down); files and data stay

The paths are the maintainer's: runsnip never picks them, it only refuses one with a reason.`

func stackCmd(ctx context.Context, g globals, args []string, out io.Writer) (int, error) {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		all, err := host.Stacks(g.state)
		if err != nil {
			return 1, err
		}
		if len(args) > 0 && args[0] == "--json" {
			return 0, json.NewEncoder(out).Encode(all)
		}
		for _, s := range all {
			fmt.Fprintf(out, "%-16s %-32s %-32s %s %s\n", s.Name, s.Dir, s.Data, s.Repo, s.Branch)
		}
		return 0, nil
	case "check-paths":
		fs := flag.NewFlagSet("check-paths", flag.ContinueOnError)
		name := fs.String("name", "", "the stack's name")
		dir := fs.String("dir", "", "the checkout's path")
		data := fs.String("data", "", "the data path")
		asJSON := fs.Bool("json", false, "print JSON")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		rs := paths.Check(*name, *dir, *data, paths.Options{State: g.state})
		if *name != "" {
			if err := host.ValidName(*name); err != nil {
				rs = append(rs, paths.Result{Kind: "name", Path: *name, Reasons: []string{err.Error()}})
			} else if e, ok, _ := host.Find(g.state, *name); ok && e.Dir != *dir {
				rs = append(rs, paths.Result{Kind: "name", Path: *name, Reasons: []string{"a stack is already registered under this name (at " + e.Dir + ")"}})
			}
		}
		if *asJSON {
			json.NewEncoder(out).Encode(rs)
		} else {
			for _, r := range rs {
				mark := "✓"
				if !r.OK && r.Kind != "name" || len(r.Reasons) > 0 {
					mark = "✗"
				}
				fmt.Fprintf(out, "%s %-5s %s\n", mark, r.Kind, r.Path)
				for _, x := range r.Reasons {
					fmt.Fprintln(out, "    ✗ "+x)
				}
				for _, x := range r.Warnings {
					fmt.Fprintln(out, "    ⚠ "+x)
				}
			}
		}
		for _, r := range rs {
			if len(r.Reasons) > 0 {
				return 1, nil
			}
		}
		return 0, nil
	case "keygen":
		if len(args) != 1 {
			return 2, errors.New("usage: runsnip stack keygen <name>")
		}
		pub, _, err := deployKey(g.state, args[0])
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(out, pub)
		return 0, nil
	case "add":
		return stackAdd(ctx, g, args, out)
	case "remove":
		if len(args) < 1 {
			return 2, errors.New("usage: runsnip stack remove <name> [--down]")
		}
		name := args[0]
		e, ok, err := host.Find(g.state, name)
		if err != nil || !ok {
			return 1, fmt.Errorf("no stack %q", name)
		}
		g.stack = name
		return runJob(ctx, g, nil, name, "stack", append([]string{"remove"}, args...), out, func(w io.Writer) (int, error) {
			if len(args) > 1 && args[1] == "--down" {
				st, err := stack.Open(e.Dir)
				if err != nil {
					return 1, err
				}
				if err := st.Stop(ctx, w, ""); err != nil {
					return 1, err
				}
			}
			if err := host.Unregister(g.state, name); err != nil {
				return 1, err
			}
			stack.Say(w, "%s forgotten; %s and %s stay as they are", name, e.Dir, e.Data)
			return 0, nil
		})
	}
	fmt.Fprintln(out, stackUsage)
	if sub == "" || sub == "help" {
		return 0, nil
	}
	return 2, fmt.Errorf("unknown: runsnip stack %s", sub)
}

// deployKey returns the stack's deploy key (made once, Ed25519): its public line and the private key's file.
func deployKey(state, name string) (pub, file string, err error) {
	if err := host.ValidName(name); err != nil {
		return "", "", err
	}
	file = filepath.Join(state, "keys", name)
	if b, err := os.ReadFile(file + ".pub"); err == nil {
		return strings.TrimSpace(string(b)), file, nil
	}
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, "runsnip "+name)
	if err != nil {
		return "", "", err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", "", err
	}
	host, _ := os.Hostname()
	pub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " runsnip-" + name + "@" + host
	if err := hostWrite(file, pem.EncodeToMemory(block)); err != nil {
		return "", "", err
	}
	return pub, file, hostWrite(file+".pub", []byte(pub+"\n"))
}

func hostWrite(p string, b []byte) error { return host.WriteFile(p, b) }

// gitSSH is the git environment that uses a stack's deploy key (and pins the host's key on first use).
func gitSSH(state, keyFile string) []string {
	if keyFile == "" {
		return []string{"GIT_TERMINAL_PROMPT=0"}
	}
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -i " + keyFile + " -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=" + filepath.Join(state, "known_hosts")}
}

func stackAdd(ctx context.Context, g globals, args []string, out io.Writer) (int, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return 2, errors.New(stackUsage)
	}
	name := args[0]
	fs := flag.NewFlagSet("stack add", flag.ContinueOnError)
	repo := fs.String("repo", "", "the infra repository (git URL or path)")
	branch := fs.String("branch", "infra", "its branch")
	dir := fs.String("dir", "", "where to clone it")
	data := fs.String("data", "", "the data folder")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	if err := host.ValidName(name); err != nil {
		return 2, err
	}
	if *repo == "" || *dir == "" || *data == "" {
		return 2, errors.New("--repo, --dir and --data are all required (the paths are yours to choose)")
	}
	if !serverMode(g) {
		if err := host.Ensure(g.state); err != nil {
			return 1, fmt.Errorf("cannot create runsnip's state %s: %v", g.state, err)
		}
	}
	g.stack = name
	return runJob(ctx, g, nil, name, "stack", append([]string{"add"}, args...), out, func(w io.Writer) (int, error) {
		rs := paths.Check(name, *dir, *data, paths.Options{State: g.state})
		for _, r := range rs {
			for _, x := range r.Warnings {
				fmt.Fprintf(w, "⚠ %s %s: %s\n", r.Kind, r.Path, x)
			}
		}
		if err := paths.Errors(rs); err != nil {
			return 1, err
		}
		if e, ok, _ := host.Find(g.state, name); ok && e.Dir != rs[0].Resolved {
			return 1, fmt.Errorf("a stack is already registered as %s (at %s)", name, e.Dir)
		}
		cloneDir, dataDir := rs[0].Resolved, rs[1].Resolved
		keyFile := ""
		if _, err := os.Stat(filepath.Join(g.state, "keys", name)); err == nil {
			keyFile = filepath.Join(g.state, "keys", name)
		}
		created := false
		if _, err := os.Stat(cloneDir); errors.Is(err, os.ErrNotExist) {
			created = true
		}
		stack.Say(w, "git clone --branch %s --single-branch --depth 1 %s %s", *branch, *repo, cloneDir)
		err := run.Exec{}.Run(ctx, run.Call{Stdout: w, Stderr: w, Env: gitSSH(g.state, keyFile), Name: "git", Args: []string{"clone", "--branch", *branch, "--single-branch", "--depth", "1", *repo, cloneDir}})
		if err != nil {
			if keyFile == "" && (strings.HasPrefix(*repo, "git@") || strings.HasPrefix(*repo, "ssh://")) {
				fmt.Fprintf(w, "  a private repository? runsnip stack keygen %s, add the key as a read-only deploy key, then add again\n", name)
			}
			return 1, fmt.Errorf("clone failed")
		}
		d, err := def.Load(cloneDir)
		if err != nil {
			if created {
				os.RemoveAll(cloneDir)
			}
			return 1, fmt.Errorf("%v — not a runsnip stack (its infra branch needs a stack.yaml; runsnip adopt drafts one)", err)
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return 1, err
		}
		_ = os.WriteFile(filepath.Join(dataDir, paths.MarkerFile), []byte(name+"\n"), 0o600)
		st, err := stack.New(cloneDir, d)
		if err != nil {
			return 1, err
		}
		// .env: the server's own, mode 600; the data folder is the maintainer's choice, written where the stack reads it.
		if _, err := os.Stat(st.EnvPath); errors.Is(err, os.ErrNotExist) {
			if err := host.WriteFile(st.EnvPath, nil); err != nil {
				return 1, err
			}
		}
		dataVar := dataDirVar(d)
		if _, err := envmodel.Apply(st, envmodel.Change{Set: map[string]string{dataVar: dataDir}, Order: []string{dataVar}}); err != nil {
			return 1, err
		}
		if err := host.Register(g.state, host.StackEntry{Name: name, Dir: cloneDir, Data: dataDir, Repo: *repo, Branch: *branch, KeyFile: keyFile, Added: time.Now().UTC(), AddedBy: who()}); err != nil {
			return 1, err
		}
		stack.Say(w, "%s registered: checkout %s, data %s (%s in .env)", name, cloneDir, dataDir, dataVar)
		fmt.Fprintf(w, "  next: the .env wizard (web), or runsnip --stack %s env generate · env list --filter missing · login · start\n", name)
		return 0, nil
	})
}

// dataDirVar is the .env variable a stack reads its data folder from: DATA_DIR, or the one its data_dir names.
func dataDirVar(d *def.Stack) string {
	if d.DataDir != "" {
		if n := expand.Names(d.DataDir); len(n) > 0 {
			return n[0]
		}
	}
	return "DATA_DIR"
}

var _ = jobs.LogPath
