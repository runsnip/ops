package dbx

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/sysinfo"
)

// Sidecar is <dump>.json: where the dump came from, what it holds and how it was verified.
type Sidecar struct {
	Tool       string    `json:"tool"`
	File       string    `json:"file"`
	Format     string    `json:"format"` // pg_dump -Fc, mysqldump (gzip)
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished"`
	Seconds    float64   `json:"seconds"`
	Server     string    `json:"server"`
	Stack      string    `json:"stack,omitempty"`
	ComposeDir string    `json:"compose_dir,omitempty"`
	Project    string    `json:"project,omitempty"`
	Service    string    `json:"service,omitempty"`
	Container  string    `json:"container"`
	Image      string    `json:"image"`
	Kind       Kind      `json:"kind"`
	Database   string    `json:"database"`
	User       string    `json:"user"`
	// ServerVersion is the database server's; DumpVersion the dump tool's.
	ServerVersion string `json:"server_version"`
	DumpVersion   string `json:"dump_version"`
	ServerTables  int    `json:"server_tables"`
	DumpTables    int    `json:"dump_tables"`
	DBSize        int64  `json:"db_size"`
	// Counts are the biggest tables, counted on the server before and after the dump and in the dump.
	Counts []Check `json:"counts"`
	// DumpRows is every table's row count in the dump (what a restore is compared with).
	DumpRows   map[string]int64 `json:"dump_rows"`
	RemoteCopy string           `json:"remote_copy,omitempty"`
	Verified   bool             `json:"verified"`
	Problems   []string         `json:"problems,omitempty"`
	Warnings   []string         `json:"warnings,omitempty"`
}

// PullOptions are `db pull`'s.
type PullOptions struct {
	Select      Select
	Docker      Docker
	Output      string // the local file ("" → <server>-<project>-<time>.dump in the current folder)
	Force       bool   // replace an existing file
	KeepRemote  bool   // also keep the dump on the server
	RemoteDir   string // where (default $HOME/runsnip-backups)
	CountTables int    // the biggest N tables are counted (0: all)
	PgImage     string // the image pg_restore runs from when it is not installed here ("" → postgres:<major>-alpine)
	Version     string
	Run         run.Runner
	Out         io.Writer // what is said
	Progress    io.Writer // the moving byte counter (stderr)
	TTY         bool      // Progress is a terminal: redraw one line
	Now         func() time.Time
}

