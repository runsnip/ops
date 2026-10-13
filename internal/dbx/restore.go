package dbx

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/sysinfo"
)

// RestoreOptions are `db restore`'s.
type RestoreOptions struct {
	File string
	// Into is a local container: an existing one (the database is created in it), else a throwaway one of that
	// name, made for the restore and bound to 127.0.0.1.
	Into string
	// DB is the database to create ("" → the dump's own name; required on a server).
	DB    string
	Clean bool // local only: drop DB first when it exists
	Port  int  // the throwaway's local port (0: a free one)
	Image string
	// KeepOwners keeps the dump's owners and grants (the roles must exist); by default objects belong to the user
	// restoring them.
	KeepOwners bool
	// ToServer restores on h, a server, into a database that does not exist yet; it needs YesIAmSure.
	ToServer   bool
	YesIAmSure bool
	Select     Select
	Docker     Docker
	Run        run.Runner
	Out        io.Writer
	Progress   io.Writer
	TTY        bool
}

// DetectKind tells a dump's engine from its first bytes: "PGDMP" opens a pg_dump archive, gzip a mysqldump file.
func DetectKind(file string) (Kind, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b := make([]byte, 5)
	n, _ := io.ReadFull(f, b)
	switch {
	case n == 5 && string(b) == "PGDMP":
		return Postgres, nil
	case n >= 2 && b[0] == 0x1f && b[1] == 0x8b:
		return MySQL, nil
	}
	return "", fmt.Errorf("%s is neither a pg_dump -Fc archive nor a gzipped mysqldump", file)
}

