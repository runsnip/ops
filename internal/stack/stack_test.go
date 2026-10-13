package stack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runsnip/ops/internal/compose"
	"github.com/runsnip/ops/internal/run"
)

const composeYAML = `name: demo
x-runsnip-profiles:
  extra: EXTRA_TOKEN
services:
  caddy:
    image: caddy:2-alpine
    volumes: ["./Caddyfile:/etc/caddy/Caddyfile:ro", "${DATA_DIR:-/srv/demo}/caddy:/data"]
  nginx:
    image: nginx:1.27-alpine
  app:
    image: ${REGISTRY}-app:${APP_TAG:-latest}
  migrate:
    image: ${REGISTRY:-ghcr.io/x}/demo-api:${API_TAG:-${DEFAULT_TAG:-latest}}
  api:
    image: ${REGISTRY:-ghcr.io/x}/demo-api:${API_TAG:-${DEFAULT_TAG:-latest}}
    healthcheck: { test: ["CMD", "true"] }
  image-worker:
    image: ${REGISTRY}-service-image-worker:${SERVICE_IMAGE_WORKER_TAG:-latest}
  worker:
    image: ${REGISTRY}-worker:${WORKER_TAG:-latest}
    profiles: [extra]
  postgres:
    image: postgres:16-alpine
`

func newStack(t *testing.T, stackYAML, env string) (*Stack, *run.Fake) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(composeYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "stack.yaml"), []byte(stackYAML), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &run.Fake{}
	f.On("", "")
	s.Run = f
	s.Getenv = func(string) string { return "" }
	return s, f
}

// compose lines without the fixed prefix (--project-directory, -f, --env-file).
func composeCalls(f *run.Fake) []string {
	var out []string
	for _, l := range f.Lines() {
		if i := strings.Index(l, "/.env "); i >= 0 && strings.HasPrefix(l, "docker compose") {
			out = append(out, l[i+6:])
		}
	}
	return out
}

func TestComponentsComeFromTheComposeFile(t *testing.T) {
	s, _ := newStack(t, "standard: 1\n", "REGISTRY=ghcr.io/x/demo\nAPI_TAG=\nDEFAULT_TAG=0.3.0\n")
	var names []string
	for _, c := range s.Components() {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, " "); got != "app api image-worker worker" {
		t.Fatalf("components: %s", got)
	}
	api, _ := s.Component("api")
	if api.Image.TagVar != "API_TAG" || strings.Join(api.Before, ",") != "migrate" {
		t.Fatalf("one image, two services: the one named like it is the component, the other a migration first: %+v", api)
	}
	if s.Tag(api, s.Values()) != "0.3.0" {
		t.Fatalf("an empty pin falls back to the compose default: %s", s.Tag(api, s.Values()))
	}
	if c, ok := s.Component("service-image-worker"); !ok || c.Name != "image-worker" {
		t.Fatal("found by its image name too")
	}
	if s.DefaultComponent() != "app" {
		t.Fatal("the default component is the first")
	}
	if strings.Join(s.Proxies(), " ") != "caddy nginx" {
		t.Fatalf("proxies: %v", s.Proxies())
	}
	if ok, why := s.Enabled("worker", s.Values()); ok || why != "off, EXTRA_TOKEN is not set" {
		t.Fatalf("worker: %v %q", ok, why)
	}
	if p, ok := s.Postgres(); !ok || p.Service != "postgres" || p.User != "postgres" || p.DB != "postgres" {
		t.Fatalf("postgres: %+v", p)
	}
	if s.DataDir() != "/srv/demo" || s.BackupDir() != "/srv/demo/backups" {
		t.Fatalf("data %s backups %s", s.DataDir(), s.BackupDir())
	}
}

