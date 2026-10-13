package shim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shim, run with a stub uname for each machine, starts the binary Binary names — and refuses others.
func TestShimPicksTheBinaryForTheMachine(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "runsnip"), Script, 0o755); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	for _, tg := range Targets {
		p := filepath.Join(dir, "bin", "runsnip-"+tg.OS+"-"+tg.Arch)
		os.WriteFile(p, []byte("#!/bin/sh\necho \"$(basename \"$0\") home=$RUNSNIP_HOME args=$*\"\n"), 0o755)
	}
	stubs := t.TempDir()
	for machine, want := range map[string]string{"x86_64": "linux-amd64", "amd64": "linux-amd64", "aarch64": "linux-arm64", "arm64": "linux-arm64", "riscv64": ""} {
		os.WriteFile(filepath.Join(stubs, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -m) echo "+machine+" ;; -s) echo Linux ;; esac\n"), 0o755)
		cmd := exec.Command("sh", filepath.Join(dir, "runsnip"), "ps", "-a")
		cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"), "RUNSNIP_HOME=")
		out, err := cmd.CombinedOutput()
		got := strings.TrimSpace(string(out))
		if want == "" {
			if err == nil || !strings.Contains(got, "no binary for riscv64") {
				t.Errorf("%s: want a refusal, got %q %v", machine, got, err)
			}
			if Binary("linux", machine) != "" {
				t.Errorf("Binary(%s) should be empty", machine)
			}
			continue
		}
		if err != nil || !strings.HasPrefix(got, "runsnip-"+want+" ") || !strings.Contains(got, "args=ps -a") {
			t.Errorf("%s: %q %v", machine, got, err)
		}
		real, _ := filepath.EvalSymlinks(dir)
		if !strings.Contains(got, "home="+real) && !strings.Contains(got, "home="+dir) {
			t.Errorf("%s: RUNSNIP_HOME is the shim's folder: %q", machine, got)
		}
		if Binary("linux", machine) != "bin/runsnip-"+want {
			t.Errorf("Binary(%s) = %s", machine, Binary("linux", machine))
		}
	}
	// A missing binary is said, not a cryptic exec error.
	os.Remove(filepath.Join(dir, "bin", "runsnip-linux-amd64"))
	os.WriteFile(filepath.Join(stubs, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -m) echo x86_64 ;; -s) echo Linux ;; esac\n"), 0o755)
	cmd := exec.Command("sh", filepath.Join(dir, "runsnip"))
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "missing") {
		t.Fatalf("%q %v", out, err)
	}
}
