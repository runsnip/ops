package stack

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/sysinfo"
)

// ───────────────────────── postgres ─────────────────────────

// Postgres is the stack's database: the definition's, else the service running a postgres image, with
// POSTGRES_USER and POSTGRES_DB from .env.
func (s *Stack) Postgres() (def.Postgres, bool) {
	p := def.Postgres{}
	if s.Def.Postgres != nil {
		p = *s.Def.Postgres
	}
	if p.Service == "" {
		for _, n := range s.Compose.Order {
			img := s.Compose.Services[n].Image
			if strings.HasPrefix(img, "postgres:") || strings.Contains(img, "/postgres:") || strings.Contains(img, "pgvector") || strings.Contains(img, "postgis") {
				p.Service = n
				break
			}
		}
	}
	if p.Service == "" {
		return p, false
	}
	if p.User == "" {
		p.User = "${POSTGRES_USER:-postgres}"
	}
	if p.DB == "" {
		p.DB = "${POSTGRES_DB:-" + p.User + "}"
	}
	p.User, p.DB = s.Expand(p.User), s.Expand(p.DB)
	return p, true
}

// SQL runs a query with psql -tA and returns its output, trimmed.
func (s *Stack) SQL(ctx context.Context, query string) (string, error) {
	p, ok := s.Postgres()
	if !ok {
		return "", fmt.Errorf("no postgres service")
	}
	out, err := s.DCOutput(ctx, "exec", "-T", p.Service, "psql", "-tAU", p.User, "-d", p.DB, "-c", query)
	return strings.TrimSpace(out), err
}

// Psql opens psql, or runs one statement.
func (s *Stack) Psql(ctx context.Context, query string) error {
	p, ok := s.Postgres()
	if !ok {
		return fmt.Errorf("no postgres service in %s (postgres: in %s)", filepath.Base(s.ComposePath), def.FileName)
	}
	if query != "" {
		return s.Attach(ctx, "exec", "-T", p.Service, "psql", "-U", p.User, "-d", p.DB, "-c", query)
	}
	return s.Attach(ctx, "exec", p.Service, "psql", "-U", p.User, "-d", p.DB)
}

// ExecDefault is the service `exec` enters when none is named: the default component.
func (s *Stack) ExecDefault() string { return s.DefaultComponent() }

// Shell opens a shell in svc (bash when the image has it), or runs cmd there.
func (s *Stack) Shell(ctx context.Context, svc string, root bool, cmd []string) error {
	args := []string{"exec"}
	if len(cmd) > 0 {
		args = append(args, "-T") // no TTY: pipes and redirects work
	}
	if root {
		args = append(args, "-u", "root")
	}
	args = append(args, svc)
	if len(cmd) == 0 {
		if root {
			return s.Attach(ctx, append(args, "sh")...)
		}
		return s.Attach(ctx, append(args, "sh", "-c", "command -v bash >/dev/null && exec bash || exec sh")...)
	}
	return s.Attach(ctx, append(args, cmd...)...)
}

// ───────────────────────── health ─────────────────────────

// HealthOf is how to ask svc whether it is ready (the standard: GET /health on $PORT).
func (s *Stack) HealthOf(svc string) def.Health {
	h := def.Health{Path: "/health", Port: "$PORT"}
	if o, ok := s.Def.Services[svc]; ok && o.Health != nil {
		if o.Health.Path != "" {
			h.Path = o.Health.Path
		}
		if o.Health.Port != "" {
			h.Port = o.Health.Port
		}
		h.Cmd = o.Health.Cmd
	}
	return h
}

// HealthCmd is the command run inside svc's container to ask for its health: its own command, else whichever of
// wget, curl and node the image has, printing the answer.
func (s *Stack) HealthCmd(svc string) []string {
	h := s.HealthOf(svc)
	if len(h.Cmd) > 0 {
		return h.Cmd
	}
	url := "http://127.0.0.1:" + h.Port + h.Path
	script := `u="` + url + `"; if command -v wget >/dev/null 2>&1; then wget -qO- "$u"; ` +
		`elif command -v curl >/dev/null 2>&1; then curl -fsS "$u"; ` +
		`else node -e 'fetch(process.argv[1]).then(async r=>{console.log(await r.text());process.exit(r.ok?0:1)}).catch(e=>{console.error(e.message);process.exit(1)})' "$u"; fi`
	return []string{"sh", "-c", script}
}

