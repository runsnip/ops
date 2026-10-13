package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// RenderOnce loads everything once and renders every tab one after another (no terminal control): `runsnip top
// --once` for a quick look over SSH, in scripts and in tests. Logs (a stream) and Doctor (runsnip doctor) are left
// out.
func RenderOnce(m Model, width int) string {
	m.w, m.h = width, 40
	m.loadEnv()
	var mm tea.Model = m
	// Two samples a second apart: CPU and network are rates.
	mm, _ = mm.Update(m.fetchFast()())
	time.Sleep(time.Second)
	mm, _ = mm.Update(mm.(Model).fetchFast()())
	mm, _ = mm.Update(m.fetchSlow(false)())
	cur := mm.(Model)
	var parts []string
	for i := range tabNames {
		if tab(i) == tabLogs || tab(i) == tabDoctor {
			continue
		}
		cur.tab = tab(i)
		parts = append(parts, squeeze(cur.View()))
	}
	return strings.Join(parts, "\n\n")
}

// squeeze drops the padding that fills a terminal screen: runs of blank lines become one.
func squeeze(s string) string {
	var out []string
	blank := false
	for _, l := range strings.Split(strings.TrimRight(s, "\n "), "\n") {
		if strings.TrimSpace(l) == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
