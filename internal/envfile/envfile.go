// Package envfile reads and writes a stack's .env in place: comments, order and the file's mode stay, every write is
// atomic (a temporary file renamed over), and values are read the way docker compose reads them.
package envfile

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// File is one .env.
type File struct {
	Path  string
	lines []string
}

// ErrMissing is returned (wrapped) when the file does not exist.
var ErrMissing = errors.New("missing")

// Load reads path.
func Load(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s %w: cp .env.example .env && chmod 600 .env, then edit it (or the Env tab)", path, ErrMissing)
	}
	if err != nil {
		return nil, err
	}
	return Parse(path, b), nil
}

// Parse reads b as the content of path.
func Parse(path string, b []byte) *File {
	f := &File{Path: path}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		f.lines = append(f.lines, sc.Text())
	}
	return f
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// assignment splits an active line `KEY=value` (an `export ` prefix allowed).
func assignment(l string) (key, raw string, ok bool) {
	t := strings.TrimLeft(l, " \t")
	if strings.HasPrefix(t, "#") {
		return "", "", false
	}
	t = strings.TrimPrefix(t, "export ")
	k, v, ok := strings.Cut(t, "=")
	if !ok || !keyRe.MatchString(strings.TrimSpace(k)) {
		return "", "", false
	}
	return strings.TrimSpace(k), v, true
}

// Value decodes a raw value as compose does: quotes removed (\n kept as two characters inside single quotes,
// turned into a line break inside double quotes), an inline ` #comment` dropped from unquoted values.
func Value(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && v[0] == '\'' {
		if i := strings.LastIndexByte(v, '\''); i > 0 {
			return v[1:i]
		}
	}
	if len(v) >= 2 && v[0] == '"' {
		if i := strings.LastIndexByte(v, '"'); i > 0 {
			return strings.NewReplacer(`\n`, "\n", `\"`, `"`, `\\`, `\`).Replace(v[1:i])
		}
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// Get returns the last active value of key ("" when unset), as compose reads it.
func (f *File) Get(key string) string {
	v, _ := f.Lookup(key)
	return v
}

// Lookup returns the last active value of key and whether it is set.
func (f *File) Lookup(key string) (string, bool) {
	v, set := "", false
	for _, l := range f.lines {
		if k, raw, ok := assignment(l); ok && k == key {
			v, set = Value(raw), true
		}
	}
	return v, set
}

// Values returns every active variable.
func (f *File) Values() map[string]string {
	m := map[string]string{}
	for _, l := range f.lines {
		if k, raw, ok := assignment(l); ok {
			m[k] = Value(raw)
		}
	}
	return m
}

// Keys lists the active variables in file order, once each.
func (f *File) Keys() []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range f.lines {
		if k, _, ok := assignment(l); ok && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// Has lists the active keys whose value contains marker (e.g. CHANGE_ME placeholders).
func (f *File) Has(marker string) []string {
	var keys []string
	for _, l := range f.lines {
		if k, raw, ok := assignment(l); ok && strings.Contains(raw, marker) {
			keys = append(keys, k)
		}
	}
	return keys
}

// Set replaces the first active assignment of key and drops later ones (which would win otherwise); with no active
// line the assignment is appended. Commented lines are never touched. The file is written at once.
func (f *File) Set(key, value string) error {
	f.SetLine(key, value)
	return f.Save()
}

// SetLine changes the content without writing it.
func (f *File) SetLine(key, value string) {
	line := key + "=" + Quote(value)
	found := false
	out := f.lines[:0:0]
	for _, l := range f.lines {
		if k, _, ok := assignment(l); ok && k == key {
			if found {
				continue
			}
			found = true
			out = append(out, line)
			continue
		}
		out = append(out, l)
	}
	if !found {
		out = append(out, line)
	}
	f.lines = out
}

// Append adds raw lines at the end without writing.
func (f *File) Append(lines ...string) { f.lines = append(f.lines, lines...) }

// Lines returns the content.
func (f *File) Lines() []string { return append([]string(nil), f.lines...) }

var safeRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./~-]*$`)

// Quote writes a value in the one form bash (`. ./.env`) and compose read alike: as it is when it holds only safe
// characters, else single-quoted.
func Quote(v string) string {
	if safeRe.MatchString(v) {
		return v
	}
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

// Save writes the file atomically with mode 600 (or the mode it has when it is stricter).
func (f *File) Save() error { return WriteAtomic(f.Path, []byte(strings.Join(f.lines, "\n")+"\n")) }

// WriteAtomic writes b to path through a temporary file in the same directory, mode 600 unless path already has a
// stricter one.
func WriteAtomic(path string, b []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&^0o600 == 0 {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".env.tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Toggle turns key on (`KEY=value`) or off (`# KEY=value`, the value kept for later), writing the file.
func (f *File) Toggle(key string, on bool, value string) error {
	done := false
	for i, l := range f.lines {
		t := strings.TrimSpace(l)
		if k, _, ok := assignment(l); ok && k == key {
			if !on {
				f.lines[i] = "# " + strings.TrimLeft(l, " \t")
			}
			done = true
			continue
		}
		if !done && on && strings.HasPrefix(t, "#") {
			c := strings.TrimSpace(strings.TrimPrefix(t, "#"))
			if k, raw, ok := assignment(c); ok && k == key {
				if value == "" {
					value = Value(raw)
				}
				f.lines[i] = key + "=" + Quote(value)
				done = true
			}
		}
	}
	if !done && on {
		f.lines = append(f.lines, key+"="+Quote(value))
	}
	return f.Save()
}
