package serve

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/runsnip/ops/internal/graph"
	"github.com/runsnip/ops/internal/host"
)

// stackGraph is `runsnip graph --json` for the page: the same data, read from the stack's files.
func (s *Server) stackGraph(w http.ResponseWriter, r *http.Request, _ *state) {
	st, _, err := s.openStack(r.PathValue("name"))
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	g, err := graph.Build(st)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, g)
}

// The positions an admin dragged nodes to, per stack, in the state folder: a view preference, not a change to the
// stack, so the page writes it directly (no job).
func (s *Server) layoutPath(name string) (string, bool) {
	if _, ok, _ := host.Find(s.Env.State, name); !ok {
		return "", false
	}
	return filepath.Join(s.Env.State, "layouts", name+".json"), true
}

func (s *Server) getLayout(w http.ResponseWriter, r *http.Request, _ *state) {
	p, ok := s.layoutPath(r.PathValue("name"))
	if !ok {
		fail(w, 404, "no such stack")
		return
	}
	pos := map[string][2]float64{}
	_ = host.ReadJSON(p, &pos)
	writeJSON(w, pos)
}

func (s *Server) putLayout(w http.ResponseWriter, r *http.Request, _ *state) {
	p, ok := s.layoutPath(r.PathValue("name"))
	if !ok {
		fail(w, 404, "no such stack")
		return
	}
	var pos map[string][2]float64
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&pos); err != nil || len(pos) > 2000 {
		fail(w, 400, "bad layout")
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if err := host.WriteJSON(p, pos); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) deleteLayout(w http.ResponseWriter, r *http.Request, _ *state) {
	p, ok := s.layoutPath(r.PathValue("name"))
	if !ok {
		fail(w, 404, "no such stack")
		return
	}
	_ = os.Remove(p)
	writeJSON(w, map[string]any{"ok": true})
}
