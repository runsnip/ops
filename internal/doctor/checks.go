package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/envfile"
	"github.com/runsnip/ops/internal/keygen"
	"github.com/runsnip/ops/internal/run"
	"github.com/runsnip/ops/internal/sysinfo"
)

// Types documents every check type and the fields it reads (`runsnip validate` and STANDARD.md use it).
var Types = map[string]string{
	"env_mode":         "standard: .env is readable by its owner only (600)",
	"env_complete":     "standard: every variable of the env example is in .env",
	"placeholders":     "standard: no value is still the env rules' placeholder (CHANGE_ME)",
	"versions":         "standard: each enabled component runs the version .env pins",
	"containers":       "standard: every enabled service is running; disabled ones say why (notes: service → consequence)",
	"health_status":    "standard: compose health checks are healthy (services: limit the list; restart counts shown)",
	"config_fresh":     "standard: no file mounted into a proxy is newer than its container",
	"pg_budget":        "standard: the services' Postgres pools plus a reserve fit max_connections (pool_env, url_env, reserve, default_pool, reads)",
	"shield_route":     "standard: with the shield switch on, the address Caddy forwards every request to answers",
	"env_set":          "keys (all set) and/or any (one of them set)",
	"env_min_len":      "key has at least min characters",
	"env_differs":      "key's value differs from other's",
	"env_suffix":       "key, when set, ends with suffix",
	"isolated_domains": "base and others: no domain of others is base, inside it, or holds it",
	"log_color":        "LOG_FORMAT, LOG_COLOR and FORCE_COLOR make readable, coloured logs",
	"dns":              "hosts (with if) resolve to this machine; proxied may differ; skip_env skips the section",
	"exec":             "cmd in service (or each of services, {service} in cmd): cases match its output, else its exit status decides",
	"http":             "from a service's container (client wget or node): url, host header, expect / reject codes, codes → messages",
	"https_headers":    "HEAD url (connect_env: curl --connect-to): absent headers, contains {header: text}",
	"sql":              "query returns lines \"level|text\" (pass, fail, note, warn, info)",
	"logs":             "pattern in service's logs (tail lines): present true/false; {count}",
	"ask":              "an on-demand TLS ask endpoint: from, url with {host}, probes",
	"settings_or_env":  "items each kept in the settings table (query lists keys) or .env; modes: prefix of key → label",
	"key_pairs":        "keys: <P>_JWT_KEY's public half is <P>_PUBLIC_KEY",
	"hkdf":             "key equals HKDF-SHA256(from, info), base64url",
	"kid_pinned":       "the first kid:private of key has its kid:public in other",
	"compare":          "left op right (numbers; 1g = 1024 MB; {cores}, {ram_mb}), right × factor",
	"path_owner":       "path is owned by uid",
	"file_present":     "path is a non-empty file; glob lists what is there",
	"info":             "prints note (level: info by default, or note/warn)",
	"docker":           "standard: the Docker engine answers",
	"sysctl":           "key (a kernel setting, e.g. vm.max_map_count) is at least min",
	"registry":         "docker is logged in to the registry and the default component's pinned image is readable",
}

type checkFn func(d *Doctor, c def.Check)

var checks map[string]checkFn

func init() {
	checks = map[string]checkFn{
		"env_mode": envMode, "env_complete": envComplete, "placeholders": placeholders, "versions": versions,
		"containers": containers, "health_status": healthStatus, "config_fresh": configFresh, "pg_budget": pgBudget,
		"shield_route": shieldRoute, "env_set": envSet, "env_min_len": envMinLen, "env_differs": envDiffers,
		"env_suffix": envSuffix, "isolated_domains": isolatedDomains, "log_color": logColor, "dns": dnsCheck,
		"exec": execCheck, "http": httpCheck, "https_headers": httpsHeaders, "sql": sqlCheck, "logs": logsCheck,
		"ask": askCheck, "settings_or_env": settingsOrEnv, "key_pairs": keyPairs, "hkdf": hkdfCheck,
		"kid_pinned": kidPinned, "compare": compare, "path_owner": pathOwner, "file_present": filePresent,
		"docker": dockerCheck, "sysctl": sysctlCheck, "registry": registryCheck,
		"info": func(d *Doctor, c def.Check) {
			l := Info
			if c.Level != "" {
				l = failLevel(c)
			}
			d.add(l, d.text(c.Note, nil))
		},
	}
}

func keysOf(c def.Check) []string {
	if c.Key != "" {
		return append([]string{c.Key}, c.Keys...)
	}
	return c.Keys
}

// ───────────────────────── .env ─────────────────────────