// Health asks svc (the default component when empty) for its health and prints the answer.
func (s *Stack) Health(ctx context.Context, out io.Writer, svc string) error {
	if svc == "" {
		svc = s.DefaultComponent()
	}
	if err := s.RequireService(svc); err != nil {
		return err
	}
	o, err := s.Exec(ctx, svc, nil, s.HealthCmd(svc)...)
	fmt.Fprintln(out, strings.TrimSpace(o))
	return err
}

// ───────────────────────── backups ─────────────────────────

// BackupDir is where backups go (default <data dir>/backups).
func (s *Stack) BackupDir() string {
	if s.Def.Backup.Dir != "" {
		d := s.Expand(s.Def.Backup.Dir)
		if !filepath.IsAbs(d) {
			d = filepath.Join(s.Dir, d)
		}
		return d
	}
	return filepath.Join(s.DataDir(), "backups")
}

// BackupKeep is how many files of each target are kept (BACKUP_KEEP, default 10; 0 keeps everything).
func (s *Stack) BackupKeep() int {
	t := s.Def.Backup.Keep
	if t == "" {
		t = "${BACKUP_KEEP:-10}"
	}
	n, err := strconv.Atoi(strings.TrimSpace(s.Expand(t)))
	if err != nil {
		return 10
	}
	return n
}

// BackupTargets are what `backup` saves: the definition's, else one pg_dump of the stack's database.
func (s *Stack) BackupTargets() []def.BackupTarget {
	if len(s.Def.Backup.Targets) > 0 {
		return s.Def.Backup.Targets
	}
	if p, ok := s.Postgres(); ok {
		return []def.BackupTarget{{Name: "db", Type: "pg_dump", Service: p.Service}}
	}
	return nil
}

// Backup is one file in the backup directory.
type Backup struct {
	Name string
	Size int64
	Time time.Time
}

// Backups lists the backup directory, newest first.
func (s *Stack) Backups() ([]Backup, error) {
	entries, err := os.ReadDir(s.BackupDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Backup
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			out = append(out, Backup{Name: e.Name(), Size: info.Size(), Time: info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// RunBackup saves every target into the backup directory (gzip, mode 600), then keeps the newest BACKUP_KEEP of
// each: without it the folder grows until the disk is full, and "disk full" does not say "too many backups".
func (s *Stack) RunBackup(ctx context.Context, out io.Writer, now time.Time) ([]string, error) {
	if _, err := s.Env(); err != nil {
		return nil, err
	}
	targets := s.BackupTargets()
	if len(targets) == 0 {
		return nil, fmt.Errorf("nothing to back up: no postgres service and no backup.targets in %s", def.FileName)
	}
	dir := s.BackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	stamp := now.Format("20060102-150405")
	var saved []string
	for _, t := range targets {
		Say(out, "%s (%s)", t.Name, t.Type)
		cmd, save, ext, err := s.backupCmd(t, stamp)
		if err != nil {
			return saved, err
		}
		if !save {
			if err := s.DC(ctx, out, append([]string{"exec", "-T", t.Service}, cmd...)...); err != nil {
				return saved, fmt.Errorf("%s: %v", t.Name, err)
			}
			continue
		}
		name := filepath.Join(dir, t.Name+"-"+stamp+ext)
		f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return saved, err
		}
		gz := gzip.NewWriter(f)
		var errb bytes.Buffer
		err = s.Run.Run(ctx, run.Call{Dir: s.Dir, Env: s.composeEnv(), Stdout: gz, Stderr: &errb, Name: "docker", Args: s.composeArgs(append([]string{"exec", "-T", t.Service}, cmd...)...)})
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(name)
			return saved, fmt.Errorf("%s: %v %s", t.Name, err, strings.TrimSpace(errb.String()))
		}
		if st, err := os.Stat(name); err == nil {
			fmt.Fprintf(out, "  %s  %s\n", name, sysinfo.HumanBytes(uint64(st.Size())))
		}
		saved = append(saved, name)
	}
	if keep := s.BackupKeep(); keep > 0 {
		removed := 0
		dirs := []string{dir}
		// Backups a stack kept beside its compose file before the standard's <data>/backups keep falling under the
		// same rule, so they leave one by one instead of filling the disk.
		legacy := append([]string{"backups"}, s.Def.Backup.Legacy...)
		for _, l := range legacy {
			if p := filepath.Join(s.Dir, l); p != dir {
				dirs = append(dirs, p)
			}
		}
		for _, t := range targets {
			for _, d := range dirs {
				removed += prune(d, t.Name+"-", keep)
			}
		}
		if removed > 0 {
			Say(out, "removed %d old backup(s) (keeping the newest %d of each)", removed, keep)
		}
	}
	if bs, err := s.Backups(); err == nil {
		var total int64
		for _, b := range bs {
			total += b.Size
		}
		fmt.Fprintf(out, "\n  %s: %s in %d file(s). Copy backups off this host.\n", dir, sysinfo.HumanBytes(uint64(total)), len(bs))
	}
	return saved, nil
}

func (s *Stack) backupCmd(t def.BackupTarget, stamp string) (cmd []string, save bool, ext string, err error) {
	if t.Service == "" {
		if p, ok := s.Postgres(); ok && strings.HasPrefix(t.Type, "pg_") {
			t.Service = p.Service
		} else {
			return nil, false, "", fmt.Errorf("backup %s: no service", t.Name)
		}
	}
	p, _ := s.Postgres()
	user, db := p.User, p.DB
	if t.User != "" {
		user = s.Expand(t.User)
	}
	if t.DB != "" {
		db = s.Expand(t.DB)
	}
	switch t.Type {
	case "pg_dump":
		return []string{"pg_dump", "-U", user, db}, true, ".sql.gz", nil
	case "pg_dumpall":
		return []string{"pg_dumpall", "-U", user}, true, ".sql.gz", nil
	case "exec":
		c := make([]string, len(t.Cmd))
		for i, a := range t.Cmd {
			c[i] = strings.ReplaceAll(s.Expand(a), "{stamp}", stamp)
		}
		return c, def.Truth(t.Save, true), or(t.Ext, ".gz"), nil
	}
	return nil, false, "", fmt.Errorf("backup %s: unknown type %q (pg_dump, pg_dumpall, exec)", t.Name, t.Type)
}

// prune keeps the newest keep files of dir starting with prefix.
func prune(dir, prefix string, keep int) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type f struct {
		name string
		at   time.Time
	}
	var fs []f
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			fs = append(fs, f{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].at.After(fs[j].at) })
	n := 0
	for i := keep; i < len(fs); i++ {
		if os.Remove(filepath.Join(dir, fs[i].name)) == nil {
			n++
		}
	}
	return n
}

