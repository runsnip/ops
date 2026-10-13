package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/runsnip/ops/internal/dbx"
	"github.com/runsnip/ops/internal/run"
)

const dbUsage = `runsnip db — a stack's database between a server and this machine (run it on your own machine)

  runsnip db pull <ssh-target> [where] [-o FILE] [--force] [--keep-remote [--remote-dir D]] [--count-tables N]
        dump the database over SSH into a local file: <file>.part until the dump ended well on every hop and was
        read back whole with every table's rows counted and compared with the server's; then <file> and
        <file>.json (server, stack, database, size, sha256, versions, counts). Nothing is written on the server
        unless --keep-remote. PostgreSQL: pg_dump -Fc (.dump). MySQL/MariaDB: mysqldump --single-transaction (.sql.gz).
  runsnip db tunnel <ssh-target> [where] [--local-port N] [--via ip|sidecar]
        127.0.0.1:N here → the database container, for a desktop client. Nothing is published on the server.
  runsnip db restore <file> --into <container> [--db NAME] [--clean] [--port N] [--image IMG] [--keep-owners]
        restore into a local container: an existing one (a new database in it), else a throwaway one of that name
        on 127.0.0.1; every table's rows are compared with the dump's
  runsnip db restore <file> --to-server <ssh-target> --db NEW --yes-i-am-sure [where]
        restore on a server, only into a database that does not exist yet
  runsnip db verify <file>      check a dump against its sidecar: sha256, read whole, rows per table

  where: --compose DIR (the compose folder on the server) | --stack NAME (a runsnip stack there, else a compose
         project) · --service S · --container C · --kind postgres|mysql|mariadb · --sudo (sudo -n docker)
         Nothing given: the server's only database container. --local instead of <ssh-target>: this machine.

The server needs sshd and docker only. The user, database and password are read inside the database container from
its own environment; none of them is passed on a command line or printed (the user and database names excepted).
ssh is the system's (keys, agent, known_hosts, ~/.ssh/config); RUNSNIP_SSH replaces it ("ssh -F file", like
GIT_SSH_COMMAND).`

// interspersed parses flags placed before, between and after the positional arguments.
func interspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type dbFlags struct {
	sel         dbx.Select
	kind        string
	sudo, local bool
}

func (f *dbFlags) add(fs *flag.FlagSet) {
	fs.StringVar(&f.sel.Compose, "compose", "", "the compose folder on the server")
	fs.StringVar(&f.sel.Stack, "stack", "", "a runsnip stack on the server, else a compose project name")
	fs.StringVar(&f.sel.Service, "service", "", "the database's compose service")
	fs.StringVar(&f.sel.Container, "container", "", "the database's container (name or id)")
	fs.StringVar(&f.kind, "kind", "", "postgres, mysql or mariadb (default: from the image)")
	fs.BoolVar(&f.sudo, "sudo", false, "run docker through sudo -n on the server")
	fs.BoolVar(&f.local, "local", false, "this machine instead of an SSH target")
}

func (f *dbFlags) selection() (dbx.Select, error) {
	k, err := dbx.ParseKind(f.kind)
	f.sel.Kind = k
	return f.sel, err
}

func dbRunner() run.Runner {
	if testRunner != nil {
		return testRunner
	}
	return run.Exec{}
}

