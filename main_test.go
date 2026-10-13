package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/run"
)

func stackDirFor(t *testing.T) (string, *run.Fake) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "stack.yaml"), []byte("standard: 1\nshield: {addr_env: SHIELD_ADDR, addr: \"172.17.0.1:9180\", copy_headers: [X-Shield-Client]}\ndownloads:\n  - {command: models, dir: \"${DATA_DIR:-/x}/m\", url: \"https://e.x/{item}\", file: \"{item}.bin\", items: [small]}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("name: demo\nservices:\n  app:\n    image: ${P}-app:${APP_TAG:-latest}\n  api:\n    image: ${P}-api:${API_TAG:-latest}\n  db:\n    image: postgres:16\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("APP_TAG=1.0.0\nDATA_DIR="+dir+"\n"), 0o600)
	t.Setenv("RUNSNIP_HOME", dir)
	f := &run.Fake{}
	f.On("", "")
	testRunner = f
	t.Cleanup(func() { testRunner = nil })
	return dir, f
}

func cli(t *testing.T, args ...string) (int, string, error) {
	t.Helper()
	var out bytes.Buffer
	code, err := run1(context.Background(), args, &out)
	return code, out.String(), err
}

func TestCommandParsing(t *testing.T) {
	dir, f := stackDirFor(t)

	if code, out, _ := cli(t, "help"); code != 0 || !strings.Contains(out, "runsnip doctor") || !strings.Contains(out, "runsnip models") {
		t.Fatalf("help lists the commands and the stack's own downloads:\n%s", out)
	}
	if code, _, err := cli(t, "frobnicate"); code != 2 || err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown: %d %v", code, err)
	}
	// pin with one argument pins the default component (the first in compose).
	f.Calls = nil
	if _, _, err := cli(t, "pin", "1.1.0"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(b), "APP_TAG=1.1.0") || !strings.Contains(string(b), "APP_TAG_PREVIOUS=1.0.0") {
		t.Fatalf(".env:\n%s", b)
	}
	if code, _, err := cli(t, "pin"); code != 2 || err == nil {
		t.Fatal("pin without a version is a usage error")
	}
	// restart: the service and --no-pull in either order; aliases.
	for _, args := range [][]string{{"restart", "api", "--no-pull"}, {"redeploy", "--no-pull", "api"}} {
		f.Calls = nil
		if _, _, err := cli(t, args...); err != nil {
			t.Fatal(err)
		}
		l := strings.Join(f.Lines(), "\n")
		if strings.Contains(l, " pull") || !strings.Contains(l, "up -d --no-deps --force-recreate api") {
			t.Fatalf("%v:\n%s", args, l)
		}
	}
	if code, _, _ := cli(t, "restart"); code != 2 {
		t.Fatal("restart needs a service")
	}
	if _, _, err := cli(t, "start", "nope"); err == nil || !strings.Contains(err.Error(), "app api") {
		t.Fatalf("start <unknown> names the components: %v", err)
	}
	if code, _, err := cli(t, "infra", "push"); code != 2 || err == nil {
		t.Fatal("infra has one subcommand")
	}
	if _, out, err := cli(t, "models"); err != nil || !strings.Contains(out, "runsnip models get small") {
		t.Fatalf("a stack's download command: %q %v", out, err)
	}
	if _, out, _ := cli(t, "version"); !strings.Contains(out, "app") || !strings.Contains(out, "1.1.0") {
		t.Fatal(out)
	}
	if _, out, _ := cli(t, "shield", "snippet"); !strings.Contains(out, "forward_auth {$SHIELD_ADDR:172.17.0.1:9180}") || !strings.Contains(out, "copy_headers X-Shield-Client") || !strings.Contains(out, "(shield_off)") {
		t.Fatal(out)
	}
	if _, _, err := cli(t, "env", "nope"); err == nil {
		t.Fatal("env has two subcommands")
	}
}

func TestNoCommandInstallsAThirdPartyService(t *testing.T) {
	_, f := stackDirFor(t)
	for _, args := range [][]string{{"setup"}, {"setup", "--install-shield"}} {
		if code, _, err := cli(t, args...); code != 2 || err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	for _, args := range [][]string{{"help"}, {"shield", "snippet"}, {"version"}, {"doctor"}} {
		cli(t, args...)
	}
	for _, l := range f.Lines() {
		if strings.HasPrefix(l, "systemctl") || strings.Contains(l, "install") {
			t.Fatalf("runsnip ran %q", l)
		}
	}
}
