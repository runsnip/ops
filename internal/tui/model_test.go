package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/stack"
)

const psJSON = `{"Service":"api","Name":"api","Image":"ghcr.io/x/demo-api:0.2.0","State":"running","Health":"healthy","Status":"Up"}
{"Service":"web","Name":"web","Image":"ghcr.io/x/demo-web:0.2.0","State":"exited","Status":"Exited (1)"}`

func newModel(t *testing.T) (Model, *run.Fake) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "stack.yaml"), []byte("standard: 1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("name: demo\nservices:\n  postgres:\n    image: postgres:16\n  api:\n    image: ${P}-api:${API_TAG:-latest}\n  web:\n    image: ${P}-web:${WEB_TAG:-latest}\n  caddy:\n    image: caddy:2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("P=ghcr.io/x/demo\nAPI_TAG=0.2.0\nAPI_TAG_PREVIOUS=0.1.0\nWEB_TAG=0.2.0\nJWT_SECRET=supersecretvalue\n"), 0o600)
	os.WriteFile(filepath.Join(dir, ".env.example"), []byte("# The API's version.\nAPI_TAG=latest\n# A secret.\nJWT_SECRET=\n# OPTIONAL=1\n"), 0o644)
	st, err := stack.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &run.Fake{}
	f.On(" ps --all", psJSON)
	f.On("docker stats", `{"Name":"api","CPUPerc":"137.50%","MemUsage":"1.5GiB / 15.6GiB","NetIO":"12.5MB / 3.2MB"}`)
	f.Failing("shieldctl", "not found")
	f.Failing("systemctl", "not found")
	f.Failing("git", "not a repository")
	f.On("", "")
	st.Run = f
	m := New(st, "test")
	m.w, m.h = 120, 40
	var mm tea.Model = m
	mm, _ = mm.Update(m.fetchFast()())
	mm, _ = mm.Update(m.fetchSlow(false)())
	return mm.(Model), f
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	var mm tea.Model = m
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		var cmd tea.Cmd
		mm, cmd = mm.Update(msg)
		if cmd != nil && mm.(Model).busy != "" && mm.(Model).busy != "doctor" {
			mm, _ = mm.Update(cmd())
		}
	}
	return mm.(Model)
}

func TestServicesTabRestartsTheSelectedServiceAfterConfirm(t *testing.T) {
	m, f := newModel(t)
	m = press(t, m, "2", "j", "j")
	if m.tab != tabServices || m.visibleServices()[m.sel[tabServices]].Name != "web" {
		t.Fatalf("tab %v sel %d", m.tab, m.sel[tabServices])
	}
	m = press(t, m, "r")
	if m.mode != modeConfirm || !strings.Contains(m.View(), "Restart web?") {
		t.Fatal("no confirmation")
	}
	f.Calls = nil
	m = press(t, m, "y")
	l := strings.Join(f.Lines(), "\n")
	if m.mode != modeOutput || !strings.Contains(l, "up -d --no-deps --force-recreate web") || strings.Contains(l, " pull ") {
		t.Fatalf("mode %v calls %s", m.mode, l)
	}
	m = press(t, m, "esc")
	f.Calls = nil
	m = press(t, m, "p", "n")
	if len(f.Calls) != 0 || m.mode != modeNormal {
		t.Fatal("n cancels")
	}
}

func TestVersionsTabPinsThroughThePromptAndRollsBack(t *testing.T) {
	m, f := newModel(t)
	m = press(t, m, "3")
	if v := m.View(); !strings.Contains(v, "api") || !strings.Contains(v, "0.1.0") {
		t.Fatal(v)
	}
	m = press(t, m, "p")
	if m.mode != modeInput {
		t.Fatal("pin asks for a version")
	}
	m.input.SetValue("0.3.0")
	m = press(t, m, "enter")
	if m.mode != modeConfirm {
		t.Fatal("then confirms")
	}
	f.Calls = nil
	m = press(t, m, "y")
	b, _ := os.ReadFile(m.st.EnvPath)
	if !strings.Contains(string(b), "API_TAG=0.3.0") || !strings.Contains(string(b), "API_TAG_PREVIOUS=0.2.0") {
		t.Fatalf(".env:\n%s", b)
	}
	if !strings.Contains(strings.Join(f.Lines(), "\n"), "up -d --no-deps --force-recreate api") {
		t.Fatal(f.Lines())
	}
}

func TestEnvTabTogglesAndMasksSecrets(t *testing.T) {
	m, _ := newModel(t)
	m = press(t, m, "8")
	v := m.View()
	if !strings.Contains(v, "•••••••• (16 chars)") || strings.Contains(v, "supersecretvalue") {
		t.Fatalf("secrets masked:\n%s", v)
	}
	if !strings.Contains(v, "The API's version.") {
		t.Fatal("the example's comment is the help")
	}
	m = press(t, m, "j", "j", " ") // OPTIONAL: off → on
	b, _ := os.ReadFile(m.st.EnvPath)
	if !strings.Contains(string(b), "OPTIONAL=1") {
		t.Fatalf("toggled on:\n%s", b)
	}
}

func TestOverviewLeadsWithFourCardsAndRenderOnceShowsEveryTab(t *testing.T) {
	m, _ := newModel(t)
	out := RenderOnce(m, 140)
	for _, want := range []string{"CPU", "RAM", "NETWORK", "HEALTHY", "critical", "down: postgres web", "COMPONENT", "INFRA BRANCH", "VARIABLE"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}
