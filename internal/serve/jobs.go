package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/runsnip/ops/internal/auth"
	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
)

// What the page may ask the command line to do — the same arguments a console would type, nothing else.
var safeArg = regexp.MustCompile(`^[A-Za-z0-9._:/@+=~,-]{1,256}$`)

type rule struct {
	prefix    []string // fixed words
	args      int      // positional arguments after them
	flags     []string // allowed flags (each may take one value when listed with "=")
	stack     bool     // runs against a stack (--stack <name>)
	dangerous bool     // needs a fresh authentication
	more      bool     // any number of extra positional arguments (env generate KEY…)
}

var rules = []rule{
	{prefix: []string{"doctor"}, stack: true},
	{prefix: []string{"start"}, args: 0, stack: true, dangerous: true},
	{prefix: []string{"start"}, args: 1, stack: true, dangerous: true},
	{prefix: []string{"restart"}, args: 1, flags: []string{"--no-pull"}, stack: true, dangerous: true},
	{prefix: []string{"stop"}, args: 0, stack: true, dangerous: true},
	{prefix: []string{"stop"}, args: 1, stack: true, dangerous: true},
	{prefix: []string{"reload"}, stack: true, dangerous: true},
	{prefix: []string{"pin"}, args: 2, stack: true, dangerous: true},
	{prefix: []string{"rollback"}, args: 1, stack: true, dangerous: true},
	{prefix: []string{"backup"}, stack: true},
	{prefix: []string{"infra", "pull"}, stack: true, dangerous: true},
	{prefix: []string{"login"}, stack: true, dangerous: true},
	{prefix: []string{"env", "set", "--stdin"}, flags: []string{"--restart", "--on=", "--off="}, stack: true, dangerous: true},
	{prefix: []string{"env", "generate"}, flags: []string{"--force"}, more: true, stack: true, dangerous: true},
	{prefix: []string{"env", "combine", "--apply"}, stack: true, dangerous: true},
	{prefix: []string{"stack", "add"}, args: 1, flags: []string{"--repo=", "--branch=", "--dir=", "--data="}, dangerous: true},
	{prefix: []string{"stack", "remove"}, args: 1, flags: []string{"--down"}, dangerous: true},
	{prefix: []string{"stack", "keygen"}, args: 1},
	{prefix: []string{"self", "check"}},
	{prefix: []string{"self", "update"}, flags: []string{"--version="}, dangerous: true},
	{prefix: []string{"self", "rollback"}, dangerous: true},
	{prefix: []string{"admin", "add"}, args: 1, dangerous: true},
	{prefix: []string{"admin", "reset"}, args: 1, dangerous: true},
	{prefix: []string{"admin", "remove"}, args: 1, dangerous: true},
}

// Allowed checks a command against the rules: which rule it matches (nil when none).
func Allowed(cmd []string) (*rule, error) {
	for _, a := range cmd {
		if !safeArg.MatchString(a) {
			return nil, fmt.Errorf("argument %q is not allowed", a)
		}
	}
	for i := range rules {
		r := &rules[i]
		if len(cmd) < len(r.prefix) || strings.Join(cmd[:len(r.prefix)], " ") != strings.Join(r.prefix, " ") {
			continue
		}
		rest, pos, ok := cmd[len(r.prefix):], 0, true
		for j := 0; j < len(rest); j++ {
			a := rest[j]
			if strings.HasPrefix(a, "-") {
				known := false
				for _, f := range r.flags {
					if f == a {
						known = true
					} else if strings.HasSuffix(f, "=") && strings.TrimSuffix(f, "=") == a && j+1 < len(rest) && !strings.HasPrefix(rest[j+1], "-") {
						known, j = true, j+1
					}
				}
				if !known {
					ok = false
				}
				continue
			}
			pos++
		}
		if ok && (pos == r.args || r.more && pos >= r.args) {
			return r, nil
		}
	}
	return nil, fmt.Errorf("runsnip %s is not something the web can run", strings.Join(cmd, " "))
}

