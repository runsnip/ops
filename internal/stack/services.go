package stack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/runsnip/ops/internal/dockerapi"
	"github.com/runsnip/ops/internal/proc"
	"github.com/runsnip/ops/internal/run"
)

// Service is one compose service as `docker compose ps` reports it.
type Service struct {
	Name, Container, Image, State, Health, Status string
	Ports                                         string
	Off                                           string // why it is not enabled ("" when it is)
	CPU, Mem                                      string
	CPUPerc                                       float64
	MemBytes, NetRx, NetTx                        uint64
	id                                            string // the container's id when it came from the Engine API
}

// Services lists every service compose defines — enabled or not, with or without a container — then containers of
// services compose no longer defines.
func (s *Stack) Services(ctx context.Context) ([]Service, error) {
	running, err := s.Containers(ctx)
	if err != nil {
		return nil, err
	}
	vals := s.Values()
	out := mergeServices(s.Compose.Order, running)
	for i := range out {
		if ok, why := s.Enabled(out[i].Name, vals); !ok && s.HasService(out[i].Name) {
			out[i].Off = why
		}
	}
	return out, nil
}

func mergeServices(defined []string, running []Service) []Service {
	byName := map[string]Service{}
	for _, r := range running {
		byName[r.Name] = r
	}
	out := make([]Service, 0, len(defined)+len(running))
	seen := map[string]bool{}
	for _, name := range defined {
		if r, ok := byName[name]; ok {
			out = append(out, r)
		} else {
			out = append(out, Service{Name: name, State: "not created"})
		}
		seen[name] = true
	}
	for _, r := range running {
		if !seen[r.Name] {
			r.Status = strings.TrimSpace(r.Status + " (not in compose)")
			out = append(out, r)
		}
	}
	return out
}

type psRow struct {
	Service, Name, Image, State, Health, Status string
	Publishers                                  []struct {
		URL                       string
		TargetPort, PublishedPort int
		Protocol                  string
	}
}

// docker returns the Engine API client when this stack runs real programs and the socket is there; tests (a fake
// runner) and hosts without the socket use the CLI.
func (s *Stack) docker() *dockerapi.Client {
	if _, real := s.Run.(run.Exec); !real {
		return nil
	}
	s.dockerOnce.Do(func() { s.dockerClient = dockerapi.New(dockerapi.Socket()) })
	return s.dockerClient
}

// ProjectName for compose labels: COMPOSE_PROJECT_NAME, else compose's name, else the folder's.
func (s *Stack) composeProject() string {
	if p := s.Getenv("COMPOSE_PROJECT_NAME"); p != "" {
		return p
	}
	return s.ProjectName()
}

