// Package graph is `runsnip graph`: a stack as nodes and edges — its public hostnames, proxies, services, datastores
// and the outside hosts it talks to — read from what the stack declares (the compose file, the proxies' config files,
// .env and stack.yaml), never from names known in advance. The web page draws exactly this.
package graph

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/runsnip/ops/internal/compose"
	"github.com/runsnip/ops/internal/stack"
)

// Node kinds, left to right.
const (
	KindHost     = "host"     // a public hostname the proxies serve
	KindProxy    = "proxy"    // caddy, nginx, traefik…
	KindApp      = "app"      // any other service
	KindData     = "data"     // a datastore (by image, or stack.yaml)
	KindExternal = "external" // a host outside the stack it calls
	KindRegistry = "registry" // where its images come from
)

// Layers are the columns' bands, left to right.
var Layers = []string{"ingress", "edge", "app", "data", "external"}

// Edge kinds.
const (
	EdgeRoute   = "route"   // hostname → proxy → proxy → service, labelled with the path
	EdgeDepends = "depends" // compose depends_on
	EdgeCalls   = "calls"   // a service's environment points at another service or an outside host
	EdgeData    = "data"    // … at a datastore
)

// Node is one box.
type Node struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Layer     string   `json:"layer"`
	Col       int      `json:"col"`
	Row       int      `json:"row"`
	Group     string   `json:"group,omitempty"`
	Icon      string   `json:"icon"`
	Image     string   `json:"image,omitempty"`
	Service   string   `json:"service,omitempty"`   // the compose service (live state joins on it)
	Component string   `json:"component,omitempty"` // the released component it belongs to (pin, rollback)
	Networks  []string `json:"networks,omitempty"`
	Hosts     []string `json:"hosts,omitempty"` // an external node's hosts (one registrable domain)
	Off       string   `json:"off,omitempty"`   // why compose does not start it (profile switched off)
}

// Edge is one arrow.
type Edge struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Kind   string   `json:"kind"`
	Labels []string `json:"labels,omitempty"`
	Via    []string `json:"via,omitempty"` // the variables a calls/data edge was read from
}

// Network is a compose network and the services on it (the optional overlay).
type Network struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// Graph is the whole picture.
type Graph struct {
	Stack    string    `json:"stack"`
	Title    string    `json:"title"`
	Accent   string    `json:"accent,omitempty"`
	Layers   []string  `json:"layers"`
	Nodes    []Node    `json:"nodes"`
	Edges    []Edge    `json:"edges"`
	Networks []Network `json:"networks"`
	Notes    []string  `json:"notes,omitempty"`
}

// datastoreImages are images known to hold data. stack.yaml's graph.datastores adds to them.
var datastoreImages = []string{"postgres", "postgis", "pgvector", "timescaledb", "mysql", "mariadb", "percona", "mongo", "mongodb",
	"redis", "valkey", "keydb", "dragonfly", "memcached", "clickhouse", "clickhouse-server", "elasticsearch",
	"opensearch", "meilisearch", "typesense", "qdrant", "weaviate", "minio", "rabbitmq", "nats", "kafka", "redpanda",
	"cassandra", "scylla", "influxdb", "cockroach", "etcd", "neo4j", "couchdb", "versitygw", "garage", "seaweedfs"}

// Build reads the stack. vals is .env as it is.
func Build(st *stack.Stack) (*Graph, error) {
	vals := st.Values()
	g := &Graph{Stack: st.Def.Name, Title: st.Title(), Layers: Layers}
	if g.Stack == "" {
		g.Stack = st.ProjectName()
	}
	if st.Def.Graph != nil {
		g.Accent = st.Def.Graph.Accent
	}
	b := &builder{st: st, g: g, vals: vals, nodes: map[string]*Node{}, edges: map[string]*Edge{}, names: map[string]string{}}
	b.services()
	b.routes()
	b.dependsOn()
	b.calls()
	b.registries()
	b.layout()
	b.finish()
	return g, nil
}

type builder struct {
	st    *stack.Stack
	g     *Graph
	vals  map[string]string
	nodes map[string]*Node
	order []string
	edges map[string]*Edge
	eord  []string
	names map[string]string // every name a service answers to → its node id
	own   map[string]bool   // the stack's own public hostnames
}

func (b *builder) add(n Node) *Node {
	if x, ok := b.nodes[n.ID]; ok {
		return x
	}
	b.nodes[n.ID] = &n
	b.order = append(b.order, n.ID)
	return b.nodes[n.ID]
}

