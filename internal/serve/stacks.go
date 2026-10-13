package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/runsnip/ops/internal/envmodel"
	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/internal/sysinfo"
)

// openStack opens a registered stack.
func (s *Server) openStack(name string) (*stack.Stack, host.StackEntry, error) {
	e, ok, err := host.Find(s.Env.State, name)
	if err != nil {
		return nil, e, err
	}
	if !ok {
		return nil, e, fmt.Errorf("no stack %q", name)
	}
	st, err := stack.Open(e.Dir)
	return st, e, err
}

func (s *Server) listStacks(w http.ResponseWriter, r *http.Request, _ *state) {
	all, err := host.Stacks(s.Env.State)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	type row struct {
		host.StackEntry
		Title string `json:"title"`
		Error string `json:"error,omitempty"`
	}
	out := []row{}
	for _, e := range all {
		rw := row{StackEntry: e}
		if st, err := stack.Open(e.Dir); err == nil {
			rw.Title = st.Title()
		} else {
			rw.Error = err.Error()
		}
		out = append(out, rw)
	}
	writeJSON(w, out)
}

// ───────────────────────── live snapshots ─────────────────────────

// Snapshot is what a stack's page shows, pushed every 2 s while it is open.
type Snapshot struct {
	Stack    string          `json:"stack"`
	Title    string          `json:"title"`
	Host     string          `json:"host"`
	At       time.Time       `json:"at"`
	System   System          `json:"system"`
	Cards    []Card          `json:"cards"`
	Services []stack.Service `json:"services"`
	Versions []stack.Version `json:"versions"`
	Shield   ShieldState     `json:"shield"`
	Error    string          `json:"error,omitempty"`
}

// System is the host.
type System struct {
	Supported bool    `json:"supported"`
	CPUs      int     `json:"cpus"`
	CPU       float64 `json:"cpu"`
	Load1     float64 `json:"load1"`
	Mem       string  `json:"mem"`
	Swap      string  `json:"swap"`
	Disk      string  `json:"disk"`
	Uptime    string  `json:"uptime"`
}

// Card is one of the four big numbers.
type Card struct {
	Label string    `json:"label"`
	Value string    `json:"value"`
	Unit  string    `json:"unit"`
	Sub   string    `json:"sub"`
	State string    `json:"state"` // normal, warning, critical, measuring
	Hist  []float64 `json:"hist"`
}

// ShieldState is the shield switch as .env sets it.
type ShieldState struct {
	Declared bool   `json:"declared"`
	Proxy    string `json:"proxy,omitempty"`
	Switch   string `json:"switch"`
	On       bool   `json:"on"`
	Addr     string `json:"addr"`
}

// sampler samples one stack while at least one page watches it.
type sampler struct {
	srv     *Server
	name    string
	mu      sync.Mutex
	viewers int
	stop    context.CancelFunc
	subs    map[chan []byte]bool
	snap    Snapshot
	prevSys sysinfo.Snapshot
	prevNet [2]uint64
	prevAt  time.Time
	hist    map[string][]float64
	st      *stack.Stack
}

type samplers struct {
	srv *Server
	mu  sync.Mutex
	m   map[string]*sampler
}

func newSamplers(s *Server) *samplers { return &samplers{srv: s, m: map[string]*sampler{}} }

func (ss *samplers) get(name string) *sampler {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if x, ok := ss.m[name]; ok {
		return x
	}
	x := &sampler{srv: ss.srv, name: name, subs: map[chan []byte]bool{}, hist: map[string][]float64{}}
	ss.m[name] = x
	return x
}

// Viewers counts open pages over every stack (tests, measurements).
func (s *Server) Viewers() int {
	s.samplers.mu.Lock()
	defer s.samplers.mu.Unlock()
	n := 0
	for _, x := range s.samplers.m {
		x.mu.Lock()
		n += x.viewers
		x.mu.Unlock()
	}
	return n
}

func (x *sampler) subscribe() chan []byte {
	ch := make(chan []byte, 4)
	x.mu.Lock()
	x.subs[ch] = true
	x.viewers++
	if x.viewers == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		x.stop = cancel
		go x.loop(ctx)
	}
	x.mu.Unlock()
	return ch
}

func (x *sampler) unsubscribe(ch chan []byte) {
	x.mu.Lock()
	delete(x.subs, ch)
	x.viewers--
	if x.viewers == 0 && x.stop != nil {
		x.stop()
		x.stop, x.st = nil, nil
		// Nobody watches: hand the memory the samples took back to the system.
		go func() { time.Sleep(time.Second); debug.FreeOSMemory() }()
	}
	x.mu.Unlock()
}

