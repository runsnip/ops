package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/stack"
)

func envStack(t *testing.T) string {
	t.Helper()
	dir, _ := stackDirFor(t)
	os.WriteFile(filepath.Join(dir, "stack.yaml"), []byte("standard: 1\nenv_rules:\n  vars:\n    APP_URL: { type: url }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("name: demo\nservices:\n  app:\n    image: ${P}-app:${APP_TAG:-latest}\n    environment: { APP_URL: \"${APP_URL}\", SECRET_KEY: \"${SECRET_KEY}\" }\n  db:\n    image: postgres:16\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env.example"), []byte("# Where the app is.\nAPP_URL=\nSECRET_KEY=\nNEW_ONE=x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("APP_URL=https://a\nSECRET_KEY=hunter2hunter2\nOLD=1\n"), 0o640)
	os.Chmod(filepath.Join(dir, ".env"), 0o640)
	return dir
}

func TestEnvSetValidatesSavesAtomicallyAndNamesKeysOnly(t *testing.T) {
	dir := envStack(t)
	if _, _, err := cli(t, "env", "set", "APP_URL=not a url"); err == nil || !strings.Contains(err.Error(), "APP_URL: not a URL") {
		t.Fatalf("validation: %v", err)
	}
	_, out, err := cli(t, "env", "set", "APP_URL=https://b", "--off", "OLD")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "saved .env: APP_URL OLD") || !strings.Contains(out, "read by: app") || strings.Contains(out, "https://b") {
		t.Fatalf("names keys and readers, never values: %s", out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(b), "APP_URL=https://b\n") || !strings.Contains(string(b), "# OLD=1") {
		t.Fatalf(".env:\n%s", b)
	}
	if st, _ := os.Stat(filepath.Join(dir, ".env")); st.Mode().Perm() != 0o640 {
		t.Fatalf("the mode is kept: %v", st.Mode().Perm())
	}
	baks, _ := filepath.Glob(filepath.Join(dir, ".env.bak-*"))
	if len(baks) != 1 {
		t.Fatalf("the old file is kept: %v", baks)
	}
}

func TestEnvSetFromStdinKeepsValuesOffTheCommandLine(t *testing.T) {
	dir := envStack(t)
	st, _ := stack.Open(dir)
	var out strings.Builder
	if err := envSet(t.Context(), st, []string{"--stdin"}, strings.NewReader(`{"SECRET_KEY":"s3cr3t value"}`), &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.Contains(string(b), "SECRET_KEY='s3cr3t value'") || strings.Contains(out.String(), "s3cr3t") {
		t.Fatalf(".env:\n%s\nout: %s", b, out.String())
	}
}

func TestEnvCombineDiffMasksSecretsAndApplyWrites(t *testing.T) {
	dir := envStack(t)
	if _, out, _ := cli(t, "env", "list", "--filter", "new"); !strings.Contains(out, "NEW_ONE") || strings.Contains(out, "APP_URL") {
		t.Fatal(out)
	}
	_, out, err := cli(t, "env", "combine", "--diff")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "+NEW_ONE=") || strings.Contains(out, "hunter2") || !strings.Contains(out, "SECRET_KEY=•••••••• (14 chars)") {
		t.Fatalf("diff:\n%s", out)
	}
	before, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if _, _, err := cli(t, "env", "combine", "--diff"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, ".env")); string(after) != string(before) {
		t.Fatal("--diff writes nothing")
	}
	if _, out, err = cli(t, "env", "combine", "--apply"); err != nil || !strings.Contains(out, "new, need a value: NEW_ONE") {
		t.Fatalf("%v %s", err, out)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if !strings.HasPrefix(string(b), "# Where the app is.\nAPP_URL=https://a\n") || !strings.Contains(string(b), "\nOLD=1\n") {
		t.Fatalf("applied:\n%s", b)
	}
	if _, out, _ = cli(t, "env", "combine", "--apply"); !strings.Contains(out, "nothing written") {
		t.Fatal(out)
	}
	if _, out, _ = cli(t, "env", "list", "--filter", "missing"); !strings.Contains(out, "NEW_ONE") || strings.Contains(out, "APP_URL") {
		t.Fatal(out)
	}
	if _, out, _ = cli(t, "env", "list"); strings.Contains(out, "hunter2") {
		t.Fatal("list masks secrets")
	}
}

func TestExampleChangesAfterAPull(t *testing.T) {
	added, dropped := stack.ExampleChanges("A=\nGONE=\n# OPT=\n", "A=\nB=\n# C=\n#   D=1 E=2\n", "A=1\nGONE=2\n")
	if strings.Join(added, " ") != "B" || strings.Join(dropped, " ") != "GONE" {
		t.Fatalf("added %v dropped %v", added, dropped)
	}
}
