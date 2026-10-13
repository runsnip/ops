// Package jobs records every command that changes a server — from the web, over SSH, or at the console — as a job
// on disk (its command, who ran it, when, its whole log and its exit code), and keeps heavy commands of one stack from
// overlapping with a lock.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/runsnip/ops/internal/host"
)

// Job is one run.
type Job struct {
	ID      string     `json:"id"`
	Stack   string     `json:"stack,omitempty"`
	Command []string   `json:"command"` // runsnip's arguments, never a secret value (values go through stdin)
	User    string     `json:"user"`
	Via     string     `json:"via"` // web, ssh, cli
	Started time.Time  `json:"started"`
	Ended   *time.Time `json:"ended,omitempty"`
	Exit    *int       `json:"exit,omitempty"`
	PID     int        `json:"pid,omitempty"`
	Status  string     `json:"status"` // running, ok, failed, lost
}

// Dir is the jobs folder of a state folder.
func Dir(state string) string { return filepath.Join(state, "jobs") }

func metaPath(state, id string) string { return filepath.Join(Dir(state), id, "job.json") }

// LogPath is a job's log.
func LogPath(state, id string) string { return filepath.Join(Dir(state), id, "log") }

// NewID is a sortable job id: the time, then randomness.
func NewID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// Create records a job that is about to run.
func Create(state string, j Job) (Job, error) {
	if j.ID == "" {
		j.ID = NewID()
	}
	if j.Started.IsZero() {
		j.Started = time.Now().UTC()
	}
	j.Status = "running"
	if err := os.MkdirAll(filepath.Join(Dir(state), j.ID), 0o700); err != nil {
		return j, err
	}
	if f, err := os.OpenFile(LogPath(state, j.ID), os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	return j, host.WriteJSON(metaPath(state, j.ID), j)
}

// Update rewrites a job's record.
func Update(state string, j Job) error { return host.WriteJSON(metaPath(state, j.ID), j) }

// Finish records how a job ended.
func Finish(state string, j Job, exit int) error {
	now := time.Now().UTC()
	j.Ended, j.Exit = &now, &exit
	j.Status = "ok"
	if exit != 0 {
		j.Status = "failed"
	}
	return Update(state, j)
}

// Get reads a job.
func Get(state, id string) (Job, error) {
	if strings.ContainsAny(id, "/\\.") {
		return Job{}, fmt.Errorf("bad job id")
	}
	var j Job
	b, err := os.ReadFile(metaPath(state, id))
	if err != nil {
		return j, err
	}
	err = json.Unmarshal(b, &j)
	if j.Status == "running" && j.PID > 0 && !alive(j.PID) {
		j.Status = "lost" // its process is gone without saying how it ended
	}
	return j, err
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// List returns the newest jobs first (of one stack when stack is set), at most limit.
func List(state, stack string, limit int) ([]Job, error) {
	entries, err := os.ReadDir(Dir(state))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	var out []Job
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		j, err := Get(state, e.Name())
		if err != nil || stack != "" && j.Stack != stack {
			continue
		}
		out = append(out, j)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// OpenLog opens a job's log for appending.
func OpenLog(state, id string) (*os.File, error) {
	return os.OpenFile(LogPath(state, id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// ───────────────────────── locks ─────────────────────────

// Holder is who holds a stack's lock.
type Holder struct {
	Job     string    `json:"job"`
	Command string    `json:"command"`
	User    string    `json:"user"`
	Since   time.Time `json:"since"`
}

// ErrBusy says a stack is locked by another job.
type ErrBusy struct {
	Stack  string
	Holder Holder
}

func (e ErrBusy) Error() string {
	return fmt.Sprintf("%s is busy: job %s (runsnip %s) by %s since %s — wait for it, or --wait <duration>", e.Stack, e.Holder.Job, e.Holder.Command, e.Holder.User, e.Holder.Since.Local().Format("15:04:05"))
}

func lockPath(state, stack string) string { return filepath.Join(state, "locks", stack+".lock") }

// Lock takes a stack's lock (an flock, so a crashed process frees it), waiting up to wait. The returned function
// releases it.
func Lock(state, stack string, h Holder, wait time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Join(state, "locks"), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(state, stack), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			var cur Holder
			b, _ := os.ReadFile(lockPath(state, stack))
			_ = json.Unmarshal(b, &cur)
			f.Close()
			return nil, ErrBusy{Stack: stack, Holder: cur}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if h.Since.IsZero() {
		h.Since = time.Now()
	}
	b, _ := json.Marshal(h)
	_ = f.Truncate(0)
	_, _ = f.WriteAt(b, 0)
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Busy lists the stacks whose lock is held right now, with their holders.
func Busy(state string) []ErrBusy {
	files, _ := filepath.Glob(filepath.Join(state, "locks", "*.lock"))
	var out []ErrBusy
	for _, p := range files {
		f, err := os.OpenFile(p, os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			var h Holder
			b, _ := os.ReadFile(p)
			_ = json.Unmarshal(b, &h)
			out = append(out, ErrBusy{Stack: strings.TrimSuffix(filepath.Base(p), ".lock"), Holder: h})
		} else {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		}
		f.Close()
	}
	return out
}

// Tee is a writer to the terminal and a job's log.
func Tee(w io.Writer, log *os.File) io.Writer {
	if log == nil {
		return w
	}
	return io.MultiWriter(w, log)
}
