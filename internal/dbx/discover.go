package dbx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is the database engine, which decides the dump tool and the file format.
type Kind string

const (
	Postgres Kind = "postgres"
	MySQL    Kind = "mysql"
	MariaDB  Kind = "mariadb"
)

// IsMySQL is true for MySQL and MariaDB (one dump format, one client).
func (k Kind) IsMySQL() bool { return k == MySQL || k == MariaDB }

// Port is the engine's port inside its container.
func (k Kind) Port() int {
	if k.IsMySQL() {
		return 3306
	}
	return 5432
}

// KindOfImage tells the engine from an image reference ("" when it is none of them).
func KindOfImage(image string) Kind {
	i := strings.ToLower(image)
	if at := strings.LastIndex(i, "@"); at >= 0 {
		i = i[:at]
	}
	repo := i
	if c := strings.LastIndex(i, ":"); c > strings.LastIndex(i, "/") {
		repo = i[:c]
	}
	name := repo[strings.LastIndex(repo, "/")+1:]
	switch {
	case strings.Contains(name, "mariadb"):
		return MariaDB
	case strings.Contains(name, "mysql") || strings.Contains(name, "percona"):
		return MySQL
	case strings.Contains(name, "postgres") || strings.Contains(name, "postgis") || strings.Contains(name, "pgvector") ||
		strings.Contains(name, "timescaledb") || name == "pg":
		return Postgres
	}
	return ""
}

// ParseKind reads a --kind or a stack.yaml database.kind.
func ParseKind(s string) (Kind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", nil
	case "postgres", "postgresql", "pg":
		return Postgres, nil
	case "mysql":
		return MySQL, nil
	case "mariadb":
		return MariaDB, nil
	}
	return "", fmt.Errorf("unknown database kind %q (postgres, mysql, mariadb)", s)
}

// Container is one container docker knows, with its compose labels.
type Container struct {
	ID, Image, State, Name    string
	Project, Service, WorkDir string
}

// psFormat lists containers one per line, tab-separated, compose labels included.
const psFormat = `{{.ID}}	{{.Image}}	{{.State}}	{{.Names}}	{{.Label "com.docker.compose.project"}}	{{.Label "com.docker.compose.service"}}	{{.Label "com.docker.compose.project.working_dir"}}`

// ParsePS reads `docker ps -a --format psFormat`.
func ParsePS(out string) []Container {
	var cs []Container
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(l, "\r"), "\t")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		for len(f) < 7 {
			f = append(f, "")
		}
		cs = append(cs, Container{ID: f[0], Image: f[1], State: f[2], Name: f[3], Project: f[4], Service: f[5], WorkDir: f[6]})
	}
	return cs
}

// Select is how the operator names the database: a compose folder on the server, a stack (runsnip's registry on
// the server, else a compose project name), a service, a container — or nothing, when the server has one database.
type Select struct {
	Compose, Stack, Service, Container string
	Kind                               Kind
}

// Target is the database found.
type Target struct {
	Container Container
	Kind      Kind
	// Dir is the compose folder on the server, when known.
	Dir string
	// DB replaces the container's own database (a restore's new one).
	DB string
}

// stackFile is the part of a stack.yaml these commands read.
type stackFile struct {
	Postgres *struct {
		Service string `yaml:"service"`
	} `yaml:"postgres"`
	Database *struct {
		Service string `yaml:"service"`
		Kind    string `yaml:"kind"`
	} `yaml:"database"`
}

// overrides reads a stack.yaml's database (or postgres) block: the service and the kind it names.
func overrides(b []byte) (svc string, kind Kind, err error) {
	var f stackFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return "", "", fmt.Errorf("stack.yaml: %v", err)
	}
	if f.Postgres != nil {
		svc, kind = f.Postgres.Service, Postgres
		if svc == "" {
			kind = ""
		}
	}
	if f.Database != nil {
		if f.Database.Service != "" {
			svc = f.Database.Service
		}
		k, err := ParseKind(f.Database.Kind)
		if err != nil {
			return "", "", err
		}
		if k != "" {
			kind = k
		}
	}
	return svc, kind, nil
}

