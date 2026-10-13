// Package tui is the `runsnip top` terminal dashboard: tabs to watch and operate a stack (overview with the four
// numbers worth a glance, services, versions, logs, doctor, security, backups, the infra checkout, .env), keyboard
// and mouse driven.
package tui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/runsnip/ops/internal/doctor"
	"github.com/runsnip/ops/internal/envfile"
	"github.com/runsnip/ops/internal/envmodel"
	"github.com/runsnip/ops/internal/keygen"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/sysinfo"
)

type tab int

const (
	tabOverview tab = iota
	tabServices
	tabVersions
	tabLogs
	tabDoctor
	tabBackups
	tabInfra
	tabEnv
)

var tabNames = []string{"Overview", "Services", "Versions", "Logs", "Doctor", "Backups", "Infra", "Env"}

const (
	fastEvery = 2 * time.Second  // services, stats, host
	slowEvery = 20 * time.Second // versions, backups, infra
	maxLog    = 2000
	histLen   = 60 // samples kept for the sparklines (two minutes)
)

type mode int

const (
	modeNormal mode = iota
	modeConfirm
	modeInput
	modeOutput
)

// action is an operation run in the background; its output is shown when it ends.
type action struct {
	title           string
	run             func(ctx context.Context, out io.Writer) error
	alt             *action
	label, altLabel string
}

// Model is the Bubble Tea model.
type Model struct {
	st      *stack.Stack
	version string
	host    string
	w, h    int
	tab     tab
	sel     map[tab]int

	services []stack.Service
	versions []stack.Version
	backups  []stack.Backup
	infra    stack.InfraState
	report   *doctor.Report
	doctorAt time.Time

	netRx, netTx         float64
	netAt                time.Time
	netPrevRx, netPrevTx uint64
	netPrevAt            time.Time
	hist                 map[string][]float64

	env    []envmodel.Var
	envErr string
	reveal bool
	envKey string

	sys     sysinfo.Snapshot
	cpu     float64
	fastErr string
	slowErr string
	updated time.Time

	logSvc    string
	logLines  []string
	logFollow bool
	logFilter string
	logScroll int
	logCancel context.CancelFunc
	logCh     chan string
	logGen    int

	docScroll int

	mode      mode
	prompt    string
	input     textinput.Model
	onInput   func(string) *action
	filtering bool
	svcFilter string
	svcSearch textinput.Model
	searching bool
	confirm   *action
	busy      string
	output    string
	outputErr bool
	status    string
}

// New builds the dashboard for a stack.
func New(st *stack.Stack, version string) Model {
	host, _ := os.Hostname()
	ti := textinput.New()
	ti.CharLimit = 64
	search := textinput.New()
	search.CharLimit = 40
	search.Prompt = ""
	search.Placeholder = "type to filter services"
	return Model{st: st, version: version, host: host, w: 120, h: 40, sel: map[tab]int{}, logFollow: true, input: ti, svcSearch: search, hist: map[string][]float64{}}
}

// ───────────────────────── messages ─────────────────────────

type tickMsg time.Time
type slowTickMsg time.Time
type fastMsg struct {
	services []stack.Service
	sys      sysinfo.Snapshot
	err      error
}
type slowMsg struct {
	versions []stack.Version
	backups  []stack.Backup
	infra    stack.InfraState
	err      error
}
type doctorMsg doctor.Report
type logLineMsg struct {
	gen  int
	line string
}
type logEndMsg struct{ gen int }
type actionDoneMsg struct {
	title, out string
	err        error
}

func (m Model) fetchFast() tea.Cmd {
	st := m.st
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		svcs, err := st.Services(ctx)
		if err == nil {
			st.Stats(ctx, svcs)
		}
		return fastMsg{services: svcs, sys: sysinfo.Read(diskDir(st)), err: err}
	}
}

func orStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (m Model) fetchSlow(fetchInfra bool) tea.Cmd {
	st := m.st
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var msg slowMsg
		msg.versions, msg.err = st.Versions(ctx)
		msg.backups, _ = st.Backups()
		msg.infra, _ = st.Infra(ctx, fetchInfra)
		return msg
	}
}

