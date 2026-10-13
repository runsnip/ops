// Package envform is `runsnip env combine` made editable: .env laid over the example's lines (its order, comments
// and sections), with .env's values and on/off. Untouched, the form writes byte for byte what combine writes.
package envform

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// A variable is ON when its line is active (`KEY=value`) and OFF when it is commented (`# KEY=value`): off keeps its
// value, for when it is turned back on. The state is .env's, as written there:
//
//   - active in .env: on, with .env's value — opened, if the example has it commented;
//   - commented in .env, and nowhere active: off, with that value;
//   - not in .env, active in the example: NEW — on and empty, so someone decides; the example's value is offered,
//     never slipped in;
//   - not in .env, commented in the example: off, as the example has it;
//   - only in .env: kept, on or off as it was, in a section of its own at the end.
//
// With no .env yet the form is the example, as `cp .env.example .env` makes it. Saving backs the old file up to
// .env.bak-<time>, writes mode 600 atomically, and refuses when .env changed after the form read it. Values are
// written in the one form bash and compose read alike: as they are when they hold only safe characters, else in
// single quotes.

// Field is one variable: of .env.example, or only of .env (Extra).
type Field struct {
	Key     string
	Section string
	// Help is the comment block right above the variable in the example, and
	// the comment after it, without the #s.
	Help string
	// Default is the value .env.example gives.
	Default string
	// Optional variables are commented out in .env.example (`# KEY=`).
	Optional bool
	Secret   bool
	// Set: .env has the variable, on or off.
	Set bool
	// New: .env exists, and does not have this variable the example names.
	New bool
	// Opened: commented in the example, on in .env.
	Opened bool
	// Extra: only .env names it.
	Extra   bool
	Enabled bool
	Value   string

	// written is the value as .env (or the example) wrote it, comment and
	// all, so an untouched value is written back exactly as it was.
	written string
	// comment is the comment after the value, kept through an edit.
	comment string
	// original and originalEnabled are as read, to tell an edit.
	original        string
	originalEnabled bool
	// line is the example's own line.
	line string
}

type exampleLine struct {
	text  string
	field int // -1 for a line kept as text
}

// EnvForm is .env.example's lines with .env's values.
type EnvForm struct {
	Fields []Field
	// HasEnv: there is a .env to combine with.
	HasEnv bool
	lines  []exampleLine
	// loaded is .env as it was read; a save compares it with the file now.
	loaded []byte
}

// The patterns are combine's. In the example a variable starts its line, a
// commented one is `#` and at most one space before it; .env lines may also
// be indented, and active ones exported.
var (
	exampleVar   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	commentedVar = regexp.MustCompile(`^# ?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	envVar       = regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	envOff       = regexp.MustCompile(`^[ \t]*# ?([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	sectionLine  = regexp.MustCompile(`^#\s*-{2,}\s*(.+?)[\s-]*$`)
	secretName   = regexp.MustCompile(`(SECRET|PASSWORD|TOKEN|_JWT_KEY|API_KEY|ACCESS_KEY|PRIVATE)`)
	safeValue    = regexp.MustCompile(`^[A-Za-z0-9_./:@%+,=~^-]*$`)
)

// The section only-.env variables go in, as combine writes it.
const (
	extraSection = "only in .env"
	extraHeader  = "# ---- only in the old .env, the example does not name them -------------------"
	extraNote    = "# Kept so nothing is lost. Still used? If not, delete them."
)

// ErrEnvChanged: .env is not what the form read.
var ErrEnvChanged = errors.New(".env changed since it was read (did a command just write it?) — reload, then edit again")

// splitComment parses what follows `KEY=`: the value as written, and the
// comment after it. A comment starts at a # with a space before it, outside
// quotes — as both bash and compose read it.
func splitComment(rest string) (raw, comment string) {
	rest = strings.TrimSpace(rest)
	if rest != "" && (rest[0] == '\'' || rest[0] == '"') {
		if end := strings.IndexByte(rest[1:], rest[0]); end >= 0 {
			raw, after := rest[:end+2], strings.TrimSpace(rest[end+2:])
			return raw, strings.TrimSpace(strings.TrimPrefix(after, "#"))
		}
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] == '#' && (rest[i-1] == ' ' || rest[i-1] == '\t') {
			return strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+1:])
		}
	}
	return rest, ""
}

