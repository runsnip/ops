package tui

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/runsnip/ops/internal/proc"
	"github.com/runsnip/ops/internal/release"
)

// BuildModel is `runsnip build`: pick components on the build machine, release them through the project's build
// script (and its infra publisher), and watch the CI runs that turn the bundles into images.
type BuildModel struct {
	repo  *release.Repo
	w, h  int
	tab   int // 0 Components, 1 Build, 2 Actions
	items []string
	sel   int

	st      release.State
	loading bool
	picked  map[string]bool
	touched bool // the user changed the selection: the first inspection must not overwrite it
	dry     bool
	verify  bool

	running  bool
	lines    []string
	scroll   int
	status   map[string]string // component → queued | building | pushed | dry-run | failed
	stepGen  int
	ch       chan string
	cancel   context.CancelFunc
	kill     func()
	runs     []release.Run
	runsErr  string
	runsAt   time.Time
	mode     mode
	prompt   string
	input    textinput.Model
	confirmT string
	bumpTo   map[string]string // confirm sets these component versions; empty = confirm starts a run
	errc     chan error
	note     string
}

var buildTabs = []string{"Components", "Build", "Actions"}

// NewBuild returns the build dashboard for a repository checkout.
func NewBuild(repo *release.Repo) BuildModel {
	ti := textinput.New()
	ti.CharLimit = 20
	return BuildModel{repo: repo, w: 120, h: 40, items: []string{release.Infra}, picked: map[string]bool{}, status: map[string]string{}, loading: true, input: ti}
}

// DryRun starts with the dry run on.
func (m BuildModel) DryRun() BuildModel { m.dry = true; return m }

// Pick preselects a component (the first inspection keeps it).
func (m BuildModel) Pick(c string) BuildModel { m.picked[c], m.touched = true, true; return m }

type inspectMsg release.State
type runsMsg struct {
	runs []release.Run
	err  error
}
type buildLineMsg struct {
	gen  int
	line string
}
type buildEndMsg struct {
	gen int
	err error
}
type runsTickMsg time.Time
type bumpDoneMsg struct{ err error }

func (m BuildModel) inspect() tea.Cmd {
	repo := m.repo
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return inspectMsg(repo.Inspect(ctx))
	}
}

func (m BuildModel) fetchRuns() tea.Cmd {
	repo := m.repo
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		runs, err := repo.Runs(ctx)
		return runsMsg{runs, err}
	}
}

func runsTick() tea.Cmd {
	return tea.Tick(10*time.Second, func(t time.Time) tea.Msg { return runsTickMsg(t) })
}

// Init inspects the repository and starts polling Actions.
func (m BuildModel) Init() tea.Cmd { return tea.Batch(m.inspect(), m.fetchRuns(), runsTick()) }

func (m BuildModel) components() []string {
	var out []string
	for _, c := range m.st.Components {
		if m.picked[c] {
			out = append(out, c)
		}
	}
	return out
}

// steps are the commands a run executes, in order.
func (m BuildModel) steps() [][]string {
	return m.repo.Steps(m.components(), m.picked[release.Infra], m.dry, m.verify)
}

// start runs the steps one after another, streaming their output.
func (m *BuildModel) start() tea.Cmd {
	m.stepGen++
	m.running, m.lines, m.scroll, m.tab = true, nil, 0, 1
	m.status = map[string]string{}
	for _, c := range m.components() {
		m.status[c] = "queued"
	}
	if m.picked[release.Infra] {
		m.status[release.Infra] = "queued"
	}
	ctx, cancel := context.WithCancel(context.Background())
	group := &atomic.Int64{}
	m.cancel = cancel // `s`: SIGTERM, then SIGKILL after a grace period (proc.Command)
	m.kill = func() { // quitting: nothing would be left to send the SIGKILL, and a stuck docker ignores SIGTERM
		cancel()
		proc.Kill(int(group.Load()))
	}
	ch := make(chan string, 512)
	m.ch = ch
	steps, dir, gen := m.steps(), m.repo.Dir, m.stepGen
	errc := make(chan error, 1)
	m.errc = errc
	go func() {
		defer close(ch)
		for _, s := range steps {
			ch <- "$ " + strings.Join(s, " ")
			cmd := proc.Command(ctx, s[0], s[1:]...)
			cmd.Dir = dir
			out, err := cmd.StdoutPipe()
			if err != nil {
				errc <- err
				return
			}
			cmd.Stderr = cmd.Stdout
			if err := cmd.Start(); err != nil {
				errc <- err
				return
			}
			group.Store(int64(cmd.Process.Pid))
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				ch <- sc.Text()
			}
			if err := cmd.Wait(); err != nil {
				errc <- fmt.Errorf("%s: %w", strings.Join(s[:2], " "), err)
				return
			}
		}
		errc <- nil
	}()
	return waitBuild(gen, ch, errc)
}

