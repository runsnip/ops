package doctor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/keygen"
	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/sysinfo"
)

const composeYAML = `name: demo
x-runsnip-profiles:
  extra: EXTRA_TOKEN
x-logging: &logging
  driver: json-file
  options: { max-size: "10m", max-file: "3" }
services:
  caddy:
    image: caddy:2-alpine
    logging: *logging
    volumes: ["./Caddyfile:/etc/caddy/Caddyfile:ro"]
  app:
    image: ${IMAGE_PREFIX}-app:${APP_TAG:-latest}
    logging: *logging
    healthcheck: { test: ["CMD", "true"] }
  api:
    image: ${IMAGE_PREFIX}-api:${API_TAG:-latest}
    logging: *logging
  postgres:
    image: postgres:16-alpine
    logging: *logging
    healthcheck: { test: ["CMD", "pg_isready"] }
  worker:
    image: ${IMAGE_PREFIX}-worker:${WORKER_TAG:-latest}
    profiles: [extra]
    logging: *logging
`

func secret() string { return strings.Repeat("s", 40) }

func newStack(t *testing.T, stackYAML, env string) (*stack.Stack, *run.Fake) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"docker-compose.yml": composeYAML, "stack.yaml": stackYAML, ".env": env, ".env.example": "IMAGE_PREFIX=\nAPP_TAG=\nJWT_SECRET=\n",
		"Caddyfile": "{}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(dir, "Caddyfile"), old, old)
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &run.Fake{}
	st.Run = f
	return st, f
}

func newDoctor(st *stack.Stack) *Doctor {
	d := New(st)
	d.LookupIP = func(context.Context, string) ([]string, error) { return []string{"203.0.113.9"}, nil }
	d.PublicIP = func(context.Context) string { return "203.0.113.9" }
	d.Head = func(context.Context, string, string) (http.Header, error) { return http.Header{}, nil }
	d.Host = func() sysinfo.Snapshot { return sysinfo.Snapshot{CPUs: 2, MemTotal: 4 << 30, Supported: true} }
	return d
}

const ps = `{"Service":"caddy","Name":"caddy","Image":"caddy:2-alpine","State":"running","Status":"Up"}
{"Service":"app","Name":"app","Image":"ghcr.io/x/demo-app:1.2.0","State":"running","Health":"healthy","Status":"Up"}
{"Service":"api","Name":"api","Image":"ghcr.io/x/demo-api:0.9.0","State":"running","Status":"Up"}
{"Service":"postgres","Name":"postgres","Image":"postgres:16-alpine","State":"running","Health":"healthy","Status":"Up"}`

func text(r Report) string {
	var b strings.Builder
	for _, s := range r.Sections {
		b.WriteString("# " + s.Title + "\n")
		for _, it := range s.Items {
			b.WriteString(string(it.Level) + " " + it.Text + "\n")
		}
	}
	return b.String()
}

func standardRules(f *run.Fake) {
	f.On("ps --all --format json", ps)
	f.On("{{.RestartCount}}", "0\n")
	f.On("{{.Created}}", time.Now().Format(time.RFC3339Nano))
	f.On("inspect -f {{range .Config.Env}}{{println .}}{{end}} app", "DATABASE_URL=postgres://x\nPOOL_SIZE=8\n")
	f.On("inspect -f {{range .Config.Env}}{{println .}}{{end}} api", "DATABASE_URL=postgres://x\n")
	f.On("inspect -f", "PATH=/bin\n")
	f.On("SHOW max_connections", "20\n")
	f.On("docker info", "29.0.1\n")
	f.On("pg_stat_activity", "3\n")
}

