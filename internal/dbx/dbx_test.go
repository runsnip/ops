package dbx

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/run"
)

// ───────────────────────── quoting ─────────────────────────

func TestQuoteSurvivesTwoShells(t *testing.T) {
	words := []string{"plain", "", "it's", `a"b`, "$HOME", "`id`", "a b\tc", "line\nbreak", `back\slash`, "*", "-n", "'", "''", "ü日本"}
	// Each word goes through the login shell, then through sh -c — what an SSH command meets on the way.
	inner := Join(append([]string{"printf", `%s\n`}, words...)...)
	outer := Join("sh", "-c", inner)
	got, err := exec.Command("sh", "-c", outer).Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Join(words, "\n") + "\n"; string(got) != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestQuoteLeavesSafeWordsAlone(t *testing.T) {
	for in, want := range map[string]string{"docker": "docker", "a/b-c_d.e:f=g@h": "a/b-c_d.e:f=g@h", "a b": "'a b'", "it's": `'it'\''s'`} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestContainerScriptsKeepSecretsInside(t *testing.T) {
	for _, k := range []Kind{Postgres, MySQL} {
		for _, s := range []string{InfoScript(k), DumpScript(k), SQLScript(k, ""), SQLScript(k, "x")} {
			for _, bad := range []string{"secret", "--password", "-p\"$"} {
				if strings.Contains(s, bad) {
					t.Errorf("%s script carries %q: %s", k, bad, s)
				}
			}
			// The scripts are valid sh.
			if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
				t.Errorf("%s: %v %s\n%s", k, err, out, s)
			}
		}
	}
	d := Docker{Sudo: true}
	if got := d.Exec("c1", "echo hi"); got != `sudo -n docker exec -i c1 sh -c 'echo hi'` {
		t.Fatal(got)
	}
}

// The environment script reads the image's variables, including *_FILE secrets, and exports the password only.
func TestPgEnvReadsTheContainersEnvironment(t *testing.T) {
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("s3cr3t"), 0o600)
	cmd := exec.Command("sh", "-c", pgEnv+`printf '%s|%s|%s' "$u" "$d" "$PGPASSWORD"`)
	cmd.Env = []string{"POSTGRES_USER=app", "POSTGRES_PASSWORD_FILE=" + pw, "PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil || string(out) != "app|app|s3cr3t" {
		t.Fatalf("%q %v", out, err)
	}
	cmd = exec.Command("sh", "-c", myEnv+`printf '%s|%s|%s' "$u" "$d" "$MYSQL_PWD"`)
	cmd.Env = []string{"MYSQL_USER=app", "MYSQL_PASSWORD=pw", "MYSQL_DATABASE=shop", "PATH=" + os.Getenv("PATH")}
	if out, err = cmd.Output(); err != nil || string(out) != "app|shop|pw" {
		t.Fatalf("%q %v", out, err)
	}
	cmd.Env = append(cmd.Env, "MARIADB_ROOT_PASSWORD=r")
	cmd = exec.Command("sh", "-c", myEnv+`printf '%s|%s|%s' "$u" "$d" "$MYSQL_PWD"`)
	cmd.Env = []string{"MYSQL_USER=app", "MYSQL_PASSWORD=pw", "MYSQL_DATABASE=shop", "MARIADB_ROOT_PASSWORD=r", "PATH=" + os.Getenv("PATH")}
	if out, err = cmd.Output(); err != nil || string(out) != "root|shop|r" {
		t.Fatalf("%q %v", out, err)
	}
}

// ───────────────────────── discovery ─────────────────────────

