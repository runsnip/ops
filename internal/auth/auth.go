// Package auth guards `runsnip serve`, which can do everything runsnip can on a server: admins with passkeys
// (WebAuthn) or TOTP, a one-time setup code that only works until the first admin exists, per-admin enrolment codes,
// short sessions, re-authentication for dangerous actions, and rate limits on every guess.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/runsnip/ops/internal/host"
)

// Admin is one person who may run the server.
type Admin struct {
	ID          string                `json:"id"` // the WebAuthn user handle (random)
	Name        string                `json:"name"`
	Created     time.Time             `json:"created"`
	Passkeys    []webauthn.Credential `json:"passkeys,omitempty"`
	TOTP        string                `json:"totp,omitempty"` // base32 secret, once confirmed
	TOTPPending string                `json:"totpPending,omitempty"`
	TOTPLast    uint64                `json:"totpLast,omitempty"` // the last time step used: a code works once
	Enroll      *Code                 `json:"enroll,omitempty"`   // a pending enrolment (admin add / reset)
}

// Code is a one-time code, kept as its hash.
type Code struct {
	Hash    string    `json:"hash"`
	Expires time.Time `json:"expires"`
}

// HasCredential reports whether the admin can sign in.
func (a Admin) HasCredential() bool { return len(a.Passkeys) > 0 || a.TOTP != "" }

// WebAuthn's view of an admin.
type waUser struct{ a *Admin }

func (u waUser) WebAuthnID() []byte {
	b, _ := base64.RawURLEncoding.DecodeString(u.a.ID)
	return b
}
func (u waUser) WebAuthnName() string                       { return u.a.Name }
func (u waUser) WebAuthnDisplayName() string                { return u.a.Name }
func (u waUser) WebAuthnCredentials() []webauthn.Credential { return u.a.Passkeys }

// Config is what the first setup fixed: the origin people open (WebAuthn binds passkeys to its host).
type Config struct {
	Origin string `json:"origin,omitempty"`
}

// Store is the admins and codes, on disk.
type Store struct {
	Dir string
	Now func() time.Time
	mu  sync.Mutex
}

// NewStore keeps its files in the state folder.
func NewStore(state string) *Store { return &Store{Dir: state, Now: time.Now} }

type file struct {
	Admins []Admin `json:"admins"`
	Setup  *Code   `json:"setup,omitempty"`
	Config Config  `json:"config"`
}

func (s *Store) path() string { return filepath.Join(s.Dir, "admins.json") }

func (s *Store) load() (file, error) {
	var f file
	err := host.ReadJSON(s.path(), &f)
	return f, err
}

func (s *Store) save(f file) error { return host.WriteJSON(s.path(), f) }

// Update changes the store under its lock.
func (s *Store) Update(fn func(*file) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	return s.save(f)
}

// Admins lists the admins.
func (s *Store) Admins() ([]Admin, error) {
	f, err := s.load()
	return f.Admins, err
}

// Config reads the fixed config.
func (s *Store) Config() Config {
	f, _ := s.load()
	return f.Config
}

// SetOrigin fixes the origin passkeys are bound to.
func (s *Store) SetOrigin(origin string) error {
	return s.Update(func(f *file) error { f.Config.Origin = origin; return nil })
}

// Find returns an admin by name or id.
func (s *Store) Find(key string) (Admin, bool) {
	all, _ := s.Admins()
	for _, a := range all {
		if a.Name == key || a.ID == key {
			return a, true
		}
	}
	return Admin{}, false
}

// ───────────────────────── codes ─────────────────────────

// alphabet: no 0/O, 1/I/L — a code is read off a terminal and typed.
const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewCode returns a code like K7Q2M-9XHRT (50 bits).
func NewCode() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	out := make([]byte, 0, 11)
	for i, c := range b {
		if i == 5 {
			out = append(out, '-')
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}

func hashCode(c string) string {
	c = strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(c))
	h := sha256.Sum256([]byte(c))
	return hex.EncodeToString(h[:])
}

func (c *Code) matches(code string, now time.Time) bool {
	return c != nil && now.Before(c.Expires) && subtle.ConstantTimeCompare([]byte(c.Hash), []byte(hashCode(code))) == 1
}

// ErrAdminsExist: setup codes are only for a server without admins.
var ErrAdminsExist = errors.New("admins exist already: the setup code is only for the first one (runsnip admin add <name>)")

