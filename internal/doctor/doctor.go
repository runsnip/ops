// Package doctor checks a running stack and says what to fix. The contract (stack standard v1, "doctor"):
//
//   - every check reports — pass, fail, note (true but worth knowing, not counted), warn (a feature is off) or info;
//   - a probe that fails (a container not running, a command that errors, a panic in a check) is reported as that
//     check's failure and the doctor goes on: it never stops part-way and never exits silently;
//   - the summary counts failures; the command exits 1 when there is any.
//
// Sections come from the stack definition (doctor.sections), each a list of typed checks (checks.go lists the types).
// The standard's own checks — .env, versions, containers, health, running configuration, the Postgres connection
// budget, the request shield's route — run in the project's sections where it places them, and otherwise in standard sections of their
// own after the project's.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/expand"
	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/sysinfo"
)

// Level is a result's kind.
type Level string

const (
	Pass Level = "pass"
	Fail Level = "fail"
	Note Level = "note"
	Warn Level = "warn"
	Info Level = "info"
)

// Item is one line of the report.
type Item struct {
	Level Level  `json:"level"`
	Text  string `json:"text"`
}

// Section is a titled group of results.
type Section struct {
	Title string `json:"title"`
	Items []Item `json:"items"`
}

// Report is a whole run.
type Report struct {
	Sections []Section `json:"sections"`
	Failures int       `json:"failures"`
}

// Doctor runs checks against one stack. Its fields are the world it looks at; tests replace them.
type Doctor struct {
	St *stack.Stack
	// Live receives each section as it completes (the CLI prints as it goes).
	Live func(Section)

	LookupIP   func(ctx context.Context, host string) ([]string, error)
	PublicIP   func(ctx context.Context) string
	Head       func(ctx context.Context, url, connectTo string) (http.Header, error)
	Host       func() sysinfo.Snapshot
	Stat       func(path string) (os.FileInfo, error)
	ReadDir    func(path string) ([]os.DirEntry, error)
	OwnerOf    func(path string) (string, error)
	Now        func() time.Time
	ExampleKey func() ([]string, error)

	ctx     context.Context
	vals    map[string]string
	ps      map[string]stack.Service
	psErr   error
	report  Report
	section *Section
	used    map[string]bool
}

// New returns a doctor looking at the real world.
func New(st *stack.Stack) *Doctor {
	return &Doctor{
		St:       st,
		LookupIP: func(ctx context.Context, h string) ([]string, error) { return net.DefaultResolver.LookupHost(ctx, h) },
		PublicIP: publicIP,
		Head:     head,
		Host:     func() sysinfo.Snapshot { return sysinfo.Read(st.Dir) },
		Stat:     os.Stat,
		ReadDir:  os.ReadDir,
		OwnerOf:  ownerOf,
		Now:      time.Now,
	}
}

// Run checks everything and returns the report.
func (d *Doctor) Run(ctx context.Context) Report {
	d.ctx = ctx
	d.report = Report{}
	d.used = map[string]bool{}
	env, err := d.St.Env()
	if err != nil {
		d.begin("Configuration")
		d.add(Fail, err.Error())
		d.end()
		return d.report
	}
	d.vals = env.Values()
	d.loadPS()
	for _, s := range d.St.Def.Doctor.Sections {
		for _, c := range s.Checks {
			// A check limited to some services does not stand for the standard one over all of them.
			if c.Service == "" && len(c.Services) == 0 {
				d.used[c.Type] = true
			}
		}
	}
	for _, s := range d.St.Def.Doctor.Sections {
		d.runSection(s)
	}
	for _, s := range StandardSections(d.St) {
		if d.skipped(s.Title) {
			continue
		}
		var left []def.Check
		for _, c := range s.Checks {
			if !d.used[c.Type] {
				left = append(left, c)
			}
		}
		if len(left) > 0 {
			s.Checks = left
			d.runSection(s)
		}
	}
	return d.report
}

func (d *Doctor) skipped(title string) bool {
	for _, t := range d.St.Def.Doctor.Skip {
		if strings.EqualFold(t, title) {
			return true
		}
	}
	return false
}