func (m Model) runDoctor() tea.Cmd {
	st := m.st
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		return doctorMsg(doctor.New(st).Run(ctx))
	}
}

func tick() tea.Cmd { return tea.Tick(fastEvery, func(t time.Time) tea.Msg { return tickMsg(t) }) }
func slowTick() tea.Cmd {
	return tea.Tick(slowEvery, func(t time.Time) tea.Msg { return slowTickMsg(t) })
}

func runAction(a *action) tea.Cmd {
	return func() tea.Msg {
		var buf bytes.Buffer
		err := a.run(context.Background(), &buf)
		return actionDoneMsg{title: a.title, out: buf.String(), err: err}
	}
}

func (m *Model) startLogs() tea.Cmd {
	if m.logCancel != nil {
		m.logCancel()
	}
	m.logGen++
	m.logLines, m.logScroll = nil, 0
	ctx, cancel := context.WithCancel(context.Background())
	m.logCancel = cancel
	ch := make(chan string, 256)
	m.logCh = ch
	cmd := m.st.LogsCmd(ctx, m.logSvc, m.logFollow, 300)
	go func() {
		defer close(ch)
		out, err := cmd.StdoutPipe()
		if err != nil {
			ch <- "! " + err.Error()
			return
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			ch <- "! " + err.Error()
			return
		}
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			select {
			case ch <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		_ = cmd.Wait()
	}()
	return waitLog(m.logGen, ch)
}

func waitLog(gen int, ch chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return logEndMsg{gen: gen}
		}
		return logLineMsg{gen: gen, line: line}
	}
}

// ───────────────────────── update ─────────────────────────

// Init starts the refresh loops.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.fetchFast(), m.fetchSlow(false), tick(), slowTick())
}

// Update handles one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.fetchFast(), tick())
	case slowTickMsg:
		return m, tea.Batch(m.fetchSlow(false), slowTick())
	case fastMsg:
		if msg.err != nil {
			m.fastErr = msg.err.Error()
		} else {
			keep := m.selectedService()
			m.fastErr, m.services = "", msg.services
			m.selectService(keep)
			m.sampleNet(time.Now())
		}
		m.cpu = sysinfo.CPUPercent(m.sys, msg.sys)
		m.sys, m.updated = msg.sys, time.Now()
		m.sample()
		m.clampSel()
		return m, nil
	case slowMsg:
		m.versions, m.backups, m.infra = msg.versions, msg.backups, msg.infra
		m.slowErr = ""
		if msg.err != nil {
			m.slowErr = msg.err.Error()
		}
		m.clampSel()
		return m, nil
	case doctorMsg:
		r := doctor.Report(msg)
		m.report, m.doctorAt, m.busy = &r, time.Now(), ""
		return m, nil
	case logLineMsg:
		if msg.gen != m.logGen {
			return m, nil
		}
		m.logLines = append(m.logLines, msg.line)
		if len(m.logLines) > maxLog {
			m.logLines = m.logLines[len(m.logLines)-maxLog:]
		}
		return m, waitLog(msg.gen, m.logCh)
	case logEndMsg:
		return m, nil
	case actionDoneMsg:
		m.busy = ""
		m.output, m.outputErr = strings.TrimRight(msg.out, "\n"), msg.err != nil
		if msg.err != nil {
			m.output = strings.TrimSpace(m.output + "\n✗ " + msg.err.Error())
		}
		if m.output == "" {
			m.output = "✓ done"
		}
		m.prompt, m.mode = msg.title, modeOutput
		if m.tab == tabEnv {
			m.loadEnv()
		}
		return m, tea.Batch(m.fetchFast(), m.fetchSlow(false))
	case tea.MouseMsg:
		return m.mouse(msg)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// sample keeps the last minutes of the four overview numbers for the sparklines.
func (m *Model) sample() {
	push := func(k string, v float64) {
		h := append(m.hist[k], v)
		if len(h) > histLen {
			h = h[len(h)-histLen:]
		}
		m.hist[k] = h
	}
	push("cpu", m.cpu)
	if m.sys.MemTotal > 0 {
		push("mem", 100*float64(m.sys.MemTotal-m.sys.MemAvail)/float64(m.sys.MemTotal))
	}
	push("net", m.netRx+m.netTx)
	up, exp := m.healthCount()
	if exp > 0 {
		push("up", 100*float64(up)/float64(exp))
	}
}