func (o *PullOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func say(out io.Writer, format string, a ...any) { fmt.Fprintf(out, "▶ "+format+"\n", a...) }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// DefaultName is the dump's file name when -o is not given.
func DefaultName(server string, t Target, at time.Time) string {
	if i := strings.LastIndex(server, "@"); i >= 0 {
		server = server[i+1:]
	}
	who := t.Container.Project
	if who == "" {
		who = t.Container.Name
	}
	if t.Container.Service != "" && t.Container.Service != who {
		who += "-" + t.Container.Service
	}
	n := unsafeName.ReplaceAllString(server+"-"+who, "_") + "-" + at.UTC().Format("20060102-150405")
	return n + Ext(t.Kind)
}

// Ext is the dump file's extension.
func Ext(k Kind) string {
	if k.IsMySQL() {
		return ".sql.gz"
	}
	return ".dump"
}

// Pull dumps the database on h into a local file, verifies it and writes its sidecar. Nothing is written to the
// server's disk unless KeepRemote. The file appears under its name only once the dump ended well on every hop and
// was verified; until then it is <file>.part, and a failed verification leaves <file>.failed.
func Pull(ctx context.Context, h Host, o PullOptions) (*Sidecar, error) {
	out := o.Out
	started := o.now()
	say(out, "finding the database on %s", h.Name())
	t, err := Find(ctx, h, o.Docker, o.Select)
	if err != nil {
		return nil, err
	}
	raw, err := sh(ctx, h, o.Docker.Exec(t.Container.ID, InfoScript(t.Kind)))
	if err != nil {
		return nil, fmt.Errorf("asking %s: %v", t.Container.Name, err)
	}
	info, err := ParseInfo(raw)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "  %s · %s (%s) · %s %s · database %s as %s\n", t.Container.Name, t.Container.Image, t.Kind, t.Kind, info.ServerVersion, info.Database, info.User)

	file := o.Output
	if file == "" {
		file = DefaultName(h.Name(), t, started)
	}
	if _, err := os.Stat(file); err == nil && !o.Force {
		return nil, fmt.Errorf("%s exists: choose another -o, or --force to replace it", file)
	}
	if dir := filepath.Dir(file); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}

	say(out, "counting the %s on the server", tablesWord(o.CountTables))
	before, err := ServerCounts(ctx, h, o.Docker, t, o.CountTables)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "  %d tables, %s\n", before.Tables, sysinfo.HumanBytes(uint64(max(before.DBSize, 0))))
	if free, ok := diskFree(filepath.Dir(absPath(file))); ok && free < uint64(before.DBSize) {
		fmt.Fprintf(out, "  ⚠ %s free here for a database of %s (the dump is compressed, usually much smaller)\n", sysinfo.HumanBytes(free), sysinfo.HumanBytes(uint64(before.DBSize)))
	}

	part := file + ".part"
	sc := &Sidecar{Tool: "runsnip " + o.Version, File: filepath.Base(file), Started: started, Server: h.Name(), Stack: o.Select.Stack,
		ComposeDir: t.Dir, Project: t.Container.Project, Service: t.Container.Service, Container: t.Container.Name, Image: t.Container.Image,
		Kind: t.Kind, Database: info.Database, User: info.User, ServerVersion: info.ServerVersion, DumpVersion: info.DumpVersion,
		ServerTables: before.Tables, DBSize: before.DBSize}
	sc.Format = "pg_dump -Fc"
	if t.Kind.IsMySQL() {
		sc.Format = "mysqldump, gzip"
	}

	if o.KeepRemote {
		err = pullKept(ctx, h, o, t, part, sc)
	} else {
		say(out, "dumping %s over %s into %s", info.Database, connWord(h), part)
		err = pullStream(ctx, h, o, t, part, sc)
	}
	if err != nil {
		os.Remove(part)
		return nil, err
	}

	say(out, "counting again on the server (a table that moved was being written to)")
	after, err := ServerCounts(ctx, h, o.Docker, t, o.CountTables)
	if err != nil {
		os.Remove(part)
		return nil, err
	}

	say(out, "verifying the dump (read whole, rows counted)")
	dc, dumpTables, err := VerifyFile(ctx, o.Run, part, t.Kind, Major(info.DumpVersion), o.PgImage, out)
	if err != nil {
		fail := file + ".failed"
		_ = os.Rename(part, fail)
		sc.Problems = append(sc.Problems, err.Error())
		_ = writeSidecar(fail+".json", sc)
		return sc, fmt.Errorf("the dump does not verify (kept as %s): %v", fail, err)
	}
	sc.DumpTables, sc.DumpRows = dumpTables, dc.Rows
	sc.Counts, sc.Problems, sc.Warnings = Compare(before, after, dc, dumpTables, t.Kind.IsMySQL())
	sc.Finished = o.now()
	sc.Seconds = sc.Finished.Sub(started).Seconds()
	printChecks(out, sc)
	if len(sc.Problems) > 0 {
		fail := file + ".failed"
		_ = os.Rename(part, fail)
		_ = writeSidecar(fail+".json", sc)
		return sc, fmt.Errorf("the dump does not match the server (kept as %s): %s", fail, strings.Join(sc.Problems, "; "))
	}
	sc.Verified = true
	if err := os.Rename(part, file); err != nil {
		return sc, err
	}
	syncDir(filepath.Dir(absPath(file)))
	if err := writeSidecar(file+".json", sc); err != nil {
		return sc, err
	}
	for _, w := range sc.Warnings {
		fmt.Fprintln(out, "  ⚠ "+w)
	}
	fmt.Fprintf(out, "\n✓ %s  %s  sha256 %s\n  %s.json\n", file, sysinfo.HumanBytes(uint64(sc.Size)), sc.SHA256, file)
	return sc, nil
}

func tablesWord(n int) string {
	if n <= 0 {
		return "rows of every table"
	}
	return fmt.Sprintf("rows of the %d biggest tables", n)
}