func TestKindOfImage(t *testing.T) {
	for img, want := range map[string]Kind{"postgres:16-alpine": Postgres, "docker.io/library/postgres@sha256:ab": Postgres, "postgis/postgis:16-3.4": Postgres,
		"pgvector/pgvector:pg16": Postgres, "timescale/timescaledb:latest-pg16": Postgres, "mariadb:12.3": MariaDB, "mysql:8.4": MySQL,
		"percona/percona-server:8.0": MySQL, "redis:7": "", "ghcr.io/x/postgres-backup-tool-not:1": Postgres, "nginx": ""} {
		if got := KindOfImage(img); got != want {
			t.Errorf("%s: %q, want %q", img, got, want)
		}
	}
}

type fakeHost struct {
	rules   []fakeRule
	scripts []string
}

type fakeRule struct {
	match string
	fn    func(stdin io.Reader, stdout, stderr io.Writer) error
}

func (f *fakeHost) on(match, out string) *fakeHost {
	f.rules = append(f.rules, fakeRule{match, func(_ io.Reader, w, _ io.Writer) error { io.WriteString(w, out); return nil }})
	return f
}

func (f *fakeHost) onFn(match string, fn func(stdin io.Reader, stdout, stderr io.Writer) error) *fakeHost {
	f.rules = append(f.rules, fakeRule{match, fn})
	return f
}

func (f *fakeHost) Sh(_ context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	f.scripts = append(f.scripts, script)
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	for _, r := range f.rules {
		if strings.Contains(script, r.match) {
			return r.fn(stdin, stdout, stderr)
		}
	}
	return fmt.Errorf("fake host: no rule for %q", script)
}
func (f *fakeHost) Name() string { return "fake" }
func (f *fakeHost) Close()       {}

const psOut = "aaa\tpostgres:16-alpine\trunning\told-db-1\told\tdb\t/opt/old\n" +
	"bbb\tredis:7\trunning\told-redis-1\told\tredis\t/opt/old\n" +
	"ccc\tmariadb:11\trunning\tshop-db-1\tshop\tmariadb\t/srv/shop\n" +
	"ddd\tpostgres:15\texited\tgone-db-1\tgone\tpostgres\t/srv/gone\n"

func TestFindPicksTheDatabaseOfAComposeFolder(t *testing.T) {
	ctx := context.Background()
	h := (&fakeHost{}).on("docker ps", psOut).on("pwd", "/opt/old\n/data/old\n").on("cat ", "")
	tg, err := Find(ctx, h, Docker{}, Select{Compose: "/opt/old"})
	if err != nil || tg.Container.ID != "aaa" || tg.Kind != Postgres || tg.Dir != "/opt/old" {
		t.Fatalf("%+v %v", tg, err)
	}
	// A compose project by name; the engine from the image.
	tg, err = Find(ctx, h, Docker{}, Select{Stack: "shop"})
	if err != nil || tg.Container.ID != "ccc" || tg.Kind != MariaDB {
		t.Fatalf("%+v %v", tg, err)
	}
	// Nothing named and two running databases: ask.
	if _, err = Find(ctx, h, Docker{}, Select{}); err == nil || !strings.Contains(err.Error(), "2 database containers") {
		t.Fatal(err)
	}
	// A database that is not running.
	if _, err = Find(ctx, h, Docker{}, Select{Stack: "gone"}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatal(err)
	}
	if _, err = Find(ctx, h, Docker{}, Select{Stack: "nope"}); err == nil || !strings.Contains(err.Error(), "projects: gone old shop") {
		t.Fatal(err)
	}
	if _, err = Find(ctx, h, Docker{}, Select{Stack: "old", Service: "cache"}); err == nil || !strings.Contains(err.Error(), "services: db redis") {
		t.Fatal(err)
	}
}