func (m *Model) clampSel() {
	for t, n := range map[tab]int{tabServices: len(m.visibleServices()), tabVersions: len(m.versions), tabBackups: len(m.backups), tabEnv: len(m.env)} {
		if m.sel[t] >= n {
			m.sel[t] = max(0, n-1)
		}
	}
}

func (m Model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode != modeNormal && m.mode != modeOutput {
		return m, nil
	}
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y == tabBarRow:
		if t, ok := tabAt(msg.X); ok {
			m.mode = modeNormal
			return m.switchTab(t)
		}
	case msg.Button == tea.MouseButtonWheelUp:
		return m.move(-3), nil
	case msg.Button == tea.MouseButtonWheelDown:
		return m.move(3), nil
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y >= m.listTopRow():
		row := msg.Y - m.listTopRow()
		switch m.tab {
		case tabServices, tabVersions, tabBackups, tabEnv:
			if row < m.listLen()-m.listOffset() {
				m.sel[m.tab] = row + m.listOffset()
				if m.tab == tabEnv && msg.X < envCheckWidth {
					return m.envToggle(), nil
				}
			}
		}
	}
	return m, nil
}

func (m Model) listLen() int {
	switch m.tab {
	case tabServices:
		return len(m.visibleServices())
	case tabVersions:
		return len(m.versions)
	case tabBackups:
		return len(m.backups)
	case tabEnv:
		return len(m.env)
	}
	return 0
}

func (m Model) move(d int) Model {
	switch m.tab {
	case tabLogs:
		m.logScroll = max(0, min(m.logScroll-d, max(0, len(m.filteredLogs())-1)))
		return m
	case tabDoctor:
		m.docScroll = max(0, m.docScroll+d)
		return m
	}
	n := m.listLen()
	if n == 0 {
		return m
	}
	m.sel[m.tab] = max(0, min(n-1, m.sel[m.tab]+d))
	return m
}