func connWord(h Host) string {
	if _, ok := h.(*SSH); ok {
		return "SSH"
	}
	return "a pipe"
}

// pullStream streams the dump straight into part: nothing touches the server's disk.
func pullStream(ctx context.Context, h Host, o PullOptions, t Target, part string, sc *Sidecar) error {
	if s, ok := h.(*SSH); ok && t.Kind.IsMySQL() {
		s.Compress = true // plain SQL: let ssh compress it on the wire
		defer func() { s.Compress = false }()
	}
	return receive(ctx, o, part, sc, t.Kind, func(w io.Writer, errw io.Writer) error {
		return h.Sh(ctx, o.Docker.Exec(t.Container.ID, DumpScript(t.Kind)), nil, w, errw)
	})
}

// pullKept dumps into a file on the server first (mode 600), then copies it, and checks both hashes match.
func pullKept(ctx context.Context, h Host, o PullOptions, t Target, part string, sc *Sidecar) error {
	name := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(part, ".part")), ".gz")
	dir := `"${HOME:-/tmp}"/runsnip-backups`
	if o.RemoteDir != "" {
		dir = Quote(o.RemoteDir)
	}
	p := dir + "/" + Quote(name)
	say(o.Out, "dumping into %s on the server (--keep-remote)", strings.ReplaceAll(p, `"${HOME:-/tmp}"`, "~"))
	script := `umask 077 && mkdir -p ` + dir + ` && { ` + o.Docker.Exec(t.Container.ID, DumpScript(t.Kind)) + ` > ` + p + `.part || { s=$?; rm -f ` + p + `.part; exit $s; }; } && mv ` + p + `.part ` + p + ` && echo ` + p
	where, err := sh(ctx, h, script)
	if err != nil {
		return fmt.Errorf("dump on the server: %v", err)
	}
	sc.RemoteCopy = strings.TrimSpace(where)
	say(o.Out, "copying it over %s", connWord(h))
	var rawSum string
	err = receive(ctx, o, part, sc, t.Kind, func(w io.Writer, errw io.Writer) error {
		hh := sha256.New()
		if err := h.Sh(ctx, "cat "+p, nil, io.MultiWriter(w, hh), errw); err != nil {
			return err
		}
		rawSum = hex.EncodeToString(hh.Sum(nil))
		return nil
	})
	if err != nil {
		return err
	}
	sum, err := sh(ctx, h, `f=`+p+`; if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f"; else shasum -a 256 "$f"; fi`)
	if err != nil {
		return fmt.Errorf("hashing the server's copy: %v", err)
	}
	if f := strings.Fields(sum); len(f) == 0 || f[0] != rawSum {
		return fmt.Errorf("the copy differs from the server's file (sha256 %s here, %q there)", rawSum, strings.TrimSpace(sum))
	}
	fmt.Fprintf(o.Out, "  same sha256 as the server's copy %s\n", sc.RemoteCopy)
	return nil
}