func envMode(d *Doctor, c def.Check) {
	st, err := d.Stat(d.St.EnvPath)
	if err != nil {
		d.add(Fail, err.Error())
		return
	}
	m := st.Mode().Perm()
	d.verdict(m&0o077 == 0, c, ".env is readable by its owner only ({mode})", ".env is readable by others ({mode}): chmod 600 .env", map[string]string{"mode": fmt.Sprintf("%o", m)})
}

func envComplete(d *Doctor, c def.Check) {
	ex, err := envfile.Load(d.St.ExamplePath)
	if err != nil {
		d.add(Note, "no env example to compare .env with ("+filepath.Base(d.St.ExamplePath)+")")
		return
	}
	env, _ := envfile.Load(d.St.EnvPath)
	var missing []string
	for _, k := range ex.Keys() {
		if _, ok := env.Lookup(k); !ok {
			missing = append(missing, k)
		}
	}
	// .env is the server's own: `infra pull` never overwrites it, so a variable added to the example is silently
	// absent here, and compose runs on with an empty string.
	d.verdict(len(missing) == 0, c, ".env has every variable of "+filepath.Base(d.St.ExamplePath),
		".env lacks variables "+filepath.Base(d.St.ExamplePath)+" has: {keys} — runsnip env combine", map[string]string{"keys": strings.Join(missing, " ")})
}

func placeholders(d *Doctor, c def.Check) {
	p := d.St.Def.EnvRules.Placeholder
	if p == "" {
		return
	}
	env, _ := envfile.Load(d.St.EnvPath)
	keys := env.Has(p)
	d.verdict(len(keys) == 0, c, "no value is still "+p, "still "+p+": {keys} — runsnip env generate fills the generated ones", map[string]string{"keys": strings.Join(keys, " ")})
}

func envSet(d *Doctor, c def.Check) {
	var missing []string
	for _, k := range keysOf(c) {
		if d.vals[k] == "" {
			missing = append(missing, k)
		}
	}
	anyOK := len(c.Any) == 0
	for _, k := range c.Any {
		anyOK = anyOK || d.vals[k] != ""
	}
	all := append(append([]string{}, keysOf(c)...), c.Any...)
	d.verdict(len(missing) == 0 && anyOK, c, "{keys} set", "{missing} empty", map[string]string{
		"keys": strings.Join(keysOf(c), ", "), "missing": strings.TrimSpace(strings.Join(missing, " ") + map[bool]string{true: "", false: " (and none of " + strings.Join(c.Any, ", ") + ")"}[anyOK]),
		"all": strings.Join(all, ", ")})
}

func envMinLen(d *Doctor, c def.Check) {
	v := d.vals[c.Key]
	d.verdict(len(v) >= c.Min, c, c.Key+" is long enough", c.Key+" is empty or shorter than {min} characters", map[string]string{"min": strconv.Itoa(c.Min), "len": strconv.Itoa(len(v))})
}

func envDiffers(d *Doctor, c def.Check) {
	d.verdict(d.vals[c.Key] != d.vals[c.Other], c, c.Key+" differs from "+c.Other, c.Key+" equals "+c.Other+" — it must be a key of its own", nil)
}

func envSuffix(d *Doctor, c def.Check) {
	v := d.vals[c.Key]
	if v == "" {
		return
	}
	d.verdict(strings.HasSuffix(v, c.Suffix), c, "", c.Key+" does not end in "+c.Suffix, nil)
}

// Nested reports which of others share base's domain (base itself, inside it, or holding it).
func Nested(base string, others []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range others {
		if o == "" || seen[o] {
			continue
		}
		seen[o] = true
		if o == base || strings.HasSuffix(base, "."+o) || strings.HasSuffix(o, "."+base) {
			out = append(out, o)
		}
	}
	return out
}

func (d *Doctor) expandAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = d.expand(x)
	}
	return out
}

func isolatedDomains(d *Doctor, c def.Check) {
	base, others := d.expand(c.Base), d.expandAll(c.Others)
	bad := Nested(base, others)
	var shown []string
	for _, o := range others {
		if o != "" {
			shown = append(shown, o)
		}
	}
	d.verdict(len(bad) == 0, c, "domains apart (base "+base+" | "+strings.Join(dedupe(shown), " | ")+")",
		"share the domain of "+base+": {bad} — pages there could read the app's cookies", map[string]string{"bad": strings.Join(bad, " ")})
}