func (x *sampler) loop(ctx context.Context) {
	fast, slow := time.NewTicker(2*time.Second), time.NewTicker(20*time.Second)
	defer fast.Stop()
	defer slow.Stop()
	x.sample(ctx, true)
	x.broadcast()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			x.sample(ctx, false)
		case <-slow.C:
			x.sample(ctx, true)
		}
		x.broadcast()
	}
}

func (x *sampler) broadcast() {
	x.mu.Lock()
	b, _ := json.Marshal(x.snap)
	for ch := range x.subs {
		select {
		case ch <- b:
		default:
		}
	}
	x.mu.Unlock()
}

func gauge(frac float64) string {
	switch {
	case frac >= 0.9:
		return "critical"
	case frac >= 0.75:
		return "warning"
	}
	return "normal"
}

func (x *sampler) push(k string, v float64) []float64 {
	h := append(x.hist[k], v)
	if len(h) > 90 {
		h = h[len(h)-90:]
	}
	x.hist[k] = h
	return append([]float64(nil), h...)
}

func (x *sampler) sample(ctx context.Context, slow bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if x.st == nil || slow {
		st, _, err := x.srv.openStack(x.name)
		if err != nil {
			x.mu.Lock()
			x.snap.Error = err.Error()
			x.mu.Unlock()
			return
		}
		x.st = st
	}
	st := x.st
	x.mu.Lock()
	snap := x.snap
	x.mu.Unlock()
	snap.Stack, snap.Title, snap.At, snap.Error = x.name, st.Title(), time.Now(), ""
	snap.Host, _ = os.Hostname()
	sys := sysinfo.Read(st.DataDir())
	if svcs, err := st.Services(ctx); err != nil {
		snap.Error = err.Error()
	} else {
		st.Stats(ctx, svcs)
		snap.Services = svcs
	}
	if slow {
		snap.Versions, _ = st.Versions(ctx)
		snap.Shield = shieldState(st)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	cpu := sysinfo.CPUPercent(x.prevSys, sys)
	var rx, tx uint64
	for _, v := range snap.Services {
		rx, tx = rx+v.NetRx, tx+v.NetTx
	}
	rate, measured := 0.0, false
	if !x.prevAt.IsZero() {
		if dt := time.Since(x.prevAt).Seconds(); dt > 0 {
			rate = (max(0, float64(rx)-float64(x.prevNet[0])) + max(0, float64(tx)-float64(x.prevNet[1]))) / dt
			measured = true
		}
	}
	x.prevSys, x.prevNet, x.prevAt = sys, [2]uint64{rx, tx}, time.Now()
	snap.System = System{Supported: sys.Supported, CPUs: sys.CPUs, CPU: cpu, Load1: sys.Load1,
		Mem:    sysinfo.HumanBytes(sys.MemTotal-sys.MemAvail) + " / " + sysinfo.HumanBytes(sys.MemTotal),
		Swap:   sysinfo.HumanBytes(sys.SwapTotal-sys.SwapFree) + " / " + sysinfo.HumanBytes(sys.SwapTotal),
		Disk:   sysinfo.HumanBytes(sys.DiskTotal-sys.DiskFree) + " / " + sysinfo.HumanBytes(sys.DiskTotal),
		Uptime: (time.Duration(sys.Uptime) * time.Second).String()}
	snap.Cards = x.cards(sys, cpu, rate, measured, snap.Services)
	x.snap = snap
}

// cards: CPU, RAM, network, and services healthy — the fourth answers "is anything down?", the first question.
func (x *sampler) cards(sys sysinfo.Snapshot, cpu, rate float64, measured bool, svcs []stack.Service) []Card {
	c := []Card{{Label: "CPU", Value: "–", Sub: "needs Linux /proc", State: "measuring"}}
	if sys.Supported {
		c[0] = Card{Label: "CPU", Value: fmt.Sprintf("%.0f", cpu), Unit: "%", Sub: fmt.Sprintf("%d cores · load %.2f", sys.CPUs, sys.Load1), State: gauge(cpu / 100), Hist: x.push("cpu", cpu)}
	}
	ram := Card{Label: "RAM", Value: "–", Sub: "needs Linux /proc", State: "measuring"}
	if sys.MemTotal > 0 {
		used := sys.MemTotal - sys.MemAvail
		ram.Value, ram.Unit, _ = strings.Cut(sysinfo.HumanBytes(used), " ")
		ram.Sub, ram.State = "used of "+sysinfo.HumanBytes(sys.MemTotal), gauge(float64(used)/float64(sys.MemTotal))
		ram.Hist = x.push("mem", 100*float64(used)/float64(sys.MemTotal))
	}
	net := Card{Label: "Network", Value: "–", Sub: "measuring (two samples)", State: "measuring"}
	if measured {
		net.Value, net.Unit, _ = strings.Cut(humanRate(rate), " ")
		net.Sub, net.State, net.Hist = "in + out, containers", "normal", x.push("net", rate)
	}
	up, exp := 0, 0
	var down []string
	for _, v := range svcs {
		if v.Off != "" || v.State == "exited" && strings.Contains(v.Status, "(0)") {
			continue
		}
		exp++
		if v.State == "running" && (v.Health == "" || v.Health == "healthy") {
			up++
		} else {
			down = append(down, v.Name)
		}
	}
	h := Card{Label: "Healthy", Value: fmt.Sprintf("%d/%d", up, exp), Sub: "services up and healthy", State: "normal"}
	if exp > 0 {
		h.Hist = x.push("up", 100*float64(up)/float64(exp))
	}
	if len(down) > 0 {
		h.State, h.Sub = "critical", "down: "+strings.Join(down, " ")
	}
	return append(c, ram, net, h)
}

func humanRate(b float64) string {
	units := []string{"B/s", "kB/s", "MB/s", "GB/s"}
	i := 0
	for b >= 1000 && i < len(units)-1 {
		b /= 1000
		i++
	}
	if b >= 100 || i == 0 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

func shieldState(st *stack.Stack) ShieldState {
	sh := st.Def.Shield
	if sh == nil {
		return ShieldState{}
	}
	vals := st.Values()
	return ShieldState{Declared: true, Switch: sh.SwitchVar(), On: sh.On(vals), Addr: sh.Address(vals) + sh.Path(), Proxy: sh.ProxyService()}
}

func orStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func sse(w http.ResponseWriter) (http.Flusher, bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unsupported")
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	return f, true
}

func (s *Server) stackEvents(w http.ResponseWriter, r *http.Request, _ *state) {
	name := r.PathValue("name")
	if _, ok, _ := host.Find(s.Env.State, name); !ok {
		fail(w, 404, "no such stack")
		return
	}
	f, ok := sse(w)
	if !ok {
		return
	}
	x := s.samplers.get(name)
	ch := x.subscribe()
	defer x.unsubscribe(ch)
	f.Flush()
	keep := time.NewTicker(25 * time.Second)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
		case <-keep.C:
			io.WriteString(w, ": keep-alive\n\n")
			f.Flush()
		}
	}
}