// Restore restores a dump and compares every table's rows with the dump's.
func Restore(ctx context.Context, h Host, o RestoreOptions) error {
	out := o.Out
	kind, err := DetectKind(o.File)
	if err != nil {
		return err
	}
	sc, scErr := ReadSidecar(o.File)
	if scErr == nil {
		say(out, "checking %s against its sidecar", o.File)
		sum, err := FileSHA256(o.File)
		if err != nil {
			return err
		}
		if sum != sc.SHA256 {
			return fmt.Errorf("%s does not match its sidecar (sha256 %s, the sidecar says %s): the file changed or is damaged", o.File, sum, sc.SHA256)
		}
		if !sc.Verified {
			return fmt.Errorf("%s.json says this dump did not verify: %s", o.File, strings.Join(sc.Problems, "; "))
		}
		if sc.Kind.IsMySQL() != kind.IsMySQL() {
			return fmt.Errorf("%s.json says %s, the file is %s", o.File, sc.Kind, kind)
		}
		kind = sc.Kind
	} else {
		sc = nil
		fmt.Fprintf(out, "  no %s.json: the dump is read whole first to know its rows\n", o.File)
	}
	major := 0
	if sc != nil {
		major = Major(sc.DumpVersion)
	}
	var dumpRows map[string]int64
	if sc != nil && sc.DumpRows != nil {
		dumpRows = sc.DumpRows
	} else {
		dc, _, err := VerifyFile(ctx, o.Run, o.File, kind, major, "", out)
		if err != nil {
			return fmt.Errorf("the dump does not verify: %v", err)
		}
		dumpRows = dc.Rows
	}

	db := o.DB
	var t Target
	var created *throwaway
	if o.ToServer {
		if !o.YesIAmSure {
			return errors.New("restoring on a server needs --yes-i-am-sure")
		}
		if o.Clean {
			return errors.New("--clean never applies to a server: a server restore only creates a new database")
		}
		if db == "" {
			return errors.New("name the new database on the server with --db (it must not exist yet)")
		}
		say(out, "finding the database on %s", h.Name())
		if t, err = Find(ctx, h, o.Docker, o.Select); err != nil {
			return err
		}
	} else {
		if o.Into == "" {
			return errors.New("restore into what? --into <container> (an existing local container, or the name of a new throwaway one)")
		}
		if db == "" && sc != nil {
			db = sc.Database
		}
		if db == "" {
			db = "restored"
		}
		cs, err := sh(ctx, h, o.Docker.Cmd("ps", "-a", "--no-trunc", "--filter", "name=^/"+o.Into+"$", "--format", psFormat))
		if err != nil {
			return err
		}
		if found := ParsePS(cs); len(found) > 0 {
			t.Container = found[0]
			if t.Container.State != "running" {
				return fmt.Errorf("%s is %s: start it first", o.Into, t.Container.State)
			}
			t.Kind = KindOfImage(t.Container.Image)
			if t.Kind == "" {
				t.Kind = kind
			}
			fmt.Fprintf(out, "  into the existing container %s (%s)\n", o.Into, t.Container.Image)
		} else {
			image := o.Image
			if image == "" {
				image = defaultImage(kind, sc)
			}
			if created, err = startThrowaway(ctx, h, o, kind, image); err != nil {
				return err
			}
			t.Container = Container{ID: o.Into, Name: o.Into, Image: image}
			t.Kind = kind
		}
	}
	if t.Kind.IsMySQL() != kind.IsMySQL() {
		return fmt.Errorf("%s runs %s, the dump is %s", t.Container.Name, t.Kind, kind)
	}

	admin := Target{Container: t.Container, Kind: t.Kind, DB: "postgres"}
	exists := "SELECT 1 FROM pg_database WHERE datname = " + pgString(db) + ";\n"
	create := "CREATE DATABASE " + pgIdent(db) + ";\n"
	drop := "DROP DATABASE IF EXISTS " + pgIdent(db) + " WITH (FORCE);\n"
	if t.Kind.IsMySQL() {
		admin.DB = "mysql"
		exists = "SELECT 1 FROM information_schema.schemata WHERE schema_name = " + MyString(db) + ";\n"
		create = "CREATE DATABASE " + MyIdent(db) + " CHARACTER SET utf8mb4;\n"
		drop = "DROP DATABASE IF EXISTS " + MyIdent(db) + ";\n"
	}
	sqlAdmin := func(q string) (string, error) {
		return shIn(ctx, h, o.Docker.Exec(t.Container.ID, SQLScript(admin.Kind, admin.DB)), strings.NewReader(q))
	}
	got, err := sqlAdmin(exists)
	if err != nil {
		return fmt.Errorf("asking %s: %v", t.Container.Name, err)
	}
	if strings.TrimSpace(got) == "1" {
		if o.ToServer || !o.Clean {
			return fmt.Errorf("database %q exists in %s: restore only creates a new one (another --db%s)", db, t.Container.Name, map[bool]string{true: "", false: ", or --clean to drop it first — local only"}[o.ToServer])
		}
		say(out, "dropping the local database %s (--clean)", db)
		if _, err := sqlAdmin(drop); err != nil {
			return err
		}
	}
	say(out, "creating database %s in %s on %s", db, t.Container.Name, h.Name())
	if _, err := sqlAdmin(create); err != nil {
		return err
	}

	f, err := os.Open(o.File)
	if err != nil {
		return err
	}
	defer f.Close()
	st, _ := f.Stat()
	var in io.Reader = bufio.NewReaderSize(f, 1<<20)
	script := pgEnv + `exec pg_restore --exit-on-error --single-transaction`
	if !o.KeepOwners {
		script += ` --no-owner --no-acl`
	}
	script += ` -U "$u" -d ` + Quote(db)
	if t.Kind.IsMySQL() {
		gz, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		in = gz
		script = SQLScript(t.Kind, db)
	}
	say(out, "restoring %s (%s)", o.File, sysinfo.HumanBytes(uint64(st.Size())))
	p := newProgress(o.Progress, o.TTY, "sent")
	var errb strings.Builder
	err = h.Sh(ctx, o.Docker.Exec(t.Container.ID, script), progressReader{p: p, r: in}, io.Discard, &errb)
	p.stop()
	if err != nil {
		return fmt.Errorf("the restore failed (database %s is left as it is, for a look): %v: %s", db, err, lastLines(strings.TrimSpace(errb.String()), 8))
	}

	say(out, "counting every table of %s", db)
	restored, err := AllCounts(ctx, h, o.Docker, Target{Container: t.Container, Kind: t.Kind, DB: db})
	if err != nil {
		return err
	}
	bad := CompareRestore(out, dumpRows, restored, "restored")
	if created != nil {
		created.print(out, db)
	}
	if bad > 0 {
		return fmt.Errorf("%d table(s) differ from the dump", bad)
	}
	fmt.Fprintf(out, "\n✓ %d tables restored into %s on %s, every count equal to the dump's\n", len(restored), db, t.Container.Name)
	return nil
}

// CompareRestore prints each table's rows in the dump and restored, and returns how many differ.
func CompareRestore(out io.Writer, dump, restored map[string]int64, label string) int {
	bad := 0
	all := map[string]bool{}
	for k := range dump {
		all[k] = true
	}
	for k := range restored {
		all[k] = true
	}
	fmt.Fprintf(out, "\n  %-44s %14s %14s\n", "TABLE", "DUMP", strings.ToUpper(label))
	for _, k := range sortedKeys(all) {
		d, inDump := dump[k]
		r, inDB := restored[k]
		mark := "✓"
		switch {
		case !inDB:
			mark, bad = "✗ missing", bad+1
		case !inDump && r != 0:
			mark, bad = "✗ not in the dump", bad+1
		case d != r:
			mark, bad = "✗ different", bad+1
		}
		fmt.Fprintf(out, "  %-44s %14d %14d  %s\n", trunc(k, 44), d, r, mark)
	}
	return bad
}