func TestFindReadsTheStackFilesOverride(t *testing.T) {
	ctx := context.Background()
	ps := "aaa\tghcr.io/acme/dbimage:3\trunning\tx-store-1\tx\tstore\t/opt/x\nbbb\tpostgres:16\trunning\tx-pg-1\tx\tpg\t/opt/x\n"
	h := (&fakeHost{}).on("docker ps", ps).on("pwd", "/opt/x\n/opt/x\n").on("stack.local.yaml", "").
		on("stack.yaml", "standard: 1\ndatabase:\n  service: store\n  kind: mysql\n")
	tg, err := Find(ctx, h, Docker{}, Select{Compose: "/opt/x"})
	if err != nil || tg.Container.ID != "aaa" || tg.Kind != MySQL {
		t.Fatalf("%+v %v", tg, err)
	}
	if _, _, err := overrides([]byte("database:\n  kind: oracle\n")); err == nil {
		t.Fatal("an unknown kind is an error")
	}
}

// ───────────────────────── parsing ─────────────────────────

func TestParseInfoAndMajor(t *testing.T) {
	i, err := ParseInfo("app\tolddb\t16.4 (Debian 16.4-1.pgdg120+1)\tpg_dump (PostgreSQL) 16.4\n")
	if err != nil || i.User != "app" || i.Database != "olddb" || Major(i.DumpVersion) != 16 || Major(i.ServerVersion) != 16 {
		t.Fatalf("%+v %v", i, err)
	}
	if Major("mysqldump  Ver 10.19 Distrib 10.11.6-MariaDB") != 10 || Major("9.6.24") != 9 {
		t.Fatal("major")
	}
	if _, err := ParseInfo("psql: error: connection refused"); err == nil {
		t.Fatal("garbage is an error")
	}
}

func TestParseCounts(t *testing.T) {
	c, err := ParseCounts("@total\t3\t81920\npublic.users\t40960\t1000\n\"Odd\".\"Audit Log\"\t8192\t7\n")
	if err != nil || c.Tables != 3 || c.DBSize != 81920 || len(c.Top) != 2 || c.Top[1].Table != `"Odd"."Audit Log"` || c.Top[1].Rows != 7 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := ParseCounts("public.users\t1\t2\n"); err == nil {
		t.Fatal("no totals is an error")
	}
	if _, err := ParseCounts("@total\t1\t2\nusers\tbig\t3\n"); err == nil {
		t.Fatal("a bad number is an error")
	}
}

const tocOut = `;
; Archive created at 2026-10-13 06:00:00 UTC
;     dbname: olddb
;     Dumped from database version: 16.4
;     Dumped by pg_dump version: 16.4
;
; Selected TOC Entries:
;
215; 1259 16386 TABLE public users app
216; 1259 16390 TABLE public files app
217; 1259 16399 TABLE odd Audit Log app
3345; 0 16386 TABLE DATA public users app
3346; 0 16390 TABLE DATA public files app
3347; 0 16399 TABLE DATA odd Audit Log app
3299; 0 0 TABLE ATTACH public files_2026 app
3200; 2606 16395 CONSTRAINT public users users_pkey app
3201; 2606 16396 FK CONSTRAINT public files files_user_fkey app
`

func TestParseTOC(t *testing.T) {
	toc, err := ParseTOC(tocOut)
	if err != nil || toc.Tables != 3 || toc.TableData != 3 || toc.Entries != 9 || toc.Database != "olddb" || toc.DumpVersion != "16.4" {
		t.Fatalf("%+v %v", toc, err)
	}
	if _, err := ParseTOC("pg_restore: error: input file does not appear to be a valid archive\n"); err == nil {
		t.Fatal("no entries is an error")
	}
}

