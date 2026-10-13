package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// bigFont draws digits five rows tall, readable from across the room.
var bigFont = map[rune][5]string{
	'0': {"████", "█  █", "█  █", "█  █", "████"},
	'1': {" ██ ", "███ ", " ██ ", " ██ ", "████"},
	'2': {"████", "   █", "████", "█   ", "████"},
	'3': {"████", "   █", " ███", "   █", "████"},
	'4': {"█  █", "█  █", "████", "   █", "   █"},
	'5': {"████", "█   ", "████", "   █", "████"},
	'6': {"████", "█   ", "████", "█  █", "████"},
	'7': {"████", "   █", "  █ ", " █  ", " █  "},
	'8': {"████", "█  █", "████", "█  █", "████"},
	'9': {"████", "█  █", "████", "   █", "████"},
	'.': {"  ", "  ", "  ", "  ", "█ "},
	'%': {"█  █", "   █", "  █ ", " █  ", "█  █"},
	'/': {"   █", "  █ ", " █  ", " █  ", "█   "},
	'-': {"    ", "    ", "████", "    ", "    "},
}

// hugeFont is bigFont's larger sibling: seven rows tall, strokes two columns wide. Used when the cards have room.
var hugeFont = map[rune][7]string{
	'0': {"██████", "██  ██", "██  ██", "██  ██", "██  ██", "██  ██", "██████"},
	'1': {"  ██  ", "████  ", "  ██  ", "  ██  ", "  ██  ", "  ██  ", "██████"},
	'2': {"██████", "    ██", "    ██", "██████", "██    ", "██    ", "██████"},
	'3': {"██████", "    ██", "    ██", " █████", "    ██", "    ██", "██████"},
	'4': {"██  ██", "██  ██", "██  ██", "██████", "    ██", "    ██", "    ██"},
	'5': {"██████", "██    ", "██    ", "██████", "    ██", "    ██", "██████"},
	'6': {"██████", "██    ", "██    ", "██████", "██  ██", "██  ██", "██████"},
	'7': {"██████", "    ██", "    ██", "   ██ ", "  ██  ", "  ██  ", "  ██  "},
	'8': {"██████", "██  ██", "██  ██", "██████", "██  ██", "██  ██", "██████"},
	'9': {"██████", "██  ██", "██  ██", "██████", "    ██", "    ██", "██████"},
	'.': {"   ", "   ", "   ", "   ", "   ", "   ", "██ "},
	'%': {"██  ██", "    ██", "   ██ ", "  ██  ", " ██   ", "██    ", "██  ██"},
	'/': {"    ██", "   ██ ", "   ██ ", "  ██  ", " ██   ", " ██   ", "██    "},
	'-': {"      ", "      ", "      ", "██████", "      ", "      ", "      "},
}

// bigText renders s in bigFont, or hugeFont when huge (unknown runes are skipped).
func bigText(s string, huge bool) []string {
	n := 5
	if huge {
		n = 7
	}
	rows := make([]string, n)
	for _, r := range s {
		var g []string
		if huge {
			if h, ok := hugeFont[r]; ok {
				g = h[:]
			}
		} else if b, ok := bigFont[r]; ok {
			g = b[:]
		}
		if g == nil {
			continue
		}
		for i := range rows {
			rows[i] += g[i] + " "
		}
	}
	return rows
}

// stat is one of the four cards that lead the Overview: one big number, a label, a sparkline of the last minutes and
// a state said in words (normal, warning, critical), not by colour alone.
type stat struct {
	title, value, unit, sub string
	state                   string // normal, warning, critical, or "" while measuring
	hist                    []float64
	color                   lipgloss.Color
}

const cardsMinWidth, cardsMinHeight = 100, 30

func level(frac float64) string {
	switch {
	case frac >= 0.9:
		return "critical"
	case frac >= 0.75:
		return "warning"
	}
	return "normal"
}

// healthCount is how many enabled services are healthy (running, and healthy when they declare a health check) out
// of those expected to run (one-shot jobs that finished are not).
func (m Model) healthCount() (up, expected int) {
	for _, v := range m.services {
		switch {
		case v.Off != "":
		case stateText(v) == "done":
		default:
			expected++
			if v.State == "running" && (v.Health == "" || v.Health == "healthy") {
				up++
			}
		}
	}
	return up, expected
}