// SetupCode makes the one-time code that lets the first admin in (valid for ttl, replacing any earlier one).
func (s *Store) SetupCode(ttl time.Duration) (string, error) {
	code := NewCode()
	err := s.Update(func(f *file) error {
		for _, a := range f.Admins {
			if a.HasCredential() {
				return ErrAdminsExist
			}
		}
		f.Setup = &Code{Hash: hashCode(code), Expires: s.Now().Add(ttl)}
		return nil
	})
	return code, err
}

// NeedsSetup: no admin can sign in yet.
func (s *Store) NeedsSetup() bool {
	all, _ := s.Admins()
	for _, a := range all {
		if a.HasCredential() {
			return false
		}
	}
	return true
}

// UseSetupCode checks the setup code and creates the first admin (who then registers a passkey or TOTP). The code
// works once, only while no admin can sign in.
func (s *Store) UseSetupCode(code, name string) (Admin, error) {
	var created Admin
	err := s.Update(func(f *file) error {
		for _, a := range f.Admins {
			if a.HasCredential() {
				return ErrAdminsExist
			}
		}
		if !f.Setup.matches(code, s.Now()) {
			return errors.New("the setup code is wrong or expired (sudo runsnip admin setup-code prints a new one)")
		}
		if err := validName(name); err != nil {
			return err
		}
		f.Setup = nil
		created = newAdmin(name, s.Now())
		f.Admins = append(dropName(f.Admins, name), created)
		return nil
	})
	return created, err
}

func validName(n string) error {
	if len(n) < 2 || len(n) > 32 || strings.ContainsAny(n, " \t\n/\\:") {
		return fmt.Errorf("a name is 2–32 characters, no spaces")
	}
	return nil
}

func newAdmin(name string, now time.Time) Admin {
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return Admin{ID: base64.RawURLEncoding.EncodeToString(id), Name: name, Created: now.UTC()}
}

func dropName(all []Admin, name string) []Admin {
	out := all[:0]
	for _, a := range all {
		if a.Name != name {
			out = append(out, a)
		}
	}
	return out
}

// Add creates an admin with an enrolment code (they register a passkey or TOTP with it on the web).
func (s *Store) Add(name string, ttl time.Duration) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	code := NewCode()
	err := s.Update(func(f *file) error {
		for _, a := range f.Admins {
			if a.Name == name {
				return fmt.Errorf("%s exists (runsnip admin reset %s gives a new code)", name, name)
			}
		}
		a := newAdmin(name, s.Now())
		a.Enroll = &Code{Hash: hashCode(code), Expires: s.Now().Add(ttl)}
		f.Admins = append(f.Admins, a)
		return nil
	})
	return code, err
}

// Reset removes an admin's passkeys and TOTP and gives a new enrolment code.
func (s *Store) Reset(name string, ttl time.Duration) (string, error) {
	code := NewCode()
	err := s.Update(func(f *file) error {
		for i := range f.Admins {
			if f.Admins[i].Name == name {
				a := &f.Admins[i]
				a.Passkeys, a.TOTP, a.TOTPPending, a.TOTPLast = nil, "", "", 0
				a.Enroll = &Code{Hash: hashCode(code), Expires: s.Now().Add(ttl)}
				return nil
			}
		}
		return fmt.Errorf("no admin %s", name)
	})
	return code, err
}

// Remove deletes an admin. The last admin who can sign in is never removed.
func (s *Store) Remove(name string) error {
	return s.Update(func(f *file) error {
		left, found := 0, false
		for _, a := range f.Admins {
			if a.Name == name {
				found = true
			} else if a.HasCredential() {
				left++
			}
		}
		if !found {
			return fmt.Errorf("no admin %s", name)
		}
		if left == 0 {
			return errors.New("the last admin who can sign in is not removed (add another first)")
		}
		f.Admins = dropName(f.Admins, name)
		return nil
	})
}

// UseEnrolCode checks an admin's enrolment code; it stays valid until a credential is registered.
func (s *Store) UseEnrolCode(name, code string) (Admin, error) {
	a, ok := s.Find(name)
	if !ok || !a.Enroll.matches(code, s.Now()) {
		return Admin{}, errors.New("the name or the enrolment code is wrong, or the code expired (runsnip admin reset <name>)")
	}
	return a, nil
}

// ───────────────────────── TOTP (RFC 6238: SHA-1, 30 s, 6 digits) ─────────────────────────

// NewTOTPSecret is 20 random bytes in base32.
func NewTOTPSecret() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return strings.TrimRight(base32.StdEncoding.EncodeToString(b), "=")
}