func dedupe(xs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// logColor: plain logs are not an error, but people reading them take them for one. App logs are coloured only in
// PRETTY format with LOG_COLOR (no TTY in a container, so auto means none); Next.js's own follow FORCE_COLOR.
func logColor(d *Doctor, c def.Check) {
	f, col, force := pick(d.vals["LOG_FORMAT"], "JSON"), pick(d.vals["LOG_COLOR"], "auto"), d.vals["FORCE_COLOR"]
	switch {
	case f != "PRETTY":
		d.add(Note, "app logs: "+f+", no colour (only PRETTY is coloured). To read them coloured: LOG_FORMAT=PRETTY LOG_COLOR=ansi FORCE_COLOR=1")
	case col == "none" || col == "auto" && (force == "" || force == "0"):
		d.add(Note, "app logs: PRETTY but not coloured (LOG_COLOR="+col+", no TTY). Set LOG_COLOR=ansi or FORCE_COLOR=1")
	default:
		d.add(Pass, "app logs: PRETTY, coloured")
	}
	if force == "" {
		d.add(Note, "Next.js logs: no colour — FORCE_COLOR is empty (1 turns it on; do not set 0, Next takes it as on)")
	}
}

// ───────────────────────── compose ─────────────────────────

func versions(d *Doctor, c def.Check) {
	for _, comp := range d.St.Components() {
		if ok, _ := d.St.Enabled(comp.Service, d.vals); !ok {
			continue
		}
		want := d.St.Tag(comp, d.vals)
		img := d.ps[comp.Service].Image
		run := ""
		if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
			run = img[i+1:]
		} else if img != "" {
			run = "latest"
		}
		switch {
		case img == "":
			d.add(Fail, fmt.Sprintf("%s: no container to read the image of (.env pins %s)", comp.Name, want))
		case run == want:
			d.add(Pass, comp.Name+": "+run)
		default:
			// The pin was changed and the old `restart` used: .env says one thing, the container runs another.
			d.add(Fail, fmt.Sprintf("%s: .env pins %s, %s runs — runsnip restart %s", comp.Name, want, run, comp.Name))
		}
	}
}

func containers(d *Doctor, c def.Check) {
	if d.psErr != nil {
		d.add(Fail, "docker compose ps: "+d.psErr.Error())
		return
	}
	for _, n := range d.St.Compose.Order {
		if ok, why := d.St.Enabled(n, d.vals); !ok {
			if extra := c.Notes[n]; extra != "" {
				why += " — " + extra
			}
			d.add(Note, n+": "+why)
		}
	}
	for _, n := range d.St.EnabledServices(d.vals) {
		st := d.ps[n].State
		if st == "running" {
			d.add(Pass, n+": running")
		} else if st == "exited" && strings.Contains(d.ps[n].Status, "(0)") && !d.St.Compose.Services[n].Healthcheck {
			d.add(Pass, n+": done (a one-shot job)")
		} else {
			d.add(Fail, n+": "+pick(st, "not running"))
		}
	}
}

func healthStatus(d *Doctor, c def.Check) {
	names := c.Services
	if c.Service != "" {
		names = append([]string{c.Service}, names...)
	}
	explicit := len(names) > 0
	if !explicit {
		for _, n := range d.St.EnabledServices(d.vals) {
			if d.St.Compose.Services[n].Healthcheck {
				names = append(names, n)
			}
		}
	}
	var healthy []string
	for _, n := range names {
		p := d.ps[n]
		restarts := d.inspect(p.Container, "{{.RestartCount}}")
		extra := map[string]string{"service": n, "restarts": pick(restarts, "?"), "state": pick(p.Health, p.State)}
		switch {
		case (p.Container == "" || p.State != "running") && !explicit:
			// Not running: the containers check has said so already.
		case p.Container == "" || p.State != "running":
			d.add(failLevel(c), d.text(pick(c.Fail, "{service} is not running — runsnip logs {service}"), extra))
		case p.Health == "healthy":
			if explicit {
				d.add(Pass, d.text(pick(c.Pass, "{service} healthy (restarted {restarts} times)"), extra))
			} else {
				healthy = append(healthy, n)
			}
		case p.Health == "starting":
			d.add(Note, n+" is starting")
		case p.Health == "":
			d.add(Note, n+": no health check result")
		default:
			d.add(failLevel(c), d.text(pick(c.Fail, "{service} {state} (restarted {restarts} times) — runsnip logs {service}"), extra))
		}
	}
	if len(healthy) > 0 {
		d.add(Pass, fmt.Sprintf("%d health checks healthy: %s", len(healthy), strings.Join(healthy, " ")))
	}
}