// stackLogs streams `docker compose logs -f` of one service (or all) while the page reads it.
func (s *Server) stackLogs(w http.ResponseWriter, r *http.Request, _ *state) {
	st, _, err := s.openStack(r.PathValue("name"))
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	svc := r.URL.Query().Get("service")
	if svc != "" && !st.HasService(svc) {
		fail(w, 404, "unknown service")
		return
	}
	f, ok := sse(w)
	if !ok {
		return
	}
	cmd := st.LogsCmd(r.Context(), svc, true, 200)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "data: %s\n\n", jsonString("! "+err.Error()))
		f.Flush()
		return
	}
	defer func() { _ = cmd.Wait() }()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fmt.Fprintf(w, "data: %s\n\n", jsonString(sc.Text()))
		f.Flush()
	}
}

func jsonString(v string) string { b, _ := json.Marshal(v); return string(b) }

// ───────────────────────── .env ─────────────────────────

// envList is the stack's .env model, secrets masked (revealing one needs a fresh authentication).
func (s *Server) envList(w http.ResponseWriter, r *http.Request, _ *state) {
	st, _, err := s.openStack(r.PathValue("name"))
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	m, err := envmodel.Build(st)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	vars := m.Filter(r.URL.Query().Get("filter"), r.URL.Query().Get("search"))
	if vars == nil {
		vars = []envmodel.Var{}
	}
	for i := range vars {
		if vars[i].Secret {
			vars[i].Value = envmodel.Mask(vars[i].Value)
		}
	}
	writeJSON(w, map[string]any{"vars": vars, "total": len(m.Vars)})
}

func (s *Server) envValidate(w http.ResponseWriter, r *http.Request, _ *state) {
	st, _, err := s.openStack(r.PathValue("name"))
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	var in struct{ Key, Value string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	rule := st.Def.EnvRules.Vars[in.Key]
	problem := ""
	if err := envmodel.Check(&rule, in.Value); err != nil {
		problem = err.Error()
	}
	writeJSON(w, map[string]any{"problem": problem})
}

func (s *Server) envReveal(w http.ResponseWriter, r *http.Request, st0 *state) {
	st, _, err := s.openStack(r.PathValue("name"))
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	var in struct{ Key string }
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "bad request")
		return
	}
	m, err := envmodel.Build(st)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	v, ok := m.Find(in.Key)
	if !ok {
		fail(w, 404, "no such variable")
		return
	}
	s.audit(r, st0.Session, "env.reveal", map[string]any{"stack": r.PathValue("name"), "key": in.Key})
	writeJSON(w, map[string]any{"value": v.Value})
}