// startJob runs a runsnip command as a job: the same binary, its arguments, the admin's name, its log on disk.
func (s *Server) startJob(w http.ResponseWriter, r *http.Request, st *state) {
	var in struct {
		Stack   string   `json:"stack"`
		Command []string `json:"command"`
		Stdin   string   `json:"stdin"` // values (env set, a registry token): never on the command line or in the log
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	rl, err := Allowed(in.Command)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if rl.dangerous && !st.Fresh(s.Now()) {
		fail(w, http.StatusPreconditionRequired, "reauth")
		return
	}
	if rl.stack {
		if _, ok, _ := host.Find(s.Env.State, in.Stack); !ok {
			fail(w, 404, "no such stack")
			return
		}
	} else {
		in.Stack = ""
	}
	j, err := s.spawn(in.Stack, in.Command, in.Stdin, st.Session)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.audit(r, st.Session, "job.start", map[string]any{"job": j.ID, "stack": in.Stack, "command": in.Command})
	writeJSON(w, j)
}

// spawn starts `runsnip [--stack s] <command>` in its own session (it outlives a restart of serve: the unit uses
// KillMode=process), with the job's id; the child writes its log itself.
func (s *Server) spawn(stackName string, command []string, stdin string, sess *auth.Session) (jobs.Job, error) {
	label := stackName
	if label == "" {
		label = "_" + command[0]
		if command[0] == "self" {
			label = "_runsnip" // the name the command line's own lock and record use
		}
	}
	j, err := jobs.Create(s.Env.State, jobs.Job{Stack: label, Command: command, User: sess.Name, Via: "web"})
	if err != nil {
		return j, err
	}
	args := command
	if stackName != "" {
		args = append([]string{"--stack", stackName}, command...)
	}
	cmd := exec.Command(s.Env.Exe, args...)
	cmd.Env = append(os.Environ(), "RUNSNIP_JOB_ID="+j.ID, "RUNSNIP_JOB_USER="+sess.Name, "RUNSNIP_STATE="+s.Env.State, "NO_COLOR=1")
	cmd.Stdin = strings.NewReader(stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = jobs.Finish(s.Env.State, j, 127)
		return j, err
	}
	j.PID = cmd.Process.Pid
	_ = jobs.Update(s.Env.State, j)
	go func() {
		err := cmd.Wait()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			code = 1
		}
		// A child that died without recording its end (killed, crashed) gets its exit code here.
		if cur, err := jobs.Get(s.Env.State, j.ID); err == nil && cur.Ended == nil {
			_ = jobs.Finish(s.Env.State, cur, code)
		}
	}()
	return j, nil
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, _ *state) {
	list, err := jobs.List(s.Env.State, r.URL.Query().Get("stack"), 50)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	if list == nil {
		list = []jobs.Job{}
	}
	writeJSON(w, list)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, _ *state) {
	j, err := jobs.Get(s.Env.State, r.PathValue("id"))
	if err != nil {
		fail(w, 404, "no such job")
		return
	}
	writeJSON(w, j)
}

// jobLog streams a job's log from the start, then as it grows, and ends with the job's outcome.
func (s *Server) jobLog(w http.ResponseWriter, r *http.Request, _ *state) {
	id := r.PathValue("id")
	if _, err := jobs.Get(s.Env.State, id); err != nil {
		fail(w, 404, "no such job")
		return
	}
	f, ok := sse(w)
	if !ok {
		return
	}
	file, err := os.Open(jobs.LogPath(s.Env.State, id))
	if err != nil {
		return
	}
	defer file.Close()
	rd := bufio.NewReader(file)
	var partial bytes.Buffer
	for {
		line, err := rd.ReadString('\n')
		if err == nil {
			partial.WriteString(line)
			fmt.Fprintf(w, "data: %s\n\n", jsonString(strings.TrimRight(partial.String(), "\n")))
			partial.Reset()
			f.Flush()
			continue
		}
		partial.WriteString(line)
		j, _ := jobs.Get(s.Env.State, id)
		if j.Status != "running" {
			if partial.Len() > 0 {
				fmt.Fprintf(w, "data: %s\n\n", jsonString(partial.String()))
			}
			b, _ := json.Marshal(j)
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", b)
			f.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// checkPaths asks the command line whether paths may hold a stack (runsnip stack check-paths --json).
func (s *Server) checkPaths(w http.ResponseWriter, r *http.Request, _ *state) {
	var in struct{ Name, Dir, Data string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Env.Exe, "stack", "check-paths", "--json", "--name", in.Name, "--dir", in.Dir, "--data", in.Data)
	cmd.Env = append(os.Environ(), "RUNSNIP_STATE="+s.Env.State)
	out, _ := cmd.Output()
	w.Header().Set("Content-Type", "application/json")
	if len(bytes.TrimSpace(out)) == 0 {
		out = []byte("[]")
	}
	w.Write(out)
}

func (s *Server) listAdmins(w http.ResponseWriter, r *http.Request, _ *state) {
	all, err := s.Auth.Admins()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	type row struct {
		Name     string    `json:"name"`
		Passkeys int       `json:"passkeys"`
		TOTP     bool      `json:"totp"`
		Pending  bool      `json:"pending"`
		Created  time.Time `json:"created"`
	}
	out := []row{}
	for _, a := range all {
		out = append(out, row{a.Name, len(a.Passkeys), a.TOTP != "", !a.HasCredential(), a.Created})
	}
	writeJSON(w, out)
}

// auditTail is the last records of the audit log (newest last).
func (s *Server) auditTail(w http.ResponseWriter, r *http.Request, _ *state) {
	b, err := os.ReadFile(s.Env.State + "/audit.log")
	if err != nil && !os.IsNotExist(err) {
		fail(w, 500, err.Error())
		return
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	out := []json.RawMessage{}
	for _, l := range lines {
		if json.Valid([]byte(l)) {
			out = append(out, json.RawMessage(l))
		}
	}
	writeJSON(w, out)
}