func TestCountCopyRows(t *testing.T) {
	sql := "--\n-- Data for Name: users\n--\n\nCOPY public.users (id, name) FROM stdin;\n1\tann\n2\tbo\\nb\n3\t\\\\.\n\\.\n\n" +
		"COPY \"odd\".\"Audit Log\" (id, \"note.x\") FROM stdin;\n1\t(a). b\n\\.\n" +
		"COPY public.empty (id) FROM stdin;\n\\.\n" +
		"COPY public.nocols FROM stdin;\n\n\n\\.\n" +
		"SELECT pg_catalog.setval('public.users_id_seq', 3, true);\n"
	dc, err := CountCopyRows(strings.NewReader(sql))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"public.users": 3, `"odd"."Audit Log"`: 1, "public.empty": 0, "public.nocols": 2}
	if fmt.Sprint(dc.Rows) != fmt.Sprint(want) || !dc.Complete {
		t.Fatalf("%v", dc.Rows)
	}
	// A row longer than the reader's buffer is still one row.
	long := "COPY public.big (v) FROM stdin;\n" + strings.Repeat("x", 3<<20) + "\n2\n\\.\n"
	if dc, err = CountCopyRows(strings.NewReader(long)); err != nil || dc.Rows["public.big"] != 2 {
		t.Fatalf("%v %v", dc.Rows, err)
	}
	// Cut inside a block.
	if _, err = CountCopyRows(strings.NewReader("COPY public.users (id) FROM stdin;\n1\n2\n")); err == nil || !strings.Contains(err.Error(), "cut") {
		t.Fatal(err)
	}
}

func TestCountInsertRows(t *testing.T) {
	dump := "-- MariaDB dump 10.19\n" +
		"CREATE TABLE `users` (\n  `id` int NOT NULL\n);\n" +
		"/*!50001 CREATE TABLE `v` (`id` int) */;\n" +
		"INSERT INTO `users` VALUES (1,'a(b)'),(2,'it\\'s; (x)'),(3,NULL),(4,0x28293B);\n" +
		"CREATE TABLE `odd``name` (`id` int);\n" +
		"INSERT INTO `odd``name` VALUES\n(1,'x\\\\'),\n(2,\"q\\\"(\");\n" +
		"CREATE TABLE `empty` (`id` int);\n" +
		"INSERT INTO `users` VALUES (5,'line\\nbreak');\n" +
		"-- Dump completed on 2026-10-13  6:00:00\n"
	dc, err := CountInsertRows(strings.NewReader(dump))
	if err != nil {
		t.Fatal(err)
	}
	if dc.Rows["users"] != 5 || dc.Rows["odd`name"] != 2 || dc.Tables != 3 || !dc.Complete {
		t.Fatalf("%+v", dc)
	}
	cut := strings.SplitAfter(dump, "(2,'it")[0]
	if _, err := CountInsertRows(strings.NewReader(cut)); err == nil {
		t.Fatal("a cut dump is an error")
	}
	noEnd := strings.TrimSuffix(dump, "-- Dump completed on 2026-10-13  6:00:00\n")
	if dc, err := CountInsertRows(strings.NewReader(noEnd)); err != nil || dc.Complete {
		t.Fatalf("%v %v", dc.Complete, err)
	}
}

func TestCompare(t *testing.T) {
	before := Counts{Tables: 3, Top: []TableCount{{Table: "a", Rows: 10}, {Table: "b", Rows: 5}, {Table: "c", Rows: 7}, {Table: "d", Rows: 0}}}
	after := Counts{Tables: 3, Top: []TableCount{{Table: "a", Rows: 10}, {Table: "b", Rows: 6}, {Table: "c", Rows: 7}, {Table: "d", Rows: 0}}}
	dump := DataCounts{Rows: map[string]int64{"a": 10, "b": 5, "c": 6}}
	cs, problems, warnings := Compare(before, after, dump, 3, false)
	st := map[string]string{}
	for _, c := range cs {
		st[c.Table] = c.Status
	}
	if st["a"] != "ok" || st["b"] != "changed" || st["c"] != "different" || st["d"] != "missing" || len(problems) != 2 || len(warnings) != 1 {
		t.Fatalf("%v %v %v", st, problems, warnings)
	}
	// MySQL dumps write nothing for an empty table.
	cs, problems, _ = Compare(before, before, DataCounts{Rows: map[string]int64{"a": 10, "b": 5, "c": 7}}, 3, true)
	if len(problems) != 0 || cs[3].Status != "ok" {
		t.Fatalf("%v %v", cs, problems)
	}
	if _, problems, _ = Compare(before, before, DataCounts{Rows: map[string]int64{"a": 10, "b": 5, "c": 7, "d": 0}}, 2, false); len(problems) != 1 {
		t.Fatal("a table missing from the dump is a problem", problems)
	}
}

