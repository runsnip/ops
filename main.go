// Command runsnip operates a compose stack that follows the runsnip stack standard (STANDARD.md): on the server, a
// dashboard with tabs and the scriptable commands (start, restart, pin, rollback, doctor, backup…); on the build
// machine, `runsnip build`. The server never builds it: the project's infra branch ships Linux binaries next to a
// small `runsnip` shim that picks the right one.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/doctor"
	"github.com/runsnip/ops/internal/graph"
	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
	"github.com/runsnip/ops/internal/release"
	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/serve"
	"github.com/runsnip/ops/internal/shield"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/standard"
	"github.com/runsnip/ops/internal/tui"
)

// testRunner replaces the programs runsnip runs (tests).
var testRunner run.Runner

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

const usage = `runsnip — operations for a compose stack (runsnip stack standard v1)    %s

  runsnip                         dashboard with tabs (same as runsnip top)
  runsnip top [--once] [--width N]  dashboard; --once prints every tab and exits

Deploy
  runsnip infra pull              update this folder from the infra branch, and say what to reload
  runsnip start                   git pull → pull images → start the whole stack → reload the proxies
  runsnip start <component>       pull one component's image and recreate it alone
  runsnip restart <svc> [--no-pull]  pull and recreate ONE service (never its dependencies); --no-pull keeps the image
  runsnip reload                  recreate the proxies so they read their config files again
  runsnip stop [svc]              bring the stack down, or stop one service
  runsnip pin [component] <tag>   pin a version in .env, then pull and recreate it (all: every component)
  runsnip rollback [component]    back to the version pinned before the last pin
  runsnip version                 pinned, running and previous version of every component

Watch
  runsnip ps                      containers
  runsnip logs [svc] [-n N] [--no-follow]
  runsnip health [svc]            ask a service's health endpoint (default: the main component)
  runsnip doctor [--json]         check .env, DNS, containers, routes, certificates, keys, the shield route… (exit 1 on failures)

Operate
  runsnip env list|combine|set|generate  .env laid over its example: list, review and apply the example's changes,
                                  set values (validated, the services reading them restarted on --restart), generate
                                  secrets (runsnip env help)
  runsnip psql [statement]        psql on the stack's database
  runsnip exec [--root] [svc] [cmd…]  a shell, or a command, in a container (default: the main component)
  runsnip backup                  back up the database (and the stack's other targets), keep the newest N
  runsnip prune                   remove unused images and build cache
%s
On your own machine (the server needs only sshd and docker)
  runsnip db pull <ssh-target> [--compose DIR|--stack NAME] [--service S] [-o FILE]
                                  a verified dump of the server's database, streamed over SSH (runsnip db help)
  runsnip db tunnel <ssh-target>  127.0.0.1:N here → the database, nothing published on the server
  runsnip db restore <file> --into <container>   into a local container, every table's rows compared
  runsnip db verify <file>        a dump against its sidecar

Install
  runsnip graph [--json]          the stack as nodes and edges: hostnames → proxies → services → data, outside hosts
  runsnip shield snippet          the Caddy snippet that puts the request shield in front of the sites (SHIELD=on)
  runsnip login                   docker login to the registry, then check every image can be pulled
  runsnip init                    create the data folders, check .env
  sudo ./runsnip install          link /usr/local/bin/runsnip to this folder's runsnip

Standard
  runsnip validate [dir]          check a stack against the schema and the standard, each gap with its fix
  runsnip adopt [dir]             read a project's compose and .env, write a draft stack.yaml and a report
  runsnip serve [--listen addr]   web dashboard (opt-in, 127.0.0.1 only unless --public with a token)

On the build machine, inside the repository:
  runsnip build [--dry-run] [component…]  pick components, release them, follow the image builds

Stack folder: RUNSNIP_HOME, else the folder holding this program (bin/.. for the shipped binaries), else the
current folder. RUNSNIP_COMPOSE_FILE and RUNSNIP_ENV_FILE point at other files.`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var out io.Writer = os.Stdout
	var errOut io.Writer = os.Stderr
	// Started by runsnip serve as a job: everything said goes into the job's log on disk (which the page follows,
	// and which outlives a restart of serve), and the job ends with this process's exit code.
	finish := func(int) {}
	if id := os.Getenv("RUNSNIP_JOB_ID"); id != "" {
		state := host.Dir()
		if log, err := jobs.OpenLog(state, id); err == nil {
			defer log.Close()
			out, errOut = io.MultiWriter(os.Stdout, log), io.MultiWriter(os.Stderr, log)
			finish = func(code int) {
				if j, err := jobs.Get(state, id); err == nil && j.Status == "running" {
					_ = jobs.Finish(state, j, code)
				}
			}
		}
	}
	code, err := run1(ctx, os.Args[1:], out)
	if err != nil {
		fmt.Fprintln(errOut, color("31", "✗")+"  "+err.Error())
		if code == 0 {
			code = 1
		}
	}
	finish(code)
	os.Exit(code)
}

