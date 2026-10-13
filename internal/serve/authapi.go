package serve

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/runsnip/ops/internal/auth"
)

const sessionCookie = "runsnip_session"

// level is what a handler needs.
type level int

const (
	enrolling level = iota // a session that may only register its first credential
	signedIn
	fresh // signed in, and authenticated within the last minutes (dangerous actions)
)

// state is a browser's session, with whether it is still enrolling.
type state struct {
	*auth.Session
	enrolling bool
}

func (s *Server) session(r *http.Request) *state {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	sess := s.Sessions.Get(c.Value)
	if sess == nil {
		return nil
	}
	if a, ok := s.Auth.Find(sess.AdminID); !ok || a.Name != sess.Name {
		s.Sessions.End(c.Value) // removed, or reset under another id
		return nil
	}
	return &state{Session: sess, enrolling: sess.Enrolling}
}

// origin is where the page must be loaded from: the fixed origin, else this request's own.
func (s *Server) origin(r *http.Request) string {
	if o := s.Auth.Config().Origin; o != "" {
		return o
	}
	return requestOrigin(r)
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// sameOrigin: a request that changes something comes from the page itself.
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o != "" && (o == s.origin(r) || o == requestOrigin(r))
}

// need guards a handler: the session level it needs, the CSRF token and the origin on every POST.
func (s *Server) need(l level, h func(http.ResponseWriter, *http.Request, *state)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := s.session(r)
		if st == nil {
			fail(w, http.StatusUnauthorized, "sign in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(st.CSRF)) != 1 {
				fail(w, http.StatusForbidden, "bad CSRF token or origin")
				return
			}
		}
		switch {
		case l > enrolling && st.enrolling:
			fail(w, http.StatusForbidden, "register a passkey or TOTP first")
			return
		case l == fresh && !st.Fresh(s.Now()):
			fail(w, http.StatusPreconditionRequired, "reauth")
			return
		}
		h(w, r, st)
	}
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, a auth.Admin, enrolling bool) *auth.Session {
	sess := s.Sessions.Start(a, enrolling)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sess.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: strings.HasPrefix(s.origin(r), "https://")})
	return sess
}

// limited answers 429 when a key has failed too often.
func (s *Server) limited(w http.ResponseWriter, keys ...string) bool {
	if ok, wait := s.Limit.Allowed(keys...); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		fail(w, http.StatusTooManyRequests, fmt.Sprintf("too many tries: wait %s", wait.Round(time.Second)))
		return true
	}
	return false
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st := s.session(r)
	out := map[string]any{"needsSetup": s.Auth.NeedsSetup(), "version": s.Env.Version, "signedIn": st != nil}
	if st != nil {
		out["name"], out["csrf"], out["enrolling"], out["fresh"] = st.Name, st.CSRF, st.enrolling, st.Fresh(s.Now())
	}
	writeJSON(w, out)
}

// setup turns the one-time setup code into the first admin, signed in to register a credential. It also fixes the
// origin passkeys are bound to: the one this page is open on.
func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limited(w, "ip:"+ip, "setup") {
		return
	}
	if !s.sameOrigin(r) {
		fail(w, http.StatusForbidden, "bad origin")
		return
	}
	var in struct{ Code, Name string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	a, err := s.Auth.UseSetupCode(in.Code, strings.TrimSpace(in.Name))
	if err != nil {
		s.Limit.Fail("ip:"+ip, "setup")
		s.audit(r, nil, "setup.failed", nil)
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	if s.Auth.Config().Origin == "" {
		_ = s.Auth.SetOrigin(requestOrigin(r))
	}
	sess := s.startSession(w, r, a, true)
	s.audit(r, sess, "setup", map[string]any{"admin": a.Name, "origin": s.origin(r)})
	writeJSON(w, map[string]any{"name": a.Name, "csrf": sess.CSRF})
}

// enroll turns an admin's enrolment code (runsnip admin add / reset) into a session that registers a credential.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	var in struct{ Name, Code string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if s.limited(w, "ip:"+ip, "name:"+in.Name) {
		return
	}
	if !s.sameOrigin(r) {
		fail(w, http.StatusForbidden, "bad origin")
		return
	}
	a, err := s.Auth.UseEnrolCode(in.Name, in.Code)
	if err != nil {
		s.Limit.Fail("ip:"+ip, "name:"+in.Name)
		s.audit(r, nil, "enroll.failed", map[string]any{"admin": in.Name})
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	sess := s.startSession(w, r, a, true)
	s.audit(r, sess, "enroll", map[string]any{"admin": a.Name})
	writeJSON(w, map[string]any{"name": a.Name, "csrf": sess.CSRF})
}

