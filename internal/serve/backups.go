package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/runsnip/ops/internal/stack"
)

// A backup leaves the server only to an admin who authenticated in the last minutes: the page asks for a ticket
// with a POST (fresh session, CSRF, origin), then the browser downloads with a GET carrying that ticket. A ticket
// works once, for one minute, for the session that asked, for that one file. The download and how much of it was
// sent go into the audit log.

const ticketTTL = time.Minute

type ticket struct {
	session, stack, file string
	until                time.Time
}

type tickets struct {
	mu sync.Mutex
	m  map[string]ticket
}

func (t *tickets) issue(tk ticket) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	id := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]ticket{}
	}
	for k, v := range t.m {
		if time.Now().After(v.until) {
			delete(t.m, k)
		}
	}
	t.m[id] = tk
	return id
}

// take spends a ticket.
func (t *tickets) take(id string, now time.Time) (ticket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tk, ok := t.m[id]
	delete(t.m, id)
	return tk, ok && now.Before(tk.until)
}

// sessionID names a session in a ticket without keeping its cookie.
func sessionID(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

type backupRow struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

// findBackup is the stack's backup called name: only a file the listing shows, never a path.
func findBackup(st *stack.Stack, name string) (stack.Backup, bool) {
	if name == "" || filepath.Base(name) != name || name[0] == '.' {
		return stack.Backup{}, false
	}
	bs, err := st.Backups()
	if err != nil {
		return stack.Backup{}, false
	}
	for _, b := range bs {
		if b.Name == name {
			return b, true
		}
	}
	return stack.Backup{}, false
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request, _ *state) {
	st, _, err := s.openStack(r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	bs, err := st.Backups()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	rows := []backupRow{}
	for _, b := range bs {
		rows = append(rows, backupRow{Name: b.Name, Size: b.Size, Time: b.Time})
	}
	writeJSON(w, map[string]any{"dir": st.BackupDir(), "backups": rows})
}

// backupTicket answers a fresh session's request to download one backup with a one-time ticket.
func (s *Server) backupTicket(w http.ResponseWriter, r *http.Request, st0 *state) {
	var in struct {
		File string `json:"file"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, err.Error())
		return
	}
	name := r.PathValue("name")
	st, _, err := s.openStack(name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if _, ok := findBackup(st, in.File); !ok {
		fail(w, http.StatusNotFound, "no such backup")
		return
	}
	id := s.tickets.issue(ticket{session: sessionID(st0.Token), stack: name, file: in.File, until: s.Now().Add(ticketTTL)})
	writeJSON(w, map[string]string{"url": "/api/stacks/" + name + "/backups/download?ticket=" + id})
}

// downloadBackup streams the file a ticket names.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, st0 *state) {
	name := r.PathValue("name")
	tk, ok := s.tickets.take(r.URL.Query().Get("ticket"), s.Now())
	if !ok || tk.session != sessionID(st0.Token) || tk.stack != name {
		s.audit(r, st0.Session, "backup.download.refused", map[string]any{"stack": name})
		fail(w, http.StatusForbidden, "this download link is spent or expired: press Download again")
		return
	}
	st, _, err := s.openStack(name)
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	b, ok := findBackup(st, tk.file)
	if !ok {
		fail(w, http.StatusNotFound, "no such backup")
		return
	}
	f, err := os.Open(filepath.Join(st.BackupDir(), b.Name))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", b.Name))
	h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	start := s.Now()
	n, err := io.Copy(w, f)
	s.audit(r, st0.Session, "backup.download", map[string]any{"stack": name, "file": b.Name, "size": fi.Size(), "sent": n,
		"complete": err == nil && n == fi.Size(), "seconds": s.Now().Sub(start).Seconds()})
}