var colorOn = (term.IsTerminal(int(os.Stdout.Fd())) || os.Getenv("RUNSNIP_COLOR") != "") && os.Getenv("NO_COLOR") == ""

func color(code, s string) string {
	if !colorOn {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// stackDir finds the stack: RUNSNIP_HOME, else the program's folder (bin/.. for the shipped binaries) when it holds a
// stack.yaml, else the current folder.
func stackDir() string {
	if d := os.Getenv("RUNSNIP_HOME"); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		dir := filepath.Dir(exe)
		if filepath.Base(dir) == "bin" {
			dir = filepath.Dir(dir)
		}
		if _, err := os.Stat(filepath.Join(dir, def.FileName)); err == nil {
			return dir
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func run1(ctx context.Context, args []string, out io.Writer) (int, error) {
	g, args, err := parseGlobals(args)
	if err != nil {
		return 2, err
	}
	cmd := "top"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprintf(out, usage+"\n", version, downloadsUsage())
		return 0, nil
	case "-v", "--version":
		fmt.Fprintln(out, "runsnip", version)
		return 0, nil
	case "build":
		return 0, build(ctx, args)
	case "validate":
		return validate(args, out)
	case "serve":
		return 0, serve.Main(ctx, serve.Env{State: g.state, Version: version, Exe: selfExe()}, args, out)
	case "adopt":
		return 0, adopt(args, out)
	case "db":
		return dbCmd(ctx, g, args, out)
	}
	switch cmd {
	case "stack":
		return stackCmd(ctx, g, args, out)
	case "admin":
		return adminCmd(g, args, out)
	case "self":
		return selfCmd(ctx, g, args, out)
	case "jobs":
		return jobsCmd(g, args, out)
	}
	st, name, err := openStack(g)
	if err != nil {
		return 1, err
	}
	if testRunner != nil {
		st.Run = testRunner
	}
	if isJob(cmd, args) {
		return runJob(ctx, g, st, name, cmd, args, out, func(w io.Writer) (int, error) { return dispatch(ctx, st, cmd, args, w) })
	}
	return dispatch(ctx, st, cmd, args, out)
}

// dispatch runs one command against a stack.
func dispatch(ctx context.Context, st *stack.Stack, cmd string, args []string, out io.Writer) (int, error) {
	switch cmd {
	case "top":
		fs := flag.NewFlagSet("top", flag.ContinueOnError)
		once := fs.Bool("once", false, "print every tab once and exit")
		width := fs.Int("width", 120, "width for --once")
		if err := fs.Parse(args); err != nil {
			return 2, err
		}
		m := tui.New(st, version)
		if _, err := st.Env(); err != nil && stack.IsMissingEnv(err) {
			m = m.StartOnEnv()
		}
		if *once {
			fmt.Fprintln(out, tui.RenderOnce(m, *width))
			return 0, nil
		}
		if !term.IsTerminal(int(os.Stdout.Fd())) {
			fmt.Fprintf(out, usage+"\n", version, downloadsUsage())
			return 0, nil
		}
		_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx)).Run()
		if errors.Is(err, tea.ErrProgramKilled) {
			return 0, nil
		}
		return 0, err
	case "install":
		return 0, st.Install(out, "/usr/local/bin/runsnip")
	case "init":
		return 0, st.Init(out, func(vals map[string]string) []string {
			iso := st.Def.Init.Isolated
			if iso == nil {
				return nil
			}
			others := make([]string, len(iso.Others))
			for i, o := range iso.Others {
				others[i] = st.ExpandWith(o, vals)
			}
			return doctor.Nested(st.ExpandWith(iso.Base, vals), others)
		})
	case "login":
		return 0, login(ctx, st, out)
	case "shield":
		if len(args) == 1 && args[0] == "snippet" {
			sh := st.Def.Shield
			if sh == nil {
				return 1, errors.New("this stack declares no request shield (stack.yaml: shield)")
			}
			addr := sh.Addr
			if sh.AddrEnv != "" {
				addr = "{$" + sh.AddrEnv + ":" + sh.Addr + "}"
			}
			fmt.Fprint(out, shield.CaddySnippet(addr, sh.Path(), sh.CopyHeaders))
			return 0, nil
		}
		return 2, errors.New("usage: runsnip shield snippet")
	case "start", "deploy", "update", "up":
		if len(args) > 0 {
			c, ok := st.Component(args[0])
			if !ok {
				return 1, stack.ErrUnknown{Kind: "component", Name: args[0], Known: strings.Join(st.ComponentNames(), " ") + " (nothing: the whole stack)"}
			}
			return 0, st.Restart(ctx, out, c.Service, false)
		}
		return 0, st.Start(ctx, out)
	case "restart", "redeploy":
		svc, noPull := "", false
		for _, a := range args {
			if a == "--no-pull" {
				noPull = true
			} else {
				svc = a
			}
		}
		if svc == "" {
			return 2, fmt.Errorf("which service? %s", strings.Join(st.Compose.Order, " "))
		}
		return 0, st.Restart(ctx, out, svc, noPull)
	case "reload":
		return 0, st.Reload(ctx, out)
	case "stop", "down":
		svc := ""
		if len(args) > 0 {
			svc = args[0]
		}
		return 0, st.Stop(ctx, out, svc)
	case "pin":
		switch len(args) {
		case 1:
			return 0, st.Pin(ctx, out, st.DefaultComponent(), args[0])
		case 2:
			return 0, st.Pin(ctx, out, args[0], args[1])
		}
		return 2, fmt.Errorf("usage: runsnip pin [component] <tag>   (e.g. runsnip pin %s 1.2.3)", st.DefaultComponent())
	case "rollback":
		name := st.DefaultComponent()
		if len(args) > 0 {
			name = args[0]
		}
		return 0, st.Rollback(ctx, out, name)
	case "graph":
		g, err := graph.Build(st)
		if err != nil {
			return 1, err
		}
		if len(args) > 0 && args[0] == "--json" {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return 0, enc.Encode(g)
		}
		graph.Text(out, g)
		return 0, nil
	case "version":
		return 0, printVersions(ctx, st, out)
	case "ps", "status":
		if _, err := st.Env(); err != nil {
			return 1, err
		}
		return 0, st.Attach(ctx, "ps")
	case "logs":
		return 0, logs(ctx, st, args)
	case "health":
		svc := ""
		if len(args) > 0 {
			svc = args[0]
		}
		return 0, st.Health(ctx, out, svc)
	case "doctor":
		return runDoctor(ctx, st, args, out)
	case "env":
		return 0, envCmd(ctx, st, args, out)
	case "psql":
		if _, err := st.Env(); err != nil {
			return 1, err
		}
		return 0, st.Psql(ctx, strings.Join(args, " "))
	case "exec", "shell", "sh":
		return 0, execCmd(ctx, st, args)
	case "backup":
		_, err := st.RunBackup(ctx, out, time.Now())
		return 0, err
	case "prune":
		return 0, st.Prune(ctx, out)
	case "infra":
		if len(args) != 1 || args[0] != "pull" {
			return 2, errors.New("usage: runsnip infra pull")
		}
		return 0, st.InfraPull(ctx, out)
	}
	if d, ok := st.Download(cmd); ok {
		if len(args) > 0 && args[0] == "get" {
			return 0, st.DownloadGet(ctx, out, d, args[1:])
		}
		st.DownloadList(out, d)
		return 0, nil
	}
	return 2, fmt.Errorf("unknown command %q (runsnip help lists them)", cmd)
}

// selfExe is this program, for serve to start its jobs with.
func selfExe() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "runsnip"
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// downloadsUsage lists the stack's own download commands in the help, when the stack can be read.
func downloadsUsage() string {
	st, err := stack.Open(stackDir())
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, d := range st.Def.Downloads {
		fmt.Fprintf(&b, "  runsnip %-22s %s\n", d.Command+" [get <name…>]", orDefault(d.Title, "files kept on the data disk"))
	}
	return b.String()
}

func printVersions(ctx context.Context, st *stack.Stack, out io.Writer) error {
	vs, err := st.Versions(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  %-18s %-12s %-12s %-12s %s\n", "COMPONENT", "PINNED", "RUNNING", "PREVIOUS", "IMAGE (VARIABLE)")
	for _, v := range vs {
		running := v.Running
		if running == "" {
			running = "-"
		}
		line := fmt.Sprintf("  %-18s %-12s %-12s %-12s %s (%s)", v.Component, v.Pinned, running, orDefault(v.Previous, "-"), v.Image, v.TagVar)
		if v.Off != "" {
			line += "  — " + v.Off
		}
		fmt.Fprintln(out, line)
	}
	fmt.Fprintf(out, "\nrunsnip %s\n", version)
	return nil
}

func logs(ctx context.Context, st *stack.Stack, args []string) error {
	if _, err := st.Env(); err != nil {
		return err
	}
	svc, n, follow := "", 100, true
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n":
			if i+1 < len(args) {
				n, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--no-follow":
			follow = false
		default:
			svc = args[i]
		}
	}
	if svc != "" {
		if err := st.RequireService(svc); err != nil {
			return err
		}
	}
	a := []string{"logs", "--tail", strconv.Itoa(n)}
	if follow {
		a = append(a, "-f")
	}
	if svc != "" {
		a = append(a, svc)
	}
	return st.Attach(ctx, a...)
}

func execCmd(ctx context.Context, st *stack.Stack, args []string) error {
	root := false
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		if args[0] != "--root" {
			return fmt.Errorf("unknown option %s", args[0])
		}
		root, args = true, args[1:]
	}
	svc := st.ExecDefault()
	if len(args) > 0 && st.HasService(args[0]) {
		svc, args = args[0], args[1:]
	}
	if _, err := st.Env(); err != nil {
		return err
	}
	return st.Shell(ctx, svc, root, args)
}