// configFresh: a proxy reads its mounted configuration once, when its container is created; `up -d` does not look
// at the file's content. A file newer than the container means it runs the old one (nginx once served five weeks of
// routes to two removed services that way).
func configFresh(d *Doctor, c def.Check) {
	stale := false
	for _, p := range d.St.Proxies() {
		ct := d.ps[p].Container
		if ct == "" {
			continue
		}
		born, err := time.Parse(time.RFC3339Nano, d.inspect(ct, "{{.Created}}"))
		if err != nil {
			continue
		}
		for _, b := range d.St.Compose.Services[p].Binds {
			if !strings.HasPrefix(b.Source, "./") {
				continue
			}
			path := filepath.Join(d.St.Dir, b.Source)
			newest := newestIn(d, path)
			if newest.After(born) {
				d.add(Fail, fmt.Sprintf("%s is newer than the %s container — runsnip reload", strings.TrimPrefix(b.Source, "./"), p))
				stale = true
			}
		}
	}
	if !stale {
		d.add(Pass, strings.Join(d.St.Proxies(), " and ")+" run the configuration on disk")
	}
}

func newestIn(d *Doctor, path string) time.Time {
	st, err := d.Stat(path)
	if err != nil {
		return time.Time{}
	}
	if !st.IsDir() {
		return st.ModTime()
	}
	t := st.ModTime()
	entries, _ := d.ReadDir(path)
	for _, e := range entries {
		if n := newestIn(d, filepath.Join(path, e.Name())); n.After(t) {
			t = n
		}
	}
	return t
}

// Budget is the Postgres connection arithmetic: every pool's maximum plus the reserve must fit max_connections, or a
// burst gets FATAL 53300 "too many clients" instead of waiting in a pool.
func Budget(pools map[string]int, reserve, max int) (sum int, ok bool) {
	for _, n := range pools {
		sum += n
	}
	return sum, sum+reserve <= max
}

func pgBudget(d *Doctor, c def.Check) {
	// Which variables carry a service's pool size and its database URL is the stack's convention: stack.yaml names them.
	pg := def.Postgres{}
	if d.St.Def.Postgres != nil {
		pg = *d.St.Def.Postgres
	}
	poolEnv, urlEnv := pick(c.PoolEnv, pg.PoolEnv), pick(c.URLEnv, pg.URLEnv)
	c.Reserve = pick(c.Reserve, pg.Reserve)
	if poolEnv == "" || urlEnv == "" {
		d.add(Fail, "pg_budget needs pool_env and url_env in stack.yaml (the variables holding a service's pool size and its Postgres URL)")
		return
	}
	defPool := c.DefaultPool
	if defPool == 0 {
		defPool = 10
	}
	reserve, _ := strconv.Atoi(d.expand(pick(c.Reserve, "5")))
	maxc, err := d.St.SQL(d.ctx, "SHOW max_connections")
	max, convErr := strconv.Atoi(strings.TrimSpace(maxc))
	if err != nil || convErr != nil {
		d.add(Fail, "cannot ask postgres for max_connections")
		return
	}
	// Read from the RUNNING containers, not .env: what the server sees is what each container was created with.
	pools := map[string]int{}
	var list []string
	for _, n := range d.St.EnabledServices(d.vals) {
		ct := d.ps[n].Container
		if ct == "" || d.ps[n].State != "running" {
			continue
		}
		envs := d.inspect(ct, "{{range .Config.Env}}{{println .}}{{end}}")
		if !regexp.MustCompile(`(?m)^` + urlEnv + `=.`).MatchString(envs) {
			continue
		}
		m := regexp.MustCompile(`(?m)^` + poolEnv + `=(\d+)$`).FindStringSubmatch(envs)
		pool := defPool
		switch {
		case m == nil || m[1] == "0":
			d.add(Fail, fmt.Sprintf("%s connects to Postgres without %s — counted as the driver's default %d; declare it in compose", n, poolEnv, defPool))
		default:
			pool, _ = strconv.Atoi(m[1])
			if len(c.Reads) > 0 {
				// An image that does not read the variable keeps the driver's default pool.
				if _, ok := d.exec(n, nil, "sh", "-c", "grep -rqs "+poolEnv+" "+strings.Join(c.Reads, " ")+" 2>/dev/null"); !ok {
					d.add(Note, fmt.Sprintf("%s: the image does not read %s (declares %d) — its real pool is the default %d, counted as %d", n, poolEnv, pool, defPool, defPool))
					pool = defPool
				}
			}
		}
		pools[n] = pool
		list = append(list, fmt.Sprintf("%s=%d", n, pool))
	}
	sum, ok := Budget(pools, reserve, max)
	if ok {
		d.add(Pass, fmt.Sprintf("pools %d + reserve %d = %d ≤ max_connections %d", sum, reserve, sum+reserve, max))
	} else {
		d.add(Fail, fmt.Sprintf("pools %d + reserve %d = %d > max_connections %d — a burst will meet 53300; raise Postgres' max_connections or lower the services' %s", sum, reserve, sum+reserve, max, poolEnv))
	}
	sort.Strings(list)
	d.add(Info, strings.Join(list, " "))
	if open, err := d.St.SQL(d.ctx, "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'"); err == nil && open != "" {
		d.add(Info, "open now: "+open+" connections")
	}
}

