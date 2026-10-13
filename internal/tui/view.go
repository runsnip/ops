package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/runsnip/ops/internal/doctor"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/sysinfo"
)

// Screen layout: row 0 header, row 1 tab bar, row 2 rule, row 3 column header, rows 4… list items.
const (
	tabBarRow = 1
	listTop   = 4
)

var (
	cBrand  = lipgloss.Color("#3B82F6")
	cMuted  = lipgloss.Color("245")
	cOK     = lipgloss.Color("42")
	cWarn   = lipgloss.Color("214")
	cBad    = lipgloss.Color("203")
	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(cBrand)
	sMuted  = lipgloss.NewStyle().Foreground(cMuted)
	sOK     = lipgloss.NewStyle().Foreground(cOK)
	sWarn   = lipgloss.NewStyle().Foreground(cWarn)
	sBad    = lipgloss.NewStyle().Foreground(cBad)
	sTabOn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(cBrand).Padding(0, 1)
	sTabOff = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Padding(0, 1)
	sSel    = lipgloss.NewStyle().Background(lipgloss.Color("236")).Bold(true)
	sHead   = lipgloss.NewStyle().Foreground(cMuted).Bold(true)
	sBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBrand).Padding(0, 1)
)

// tabAt maps a click on the tab bar to a tab (same widths as renderTabs).
func tabAt(x int) (tab, bool) {
	pos := 0
	for i, n := range tabNames {
		w := lipgloss.Width(sTabOff.Render(fmt.Sprintf("%d %s", i+1, n)))
		if x >= pos && x < pos+w {
			return tab(i), true
		}
		pos += w + 1
	}
	return 0, false
}

// View renders the whole screen.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.header() + "\n")
	b.WriteString(m.renderTabs() + "\n")
	b.WriteString(sMuted.Render(strings.Repeat("─", max(10, m.w))) + "\n")
	bodyH := max(5, m.h-5)
	body := m.body(bodyH)
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
	b.WriteString(m.footer())
	out := b.String()
	if m.mode != modeNormal {
		out = m.overlay(out)
	}
	return out
}

func (m Model) header() string {
	right := ""
	if !m.updated.IsZero() {
		right = sMuted.Render("updated " + m.updated.Format("15:04:05"))
	}
	if m.busy != "" {
		right = sWarn.Render("⟳ " + m.busy + "…")
	}
	left := sTitle.Render("runsnip") + " " + sHead.Render(m.st.Title()) + sMuted.Render(" · "+m.host+" · "+m.st.Dir)
	gap := max(1, m.w-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) renderTabs() string {
	parts := make([]string, len(tabNames))
	for i, n := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, n)
		if tab(i) == m.tab {
			parts[i] = sTabOn.Render(label)
		} else {
			parts[i] = sTabOff.Render(label)
		}
	}
	return strings.Join(parts, " ")
}

func (m Model) footer() string {
	keys := map[tab]string{
		tabOverview: "R refresh",
		tabServices: "/ search · ↑↓ select · enter logs · s start · x stop · r restart · p pull+restart · o reload proxies · S start all · X stop all",
		tabVersions: "↑↓ select · p pin · a pin all · b rollback · c check registry",
		tabLogs:     "[ ] service · f follow · / filter · x clear filter · c clear · ↑↓ scroll · g/G top/bottom",
		tabDoctor:   "d / enter run again · ↑↓ scroll",
		tabBackups:  "n back up now",
		tabInfra:    "f fetch · u pull",
		tabEnv:      "space/click [x] on·off · enter edit · g generate missing values · s show secrets",
	}[m.tab]
	s := keys + " · ←→ tabs · q quit"
	if m.status != "" {
		s = sWarn.Render(m.status) + "  " + s
	}
	return sMuted.Render(clip(s, m.w))
}

func (m Model) body(h int) string {
	switch m.tab {
	case tabServices:
		return m.viewServices(h)
	case tabVersions:
		return m.viewVersions(h)
	case tabLogs:
		return m.viewLogs(h)
	case tabBackups:
		return m.viewBackups(h)
	case tabInfra:
		return m.viewInfra()
	case tabEnv:
		return m.viewEnv(h)
	case tabDoctor:
		return m.viewDoctor(h)
	}
	return m.viewOverview()
}

// ───────────────────────── tabs ─────────────────────────

