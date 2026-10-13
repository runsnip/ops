package standard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/shim"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, body := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A project with its own naming everywhere. adopt drafts the overrides, and the draft validates.
func TestAdoptDraftsOverridesAndTheDraftValidates(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	write(t, root, map[string]string{
		"infra/vps/compose.yml": `name: demo
x-logging: &logging
  driver: json-file
  options: { max-size: "20m", max-file: "5" }
services:
  postgres:
    image: pgvector/pgvector:pg16
    logging: *logging
    environment: { POSTGRES_USER: demo, POSTGRES_DB: demo }
    healthcheck: { test: ["CMD-SHELL", "pg_isready -U demo"] }
  migrate:
    image: ${REG:-ghcr.io/demo}/demo-api:${DEMO_API_TAG:-${DEMO_TAG:-latest}}
    logging: *logging
  api:
    image: ${REG:-ghcr.io/demo}/demo-api:${DEMO_API_TAG:-${DEMO_TAG:-latest}}
    logging: *logging
    environment: { PORT: "4000" }
    volumes: ["${DEMO_DATA_DIR:-/srv/demo}/files:/files"]
    healthcheck: { test: ["CMD", "node", "-e", "fetch('http://127.0.0.1:4000/healthz')"] }
  web:
    image: ${REG:-ghcr.io/demo}/demo-web:${DEMO_WEB_TAG:-${DEMO_TAG:-latest}}
    logging: *logging
  caddy:
    image: caddy:2
    logging: *logging
    volumes: ["./Caddyfile:/etc/caddy/Caddyfile:ro"]
`,
		"infra/vps/env.example":     "# Secrets: openssl rand -hex 32.\nAUTH_SECRET=CHANGE_ME\n# Encrypts tokens. base64 of 32 bytes.\nENCRYPTION_KEY=CHANGE_ME\nDEMO_TAG=latest\nADMIN_ALLOW=\n",
		"infra/vps/Caddyfile":       "(shield_on) {\n\tforward_auth {$SHIELD_ADDR:10.0.0.1:9000} {\n\t\turi /check\n\t}\n}\n(shield_off) {\n}\nexample.com {\n\timport shield_{$SHIELD:on}\n}\n",
		"scripts/release.sh":        "echo \"▶ $c\"\n",
		"scripts/publish-infra.sh":  "BRANCH=infra\n",
		"services/api/package.json": `{"version":"0.1.0"}`,
		"apps/web/package.json":     `{"version":"0.2.0"}`,
	})
	a, err := Adopt(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"env_example: env.example", `data_dir: "${DEMO_DATA_DIR:-/srv/demo}"`, "api: { health: { path: /healthz } }",
		"postgres:\n  service: postgres\n  user: demo", "placeholder: CHANGE_ME", "{ type: random_hex, key: AUTH_SECRET, bytes: 32 }",
		"{ type: random_base64, key: ENCRYPTION_KEY, bytes: 32 }", "shield:\n  switch_default: \"on\"\n  addr_env: SHIELD_ADDR\n  addr: \"10.0.0.1:9000\"", "build: [bash, scripts/release.sh]",
		"components: [api, web]", `progress: { building: '^▶ ([a-z0-9.-]+)\s' }`, `{ match: web, file: "apps/web/package.json" }`,
		`{ match: api, file: "services/api/package.json" }`, "{ type: registry }",
	} {
		if !strings.Contains(a.Stack, want) {
			t.Errorf("draft lacks %q:\n%s", want, a.Stack)
		}
	}
	for _, want := range []string{"components without a compose healthcheck: web", "api shares its image with migrate", "no standard runsnip shim"} {
		if !strings.Contains(a.Report, want) {
			t.Errorf("report lacks %q:\n%s", want, a.Report)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "infra/vps/runsnip.draft")); err == nil {
		t.Fatal("adopt without write leaves the project untouched")
	}
	// The draft is a valid stack file.
	write(t, root, map[string]string{"infra/vps/stack.yaml": a.Stack})
	os.WriteFile(filepath.Join(root, "infra/vps/runsnip"), shim.Script, 0o755)
	r := Validate(filepath.Join(root, "infra/vps"))
	for _, f := range r.Findings {
		if f.Level == "error" {
			t.Errorf("draft does not validate: %s", f.Text)
		}
	}
	var gaps []string
	for _, f := range r.Findings {
		if f.Level == "gap" {
			gaps = append(gaps, f.Rule+": "+f.Text)
		}
	}
	if g := strings.Join(gaps, "\n"); !strings.Contains(g, "health-check: components without a compose healthcheck: web") {
		t.Fatalf("gaps:\n%s", g)
	}
}

func TestValidateReportsSchemaErrorsWithThePath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"stack.yaml":         "standard: 1\ndoctor:\n  sections:\n    - title: X\n      checks: [ { type: nope, levl: fail } ]\n",
		"docker-compose.yml": "name: x\nservices: {}\n",
	})
	r := Validate(dir)
	var errs []string
	for _, f := range r.Findings {
		if f.Level == "error" {
			errs = append(errs, f.Text)
		}
	}
	e := strings.Join(errs, "\n")
	if !strings.Contains(e, "levl") || !strings.Contains(e, "/doctor/sections/0/checks/0/type") || r.Errors() == 0 {
		t.Fatalf("errors:\n%s", e)
	}
}

func TestEveryCheckTypeAndRuleIsDocumented(t *testing.T) {
	b, err := os.ReadFile("../../STANDARD.md")
	if err != nil {
		t.Skip("STANDARD.md not written yet")
	}
	for _, r := range Rules {
		if !strings.Contains(string(b), "`"+r.ID+"`") {
			t.Errorf("STANDARD.md does not name rule %s", r.ID)
		}
	}
}