// ───────────────────────── probes ─────────────────────────

func dnsCheck(d *Doctor, c def.Check) {
	if v := d.vals[pick(c.SkipEnv, "DOCTOR_CONNECT_TO")]; v != "" {
		d.add(Note, "skipped — "+pick(c.SkipEnv, "DOCTOR_CONNECT_TO")+"="+v+" (a replica: no public DNS)")
		return
	}
	myip := d.PublicIP(d.ctx)
	d.add(Info, "this machine's IP: "+pick(myip, "unknown"))
	proxied := map[string]bool{}
	for _, p := range c.Proxied {
		proxied[d.expand(p)] = true
	}
	seen := map[string]bool{}
	for _, h := range c.Hosts {
		if h.If != "" && d.vals[h.If] == "" {
			continue
		}
		host := d.expand(h.Host)
		if seen[host] {
			continue
		}
		seen[host] = true
		ips, err := d.LookupIP(d.ctx, host)
		switch {
		case err != nil || len(ips) == 0:
			d.add(Fail, host+" does not resolve")
		case myip == "" || contains(ips, myip):
			d.add(Pass, host+" → "+pick(match(ips, myip), ips[0]))
		case proxied[host]:
			d.add(Note, host+" → "+ips[0]+" (not this machine — normal behind the Cloudflare proxy)")
		default:
			// A proxied wildcard hides the hostname from Caddy: on-demand TLS cannot work.
			d.add(Fail, host+" → "+ips[0]+" (not this machine — turn the Cloudflare proxy off for this name)")
		}
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func match(xs []string, x string) string {
	if contains(xs, x) {
		return x
	}
	return ""
}

func execCheck(d *Doctor, c def.Check) {
	svcs := c.Services
	if c.Service != "" {
		svcs = append([]string{c.Service}, svcs...)
	}
	for _, svc := range svcs {
		cmd := make([]string, len(c.Cmd))
		for i, a := range c.Cmd {
			cmd[i] = strings.ReplaceAll(d.expand(a), "{service}", svc)
		}
		out, ok := d.exec(svc, nil, cmd...)
		extra := map[string]string{"service": svc, "out": lastLine(out), "output": out}
		if len(c.Cases) == 0 {
			d.verdict(ok, c, "{service}: ok", "{service}: failed — runsnip logs {service}", extra)
			continue
		}
		d.cases(c, out, extra)
	}
}

// cases picks the first case whose regexp matches out; {1}, {2}… are its groups.
func (d *Doctor) cases(c def.Check, out string, extra map[string]string) {
	for _, cs := range c.Cases {
		re, err := regexp.Compile(cs.Match)
		if err != nil {
			d.add(Fail, "bad match "+cs.Match+": "+err.Error())
			return
		}
		m := re.FindStringSubmatch(out)
		if m == nil {
			continue
		}
		for i, g := range m {
			extra[strconv.Itoa(i)] = strings.TrimSpace(g)
		}
		d.add(Level(cs.Level), d.text(cs.Say, extra))
		return
	}
	d.add(failLevel(c), d.text(pick(c.Fail, "unexpected answer: {out}"), extra))
}

func (d *Doctor) httpCode(from, url, host, client string) string {
	if client == "node" {
		out, _ := d.exec(from, nil, "node", "-e", "fetch('"+url+"').then(r=>console.log(r.status)).catch(()=>console.log(0))")
		return pick(lastLine(out), "0")
	}
	cmd := []string{"wget", "-S", "-q", "-O", "/dev/null", "--timeout=5"}
	if host != "" {
		cmd = append(cmd, "--header", "Host: "+host)
	}
	// busybox wget exits non-zero for every non-2xx answer but still prints the headers: the code is read from them.
	out := d.St.ExecCombined(d.ctx, from, append(cmd, url)...)
	code := "0"
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") {
			code = f[1]
		}
	}
	return code
}