// dbHost connects to the server (or this machine with --local).
func dbHost(ctx context.Context, f *dbFlags, target string) (dbx.Host, error) {
	if f.local {
		if target != "" {
			return nil, errors.New("--local and an SSH target: one or the other")
		}
		return dbx.Local{Run: dbRunner()}, nil
	}
	if target == "" {
		return nil, errors.New("which server? an SSH target (user@host, or a Host of ~/.ssh/config), or --local")
	}
	s := &dbx.SSH{Target: target, Cmd: dbx.SSHCommand(), Run: dbRunner()}
	if err := s.Open(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func stderrTTY() bool { return term.IsTerminal(int(os.Stderr.Fd())) }

func dbCmd(ctx context.Context, g globals, args []string, out io.Writer) (int, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(out, dbUsage)
		return 0, nil
	}
	sub, args := args[0], args[1:]
	var f dbFlags
	fs := flag.NewFlagSet("db "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch sub {
	case "pull":
		f.add(fs)
		o := fs.String("o", "", "the local file")
		force := fs.Bool("force", false, "replace an existing file")
		keep := fs.Bool("keep-remote", false, "also keep the dump on the server")
		rdir := fs.String("remote-dir", "", "where --keep-remote writes on the server (default ~/runsnip-backups)")
		count := fs.Int("count-tables", 20, "count the rows of the N biggest tables (0: every table)")
		pgImage := fs.String("pg-image", "", "image for pg_restore when it is not installed here")
		pos, err := interspersed(fs, args)
		if err != nil || len(pos) > 1 {
			return 2, usageErr(err, "runsnip db pull <ssh-target> [--compose DIR|--stack NAME] [--service S] [-o FILE]")
		}
		if f.sel.Stack == "" {
			f.sel.Stack = g.stack
		}
		sel, err := f.selection()
		if err != nil {
			return 2, err
		}
		h, err := dbHost(ctx, &f, first(pos))
		if err != nil {
			return 1, err
		}
		defer h.Close()
		_, err = dbx.Pull(ctx, h, dbx.PullOptions{Select: sel, Docker: dbx.Docker{Sudo: f.sudo}, Output: *o, Force: *force, KeepRemote: *keep,
			RemoteDir: *rdir, CountTables: *count, PgImage: *pgImage, Version: version, Run: dbRunner(), Out: out, Progress: os.Stderr, TTY: stderrTTY()})
		if err != nil {
			return 1, err
		}
		return 0, nil
	case "tunnel":
		f.add(fs)
		port := fs.Int("local-port", 0, "the local port (default: 15432 / 13306 when free)")
		via := fs.String("via", "ip", "ip or sidecar")
		img := fs.String("sidecar-image", "", "the socat image for --via sidecar (default "+dbx.SidecarImage+")")
		pos, err := interspersed(fs, args)
		if err != nil || len(pos) != 1 || f.local {
			return 2, usageErr(err, "runsnip db tunnel <ssh-target> [--compose DIR|--stack NAME] [--service S] [--local-port N] [--via ip|sidecar]")
		}
		sel, err := f.selection()
		if err != nil {
			return 2, err
		}
		h, err := dbHost(ctx, &f, pos[0])
		if err != nil {
			return 1, err
		}
		s := h.(*dbx.SSH)
		defer s.Close()
		return exit(dbx.Tunnel(ctx, s, dbx.TunnelOptions{Select: sel, Docker: dbx.Docker{Sudo: f.sudo}, LocalPort: *port, Via: *via, Image: *img, Out: out, Run: dbRunner()}))
	case "restore":
		f.add(fs)
		into := fs.String("into", "", "a local container (existing, or a new throwaway one of that name)")
		db := fs.String("db", "", "the database to create")
		clean := fs.Bool("clean", false, "local only: drop that database first if it exists")
		port := fs.Int("port", 0, "the throwaway container's port on 127.0.0.1")
		img := fs.String("image", "", "the throwaway container's image (default: the dump's engine and version)")
		owners := fs.Bool("keep-owners", false, "keep the dump's owners and grants (the roles must exist)")
		server := fs.String("to-server", "", "restore on this SSH target instead")
		sure := fs.Bool("yes-i-am-sure", false, "confirm a restore on a server")
		pos, err := interspersed(fs, args)
		if err != nil || len(pos) != 1 {
			return 2, usageErr(err, "runsnip db restore <file> --into <container> | --to-server <ssh-target> --db NEW --yes-i-am-sure")
		}
		if (*server != "") == (*into != "") {
			return 2, errors.New("restore needs exactly one of --into <local container> and --to-server <ssh-target>")
		}
		sel, err := f.selection()
		if err != nil {
			return 2, err
		}
		ro := dbx.RestoreOptions{File: pos[0], Into: *into, DB: *db, Clean: *clean, Port: *port, Image: *img, KeepOwners: *owners,
			Select: sel, Docker: dbx.Docker{Sudo: f.sudo}, Run: dbRunner(), Out: out, Progress: os.Stderr, TTY: stderrTTY()}
		if *server != "" {
			// Checked before connecting: nothing touches the server without both.
			if !*sure || *db == "" || *clean {
				return 2, errors.New("a restore on a server needs --db <a new database> and --yes-i-am-sure, and never --clean")
			}
			ro.ToServer, ro.YesIAmSure = true, true
			f.local = false
			h, err := dbHost(ctx, &f, *server)
			if err != nil {
				return 1, err
			}
			defer h.Close()
			return exit(dbx.Restore(ctx, h, ro))
		}
		if f.sudo || f.sel.Compose != "" || f.sel.Stack != "" {
			return 2, errors.New("--compose, --stack and --sudo are for --to-server")
		}
		return exit(dbx.Restore(ctx, dbx.Local{Run: dbRunner()}, ro))
	case "verify":
		pgImage := fs.String("pg-image", "", "image for pg_restore when it is not installed here")
		pos, err := interspersed(fs, args)
		if err != nil || len(pos) != 1 {
			return 2, usageErr(err, "runsnip db verify <file>")
		}
		return exit(dbx.VerifyAgain(ctx, dbRunner(), pos[0], *pgImage, out))
	}
	return 2, fmt.Errorf("unknown db command %q (runsnip db help)", sub)
}

func first(a []string) string {
	if len(a) > 0 {
		return a[0]
	}
	return ""
}

func exit(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func usageErr(err error, usage string) error {
	if err != nil {
		return fmt.Errorf("%v\nusage: %s", err, usage)
	}
	return errors.New("usage: " + usage)
}