// decode is the value a raw .env value stands for.
func decode(raw string) string {
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return raw[1 : len(raw)-1]
	}
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(raw[1 : len(raw)-1])
	}
	return raw
}

// encode writes a value so that bash and compose both read it back as it is.
// Single quotes are literal in both; a value that holds one has no form both
// read alike, and is refused rather than written wrong.
func encode(key, value string) (string, error) {
	if strings.ContainsAny(value, "\n\r") {
		return "", fmt.Errorf("%s: a value cannot hold a line break", key)
	}
	if safeValue.MatchString(value) {
		return value, nil
	}
	if strings.Contains(value, "'") {
		return "", fmt.Errorf("%s: a value with ' is read differently by bash and docker compose and cannot be written safely", key)
	}
	return "'" + value + "'", nil
}

// envState is .env as combine reads it: active values (a later line wins, as
// compose reads it), commented ones (the last), and the order names first
// appear in.
type envState struct {
	on, off map[string]string
	order   []string
}

func readEnv(content []byte) envState {
	s := envState{on: map[string]string{}, off: map[string]string{}}
	seen := map[string]bool{}
	note := func(key string) {
		if !seen[key] {
			seen[key] = true
			s.order = append(s.order, key)
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if m := envVar.FindStringSubmatch(line); m != nil {
			note(m[1])
			s.on[m[1]] = m[2]
		} else if m := envOff.FindStringSubmatch(line); m != nil {
			note(m[1])
			s.off[m[1]] = m[2]
		}
	}
	return s
}

func readIfExists(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return content, err
}

// take sets the field from a value as written, on or off.
func (f *Field) take(written string, enabled bool) {
	raw, comment := splitComment(written)
	f.written, f.comment = written, comment
	f.Value, f.Enabled = decode(raw), enabled
}

func (f *Field) settle() { f.original, f.originalEnabled = f.Value, f.Enabled }

// fromEnv fills the field from .env, if .env has it; on wins over off.
func (f *Field) fromEnv(env envState) bool {
	if written, ok := env.on[f.Key]; ok {
		f.take(written, true)
	} else if written, ok := env.off[f.Key]; ok {
		f.take(written, false)
	} else {
		return false
	}
	f.Set = true
	return true
}

// Load builds the form from the example at examplePath, with the values of the .env at envPath.
func Load(examplePath, envPath string) (*EnvForm, error) {
	example, err := os.ReadFile(examplePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read the example: %w", err)
	}
	loaded, err := readIfExists(envPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read .env: %w", err)
	}
	env := readEnv(loaded)

	form := &EnvForm{loaded: loaded, HasEnv: loaded != nil}
	seen := map[string]bool{}
	section := ""
	var help []string

	for _, line := range strings.Split(strings.TrimSuffix(string(example), "\n"), "\n") {
		trimmed := strings.TrimSpace(line)

		if m := sectionLine.FindStringSubmatch(trimmed); m != nil && !commentedVar.MatchString(trimmed) {
			section = m[1]
			help = nil
			form.lines = append(form.lines, exampleLine{text: line, field: -1})
			continue
		}

		var key, rest string
		optional := false
		if m := exampleVar.FindStringSubmatch(line); m != nil {
			key, rest = m[1], m[2]
		} else if m := commentedVar.FindStringSubmatch(line); m != nil {
			key, rest, optional = m[1], m[2], true
		}

		// A variable named twice in the example is one field: its second line
		// is kept as text, so the file's shape does not change.
		if key != "" && !seen[key] {
			def, comment := splitComment(rest)
			if comment != "" {
				help = append(help, comment)
			}
			f := Field{
				Key: key, Section: section, Help: strings.Join(help, "\n"),
				Default: decode(def), Optional: optional, Secret: IsSecret(key), line: line,
			}
			switch {
			case f.fromEnv(env):
				f.Opened = optional && f.Enabled
			case optional:
				f.take(rest, false)
			case form.HasEnv:
				f.New, f.Enabled = true, true
			default:
				f.take(rest, true)
			}
			f.settle()

			// A commented example line .env does not have does not take the
			// name: an active line for it further on is still the one filled,
			// as combine does.
			if !optional || f.Set {
				seen[key] = true
			}
			form.Fields = append(form.Fields, f)
			form.lines = append(form.lines, exampleLine{field: len(form.Fields) - 1})
			help = nil
			continue
		}

		switch {
		case trimmed == "":
			help = nil
		case strings.HasPrefix(trimmed, "#"):
			help = append(help, strings.TrimSpace(strings.TrimPrefix(trimmed, "#")))
		}
		form.lines = append(form.lines, exampleLine{text: line, field: -1})
	}

	for _, key := range env.order {
		if seen[key] {
			continue
		}
		f := Field{Key: key, Section: extraSection, Extra: true, Secret: IsSecret(key),
			Help: "Only in .env; the example does not name it. Kept so nothing is lost; check whether it is still used."}
		f.fromEnv(env)
		f.settle()
		form.Fields = append(form.Fields, f)
	}
	return form, nil
}

// Edited: the value or the on/off is not the one the form read.
func (f *Field) Edited() bool { return f.Value != f.original || f.Enabled != f.originalEnabled }

// Revert puts the field back as it was read.
func (f *Field) Revert() { f.Value, f.Enabled = f.original, f.originalEnabled }

// rendered is the field's line in the new .env.
func (f *Field) rendered() (string, error) {
	if !f.Edited() && !f.Set {
		// What the example says: its own line, or empty for a new variable.
		if f.New {
			return f.Key + "=", nil
		}
		return f.line, nil
	}

	value := f.written
	if f.Value != f.original {
		encoded, err := encode(f.Key, f.Value)
		if err != nil {
			return "", err
		}
		value = encoded
		if f.comment != "" {
			value += " # " + f.comment
		}
	}
	if !f.Enabled {
		return "# " + f.Key + "=" + value, nil
	}
	return f.Key + "=" + value, nil
}

// Render is the new .env.
func (f *EnvForm) Render() (string, error) {
	var out strings.Builder
	write := func(line string) { out.WriteString(line + "\n") }

	for _, line := range f.lines {
		if line.field < 0 {
			write(line.text)
			continue
		}
		text, err := f.Fields[line.field].rendered()
		if err != nil {
			return "", err
		}
		write(text)
	}

	header := false
	for i := range f.Fields {
		if !f.Fields[i].Extra {
			continue
		}
		if !header {
			write("")
			write(extraHeader)
			write(extraNote)
			header = true
		}
		text, err := f.Fields[i].rendered()
		if err != nil {
			return "", err
		}
		write(text)
	}
	return out.String(), nil
}

// Save writes target (.env) from the form: the old file to .env.bak-<time> first, then
// the new one beside it and renamed over, both mode 600.
func (f *EnvForm) Save(target string, now time.Time) (backup string, err error) {

	content, err := f.Render()
	if err != nil {
		return "", err
	}

	current, err := readIfExists(target)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(current, f.loaded) {
		return "", ErrEnvChanged
	}

	if current != nil {
		backup = target + ".bak-" + now.Format("20060102-150405")
		if err := os.WriteFile(backup, current, 0o600); err != nil {
			return "", fmt.Errorf("cannot back up .env: %w", err)
		}
	}

	// The file keeps its mode (a group the operator chose, say), except that others never read it.
	mode := os.FileMode(0o600)
	if st, err := os.Stat(target); err == nil && st.Mode().Perm()&0o007 == 0 {
		mode = st.Mode().Perm()
	}
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return backup, err
	}
	// WriteFile keeps the mode of a file that was already there.
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return backup, err
	}
	return backup, nil
}