// ───────────────────────── ceremonies ─────────────────────────

// ceremonies keep WebAuthn's challenge state server-side for a few minutes, keyed by a random id the page sends back.
type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

type ceremony struct {
	data    webauthn.SessionData
	expires time.Time
}

func newCeremonies() *ceremonies { return &ceremonies{m: map[string]ceremony{}} }

func (c *ceremonies) put(d *webauthn.SessionData) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	c.mu.Lock()
	for k, v := range c.m {
		if time.Now().After(v.expires) {
			delete(c.m, k)
		}
	}
	c.m[id] = ceremony{*d, time.Now().Add(5 * time.Minute)}
	c.mu.Unlock()
	return id
}

func (c *ceremonies) take(id string) (webauthn.SessionData, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[id]
	delete(c.m, id)
	if !ok || time.Now().After(v.expires) {
		return webauthn.SessionData{}, false
	}
	return v.data, true
}

// body re-reads a request's JSON: {"ceremony": id, "credential": {...}} → the id and a request whose body is the
// credential, as the WebAuthn library reads it.
func splitCeremony(r *http.Request) (string, *http.Request, error) {
	var in struct {
		Ceremony   string          `json:"ceremony"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := readJSON(r, &in); err != nil {
		return "", nil, err
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(in.Credential))
	r2.ContentLength = int64(len(in.Credential))
	return in.Ceremony, r2, nil
}

func (s *Server) wa(w http.ResponseWriter) *webauthn.WebAuthn {
	wa, err := s.Auth.WebAuthn()
	if err != nil {
		fail(w, 500, "passkeys need the origin fixed (open the page on its final address, or serve --origin): "+err.Error())
		return nil
	}
	return wa
}

func (s *Server) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request, st *state) {
	wa := s.wa(w)
	if wa == nil {
		return
	}
	a, _ := s.Auth.Find(st.AdminID)
	opts, data, err := wa.BeginRegistration(auth.User(&a),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(webauthn.Credentials(a.Passkeys).CredentialDescriptors()))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ceremony": s.cer.put(data), "options": opts})
}

func (s *Server) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request, st *state) {
	wa := s.wa(w)
	if wa == nil {
		return
	}
	id, r2, err := splitCeremony(r)
	if err != nil {
		fail(w, 400, "bad request")
		return
	}
	data, ok := s.cer.take(id)
	if !ok {
		fail(w, 400, "the passkey request expired: try again")
		return
	}
	a, _ := s.Auth.Find(st.AdminID)
	cred, err := wa.FinishRegistration(auth.User(&a), data, r2)
	if err != nil {
		fail(w, 400, "the passkey was not accepted: "+protocolError(err))
		return
	}
	if err := s.Auth.AddPasskey(a.Name, *cred); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.Sessions.Enrolled(st.Token)
	s.audit(r, st.Session, "passkey.add", map[string]any{"admin": a.Name})
	writeJSON(w, map[string]any{"ok": true})
}

func protocolError(err error) string {
	if pe, ok := err.(*protocol.Error); ok && pe.DevInfo != "" {
		return pe.Details + " (" + pe.DevInfo + ")"
	}
	return err.Error()
}

func (s *Server) totpBegin(w http.ResponseWriter, r *http.Request, st *state) {
	secret, err := s.Auth.BeginTOTP(st.Name)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	u, _ := url.Parse(s.origin(r))
	writeJSON(w, map[string]any{"secret": secret, "uri": auth.TOTPURI("runsnip "+u.Hostname(), st.Name, secret)})
}

func (s *Server) totpConfirm(w http.ResponseWriter, r *http.Request, st *state) {
	var in struct{ Code string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if s.limited(w, "name:"+st.Name) {
		return
	}
	if err := s.Auth.ConfirmTOTP(st.Name, in.Code); err != nil {
		s.Limit.Fail("name:" + st.Name)
		fail(w, 400, err.Error())
		return
	}
	s.Sessions.Enrolled(st.Token)
	s.audit(r, st.Session, "totp.add", map[string]any{"admin": st.Name})
	writeJSON(w, map[string]any{"ok": true})
}

// ───────────────────────── sign in ─────────────────────────

func (s *Server) loginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		fail(w, http.StatusForbidden, "bad origin")
		return
	}
	wa := s.wa(w)
	if wa == nil {
		return
	}
	opts, data, err := wa.BeginDiscoverableLogin()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ceremony": s.cer.put(data), "options": opts})
}

func (s *Server) loginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limited(w, "ip:"+ip) || !s.checkOrigin(w, r) {
		return
	}
	wa := s.wa(w)
	if wa == nil {
		return
	}
	id, r2, err := splitCeremony(r)
	if err != nil {
		fail(w, 400, "bad request")
		return
	}
	data, ok := s.cer.take(id)
	if !ok {
		fail(w, 400, "the sign-in request expired: try again")
		return
	}
	var who auth.Admin
	cred, err := wa.FinishDiscoverableLogin(func(_, handle []byte) (webauthn.User, error) {
		a, ok := s.Auth.ByHandle(handle)
		if !ok {
			return nil, fmt.Errorf("unknown passkey")
		}
		who = a
		return auth.User(&who), nil
	}, data, r2)
	if err != nil {
		s.Limit.Fail("ip:" + ip)
		s.audit(r, nil, "login.failed", map[string]any{"method": "passkey"})
		fail(w, http.StatusUnauthorized, "the passkey was not accepted")
		return
	}
	_ = s.Auth.UpdatePasskey(who.ID, *cred)
	s.Limit.Reset("ip:" + ip)
	sess := s.startSession(w, r, who, false)
	s.audit(r, sess, "login", map[string]any{"method": "passkey"})
	writeJSON(w, map[string]any{"name": who.Name, "csrf": sess.CSRF})
}

func (s *Server) checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	if !s.sameOrigin(r) {
		fail(w, http.StatusForbidden, "bad origin")
		return false
	}
	return true
}

func (s *Server) loginTOTP(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	var in struct{ Name, Code string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if s.limited(w, "ip:"+ip, "name:"+in.Name) || !s.checkOrigin(w, r) {
		return
	}
	a, err := s.Auth.CheckTOTP(in.Name, in.Code)
	if err != nil {
		s.Limit.Fail("ip:"+ip, "name:"+in.Name)
		s.audit(r, nil, "login.failed", map[string]any{"method": "totp", "admin": in.Name})
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.Limit.Reset("ip:"+ip, "name:"+in.Name)
	sess := s.startSession(w, r, a, false)
	s.audit(r, sess, "login", map[string]any{"method": "totp"})
	writeJSON(w, map[string]any{"name": a.Name, "csrf": sess.CSRF})
}

// ───────────────────────── re-authentication ─────────────────────────

func (s *Server) reauthPasskeyBegin(w http.ResponseWriter, r *http.Request, st *state) {
	wa := s.wa(w)
	if wa == nil {
		return
	}
	a, _ := s.Auth.Find(st.AdminID)
	if len(a.Passkeys) == 0 {
		fail(w, 400, "no passkey: re-authenticate with a TOTP code")
		return
	}
	opts, data, err := wa.BeginLogin(auth.User(&a))
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ceremony": s.cer.put(data), "options": opts})
}

func (s *Server) reauthPasskeyFinish(w http.ResponseWriter, r *http.Request, st *state) {
	if s.limited(w, "name:"+st.Name) {
		return
	}
	wa := s.wa(w)
	if wa == nil {
		return
	}
	id, r2, err := splitCeremony(r)
	if err != nil {
		fail(w, 400, "bad request")
		return
	}
	data, ok := s.cer.take(id)
	if !ok {
		fail(w, 400, "expired: try again")
		return
	}
	a, _ := s.Auth.Find(st.AdminID)
	cred, err := wa.FinishLogin(auth.User(&a), data, r2)
	if err != nil {
		s.Limit.Fail("name:" + st.Name)
		fail(w, http.StatusUnauthorized, "the passkey was not accepted")
		return
	}
	_ = s.Auth.UpdatePasskey(a.ID, *cred)
	s.Sessions.Reauth(st.Token)
	s.audit(r, st.Session, "reauth", map[string]any{"method": "passkey"})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) reauthTOTP(w http.ResponseWriter, r *http.Request, st *state) {
	var in struct{ Code string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if s.limited(w, "name:"+st.Name) {
		return
	}
	if _, err := s.Auth.CheckTOTP(st.Name, in.Code); err != nil {
		s.Limit.Fail("name:" + st.Name)
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	s.Sessions.Reauth(st.Token)
	s.audit(r, st.Session, "reauth", map[string]any{"method": "totp"})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		fail(w, http.StatusForbidden, "bad origin")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if st := s.session(r); st != nil {
			// Signing someone out is a write too: only their own page may (an expired session has nothing to guard).
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(st.CSRF)) != 1 {
				fail(w, http.StatusForbidden, "bad CSRF token")
				return
			}
			s.audit(r, st.Session, "logout", nil)
		}
		s.Sessions.End(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]any{"ok": true})
}