func TestStandardSectionsRunAndAFailedProbeDoesNotStopTheDoctor(t *testing.T) {
	st, f := newStack(t, "standard: 1\npostgres: {pool_env: POOL_SIZE, url_env: DATABASE_URL}\n", "IMAGE_PREFIX=ghcr.io/x/demo\nAPP_TAG=1.2.0\nAPI_TAG=1.0.0\nJWT_SECRET=x\n")
	standardRules(f)
	r := newDoctor(st).Run(context.Background())
	out := text(r)
	for _, want := range []string{
		"# Configuration", "pass .env is readable by its owner only (600)", "pass .env has every variable",
		"# Versions", "pass app: 1.2.0", "fail api: .env pins 1.0.0, 0.9.0 runs — runsnip restart api",
		"# Containers", "note worker: off, EXTRA_TOKEN is not set", "fail worker", // not listed: off
		"pass 2 health checks healthy: app postgres",
		"# Running configuration", "pass caddy run the configuration on disk",
		"# Postgres: connection budget", "fail api connects to Postgres without POOL_SIZE", "pass pools 18 + reserve 5 = 23 ≤ max_connections 20",
	} {
		if want == "fail worker" {
			if strings.Contains(out, "fail worker") {
				t.Errorf("a service turned off by its profile is not a failure:\n%s", out)
			}
			continue
		}
		if want == "pass pools 18 + reserve 5 = 23 ≤ max_connections 20" {
			if !strings.Contains(out, "fail pools 18 + reserve 5 = 23 > max_connections 20") {
				t.Errorf("budget over max_connections must fail:\n%s", out)
			}
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if r.Failures != 3 {
		t.Errorf("failures %d:\n%s", r.Failures, out)
	}
}

func TestBudget(t *testing.T) {
	for _, c := range []struct {
		pools        map[string]int
		reserve, max int
		sum          int
		ok           bool
	}{
		{map[string]int{"a": 4, "b": 6}, 5, 15, 10, true},
		{map[string]int{"a": 4, "b": 6}, 5, 14, 10, false},
		{nil, 5, 5, 0, true},
	} {
		sum, ok := Budget(c.pools, c.reserve, c.max)
		if sum != c.sum || ok != c.ok {
			t.Errorf("%v %d %d: %d %v", c.pools, c.reserve, c.max, sum, ok)
		}
	}
}

func TestProjectChecksPassFailAndKeepGoing(t *testing.T) {
	jwt := "test-secret"
	billing, _ := keygen.Derive(jwt, "x/billing", 32)
	priv, pub, _ := keygen.Ed25519()
	stackYAML := `standard: 1
doctor:
  skip: ["Request shield", "Postgres: connection budget"]
  sections:
    - title: Keys
      checks:
        - { type: hkdf, key: BILLING, from: JWT_SECRET, info: x/billing }
        - { type: hkdf, key: WRONG, from: JWT_SECRET, info: x/billing, fail: WRONG does not match }
        - { type: key_pairs, keys: [SVC, BAD] }
        - { type: env_min_len, key: JWT_SECRET, min: 32, fail: too short }
        - { type: isolated_domains, base: "${APP}", others: ["${PREVIEW}", "${SHARE:-${PREVIEW}}"] }
        - { type: nonsense }
        - { type: env_set, keys: [MISSING_ONE], level: note, fail: "{missing} empty" }
    - title: Probes
      checks:
        - { type: http, service: caddy, url: "http://app:80/", expect: [405], codes: { 405: "route reached ({code})", 401: blocked }, fail: "answers {code}" }
        - { type: http, service: caddy, url: "http://api:80/", reject: [502], label: "caddy → api" }
        - type: exec
          service: app
          cmd: [sh, -c, health]
          cases:
            - { match: '"degraded":true', level: fail, say: "degraded {out}" }
            - { match: '"ok":true', level: pass, say: "ok {out}" }
        - { type: exec, service: api, cmd: [boom], pass: "{service} fine", fail: "{service} broken" }
        - { type: sql, query: "SELECT 1" }
        - { type: logs, service: app, pattern: "could not", tail: 200, present: false, fail: "{count} errors" }
    - title: Off
      if: { profile: extra }
      off: off — EXTRA_TOKEN is not set
      checks: [ { type: env_set, key: NEVER } ]
    - title: Not applying
      if: { running: nowhere }
      checks: [ { type: env_set, key: NEVER } ]
`
	env := "JWT_SECRET=" + jwt + "\nBILLING=" + billing + "\nWRONG=nope\nSVC_JWT_KEY=" + priv + "\nSVC_PUBLIC_KEY=" + pub +
		"\nBAD_JWT_KEY=" + priv + "\nBAD_PUBLIC_KEY=other\nAPP=app.example.com\nPREVIEW=preview.app.example.com\n"
	st, f := newStack(t, stackYAML, env)
	standardRules(f)
	f.Rules = append([]run.Rule{
		{Match: "exec -T caddy wget -S -q -O /dev/null --timeout=5 http://app:80/", Stdout: "  HTTP/1.1 405 Method Not Allowed\nwget: server returned error: HTTP/1.1 405 Method Not Allowed\n", Fail: true},
		{Match: "exec -T caddy wget -S -q -O /dev/null --timeout=5 http://api:80/", Stdout: "  HTTP/1.1 502 Bad Gateway\n", Fail: true},
		{Match: "exec -T app sh -c health", Stdout: `{"ok":true,"degraded":true}`},
		{Match: "exec -T api boom", Stderr: "no such file", Fail: true},
		{Match: "SELECT 1", Stdout: "pass|one is fine\nnote|and a note\ngarbage\n"},
		{Match: "logs --no-color --tail 200 app", Stdout: "a\ncould not ask\ncould not ask\n"},
	}, f.Rules...)
	r := newDoctor(st).Run(context.Background())
	out := text(r)
	for _, want := range []string{
		"pass BILLING matches JWT_SECRET", "fail WRONG does not match",
		"pass internal keys of svc match", "fail BAD_PUBLIC_KEY is not the public half of BAD_JWT_KEY",
		"fail too short", "fail share the domain of app.example.com: preview.app.example.com",
		`fail unknown check type "nonsense"`, "note MISSING_ONE empty",
		"pass route reached (405)", "fail caddy → api  502", `fail degraded {"ok":true,"degraded":true}`,
		"fail api broken", "pass one is fine", "note and a note", "fail 2 errors",
		"# Off\ninfo off — EXTRA_TOKEN is not set",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Not applying") || strings.Contains(out, "# Request shield") {
		t.Errorf("a section that does not apply, or is skipped, is left out:\n%s", out)
	}
	// The standard checks the project did not place still run, after its sections.
	if i, j := strings.Index(out, "# Keys"), strings.Index(out, "# Versions"); i < 0 || j < i {
		t.Errorf("standard sections come after the project's:\n%s", out)
	}
}

func TestAPanickingCheckIsReportedAndTheRestRuns(t *testing.T) {
	checks["boom"] = func(*Doctor, def.Check) { panic("kaput") }
	defer delete(checks, "boom")
	st, f := newStack(t, "standard: 1\ndoctor:\n  sections:\n    - title: T\n      checks: [ {type: boom}, {type: env_set, key: JWT_SECRET} ]\n", "JWT_SECRET=x\n")
	standardRules(f)
	out := text(newDoctor(st).Run(context.Background()))
	if !strings.Contains(out, "fail the boom check stopped: kaput") || !strings.Contains(out, "pass JWT_SECRET set") {
		t.Fatal(out)
	}
}

// With the switch on, the doctor asks the address Caddy forwards to, from inside Caddy's container: an answer of any
// kind passes; none fails with what it means for the sites. It never asks the shield's own tools.
func TestShieldRouteIsACaddyCheck(t *testing.T) {
	for _, c := range []struct {
		answer, want string
	}{
		{"  HTTP/1.1 403 Forbidden\n", "pass SHIELD=on: the shield endpoint caddy forwards to (172.17.0.1:9180/check) answers"},
		{"wget: can't connect to remote host (172.17.0.1): Connection refused\n", "fail SHIELD=on: caddy forwards every request to 172.17.0.1:9180/check first, and nothing answers there — every site returns 502"},
	} {
		st, f := newStack(t, "standard: 1\nshield: {addr_env: SHIELD_ADDR, addr: \"172.17.0.1:9180\"}\n", "JWT_SECRET=x\nSHIELD=on\n")
		f.Rules = append([]run.Rule{{Match: "exec -T caddy wget -S -q -O /dev/null -T 3 http://172.17.0.1:9180/check", Stdout: c.answer, Fail: true}}, f.Rules...)
		standardRules(f)
		out := text(newDoctor(st).Run(context.Background()))
		if !strings.Contains(out, c.want) {
			t.Errorf("missing %q in:\n%s", c.want, out)
		}
		for _, l := range f.Lines() {
			if strings.HasPrefix(l, "systemctl") {
				t.Fatalf("the doctor ran %q: it must not touch the shield itself", l)
			}
		}
	}
}

// A stack that declares no shield gets no shield section.
func TestNoShieldSectionWithoutAShield(t *testing.T) {
	st, _ := newStack(t, "standard: 1\n", "JWT_SECRET=x\n")
	for _, s := range StandardSections(st) {
		if s.Title == "Request shield" {
			t.Fatal("no shield declared, no shield section")
		}
	}
}