// Find resolves sel on the host: which container holds the database, and its engine.
func Find(ctx context.Context, h Host, d Docker, sel Select) (Target, error) {
	var t Target
	out, err := sh(ctx, h, d.Cmd("ps", "-a", "--no-trunc", "--format", psFormat))
	if err != nil {
		return t, fmt.Errorf("docker on %s: %v", h.Name(), err)
	}
	all := ParsePS(out)
	svc, kind := sel.Service, sel.Kind
	var stackSvc string
	var stackKind Kind

	dirs := map[string]bool{}
	dir := sel.Compose
	if dir == "" && sel.Stack != "" {
		// A stack registered with runsnip on the server: its folder.
		if js, err := sh(ctx, h, "command -v runsnip >/dev/null 2>&1 && runsnip stack list --json || true"); err == nil {
			var es []struct{ Name, Dir string }
			if json.Unmarshal([]byte(strings.TrimSpace(js)), &es) == nil {
				for _, e := range es {
					if e.Name == sel.Stack {
						dir = e.Dir
					}
				}
			}
		}
	}
	if dir != "" {
		p, err := sh(ctx, h, "cd "+Quote(dir)+" && pwd && pwd -P")
		if err != nil {
			return t, fmt.Errorf("compose folder %s on %s: %v", dir, h.Name(), err)
		}
		for _, l := range strings.Fields(p) {
			dirs[l] = true
		}
		dirs[strings.TrimRight(dir, "/")] = true
		t.Dir = strings.TrimSpace(strings.SplitN(p, "\n", 2)[0])
		for _, name := range []string{"stack.local.yaml", "stack.yaml"} {
			if b, err := sh(ctx, h, "cat "+Quote(t.Dir+"/"+name)+" 2>/dev/null || true"); err == nil && strings.TrimSpace(b) != "" {
				s, k, err := overrides([]byte(b))
				if err != nil {
					return t, err
				}
				if stackSvc == "" {
					stackSvc = s
				}
				if stackKind == "" {
					stackKind = k
				}
			}
		}
	}
	if svc == "" {
		svc = stackSvc
	}
	if kind == "" {
		kind = stackKind
	}

	var pool []Container
	switch {
	case sel.Container != "":
		for _, c := range all {
			if c.Name == sel.Container || strings.HasPrefix(c.ID, sel.Container) {
				pool = append(pool, c)
			}
		}
		if len(pool) == 0 {
			return t, fmt.Errorf("no container %q on %s", sel.Container, h.Name())
		}
	case dir != "":
		for _, c := range all {
			if dirs[strings.TrimRight(c.WorkDir, "/")] {
				pool = append(pool, c)
			}
		}
		if len(pool) == 0 {
			return t, fmt.Errorf("docker on %s knows no container of a compose project in %s — is the stack up? (projects: %s)", h.Name(), dir, projects(all))
		}
	case sel.Stack != "":
		for _, c := range all {
			if c.Project == sel.Stack {
				pool = append(pool, c)
			}
		}
		if len(pool) == 0 {
			return t, fmt.Errorf("no stack or compose project %q on %s (projects: %s)", sel.Stack, h.Name(), projects(all))
		}
	default:
		pool = all
	}

	var hits []Container
	for _, c := range pool {
		switch {
		case sel.Container != "":
			hits = append(hits, c)
		case svc != "":
			if c.Service == svc {
				hits = append(hits, c)
			}
		case KindOfImage(c.Image) != "":
			hits = append(hits, c)
		}
	}
	// Several replicas of one service are one database only if a single one runs.
	if len(hits) > 1 {
		var up []Container
		for _, c := range hits {
			if c.State == "running" {
				up = append(up, c)
			}
		}
		if len(up) > 0 {
			hits = up
		}
	}
	switch {
	case len(hits) == 0 && svc != "":
		return t, fmt.Errorf("no container of service %q there (services: %s)", svc, services(pool))
	case len(hits) == 0:
		return t, fmt.Errorf("no database container found on %s: name it with --service or --container (projects: %s)", h.Name(), projects(all))
	case len(hits) > 1:
		var names []string
		for _, c := range hits {
			names = append(names, c.Name+" ("+c.Image+")")
		}
		return t, fmt.Errorf("%d database containers match — name one with --compose/--stack and --service, or --container: %s", len(hits), strings.Join(names, ", "))
	}
	t.Container = hits[0]
	if t.Container.State != "running" {
		return t, fmt.Errorf("%s is %s, not running: start the database (and only the database) first", t.Container.Name, t.Container.State)
	}
	if kind == "" {
		kind = KindOfImage(t.Container.Image)
	}
	if kind == "" {
		return t, fmt.Errorf("cannot tell the engine of %s from its image %s: say --kind postgres|mysql|mariadb (or database.kind in stack.yaml)", t.Container.Name, t.Container.Image)
	}
	t.Kind = kind
	return t, nil
}

func projects(cs []Container) string {
	set := map[string]bool{}
	for _, c := range cs {
		if c.Project != "" {
			set[c.Project] = true
		}
	}
	return keys(set)
}

func services(cs []Container) string {
	set := map[string]bool{}
	for _, c := range cs {
		if c.Service != "" {
			set[c.Service] = true
		}
	}
	return keys(set)
}

func keys(m map[string]bool) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) == 0 {
		return "none"
	}
	return strings.Join(ks, " ")
}

// sh runs a script and returns its stdout; stderr goes into the error.
func sh(ctx context.Context, h Host, script string) (string, error) {
	return shIn(ctx, h, script, nil)
}

func shIn(ctx context.Context, h Host, script string, stdin io.Reader) (string, error) {
	var o, e bytes.Buffer
	if err := h.Sh(ctx, script, stdin, &o, &e); err != nil {
		msg := strings.TrimSpace(e.String())
		if msg == "" {
			return o.String(), err
		}
		return o.String(), fmt.Errorf("%v: %s", err, lastLines(msg, 6))
	}
	return o.String(), nil
}

// lastLines keeps the end of a long error output.
func lastLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