func TestParseImage(t *testing.T) {
	for in, want := range map[string]string{
		"${REGISTRY}-service-orders:${SERVICE_ORDERS_TAG:-latest}":                     "${REGISTRY}-service-orders|SERVICE_ORDERS_TAG|latest|service-orders",
		"${SHOP_REGISTRY:-ghcr.io/shop}/shop-api:${SHOP_API_TAG:-${SHOP_TAG:-latest}}": "${SHOP_REGISTRY:-ghcr.io/shop}/shop-api|SHOP_API_TAG|${SHOP_TAG:-latest}|shop-api",
		"ghcr.io/x/y:${Y_TAG}": "ghcr.io/x/y|Y_TAG||y",
	} {
		i, err := compose.ParseImage(in)
		if err != nil {
			t.Fatal(in, err)
		}
		if got := i.Repo + "|" + i.TagVar + "|" + i.TagDefault + "|" + i.ShortName(); got != want {
			t.Errorf("%s → %s", in, got)
		}
	}
	for _, in := range []string{"postgres:16-alpine", "caddy", "x:${A}-suffix"} {
		if _, err := compose.ParseImage(in); err == nil {
			t.Errorf("%s is not pinned by a variable", in)
		}
	}
}

func TestPinKeepsThePreviousVersionAndRecreatesTheComponentAlone(t *testing.T) {
	s, f := newStack(t, "standard: 1\n", "# pins\nAPI_TAG=0.1.0\n")
	var out bytes.Buffer
	if err := s.Pin(context.Background(), &out, "api", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	env, _ := s.Env()
	if env.Get("API_TAG") != "0.2.0" || env.Get("API_TAG_PREVIOUS") != "0.1.0" {
		t.Fatalf("env: %v", env.Values())
	}
	got := strings.Join(composeCalls(f), "\n")
	want := "pull migrate api\nup -d --no-deps --force-recreate migrate\nup -d --no-deps --force-recreate api\nps api"
	if got != want {
		t.Fatalf("calls:\n%s\nwant:\n%s", got, want)
	}
	f.Calls = nil
	if err := s.Rollback(context.Background(), &out, "api"); err != nil {
		t.Fatal(err)
	}
	env, _ = s.Env()
	if env.Get("API_TAG") != "0.1.0" || env.Get("API_TAG_PREVIOUS") != "0.2.0" {
		t.Fatalf("rollback: %v", env.Values())
	}
	if err := s.Pin(context.Background(), &out, "0.3.0", "api"); err == nil || !strings.Contains(err.Error(), "the order is") {
		t.Fatalf("swapped arguments: %v", err)
	}
	if err := s.Pin(context.Background(), &out, "worker", "1.0.0"); err == nil || !strings.Contains(err.Error(), "EXTRA_TOKEN is not set") {
		t.Fatalf("a component turned off is refused, saying how to turn it on: %v", err)
	}
}

func TestPreviousFromTheOldFileMode(t *testing.T) {
	s, _ := newStack(t, "standard: 1\n", "APP_TAG=2.0.0\n")
	os.WriteFile(filepath.Join(s.Dir, ".last-tag-app"), []byte("1.9.0\n"), 0o600)
	vs, err := s.Versions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if vs[0].Component != "app" || vs[0].Previous != "1.9.0" {
		t.Fatalf("a .last-tag file from the bash tool is still read: %+v", vs[0])
	}
}

func TestRestartNoPullAndStartOrder(t *testing.T) {
	s, f := newStack(t, "standard: 1\nstart:\n  git_pull: false\n  pull: false\n  prune: false\n  order: [[postgres], [app, worker]]\n", "REGISTRY=x\n")
	var out bytes.Buffer
	if err := s.Restart(context.Background(), &out, "app", true); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(composeCalls(f), "\n"); got != "up -d --no-deps --force-recreate app\nps app" {
		t.Fatalf("restart --no-pull:\n%s", got)
	}
	f.Calls = nil
	if err := s.Start(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	want := "up -d --no-deps --no-build --pull never postgres\nup -d --no-deps --no-build --pull never app\nup -d --no-deps --force-recreate --no-build --pull never caddy nginx\nps"
	if got := strings.Join(composeCalls(f), "\n"); got != want {
		t.Fatalf("start:\n%s\nwant:\n%s", got, want)
	}
	for _, c := range f.Calls {
		if c.Name == "docker" && len(c.Env) > 0 && c.Env[0] != "COMPOSE_PROFILES=" {
			t.Fatalf("no profile is on: %v", c.Env)
		}
	}
	if err := s.Restart(context.Background(), &out, "nope", true); err == nil {
		t.Fatal("an unknown service is refused")
	}
}

func TestBackupDumpsGzipsAndKeepsTheNewest(t *testing.T) {
	s, f := newStack(t, "standard: 1\nbackup:\n  dir: backups\n  keep: \"2\"\n", "POSTGRES_USER=u\nPOSTGRES_DB=d\n")
	f.Rules = append([]run.Rule{{Match: "pg_dump -U u d", Stdout: "-- dump\n"}}, f.Rules...)
	dir := filepath.Join(s.Dir, "backups")
	os.MkdirAll(dir, 0o700)
	for i, n := range []string{"db-1.sql.gz", "db-2.sql.gz", "other.txt"} {
		p := filepath.Join(dir, n)
		os.WriteFile(p, []byte("x"), 0o600)
		at := time.Now().Add(-time.Duration(10-i) * time.Hour)
		os.Chtimes(p, at, at)
	}
	var out bytes.Buffer
	saved, err := s.RunBackup(context.Background(), &out, time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err, out.String())
	}
	if len(saved) != 1 || filepath.Base(saved[0]) != "db-20261012-010203.sql.gz" {
		t.Fatalf("saved %v", saved)
	}
	left, _ := os.ReadDir(dir)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, " ") != "db-2.sql.gz db-20261012-010203.sql.gz other.txt" {
		t.Fatalf("kept: %v", names)
	}
	if st, _ := os.Stat(saved[0]); st.Mode().Perm() != 0o600 {
		t.Fatal("a dump is readable by its owner only")
	}
}