// ───────────────────────── first install ─────────────────────────

// Init checks .env's required variables and the isolated domains, creates the data directories with their owners,
// and prints the stack's notes (DNS records…).
func (s *Stack) Init(out io.Writer, isolated func(map[string]string) []string) error {
	env, err := s.Env()
	if err != nil {
		return err
	}
	vals := env.Values()
	var missing []string
	for _, k := range s.Def.Init.Require {
		if vals[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(".env lacks: %s", strings.Join(missing, " "))
	}
	if isolated != nil {
		if bad := isolated(vals); len(bad) > 0 {
			return fmt.Errorf("%s share a domain with the app: every domain that runs other people's code must be a different registrable domain", strings.Join(bad, ", "))
		}
	}
	data := s.DataDir()
	for _, d := range s.Def.Init.Dirs {
		p := filepath.Join(data, s.ExpandWith(d.Path, vals))
		if err := os.MkdirAll(p, 0o755); err != nil {
			return err
		}
		if d.Owner != "" {
			if err := chownR(p, d.Owner); err != nil {
				fmt.Fprintf(out, "! cannot chown %s to %s (%v) — run again with sudo%s\n", p, d.Owner, err, whyOf(d.Why))
			}
		}
	}
	Say(out, "created %s", data)
	for _, n := range s.Def.Init.Notes {
		if n.If != nil && !s.Holds(*n.If, vals, nil) {
			continue
		}
		fmt.Fprintln(out, s.ExpandWith(n.Text, vals))
	}
	return nil
}

func whyOf(w string) string {
	if w == "" {
		return ""
	}
	return " — " + w
}

func chownR(path, owner string) error {
	u, g, _ := strings.Cut(owner, ":")
	uid, err := strconv.Atoi(u)
	if err != nil {
		return err
	}
	gid := uid
	if g != "" {
		if gid, err = strconv.Atoi(g); err != nil {
			return err
		}
	}
	return filepath.Walk(path, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// Holds evaluates a condition (running asks docker when the condition names a container).
func (s *Stack) Holds(c def.Cond, vals map[string]string, running func(string) bool) bool {
	if c.Env != "" && vals[c.Env] == "" {
		return false
	}
	if c.Unset != "" && vals[c.Unset] != "" {
		return false
	}
	if len(c.Differs) == 2 && s.ExpandWith(c.Differs[0], vals) == s.ExpandWith(c.Differs[1], vals) {
		return false
	}
	if c.Profile != "" && !s.ProfileOn(c.Profile, vals) {
		return false
	}
	if running != nil {
		if c.Running != "" && !running(c.Running) {
			return false
		}
		if c.Stopped != "" && running(c.Stopped) {
			return false
		}
	}
	return true
}

// Install links /usr/local/bin/runsnip to the stack's shim.
func (s *Stack) Install(out io.Writer, target string) error {
	shim := filepath.Join(s.Dir, "runsnip")
	if _, err := os.Stat(shim); err != nil {
		return fmt.Errorf("%s missing (the infra branch ships it)", shim)
	}
	_ = os.Remove(target)
	if err := os.Symlink(shim, target); err != nil {
		return fmt.Errorf("%v (run with sudo)", err)
	}
	fmt.Fprintf(out, "%s → %s\n  try: runsnip ps\n", target, shim)
	return nil
}

// Prune removes unused images and the build cache.
func (s *Stack) Prune(ctx context.Context, out io.Writer) error {
	if err := s.Run.Run(ctx, run.Call{Stdout: out, Stderr: out, Name: "docker", Args: []string{"image", "prune", "-af"}}); err != nil {
		return err
	}
	return s.Run.Run(ctx, run.Call{Stdout: out, Stderr: out, Name: "docker", Args: []string{"builder", "prune", "-f"}})
}

// ───────────────────────── downloads ─────────────────────────

// Download finds a download command by name.
func (s *Stack) Download(command string) (def.Download, bool) {
	for _, d := range s.Def.Downloads {
		if d.Command == command {
			return d, true
		}
	}
	return def.Download{}, false
}

// DownloadList prints which items are on disk.
func (s *Stack) DownloadList(out io.Writer, d def.Download) {
	dir := s.Expand(d.Dir)
	for _, it := range d.Items {
		p := filepath.Join(dir, strings.ReplaceAll(d.File, "{item}", it))
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			fmt.Fprintf(out, "  ✓ %s  %s\n", it, sysinfo.HumanBytes(uint64(st.Size())))
		} else {
			fmt.Fprintf(out, "  · %s  not here   (runsnip %s get %s)\n", it, d.Command, it)
		}
	}
}

// DownloadGet fetches items into the folder: to a .part file renamed when complete, so an interrupted download
// never leaves a file a worker would take for the real one.
func (s *Stack) DownloadGet(ctx context.Context, out io.Writer, d def.Download, items []string) error {
	if len(items) == 0 {
		return fmt.Errorf("which? %s%s", strings.Join(d.Items, " "), map[bool]string{true: " (the app uses " + d.Default + ")", false: ""}[d.Default != ""])
	}
	dir := s.Expand(d.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, it := range items {
		known := false
		for _, k := range d.Items {
			known = known || k == it
		}
		if !known {
			return fmt.Errorf("no %q. Known: %s", it, strings.Join(d.Items, " "))
		}
		p := filepath.Join(dir, strings.ReplaceAll(d.File, "{item}", it))
		if st, err := os.Stat(p); err == nil && st.Size() > 0 {
			fmt.Fprintf(out, "  ✓ %s already here\n", it)
			continue
		}
		url := strings.ReplaceAll(d.URL, "{item}", it)
		Say(out, "download %s", url)
		if err := fetch(ctx, url, p+".part"); err != nil {
			os.Remove(p + ".part")
			return fmt.Errorf("%s: %v", it, err)
		}
		if err := os.Rename(p+".part", p); err != nil {
			return err
		}
	}
	if d.Owner != "" {
		own := dir
		if d.OwnDir != "" {
			own = s.Expand(d.OwnDir)
		}
		if err := chownR(own, d.Owner); err != nil {
			fmt.Fprintf(out, "! cannot chown %s (%v) — run again with sudo\n", own, err)
		}
	}
	return nil
}

func fetch(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// LookPath reports whether a program is on PATH.
func LookPath(name string) bool { _, err := exec.LookPath(name); return err == nil }