func runDoctor(ctx context.Context, st *stack.Stack, args []string, out io.Writer) (int, error) {
	asJSON := len(args) > 0 && args[0] == "--json"
	d := doctor.New(st)
	if !asJSON {
		d.Live = func(s doctor.Section) { doctor.Print(out, s, colorOn) }
	}
	r := d.Run(ctx)
	if asJSON {
		if err := doctor.JSON(out, r); err != nil {
			return 1, err
		}
	} else if r.Failures == 0 {
		fmt.Fprintln(out, color("32", doctor.Summary(r)))
	} else {
		fmt.Fprintln(out, color("31", doctor.Summary(r)))
	}
	if r.Failures > 0 {
		return 1, nil
	}
	return 0, nil
}

// login authenticates docker to the registry with an account that can read the packages, then asks the registry
// for every enabled component's image: a token without read:packages is found here, not halfway through a start.
// The token is never kept in .env: docker login stores it, and compose has no field that would read it.
func login(ctx context.Context, st *stack.Stack, out io.Writer) error {
	env, err := st.Env()
	if err != nil {
		return err
	}
	vals := env.Values()
	cs := st.Components()
	if len(cs) == 0 {
		return errors.New("no component image to log in for")
	}
	ref := st.ExpandWith(cs[0].Image.Repo, vals)
	host := orDefault(st.Def.Registry.Host, strings.SplitN(ref, "/", 2)[0])
	userEnv := st.Def.Registry.UserEnv
	user := ""
	if userEnv != "" {
		user = vals[userEnv]
	}
	if user == "" {
		parts := strings.Split(ref, "/")
		if len(parts) >= 3 {
			user = parts[1]
		}
	}
	if user == "" {
		return fmt.Errorf("cannot tell the account from %s: name the .env variable holding it in stack.yaml (registry.user_env)", ref)
	}
	var token string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintf(out, "A token that can pull from %s.\nToken for %s (not shown): ", host, user)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(b))
	} else {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n') // echo "$TOKEN" | runsnip login
		token = strings.TrimSpace(line)
	}
	if token == "" {
		return errors.New("no token received")
	}
	stack.Say(out, "docker login %s (account %s)", host, user)
	if err := st.Run.Run(ctx, run.Call{Stdin: strings.NewReader(token + "\n"), Stdout: out, Stderr: out, Name: "docker", Args: []string{"login", host, "-u", user, "--password-stdin"}}); err != nil {
		return errors.New("login failed")
	}
	ok := true
	for _, c := range cs {
		if on, _ := st.Enabled(c.Service, vals); !on {
			continue
		}
		img := st.ImageRef(c, vals, st.Tag(c, vals))
		if st.ImageExists(ctx, img) {
			fmt.Fprintf(out, "  ✓ can pull %s\n", img)
		} else {
			ok = false
			fmt.Fprintf(out, "  ⚠ logged in but cannot read %s — a token without read:packages, or the image is not pushed yet\n", img)
		}
	}
	if ok {
		fmt.Fprintln(out, "  next: runsnip start")
	}
	return nil
}

