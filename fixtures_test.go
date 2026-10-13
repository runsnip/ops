package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/doctor"
	"github.com/runsnip/ops/internal/envmodel"
	"github.com/runsnip/ops/internal/graph"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/standard"
)

// The whole tool against whole stacks, with no special case in the code: a tiny synthetic stack in testdata, and
// any other stack named in RUNSNIP_FIXTURES ("name=<stack folder>@<expectations.json>", several joined by ":").
// Other projects' stacks and their expectations stay in those projects; nothing of them is in this repository.
// Each run reads the stack, validates it against the standard, builds the .env model, lays out the doctor and draws
// the graph, then checks the nodes and edge kinds its expectations name.
type fixture struct {
	Nodes     map[string]string `json:"nodes"` // id → kind
	Edges     [][3]string       `json:"edges"` // from, to, kind
	None      []string          `json:"none"`  // ids or from→to pairs that must not be there
	Draft     bool              `json:"draft"` // a draft stack.yaml: gaps against the standard are allowed
	AllowGaps []string          `json:"allowGaps"`
}

func fixtureDirs(t *testing.T) map[string][2]string {
	dirs := map[string][2]string{"tiny": {"testdata/stacks/tiny", "testdata/stacks/tiny.expect.json"}}
	for _, kv := range strings.Split(os.Getenv("RUNSNIP_FIXTURES"), ":") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			dir, exp, _ := strings.Cut(v, "@")
			dirs[k] = [2]string{dir, exp}
		}
	}
	return dirs
}

func TestWholeToolOnFixtureStacks(t *testing.T) {
	for name, paths := range fixtureDirs(t) {
		dir := paths[0]
		t.Run(name, func(t *testing.T) {
			var want fixture
			b, err := os.ReadFile(paths[1])
			if err != nil {
				t.Fatalf("expectations: %v", err)
			}
			if err := json.Unmarshal(b, &want); err != nil {
				t.Fatalf("expectations: %v", err)
			}
			// .env: the stack's own when there is one, else its example.
			if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
				for _, ex := range []string{".env.example", "env.example"} {
					if _, err := os.Stat(filepath.Join(dir, ex)); err == nil {
						t.Setenv("RUNSNIP_ENV_FILE", filepath.Join(dir, ex))
					}
				}
			}
			st, err := stack.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			// A draft is allowed its gaps; a fixture may allow named ones (a stack that ships no release scripts).
			for _, f := range standard.Validate(dir).Findings {
				bad := f.Level == "error" || f.Level == "gap" && standard.RuleByID(f.Rule).Required
				if bad && !want.Draft && !contains(want.AllowGaps, f.Rule) {
					t.Errorf("%s: %s %s: %s", name, f.Rule, f.Level, f.Text)
				}
			}
			if _, err := envmodel.Build(st); err != nil {
				t.Errorf("env model: %v", err)
			}
			if len(doctor.StandardSections(st)) == 0 {
				t.Error("no doctor sections")
			}
			g, err := graph.Build(st)
			if err != nil {
				t.Fatal(err)
			}
			kind := map[string]string{}
			for _, n := range g.Nodes {
				kind[n.ID] = n.Kind
				if n.Icon == "" || n.Layer == "" {
					t.Errorf("%s has no icon or layer", n.ID)
				}
			}
			for id, k := range want.Nodes {
				if kind[id] != k {
					t.Errorf("node %s: kind %q, want %q", id, kind[id], k)
				}
			}
			has := map[string]bool{}
			for _, e := range g.Edges {
				has[e.From+"→"+e.To+"/"+e.Kind] = true
				has[e.From+"→"+e.To] = true
				if kind[e.From] == "" || kind[e.To] == "" {
					t.Errorf("edge %s→%s to a node that is not there", e.From, e.To)
				}
			}
			for _, e := range want.Edges {
				if !has[e[0]+"→"+e[1]+"/"+e[2]] {
					t.Errorf("missing %s edge %s → %s", e[2], e[0], e[1])
				}
			}
			for _, x := range want.None {
				if has[x] || kind[x] != "" {
					t.Errorf("%s must not be there", x)
				}
			}
			// Columns go left to right: every route edge points right.
			col := map[string]int{}
			for _, n := range g.Nodes {
				col[n.ID] = n.Col
			}
			for _, e := range g.Edges {
				if e.Kind == graph.EdgeRoute && col[e.To] <= col[e.From] {
					t.Errorf("route %s → %s goes left (%d → %d)", e.From, e.To, col[e.From], col[e.To])
				}
			}
		})
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