// Edited is how many fields the form changed.
func (f *EnvForm) Edited() int {
	count := 0
	for i := range f.Fields {
		if f.Fields[i].Edited() {
			count++
		}
	}
	return count
}

// Combined is combine's report: kept on, opened, kept off, new and still
// empty, and only in .env.
type Combined struct {
	Kept, Opened, Off int
	New, Extra        []string
}

// Combined reports what combine did.
func (f *EnvForm) Combined() Combined {
	var c Combined
	for _, field := range f.Fields {
		switch {
		case field.Extra:
			c.Extra = append(c.Extra, field.Key)
		case field.Opened:
			c.Opened++
		case field.Set && field.originalEnabled:
			c.Kept++
		case field.Set:
			c.Off++
		case field.New && field.Enabled && field.Value == "":
			c.New = append(c.New, field.Key)
		}
	}
	return c
}

// IsSecret reports whether a variable holds a secret to mask on screen: a password, secret, token, private or API
// key, any *_KEY or *_KEY_ID (but never a public key or an address), or SMTP_URL (it carries a password).
func IsSecret(key string) bool {
	if strings.HasSuffix(key, "_PUBLIC_KEY") || strings.HasSuffix(key, "_PUBLIC_KEYS") || strings.HasSuffix(key, "_URL") && key != "SMTP_URL" {
		return false
	}
	return secretName.MatchString(key) || strings.HasSuffix(key, "_KEY") || strings.HasSuffix(key, "_KEY_ID") ||
		strings.HasSuffix(key, "_KEYS") || key == "SMTP_URL"
}