func httpCheck(d *Doctor, c def.Check) {
	url, host := d.expand(c.URL), d.expand(c.Host)
	code := d.httpCode(c.Service, url, host, c.Client)
	n, _ := strconv.Atoi(code)
	label := pick(c.Label, c.Service+" → "+url)
	extra := map[string]string{"code": map[bool]string{true: "no answer", false: code}[code == "0"], "label": label}
	if msg, ok := c.Codes[n]; ok {
		lvl := Fail
		for _, e := range c.Expect {
			if e == n {
				lvl = Pass
			}
		}
		d.add(lvl, d.text(msg, extra))
		return
	}
	ok := true
	if len(c.Expect) > 0 {
		ok = false
		for _, e := range c.Expect {
			ok = ok || e == n
		}
	}
	for _, r := range append(c.Reject, 0) {
		if r == n {
			ok = false
		}
	}
	d.verdict(ok, c, "{label}  {code}", "{label}  {code}", extra)
}

func httpsHeaders(d *Doctor, c def.Check) {
	url := d.expand(c.URL)
	connect := ""
	if c.ConnectEnv != "" {
		connect = d.vals[c.ConnectEnv]
	}
	h, err := d.Head(d.ctx, url, connect)
	if err != nil {
		d.add(Fail, d.text(pick(c.None, url+" does not answer"), map[string]string{"error": err.Error()}))
		return
	}
	for _, a := range c.Absent {
		if h.Get(a) != "" {
			d.add(failLevel(c), d.text(pick(c.Fail, a+" is present"), map[string]string{"header": a}))
			return
		}
	}
	keys := make([]string, 0, len(c.Contains))
	for k := range c.Contains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.Contains(strings.ToLower(headerValues(h, k)), strings.ToLower(c.Contains[k])) {
			d.add(failLevel(c), d.text(pick(c.Note, k+" lacks "+c.Contains[k]), nil))
			return
		}
	}
	d.add(Pass, d.text(pick(c.Pass, url+" answers as expected"), nil))
}

func headerValues(h http.Header, k string) string { return strings.Join(h.Values(k), ", ") }

func sqlCheck(d *Doctor, c def.Check) {
	out, err := d.St.SQL(d.ctx, d.expand(c.Query))
	if err != nil {
		d.add(failLevel(c), d.text(pick(c.Fail, "query failed: {error}"), map[string]string{"error": lastLine(err.Error())}))
		return
	}
	for _, l := range strings.Split(out, "\n") {
		lvl, text, ok := strings.Cut(strings.TrimSpace(l), "|")
		if !ok {
			continue
		}
		switch Level(lvl) {
		case Pass, Fail, Note, Warn, Info:
			d.add(Level(lvl), text)
		}
	}
}

func logsCheck(d *Doctor, c def.Check) {
	args := []string{"logs", "--no-color"}
	if c.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(c.Tail))
	}
	out, _ := d.St.DCOutput(d.ctx, append(args, c.Service)...)
	n := strings.Count(out, c.Pattern)
	present := def.Truth(c.Present, true)
	extra := map[string]string{"count": strconv.Itoa(n), "service": c.Service}
	d.verdict((n > 0) == present, c, "{service} logs: ok", "{service} logs: {count}", extra)
}

func askCheck(d *Doctor, c def.Check) {
	for _, p := range c.Probes {
		if p.If != "" && d.vals[p.If] == "" {
			if p.Unset != "" {
				d.add(Info, d.text(p.Unset, nil))
			}
			continue
		}
		value := ""
		if p.Query != "" {
			v, _ := d.St.SQL(d.ctx, d.expand(p.Query))
			value = strings.TrimSpace(v)
			if value == "" {
				if p.None != "" {
					d.add(Info, d.text(p.None, nil))
				}
				continue
			}
		}
		host := strings.ReplaceAll(d.expand(p.Host), "{value}", value)
		url := strings.ReplaceAll(d.expand(c.URL), "{host}", host)
		out, _ := d.exec(c.Service, nil, "wget", "-qO-", "--timeout=5", url)
		said := lastLine(out) == "ok"
		ok := said == (p.Expect != "refuse")
		extra := map[string]string{"host": host, "value": value}
		if ok {
			d.add(Pass, d.text(p.Pass, extra))
		} else {
			d.add(Fail, d.text(p.Fail, extra))
		}
	}
}

func settingsOrEnv(d *Doctor, c def.Check) {
	saved, _ := d.St.SQL(d.ctx, d.expand(c.Query))
	have := map[string]bool{}
	for _, k := range strings.Split(strings.ReplaceAll(saved, "\n", ","), ",") {
		have[strings.TrimSpace(k)] = true
	}
	var missing []string
	for _, it := range c.Items {
		if !have[it.Setting] && d.vals[it.Env] == "" {
			missing = append(missing, it.Name)
		}
	}
	switch {
	case len(missing) == 0:
		mode := "?"
		v := d.vals[c.Key]
		prefixes := make([]string, 0, len(c.Modes))
		for p := range c.Modes {
			prefixes = append(prefixes, p)
		}
		sort.Strings(prefixes)
		for _, p := range prefixes {
			if strings.HasPrefix(v, p) {
				mode = c.Modes[p]
			}
		}
		if len(c.Items) > 0 && have[c.Items[0].Setting] {
			mode = pick(c.Want, mode)
		}
		d.add(Pass, d.text(c.Pass, map[string]string{"mode": mode}))
	case len(missing) == len(c.Items):
		d.add(Info, d.text(c.None, nil))
	default:
		d.add(Warn, d.text(c.Fail, map[string]string{"missing": strings.Join(missing, " ")}))
	}
}

