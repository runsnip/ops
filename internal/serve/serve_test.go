package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"

	"github.com/runsnip/ops/internal/auth"
	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
)

// TestMain doubles as the runsnip a job runs: with RUNSNIP_FAKE_CHILD set, it writes its arguments into the job's
// log and finishes the job (as the real one does), failing when an argument says "fail".
func TestMain(m *testing.M) {
	if os.Getenv("RUNSNIP_FAKE_CHILD") == "crash" {
		os.Exit(7)
	}
	if os.Getenv("RUNSNIP_FAKE_CHILD") == "1" {
		state, id := os.Getenv("RUNSNIP_STATE"), os.Getenv("RUNSNIP_JOB_ID")
		log, _ := jobs.OpenLog(state, id)
		in, _ := io.ReadAll(os.Stdin)
		fmt.Fprintf(log, "ran: %s\nstdin bytes: %d\nuser: %s\n", strings.Join(os.Args[1:], " "), len(in), os.Getenv("RUNSNIP_JOB_USER"))
		log.Close()
		code := 0
		if strings.Contains(strings.Join(os.Args, " "), "fail") {
			code = 3
		}
		// As the real one does: the child records its own outcome.
		if j, err := jobs.Get(state, id); err == nil {
			_ = jobs.Finish(state, j, code)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type client struct {
	t    *testing.T
	srv  *httptest.Server
	base string // http://localhost:<port> — a passkey's relying party is a domain, never an IP
	http *http.Client
	csrf string
}

func newServer(t *testing.T, o Options) (*Server, *client) {
	t.Helper()
	state := t.TempDir()
	host.Ensure(state)
	exe, _ := os.Executable()
	t.Setenv("RUNSNIP_FAKE_CHILD", "1")
	s := New(Env{State: state, Version: "v1.2.3", Exe: exe}, o)
	ts := httptest.NewServer(s)
	t.Cleanup(func() { os.Unsetenv("RUNSNIP_FAKE_CHILD"); ts.Close() })
	os.Setenv("RUNSNIP_FAKE_CHILD", "1")
	jar, _ := cookiejar.New(nil)
	_, port, _ := net.SplitHostPort(ts.Listener.Addr().String())
	addr := ts.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	return s, &client{t: t, srv: ts, base: "http://localhost:" + port, http: &http.Client{Jar: jar, Transport: tr}}
}

func (c *client) do(method, path string, body any, headers ...string) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Origin", c.base)
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{"raw": string(b)}
	}
	return resp.StatusCode, out
}

func (c *client) rp() virtualwebauthn.RelyingParty {
	return virtualwebauthn.RelyingParty{Name: "runsnip", ID: "localhost", Origin: c.base}
}

func optionsJSON(m map[string]any) string { b, _ := json.Marshal(m["options"]); return string(b) }

// The whole way in with a passkey: setup code → first admin → passkey → signed out → passkey sign-in → a dangerous
// action asks for re-authentication → it passes after one.
func TestSetupPasskeyLoginReauthAndAJob(t *testing.T) {
	s, c := newServer(t, Options{})
	if code, st := c.do("GET", "/api/status", nil); code != 200 || st["needsSetup"] != true {
		t.Fatalf("%d %v", code, st)
	}
	setupCode, _ := s.Auth.SetupCode(30 * time.Minute)
	if code, _ := c.do("POST", "/api/setup", map[string]string{"code": "WRONG-WRONG", "name": "ops"}); code != 401 {
		t.Fatal("a wrong setup code")
	}
	code, out := c.do("POST", "/api/setup", map[string]string{"code": setupCode, "name": "ops"})
	if code != 200 {
		t.Fatalf("setup: %d %v", code, out)
	}
	c.csrf = out["csrf"].(string)
	// Enrolling: nothing but registering a credential.
	if code, _ := c.do("GET", "/api/stacks", nil); code != 403 {
		t.Fatalf("an enrolling session sees nothing: %d", code)
	}
	authn := virtualwebauthn.NewAuthenticator()
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	code, out = c.do("POST", "/api/passkey/register/begin", nil)
	if code != 200 {
		t.Fatalf("register begin: %d %v", code, out)
	}
	att, err := virtualwebauthn.ParseAttestationOptions(optionsJSON(out))
	if err != nil {
		t.Fatal(err)
	}
	resp := virtualwebauthn.CreateAttestationResponse(c.rp(), authn, cred, *att)
	code, fin := c.do("POST", "/api/passkey/register/finish", map[string]any{"ceremony": out["ceremony"], "credential": json.RawMessage(resp)})
	if code != 200 {
		t.Fatalf("register finish: %d %v", code, fin)
	}
	if code, _ := c.do("GET", "/api/stacks", nil); code != 200 {
		t.Fatal("signed in after registering")
	}
	if code, _ := c.do("POST", "/api/setup", map[string]string{"code": setupCode, "name": "x"}); code != 401 {
		t.Fatal("no setup once an admin exists")
	}
	c.do("POST", "/api/logout", nil)
	c.csrf = ""

	// Sign in with the passkey (discoverable: the authenticator returns the user handle).
	a, _ := s.Auth.Find("ops")
	handle, _ := base64.RawURLEncoding.DecodeString(a.ID)
	authn.Options.UserHandle = handle
	authn.AddCredential(cred)
	code, out = c.do("POST", "/api/login/passkey/begin", nil)
	asn, err := virtualwebauthn.ParseAssertionOptions(optionsJSON(out))
	if code != 200 || err != nil {
		t.Fatalf("login begin: %d %v", code, err)
	}
	resp = virtualwebauthn.CreateAssertionResponse(c.rp(), authn, cred, *asn)
	code, out = c.do("POST", "/api/login/passkey/finish", map[string]any{"ceremony": out["ceremony"], "credential": json.RawMessage(resp)})
	if code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	c.csrf = out["csrf"].(string)

	// A dangerous job, not fresh: asks for re-authentication.
	s.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	s.Sessions.Now = s.Now
	if code, _ := c.do("POST", "/api/jobs", map[string]any{"command": []string{"self", "update"}}); code != http.StatusPreconditionRequired {
		t.Fatalf("not fresh: %d", code)
	}
	code, out = c.do("POST", "/api/reauth/passkey/begin", nil)
	asn, _ = virtualwebauthn.ParseAssertionOptions(optionsJSON(out))
	resp = virtualwebauthn.CreateAssertionResponse(c.rp(), authn, cred, *asn)
	if code, out := c.do("POST", "/api/reauth/passkey/finish", map[string]any{"ceremony": out["ceremony"], "credential": json.RawMessage(resp)}); code != 200 {
		t.Fatalf("reauth: %d %v", code, out)
	}
	code, out = c.do("POST", "/api/jobs", map[string]any{"command": []string{"self", "update", "--version", "v9.9.9"}, "stdin": "secret"})
	if code != 200 {
		t.Fatalf("job: %d %v", code, out)
	}
	id := out["id"].(string)
	var j jobs.Job
	for i := 0; i < 50; i++ {
		j, _ = jobs.Get(s.Env.State, id)
		if j.Status != "running" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	logb, _ := os.ReadFile(jobs.LogPath(s.Env.State, id))
	if j.Status != "ok" || !strings.Contains(string(logb), "ran: self update --version v9.9.9") || !strings.Contains(string(logb), "stdin bytes: 6") || !strings.Contains(string(logb), "user: ops") {
		t.Fatalf("the CLI ran the job: %+v\n%s", j, logb)
	}
	// Its log streams, and ends with the outcome.
	req, _ := http.NewRequest("GET", c.base+"/api/jobs/"+id+"/log", nil)
	rs, _ := c.http.Do(req)
	b, _ := io.ReadAll(rs.Body)
	if !strings.Contains(string(b), "ran: self update") || !strings.Contains(string(b), "event: end") {
		t.Fatalf("log stream: %s", b)
	}
	audit, _ := os.ReadFile(s.Env.State + "/audit.log")
	for _, e := range []string{`"event":"setup"`, `"event":"passkey.add"`, `"event":"login"`, `"event":"reauth"`, `"event":"job.start"`} {
		if !strings.Contains(string(audit), e) {
			t.Errorf("audit lacks %s", e)
		}
	}
	if strings.Contains(string(audit), "secret") {
		t.Fatal("stdin never reaches the audit log")
	}
}

func TestTOTPEnrolLoginLimitsCSRFAndAllowlist(t *testing.T) {
	s, c := newServer(t, Options{})
	sc, _ := s.Auth.SetupCode(time.Hour)
	_, out := c.do("POST", "/api/setup", map[string]string{"code": sc, "name": "ops"})
	c.csrf = out["csrf"].(string)
	_, tb := c.do("POST", "/api/totp/begin", nil)
	secret := tb["secret"].(string)
	if !strings.HasPrefix(tb["uri"].(string), "otpauth://totp/") {
		t.Fatal(tb)
	}
	step := uint64(time.Now().Unix() / 30)
	if code, _ := c.do("POST", "/api/totp/confirm", map[string]string{"code": auth.TOTPAt(secret, step)}); code != 200 {
		t.Fatal("confirm")
	}
	// CSRF: a write without the token, or from another origin, is refused — signing out included.
	if code, _ := c.do("POST", "/api/logout", nil, "X-CSRF-Token", "nope"); code != 403 {
		t.Fatalf("logout CSRF: %d", code)
	}
	if code, _ := c.do("POST", "/api/jobs", map[string]any{"command": []string{"self", "check"}}, "X-CSRF-Token", "bad"); code != 403 {
		t.Fatalf("CSRF: %d", code)
	}
	if code, _ := c.do("POST", "/api/jobs", map[string]any{"command": []string{"self", "check"}}, "Origin", "https://evil.example"); code != 403 {
		t.Fatalf("origin: %d", code)
	}
	if code, _ := c.do("POST", "/api/jobs", map[string]any{"command": []string{"rm", "-rf", "/"}}); code != 400 {
		t.Fatal("only the CLI's own commands")
	}
	c.do("POST", "/api/logout", nil)
	c.csrf = ""
	// Guessing TOTP is limited.
	for i := 0; i < 5; i++ {
		c.do("POST", "/api/login/totp", map[string]string{"name": "ops", "code": "000000"})
	}
	if code, _ := c.do("POST", "/api/login/totp", map[string]string{"name": "ops", "code": auth.TOTPAt(secret, step+1)}); code != 429 {
		t.Fatalf("rate limited: %d", code)
	}
	// The allowlist: only the ranges given.
	_, n, _ := net.ParseCIDR("10.9.0.0/16")
	s2, c2 := newServer(t, Options{Allow: []*net.IPNet{n}})
	_ = s2
	if code, _ := c2.do("GET", "/api/status", nil); code != 403 {
		t.Fatal("not on the allowlist")
	}
	if code, _ := c2.do("GET", "/healthz", nil); code != 200 {
		t.Fatal("/healthz answers anyone (the update watchdog asks it)")
	}
}

func TestAllowedCommands(t *testing.T) {
	for cmd, ok := range map[string]bool{
		"start": true, "start api": true, "pin api 1.2.3": true, "pin api": false, "restart api --no-pull": true,
		"env set --stdin --restart": true, "env set --stdin --off SMTP_URL --on X": true, "env set A=1": false, "env generate JWT_SECRET X --force": true,
		"stack add demo --repo git@github.com:x/y.git --branch infra --dir /opt/demo --data /srv/demo": true,
		"stack add demo --repo x --evil y": false, "self update --version v1.2.3": true, "admin add bob": true,
		"exec api sh": false, "psql": false, "pin api 1.2.3;rm": false, "doctor": true,
	} {
		_, err := Allowed(strings.Fields(cmd))
		if (err == nil) != ok {
			t.Errorf("%q: %v", cmd, err)
		}
	}
	if r, _ := Allowed([]string{"doctor"}); r.dangerous {
		t.Fatal("doctor is not dangerous")
	}
	if r, _ := Allowed([]string{"env", "combine", "--apply"}); !r.dangerous {
		t.Fatal("writing .env is")
	}
}

func TestHealthzAndHeaders(t *testing.T) {
	_, c := newServer(t, Options{})
	resp, _ := http.Get(c.base + "/healthz")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"version":"v1.2.3"`) {
		t.Fatal(string(b))
	}
	resp, _ = http.Get(c.base + "/")
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("headers")
	}
	if err := checkListen(Options{Listen: "0.0.0.0:7070"}); err == nil {
		t.Fatal("public needs --public")
	}
}

// A job whose child dies without recording its end still gets its exit code (the server waits for it).
func TestJobChildCrashKeepsExitCode(t *testing.T) {
	s, _ := newServer(t, Options{})
	t.Setenv("RUNSNIP_FAKE_CHILD", "crash")
	j, err := s.spawn("", []string{"self", "check"}, "", &auth.Session{Name: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if j, _ = jobs.Get(s.Env.State, j.ID); j.Ended != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if j.Status != "failed" || j.Exit == nil || *j.Exit != 7 {
		t.Fatalf("%+v", j)
	}
}

// Sessions survive a restart of serve (an update must not sign everyone out), an enrolling one stays enrolling, and
// the file on disk never holds a cookie.
func TestSessionsSurviveARestart(t *testing.T) {
	s, c := newServer(t, Options{})
	sc, _ := s.Auth.SetupCode(time.Hour)
	_, out := c.do("POST", "/api/setup", map[string]string{"code": sc, "name": "ops"})
	c.csrf = out["csrf"].(string)
	restart := func() {
		s2 := New(s.Env, Options{})
		ts := httptest.NewServer(s2)
		t.Cleanup(ts.Close)
		addr := ts.Listener.Addr().String()
		_, port, _ := net.SplitHostPort(addr)
		c.srv, c.base = ts, "http://localhost:"+port
		c.http.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
		s = s2
	}
	restart()
	if code, _ := c.do("GET", "/api/stacks", nil); code != 403 {
		t.Fatalf("still enrolling after a restart: %d", code)
	}
	_, tb := c.do("POST", "/api/totp/begin", nil)
	if code, _ := c.do("POST", "/api/totp/confirm", map[string]string{"code": auth.TOTPAt(tb["secret"].(string), uint64(time.Now().Unix()/30))}); code != 200 {
		t.Fatal("confirm")
	}
	restart()
	if code, _ := c.do("GET", "/api/stacks", nil); code != 200 {
		t.Fatalf("signed in after a restart: %d", code)
	}
	b, _ := os.ReadFile(s.Env.State + "/sessions.json")
	for _, ck := range c.http.Jar.Cookies(mustURL(c.base)) {
		if strings.Contains(string(b), ck.Value) {
			t.Fatal("the cookie is on disk")
		}
	}
	if fi, _ := os.Stat(s.Env.State + "/sessions.json"); fi.Mode().Perm() != 0o600 {
		t.Fatal(fi.Mode())
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