// receive writes what produce streams into part (gzip for MySQL), hashing the file and showing progress. part is
// complete and synced to disk when it returns nil.
func receive(ctx context.Context, o PullOptions, part string, sc *Sidecar, k Kind, produce func(w, errw io.Writer) error) error {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	disk := &countWriter{w: io.MultiWriter(f, sum)}
	var w io.Writer = disk
	var gz *gzip.Writer
	if k.IsMySQL() {
		gz = gzip.NewWriter(disk)
		w = gz
	}
	p := newProgress(o.Progress, o.TTY, "received")
	var errb bytes.Buffer
	err = produce(p.wrap(w), &errb)
	p.stop()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg != "" {
			return fmt.Errorf("the dump failed: %v: %s", err, lastLines(msg, 8))
		}
		return fmt.Errorf("the dump failed: %v", err)
	}
	if p.n.Load() == 0 {
		return fmt.Errorf("the dump is empty: %s", strings.TrimSpace(errb.String()))
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	sc.Size = disk.n
	sc.SHA256 = hex.EncodeToString(sum.Sum(nil))
	if msg := strings.TrimSpace(errb.String()); msg != "" {
		fmt.Fprintln(o.Out, "  the dump said: "+lastLines(msg, 4))
	}
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// VerifyFile reads a whole dump and counts its rows: a PostgreSQL archive through pg_restore (installed here, else in
// a throwaway container of the dump's major version), a MySQL dump directly. It returns the rows and the number of
// tables the dump holds.
func VerifyFile(ctx context.Context, r run.Runner, file string, k Kind, major int, pgImage string, out io.Writer) (DataCounts, int, error) {
	if k.IsMySQL() {
		f, err := os.Open(file)
		if err != nil {
			return DataCounts{}, 0, err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return DataCounts{}, 0, fmt.Errorf("not gzip: %v", err)
		}
		dc, err := CountInsertRows(gz)
		if err != nil {
			return dc, 0, err
		}
		if !dc.Complete {
			return dc, 0, errors.New(`the dump does not end with "-- Dump completed": it is cut`)
		}
		fmt.Fprintf(out, "  %d tables, %d with rows, %s of SQL read\n", dc.Tables, nonEmpty(dc.Rows), sysinfo.HumanBytes(uint64(dc.Bytes)))
		return dc, dc.Tables, nil
	}
	tool := PgRestore{Run: r, Major: major, Image: pgImage}
	var listOut bytes.Buffer
	if err := tool.Call(ctx, []string{"--list"}, file, &listOut); err != nil {
		return DataCounts{}, 0, fmt.Errorf("pg_restore --list: %v", err)
	}
	toc, err := ParseTOC(listOut.String())
	if err != nil {
		return DataCounts{}, 0, err
	}
	fmt.Fprintf(out, "  archive: %d entries, %d tables, %d table data · dumped from %s by pg_dump %s (pg_restore %s)\n", toc.Entries, toc.Tables, toc.TableData, toc.ServerVersion, toc.DumpVersion, tool.Where())
	pr, pw := io.Pipe()
	type res struct {
		dc  DataCounts
		err error
	}
	ch := make(chan res, 1)
	go func() {
		dc, err := CountCopyRows(pr)
		pr.CloseWithError(err)
		_, _ = io.Copy(io.Discard, pr)
		ch <- res{dc, err}
	}()
	err = tool.Call(ctx, []string{"-f", "-"}, file, pw)
	pw.CloseWithError(err)
	got := <-ch
	if err != nil {
		return got.dc, 0, fmt.Errorf("pg_restore could not read the whole archive: %v", err)
	}
	if got.err != nil {
		return got.dc, 0, got.err
	}
	if len(got.dc.Rows) != toc.TableData {
		return got.dc, 0, fmt.Errorf("the archive lists %d table data entries but holds %d COPY blocks", toc.TableData, len(got.dc.Rows))
	}
	fmt.Fprintf(out, "  %d COPY blocks, %d with rows, %s of SQL read\n", len(got.dc.Rows), nonEmpty(got.dc.Rows), sysinfo.HumanBytes(uint64(got.dc.Bytes)))
	return got.dc, toc.Tables, nil
}

func nonEmpty(m map[string]int64) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}

// PgRestore runs pg_restore: the one installed here when it is at least the dump's version, else the one in a
// throwaway container (no network, the archive on stdin).
type PgRestore struct {
	Run   run.Runner
	Major int
	Image string
	local *bool
}

func (p *PgRestore) useLocal(ctx context.Context) bool {
	if p.local != nil {
		return *p.local
	}
	ok := false
	if _, err := exec.LookPath("pg_restore"); err == nil {
		if v, err := run.Output(ctx, p.Run, run.Call{Name: "pg_restore", Args: []string{"--version"}}); err == nil && (p.Major == 0 || Major(v) >= p.Major) {
			ok = true
		}
	}
	p.local = &ok
	return ok
}

func (p *PgRestore) image() string {
	if p.Image != "" {
		return p.Image
	}
	m := p.Major
	if m < 10 {
		m = 17
	}
	return fmt.Sprintf("postgres:%d-alpine", m)
}

// Where says which pg_restore runs.
func (p *PgRestore) Where() string {
	if p.local != nil && *p.local {
		return "installed here"
	}
	return "in a throwaway " + p.image() + " container"
}