func (b *builder) edge(from, to, kind, label, via string) {
	if from == "" || to == "" || from == to {
		return
	}
	k := from + "\x00" + to + "\x00" + kind
	e, ok := b.edges[k]
	if !ok {
		e = &Edge{From: from, To: to, Kind: kind}
		b.edges[k] = e
		b.eord = append(b.eord, k)
	}
	if label != "" && !contains(e.Labels, label) {
		e.Labels = append(e.Labels, label)
	}
	if via != "" && !contains(e.Via, via) {
		e.Via = append(e.Via, via)
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

// imageBase is an image's own name: ghcr.io/o/x:1 → x, postgres:16-alpine → postgres, ${P}-api:${T} → …-api.
func imageBase(img string) string {
	img = strings.TrimSpace(img)
	if i := strings.LastIndex(img, "/"); i >= 0 {
		img = img[i+1:]
	}
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	if i := strings.LastIndex(img, ":"); i >= 0 {
		img = img[:i]
	}
	return strings.ToLower(img)
}

func (b *builder) services() {
	c := b.st.Compose
	proxies := map[string]bool{}
	for _, p := range b.st.Proxies() {
		proxies[p] = true
	}
	var stores []string
	stores = append(stores, datastoreImages...)
	if b.st.Def.Graph != nil {
		stores = append(stores, b.st.Def.Graph.Datastores...)
	}
	comp := map[string]string{}
	for _, cp := range b.st.Components() {
		for _, s := range cp.Services {
			comp[s] = cp.Name
		}
	}
	for _, name := range c.Order {
		s := c.Services[name]
		img := b.st.ExpandWith(s.Image, b.vals)
		n := Node{ID: "svc:" + name, Kind: KindApp, Name: name, Service: name, Image: img, Component: comp[name], Networks: s.Networks}
		base := imageBase(img)
		switch {
		case proxies[name]:
			n.Kind = KindProxy
		case contains(stores, base) || contains(stores, strings.TrimSuffix(base, "-alpine")):
			n.Kind = KindData
		}
		if on, why := b.st.Enabled(name, b.vals); !on {
			n.Off = why
		}
		n.Icon = iconFor(n.Kind, base)
		if o := b.st.Def.Services[name].Graph; o != nil {
			if o.Hide {
				continue
			}
			if o.Kind != "" {
				n.Kind = o.Kind
			}
			n.Layer, n.Group = o.Layer, o.Group
			if o.Icon != "" {
				n.Icon = o.Icon
			}
		}
		b.add(n)
		for _, alias := range append([]string{name, s.ContainerName}, s.Aliases...) {
			if alias != "" {
				b.names[strings.ToLower(alias)] = n.ID
			}
		}
	}
	nets := map[string][]string{}
	for _, name := range c.Order {
		for _, nw := range c.Services[name].Networks {
			nets[nw] = append(nets[nw], "svc:"+name)
		}
	}
	for _, nw := range compose.Sorted(keys(nets)) {
		b.g.Networks = append(b.g.Networks, Network{Name: nw, Members: nets[nw]})
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// proxyFiles are the configuration files a proxy service mounts from the infra folder (a mounted folder: its files).
func (b *builder) proxyFiles(svc string) []string {
	var out []string
	for _, bind := range b.st.Compose.Services[svc].Binds {
		src := bind.Source
		if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") && filepath.IsAbs(src) {
			continue // a data folder, not the checkout's configuration
		}
		p := filepath.Join(b.st.Dir, src)
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			ents, _ := os.ReadDir(p)
			for _, e := range ents {
				if !e.IsDir() {
					out = append(out, filepath.Join(p, e.Name()))
				}
			}
		} else {
			out = append(out, p)
		}
	}
	return out
}

func (b *builder) routes() {
	b.own = map[string]bool{}
	type found struct {
		proxy  string
		routes []Route
	}
	var all []found
	for _, id := range b.order {
		n := b.nodes[id]
		if n.Kind != KindProxy {
			continue
		}
		var rs []Route
		for _, f := range b.proxyFiles(n.Service) {
			body, err := os.ReadFile(f)
			if err != nil || len(body) > 4<<20 {
				continue
			}
			text := string(body)
			base := strings.ToLower(filepath.Base(f))
			switch {
			case strings.Contains(base, "caddyfile"):
				r, sites := CaddyRoutes(text, b.vals)
				rs = append(rs, r...)
				for _, hs := range sites {
					for _, h := range hs {
						b.own[h] = true
					}
				}
			case strings.Contains(text, "server {") || strings.Contains(text, "location ") || strings.Contains(text, "upstream "):
				rs = append(rs, NginxRoutes(text, b.vals)...)
			}
		}
		all = append(all, found{n.ID, rs})
	}
	// A proxy another proxy routes to is behind it: hostnames enter the stack at the proxies nobody routes to.
	behind := map[string]bool{}
	for _, f := range all {
		for _, r := range f.routes {
			if to := b.resolve(r.Target); to != f.proxy && b.nodes[to] != nil && b.nodes[to].Kind == KindProxy {
				behind[to] = true
			}
		}
	}
	for _, f := range all {
		for _, r := range f.routes {
			to := b.resolve(r.Target)
			if to == "" {
				continue
			}
			if len(r.Hosts) == 0 {
				b.edge(f.proxy, to, EdgeRoute, r.Path, "")
				continue
			}
			for _, h := range r.Hosts {
				label := r.Path
				if behind[f.proxy] {
					// Behind another proxy the host is a label (the front proxy already has the hostname node).
					if h != "*" {
						label = strings.TrimSpace(h + " " + r.Path)
					}
				} else {
					b.edge(b.host(h), f.proxy, EdgeRoute, "", "")
				}
				b.edge(f.proxy, to, EdgeRoute, label, "")
			}
		}
	}
}

func (b *builder) host(h string) string {
	return b.add(Node{ID: "host:" + h, Kind: KindHost, Name: h, Icon: iconFor(KindHost, "")}).ID
}

var ipRe = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

// resolve maps a target as written (http://api:80/x, nginx:80, postgres://u:p@db:5432/x, https://api.stripe.com)
// to a node: a service by any of its names, one of the stack's own hostnames, or an outside host.
func (b *builder) resolve(target string) string {
	h := hostOf(target)
	if h == "" {
		return ""
	}
	if id, ok := b.names[strings.ToLower(h)]; ok {
		return id
	}
	if b.own[h] {
		return "host:" + h
	}
	if h == "localhost" || strings.HasPrefix(h, "127.") || h == "0.0.0.0" || !strings.Contains(h, ".") {
		return ""
	}
	return b.external(h)
}

// hostOf is the host in a URL or host:port; "" when there is none.
func hostOf(t string) string {
	t = strings.TrimSpace(t)
	if t == "" || strings.ContainsAny(t, " {}$") {
		return ""
	}
	if strings.Contains(t, "://") {
		u, err := url.Parse(t)
		if err != nil {
			return ""
		}
		return strings.ToLower(u.Hostname())
	}
	if h, _, err := net.SplitHostPort(t); err == nil {
		return strings.ToLower(h)
	}
	t, _, _ = strings.Cut(t, "/")
	return strings.ToLower(t)
}

// registrable is the part of a hostname one registers: api.github.com → github.com, x.r2.cloudflarestorage.com →
// cloudflarestorage.com, a.b.co.uk → b.co.uk; an IP stays itself.
func registrable(h string) string {
	if ipRe.MatchString(h) || strings.Contains(h, ":") {
		return h
	}
	p := strings.Split(strings.TrimSuffix(h, "."), ".")
	if len(p) <= 2 {
		return h
	}
	// Two-level public suffixes: a short second level under a country code (co.uk, com.vn, gov.au…).
	if len(p[len(p)-1]) == 2 && len(p[len(p)-2]) <= 3 {
		return strings.Join(p[len(p)-3:], ".")
	}
	return strings.Join(p[len(p)-2:], ".")
}

func (b *builder) external(h string) string {
	dom := registrable(h)
	if b.st.Def.Graph != nil && (contains(b.st.Def.Graph.Ignore, h) || contains(b.st.Def.Graph.Ignore, dom)) {
		return ""
	}
	kind := KindExternal
	icon := iconFor(KindExternal, "")
	if ipRe.MatchString(dom) {
		icon = "server"
	}
	n := b.add(Node{ID: "ext:" + dom, Kind: kind, Name: dom, Icon: icon})
	if !contains(n.Hosts, h) {
		n.Hosts = append(n.Hosts, h)
		sort.Strings(n.Hosts)
	}
	return n.ID
}

func (b *builder) dependsOn() {
	for _, name := range b.st.Compose.Order {
		for _, d := range b.st.Compose.Services[name].DependsOn {
			if _, ok := b.nodes["svc:"+d]; ok {
				if _, ok := b.nodes["svc:"+name]; ok {
					b.edge("svc:"+name, "svc:"+d, EdgeDepends, "", "")
				}
			}
		}
	}
}

var (
	urlRe      = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s,;"'<>]+`)
	hostKey    = regexp.MustCompile(`(?i)(HOST|HOSTNAME|ADDR|ADDRESS|SERVER|SERVERS|ENDPOINT|BROKERS?|NODES?)$`)
	hostPortRe = regexp.MustCompile(`(?:^|[\s,;=@])([a-zA-Z0-9][a-zA-Z0-9_.-]*):(\d{2,5})(?:$|[\s,;/])`)
)

// calls: what each service's environment (compose's environment:, interpolated with .env) points at.
func (b *builder) calls() {
	for _, name := range b.st.Compose.Order {
		from := "svc:" + name
		if _, ok := b.nodes[from]; !ok {
			continue
		}
		s := b.st.Compose.Services[name]
		for _, k := range s.EnvKeys {
			v := b.st.ExpandWith(s.Env[k], b.vals)
			var targets []string
			targets = append(targets, urlRe.FindAllString(v, -1)...)
			for _, m := range hostPortRe.FindAllStringSubmatch(urlRe.ReplaceAllString(v, " "), -1) {
				targets = append(targets, m[1]+":"+m[2])
			}
			// A bare name is a host only under a key that says so (DB_HOST=db, not DB_TYPE=postgres).
			if hostKey.MatchString(k) {
				if _, ok := b.names[strings.ToLower(strings.TrimSpace(v))]; ok {
					targets = append(targets, strings.TrimSpace(v))
				}
			}
			for _, t := range targets {
				to := b.resolve(t)
				if to == "" || strings.HasPrefix(to, "host:") {
					continue // a link to its own public site is not a dependency
				}
				kind := EdgeCalls
				if n := b.nodes[to]; n != nil && n.Kind == KindData {
					kind = EdgeData
				}
				b.edge(from, to, kind, "", k)
			}
		}
	}
}

// registries: where the images come from, one node per registry host.
func (b *builder) registries() {
	for _, id := range b.order {
		n := b.nodes[id]
		if n.Image == "" || n.Kind == KindHost {
			continue
		}
		first, _, ok := strings.Cut(n.Image, "/")
		if !ok || !(strings.Contains(first, ".") || strings.Contains(first, ":")) || first == "localhost" {
			continue
		}
		b.add(Node{ID: "reg:" + first, Kind: KindRegistry, Name: first, Icon: iconFor(KindRegistry, "")})
	}
}

func (b *builder) finish() {
	for _, id := range b.order {
		b.g.Nodes = append(b.g.Nodes, *b.nodes[id])
	}
	sort.SliceStable(b.g.Nodes, func(i, j int) bool {
		a, c := b.g.Nodes[i], b.g.Nodes[j]
		if a.Col != c.Col {
			return a.Col < c.Col
		}
		return a.Row < c.Row
	})
	for _, k := range b.eord {
		b.g.Edges = append(b.g.Edges, *b.edges[k])
	}
	if b.g.Edges == nil {
		b.g.Edges = []Edge{}
	}
	if b.g.Networks == nil {
		b.g.Networks = []Network{}
	}
}

// Text prints the graph for a terminal: columns as sections, each node with what it points at.
func Text(w io.Writer, g *Graph) {
	out := map[string][]Edge{}
	for _, e := range g.Edges {
		out[e.From] = append(out[e.From], e)
	}
	name := map[string]string{}
	for _, n := range g.Nodes {
		name[n.ID] = n.Name
	}
	layer := ""
	for _, n := range g.Nodes {
		if n.Layer != layer {
			layer = n.Layer
			fmt.Fprintf(w, "\n%s\n", strings.ToUpper(layer))
		}
		extra := ""
		if n.Off != "" {
			extra = "  (off: " + n.Off + ")"
		}
		fmt.Fprintf(w, "  %-28s %s%s\n", n.Name, n.Kind, extra)
		for _, e := range out[n.ID] {
			lbl := ""
			if len(e.Labels) > 0 {
				lbl = "  " + strings.Join(e.Labels, ", ")
			}
			if len(e.Via) > 0 {
				lbl += "  [" + strings.Join(e.Via, " ") + "]"
			}
			fmt.Fprintf(w, "      %-8s → %s%s\n", e.Kind, name[e.To], lbl)
		}
	}
}