// Containers lists the project's containers: from the Engine API (one request), else `docker compose ps`.
func (s *Stack) Containers(ctx context.Context) ([]Service, error) {
	if d := s.docker(); d != nil {
		if cs, err := d.Containers(ctx, s.composeProject()); err == nil {
			out := make([]Service, 0, len(cs))
			for _, c := range cs {
				out = append(out, Service{Name: c.Service, Container: c.Name, Image: c.Image, State: c.State, Health: c.Health, Status: c.Status, Ports: strings.Join(c.Ports, " "), id: c.ID})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
			return out, nil
		}
	}
	b, err := s.DCOutput(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return ParsePS([]byte(b))
}

// ParsePS accepts both compose output shapes: one JSON object per line (v2.21+) or a single JSON array.
func ParsePS(b []byte) ([]Service, error) {
	var rows []psRow
	t := bytes.TrimSpace(b)
	if len(t) == 0 {
		return nil, nil
	}
	if t[0] == '[' {
		if err := json.Unmarshal(t, &rows); err != nil {
			return nil, err
		}
	} else {
		for _, l := range bytes.Split(t, []byte("\n")) {
			if l = bytes.TrimSpace(l); len(l) == 0 {
				continue
			}
			var r psRow
			if err := json.Unmarshal(l, &r); err != nil {
				return nil, err
			}
			rows = append(rows, r)
		}
	}
	out := make([]Service, 0, len(rows))
	for _, r := range rows {
		var ports []string
		seen := map[string]bool{}
		for _, p := range r.Publishers {
			if p.PublishedPort > 0 {
				x := fmt.Sprintf("%d→%d/%s", p.PublishedPort, p.TargetPort, p.Protocol)
				if !seen[x] {
					seen[x] = true
					ports = append(ports, x)
				}
			}
		}
		out = append(out, Service{Name: r.Service, Container: r.Name, Image: r.Image, State: r.State, Health: r.Health, Status: r.Status, Ports: strings.Join(ports, " ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Stats adds CPU, memory and network use from `docker stats` to running services (best effort).
func (s *Stack) Stats(ctx context.Context, svcs []Service) {
	if d := s.docker(); d != nil && len(svcs) > 0 && svcs[0].id != "" {
		var wg sync.WaitGroup
		for i := range svcs {
			if svcs[i].State != "running" || svcs[i].id == "" {
				continue
			}
			wg.Add(1)
			go func(v *Service) {
				defer wg.Done()
				st, err := d.Stats(ctx, v.id)
				if err != nil {
					return
				}
				v.CPUPerc, v.MemBytes, v.NetRx, v.NetTx = st.CPUPerc, st.MemBytes, st.NetRx, st.NetTx
				v.CPU = fmt.Sprintf("%.2f%%", st.CPUPerc)
				v.Mem = fmt.Sprintf("%.1fMiB", float64(st.MemBytes)/(1<<20))
			}(&svcs[i])
		}
		wg.Wait()
		return
	}
	var names []string
	idx := map[string]int{}
	for i, v := range svcs {
		if v.State == "running" && v.Container != "" {
			names = append(names, v.Container)
			idx[v.Container] = i
		}
	}
	if len(names) == 0 {
		return
	}
	b, err := run.Output(ctx, s.Run, run.Call{Name: "docker", Args: append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)})
	if err != nil {
		return
	}
	for _, l := range strings.Split(strings.TrimSpace(b), "\n") {
		var r struct{ Name, CPUPerc, MemUsage, NetIO string }
		if json.Unmarshal([]byte(l), &r) == nil {
			if i, ok := idx[r.Name]; ok {
				v := &svcs[i]
				v.CPU, v.Mem = r.CPUPerc, strings.TrimSpace(strings.SplitN(r.MemUsage, "/", 2)[0])
				v.CPUPerc, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(r.CPUPerc), "%"), 64)
				v.MemBytes = ParseSize(v.Mem)
				rx, tx, _ := strings.Cut(r.NetIO, "/")
				v.NetRx, v.NetTx = ParseSize(rx), ParseSize(tx)
			}
		}
	}
}

// ParseSize reads docker's sizes: "12.5MiB" (binary units) and "1.2kB" / "3.4MB" (decimal units).
func ParseSize(v string) uint64 {
	v = strings.TrimSpace(v)
	i := 0
	for i < len(v) && (v[i] >= '0' && v[i] <= '9' || v[i] == '.') {
		i++
	}
	n, err := strconv.ParseFloat(v[:i], 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{
		"": 1, "B": 1,
		"kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
	}[strings.TrimSpace(v[i:])]
	return uint64(n * mult)
}

// LogsCmd returns a `docker compose logs` command (every service when svc is empty); the caller reads its stdout.
func (s *Stack) LogsCmd(ctx context.Context, svc string, follow bool, tail int) *exec.Cmd {
	args := s.composeArgs("logs", "--no-color", "--tail", fmt.Sprint(tail))
	if follow {
		args = append(args, "-f")
	}
	if svc != "" {
		args = append(args, svc)
	}
	cmd := proc.Command(ctx, "docker", args...)
	cmd.Dir = s.Dir
	cmd.Env = append(os.Environ(), s.composeEnv()...)
	return cmd
}

// ───────────────────────── versions ─────────────────────────

// Version is a component's pinned, previous and running version.
type Version struct {
	Component, Image, TagVar, Pinned, Previous, Running, Off string
}

// Versions reads the pins from .env and the running tags from compose.
func (s *Stack) Versions(ctx context.Context) ([]Version, error) {
	env, err := s.Env()
	if err != nil {
		return nil, err
	}
	vals := env.Values()
	running := map[string]string{}
	if rows, err := s.Containers(ctx); err == nil {
		for _, r := range rows {
			running[r.Name] = tagOf(r.Image)
		}
	}
	var out []Version
	for _, c := range s.Components() {
		v := Version{Component: c.Name, Image: s.ExpandWith(c.Image.Repo, vals), TagVar: c.Image.TagVar, Pinned: s.Tag(c, vals), Running: running[c.Service], Previous: s.previous(c, env)}
		if ok, why := s.Enabled(c.Service, vals); !ok {
			v.Off = why
		}
		out = append(out, v)
	}
	return out, nil
}

func tagOf(image string) string {
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	if image == "" {
		return ""
	}
	return "latest"
}

// lastTagFile is where a stack that keeps previous versions in files (components.previous: file) keeps them.
func (s *Stack) lastTagFile(c Component) string {
	return filepath.Join(filepath.Dir(s.EnvPath), ".last-tag-"+c.Name)
}

func (s *Stack) previous(c Component, env interface{ Get(string) string }) string {
	if s.Def.Components.Previous != "file" {
		if v := env.Get(c.Image.TagVar + "_PREVIOUS"); v != "" {
			return v
		}
	}
	// Files written before the stack kept them in .env (and the file mode itself).
	if b, err := os.ReadFile(s.lastTagFile(c)); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// ───────────────────────── registry ─────────────────────────

// ImageExists checks that an image is published and readable (`runsnip login` for private packages).
func (s *Stack) ImageExists(ctx context.Context, ref string) bool {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := run.Output(cctx, s.Run, run.Call{Name: "docker", Args: []string{"manifest", "inspect", ref}})
	return err == nil
}