func keyPairs(d *Doctor, c def.Check) {
	for _, p := range c.Keys {
		k, pub := p+"_JWT_KEY", p+"_PUBLIC_KEY"
		switch {
		case d.vals[k] == "" || d.vals[pub] == "":
			d.add(Fail, "missing "+k+" or "+pub+" — runsnip env generate")
		case keygen.PublicOf(d.vals[k]) != d.vals[pub]:
			d.add(Fail, pub+" is not the public half of "+k+" — generate both again together")
		default:
			d.add(Pass, "internal keys of "+strings.ToLower(strings.ReplaceAll(p, "_", "-"))+" match")
		}
	}
}

func hkdfCheck(d *Doctor, c def.Check) {
	if d.vals[c.Key] == "" {
		d.add(failLevel(c), d.text(pick(c.None, c.Key+" is empty — runsnip env generate"), nil))
		return
	}
	want, err := keygen.Derive(d.vals[c.From], c.Info, 32)
	if err != nil {
		d.add(Fail, err.Error())
		return
	}
	d.verdict(want == d.vals[c.Key], c, c.Key+" matches "+c.From, c.Key+" does NOT match "+c.From, nil)
}

func kidPinned(d *Doctor, c def.Check) {
	priv, pins := d.vals[c.Key], d.vals[c.Other]
	if priv == "" {
		d.add(Fail, c.Key+" is empty — runsnip env generate")
		return
	}
	if pins == "" {
		d.add(Fail, c.Other+" is empty — the services cannot check tokens")
		return
	}
	first, _, _ := strings.Cut(priv, ",")
	kid, key, _ := strings.Cut(first, ":")
	want := kid + ":" + keygen.PublicOf(key)
	ok := contains(strings.Split(strings.ReplaceAll(pins, " ", ""), ","), want)
	d.verdict(ok, c, c.Other+" has the public half of the current signing key", c.Other+" lacks "+want+" — the public half of the current signing key", nil)
}

// Number reads "0.8", "2", "1g" (1024 MB), "512m" (MB).
func Number(v string) (float64, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	mult := 1.0
	switch {
	case strings.HasSuffix(v, "g"):
		mult, v = 1024, strings.TrimSuffix(v, "g")
	case strings.HasSuffix(v, "m"):
		v = strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "k"):
		mult, v = 1.0/1024, strings.TrimSuffix(v, "k")
	}
	n, err := strconv.ParseFloat(v, 64)
	return n * mult, err == nil
}

func compare(d *Doctor, c def.Check) {
	host := d.Host()
	ram := float64(host.MemTotal) / (1 << 20)
	sub := func(s string) string {
		s = strings.ReplaceAll(s, "{cores}", strconv.Itoa(max(host.CPUs, 1)))
		return strings.ReplaceAll(s, "{ram_mb}", strconv.FormatFloat(ram, 'f', 0, 64))
	}
	if strings.Contains(c.Left+c.Right, "{ram_mb}") && ram == 0 {
		return // memory is unknown here (not Linux)
	}
	l, ok1 := Number(sub(d.expand(c.Left)))
	r, ok2 := Number(sub(d.expand(c.Right)))
	if !ok1 || !ok2 {
		d.add(Fail, "cannot compare "+d.expand(c.Left)+" with "+d.expand(c.Right))
		return
	}
	if c.Factor != 0 {
		r *= c.Factor
	}
	ok := map[string]bool{"<=": l <= r, "<": l < r, ">=": l >= r, ">": l > r, "==": l == r}[pick(c.Op, "<=")]
	extra := map[string]string{"left": d.expand(c.Left), "right": strconv.FormatFloat(r, 'f', -1, 64), "cores": strconv.Itoa(host.CPUs),
		"ram_mb": strconv.FormatFloat(ram, 'f', 0, 64), "suggest": strconv.FormatFloat(maxf(float64(host.CPUs)-0.5, 0.8), 'f', 1, 64)}
	d.verdict(ok, c, "", "", extra)
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func pathOwner(d *Doctor, c def.Check) {
	p := d.expand(c.Path)
	uid, err := d.OwnerOf(p)
	extra := map[string]string{"path": p, "owner": pick(uid, "?"), "uid": c.UID}
	if err != nil {
		uid = "?"
	}
	d.verdict(uid == c.UID, c, "{path} belongs to uid {uid}", "{path} belongs to uid {owner}, not {uid}", extra)
}

func ownerOf(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return strconv.Itoa(int(s.Uid)), nil
	}
	return "", fmt.Errorf("no owner")
}