// StandardSections are the checks every stack gets (stack standard v1, "doctor").
func StandardSections(st *stack.Stack) []def.Section {
	out := []def.Section{
		{Title: "Configuration", Checks: []def.Check{{Type: "env_mode"}, {Type: "env_complete"}, {Type: "placeholders"}, {Type: "docker"}}},
		{Title: "Versions", Checks: []def.Check{{Type: "versions"}}},
		{Title: "Containers", Checks: []def.Check{{Type: "containers"}, {Type: "health_status"}}},
		{Title: "Running configuration", Checks: []def.Check{{Type: "config_fresh"}}},
	}
	if _, ok := st.Postgres(); ok && st.Def.Postgres != nil && st.Def.Postgres.PoolEnv != "" && st.Def.Postgres.URLEnv != "" {
		out = append(out, def.Section{Title: "Postgres: connection budget", Checks: []def.Check{{Type: "pg_budget"}}})
	}
	if st.Def.Shield != nil {
		out = append(out, def.Section{Title: "Request shield", Checks: []def.Check{{Type: "shield_route"}}})
	}
	return out
}

func (d *Doctor) runSection(s def.Section) {
	if s.If != nil && !d.holds(*s.If) {
		if s.Off != "" { // a section that is off says so; one that does not apply is left out
			d.begin(s.Title)
			d.add(Info, d.text(s.Off, nil))
			d.end()
		}
		return
	}
	d.begin(s.Title)
	for _, c := range s.Checks {
		d.runCheck(c)
	}
	d.end()
}

// runCheck runs one check; whatever happens inside, the doctor goes on.
func (d *Doctor) runCheck(c def.Check) {
	defer func() {
		if r := recover(); r != nil {
			d.add(Fail, fmt.Sprintf("the %s check stopped: %v — reported, the doctor goes on", c.Type, r))
		}
	}()
	if c.If != nil && !d.holds(*c.If) {
		return
	}
	fn, ok := checks[c.Type]
	if !ok {
		d.add(Fail, fmt.Sprintf("unknown check type %q in %s (runsnip validate)", c.Type, def.FileName))
		return
	}
	fn(d, c)
}

func (d *Doctor) begin(title string) {
	d.report.Sections = append(d.report.Sections, Section{Title: title})
	d.section = &d.report.Sections[len(d.report.Sections)-1]
}

func (d *Doctor) end() {
	if d.section != nil && len(d.section.Items) == 0 { // nothing to say: no empty heading
		d.report.Sections = d.report.Sections[:len(d.report.Sections)-1]
	} else if d.Live != nil && d.section != nil {
		d.Live(*d.section)
	}
	d.section = nil
}

func (d *Doctor) add(l Level, text string) {
	if d.section == nil {
		d.begin("")
	}
	if l == Fail {
		d.report.Failures++
	}
	d.section.Items = append(d.section.Items, Item{Level: l, Text: text})
}

// verdict adds a pass or, on failure, the check's level (fail unless the check says note/warn).
func (d *Doctor) verdict(ok bool, c def.Check, pass, fail string, extra map[string]string) {
	if ok {
		if p := pick(c.Pass, pass); p != "" && p != "-" { // "-": pass silently
			d.add(Pass, d.text(p, extra))
		}
		return
	}
	d.add(failLevel(c), d.text(pick(c.Fail, fail), extra))
}

func failLevel(c def.Check) Level {
	switch c.Level {
	case "note":
		return Note
	case "warn":
		return Warn
	case "info":
		return Info
	}
	return Fail
}