// viewOverview leads with the four numbers worth a glance (cards.go), then the host and the stack.
func (m Model) viewOverview() string {
	return m.overviewCards() + "\n" + m.overviewMain()
}

func (m Model) overviewMain() string {
	var b strings.Builder
	s := m.sys
	b.WriteString(sHead.Render("HOST") + "\n")
	if s.Supported {
		b.WriteString(fmt.Sprintf("  CPU     %s %5.1f%%  %d cores · load %.2f %.2f %.2f\n", bar(m.cpu/100, 24), m.cpu, s.CPUs, s.Load1, s.Load5, s.Load15))
		used := s.MemTotal - s.MemAvail
		b.WriteString(fmt.Sprintf("  Memory  %s %s / %s\n", bar(ratio(used, s.MemTotal), 24), sysinfo.HumanBytes(used), sysinfo.HumanBytes(s.MemTotal)))
		if s.SwapTotal > 0 {
			su := s.SwapTotal - s.SwapFree
			b.WriteString(fmt.Sprintf("  Swap    %s %s / %s\n", bar(ratio(su, s.SwapTotal), 24), sysinfo.HumanBytes(su), sysinfo.HumanBytes(s.SwapTotal)))
		}
		b.WriteString(fmt.Sprintf("  Uptime  %s\n", uptime(time.Duration(s.Uptime)*time.Second)))
	} else {
		b.WriteString(sMuted.Render(fmt.Sprintf("  %d CPUs · CPU/memory need Linux /proc", s.CPUs)) + "\n")
	}
	if s.DiskTotal > 0 {
		du := s.DiskTotal - s.DiskFree
		b.WriteString(fmt.Sprintf("  Disk    %s %s / %s %s\n", bar(ratio(du, s.DiskTotal), 24), sysinfo.HumanBytes(du), sysinfo.HumanBytes(s.DiskTotal), sMuted.Render("("+diskDir(m.st)+")")))
	}

	b.WriteString("\n" + sHead.Render("STACK") + "\n")
	if m.fastErr != "" {
		b.WriteString("  " + sBad.Render(m.fastErr) + "\n")
	}
	running, healthy, notCreated, problems := 0, 0, 0, []string{}
	for _, v := range m.services {
		switch {
		case v.State == "running":
			running++
			if v.Health == "healthy" {
				healthy++
			}
			if v.Health == "unhealthy" {
				problems = append(problems, v.Name+" unhealthy")
			}
		case v.State == "exited" && strings.Contains(v.Status, "(0)"):
			// one-shot jobs (migrate, openfga-migrate) finish with exit 0
		case v.Off != "":
			// turned off by its profile: not expected to run
		case v.State == "not created":
			notCreated++
		default:
			problems = append(problems, v.Name+" "+v.State)
		}
	}
	b.WriteString(fmt.Sprintf("  Services  %d running · %d healthy · %d total\n", running, healthy, len(m.services)))
	if notCreated == len(m.services) && notCreated > 0 {
		b.WriteString("  " + sMuted.Render("nothing started yet: S in the Services tab, or runsnip start") + "\n")
	} else if notCreated > 0 {
		b.WriteString("  " + sWarn.Render(fmt.Sprintf("! %d enabled but not created (runsnip start)", notCreated)) + "\n")
	}
	if len(problems) > 0 {
		b.WriteString("  " + sBad.Render("✗ "+strings.Join(problems, ", ")) + "\n")
	} else if running > 0 {
		b.WriteString("  " + sOK.Render("✓ nothing down") + "\n")
	}
	drift := []string{}
	for _, v := range m.versions {
		if v.Running != "" && v.Running != v.Pinned {
			drift = append(drift, fmt.Sprintf("%s %s≠%s", v.Component, v.Running, v.Pinned))
		}
	}
	if len(drift) > 0 {
		b.WriteString("  " + sWarn.Render("! running ≠ pinned: "+strings.Join(drift, ", ")+" (runsnip restart <component>)") + "\n")
	}
	if m.infra.Head != "" {
		line := fmt.Sprintf("  Infra     %s %s (%s)", m.infra.Head, m.infra.Subject, m.infra.When)
		if m.infra.Behind > 0 {
			line += sWarn.Render(fmt.Sprintf("  · %d new on the %s branch", m.infra.Behind, m.st.InfraBranch()))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(fmt.Sprintf("  Shield    %s\n", m.shieldLine()))
	if m.report != nil {
		b.WriteString(fmt.Sprintf("  Doctor    %s %s\n", doctorWord(*m.report), sMuted.Render("("+age(time.Since(m.doctorAt))+" ago · tab 5)")))
	}
	return b.String()
}

// listTopRow is the screen row of the first list item: below the column header, and below the cards on Services.
func (m Model) listTopRow() int {
	if m.tab == tabServices {
		return listTop + 1 // the search bar
	}
	return listTop
}

// listOffset is the first list row on screen, as table() scrolls it: the body (m.h-5) less the column header and the
// lines a tab keeps around its list.
func (m Model) listOffset() int {
	reserved := map[tab]int{tabVersions: 2, tabEnv: envHelpLines + 1}[m.tab]
	if m.tab == tabServices {
		reserved++
	}
	visible := max(1, m.h-5-1-reserved)
	return max(0, m.sel[m.tab]-visible+1)
}

func (m Model) viewServices(h int) string {
	rows := [][]string{}
	vis := m.visibleServices()
	for i, v := range vis {
		status := v.Status
		if v.Off != "" && v.Container == "" {
			status = v.Off
		}
		rows = append(rows, []string{fmt.Sprintf("%2d", i+1), v.Name, stateText(v), v.Health, tagOf(v.Image), v.CPU, v.Mem, v.Ports, status})
	}
	out := m.table(h-1, []string{" #", "SERVICE", "STATE", "HEALTH", "TAG", "CPU", "MEM", "PORTS", "STATUS"}, []int{2, 16, 11, 10, 12, 8, 10, 22, 0}, rows, func(i, col int, s string) string {
		v := vis[i]
		switch col {
		case 0:
			return sMuted.Render(s)
		case 2:
			return stateStyle(v).Render(s)
		case 3:
			if v.Health == "unhealthy" {
				return sBad.Render(s)
			}
			if v.Health == "healthy" {
				return sOK.Render(s)
			}
		}
		return s
	})
	return m.searchBar(len(vis)) + "\n" + out
}

// searchBar sits above the Services list: / focuses it, the list follows what is typed.
func (m Model) searchBar(shown int) string {
	count := sMuted.Render(fmt.Sprintf("%d services", len(m.services)))
	if m.svcFilter != "" {
		count = sWarn.Render(fmt.Sprintf("%d of %d", shown, len(m.services)))
	}
	if m.searching {
		return sTitle.Render("/ ") + m.svcSearch.View() + "  " + count + sMuted.Render("   enter keep · esc clear · ↑↓ move")
	}
	if m.svcFilter != "" {
		return sMuted.Render("/ ") + sWarn.Render(m.svcFilter) + "  " + count + sMuted.Render("   / edit · esc in the bar clears")
	}
	return sMuted.Render("/ search") + "  " + count
}

func (m Model) viewVersions(h int) string {
	rows := [][]string{}
	for _, v := range m.versions {
		state := "✓"
		switch {
		case v.Off != "":
			state = v.Off
		case v.Running == "":
			state = "not running"
		case v.Running != v.Pinned:
			state = "restart to apply"
		}
		rows = append(rows, []string{v.Component, dash(v.Pinned), dash(v.Running), dash(v.Previous), state})
	}
	out := m.table(h-2, []string{"COMPONENT", "PINNED", "RUNNING", "PREVIOUS", ""}, []int{18, 14, 14, 14, 0}, rows, func(i, col int, s string) string {
		if col == 4 {
			switch s {
			case "✓":
				return sOK.Render(s)
			case "restart to apply":
				return sWarn.Render(s)
			}
			return sMuted.Render(s)
		}
		return s
	})
	note := sMuted.Render("pins live in .env (<IMAGE>_TAG; the one before in <IMAGE>_TAG_PREVIOUS) · image per component: runsnip version")
	if m.slowErr != "" {
		note = sBad.Render(m.slowErr)
	}
	return out + "\n\n" + note
}

func (m Model) viewLogs(h int) string {
	svc := m.logSvc
	if svc == "" {
		svc = "all services"
	}
	follow := "follow on"
	if !m.logFollow {
		follow = "follow off"
	}
	head := sHead.Render("LOGS ") + svc + sMuted.Render(" · "+follow)
	if m.logFilter != "" {
		head += sWarn.Render(" · filter: " + m.logFilter)
	}
	if m.logScroll > 0 {
		head += sMuted.Render(fmt.Sprintf(" · ↑%d", m.logScroll))
	}
	lines := m.filteredLogs()
	n := max(1, h-1)
	end := max(0, len(lines)-m.logScroll)
	start := max(0, end-n)
	view := lines[start:end]
	out := []string{head}
	for _, l := range view {
		out = append(out, colorLog(l))
	}
	if len(lines) == 0 {
		out = append(out, sMuted.Render("waiting for log lines…"))
	}
	return strings.Join(out, "\n")
}

func (m Model) viewBackups(h int) string {
	rows := [][]string{}
	for _, v := range m.backups {
		rows = append(rows, []string{v.Name, sysinfo.HumanBytes(uint64(v.Size)), v.Time.Format("2006-01-02 15:04"), age(time.Since(v.Time))})
	}
	if len(rows) == 0 {
		return sHead.Render("FILE") + "\n" + sMuted.Render("no backups yet in "+m.st.BackupDir()+" — press n") + "\n\n" + backupNote
	}
	return m.table(h-3, []string{"FILE", "SIZE", "TAKEN", "AGE"}, []int{34, 10, 18, 0}, rows, nil) + "\n\n" + sMuted.Render(m.st.BackupDir()) + " · " + backupNote
}

var backupNote = sMuted.Render("Each target of the stack's backup, gzipped, the newest BACKUP_KEEP kept. Copy them off this host.")

func (m Model) viewInfra() string {
	var b strings.Builder
	in := m.infra
	b.WriteString(sHead.Render("INFRA BRANCH") + sMuted.Render("  ("+m.st.InfraBranch()+": stack.yaml, compose, proxy configs, the env example and runsnip — published from the repository)") + "\n")
	if in.Head == "" {
		b.WriteString(sMuted.Render("  not a git checkout of the infra branch") + "\n")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("  here     %s %s %s\n", in.Head, in.Subject, sMuted.Render("("+in.When+")")))
	if in.Behind == 0 {
		b.WriteString("  " + sOK.Render("✓ up to date (as of the last fetch — f to fetch)") + "\n")
	} else {
		b.WriteString("  " + sWarn.Render(fmt.Sprintf("%d new commit(s) — u to pull; it says what to reload", in.Behind)) + "\n")
		for _, c := range in.Incoming {
			b.WriteString("    " + c + "\n")
		}
	}
	return b.String()
}

// ───────────────────────── pieces ─────────────────────────

// viewEnv lists the env example's variables merged with .env (runsnip env combine): [x] = an active line in .env.
func (m Model) viewEnv(h int) string {
	if m.envErr != "" {
		return sBad.Render(m.envErr)
	}
	rows := [][]string{}
	for _, v := range m.env {
		box := "[ ]"
		if v.Enabled {
			box = "[x]"
		}
		src := "example"
		switch {
		case v.Extra:
			src = ".env only"
		case v.New:
			src = "new"
		case v.InEnv && v.Enabled:
			src = ".env"
		case v.InEnv:
			src = ".env (off)"
		}
		val := v.Value
		if v.Secret && !m.reveal && val != "" && val != m.st.Def.EnvRules.Placeholder {
			val = fmt.Sprintf("•••••••• (%d chars)", len(val))
		}
		if val == "" {
			val = "(empty)"
		}
		if v.Problem != "" {
			val += "  ✗ " + v.Problem
		}
		rows = append(rows, []string{box, v.Key, src, strings.Join(v.Services, " "), val})
	}
	out := m.table(h-envHelpLines-1, []string{"ON", "VARIABLE", "FROM", "READ BY", "VALUE"}, []int{envCheckWidth - 1, 28, 11, 18, 0}, rows, func(i, col int, s string) string {
		v := m.env[i]
		switch {
		case col == 0 && v.Enabled:
			return sOK.Render(s)
		case col == 0, col == 2 && !v.Enabled:
			return sMuted.Render(s)
		case col == 4 && (v.Problem != "" || m.st.Def.EnvRules.Placeholder != "" && strings.Contains(v.Value, m.st.Def.EnvRules.Placeholder)), col == 2 && v.New:
			return sWarn.Render(s)
		case col == 4 && !v.Enabled, col == 3:
			return sMuted.Render(s)
		}
		return s
	})
	return out + "\n" + m.envHelp()
}

// envHelpLines is the panel under the Env list: a rule, the variable's state line and its description.
const envHelpLines = 6

// envHelp describes the selected variable: its state, env.example's value and the comment above it there.
func (m Model) envHelp() string {
	if len(m.env) == 0 {
		return sMuted.Render(m.st.EnvPath + " ⇐ " + m.st.ExamplePath)
	}
	v := m.env[m.sel[tabEnv]]
	state := sMuted.Render("not in .env (off)")
	switch {
	case v.Enabled:
		state = sOK.Render("on in .env")
	case v.InEnv:
		state = sWarn.Render("commented out in .env (off)")
	case v.New:
		state = sWarn.Render("new: the example names it, .env does not have it yet")
	}
	head := sTitle.Render(v.Key) + "  " + state
	if !v.Extra {
		ex := v.Example
		if ex == "" {
			ex = "(empty)"
		}
		head += sMuted.Render("  ·  example: " + ex)
	}
	desc := v.Help
	if desc == "" {
		desc = "No description in the example."
	}
	if v.Extra {
		desc = "Only in .env: the example does not (or no longer) name it. " + desc
	}
	if len(v.Services) > 0 {
		desc += " · Read by: " + strings.Join(v.Services, ", ") + "."
	}
	if v.Generate != "" {
		desc += " · g generates it (" + v.Generate + ")."
	}
	text := lipgloss.NewStyle().Width(max(20, m.w-2)).Render(strings.ReplaceAll(desc, "\n", " "))
	lines := strings.Split(text, "\n")
	if len(lines) > envHelpLines-2 {
		lines = append(lines[:envHelpLines-3], "…")
	}
	return sMuted.Render(strings.Repeat("─", max(10, m.w))) + "\n" + head + "\n" + strings.Join(lines, "\n")
}

func (m Model) table(h int, head []string, widths []int, rows [][]string, color func(i, col int, s string) string) string {
	var b strings.Builder
	b.WriteString(sHead.Render(row(head, widths, m.w)) + "\n")
	if len(rows) == 0 {
		b.WriteString(sMuted.Render("  (nothing yet)"))
		return b.String()
	}
	visible := max(1, h-1)
	sel := m.sel[m.tab]
	off := max(0, sel-visible+1)
	for i := off; i < len(rows) && i < off+visible; i++ {
		cells := make([]string, len(rows[i]))
		for c, v := range rows[i] {
			cells[c] = fit(v, widths[c], m.w)
		}
		line := ""
		for c, v := range cells {
			if color != nil && i != sel {
				v = color(i, c, v)
			}
			line += v + " "
		}
		if i == sel && (m.tab == tabServices || m.tab == tabVersions || m.tab == tabBackups || m.tab == tabEnv) {
			line = sSel.Render(clip(line, m.w))
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func row(cells []string, widths []int, total int) string {
	out := ""
	for i, c := range cells {
		out += fit(c, widths[i], total) + " "
	}
	return out
}

func fit(s string, w, total int) string {
	if w == 0 {
		return s
	}
	if lipgloss.Width(s) > w {
		r := []rune(s)
		if len(r) > w-1 {
			r = r[:max(0, w-1)]
		}
		return string(r) + "…"
	}
	return s + strings.Repeat(" ", w-lipgloss.Width(s))
}

func (m Model) overlay(screen string) string {
	var title, body string
	switch m.mode {
	case modeConfirm:
		title, body = m.confirm.title+"?", "y / enter = run   ·   n / esc = cancel"
		if c := m.confirm; c.alt != nil {
			body = sOK.Render("y / enter") + " = " + c.label + "\n" + sWarn.Render("a") + "         = " + c.altLabel + "\n" + sMuted.Render("n / esc") + "   = cancel"
		}
	case modeInput:
		title, body = m.prompt, m.input.View()+"\n\n"+sMuted.Render("enter = ok · esc = cancel")
	case modeOutput:
		title = m.prompt
		lines := strings.Split(m.output, "\n")
		if len(lines) > max(3, m.h-12) {
			lines = lines[len(lines)-max(3, m.h-12):]
		}
		body = strings.Join(lines, "\n")
		if m.outputErr {
			body = sBad.Render(body)
		}
		body += "\n\n" + sMuted.Render("esc / enter = close")
	}
	box := sBox.Width(min(max(40, m.w-10), 100)).Render(sTitle.Render(title) + "\n\n" + body)
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, box, lipgloss.WithWhitespaceChars(" "))
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func stateText(v stack.Service) string {
	if v.State == "exited" && strings.Contains(v.Status, "(0)") {
		return "done"
	}
	return v.State
}

func stateStyle(v stack.Service) lipgloss.Style {
	switch stateText(v) {
	case "running":
		return sOK
	case "done", "not created":
		return sMuted
	case "restarting", "paused", "created":
		return sWarn
	}
	return sBad
}

func tagOf(image string) string {
	if image == "" {
		return "-" // no container yet
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return "latest"
}

func colorLog(l string) string {
	low := strings.ToLower(l)
	switch {
	case strings.Contains(low, "error") || strings.Contains(low, "fatal") || strings.Contains(low, "panic") || strings.Contains(low, `"level":50`) || strings.Contains(low, `"level":60`):
		return sBad.Render(l)
	case strings.Contains(low, "warn") || strings.Contains(low, `"level":40`):
		return sWarn.Render(l)
	}
	return l
}

func bar(frac float64, w int) string {
	frac = max(0, min(1, frac))
	n := int(frac*float64(w) + 0.5)
	st := sOK
	if frac >= 0.9 {
		st = sBad
	} else if frac >= 0.75 {
		st = sWarn
	}
	return st.Render(strings.Repeat("█", n)) + sMuted.Render(strings.Repeat("░", w-n))
}

func ratio(a, b uint64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func uptime(d time.Duration) string {
	days, h, m := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, h, m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func age(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// clip shortens a styled line to w visible cells.
func clip(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(w).Render(s)
}

// ───────────────────────── doctor and security ─────────────────────────

func doctorWord(r doctor.Report) string {
	if r.Failures == 0 {
		return sOK.Render("✓ " + doctor.Summary(r))
	}
	return sBad.Render("✗ " + doctor.Summary(r))
}

func (m Model) viewDoctor(h int) string {
	head := sHead.Render("DOCTOR") + sMuted.Render("  (runsnip doctor — every probe reports and the doctor goes on)")
	if m.report == nil {
		return head + "\n" + sMuted.Render("  running…")
	}
	when := sMuted.Render("  " + m.doctorAt.Format("15:04:05"))
	if m.busy == "doctor" {
		when = sWarn.Render("  running again…")
	}
	var lines []string
	for _, s := range m.report.Sections {
		lines = append(lines, sHead.Render(s.Title))
		for _, it := range s.Items {
			text := strings.ReplaceAll(it.Text, "\n", "\n     ")
			switch it.Level {
			case doctor.Pass:
				lines = append(lines, "  "+sOK.Render("✓")+" "+text)
			case doctor.Fail:
				lines = append(lines, "  "+sBad.Render("✗ "+text))
			case doctor.Note:
				lines = append(lines, "  "+sWarn.Render("•")+" "+text)
			case doctor.Warn:
				lines = append(lines, "  "+sWarn.Render("⚠ "+text))
			default:
				lines = append(lines, "  "+sMuted.Render(text))
			}
		}
	}
	lines = strings.Split(strings.Join(lines, "\n"), "\n")
	n := max(1, h-2)
	start := min(m.docScroll, max(0, len(lines)-n))
	end := min(len(lines), start+n)
	return head + when + "  " + doctorWord(*m.report) + "\n" + strings.Join(lines[start:end], "\n")
}

// shieldLine is the request shield's switch as .env sets it: runsnip wires Caddy to the shield and does not look at it
// (runsnip doctor checks Caddy's route to it).
func (m Model) shieldLine() string {
	sh := m.st.Def.Shield
	if sh == nil {
		return sMuted.Render("none declared")
	}
	sw, vals := sh.SwitchVar(), m.st.Values()
	if sh.On(vals) {
		return sOK.Render(sw+"=on") + sMuted.Render(" — "+sh.ProxyService()+" asks "+sh.Address(vals)+sh.Path()+" before every request (the doctor checks the route)")
	}
	return sMuted.Render("off (" + sw + " is not on; opt-in)")
}
