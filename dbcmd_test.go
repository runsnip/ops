package main

import (
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/run"
)

// `db` runs on the operator's machine with no stack around it, and refuses a server restore before connecting.
func TestDBCommandsRefuseBeforeConnecting(t *testing.T) {
	f := &run.Fake{}
	testRunner = f
	t.Cleanup(func() { testRunner = nil })
	t.Setenv("RUNSNIP_HOME", t.TempDir()) // no stack.yaml: db does not need one
	for _, args := range [][]string{
		{"db", "restore", "x.dump", "--to-server", "srv", "--db", "new"},                             // not sure
		{"db", "restore", "x.dump", "--to-server", "srv", "--yes-i-am-sure"},                         // no --db
		{"db", "restore", "x.dump", "--to-server", "srv", "--db", "n", "--yes-i-am-sure", "--clean"}, // --clean
		{"db", "restore", "x.dump"},                                      // neither --into nor --to-server
		{"db", "restore", "x.dump", "--into", "c", "--to-server", "srv"}, // both
		{"db", "tunnel", "--local"},
		{"db", "pull", "a", "b"},
		{"db", "pull", "srv", "--kind", "oracle"},
	} {
		code, _, err := cli(t, args...)
		if code != 2 || err == nil {
			t.Errorf("%v: %d %v", args, code, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Fatalf("ran %v", f.Lines())
	}
	if code, out, _ := cli(t, "db", "help"); code != 0 || !strings.Contains(out, "runsnip db pull <ssh-target>") {
		t.Fatal(out)
	}
	// The flags may follow the target.
	f.Failing("ssh", "Permission denied (publickey)")
	code, _, err := cli(t, "db", "pull", "user@srv", "--compose", "/opt/x", "-o", t.TempDir()+"/x.dump")
	if code != 1 || err == nil || !strings.Contains(err.Error(), "ssh user@srv") {
		t.Fatalf("%d %v", code, err)
	}
	if l := f.Lines()[0]; !strings.Contains(l, "-M -N -f") || !strings.Contains(l, "-- user@srv") {
		t.Fatal(l)
	}
}