func pick(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

// text fills a message: {name} from extra, then ${VAR} from .env.
func (d *Doctor) text(t string, extra map[string]string) string {
	for k, v := range extra {
		t = strings.ReplaceAll(t, "{"+k+"}", v)
	}
	return d.expand(t)
}

func (d *Doctor) expand(t string) string {
	return expand.String(t, func(k string) (string, bool) {
		if v, ok := d.vals[k]; ok {
			return v, true
		}
		if v := d.St.Getenv(k); v != "" {
			return v, true
		}
		return "", false
	})
}

func (d *Doctor) holds(c def.Cond) bool {
	return d.St.Holds(c, d.vals, d.running)
}

// ───────────────────────── containers ─────────────────────────

func (d *Doctor) loadPS() {
	d.ps = map[string]stack.Service{}
	rows, err := d.St.Containers(d.ctx)
	if err != nil {
		d.psErr = err
		return
	}
	for _, r := range rows {
		d.ps[r.Name] = r
	}
}

func (d *Doctor) running(svc string) bool { return d.ps[svc].State == "running" }

// exec runs cmd in svc and returns its stdout, and whether it exited 0.
func (d *Doctor) exec(svc string, env map[string]string, cmd ...string) (string, bool) {
	out, err := d.St.Exec(d.ctx, svc, env, cmd...)
	return strings.TrimSpace(out), err == nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func (d *Doctor) inspect(container, format string) string {
	if container == "" {
		return ""
	}
	out, err := run.Output(d.ctx, d.St.Run, run.Call{Name: "docker", Args: []string{"inspect", "-f", format, container}})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ───────────────────────── the world ─────────────────────────

func publicIP(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return strings.TrimSpace(string(b))
}

// head sends HEAD url; connectTo (curl's --connect-to, HOST1:PORT1:HOST2:PORT2, empty parts = any) sends it to
// another address and then skips certificate checks (a replica's Caddy has its own CA).
func head(ctx context.Context, url, connectTo string) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if connectTo != "" {
		from, to, err := parseConnectTo(connectTo)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if matchAddr(from, addr) {
				addr = to
			}
			return dialer.DialContext(ctx, network, addr)
		}
		tr.TLSClientConfig.InsecureSkipVerify = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	h := resp.Header.Clone()
	h.Set(":status", fmt.Sprint(resp.StatusCode))
	return h, nil
}

// parseConnectTo reads HOST1:PORT1:HOST2:PORT2.
func parseConnectTo(v string) (from [2]string, to string, err error) {
	p := strings.Split(v, ":")
	if len(p) != 4 {
		return from, "", fmt.Errorf("connect-to %q: want HOST1:PORT1:HOST2:PORT2", v)
	}
	return [2]string{p[0], p[1]}, net.JoinHostPort(p[2], p[3]), nil
}

func matchAddr(from [2]string, addr string) bool {
	h, p, _ := net.SplitHostPort(addr)
	return (from[0] == "" || from[0] == h) && (from[1] == "" || from[1] == p)
}

// Print writes a section as text.
func Print(w io.Writer, s Section, color bool) {
	c := func(code, t string) string {
		if !color {
			return t
		}
		return "\033[" + code + "m" + t + "\033[0m"
	}
	if s.Title != "" {
		fmt.Fprintln(w, c("1", s.Title))
	}
	for _, it := range s.Items {
		switch it.Level {
		case Pass:
			fmt.Fprintf(w, "  %s %s\n", c("32", "✓"), it.Text)
		case Fail:
			fmt.Fprintf(w, "  %s %s\n", c("31", "✗"), it.Text)
		case Note:
			fmt.Fprintf(w, "  %s %s\n", c("33", "•"), it.Text)
		case Warn:
			fmt.Fprintf(w, "  %s  %s\n", c("33", "⚠"), indent(it.Text, "     "))
		default:
			fmt.Fprintf(w, "  %s\n", c("2", indent(it.Text, "  ")))
		}
	}
	fmt.Fprintln(w)
}

func indent(t, pad string) string { return strings.ReplaceAll(t, "\n", "\n"+pad) }

// Summary is the last line.
func Summary(r Report) string {
	if r.Failures == 0 {
		return "No problems found."
	}
	if r.Failures == 1 {
		return "1 thing to fix."
	}
	return fmt.Sprintf("%d things to fix.", r.Failures)
}

// JSON writes the report.
func JSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func cores() int { return runtime.NumCPU() }