// Call runs pg_restore args with the file on stdin.
func (p *PgRestore) Call(ctx context.Context, args []string, file string, stdout io.Writer) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var errb bytes.Buffer
	c := run.Call{Name: "pg_restore", Args: args, Stdin: f, Stdout: stdout, Stderr: &errb}
	if !p.useLocal(ctx) {
		c.Name = "docker"
		c.Args = append([]string{"run", "--rm", "-i", "--network", "none", "--log-driver", "none", "--entrypoint", "pg_restore", p.image()}, args...)
	}
	if err := p.Run.Run(ctx, c); err != nil {
		if m := strings.TrimSpace(errb.String()); m != "" {
			return fmt.Errorf("%v: %s", err, lastLines(m, 6))
		}
		return err
	}
	return nil
}

func printChecks(out io.Writer, sc *Sidecar) {
	if len(sc.Counts) == 0 {
		return
	}
	fmt.Fprintf(out, "\n  %-40s %10s %14s %14s %14s  %s\n", "TABLE", "SIZE", "SERVER BEFORE", "SERVER AFTER", "DUMP", "")
	for _, c := range sc.Counts {
		mark := "✓"
		switch c.Status {
		case "changed":
			mark = "⚠ written during the dump"
		case "missing", "different":
			mark = "✗ " + c.Status
		}
		d := fmt.Sprint(c.Dump)
		if c.Dump < 0 {
			d = "–"
		}
		fmt.Fprintf(out, "  %-40s %10s %14d %14d %14s  %s\n", trunc(c.Table, 40), sysinfo.HumanBytes(uint64(max(c.Size, 0))), c.Before, c.After, d, mark)
	}
	fmt.Fprintf(out, "  tables: %d on the server, %d in the dump\n", sc.ServerTables, sc.DumpTables)
}

func trunc(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func writeSidecar(path string, sc *Sidecar) error {
	b, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadSidecar reads <dump>.json, if there is one.
func ReadSidecar(dump string) (*Sidecar, error) {
	b, err := os.ReadFile(dump + ".json")
	if err != nil {
		return nil, err
	}
	var sc Sidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, fmt.Errorf("%s.json: %v", dump, err)
	}
	return &sc, nil
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// FileSHA256 hashes a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ───────────────────────── progress ─────────────────────────

type progress struct {
	verb  string
	out   io.Writer
	tty   bool
	n     atomic.Int64
	start time.Time
	done  chan struct{}
	ended chan struct{}
}

func newProgress(out io.Writer, tty bool, verb string) *progress {
	p := &progress{verb: verb, out: out, tty: tty, start: time.Now(), done: make(chan struct{}), ended: make(chan struct{})}
	if out == nil {
		p.out = io.Discard
	}
	every := 10 * time.Second
	if tty {
		every = 500 * time.Millisecond
	}
	go func() {
		defer close(p.ended)
		tk := time.NewTicker(every)
		defer tk.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-tk.C:
				p.print(false)
			}
		}
	}()
	return p
}

func (p *progress) line() string {
	el := time.Since(p.start).Seconds()
	n := p.n.Load()
	rate := 0.0
	if el > 0 {
		rate = float64(n) / el
	}
	return fmt.Sprintf("  %s %s · %s/s · %.0f s", sysinfo.HumanBytes(uint64(n)), p.verb, sysinfo.HumanBytes(uint64(rate)), el)
}

func (p *progress) print(final bool) {
	switch {
	case p.tty && final:
		fmt.Fprintf(p.out, "\r\033[K%s\n", p.line())
	case p.tty:
		fmt.Fprintf(p.out, "\r\033[K%s", p.line())
	default:
		fmt.Fprintln(p.out, p.line())
	}
}

func (p *progress) stop() {
	close(p.done)
	<-p.ended
	p.print(true)
}

func (p *progress) wrap(w io.Writer) io.Writer { return progressWriter{p: p, w: w} }

type progressWriter struct {
	p *progress
	w io.Writer
}

func (pw progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	pw.p.n.Add(int64(n))
	return n, err
}