// TOTPURI is the otpauth:// link authenticator apps read.
func TOTPURI(issuer, name, secret string) string {
	return "otpauth://totp/" + url.PathEscape(issuer+":"+name) + "?secret=" + secret + "&issuer=" + url.QueryEscape(issuer) + "&algorithm=SHA1&digits=6&period=30"
}

// TOTPAt is the code for a time step.
func TOTPAt(secret string, step uint64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], step)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

// totpStep checks a code within one step of now and returns its step (0 when it does not match).
func totpStep(secret, code string, now time.Time) uint64 {
	cur := uint64(now.Unix() / 30)
	for _, s := range []uint64{cur, cur - 1, cur + 1} {
		if subtle.ConstantTimeCompare([]byte(TOTPAt(secret, s)), []byte(strings.TrimSpace(code))) == 1 {
			return s
		}
	}
	return 0
}

// BeginTOTP gives an admin a new secret to scan; it counts once a code from it is confirmed.
func (s *Store) BeginTOTP(name string) (string, error) {
	secret := NewTOTPSecret()
	err := s.Update(func(f *file) error {
		for i := range f.Admins {
			if f.Admins[i].Name == name {
				f.Admins[i].TOTPPending = secret
				return nil
			}
		}
		return fmt.Errorf("no admin %s", name)
	})
	return secret, err
}

// ConfirmTOTP checks a code of the pending secret and makes it the admin's.
func (s *Store) ConfirmTOTP(name, code string) error {
	return s.Update(func(f *file) error {
		for i := range f.Admins {
			a := &f.Admins[i]
			if a.Name != name {
				continue
			}
			step := totpStep(a.TOTPPending, code, s.Now())
			if a.TOTPPending == "" || step == 0 {
				return errors.New("the code does not match: check the device's clock and type the current code")
			}
			a.TOTP, a.TOTPPending, a.TOTPLast, a.Enroll = a.TOTPPending, "", step, nil
			return nil
		}
		return fmt.Errorf("no admin %s", name)
	})
}

// CheckTOTP signs in with a code: each code works once.
func (s *Store) CheckTOTP(name, code string) (Admin, error) {
	var out Admin
	err := s.Update(func(f *file) error {
		for i := range f.Admins {
			a := &f.Admins[i]
			if a.Name != name || a.TOTP == "" {
				continue
			}
			step := totpStep(a.TOTP, code, s.Now())
			if step == 0 || step <= a.TOTPLast {
				return errBadLogin
			}
			a.TOTPLast = step
			out = *a
			return nil
		}
		return errBadLogin
	})
	return out, err
}

var errBadLogin = errors.New("the name or the code is wrong")

// ───────────────────────── passkeys ─────────────────────────

// WebAuthn returns the relying party for the fixed origin.
func (s *Store) WebAuthn() (*webauthn.WebAuthn, error) {
	o := s.Config().Origin
	if o == "" {
		return nil, errors.New("no origin fixed yet")
	}
	u, err := url.Parse(o)
	if err != nil {
		return nil, err
	}
	return webauthn.New(&webauthn.Config{RPID: u.Hostname(), RPDisplayName: "runsnip", RPOrigins: []string{o}})
}

// User is WebAuthn's view of an admin.
func User(a *Admin) webauthn.User { return waUser{a} }

// AddPasskey saves a new credential and ends a pending enrolment.
func (s *Store) AddPasskey(name string, c webauthn.Credential) error {
	return s.Update(func(f *file) error {
		for i := range f.Admins {
			if f.Admins[i].Name == name {
				f.Admins[i].Passkeys = append(f.Admins[i].Passkeys, c)
				f.Admins[i].Enroll = nil
				return nil
			}
		}
		return fmt.Errorf("no admin %s", name)
	})
}

// ByHandle finds the admin of a passkey's user handle.
func (s *Store) ByHandle(handle []byte) (Admin, bool) {
	return s.Find(base64.RawURLEncoding.EncodeToString(handle))
}

// UpdatePasskey stores a credential's new sign count after a login.
func (s *Store) UpdatePasskey(adminID string, c webauthn.Credential) error {
	return s.Update(func(f *file) error {
		for i := range f.Admins {
			if f.Admins[i].ID != adminID {
				continue
			}
			for j := range f.Admins[i].Passkeys {
				if string(f.Admins[i].Passkeys[j].ID) == string(c.ID) {
					f.Admins[i].Passkeys[j].Authenticator.SignCount = c.Authenticator.SignCount
				}
			}
		}
		return nil
	})
}