// ───────────────────────── partial files ─────────────────────────

func mysqlHost(dump func(w, errw io.Writer) error) *fakeHost {
	h := &fakeHost{}
	h.on("docker ps", "ccc\tmariadb:11\trunning\tshop-db-1\tshop\tmariadb\t/srv/shop\n")
	h.on("SELECT VERSION()", "root\tshop\t11.4.2-MariaDB\tmariadb-dump from 11.4.2-MariaDB\n")
	h.onFn("-NB", func(in io.Reader, w, _ io.Writer) error {
		b, _ := io.ReadAll(in)
		if strings.Contains(string(b), "@total") {
			io.WriteString(w, "@total\t2\t32768\nusers\t16384\nempty\t16384\n")
		} else {
			io.WriteString(w, "users\t3\nempty\t0\n")
		}
		return nil
	})
	h.onFn("--single-transaction", func(_ io.Reader, w, errw io.Writer) error { return dump(w, errw) })
	return h
}

const goodDump = "CREATE TABLE `users` (`id` int);\nINSERT INTO `users` VALUES (1),(2),(3);\nCREATE TABLE `empty` (`id` int);\n-- Dump completed on x\n"

func pullOpts(dir string) PullOptions {
	return PullOptions{Select: Select{Stack: "shop"}, Output: filepath.Join(dir, "shop.sql.gz"), CountTables: 20, Run: &run.Fake{}, Out: io.Discard, Version: "test"}
}

func TestPullWritesTheFileOnlyWhenEveryHopSucceeded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// The dump tool fails half way: some bytes came, the exit status says no.
	h := mysqlHost(func(w, errw io.Writer) error {
		io.WriteString(w, "CREATE TABLE `users` (`id` int);\nINSERT INTO `users` VALUES (1),")
		io.WriteString(errw, "mysqldump: Error 2013: Lost connection to server during query")
		return errors.New("exit status 2")
	})
	_, err := Pull(ctx, h, pullOpts(dir))
	if err == nil || !strings.Contains(err.Error(), "Lost connection") {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("a failed dump left %v", left)
	}

	// The tool says yes but the stream is cut (no "Dump completed"): kept as .failed, never under its name.
	h = mysqlHost(func(w, _ io.Writer) error {
		io.WriteString(w, strings.TrimSuffix(goodDump, "-- Dump completed on x\n"))
		return nil
	})
	_, err = Pull(ctx, h, pullOpts(dir))
	if err == nil || !strings.Contains(err.Error(), "cut") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.sql.gz")); err == nil {
		t.Fatal("an unverified dump got the final name")
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.sql.gz.failed")); err != nil {
		t.Fatal(err)
	}

	// The dump holds fewer rows than the server: .failed, with the reason.
	h = mysqlHost(func(w, _ io.Writer) error { io.WriteString(w, strings.Replace(goodDump, ",(3)", "", 1)); return nil })
	_, err = Pull(ctx, h, pullOpts(dir))
	if err == nil || !strings.Contains(err.Error(), "users: 3 rows on the server, 2 in the dump") {
		t.Fatal(err)
	}

	// Good: the file, its sidecar, nothing else.
	h = mysqlHost(func(w, _ io.Writer) error { io.WriteString(w, goodDump); return nil })
	sc, err := Pull(ctx, h, pullOpts(dir))
	if err != nil || !sc.Verified {
		t.Fatal(err)
	}
	f, _ := os.Open(filepath.Join(dir, "shop.sql.gz"))
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(gz)
	if string(b) != goodDump {
		t.Fatalf("%q", b)
	}
	if sum, _ := FileSHA256(filepath.Join(dir, "shop.sql.gz")); sum != sc.SHA256 {
		t.Fatal("sidecar sha256")
	}
	back, err := ReadSidecar(filepath.Join(dir, "shop.sql.gz"))
	if err != nil || back.Database != "shop" || back.DumpRows["users"] != 3 || back.ServerTables != 2 || back.DumpTables != 2 {
		t.Fatalf("%+v %v", back, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "shop.sql.gz")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, "shop.sql.gz.part")); err == nil {
		t.Fatal(".part left behind")
	}
	// Never replaced without --force.
	if _, err := Pull(ctx, h, pullOpts(dir)); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatal(err)
	}
	// A changed file no longer verifies against its sidecar.
	os.WriteFile(filepath.Join(dir, "shop.sql.gz"), []byte("x"), 0o600)
	if err := VerifyAgain(ctx, &run.Fake{}, filepath.Join(dir, "shop.sql.gz"), "", io.Discard); err == nil {
		t.Fatal("a damaged file verified")
	}
}