func filePresent(d *Doctor, c def.Check) {
	p := d.expand(c.Path)
	st, err := d.Stat(p)
	list := ""
	if c.Glob != "" {
		g := d.expand(c.Glob)
		matches, _ := filepath.Glob(g)
		var names []string
		pre, suf, _ := strings.Cut(filepath.Base(g), "*")
		for _, m := range matches {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), pre), suf))
		}
		list = strings.Join(names, " ")
	}
	d.verdict(err == nil && st.Size() > 0, c, "{path}", "{path} missing", map[string]string{"path": p, "list": list})
}

// ───────────────────────── request shield ─────────────────────────

// shieldRoute is a route check, not a check of the shield: with the switch on, Caddy forwards every request to the
// shield's /check first, so the doctor asks that address from inside the proxy's own container — the hop Caddy
// takes. Any HTTP answer means the route is there; none means every site returns 502.
func shieldRoute(d *Doctor, c def.Check) {
	sh := d.St.Def.Shield
	if sh == nil {
		d.add(Info, "no request shield declared (stack.yaml: shield)")
		return
	}
	sw := sh.SwitchVar()
	if !sh.On(d.vals) {
		d.add(Info, "request shield: off ("+sw+" is not on) — Caddy forwards nothing; opt in with "+sw+"=on")
		return
	}
	addr, proxy := sh.Address(d.vals), sh.ProxyService()
	out := d.St.ExecCombined(d.ctx, proxy, "wget", "-S", "-q", "-O", "/dev/null", "-T", "3", "http://"+addr+sh.Path())
	if strings.Contains(out, "HTTP/") {
		d.add(Pass, sw+"=on: the shield endpoint "+proxy+" forwards to ("+addr+sh.Path()+") answers")
		return
	}
	d.add(Fail, sw+"=on: "+proxy+" forwards every request to "+addr+sh.Path()+" first, and nothing answers there — every site returns 502. Start the shield, or set "+sw+"=off and runsnip reload")
}

func dockerCheck(d *Doctor, c def.Check) {
	out, err := run.Output(d.ctx, d.St.Run, run.Call{Name: "docker", Args: []string{"info", "--format", "{{.ServerVersion}}"}})
	d.verdict(err == nil, c, "Docker Engine {version}", "Docker does not answer (is this user in the docker group? on a fresh server: runsnip setup)", map[string]string{"version": strings.TrimSpace(out)})
}

// sysctlCheck reads /proc/sys (Linux); elsewhere it says nothing.
func sysctlCheck(d *Doctor, c def.Check) {
	b, err := os.ReadFile("/proc/sys/" + strings.ReplaceAll(c.Key, ".", "/"))
	if err != nil {
		return
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	d.verdict(n >= c.Min, c, c.Key+" = {value}", c.Key+" = {value} < {min}", map[string]string{"value": strconv.Itoa(n), "min": strconv.Itoa(c.Min)})
}

func registryCheck(d *Doctor, c def.Check) {
	comp, ok := d.St.Component(d.St.DefaultComponent())
	if !ok {
		return
	}
	ref := d.St.ImageRef(comp, d.vals, d.St.Tag(comp, d.vals))
	host := strings.SplitN(ref, "/", 2)[0]
	if !DockerLoggedIn(host) {
		d.add(failLevel(c), "not logged in to "+host+": runsnip login (an account with read:packages)")
		return
	}
	d.verdict(d.St.ImageExists(d.ctx, ref), c, "logged in to "+host+" · "+ref+" readable", ref+" not readable: a token without read:packages or expired (runsnip login again), or that version is not released", nil)
}

// DockerLoggedIn reports whether docker's client config holds credentials for host (directly, or through a
// credential store or helper, which cannot be inspected without asking it).
func DockerLoggedIn(host string) bool {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = filepath.Join(home, ".docker")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Auths       map[string]json.RawMessage `json:"auths"`
		CredsStore  string                     `json:"credsStore"`
		CredHelpers map[string]string          `json:"credHelpers"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	if _, ok := cfg.Auths[host]; ok {
		return true
	}
	if _, ok := cfg.Auths["https://"+host]; ok {
		return true
	}
	_, helper := cfg.CredHelpers[host]
	return helper
}

var _ = sysinfo.HumanBytes
