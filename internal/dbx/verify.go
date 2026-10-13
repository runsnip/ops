package dbx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ───────────────────────── PostgreSQL archives ─────────────────────────

// TOC is what `pg_restore --list` says about an archive.
type TOC struct {
	Database      string // the database it was dumped from
	ServerVersion string // "Dumped from database version"
	DumpVersion   string // "Dumped by pg_dump version"
	Entries       int
	Tables        int // TABLE entries (plain and partitioned)
	TableData     int // TABLE DATA entries
}

// ParseTOC reads `pg_restore --list`.
func ParseTOC(out string) (TOC, error) {
	var t TOC
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, ";") {
			kv := strings.TrimSpace(strings.TrimPrefix(l, ";"))
			switch {
			case strings.HasPrefix(kv, "dbname:"):
				t.Database = strings.TrimSpace(strings.TrimPrefix(kv, "dbname:"))
			case strings.HasPrefix(kv, "Dumped from database version:"):
				t.ServerVersion = strings.TrimSpace(strings.TrimPrefix(kv, "Dumped from database version:"))
			case strings.HasPrefix(kv, "Dumped by pg_dump version:"):
				t.DumpVersion = strings.TrimSpace(strings.TrimPrefix(kv, "Dumped by pg_dump version:"))
			}
			continue
		}
		// "215; 1259 16386 TABLE public users owner"
		semi := strings.Index(l, ";")
		if semi <= 0 {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(l[:semi])); err != nil {
			continue
		}
		f := strings.Fields(l[semi+1:])
		if len(f) < 3 {
			continue
		}
		t.Entries++
		// A table is a pg_class (1259) entry described TABLE; its data and a partition's ATTACH have catalog 0.
		desc := f[2:]
		switch {
		case f[0] == "0" && len(desc) >= 2 && desc[0] == "TABLE" && desc[1] == "DATA":
			t.TableData++
		case f[0] == "1259" && desc[0] == "TABLE":
			t.Tables++
		}
	}
	if t.Entries == 0 {
		return t, fmt.Errorf("pg_restore --list shows no entries: not a PostgreSQL archive, or an empty one")
	}
	return t, nil
}

// DataCounts is what reading a whole dump found.
type DataCounts struct {
	Rows   map[string]int64 // table → rows in the dump
	Bytes  int64            // bytes read
	Tables int              // CREATE TABLE statements (MySQL dumps)
	// Complete is true when the dump says it ended (MySQL's "-- Dump completed"; for a PostgreSQL archive pg_restore
	// itself fails on a cut one).
	Complete bool
}

// CountCopyRows reads pg_restore's SQL output (`pg_restore -f -`) and counts the rows of every COPY block: in COPY's
// text format a row is exactly one line (newlines inside values are written as \n), and a block ends with `\.`.
// Reading the whole output also proves every data block of the archive decompresses.
func CountCopyRows(r io.Reader) (DataCounts, error) {
	dc := DataCounts{Rows: map[string]int64{}}
	br := bufio.NewReaderSize(r, 1<<20)
	atStart := true
	table := ""
	in := false
	for {
		frag, err := br.ReadSlice('\n')
		dc.Bytes += int64(len(frag))
		if len(frag) > 0 {
			if atStart {
				line := bytes.TrimRight(frag, "\r\n")
				if in {
					if string(line) == `\.` && frag[len(frag)-1] == '\n' {
						in = false
					} else {
						dc.Rows[table]++
					}
				} else if bytes.HasPrefix(line, []byte("COPY ")) && bytes.HasSuffix(line, []byte("FROM stdin;")) {
					name, perr := copyTable(string(line))
					if perr != nil {
						return dc, perr
					}
					table, in = name, true
					dc.Rows[table] += 0
				}
			}
			atStart = frag[len(frag)-1] == '\n'
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return dc, err
		}
	}
	if in {
		return dc, fmt.Errorf("the dump ends inside the data of %s: it is cut", table)
	}
	dc.Complete = true
	return dc, nil
}