func (m Model) switchTab(t tab) (tea.Model, tea.Cmd) {
	if t == m.tab {
		return m, nil
	}
	m.tab = t
	switch t {
	case tabEnv:
		m.loadEnv()
	case tabLogs:
		if m.logCh == nil {
			return m, m.startLogs()
		}
	case tabDoctor:
		if m.report == nil && m.busy == "" {
			m.busy = "doctor"
			return m, m.runDoctor()
		}
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.mode {
	case modeConfirm:
		switch k {
		case "y", "Y", "enter":
			a := m.confirm
			m.mode, m.confirm, m.busy = modeNormal, nil, a.title
			return m, runAction(a)
		case "a", "A":
			if a := m.confirm.alt; a != nil {
				m.mode, m.confirm, m.busy = modeNormal, nil, a.title
				return m, runAction(a)
			}
		case "n", "N", "esc", "q":
			m.mode, m.confirm = modeNormal, nil
		}
		return m, nil
	case modeInput:
		switch k {
		case "esc":
			m.mode, m.filtering, m.envKey = modeNormal, false, ""
			m.input.CharLimit = 64
			return m, nil
		case "enter":
			v := strings.TrimSpace(m.input.Value())
			m.mode = modeNormal
			if key := m.envKey; key != "" {
				m.envKey = ""
				m.input.CharLimit = 64
				return m.envWrite(key, m.envSet(key, v)), nil
			}
			if m.filtering {
				m.filtering, m.logFilter, m.logScroll = false, v, 0
				return m, nil
			}
			if v == "" || m.onInput == nil {
				return m, nil
			}
			if a := m.onInput(v); a != nil {
				m.confirm, m.mode = a, modeConfirm
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	case modeOutput:
		if k == "esc" || k == "enter" || k == "q" {
			m.mode = modeNormal
		}
		return m, nil
	}
	if m.searching {
		return m.searchKey(msg)
	}
	if m.busy != "" && m.busy != "doctor" && k != "ctrl+c" && k != "q" {
		return m, nil
	}
	switch k {
	case "ctrl+c", "q":
		if m.logCancel != nil {
			m.logCancel()
		}
		return m, tea.Quit
	case "tab", "right", "L":
		return m.switchTab((m.tab + 1) % tab(len(tabNames)))
	case "shift+tab", "left", "H":
		return m.switchTab((m.tab + tab(len(tabNames)) - 1) % tab(len(tabNames)))
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return m.switchTab(tab(k[0] - '1'))
	case "up", "k":
		return m.move(-1), nil
	case "down", "j":
		return m.move(1), nil
	case "pgup":
		return m.move(-10), nil
	case "pgdown":
		return m.move(10), nil
	case "R":
		m.status = "refreshing…"
		if m.tab == tabEnv {
			m.loadEnv()
		}
		return m, tea.Batch(m.fetchFast(), m.fetchSlow(m.tab == tabInfra))
	}
	return m.tabKey(k)
}

func (m *Model) ask(prompt, placeholder string, onInput func(string) *action) tea.Cmd {
	m.mode, m.prompt, m.onInput = modeInput, prompt, onInput
	m.input.SetValue("")
	m.input.Placeholder = placeholder
	return m.input.Focus()
}

func (m Model) tabKey(k string) (tea.Model, tea.Cmd) {
	st := m.st
	switch m.tab {
	case tabServices:
		if len(m.services) == 0 {
			return m, nil
		}
		if k == "/" {
			m.searching = true
			return m, m.svcSearch.Focus()
		}
		vis := m.visibleServices()
		if len(vis) == 0 {
			return m, nil
		}
		svc := vis[m.sel[tabServices]].Name
		switch k {
		case "enter", "l":
			m.logSvc = svc
			m.tab = tabLogs
			return m, m.startLogs()
		case "s":
			m.confirm, m.mode = &action{title: "Start " + svc, label: svc + " only (and what it depends on)",
				run:      func(ctx context.Context, out io.Writer) error { return st.StartService(ctx, out, svc) },
				alt:      &action{title: "Pull and start the whole stack", run: st.Start},
				altLabel: fmt.Sprintf("the whole stack (%d services)", len(m.services))}, modeConfirm
		case "x":
			m.confirm, m.mode = &action{title: "Stop " + svc, label: svc + " only",
				run:      func(ctx context.Context, out io.Writer) error { return st.Stop(ctx, out, svc) },
				alt:      &action{title: "Stop the whole stack (containers and data stay)", run: st.StopAll},
				altLabel: fmt.Sprintf("the whole stack (%d services)", len(m.services))}, modeConfirm
		case "X":
			m.confirm, m.mode = &action{title: "Stop the whole stack (containers and data stay)", run: st.StopAll}, modeConfirm
		case "o":
			m.confirm, m.mode = &action{title: "Reload the proxies (" + strings.Join(st.Proxies(), ", ") + ")", run: st.Reload}, modeConfirm
		case "r":
			m.confirm, m.mode = &action{title: "Restart " + svc, run: func(ctx context.Context, out io.Writer) error { return st.Restart(ctx, out, svc, true) }}, modeConfirm
		case "p":
			m.confirm, m.mode = &action{title: "Pull and restart " + svc, run: func(ctx context.Context, out io.Writer) error { return st.Restart(ctx, out, svc, false) }}, modeConfirm
		case "S":
			m.confirm, m.mode = &action{title: "Pull and start the whole stack", run: st.Start}, modeConfirm
		}
	case tabVersions:
		if len(m.versions) == 0 {
			return m, nil
		}
		v := m.versions[m.sel[tabVersions]]
		switch k {
		case "p":
			return m, m.ask("Pin "+v.Component+" to version", v.Pinned, func(tag string) *action {
				return &action{title: "Pin " + v.Component + " → " + tag, run: func(ctx context.Context, out io.Writer) error { return st.Pin(ctx, out, v.Component, tag) }}
			})
		case "a":
			return m, m.ask("Pin EVERY enabled component to version", v.Pinned, func(tag string) *action {
				return &action{title: "Pin all → " + tag, run: func(ctx context.Context, out io.Writer) error { return st.Pin(ctx, out, "all", tag) }}
			})
		case "b":
			if v.Previous == "" {
				m.status = "no earlier version recorded for " + v.Component
				return m, nil
			}
			m.confirm, m.mode = &action{title: "Roll back " + v.Component + " → " + v.Previous, run: func(ctx context.Context, out io.Writer) error { return st.Rollback(ctx, out, v.Component) }}, modeConfirm
		case "c":
			m.busy = "checking " + v.Component + ":" + v.Pinned
			ref := v.Image + ":" + v.Pinned
			return m, runAction(&action{title: "Registry check", run: func(ctx context.Context, out io.Writer) error {
				if st.ImageExists(ctx, ref) {
					_, err := io.WriteString(out, "✓ "+ref+" is published and readable\n")
					return err
				}
				return errString(ref + " not found or not readable (released? runsnip login?)")
			}})
		}
	case tabLogs:
		switch k {
		case "f":
			m.logFollow = !m.logFollow
			return m, m.startLogs()
		case "[", "]":
			names := append([]string{""}, m.serviceNames()...)
			i := indexOf(names, m.logSvc)
			if k == "]" {
				i = (i + 1) % len(names)
			} else {
				i = (i + len(names) - 1) % len(names)
			}
			m.logSvc = names[i]
			return m, m.startLogs()
		case "/":
			m.filtering = true
			cmd := m.ask("Filter log lines containing (empty = all)", "", nil)
			m.input.SetValue(m.logFilter)
			return m, cmd
		case "x":
			m.logFilter = ""
		case "c":
			m.logLines, m.logScroll = nil, 0
		case "g":
			m.logScroll = max(0, len(m.filteredLogs())-1)
		case "G":
			m.logScroll = 0
		}
	case tabDoctor:
		if k == "d" || k == "enter" {
			if m.busy == "doctor" {
				return m, nil
			}
			m.busy, m.docScroll = "doctor", 0
			return m, m.runDoctor()
		}
	case tabBackups:
		if k == "n" {
			m.confirm, m.mode = &action{title: "Back up now", run: func(ctx context.Context, out io.Writer) error {
				_, err := st.RunBackup(ctx, out, time.Now())
				return err
			}}, modeConfirm
		}
	case tabEnv:
		if len(m.env) == 0 {
			return m, nil
		}
		v := m.env[m.sel[tabEnv]]
		switch k {
		case " ", "x":
			return m.envToggle(), nil
		case "enter", "e":
			m.envKey = v.Key
			cmd := m.ask("Set "+v.Key+" (written to .env, turned on)", v.Example, nil)
			m.input.CharLimit = 4096
			m.input.SetValue(v.Value)
			return m, cmd
		case "g":
			cmd := m.generate()
			return m, cmd
		case "s":
			m.reveal = !m.reveal
		}
	case tabInfra:
		switch k {
		case "f":
			m.status = "fetching the infra branch…"
			return m, m.fetchSlow(true)
		case "u":
			m.confirm, m.mode = &action{title: "Pull the infra branch", run: st.InfraPull}, modeConfirm
		}
	}
	return m, nil
}

// generate is `env generate` from the Env tab.
func (m *Model) generate() tea.Cmd {
	st := m.st
	m.confirm, m.mode = &action{title: "Fill every generated value .env lacks (runsnip env generate)", run: func(_ context.Context, out io.Writer) error {
		env, err := st.Env()
		if err != nil {
			if !stack.IsMissingEnv(err) {
				return err
			}
			if err := envfile.WriteAtomic(st.EnvPath, nil); err != nil {
				return err
			}
			if env, err = st.Env(); err != nil {
				return err
			}
		}
		res, err := keygen.Fill(env, st.Def.EnvRules, out)
		for _, w := range res.Warnings {
			fmt.Fprintln(out, "⚠ "+w)
		}
		return err
	}}, modeConfirm
	return nil
}

// StartOnEnv opens the dashboard on the Env tab: .env does not exist yet and is created (mode 600) on the first change.
func (m Model) StartOnEnv() Model {
	m.tab = tabEnv
	m.loadEnv()
	m.status = "no .env yet — turn variables on here (g fills every generated value); it is created with mode 600"
	return m
}

func (m *Model) sampleNet(now time.Time) {
	var rx, tx uint64
	for _, v := range m.services {
		rx += v.NetRx
		tx += v.NetTx
	}
	if !m.netPrevAt.IsZero() {
		if dt := now.Sub(m.netPrevAt).Seconds(); dt > 0 {
			m.netRx = max(0, float64(rx)-float64(m.netPrevRx)) / dt
			m.netTx = max(0, float64(tx)-float64(m.netPrevTx)) / dt
			m.netAt = now
		}
	}
	m.netPrevRx, m.netPrevTx, m.netPrevAt = rx, tx, now
}

func (m Model) searchKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "enter", "tab":
		m.searching = false
		m.svcSearch.Blur()
		return m, nil
	case "esc":
		m.searching, m.svcFilter = false, ""
		m.svcSearch.SetValue("")
		m.svcSearch.Blur()
		return m, nil
	case "up", "down", "pgup", "pgdown":
		d := map[string]int{"up": -1, "down": 1, "pgup": -10, "pgdown": 10}[msg.String()]
		return m.move(d), nil
	}
	var cmd tea.Cmd
	m.svcSearch, cmd = m.svcSearch.Update(msg)
	if v := strings.TrimSpace(m.svcSearch.Value()); v != m.svcFilter {
		m.svcFilter, m.sel[tabServices] = v, 0
	}
	return m, cmd
}

func (m Model) visibleServices() []stack.Service {
	if m.svcFilter == "" {
		return m.services
	}
	var out []stack.Service
	for _, v := range m.services {
		if strings.Contains(strings.ToLower(v.Name), strings.ToLower(m.svcFilter)) {
			out = append(out, v)
		}
	}
	return out
}

func (m Model) selectedService() string {
	vis := m.visibleServices()
	if i := m.sel[tabServices]; i < len(vis) {
		return vis[i].Name
	}
	return ""
}

func (m *Model) selectService(name string) {
	for i, v := range m.visibleServices() {
		if v.Name == name {
			m.sel[tabServices] = i
			return
		}
	}
}

const envCheckWidth = 5

func (m *Model) loadEnv() {
	m.env, m.envErr = nil, ""
	model, err := envmodel.Build(m.st)
	if err != nil {
		m.envErr = err.Error()
		return
	}
	m.env = model.Vars
	m.clampSel()
}

func (m Model) envSet(key, value string) error {
	_, err := envmodel.Apply(m.st, envmodel.Change{Set: map[string]string{key: value}, Order: []string{key}})
	return err
}

func (m Model) envToggle() Model {
	v := m.env[m.sel[tabEnv]]
	c := envmodel.Change{On: []string{v.Key}}
	if v.Enabled {
		c = envmodel.Change{Off: []string{v.Key}}
	} else if !v.InEnv {
		c = envmodel.Change{Set: map[string]string{v.Key: v.Value}, Order: []string{v.Key}}
	}
	_, err := envmodel.Apply(m.st, c)
	return m.envWrite(v.Key, err)
}

func (m Model) envWrite(key string, err error) Model {
	if err != nil {
		m.status = "✗ " + err.Error()
		return m
	}
	m.loadEnv()
	m.status = "✓ " + key + " saved in .env · applies when the services reading it are recreated (r in Services)"
	for _, v := range m.env {
		if v.Key == key && len(v.Services) > 0 {
			m.status = "✓ " + key + " saved in .env · read by " + strings.Join(v.Services, " ") + ": r in Services recreates one"
		}
	}
	return m
}

func (m Model) filteredLogs() []string {
	if m.logFilter == "" {
		return m.logLines
	}
	var out []string
	for _, l := range m.logLines {
		if strings.Contains(strings.ToLower(l), strings.ToLower(m.logFilter)) {
			out = append(out, l)
		}
	}
	return out
}

func (m Model) serviceNames() []string {
	out := make([]string, 0, len(m.services))
	for _, s := range m.services {
		out = append(out, s.Name)
	}
	return out
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return 0
}

func diskDir(st *stack.Stack) string {
	if _, err := os.Stat(st.DataDir()); err == nil {
		return st.DataDir()
	}
	return st.Dir
}

type errString string

func (e errString) Error() string { return string(e) }