func waitBuild(gen int, ch chan string, errc chan error) tea.Cmd {
	return func() tea.Msg {
		if line, ok := <-ch; ok {
			return buildLineMsg{gen, line}
		}
		return buildEndMsg{gen, <-errc}
	}
}

// Update handles one message.
func (m BuildModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case inspectMsg:
		first := m.st.Released == nil
		m.st, m.loading = release.State(msg), false
		m.items = append(append([]string{}, m.st.Components...), release.Infra)
		m.sel = min(m.sel, len(m.items)-1)
		if first && !m.touched { // preselect what changed since its last release
			for c, files := range m.st.Changed {
				m.picked[c] = len(files) > 0
			}
		}
	case runsMsg:
		m.runs, m.runsAt = msg.runs, time.Now()
		m.runsErr = ""
		if msg.err != nil {
			m.runsErr = msg.err.Error()
		}
	case runsTickMsg:
		return m, tea.Batch(m.fetchRuns(), runsTick())
	case buildLineMsg:
		if msg.gen != m.stepGen {
			return m, nil
		}
		m.lines = append(m.lines, msg.line)
		if len(m.lines) > 5000 {
			m.lines = m.lines[len(m.lines)-5000:]
		}
		reStep, rePushed := m.repo.Progress()
		switch {
		case reStep.MatchString(msg.line):
			m.markBuilding(reStep.FindStringSubmatch(msg.line)[1])
		case rePushed.MatchString(msg.line):
			m.status[rePushed.FindStringSubmatch(msg.line)[1]] = "pushed"
		case strings.Contains(msg.line, "would push deploy/") || strings.HasPrefix(strings.TrimSpace(msg.line), "would push"):
			m.markCurrent("dry-run ok")
		case strings.HasPrefix(msg.line, "$ "+strings.Join(m.repo.PublishInfraCmd(), " ")):
			m.status[release.Infra] = "building"
		}
		return m, waitBuild(msg.gen, m.ch, m.errc)
	case buildEndMsg:
		if msg.gen != m.stepGen {
			return m, nil
		}
		m.running = false
		if msg.err != nil {
			m.lines = append(m.lines, "✗ "+msg.err.Error())
			for c, s := range m.status {
				if s == "building" {
					m.status[c] = "failed"
				}
			}
		} else {
			if m.status[release.Infra] == "building" {
				m.status[release.Infra] = map[bool]string{true: "dry-run ok", false: "pushed"}[m.dry]
			}
			m.lines = append(m.lines, "✓ finished")
		}
		m.note = "run ended — d toggles dry run, enter runs again, 1 goes back to components"
		return m, tea.Batch(m.inspect(), m.fetchRuns())
	case bumpDoneMsg:
		m.mode = modeNormal
		if msg.err != nil {
			m.note = "✗ " + msg.err.Error()
		} else {
			m.note = "✓ version committed and pushed"
		}
		m.loading = true
		return m, m.inspect()
	case tea.MouseMsg:
		if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y == tabBarRow {
			if t, ok := buildTabAt(msg.X); ok {
				m.tab = t
			}
		} else if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && m.tab == 0 && msg.Y >= listTop && msg.Y-listTop < len(m.items) {
			m.sel = msg.Y - listTop
			m.picked[m.items[m.sel]] = !m.picked[m.items[m.sel]]
			m.touched = true
		} else if msg.Button == tea.MouseButtonWheelUp && m.tab == 1 {
			m.scroll = min(m.scroll+3, max(0, len(m.lines)-1))
		} else if msg.Button == tea.MouseButtonWheelDown && m.tab == 1 {
			m.scroll = max(0, m.scroll-3)
		}
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// markBuilding: the previous building component (dry run: no push line) is done once the next one starts.
func (m *BuildModel) markBuilding(c string) {
	for k, s := range m.status {
		if s == "building" && k != release.Infra {
			if m.dry {
				m.status[k] = "dry-run ok"
			} else {
				m.status[k] = "pushed"
			}
		}
	}
	m.status[c] = "building"
}

func (m *BuildModel) markCurrent(s string) {
	for k, v := range m.status {
		if v == "building" && k != release.Infra {
			m.status[k] = s
		}
	}
}

func (m BuildModel) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.mode {
	case modeConfirm:
		switch k {
		case "y", "Y", "enter":
			m.mode = modeNormal
			if vs := m.bumpTo; len(vs) > 0 {
				repo := m.repo
				m.bumpTo = nil
				return m, func() tea.Msg { return bumpDoneMsg{repo.SetVersions(context.Background(), vs)} }
			}
			return m, m.start()
		case "n", "N", "esc":
			m.mode = modeNormal
		}
		return m, nil
	case modeInput:
		switch k {
		case "esc":
			m.mode = modeNormal
			return m, nil
		case "enter":
			v := strings.TrimSpace(m.input.Value())
			m.mode = modeNormal
			if v == "" {
				return m, nil
			}
			// Each picked component moves from its own version (patch/minor/major), or all to the one typed.
			bumps, parts := map[string]string{}, []string{}
			for _, c := range m.components() {
				nv := v
				if v == "patch" || v == "minor" || v == "major" {
					var err error
					if nv, err = release.Bump(m.st.Versions[c], v); err != nil {
						m.note = "✗ " + c + ": " + err.Error()
						return m, nil
					}
				}
				bumps[c] = nv
				parts = append(parts, fmt.Sprintf("%s %s → %s", c, dash(m.st.Versions[c]), nv))
			}
			m.confirmT = "Set " + strings.Join(parts, ", ") + "; commit the version files alone and push main"
			m.bumpTo, m.mode = bumps, modeConfirm
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	switch k {
	case "ctrl+c", "q":
		if m.kill != nil {
			m.kill()
		}
		return m, tea.Quit
	case "tab", "right":
		m.tab = (m.tab + 1) % len(buildTabs)
	case "shift+tab", "left":
		m.tab = (m.tab + len(buildTabs) - 1) % len(buildTabs)
	case "1", "2", "3":
		m.tab = int(k[0] - '1')
	case "R":
		m.loading = true
		return m, tea.Batch(m.inspect(), m.fetchRuns())
	}
	// Run options work from every tab (after a run you are on Build), but not mid-run.
	if m.running {
		switch k {
		case "a", "n", "c", "d", "V", "v":
			m.note = "a run is in progress — wait for it, or s on the Build tab to stop it"
		}
	} else {
		switch k {
		case "a":
			for _, c := range m.st.Components {
				m.picked[c] = true
			}
			m.touched = true
		case "n":
			m.picked, m.touched = map[string]bool{}, true
		case "c":
			for c, f := range m.st.Changed {
				m.picked[c] = len(f) > 0
			}
			m.touched = true
		case "d":
			m.dry = !m.dry
			m.note = map[bool]string{true: "dry run on — enter builds, pushes nothing", false: "dry run off — enter releases for real"}[m.dry]
		case "V":
			m.verify = !m.verify
		case "v":
			comps := m.components()
			if len(comps) == 0 {
				m.note = "pick the components whose version to raise first (space)"
				return m, nil
			}
			now := make([]string, len(comps))
			for i, c := range comps {
				now[i] = c + " " + dash(m.st.Versions[c])
			}
			m.mode, m.prompt = modeInput, "New version for "+strings.Join(now, ", ")+" — or patch / minor / major (each from its own)"
			m.input.SetValue("")
			m.input.Placeholder = "patch"
			return m, m.input.Focus()
		}
	}
	switch m.tab {
	case 0:
		switch k {
		case "up", "k":
			m.sel = max(0, m.sel-1)
		case "down", "j":
			m.sel = min(len(m.items)-1, m.sel+1)
		case " ", "x":
			m.picked[m.items[m.sel]] = !m.picked[m.items[m.sel]]
			m.touched = true
		case "enter":
			return m.askStart()
		}
	case 1:
		switch k {
		case "up", "k":
			m.scroll = min(m.scroll+1, max(0, len(m.lines)-1))
		case "down", "j":
			m.scroll = max(0, m.scroll-1)
		case "G":
			m.scroll = 0
		case "s":
			if m.running && m.cancel != nil {
				m.cancel()
				m.lines = append(m.lines, "! stopped by you")
			}
		case "enter":
			return m.askStart()
		}
	}
	return m, nil
}

func (m BuildModel) askStart() (tea.Model, tea.Cmd) {
	if m.running {
		m.note = "a run is in progress — wait for it, or s on the Build tab to stop it"
		return m, nil
	}
	if m.loading {
		m.note = "still reading the repository — try again in a moment"
		return m, nil
	}
	if len(m.steps()) == 0 {
		m.note = "nothing selected"
		return m, nil
	}
	if !m.dry {
		var stale []string
		for _, c := range m.components() {
			if m.st.NeedsBump(c) {
				stale = append(stale, c+" "+m.st.Versions[c])
			}
		}
		if len(stale) > 0 {
			m.note = "✗ changed since released at the same version: " + strings.Join(stale, ", ") + " — v raises it (d = dry run works anyway)"
			return m, nil
		}
		if len(m.st.Problems) > 0 {
			m.note = "✗ cannot release: " + strings.Join(m.st.Problems, "; ") + " (d = dry run works anyway)"
			return m, nil
		}
	}
	var named []string
	for _, c := range m.components() {
		named = append(named, c+" v"+m.st.Versions[c])
	}
	what := strings.Join(named, ", ")
	if m.picked[release.Infra] {
		what = strings.TrimSpace(what + " + infra branch")
	}
	kind := "Release"
	if m.dry {
		kind = "Dry run (build, push nothing)"
	}
	if m.verify && len(m.repo.Def.Release.Verify) > 0 {
		kind += ", after " + strings.Join(m.repo.Def.Release.Verify, " ") + ","
	}
	m.confirmT, m.bumpTo, m.mode = kind+": "+what, nil, modeConfirm
	return m, nil
}

// View renders the screen.
func (m BuildModel) View() string {
	var b strings.Builder
	left := sTitle.Render("runsnip build") + sMuted.Render(fmt.Sprintf(" · %s@%s · %s", m.st.Branch, m.st.Commit, m.repo.Dir))
	right := ""
	if m.running {
		right = sWarn.Render("⟳ running…")
	} else if m.loading {
		right = sMuted.Render("checking…")
	}
	b.WriteString(left + strings.Repeat(" ", max(1, m.w-lipgloss.Width(left)-lipgloss.Width(right))) + right + "\n")
	parts := make([]string, len(buildTabs))
	for i, n := range buildTabs {
		label := fmt.Sprintf("%d %s", i+1, n)
		if i == m.tab {
			parts[i] = sTabOn.Render(label)
		} else {
			parts[i] = sTabOff.Render(label)
		}
	}
	b.WriteString(strings.Join(parts, " ") + "\n")
	b.WriteString(sMuted.Render(strings.Repeat("─", max(10, m.w))) + "\n")
	bodyH := max(5, m.h-5)
	var body string
	switch m.tab {
	case 0:
		body = m.viewComponents()
	case 1:
		body = m.viewBuild(bodyH)
	default:
		body = m.viewRuns()
	}
	lines := strings.Split(body, "\n")
	if len(lines) > bodyH {
		lines = lines[:bodyH]
	}
	for len(lines) < bodyH {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = clip(lines[i], m.w)
	}
	b.WriteString(strings.Join(lines, "\n") + "\n")
	keys := map[int]string{
		0: "space pick · a all · c changed · n none · d dry run · V verify first · v version · enter run · R reload",
		1: "↑↓ scroll · G bottom · s stop · d dry run · enter run again · 1 back to components",
		2: "refreshes every 10 s · R now",
	}[m.tab] + " · ←→ tabs · q quit"
	if m.note != "" {
		keys = m.note + "  " + keys
	}
	b.WriteString(sMuted.Render(clip(keys, m.w)))
	out := b.String()
	if m.mode == modeConfirm || m.mode == modeInput {
		title, inner := m.confirmT+"?", "y / enter = go   ·   n / esc = cancel"
		if m.mode == modeInput {
			title, inner = m.prompt, m.input.View()+"\n\n"+sMuted.Render("enter = ok · esc = cancel")
		}
		box := sBox.Width(min(max(40, m.w-10), 100)).Render(sTitle.Render(title) + "\n\n" + inner)
		out = lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box)
	}
	return out
}

func (m BuildModel) viewComponents() string {
	var b strings.Builder
	b.WriteString(sHead.Render(fmt.Sprintf("    %-16s %-10s %-10s %-12s %s", "COMPONENT", "VERSION", "RELEASED", "STATUS", "CHANGED SINCE ITS RELEASE")) + "\n")
	for i, c := range m.items {
		box := "[ ]"
		if m.picked[c] {
			box = sOK.Render("[x]")
		}
		rel := m.st.Released[c].Version
		if c == release.Infra {
			rel = "branch"
		}
		changed := m.st.Changed[c]
		ch := sMuted.Render("no changes")
		if len(changed) > 0 {
			ch = sWarn.Render(fmt.Sprintf("%d file(s)", len(changed))) + sMuted.Render(" "+strings.Join(changed[:min(3, len(changed))], ", "))
			if changed[0] == "(never released)" {
				ch = sWarn.Render("never released")
			}
		}
		ver := fmt.Sprintf("%-10s", dash(m.st.Versions[c]))
		if m.st.NeedsBump(c) {
			ver = sWarn.Render(ver)
			ch = sWarn.Render("raise the version (v) · ") + ch
		}
		status := m.status[c]
		line := fmt.Sprintf(" %s %-16s %s %-10s %-12s %s", box, c, ver, dash(rel), statusText(status), ch)
		if i == m.sel {
			line = sSel.Render(clip(line, m.w))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	opts := []string{}
	if m.dry {
		opts = append(opts, sWarn.Render("dry run: builds bundles, pushes nothing"))
	} else {
		opts = append(opts, "release ("+strings.Join(m.repo.BuildCmd(), " ")+" <components>, then CI builds the images)")
	}
	if m.verify && len(m.repo.Def.Release.Verify) > 0 {
		opts = append(opts, strings.Join(m.repo.Def.Release.Verify, " ")+" first")
	}
	b.WriteString(sHead.Render("RUN  ") + strings.Join(opts, " · ") + "\n")
	b.WriteString(sHead.Render("READY") + "\n")
	check := func(ok bool, label string) string {
		if ok {
			return "  " + sOK.Render("✓") + " " + label
		}
		return "  " + sBad.Render("✗") + " " + label
	}
	b.WriteString(check(len(m.st.Dirty) == 0, fmt.Sprintf("working tree clean (%d tracked change(s))", len(m.st.Dirty))) + "\n")
	b.WriteString(check(m.st.Pushed, "HEAD "+m.st.Commit+" is on "+m.repo.Remote()+"/"+m.repo.Main()) + "\n")
	b.WriteString(check(m.st.DockerOK, "Docker with buildx") + "\n")
	b.WriteString(check(m.st.GhOK, "gh CLI signed in (Actions tab)") + "\n")
	for _, p := range m.st.Problems {
		if !strings.Contains(p, "uncommitted") && !strings.Contains(p, "not on") && !strings.Contains(p, "Docker") {
			b.WriteString("  " + sBad.Render("✗ "+p) + "\n")
		}
	}
	return b.String()
}

func statusText(s string) string {
	switch s {
	case "":
		return ""
	case "pushed", "dry-run ok":
		return sOK.Render(s)
	case "failed":
		return sBad.Render(s)
	case "building":
		return sWarn.Render(s)
	}
	return sMuted.Render(s)
}

func (m BuildModel) viewBuild(h int) string {
	var head []string
	for _, c := range m.items {
		if s := m.status[c]; s != "" {
			head = append(head, c+" "+statusText(s))
		}
	}
	out := []string{sHead.Render("STEPS ") + strings.Join(head, sMuted.Render(" · "))}
	if len(m.lines) == 0 {
		out = append(out, sMuted.Render("nothing run yet — pick components on tab 1 and press enter"))
		return strings.Join(out, "\n")
	}
	n := max(1, h-1)
	end := max(0, len(m.lines)-m.scroll)
	for _, l := range m.lines[max(0, end-n):end] {
		out = append(out, colorLog(l))
	}
	return strings.Join(out, "\n")
}

func (m BuildModel) viewRuns() string {
	var b strings.Builder
	b.WriteString(sHead.Render(fmt.Sprintf("%-16s %-10s %-24s %-8s %s", "COMPONENT", "VERSION", "STATUS", "AGE", "RUN")) + "\n")
	if m.runsErr != "" {
		return b.String() + sBad.Render(m.runsErr) + "\n" + sMuted.Render("gh auth login, then R")
	}
	if len(m.runs) == 0 {
		return b.String() + sMuted.Render("no image builds yet (the deploy branches run them)")
	}
	for _, r := range m.runs {
		st := r.Status
		style := sWarn
		if r.Status == "completed" {
			st = r.Conclusion
			style = sOK
			if r.Conclusion != "success" {
				style = sBad
			}
		}
		b.WriteString(fmt.Sprintf("%-16s %-10s %s %-8s %s\n", r.Component, r.Version, style.Render(fmt.Sprintf("%-24s", st)), age(time.Since(r.Created)), sMuted.Render(r.URL)))
	}
	if !m.runsAt.IsZero() {
		b.WriteString("\n" + sMuted.Render("updated "+m.runsAt.Format("15:04:05")+" · deploy on the server: runsnip pin <component> <version>"))
	}
	return b.String()
}

func buildTabAt(x int) (int, bool) {
	pos := 0
	for i, n := range buildTabs {
		w := lipgloss.Width(sTabOff.Render(fmt.Sprintf("%d %s", i+1, n)))
		if x >= pos && x < pos+w {
			return i, true
		}
		pos += w + 1
	}
	return 0, false
}