func pgString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func pgIdent(s string) string  { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func defaultImage(k Kind, sc *Sidecar) string {
	if sc != nil && KindOfImage(sc.Image).IsMySQL() == k.IsMySQL() && k.IsMySQL() {
		return sc.Image // the server's own engine and version
	}
	if k.IsMySQL() {
		return "mariadb:11"
	}
	m := 17
	if sc != nil {
		if v := Major(sc.DumpVersion); v >= 10 {
			m = v
		}
	}
	return fmt.Sprintf("postgres:%d-alpine", m)
}

type throwaway struct {
	name, password, user string
	port                 int
	kind                 Kind
}

func (t *throwaway) print(out io.Writer, db string) {
	fmt.Fprintf(out, "\n  %s is a throwaway container on 127.0.0.1:%d (local only).\n", t.name, t.port)
	if t.kind.IsMySQL() {
		fmt.Fprintf(out, "  connect: mysql -h 127.0.0.1 -P %d -u root -p%s %s\n", t.port, t.password, db)
	} else {
		fmt.Fprintf(out, "  connect: postgres://%s:%s@127.0.0.1:%d/%s\n", t.user, t.password, t.port, db)
	}
	fmt.Fprintf(out, "  remove it: docker rm -f %s\n", t.name)
}

// startThrowaway runs a new local database container for the restore, published on 127.0.0.1 only.
func startThrowaway(ctx context.Context, h Host, o RestoreOptions, k Kind, image string) (*throwaway, error) {
	port := o.Port
	if port == 0 {
		var err error
		if port, err = freePort(); err != nil {
			return nil, err
		}
	}
	pw := make([]byte, 12)
	if _, err := rand.Read(pw); err != nil {
		return nil, err
	}
	t := &throwaway{name: o.Into, password: hex.EncodeToString(pw), user: "restore", port: port, kind: k}
	args := []string{"run", "-d", "--name", o.Into, "--label", "runsnip.db=restore", "-p", "127.0.0.1:" + strconv.Itoa(port) + ":" + strconv.Itoa(k.Port())}
	ready := pgEnv + `pg_isready -q -h 127.0.0.1 -U "$u"`
	if k.IsMySQL() {
		t.user = "root"
		args = append(args, "-e", "MARIADB_ROOT_PASSWORD="+t.password, "-e", "MYSQL_ROOT_PASSWORD="+t.password)
		ready = myEnv + `a=$(command -v mariadb-admin || command -v mysqladmin); "$a" ping -h 127.0.0.1 -uroot --silent`
	} else {
		args = append(args, "-e", "POSTGRES_USER=restore", "-e", "POSTGRES_PASSWORD="+t.password, "-e", "POSTGRES_DB=restore")
	}
	args = append(args, image)
	say(o.Out, "starting a throwaway %s container %s on 127.0.0.1:%d", image, o.Into, port)
	if _, err := sh(ctx, h, o.Docker.Cmd(args...)); err != nil {
		return nil, err
	}
	// Ready means listening on TCP: the images' first start runs a temporary server on the socket only.
	deadline := time.Now().Add(120 * time.Second)
	for {
		if _, err := sh(ctx, h, o.Docker.Cmd("exec", o.Into, "sh", "-c", ready)); err == nil {
			return t, nil
		}
		if time.Now().After(deadline) {
			logs, _ := sh(ctx, h, o.Docker.Cmd("logs", "--tail", "20", o.Into)+" 2>&1")
			return nil, fmt.Errorf("%s did not become ready in 120 s:\n%s", o.Into, logs)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

type progressReader struct {
	p *progress
	r io.Reader
}

func (pr progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	pr.p.n.Add(int64(n))
	return n, err
}

// VerifyAgain checks a dump against its sidecar: the same sha256, read whole, the same rows in every table.
func VerifyAgain(ctx context.Context, r run.Runner, file, pgImage string, out io.Writer) error {
	kind, err := DetectKind(file)
	if err != nil {
		return err
	}
	sc, err := ReadSidecar(file)
	if err != nil {
		return fmt.Errorf("no sidecar to check against: %v", err)
	}
	sum, err := FileSHA256(file)
	if err != nil {
		return err
	}
	if sum != sc.SHA256 {
		return fmt.Errorf("sha256 %s, the sidecar says %s: the file changed or is damaged", sum, sc.SHA256)
	}
	fmt.Fprintf(out, "  sha256 %s ✓\n", sum)
	dc, tables, err := VerifyFile(ctx, r, file, kind, Major(sc.DumpVersion), pgImage, out)
	if err != nil {
		return err
	}
	if bad := CompareRestore(out, sc.DumpRows, dc.Rows, "read again"); bad > 0 {
		return fmt.Errorf("%d table(s) differ from the sidecar", bad)
	}
	if tables != sc.DumpTables {
		return fmt.Errorf("%d tables, the sidecar says %d", tables, sc.DumpTables)
	}
	fmt.Fprintf(out, "\n✓ %s: %d tables, the rows the sidecar recorded, from %s (%s, %s)\n", file, tables, sc.Server, sc.Database, sc.Finished.Format("2006-01-02 15:04 MST"))
	return nil
}
