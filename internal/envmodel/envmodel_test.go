package envmodel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/stack"
)

const example = `# ---- the app ----------------------------------------------------------------
# The app's address.
# Two lines of help.
APP_URL=https://example.com

# A help block ends at a blank line, so this one is not JWT_SECRET's.

JWT_SECRET=
#   LOG_FORMAT=PRETTY  LOG_COLOR=ansi   (prose, not a variable)
# Optional: the SMTP relay.
# SMTP_URL=smtp://user:pass@host:587
API_TAG=latest
PORT=8080
`

const compose = `name: demo
x-app-env: &app-env
  JWT_SECRET: ${JWT_SECRET}
services:
  api:
    image: ${P:-x}/demo-api:${API_TAG:-latest}
    environment:
      <<: *app-env
      APP_URL: ${APP_URL}
  worker:
    image: busybox
    env_file: [.env]
  web:
    image: nginx
    environment: { PORT: "${PORT:-80}" }
`

func build(t *testing.T, env string, rules string) *Model {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env.example"), []byte(example), 0o644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(compose), 0o644)
	os.WriteFile(filepath.Join(dir, "stack.yaml"), []byte("standard: 1\n"+rules), 0o644)
	if env != "" {
		os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600)
	}
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Build(st)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func get(t *testing.T, m *Model, k string) Var {
	t.Helper()
	v, ok := m.Find(k)
	if !ok {
		t.Fatalf("no %s", k)
	}
	return v
}

// The semantics another tool's .env editor set: an active line beats a commented one, the last assignment wins, a blank line ends a help
// block, prose that looks like assignments is not a variable.
func TestDotenvSemantics(t *testing.T) {
	m := build(t, "APP_URL=https://one\n# APP_URL=https://commented\nAPP_URL=https://last\n# JWT_SECRET=kept-off\nMY_OWN=1\n", "")
	if v := get(t, m, "APP_URL"); v.Value != "https://last" || !v.Enabled || v.Help != "The app's address.\nTwo lines of help." || v.Section != "the app" {
		t.Fatalf("APP_URL: %+v", v)
	}
	if v := get(t, m, "JWT_SECRET"); v.Enabled || v.Value != "kept-off" || !v.InEnv || v.Help != "" || !v.Secret {
		t.Fatalf("JWT_SECRET (off, its value kept; the block above it ended at a blank line; a secret): %+v", v)
	}
	if _, ok := m.Find("LOG_FORMAT"); ok {
		t.Fatal("prose `#   A=1 B=2` is not a variable")
	}
	if v := get(t, m, "SMTP_URL"); !v.Optional || v.Enabled || !v.Secret || !strings.HasSuffix(v.Help, "Optional: the SMTP relay.") {
		t.Fatalf("SMTP_URL: %+v", v)
	}
	if v := get(t, m, "MY_OWN"); !v.Extra || v.Help != "" {
		t.Fatalf("only in .env: %+v", v)
	}
	if v := get(t, m, "PORT"); !v.New || v.Value != "" || v.Example != "8080" {
		t.Fatalf("new: the example's value is offered, not slipped in: %+v", v)
	}
	if v := get(t, m, "API_TAG"); v.Written != "runsnip" || !strings.Contains(v.Help, "runsnip pin api") {
		t.Fatalf("a pin runsnip writes has its own help: %+v", v)
	}
}

func TestServicesReadingAVariable(t *testing.T) {
	m := build(t, "APP_URL=x\n", "")
	for k, want := range map[string]string{
		"JWT_SECRET": "api worker", // through the merged anchor, and the env_file
		"APP_URL":    "api worker",
		"PORT":       "worker web",
		"API_TAG":    "api worker",
	} {
		if got := strings.Join(get(t, m, k).Services, " "); got != want {
			t.Errorf("%s read by %q, want %q", k, got, want)
		}
	}
}

func TestValidationAndGenerators(t *testing.T) {
	rules := `env_rules:
  generate:
    - { type: random_hex, key: JWT_SECRET, bytes: 32 }
  vars:
    APP_URL: { type: url, required: true }
    PORT: { type: port }
    JWT_SECRET: { type: hex, bytes: 32 }
`
	m := build(t, "APP_URL=not a url\nPORT=99999\nJWT_SECRET=abcd\n", rules)
	if p := get(t, m, "APP_URL").Problem; p == "" {
		t.Fatal("a bad URL is a problem")
	}
	if p := get(t, m, "PORT").Problem; !strings.Contains(p, "port") {
		t.Fatalf("port: %q", p)
	}
	if v := get(t, m, "JWT_SECRET"); !strings.Contains(v.Problem, "needs 32") || v.Generate != "random_hex" {
		t.Fatalf("secret: %+v", v)
	}
	if got := len(m.Filter("problems", "")); got != 3 {
		t.Fatalf("problems filter: %d", got)
	}
	for _, c := range []struct {
		r  def.VarRule
		v  string
		ok bool
	}{
		{def.VarRule{Type: "email"}, "a@b.co", true}, {def.VarRule{Type: "email"}, "a@", false},
		{def.VarRule{Type: "domain"}, "app.example.com", true}, {def.VarRule{Type: "domain"}, "http://x", false},
		{def.VarRule{Type: "base64", Bytes: 32}, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=", true},
		{def.VarRule{Type: "base64", Bytes: 32}, "AAEC", false},
		{def.VarRule{Type: "cidrs"}, "10.0.0.0/8 192.0.2.1", true}, {def.VarRule{Type: "cidrs"}, "nope", false},
		{def.VarRule{Enum: []string{"a", "b"}}, "c", false}, {def.VarRule{Pattern: "[a-z]+"}, "abc", true},
		{def.VarRule{Required: true}, "", false}, {def.VarRule{Type: "port"}, "", true},
	} {
		if err := Check(&c.r, c.v); (err == nil) != c.ok {
			t.Errorf("%+v %q: %v", c.r, c.v, err)
		}
	}
	if len(m.Filter("secrets", "")) != 2 || len(m.Filter("", "relay")) != 1 {
		t.Fatal("filters")
	}
}
