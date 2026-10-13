// Package host is runsnip's own state on a server: the registered stacks, the admins, the jobs, the locks, the
// audit log and the deploy keys, under one folder (/var/lib/runsnip, or RUNSNIP_STATE). Everything is a file, mode
// 600 in a 700 folder, written atomically.
package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// DefaultState is where runsnip keeps its state on a server.
const DefaultState = "/var/lib/runsnip"

// Dir is the state folder: RUNSNIP_STATE, else /var/lib/runsnip.
func Dir() string {
	if d := os.Getenv("RUNSNIP_STATE"); d != "" {
		return d
	}
	return DefaultState
}

// Ensure creates the state folder (700) and its subfolders.
func Ensure(dir string) error {
	for _, d := range []string{dir, filepath.Join(dir, "jobs"), filepath.Join(dir, "locks"), filepath.Join(dir, "keys")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		_ = os.Chmod(d, 0o700)
	}
	return nil
}

// WriteJSON writes v to path atomically, mode 600.
func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(b, '\n'))
}

// WriteFile writes path atomically, mode 600.
func WriteFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadJSON reads path into v; a missing file leaves v as it is.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ───────────────────────── stacks ─────────────────────────

// StackEntry is a registered stack.
type StackEntry struct {
	Name    string    `json:"name"`
	Dir     string    `json:"dir"`  // the infra checkout (holds stack.yaml)
	Data    string    `json:"data"` // the data folder the maintainer chose
	Repo    string    `json:"repo,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	KeyFile string    `json:"keyFile,omitempty"` // the deploy key, for a private repository
	Added   time.Time `json:"added"`
	AddedBy string    `json:"addedBy,omitempty"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidName checks a stack's name: lowercase letters, digits and hyphens, 1–32 characters.
func ValidName(n string) error {
	if !nameRe.MatchString(n) {
		return fmt.Errorf("%q: a stack's name is lowercase letters, digits and hyphens (1–32)", n)
	}
	return nil
}

var regMu sync.Mutex

func stacksPath(dir string) string { return filepath.Join(dir, "stacks.json") }

// Stacks lists the registered stacks, by name.
func Stacks(dir string) ([]StackEntry, error) {
	var s []StackEntry
	if err := ReadJSON(stacksPath(dir), &s); err != nil {
		return nil, err
	}
	sort.Slice(s, func(i, j int) bool { return s[i].Name < s[j].Name })
	return s, nil
}

// Find returns a registered stack.
func Find(dir, name string) (StackEntry, bool, error) {
	all, err := Stacks(dir)
	if err != nil {
		return StackEntry{}, false, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, true, nil
		}
	}
	return StackEntry{}, false, nil
}

// Register adds (or replaces, on re-adopt) a stack.
func Register(dir string, e StackEntry) error {
	regMu.Lock()
	defer regMu.Unlock()
	all, err := Stacks(dir)
	if err != nil {
		return err
	}
	out := all[:0]
	for _, s := range all {
		if s.Name != e.Name {
			out = append(out, s)
		}
	}
	return WriteJSON(stacksPath(dir), append(out, e))
}

// Unregister removes a stack from the registry (its files stay).
func Unregister(dir, name string) error {
	regMu.Lock()
	defer regMu.Unlock()
	all, err := Stacks(dir)
	if err != nil {
		return err
	}
	out := all[:0]
	found := false
	for _, s := range all {
		if s.Name == name {
			found = true
			continue
		}
		out = append(out, s)
	}
	if !found {
		return fmt.Errorf("no stack %q", name)
	}
	return WriteJSON(stacksPath(dir), out)
}

// ───────────────────────── audit ─────────────────────────

var auditMu sync.Mutex

// Audit appends one record to audit.log: append-only (O_APPEND, never rewritten), mode 600, one JSON object a line.
// Values of .env never go in: the caller passes key names.
func Audit(dir string, rec map[string]any) error {
	auditMu.Lock()
	defer auditMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	rec["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "audit.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
