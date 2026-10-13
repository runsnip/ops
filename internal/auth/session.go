package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Session timing: short, and short again for what can do harm. serve's flags may shorten them, never lengthen them.
var (
	IdleTimeout = 30 * time.Minute
	MaxAge      = 12 * time.Hour
	FreshFor    = 5 * time.Minute // a dangerous action needs a sign-in (or re-authentication) this recent
)

// Session is one signed-in browser.
type Session struct {
	Token    string    `json:"-"` // the cookie; on disk only its hash names the session
	AdminID  string    `json:"admin"`
	Name     string    `json:"name"`
	CSRF     string    `json:"csrf"`
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen"`
	AuthAt   time.Time `json:"authAt"` // the last sign-in or re-authentication
	// Enrolling: came in with a setup or enrolment code and may do nothing but register a credential.
	Enrolling bool `json:"enrolling,omitempty"`
}

// Fresh reports whether a dangerous action may go ahead without re-authenticating.
func (s *Session) Fresh(now time.Time) bool { return now.Sub(s.AuthAt) < FreshFor }

// Sessions keeps the signed-in browsers. With a file they survive a restart of serve (an update must not sign
// everyone out mid-deploy); the file names each session by the SHA-256 of its cookie, never the cookie itself, so
// reading it gives no way in. Mode 600 in the state folder (700).
type Sessions struct {
	mu    sync.Mutex
	m     map[string]*Session // by hash of the token
	Now   func() time.Time
	file  string
	saved time.Time
}

// NewSessions returns an empty set kept in memory only.
func NewSessions() *Sessions { return &Sessions{m: map[string]*Session{}, Now: time.Now} }

// LoadSessions returns the sessions kept in file (expired ones dropped); it writes them back on every change.
func LoadSessions(file string) *Sessions {
	ss := &Sessions{m: map[string]*Session{}, Now: time.Now, file: file}
	if b, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(b, &ss.m)
	}
	now := ss.Now()
	for k, s := range ss.m {
		if s == nil || now.Sub(s.LastSeen) > IdleTimeout || now.Sub(s.Created) > MaxAge {
			delete(ss.m, k)
		}
	}
	return ss
}

func hashToken(tok string) string { h := sha256.Sum256([]byte(tok)); return hex.EncodeToString(h[:]) }

// save writes the file (callers hold mu); touch-only changes are written at most once a minute.
func (ss *Sessions) save(touchOnly bool) {
	if ss.file == "" || touchOnly && ss.Now().Sub(ss.saved) < time.Minute {
		return
	}
	b, err := json.Marshal(ss.m)
	if err != nil {
		return
	}
	tmp := ss.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, ss.file) == nil {
		ss.saved = ss.Now()
	}
}

func token() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start opens a session for a signed-in admin (enrolling: one that may only register a credential).
func (ss *Sessions) Start(a Admin, enrolling bool) *Session {
	now := ss.Now()
	s := &Session{Token: token(), AdminID: a.ID, Name: a.Name, CSRF: token(), Created: now, LastSeen: now, AuthAt: now, Enrolling: enrolling}
	ss.mu.Lock()
	ss.m[hashToken(s.Token)] = s
	ss.save(false)
	ss.mu.Unlock()
	return s
}

// Get returns a live session (touching it), nil when unknown, idle too long or too old.
func (ss *Sessions) Get(tok string) *Session {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	k := hashToken(tok)
	s, ok := ss.m[k]
	if !ok {
		return nil
	}
	now := ss.Now()
	if now.Sub(s.LastSeen) > IdleTimeout || now.Sub(s.Created) > MaxAge {
		delete(ss.m, k)
		ss.save(false)
		return nil
	}
	s.Token = tok
	s.LastSeen = now
	ss.save(true)
	return s
}

// Enrolled turns an enrolling session into a signed-in one, freshly authenticated.
func (ss *Sessions) Enrolled(tok string) {
	ss.mu.Lock()
	if s, ok := ss.m[hashToken(tok)]; ok {
		s.Enrolling, s.AuthAt = false, ss.Now()
		ss.save(false)
	}
	ss.mu.Unlock()
}

// Reauth marks a session freshly authenticated.
func (ss *Sessions) Reauth(tok string) {
	ss.mu.Lock()
	if s, ok := ss.m[hashToken(tok)]; ok {
		s.AuthAt = ss.Now()
		ss.save(false)
	}
	ss.mu.Unlock()
}

// End signs a session out.
func (ss *Sessions) End(tok string) {
	ss.mu.Lock()
	delete(ss.m, hashToken(tok))
	ss.save(false)
	ss.mu.Unlock()
}

// EndAdmin signs an admin out everywhere (removed, reset).
func (ss *Sessions) EndAdmin(adminID string) {
	ss.mu.Lock()
	for k, s := range ss.m {
		if s.AdminID == adminID {
			delete(ss.m, k)
		}
	}
	ss.save(false)
	ss.mu.Unlock()
}

// ───────────────────────── rate limits ─────────────────────────

// Limiter counts failures per key (an address, a name) in a window; past the limit the key waits.
type Limiter struct {
	Max    int
	Window time.Duration
	Now    func() time.Time
	mu     sync.Mutex
	fails  map[string][]time.Time
}

// NewLimiter allows max failures per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{Max: max, Window: window, Now: time.Now, fails: map[string][]time.Time{}}
}

func (l *Limiter) recent(key string) []time.Time {
	now := l.Now()
	var keep []time.Time
	for _, t := range l.fails[key] {
		if now.Sub(t) < l.Window {
			keep = append(keep, t)
		}
	}
	l.fails[key] = keep
	return keep
}

// Allowed reports whether key may try again, and if not, for how long it waits.
func (l *Limiter) Allowed(keys ...string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if r := l.recent(k); len(r) >= l.Max {
			return false, l.Window - l.Now().Sub(r[0])
		}
	}
	return true, 0
}

// Fail records a failure for each key.
func (l *Limiter) Fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		l.fails[k] = append(l.recent(k), l.Now())
	}
}

// Reset clears a key after a success.
func (l *Limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}