func build(ctx context.Context, args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("runsnip build [--dry-run] [component…] — pick components, release them through the project's build script and image workflows, follow the builds.\nRun inside the project's repository.")
		return nil
	}
	wd, _ := os.Getwd()
	repo, err := release.Open(wd, os.Getenv("RUNSNIP_STACK"))
	if err != nil {
		return err
	}
	for _, a := range args {
		if a == "--status" { // what the Components tab shows, as text (scripts, a quick look)
			return buildStatus(ctx, repo, os.Stdout)
		}
	}
	m := tui.NewBuild(repo)
	for _, a := range args {
		if a == "--dry-run" {
			m = m.DryRun()
		} else {
			m = m.Pick(a)
		}
	}
	_, err = tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

func buildStatus(ctx context.Context, repo *release.Repo, out io.Writer) error {
	st := repo.Inspect(ctx)
	fmt.Fprintf(out, "%s@%s · %s\n\n  %-22s %-10s %-10s %s\n", st.Branch, st.Commit, repo.Dir, "COMPONENT", "VERSION", "RELEASED", "CHANGED SINCE ITS RELEASE")
	for _, c := range append(append([]string{}, st.Components...), release.Infra) {
		ch := st.Changed[c]
		changed := "no changes"
		if len(ch) > 0 {
			changed = fmt.Sprintf("%d file(s)", len(ch))
			if ch[0] == "(never released)" {
				changed = "never released"
			}
		}
		if st.NeedsBump(c) {
			changed += " — raise the version"
		}
		fmt.Fprintf(out, "  %-22s %-10s %-10s %s\n", c, orDefault(st.Versions[c], "-"), orDefault(st.Released[c].Version, map[bool]string{true: "branch", false: "-"}[c == release.Infra]), changed)
	}
	for _, p := range st.Problems {
		fmt.Fprintln(out, "  ✗ "+p)
	}
	return nil
}

func validate(args []string, out io.Writer) (int, error) {
	dir := stackDir()
	if len(args) > 0 {
		dir = args[0]
	}
	r := standard.Validate(dir)
	standard.PrintValidation(out, r, colorOn)
	if r.Errors() > 0 {
		return 1, nil
	}
	return 0, nil
}

func adopt(args []string, out io.Writer) error {
	dir, _ := os.Getwd()
	write := true
	for _, a := range args {
		if a == "--print" {
			write = false
		} else {
			dir = a
		}
	}
	res, err := standard.Adopt(dir, write)
	if err != nil {
		return err
	}
	if write {
		p := filepath.Join(res.InfraDir, def.FileName+".draft")
		if err := os.WriteFile(p, []byte(res.Stack), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s\n\n", p)
	}
	fmt.Fprint(out, res.Report)
	if !write {
		fmt.Fprint(out, "\n---- "+def.FileName+" ----\n"+res.Stack)
	}
	return nil
}
