package jobs

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestJobsRecordAndList(t *testing.T) {
	state := t.TempDir()
	a, err := Create(state, Job{Stack: "demo", Command: []string{"pin", "api", "1.2.0"}, User: "ops", Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // ids sort by second
	b, _ := Create(state, Job{Stack: "other", Command: []string{"backup"}, User: "root", Via: "ssh"})
	log, _ := OpenLog(state, a.ID)
	log.WriteString("pulling\n")
	log.Close()
	Finish(state, a, 3)
	got, _ := Get(state, a.ID)
	if got.Status != "failed" || *got.Exit != 3 || got.Ended == nil {
		t.Fatalf("%+v", got)
	}
	if bb, _ := os.ReadFile(LogPath(state, a.ID)); string(bb) != "pulling\n" {
		t.Fatal("log")
	}
	all, _ := List(state, "", 0)
	if len(all) != 2 || all[0].ID != b.ID {
		t.Fatalf("newest first: %+v", all)
	}
	if only, _ := List(state, "demo", 0); len(only) != 1 || only[0].ID != a.ID {
		t.Fatal("per stack")
	}
	if _, err := Get(state, "../x"); err == nil {
		t.Fatal("ids are not paths")
	}
	// A job whose process is gone without finishing is "lost", not "running" forever.
	c, _ := Create(state, Job{Stack: "demo", Command: []string{"start"}})
	c.PID = 999999
	Update(state, c)
	if got, _ := Get(state, c.ID); got.Status != "lost" {
		t.Fatalf("lost: %+v", got)
	}
}

func TestLockIsPerStackSaysWhoHoldsItAndWaits(t *testing.T) {
	state := t.TempDir()
	release, err := Lock(state, "demo", Holder{Job: "j1", Command: "start", User: "ops"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(state, "other", Holder{}, 0); err != nil {
		t.Fatal("another stack is not blocked")
	}
	_, err = Lock(state, "demo", Holder{Job: "j2"}, 300*time.Millisecond)
	var busy ErrBusy
	if !errors.As(err, &busy) || busy.Holder.Job != "j1" || !strings.Contains(err.Error(), "runsnip start") {
		t.Fatalf("busy: %v", err)
	}
	if b := Busy(state); len(b) != 2 {
		t.Fatalf("busy stacks: %+v", b)
	}
	go func() { time.Sleep(300 * time.Millisecond); release() }()
	r2, err := Lock(state, "demo", Holder{Job: "j2"}, 3*time.Second)
	if err != nil {
		t.Fatalf("waits for the lock: %v", err)
	}
	r2()
}