// Find returns the field of key (nil when the form has none).
func (f *EnvForm) Find(key string) *Field {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return &f.Fields[i]
		}
	}
	return nil
}

// Set gives key a value and turns it on; a key neither file names is added at the end, as only-in-.env.
func (f *EnvForm) Set(key, value string) {
	fd := f.Find(key)
	if fd == nil {
		f.Fields = append(f.Fields, Field{Key: key, Section: extraSection, Extra: true, Secret: IsSecret(key)})
		fd = &f.Fields[len(f.Fields)-1]
	}
	fd.Value, fd.Enabled = value, true
}

// Toggle turns key on or off (off keeps its value, commented).
func (f *EnvForm) Toggle(key string, on bool) bool {
	fd := f.Find(key)
	if fd == nil {
		return false
	}
	fd.Enabled = on
	return true
}

// Current is .env as the form read it ("" when there was none).
func (f *EnvForm) Current() string { return string(f.loaded) }

// Diff is a unified-style line diff from old to new ("-" removed, "+" added, 2 lines of context), empty when equal.
func Diff(old, new string) string {
	if old == new {
		return ""
	}
	a, b := strings.Split(strings.TrimSuffix(old, "\n"), "\n"), strings.Split(strings.TrimSuffix(new, "\n"), "\n")
	if old == "" {
		a = nil
	}
	// LCS table: .env files are a few hundred lines.
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte
		text string
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i, j = i+1, j+1
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			ops = append(ops, op{'+', b[j]})
			j++
		default:
			ops = append(ops, op{'-', a[i]})
			i++
		}
	}
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.kind != ' ' {
			for d := max(0, k-2); d <= min(len(ops)-1, k+2); d++ {
				keep[d] = true
			}
		}
	}
	var out strings.Builder
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap {
			out.WriteString("@@\n")
			gap = false
		}
		out.WriteByte(o.kind)
		out.WriteString(o.text + "\n")
	}
	return out.String()
}