// copyTable is the table of a `COPY schema.table (cols) FROM stdin;` line, as pg_dump writes it (quoted only where
// needed).
func copyTable(line string) (string, error) {
	s := strings.TrimPrefix(line, "COPY ")
	end := 0
	for part := 0; part < 2; part++ {
		if end < len(s) && s[end] == '"' {
			i := end + 1
			for {
				if i >= len(s) {
					return "", fmt.Errorf("bad COPY line %q", line)
				}
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			end = i + 1
		} else {
			for end < len(s) && s[end] != '.' && s[end] != ' ' {
				end++
			}
		}
		if part == 0 {
			if end >= len(s) || s[end] != '.' {
				return "", fmt.Errorf("bad COPY line %q", line)
			}
			end++
		}
	}
	return s[:end], nil
}

// ───────────────────────── MySQL dumps ─────────────────────────

// CountInsertRows reads a mysqldump file (plain SQL) and counts the rows of every `INSERT INTO `t` VALUES (…),(…);`,
// a tuple at a time, minding quotes and backslash escapes, at any line length. It also counts CREATE TABLE
// statements and sees whether the dump reached its "-- Dump completed" line.
func CountInsertRows(r io.Reader) (DataCounts, error) {
	dc := DataCounts{Rows: map[string]int64{}}
	br := bufio.NewReaderSize(r, 1<<16)
	var head []byte // the current line's beginning, outside VALUES
	values := false
	table := ""
	depth := 0
	var quote byte
	esc := false
	lastLine := ""
	for {
		b, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return dc, err
		}
		dc.Bytes++
		if values {
			switch {
			case esc:
				esc = false
			case quote != 0:
				if b == '\\' {
					esc = true
				} else if b == quote {
					quote = 0
				}
			case b == '\'' || b == '"':
				quote = b
			case b == '(':
				if depth == 0 {
					dc.Rows[table]++
				}
				depth++
			case b == ')':
				depth--
			case b == ';' && depth == 0:
				values = false
			}
			continue
		}
		// "INSERT INTO `t` VALUES (…" — mysqldump puts a space after VALUES, newer mariadb-dump may put a newline.
		if (b == ' ' || b == '\n' || b == '(') && bytes.HasPrefix(head, []byte("INSERT INTO `")) && bytes.HasSuffix(head, []byte(" VALUES")) {
			name, ok := myName(head[len("INSERT INTO "):])
			if !ok {
				return dc, fmt.Errorf("bad INSERT line %.80q", head)
			}
			table, values, depth, quote, esc = name, true, 0, 0, false
			dc.Rows[table] += 0
			head = head[:0]
			if b == '(' {
				dc.Rows[table]++
				depth = 1
			}
			continue
		}
		if b == '\n' {
			l := string(bytes.TrimRight(head, "\r"))
			if strings.HasPrefix(l, "CREATE TABLE ") {
				dc.Tables++
			}
			if strings.TrimSpace(l) != "" {
				lastLine = l
			}
			head = head[:0]
			continue
		}
		if len(head) < 1<<16 {
			head = append(head, b)
		}
	}
	if values || quote != 0 {
		return dc, fmt.Errorf("the dump ends inside the rows of %s: it is cut", table)
	}
	dc.Complete = strings.HasPrefix(lastLine, "-- Dump completed")
	return dc, nil
}

// myName reads a backquoted identifier at the start of b.
func myName(b []byte) (string, bool) {
	if len(b) < 2 || b[0] != '`' {
		return "", false
	}
	var out []byte
	for i := 1; i < len(b); i++ {
		if b[i] == '`' {
			if i+1 < len(b) && b[i+1] == '`' {
				out = append(out, '`')
				i++
				continue
			}
			return string(out), true
		}
		out = append(out, b[i])
	}
	return "", false
}

// ───────────────────────── comparison ─────────────────────────

// Check is one table compared: on the server before and after the dump, and in the dump.
type Check struct {
	Table  string `json:"table"`
	Size   int64  `json:"size"`
	Before int64  `json:"server_before"`
	After  int64  `json:"server_after"`
	Dump   int64  `json:"dump"`
	Status string `json:"status"` // ok, changed (live writes during the dump), missing, different
}

// Compare checks the dump against the server. A table whose count did not move during the dump must hold exactly
// that many rows in the dump; one that moved was being written to (a warning: stop the application for the final
// backup). Tables absent from a MySQL dump are empty (absentIsEmpty); a PostgreSQL archive has a COPY for every table.
func Compare(before, after Counts, dump DataCounts, dumpTables int, absentIsEmpty bool) (checks []Check, problems, warnings []string) {
	afterRows := map[string]int64{}
	for _, t := range after.Top {
		afterRows[t.Table] = t.Rows
	}
	for _, t := range before.Top {
		c := Check{Table: t.Table, Size: t.Size, Before: t.Rows, After: t.Rows, Dump: -1}
		if a, ok := afterRows[t.Table]; ok {
			c.After = a
		}
		n, ok := dump.Rows[t.Table]
		if !ok && absentIsEmpty {
			n, ok = 0, true
		}
		switch {
		case !ok:
			c.Status = "missing"
			problems = append(problems, fmt.Sprintf("%s: %d rows on the server, not in the dump", t.Table, c.Before))
		case c.Before != c.After:
			c.Dump = n
			c.Status = "changed"
			warnings = append(warnings, fmt.Sprintf("%s changed during the dump (%d → %d rows; the dump has %d): the application was writing", t.Table, c.Before, c.After, n))
		case n != c.Before:
			c.Dump = n
			c.Status = "different"
			problems = append(problems, fmt.Sprintf("%s: %d rows on the server, %d in the dump", t.Table, c.Before, n))
		default:
			c.Dump = n
			c.Status = "ok"
		}
		checks = append(checks, c)
	}
	switch {
	case before.Tables != after.Tables:
		warnings = append(warnings, fmt.Sprintf("the number of tables changed during the dump (%d → %d)", before.Tables, after.Tables))
	case dumpTables >= 0 && dumpTables != before.Tables:
		problems = append(problems, fmt.Sprintf("%d tables on the server, %d in the dump", before.Tables, dumpTables))
	}
	return checks, problems, warnings
}