// A dump that fails half way leaves nothing under a backup's name, and no .part either.
func TestAFailedBackupLeavesNoFile(t *testing.T) {
	s, f := newStack(t, "standard: 1\nbackup:\n  dir: backups\n", "POSTGRES_USER=u\nPOSTGRES_DB=d\n")
	f.Rules = append([]run.Rule{{Match: "pg_dump -U u d", Fn: func(c run.Call) (string, error) {
		return "-- half a dump\nCOPY public.t (id) FROM stdin;\n1\n", errors.New("exit status 1")
	}}}, f.Rules...)
	var out bytes.Buffer
	if _, err := s.RunBackup(context.Background(), &out, time.Now()); err == nil {
		t.Fatal("a failed dump is an error")
	}
	if left, _ := os.ReadDir(filepath.Join(s.Dir, "backups")); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

func TestInfraPullSaysWhatToReload(t *testing.T) {
	s, f := newStack(t, "standard: 1\n", "")
	os.MkdirAll(filepath.Join(s.Dir, ".git"), 0o755)
	n := 0
	f.Rules = append([]run.Rule{
		{Match: "git rev-parse HEAD", Fn: func(run.Call) (string, error) { n++; return map[int]string{1: "aaaaaaa1\n", 2: "bbbbbbb2\n"}[n], nil }},
		{Match: "git diff --name-only", Stdout: "Caddyfile\n.env.example\nbin/runsnip-linux-amd64\n"},
	}, f.Rules...)
	var out bytes.Buffer
	if err := s.InfraPull(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Updated aaaaaaa → bbbbbbb", "Caddyfile changed:", "runsnip reload (caddy reads it only when recreated)", "runsnip env combine", "runsnip itself changed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "runsnip start") {
		t.Errorf("the compose file did not change:\n%s", out.String())
	}
}

// The project label containers are read by follows compose's precedence.
func TestProjectNameFollowsCompose(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	env := map[string]string{}
	s := &Stack{Dir: filepath.Join(dir, "infra"), EnvPath: envPath, Compose: &compose.File{Name: "demo"}, Getenv: func(k string) string { return env[k] }}
	if s.ProjectName() != "demo" {
		t.Fatal(s.ProjectName())
	}
	os.WriteFile(envPath, []byte("COMPOSE_PROJECT_NAME=fromenvfile\n"), 0o600)
	if s.ProjectName() != "fromenvfile" {
		t.Fatal(s.ProjectName())
	}
	env["COMPOSE_PROJECT_NAME"] = "fromprocess"
	if s.ProjectName() != "fromprocess" {
		t.Fatal(s.ProjectName())
	}
	s.Compose.Name, env["COMPOSE_PROJECT_NAME"] = "", ""
	os.Remove(envPath)
	if s.ProjectName() != "infra" {
		t.Fatal(s.ProjectName())
	}
}
