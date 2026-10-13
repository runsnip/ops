package graph

import (
	"regexp"
	"strings"
)

// Directive is one line of a proxy's configuration with its block: `reverse_proxy app:80`, `location /api/ { … }`.
type Directive struct {
	Name  string
	Args  []string
	Block []Directive
}

// tokens splits text into words, keeping quoted strings whole; newline and ; are returned as "\n" and ";".
func tokens(text string, semicolons bool) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	quote := rune(0)
	for _, r := range text {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '"' || r == '\'' || r == '`':
			quote = r
		case r == '\n':
			flush()
			out = append(out, "\n")
		case r == ';' && semicolons:
			flush()
			out = append(out, ";")
		case r == ' ' || r == '\t' || r == '\r':
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

// stripComments drops # comments (a # that starts a word).
func stripComments(text string) string {
	re := regexp.MustCompile(`(?m)(^|[ \t])#.*$`)
	return re.ReplaceAllString(text, "$1")
}

// parseBlocks turns tokens into directives. A directive ends at end (newline for Caddy, ; for nginx); a word `{`
// opens its block and `}` closes it. Braces glued to words ({$VAR}, {uri}) are words.
func parseBlocks(toks []string, end string) []Directive {
	var parse func(i int) ([]Directive, int)
	parse = func(i int) ([]Directive, int) {
		var list []Directive
		var cur []string
		push := func(block []Directive) {
			if len(cur) > 0 || block != nil {
				d := Directive{Block: block}
				if len(cur) > 0 {
					d.Name, d.Args = cur[0], cur[1:]
				}
				list = append(list, d)
			}
			cur = nil
		}
		for i < len(toks) {
			t := toks[i]
			i++
			switch {
			case t == "{":
				var block []Directive
				block, i = parse(i)
				if block == nil {
					block = []Directive{}
				}
				push(block)
			case t == "}":
				push(nil)
				return list, i
			case t == end || t == "\n" || t == ";":
				if t == end {
					push(nil)
				}
			default:
				cur = append(cur, t)
			}
		}
		push(nil)
		return list, i
	}
	list, _ := parse(0)
	return list
}

// ───────────────────────── Caddy ─────────────────────────

// Route is one way a request goes: from hosts (or the proxy itself when nil), on a path, to a target as written.
type Route struct {
	Hosts  []string
	Path   string
	Target string
}

var caddyEnv = regexp.MustCompile(`\{\$([A-Za-z_][A-Za-z0-9_]*)(?::([^}]*))?\}`)

// expandCaddy replaces {$VAR} and {$VAR:default} with .env's values.
func expandCaddy(s string, env map[string]string) string {
	return caddyEnv.ReplaceAllStringFunc(s, func(m string) string {
		p := caddyEnv.FindStringSubmatch(m)
		if v := env[p[1]]; v != "" {
			return v
		}
		return p[2]
	})
}

// CaddyRoutes reads a Caddyfile: each site's addresses and where its reverse_proxy directives send requests (inside
// handle/route blocks and imported snippets), with the path matcher as the label. Also the global options' asks
// (on_demand_tls ask) as routes from the proxy itself.
func CaddyRoutes(text string, env map[string]string) (routes []Route, sites [][]string) {
	top := parseBlocks(tokens(stripComments(text), false), "\n")
	snippets := map[string][]Directive{}
	for _, d := range top {
		if strings.HasPrefix(d.Name, "(") && strings.HasSuffix(d.Name, ")") {
			snippets[strings.Trim(d.Name, "()")] = d.Block
		}
	}
	matchers := map[string]string{}
	var walk func(hosts []string, path string, ds []Directive, depth int)
	walk = func(hosts []string, path string, ds []Directive, depth int) {
		if depth > 8 {
			return
		}
		for _, d := range ds {
			args := make([]string, len(d.Args))
			for i, a := range d.Args {
				args[i] = expandCaddy(a, env)
			}
			switch d.Name {
			case "import":
				if len(args) > 0 {
					walk(hosts, path, snippets[args[0]], depth+1)
				}
			case "reverse_proxy":
				p, targets := path, args
				if len(targets) > 0 && (strings.HasPrefix(targets[0], "/") || strings.HasPrefix(targets[0], "@") || targets[0] == "*") {
					p = matcherLabel(targets[0], matchers)
					targets = targets[1:]
				}
				for _, sub := range d.Block {
					if sub.Name == "to" {
						for _, a := range sub.Args {
							targets = append(targets, expandCaddy(a, env))
						}
					}
				}
				for _, t := range targets {
					routes = append(routes, Route{Hosts: hosts, Path: p, Target: t})
				}
			case "handle", "handle_path", "route":
				p := path
				if len(args) > 0 {
					p = matcherLabel(args[0], matchers)
				}
				walk(hosts, p, d.Block, depth+1)
			case "ask":
				if len(args) > 0 {
					routes = append(routes, Route{Path: "tls ask", Target: args[0]})
				}
			default:
				if strings.HasPrefix(d.Name, "@") && len(args) > 0 {
					label := strings.Join(args, " ")
					if args[0] == "path" {
						label = strings.Join(args[1:], " ")
					}
					matchers[d.Name] = label
				}
				if d.Block != nil {
					walk(hosts, path, d.Block, depth+1)
				}
			}
		}
	}
	for i, d := range top {
		if d.Block == nil || strings.HasPrefix(d.Name, "(") {
			continue
		}
		if d.Name == "" && i == 0 { // global options
			walk(nil, "", d.Block, 0)
			continue
		}
		var hosts []string
		for _, a := range append([]string{d.Name}, d.Args...) {
			for _, h := range strings.Split(a, ",") {
				if h = strings.TrimSpace(expandCaddy(h, env)); h != "" {
					hosts = append(hosts, siteHost(h))
				}
			}
		}
		sites = append(sites, hosts)
		walk(hosts, "", d.Block, 0)
	}
	return routes, sites
}

func matcherLabel(m string, matchers map[string]string) string {
	if l, ok := matchers[m]; ok {
		return l
	}
	return m
}

// siteHost is an address's host: https://a.b:443 → a.b; :80 and https:// (any host) → "*".
func siteHost(a string) string {
	if i := strings.Index(a, "://"); i >= 0 {
		a = a[i+3:]
	}
	a = strings.TrimSuffix(a, "/")
	if i := strings.LastIndex(a, ":"); i >= 0 && !strings.Contains(a[i:], "]") {
		a = a[:i]
	}
	if a == "" {
		return "*"
	}
	return a
}

// ───────────────────────── nginx ─────────────────────────

var nginxEnv = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// NginxRoutes reads nginx configuration (templates included: ${VAR} from .env): each server's names and where its
// locations proxy to, following upstream blocks, `set $var target` and `map $x $var { pattern target; }`.
func NginxRoutes(text string, env map[string]string) []Route {
	text = nginxEnv.ReplaceAllStringFunc(stripComments(text), func(m string) string { return env[m[2:len(m)-1]] })
	top := parseBlocks(tokens(text, true), ";")
	// http-level blocks may wrap servers; flatten one level.
	var all []Directive
	var flat func(ds []Directive)
	flat = func(ds []Directive) {
		for _, d := range ds {
			all = append(all, d)
			if d.Name == "http" {
				flat(d.Block)
			}
		}
	}
	flat(top)
	upstreams := map[string][]string{}
	maps := map[string][][2]string{} // $var → (pattern, target)
	for _, d := range all {
		switch {
		case d.Name == "upstream" && len(d.Args) > 0:
			for _, s := range d.Block {
				if s.Name == "server" && len(s.Args) > 0 {
					upstreams[d.Args[0]] = append(upstreams[d.Args[0]], s.Args[0])
				}
			}
		case d.Name == "map" && len(d.Args) == 2:
			for _, s := range d.Block {
				if len(s.Args) > 0 && s.Name != "default" && s.Name != "hostnames" {
					maps[d.Args[1]] = append(maps[d.Args[1]], [2]string{s.Name, s.Args[0]})
				}
			}
		}
	}
	var routes []Route
	for _, srv := range all {
		if srv.Name != "server" {
			continue
		}
		var hosts []string
		for _, s := range srv.Block {
			if s.Name == "server_name" {
				for _, h := range s.Args {
					if h != "_" && h != "" {
						hosts = append(hosts, h)
					}
				}
			}
		}
		var walk func(ds []Directive, path string, vars map[string]string)
		walk = func(ds []Directive, path string, vars map[string]string) {
			local := map[string]string{}
			for k, v := range vars {
				local[k] = v
			}
			for _, d := range ds {
				if d.Name == "set" && len(d.Args) == 2 {
					local[d.Args[0]] = d.Args[1]
				}
			}
			for _, d := range ds {
				switch d.Name {
				case "location":
					p := ""
					if len(d.Args) > 0 {
						p = d.Args[len(d.Args)-1]
					}
					walk(d.Block, p, local)
				case "if":
					walk(d.Block, path, local)
				case "proxy_pass", "grpc_pass", "fastcgi_pass", "uwsgi_pass":
					if len(d.Args) == 0 {
						continue
					}
					for _, t := range resolveNginx(d.Args[0], local, upstreams, maps) {
						label := path
						if t[0] != "" {
							label = t[0]
						}
						routes = append(routes, Route{Hosts: hosts, Path: label, Target: t[1]})
					}
				}
			}
		}
		walk(srv.Block, "", nil)
	}
	return routes
}

var nginxVar = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)

// resolveNginx turns a proxy_pass argument into targets: (label from a map pattern, target).
func resolveNginx(arg string, vars map[string]string, upstreams map[string][]string, maps map[string][][2]string) [][2]string {
	if v := nginxVar.FindString(arg); v != "" {
		if val, ok := vars[v]; ok {
			return resolveNginx(strings.Replace(arg, v, val, 1), nil, upstreams, maps)
		}
		if m, ok := maps[v]; ok {
			var out [][2]string
			for _, pt := range m {
				for _, t := range resolveNginx(strings.Replace(arg, v, pt[1], 1), nil, upstreams, maps) {
					out = append(out, [2]string{strings.TrimPrefix(strings.TrimPrefix(pt[0], "~*"), "~"), t[1]})
				}
			}
			return out
		}
		return nil
	}
	host := arg
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host, _, _ = strings.Cut(host, "/")
	if ups, ok := upstreams[host]; ok {
		var out [][2]string
		for _, u := range ups {
			out = append(out, [2]string{"", u})
		}
		return out
	}
	return [][2]string{{"", arg}}
}
