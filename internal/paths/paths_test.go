package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runsnip/ops/internal/host"
)

func opts(t *testing.T, fs FSInfo) Options {
	return Options{
		State:    t.TempDir(),
		FS:       func(string) (FSInfo, error) { return fs, nil },
		Writable: func(string) bool { return true },
		// No links unless a test makes some: the machine running the tests (macOS: /etc → /private/etc) is not a server.
		EvalLinks: func(p string) (string, error) { return p, nil },
	}
}

var disk = FSInfo{Type: "ext4", FreeBytes: 50 << 30}

func reasons(r Result) string { return strings.Join(r.Reasons, "; ") }

func TestSyntaxAndSystemFolders(t *testing.T) {
	o := opts(t, disk)
	for p, want := range map[string]string{
		"":                  "no path given",
		"srv/x":             "not an absolute path",
		"/srv/x/../y":       "contains . or ..",
		"/srv//x":           "not normalised: write /srv/x",
		"/":                 "system folder",
		"/etc":              "system folder",
		"/etc/runsnip":      "inside /etc, a system folder",
		"/home":             "system folder",
		"/var/lib/docker/x": "inside /var/lib/docker",
		"/proc/1":           "inside /proc",
		"/usr/local":        "system folder",
		"/tmp/stack":        "inside /tmp",
	} {
		r := one("demo", Clone, p, o)
		if r.OK || !strings.Contains(reasons(r), want) {
			t.Errorf("%q: %v, want %q", p, r.Reasons, want)
		}
	}
	for _, p := range []string{"/opt/demo", "/srv/data/demo", "/home/ops/demo", "/root/example-app"} {
		if r := one("demo", Data, p, o); !r.OK {
			t.Errorf("%s refused: %v", p, r.Reasons)
		}
	}
}

func TestOtherStacksStateAndEachOther(t *testing.T) {
	o := opts(t, disk)
	host.Register(o.State, host.StackEntry{Name: "other", Dir: "/opt/other", Data: "/srv/data/other", Added: time.Now()})
	host.Register(o.State, host.StackEntry{Name: "demo", Dir: "/opt/demo", Data: "/srv/data/demo", Added: time.Now()})
	rs := Check("demo", "/opt/other/infra", "/srv/data", o)
	if !strings.Contains(reasons(rs[0]), "a path of the stack other") || !strings.Contains(reasons(rs[1]), "a path of the stack other") {
		t.Fatalf("inside / holding another stack's paths: %+v", rs)
	}
	if rs := Check("demo", "/opt/demo", "/srv/data/demo", o); !OK(rs) {
		t.Fatalf("a stack's own registered paths are not another's: %+v", rs)
	}
	rs = Check("demo", "/srv/demo", "/srv/demo/data", o)
	if OK(rs) || !strings.Contains(reasons(rs[1]), "inside each other") {
		t.Fatalf("data inside the checkout: %+v", rs)
	}
	if r := one("demo", Data, o.State+"/x", o); r.OK {
		t.Fatal("runsnip's state is refused")
	}
}

func TestEmptyAbsentReadoptAndFilesystems(t *testing.T) {
	o := opts(t, disk)
	base := t.TempDir()
	full := filepath.Join(base, "full")
	os.MkdirAll(full, 0o755)
	os.WriteFile(filepath.Join(full, "x"), []byte("x"), 0o644)
	if r := one("demo", Clone, full, o); r.OK || !strings.Contains(reasons(r), "not empty") {
		t.Fatalf("a non-empty checkout: %v", r.Reasons)
	}
	if r := one("demo", Clone, filepath.Join(base, "absent"), o); !r.OK {
		t.Fatalf("absent is fine: %v", r.Reasons)
	}
	data := filepath.Join(base, "data")
	os.MkdirAll(data, 0o700)
	os.WriteFile(filepath.Join(data, MarkerFile), []byte("demo\n"), 0o600)
	os.WriteFile(filepath.Join(data, "pgdata"), nil, 0o600)
	if r := one("demo", Data, data, o); !r.OK || !strings.Contains(strings.Join(r.Warnings, " "), "re-adopt") {
		t.Fatalf("this stack's own data: %+v", r)
	}
	if r := one("else", Data, data, o); r.OK || !strings.Contains(reasons(r), "another stack (demo)") {
		t.Fatalf("another stack's data: %+v", r)
	}
	for typ, want := range map[string]string{"tmpfs": "gone at the next boot", "ramfs": "gone at the next boot"} {
		if r := one("demo", Data, "/srv/x", opts(t, FSInfo{Type: typ, FreeBytes: 50 << 30})); r.OK || !strings.Contains(reasons(r), want) {
			t.Errorf("%s: %v", typ, r.Reasons)
		}
	}
	if r := one("demo", Data, "/srv/x", opts(t, FSInfo{Type: "nfs", FreeBytes: 50 << 30})); !r.OK || len(r.Warnings) == 0 {
		t.Errorf("nfs warns: %+v", r)
	}
	if r := one("demo", Data, "/srv/x", opts(t, FSInfo{Type: "ext4", FreeBytes: 5 << 30})); !r.OK || !strings.Contains(strings.Join(r.Warnings, " "), "5.0 GB free") {
		t.Errorf("low disk warns: %+v", r)
	}
	if r := one("demo", Data, "/srv/x", opts(t, FSInfo{Type: "ext4", FreeBytes: 100 << 20})); r.OK {
		t.Error("almost no disk refuses")
	}
	ro := opts(t, disk)
	ro.Writable = func(string) bool { return false }
	if r := one("demo", Data, "/srv/x", ro); r.OK || !strings.Contains(reasons(r), "cannot be written") {
		t.Errorf("unwritable: %v", r.Reasons)
	}
}

func TestSymlinksAreResolvedFirst(t *testing.T) {
	o := opts(t, disk)
	o.EvalLinks = func(p string) (string, error) {
		if p == "/srv/link" {
			return "/etc", nil
		}
		if p == "/" || p == "/srv" {
			return p, nil
		}
		return "", os.ErrNotExist
	}
	r := one("demo", Data, "/srv/link/demo", o)
	if r.OK || r.Resolved != "/etc/demo" || !strings.Contains(reasons(r), "inside /etc") {
		t.Fatalf("%+v", r)
	}
}