func TestRestoreOnAServerIsStrict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "x.dump")
	os.WriteFile(file, []byte("PGDMP-not-really"), 0o600)
	h := &fakeHost{}
	for _, o := range []RestoreOptions{
		{File: file, ToServer: true, DB: "new"},                              // not sure
		{File: file, ToServer: true, YesIAmSure: true},                       // no database named
		{File: file, ToServer: true, YesIAmSure: true, DB: "n", Clean: true}, // --clean
	} {
		o.Out, o.Run = io.Discard, &run.Fake{}
		// The file is checked first (it does not verify), so make it pass that by giving a sidecar.
		sc := &Sidecar{SHA256: mustSum(t, file), Verified: true, Kind: Postgres, DumpRows: map[string]int64{}}
		writeSidecar(file+".json", sc)
		if err := Restore(ctx, h, o); err == nil {
			t.Fatalf("%+v restored", o)
		}
	}
	if len(h.scripts) != 0 {
		t.Fatalf("the server was touched: %v", h.scripts)
	}
	// An existing database on the server is never touched.
	h = (&fakeHost{}).on("docker ps", psOut).onFn("psql -XqtA", func(in io.Reader, w, _ io.Writer) error {
		b, _ := io.ReadAll(in)
		if strings.Contains(string(b), "CREATE") || strings.Contains(string(b), "DROP") {
			t.Errorf("ran %s on the server", b)
		}
		if strings.Contains(string(b), "FROM pg_database WHERE datname = 'olddb'") {
			io.WriteString(w, "1\n")
		}
		return nil
	})
	err := Restore(ctx, h, RestoreOptions{File: file, ToServer: true, YesIAmSure: true, DB: "olddb", Select: Select{Stack: "old"}, Out: io.Discard, Run: &run.Fake{}})
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatal(err)
	}
	for _, s := range h.scripts {
		if strings.Contains(s, "pg_restore") {
			t.Fatalf("ran %s", s)
		}
	}
	// A file that does not match its sidecar is refused before anything.
	writeSidecar(file+".json", &Sidecar{SHA256: "00", Verified: true, Kind: Postgres})
	if err := Restore(ctx, &fakeHost{}, RestoreOptions{File: file, Into: "x", Out: io.Discard, Run: &run.Fake{}}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatal(err)
	}
}

func mustSum(t *testing.T, f string) string {
	s, err := FileSHA256(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompareRestore(t *testing.T) {
	var b bytes.Buffer
	if n := CompareRestore(&b, map[string]int64{"a": 1, "b": 0}, map[string]int64{"a": 1, "b": 0, "c": 0}, "restored"); n != 0 {
		t.Fatal(b.String())
	}
	if n := CompareRestore(&b, map[string]int64{"a": 1, "b": 2}, map[string]int64{"a": 1, "c": 4}, "restored"); n != 2 {
		t.Fatal(b.String())
	}
}
