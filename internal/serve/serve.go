// Package serve is `runsnip serve`: the web front end of a server — install, add stacks, edit .env, deploy, watch,
// update runsnip — for the people the server's admins are. It never does the work itself: every action runs the
// runsnip command line as a job (the same binary, the same arguments a console would type), whose log lives on disk
// and streams to the page. It has the highest privilege on the server, so:
//
//   - it listens on 127.0.0.1 unless --public (put Caddy in front, on a hostname you choose; the request shield is
//     optional there);
//   - nobody gets in without an admin's passkey or TOTP; the first admin comes with a one-time setup code that only
//     works until an admin exists;
//   - sessions are short, dangerous actions need a fresh authentication, guesses are rate-limited, an IP allowlist is
//     optional, every write is CSRF-checked, every answer has a strict CSP, and an append-only audit log keeps it all;
//   - nothing is sampled while no page is open.
package serve

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/auth"
	"github.com/runsnip/ops/internal/host"
)

//go:embed web
var webFS embed.FS

// Env is what serve needs from the command line.
type Env struct {
	State   string // runsnip's state folder
	Version string
	Exe     string // this program, to start jobs with
}

// Options are serve's flags.
type Options struct {
	Listen string
	Public bool
	Origin string       // the origin people open (passkeys are bound to its host); fixed at the first setup otherwise
	Allow  []*net.IPNet // optional allowlist
}

