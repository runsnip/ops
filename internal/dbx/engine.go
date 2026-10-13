package dbx

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The scripts below run inside the database container (sh -c). They read the user, the database and the password
// from the container's own environment, so no secret travels on a command line, over SSH or into a log; only the
// user and database names are printed back (by Info).

// pgEnv sets $u (user) and $d (database) and exports PGPASSWORD, the official image's variables first, then the
// common other images'.
const pgEnv = `for v in POSTGRES_PASSWORD POSTGRES_USER; do eval "f=\${${v}_FILE:-}"; if [ -n "$f" ] && [ -r "$f" ]; then eval "$v=\$(cat \"\$f\")"; fi; done; ` +
	`u="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-${POSTGRESQL_USER:-postgres}}}"; ` +
	`d="${POSTGRES_DB:-${POSTGRESQL_DATABASE:-$u}}"; ` +
	`export PGPASSWORD="${POSTGRES_PASSWORD:-${POSTGRESQL_PASSWORD:-}}"; `

// myEnv sets $u, $d, $cli and $dump and exports MYSQL_PWD (root when the container knows root's password, else its
// application user).
const myEnv = `pw="${MYSQL_ROOT_PASSWORD:-${MARIADB_ROOT_PASSWORD:-}}"; f="${MYSQL_ROOT_PASSWORD_FILE:-${MARIADB_ROOT_PASSWORD_FILE:-}}"; ` +
	`if [ -z "$pw" ] && [ -n "$f" ] && [ -r "$f" ]; then pw=$(cat "$f"); fi; u=root; ` +
	`if [ -z "$pw" ] && [ -n "${MYSQL_PASSWORD:-${MARIADB_PASSWORD:-}}" ]; then u="${MYSQL_USER:-${MARIADB_USER:-root}}"; pw="${MYSQL_PASSWORD:-${MARIADB_PASSWORD:-}}"; fi; ` +
	`d="${MYSQL_DATABASE:-${MARIADB_DATABASE:-}}"; export MYSQL_PWD="$pw"; ` +
	`cli=$(command -v mariadb || command -v mysql); dump=$(command -v mariadb-dump || command -v mysqldump); `

const myNeedDB = `[ -n "$d" ] || { echo "the container sets no MYSQL_DATABASE / MARIADB_DATABASE: nothing says which database to dump" >&2; exit 3; }; `

// Info is what the database says about itself.
type Info struct {
	User, Database, ServerVersion, DumpVersion string
}

// InfoScript prints user, database, server version and dump tool version, tab-separated.
func InfoScript(k Kind) string {
	if k.IsMySQL() {
		return myEnv + myNeedDB + `printf '%s\t%s\t' "$u" "$d"; "$cli" -u"$u" -NBe 'SELECT VERSION()' | tr -d '\n'; printf '\t'; "$dump" --version`
	}
	return pgEnv + `printf '%s\t%s\t' "$u" "$d"; psql -XtA -U "$u" -d "$d" -c 'SHOW server_version' | tr -d '\n'; printf '\t'; pg_dump --version`
}

// ParseInfo reads InfoScript's output.
func ParseInfo(out string) (Info, error) {
	f := strings.Split(strings.TrimSpace(out), "\t")
	if len(f) < 4 || f[0] == "" || f[1] == "" || f[2] == "" {
		return Info{}, fmt.Errorf("unexpected answer from the database: %q", out)
	}
	return Info{User: f[0], Database: f[1], ServerVersion: strings.TrimSpace(f[2]), DumpVersion: strings.TrimSpace(f[3])}, nil
}

var majorRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?`)

// Major is the major version in a version string ("pg_dump (PostgreSQL) 16.4" → 16; "9.6.24" → 9).
func Major(v string) int {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, ")"); i >= 0 && (v == "" || v[0] < '0' || v[0] > '9') {
		v = v[i+1:] // "pg_dump (PostgreSQL) 16.4"
	}
	m := majorRe.FindStringSubmatch(v)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// DumpScript streams the dump to stdout: pg_dump -Fc, or mysqldump --single-transaction (plain SQL).
func DumpScript(k Kind) string {
	if k.IsMySQL() {
		return myEnv + myNeedDB + `x=; if "$dump" --help 2>/dev/null | grep -q -- --set-gtid-purged; then x=--set-gtid-purged=OFF; fi; ` +
			`ev=; if [ "$u" = root ]; then ev=--events; fi; ` +
			`exec "$dump" -u"$u" --single-transaction --quick --routines --triggers $ev --hex-blob --no-tablespaces --default-character-set=utf8mb4 $x "$d"`
	}
	return pgEnv + `exec pg_dump -Fc -U "$u" -d "$d"`
}

// SQLScript runs the SQL on stdin, printing rows tab-separated without headers, in the container's database or in
// db when it is given (a restore's new database).
func SQLScript(k Kind, db string) string {
	set := ""
	if db != "" {
		set = "d=" + Quote(db) + "; "
	}
	if k.IsMySQL() {
		return myEnv + set + myNeedDB + `exec "$cli" -u"$u" -NB "$d"`
	}
	return pgEnv + set + `exec psql -XqtA -v ON_ERROR_STOP=1 -F "$(printf '\t')" -U "$u" -d "$d"`
}

// ───────────────────────── row counts ─────────────────────────

// TableCount is one table's rows and size.
type TableCount struct {
	Table string `json:"table"`
	Size  int64  `json:"size"`
	Rows  int64  `json:"rows"`
}

// Counts is the server's side of the comparison.
type Counts struct {
	Tables int          `json:"tables"`
	DBSize int64        `json:"db_size"`
	Top    []TableCount `json:"top"`
}

// pgCountsSQL counts the rows of the biggest tables exactly (count(*), in one statement through query_to_xml), and
// the tables a dump holds: plain and partitioned tables, outside the system schemas and extensions. Names are as
// pg_dump writes them (quote_ident).
func pgCountsSQL(limit int) string {
	lim := ""
	if limit > 0 {
		lim = " LIMIT " + strconv.Itoa(limit)
	}
	const rel = `FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'
   AND NOT EXISTS (SELECT 1 FROM pg_depend x WHERE x.classid = 'pg_class'::regclass AND x.objid = c.oid AND x.deptype = 'e')`
	return `SELECT '@total', (SELECT count(*) ` + rel + ` AND c.relkind IN ('r', 'p')), pg_database_size(current_database());
SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname), pg_total_relation_size(c.oid),
       (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text::bigint
  ` + rel + ` AND c.relkind = 'r'
 ORDER BY pg_total_relation_size(c.oid) DESC, 1` + lim + `;
`
}

// myTablesSQL lists the tables (biggest first) and the totals; the counts come from a second statement built from it.
func myTablesSQL(limit int) string {
	lim := ""
	if limit > 0 {
		lim = " LIMIT " + strconv.Itoa(limit)
	}
	return `SELECT '@total', COUNT(*), COALESCE(SUM(data_length + index_length), 0) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE';
SELECT table_name, data_length + index_length FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE' ORDER BY 2 DESC, 1` + lim + `;
`
}

// MyIdent quotes a MySQL identifier.
func MyIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// MyString quotes a MySQL string literal.
func MyString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// myCountSQL counts the rows of the named tables exactly.
func myCountSQL(tables []string) string {
	var parts []string
	for _, t := range tables {
		parts = append(parts, "SELECT "+MyString(t)+", COUNT(*) FROM "+MyIdent(t))
	}
	return strings.Join(parts, " UNION ALL ") + ";\n"
}

// ParseCounts reads the counting statements' output: an @total line, then table, size and (when present) rows.
func ParseCounts(out string) (Counts, error) {
	var c Counts
	seen := false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if f[0] == "@total" && len(f) >= 3 {
			n, err1 := strconv.Atoi(f[1])
			sz, err2 := strconv.ParseInt(f[2], 10, 64)
			if err1 != nil || err2 != nil {
				return c, fmt.Errorf("bad totals line %q", l)
			}
			c.Tables, c.DBSize, seen = n, sz, true
			continue
		}
		if len(f) < 2 {
			return c, fmt.Errorf("bad count line %q", l)
		}
		t := TableCount{Table: f[0], Rows: -1}
		var err error
		if t.Size, err = strconv.ParseInt(f[1], 10, 64); err != nil {
			return c, fmt.Errorf("bad count line %q", l)
		}
		if len(f) >= 3 {
			if t.Rows, err = strconv.ParseInt(f[2], 10, 64); err != nil {
				return c, fmt.Errorf("bad count line %q", l)
			}
		}
		c.Top = append(c.Top, t)
	}
	if !seen {
		return c, fmt.Errorf("the database gave no totals")
	}
	return c, nil
}

// ServerCounts counts on the server, through the database's own client in its container.
func ServerCounts(ctx context.Context, h Host, d Docker, t Target, limit int) (Counts, error) {
	run := func(sql string) (string, error) {
		return shIn(ctx, h, d.Exec(t.Container.ID, SQLScript(t.Kind, t.DB)), strings.NewReader(sql))
	}
	if !t.Kind.IsMySQL() {
		out, err := run(pgCountsSQL(limit))
		if err != nil {
			return Counts{}, fmt.Errorf("counting rows: %v", err)
		}
		return ParseCounts(out)
	}
	out, err := run(myTablesSQL(limit))
	if err != nil {
		return Counts{}, fmt.Errorf("listing tables: %v", err)
	}
	c, err := ParseCounts(out)
	if err != nil || len(c.Top) == 0 {
		return c, err
	}
	var names []string
	for _, x := range c.Top {
		names = append(names, x.Table)
	}
	out, err = run(myCountSQL(names))
	if err != nil {
		return c, fmt.Errorf("counting rows: %v", err)
	}
	rows := map[string]int64{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		if len(f) == 2 {
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return c, fmt.Errorf("bad count line %q", l)
			}
			rows[f[0]] = n
		}
	}
	for i := range c.Top {
		n, ok := rows[c.Top[i].Table]
		if !ok {
			return c, fmt.Errorf("no count for %s", c.Top[i].Table)
		}
		c.Top[i].Rows = n
	}
	return c, nil
}

// AllCounts counts every table of a database exactly (after a restore): table → rows.
func AllCounts(ctx context.Context, h Host, d Docker, t Target) (map[string]int64, error) {
	c, err := ServerCounts(ctx, h, d, t, 0)
	if err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, x := range c.Top {
		m[x.Table] = x.Rows
	}
	return m, nil
}

// sortedKeys is a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