// overviewStats: CPU and RAM of the host, the containers' network rate, and services healthy. The fourth is
// "healthy" rather than disk free: it answers the first question an operator has (is anything down?), while disk
// fills slowly and the bars below the cards and the doctor show it.
func (m Model) overviewStats() []stat {
	cpuVal := fmt.Sprintf("%.0f%%", m.cpu)
	if m.cpu < 10 {
		cpuVal = fmt.Sprintf("%.1f%%", m.cpu)
	}
	cores := max(1, m.sys.CPUs)
	cpu := stat{title: "CPU", value: cpuVal, sub: fmt.Sprintf("%d cores · load %.2f", cores, m.sys.Load1), state: level(m.cpu / 100), hist: m.hist["cpu"], color: lipgloss.Color("141")}
	if !m.sys.Supported {
		cpu.value, cpu.state, cpu.sub = "-", "", fmt.Sprintf("%d cores · needs Linux /proc", cores)
	}
	ram := stat{title: "RAM", value: "-", sub: "needs Linux /proc", hist: m.hist["mem"], color: cBrand}
	if m.sys.MemTotal > 0 {
		used := m.sys.MemTotal - m.sys.MemAvail
		ram.value, ram.unit = humanParts(float64(used), 1024, []string{"B", "KiB", "MiB", "GiB", "TiB"})
		ram.sub = fmt.Sprintf("used of %s", humanBytes(m.sys.MemTotal))
		ram.state = level(float64(used) / float64(m.sys.MemTotal))
	}
	rate := m.netRx + m.netTx
	netVal, netUnit := humanParts(rate, 1000, []string{"B/s", "kB/s", "MB/s", "GB/s"})
	net := stat{title: "NETWORK", value: netVal, unit: netUnit, sub: fmt.Sprintf("↓ %s  ↑ %s", humanRate(m.netRx), humanRate(m.netTx)), state: "normal", hist: m.hist["net"], color: lipgloss.Color("44")}
	if m.netAt.IsZero() {
		net.value, net.unit, net.sub, net.state = "-", "", "measuring (two refreshes)…", ""
	}
	up, exp := m.healthCount()
	h := stat{title: "HEALTHY", value: fmt.Sprintf("%d/%d", up, exp), sub: "services up and healthy", hist: m.hist["up"], color: cOK, state: "normal"}
	switch {
	case exp == 0:
		h.state, h.color = "", cMuted
	case up < exp:
		h.state, h.color = "critical", cBad
		var down []string
		for _, v := range m.services {
			if v.Off == "" && stateText(v) != "done" && !(v.State == "running" && (v.Health == "" || v.Health == "healthy")) {
				down = append(down, v.Name)
			}
		}
		h.sub = "down: " + strings.Join(down, " ")
	}
	return []stat{cpu, ram, net, h}
}

func stateMark(s string) string {
	switch s {
	case "critical":
		return sBad.Render("▲ critical")
	case "warning":
		return sWarn.Render("● warning")
	case "normal":
		return sOK.Render("✓ normal")
	}
	return sMuted.Render("… measuring")
}

// spark draws the history in w cells.
func spark(h []float64, w int) string {
	if len(h) == 0 {
		return strings.Repeat(" ", w)
	}
	if len(h) > w {
		h = h[len(h)-w:]
	}
	top := 0.0
	for _, v := range h {
		top = math.Max(top, v)
	}
	ticks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range h {
		i := 0
		if top > 0 {
			i = int(v / top * float64(len(ticks)-1))
		}
		b.WriteRune(ticks[i])
	}
	return strings.Repeat(" ", w-len(h)) + b.String()
}

// overviewCards renders the four stats as big cards in a row, or one line when the terminal is too small.
func (m Model) overviewCards() string {
	stats := m.overviewStats()
	if m.w < cardsMinWidth || m.h < cardsMinHeight {
		parts := make([]string, len(stats))
		for i, s := range stats {
			parts[i] = sMuted.Render(s.title+" ") + lipgloss.NewStyle().Bold(true).Foreground(s.color).Render(strings.TrimSpace(s.value+" "+s.unit)) + " " + stateMark(s.state)
		}
		return strings.Join(parts, sMuted.Render("  ·  "))
	}
	w := (m.w - 2*len(stats)) / len(stats)
	huge := m.h >= cardsMinHeight+6
	for _, s := range stats {
		if lipgloss.Width(bigText(s.value, true)[0])+1+len(s.unit) > w-4 {
			huge = false
		}
	}
	cards := make([]string, len(stats))
	for i, s := range stats {
		big := lipgloss.NewStyle().Foreground(s.color).Bold(true).Render(strings.Join(bigText(s.value, huge), "\n"))
		if s.unit != "" {
			big = lipgloss.JoinHorizontal(lipgloss.Bottom, big, " "+lipgloss.NewStyle().Foreground(s.color).Bold(true).Render(s.unit))
		}
		body := sHead.Render(s.title) + "  " + stateMark(s.state) + "\n" + big + "\n" + sMuted.Render(clip(s.sub, w-4)) + "\n" + lipgloss.NewStyle().Foreground(s.color).Render(spark(s.hist, w-4))
		cards[i] = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(s.color).Padding(0, 1).Width(w).Render(body)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cards...)
}

// humanParts splits a quantity into a number for the big font and its unit.
func humanParts(v, base float64, units []string) (string, string) {
	i := 0
	for v >= base && i < len(units)-1 {
		v /= base
		i++
	}
	switch {
	case v >= 100 || i == 0:
		return fmt.Sprintf("%.0f", v), units[i]
	case v >= 10:
		return fmt.Sprintf("%.1f", v), units[i]
	}
	return fmt.Sprintf("%.2f", v), units[i]
}

func humanRate(v float64) string {
	n, u := humanParts(v, 1000, []string{"B/s", "kB/s", "MB/s", "GB/s"})
	return n + " " + u
}

func humanBytes(v uint64) string {
	n, u := humanParts(float64(v), 1024, []string{"B", "KiB", "MiB", "GiB", "TiB"})
	return n + " " + u
}