// Main parses the flags and serves until ctx ends.
func Main(ctx context.Context, env Env, args []string, out io.Writer) error {
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:7070", "address (loopback unless --public)")
	public := f.Bool("public", false, "allow a non-loopback address (admins still sign in; prefer Caddy in front)")
	origin := f.String("origin", "", "the https://host people open (default: fixed by the first setup)")
	allow := f.String("allow", "", "only these addresses or CIDR ranges, comma-separated (default: anyone who can reach it)")
	idle := f.Duration("idle", auth.IdleTimeout, "sign out after this long without a request (at most the default)")
	reauthAfter := f.Duration("reauth-after", auth.FreshFor, "dangerous actions ask to authenticate again after this long (at most the default)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *idle <= 0 || *idle > auth.IdleTimeout || *reauthAfter < 0 || *reauthAfter > auth.FreshFor {
		return fmt.Errorf("--idle and --reauth-after can only be shorter than %s and %s", auth.IdleTimeout, auth.FreshFor)
	}
	auth.IdleTimeout, auth.FreshFor = *idle, *reauthAfter
	o := Options{Listen: *listen, Public: *public, Origin: strings.TrimSuffix(*origin, "/")}
	for _, c := range strings.FieldsFunc(*allow, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !strings.Contains(c, "/") {
			if strings.Contains(c, ":") {
				c += "/128"
			} else {
				c += "/32"
			}
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("--allow %s: %v", c, err)
		}
		o.Allow = append(o.Allow, n)
	}
	if err := checkListen(o); err != nil {
		return err
	}
	if err := host.Ensure(env.State); err != nil {
		return err
	}
	srv := New(env, o)
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	_ = host.WriteJSON(filepath.Join(env.State, "serve.json"), map[string]string{"listen": ln.Addr().String()})
	fmt.Fprintf(out, "runsnip serve %s on http://%s (state %s)\n", env.Version, ln.Addr(), env.State)
	if srv.Auth.NeedsSetup() {
		fmt.Fprintln(out, "  no admin yet: sudo runsnip admin setup-code prints the one-time code to create the first one")
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(sh)
	}()
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func checkListen(o Options) error {
	h, _, err := net.SplitHostPort(o.Listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(h)
	if !(h == "localhost" || ip != nil && ip.IsLoopback()) && !o.Public {
		return fmt.Errorf("%s is not loopback: put Caddy in front on a hostname (reverse_proxy 127.0.0.1:7070), or add --public", o.Listen)
	}
	return nil
}

// Server is the web front end.
type Server struct {
	Env      Env
	Opt      Options
	Auth     *auth.Store
	Sessions *auth.Sessions
	Limit    *auth.Limiter
	mux      *http.ServeMux
	samplers *samplers
	cer      *ceremonies
	tickets  tickets
	Now      func() time.Time
}

// New builds the server.
func New(env Env, o Options) *Server {
	s := &Server{Env: env, Opt: o, Auth: auth.NewStore(env.State), Sessions: auth.LoadSessions(filepath.Join(env.State, "sessions.json")), Limit: auth.NewLimiter(5, 15*time.Minute), Now: time.Now}
	s.samplers = newSamplers(s)
	s.cer = newCeremonies()
	if o.Origin != "" && s.Auth.Config().Origin == "" {
		_ = s.Auth.SetOrigin(o.Origin)
	}
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", s.healthz)
	m.Handle("GET /{$}", static)
	m.Handle("GET /app.js", static)
	m.Handle("GET /app.css", static)
	m.Handle("GET /fonts/", static)
	m.Handle("GET /icons.svg", static)
	m.Handle("GET /graph.js", static)
	// Who is here, and getting in.
	m.HandleFunc("GET /api/status", s.status)
	m.HandleFunc("POST /api/setup", s.setup)
	m.HandleFunc("POST /api/enroll", s.enroll)
	m.HandleFunc("POST /api/passkey/register/begin", s.need(enrolling, s.passkeyRegisterBegin))
	m.HandleFunc("POST /api/passkey/register/finish", s.need(enrolling, s.passkeyRegisterFinish))
	m.HandleFunc("POST /api/totp/begin", s.need(enrolling, s.totpBegin))
	m.HandleFunc("POST /api/totp/confirm", s.need(enrolling, s.totpConfirm))
	m.HandleFunc("POST /api/login/passkey/begin", s.loginPasskeyBegin)
	m.HandleFunc("POST /api/login/passkey/finish", s.loginPasskeyFinish)
	m.HandleFunc("POST /api/login/totp", s.loginTOTP)
	m.HandleFunc("POST /api/reauth/passkey/begin", s.need(signedIn, s.reauthPasskeyBegin))
	m.HandleFunc("POST /api/reauth/passkey/finish", s.need(signedIn, s.reauthPasskeyFinish))
	m.HandleFunc("POST /api/reauth/totp", s.need(signedIn, s.reauthTOTP))
	m.HandleFunc("POST /api/logout", s.logout)
	// The server.
	m.HandleFunc("GET /api/stacks", s.need(signedIn, s.listStacks))
	m.HandleFunc("GET /api/stacks/{name}/events", s.need(signedIn, s.stackEvents))
	m.HandleFunc("GET /api/stacks/{name}/logs", s.need(signedIn, s.stackLogs))
	m.HandleFunc("GET /api/stacks/{name}/env", s.need(signedIn, s.envList))
	m.HandleFunc("POST /api/stacks/{name}/env/validate", s.need(signedIn, s.envValidate))
	m.HandleFunc("GET /api/stacks/{name}/graph", s.need(signedIn, s.stackGraph))
	m.HandleFunc("GET /api/stacks/{name}/layout", s.need(signedIn, s.getLayout))
	m.HandleFunc("PUT /api/stacks/{name}/layout", s.need(signedIn, s.putLayout))
	m.HandleFunc("DELETE /api/stacks/{name}/layout", s.need(signedIn, s.deleteLayout))
	m.HandleFunc("POST /api/stacks/{name}/env/reveal", s.need(fresh, s.envReveal))
	m.HandleFunc("GET /api/stacks/{name}/backups", s.need(signedIn, s.listBackups))
	m.HandleFunc("POST /api/stacks/{name}/backups/ticket", s.need(fresh, s.backupTicket))
	m.HandleFunc("GET /api/stacks/{name}/backups/download", s.need(signedIn, s.downloadBackup))
	m.HandleFunc("POST /api/paths/check", s.need(signedIn, s.checkPaths))
	m.HandleFunc("POST /api/jobs", s.need(signedIn, s.startJob))
	m.HandleFunc("GET /api/jobs", s.need(signedIn, s.listJobs))
	m.HandleFunc("GET /api/jobs/{id}", s.need(signedIn, s.getJob))
	m.HandleFunc("GET /api/jobs/{id}/log", s.need(signedIn, s.jobLog))
	m.HandleFunc("GET /api/admins", s.need(signedIn, s.listAdmins))
	m.HandleFunc("GET /api/audit", s.need(fresh, s.auditTail))
	s.mux = m
	return s
}

// The strictest policy the page works with: its own files, nothing inline, nothing from elsewhere, never framed.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
	}
	if r.URL.Path != "/healthz" && !s.allowed(r) {
		http.Error(w, "not allowed from this address", http.StatusForbidden)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// clientIP is the visitor: the connection's peer, or — when the peer is a proxy on this machine (Caddy) — the last
// address it appended to X-Forwarded-For (the only hop it vouches for).
func clientIP(r *http.Request) string {
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return h
}

func (s *Server) allowed(r *http.Request) bool {
	if len(s.Opt.Allow) == 0 {
		return true
	}
	ip := net.ParseIP(clientIP(r))
	for _, n := range s.Opt.Allow {
		if ip != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "version": s.Env.Version})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// audit records what an admin did (never a secret value).
func (s *Server) audit(r *http.Request, sess *auth.Session, event string, fields map[string]any) {
	rec := map[string]any{"event": event, "from": clientIP(r), "via": "web"}
	if sess != nil {
		rec["user"] = sess.Name
	}
	for k, v := range fields {
		rec[k] = v
	}
	_ = host.Audit(s.Env.State, rec)
}
