package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
)

// A local infra repository with a stack on its infra branch, added the way the web adds one.
func TestStackAddClonesRegistersAndRecordsAJob(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	write := func(p, b string) {
		os.MkdirAll(filepath.Dir(filepath.Join(src, p)), 0o755)
		os.WriteFile(filepath.Join(src, p), []byte(b), 0o644)
	}
	write("stack.yaml", "standard: 1\n")
	write("docker-compose.yml", "name: demo\nservices:\n  app:\n    image: x/app:${APP_TAG:-1}\n    volumes: [\"${DATA_DIR:-/srv/demo}/x:/x\"]\n")
	write(".env.example", "DATA_DIR=/srv/demo\n")
	for _, c := range [][]string{{"init", "-q", "-b", "infra"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "infra"}} {
		cmd := exec.Command("git", c...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	state := t.TempDir()
	t.Setenv("RUNSNIP_STATE", state)
	t.Setenv("RUNSNIP_HOME", t.TempDir())
	base, _ := filepath.EvalSymlinks(t.TempDir())
	clone, data := filepath.Join(base, "opt", "demo"), filepath.Join(base, "data", "demo")

	if code, out, _ := cli(t, "stack", "check-paths", "--name", "demo", "--dir", "/etc", "--data", data); code != 1 || !strings.Contains(out, "system folder") {
		t.Fatalf("check-paths refuses with a reason: %d %s", code, out)
	}
	if code, out, err := cli(t, "stack", "add", "demo", "--repo", src, "--dir", clone, "--data", data); code != 0 || err != nil {
		t.Fatalf("add: %d %v %s", code, err, out)
	}
	e, ok, _ := host.Find(state, "demo")
	if !ok || e.Dir != clone || e.Data != data {
		t.Fatalf("registered: %+v", e)
	}
	if b, _ := os.ReadFile(filepath.Join(clone, ".env")); !strings.Contains(string(b), "DATA_DIR="+data) {
		t.Fatalf(".env holds the data folder: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(data, ".runsnip-stack")); string(b) != "demo\n" {
		t.Fatal("the data folder is marked")
	}
	list, _ := jobs.List(state, "demo", 0)
	if len(list) != 1 || list[0].Status != "ok" || list[0].Command[0] != "stack" {
		t.Fatalf("a job: %+v", list)
	}
	// The same name again, elsewhere: refused. The registered stack answers --stack.
	if code, _, _ := cli(t, "stack", "add", "demo", "--repo", src, "--dir", filepath.Join(base, "x"), "--data", filepath.Join(base, "y")); code == 0 {
		t.Fatal("a name is registered once")
	}
	if _, out, err := cli(t, "--stack", "demo", "env", "list"); err != nil || !strings.Contains(out, "DATA_DIR") {
		t.Fatalf("--stack: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(state, "audit.log")); !strings.Contains(string(b), `"command":["stack","add","demo"`) {
		t.Fatalf("audited: %s", b)
	}
	pub, _, err := deployKey(state, "demo")
	if err != nil || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("deploy key: %q %v", pub, err)
	}
	if again, _, _ := deployKey(state, "demo"); again != pub {
		t.Fatal("one key per stack")
	}
	if st, _ := os.Stat(filepath.Join(state, "keys", "demo")); st.Mode().Perm() != 0o600 {
		t.Fatal("the private key is private")
	}
}
