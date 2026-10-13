package standard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/runsnip/ops/internal/compose"
	"github.com/runsnip/ops/internal/def"
)

// draft collects the stack file's lines and the report as adopt finds things.
type draft struct {
	top, infra string
	c          *compose.File
	yaml       []string // the stack file, section by section
	conforms   []string
	overrides  []string // "what — to drop it: change"
	gaps       []string // "what — fix"
	notes      []string
	write      bool // adopt may write runsnip.draft beside the compose file
}

func (d *draft) y(lines ...string) { d.yaml = append(d.yaml, lines...) }

func (d *draft) override(what, change string) {
	d.overrides = append(d.overrides, what+"\n      to drop it: "+change)
}

// findCompose picks, among dir, dir/infra and dir/infra/*, the compose file with the most pinned images.
func findCompose(dir string) (string, *compose.File, error) {
	cands := []string{dir, filepath.Join(dir, "infra")}
	more, _ := filepath.Glob(filepath.Join(dir, "infra", "*"))
	cands = append(cands, more...)
	best, bestN := "", -1
	var bestF *compose.File
	for _, c := range cands {
		p, err := compose.Find(c)
		if err != nil {
			continue
		}
		f, err := compose.Load(p)
		if err != nil {
			continue
		}
		n := 0
		for _, s := range f.Services {
			if _, err := compose.ParseImage(s.Image); err == nil {
				n++
			}
		}
		if n > bestN {
			best, bestN, bestF = p, n, f
		}
	}
	if best == "" {
		return "", nil, errNoCompose
	}
	return best, bestF, nil
}

func adopt(dir string, write bool) (Adoption, error) {
	dir, _ = filepath.Abs(dir)
	path, c, err := findCompose(dir)
	if err != nil {
		return Adoption{}, fmt.Errorf("%v under %s (looked in ., infra/, infra/*/)", err, dir)
	}
	d := &draft{infra: filepath.Dir(path), c: c, top: repoTop(filepath.Dir(path)), write: write}
	if d.top == "" {
		d.top = dir
	}
	rel, _ := filepath.Rel(d.top, d.infra)
	example := readExample(d.infra)

	d.y("# Stack definition drafted by `runsnip adopt` — runsnip stack standard v1 (github.com/runsnip/ops, STANDARD.md).",
		"# Every key below is an override of the standard or a description only this project has; everything else is a default.",
		"# Review it, rename it stack.yaml, then: runsnip validate", "standard: 1")
	name := c.Name
	if name == "" {
		name = filepath.Base(d.infra)
		d.gaps = append(d.gaps, "compose has no `name:` — fix: add `name: "+name+"` at the top of "+filepath.Base(path))
	} else {
		d.conforms = append(d.conforms, "compose project name: "+name)
	}
	d.y("name: " + name)
	d.compose(path)
	d.envFiles(example)
	d.profiles()
	comps := d.components()
	d.dataDir(name, example)
	d.health(comps)
	d.postgres()
	d.backups()
	d.envRules(example)
	d.shield()
	d.release(rel, comps)
	d.doctorChecks(example)
	d.shim()
	return Adoption{InfraDir: d.infra, Stack: strings.Join(d.yaml, "\n") + "\n", Report: d.report(path)}, nil
}

type exampleFile struct {
	name string
	text string
	keys []string
}

func readExample(dir string) exampleFile {
	for _, n := range []string{".env.example", "env.example", ".env.sample", "example.env"} {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			var keys []string
			for _, m := range regexp.MustCompile(`(?m)^#? ?([A-Z][A-Z0-9_]*)=`).FindAllStringSubmatch(string(b), -1) {
				keys = append(keys, m[1])
			}
			return exampleFile{name: n, text: string(b), keys: keys}
		}
	}
	return exampleFile{}
}

func (d *draft) compose(path string) {
	switch filepath.Base(path) {
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml":
		d.conforms = append(d.conforms, "compose file: "+filepath.Base(path)+" (docker compose's own name, found without -f)")
	}
}

func (d *draft) envFiles(ex exampleFile) {
	switch ex.name {
	case ".env.example":
		d.conforms = append(d.conforms, ".env.example")
	case "":
		d.gaps = append(d.gaps, "no env example — fix: write .env.example, every variable with its comment")
	default:
		d.y("env_example: " + ex.name)
		d.override("env_example: "+ex.name, "rename it to .env.example (git mv), and update the publish script")
	}
}

func (d *draft) profiles() {
	used := map[string]bool{}
	for _, s := range d.c.Services {
		for _, p := range s.Profiles {
			used[p] = true
		}
	}
	if len(used) == 0 {
		d.conforms = append(d.conforms, "no compose profiles (nothing to switch)")
		return
	}
	if len(d.c.StringMap("x-runsnip-profiles")) > 0 {
		d.conforms = append(d.conforms, "profiles switched in x-runsnip-profiles")
		return
	}
	for k := range d.c.Extensions {
		if strings.HasSuffix(k, "-profiles") && len(d.c.StringMap(k)) > 0 {
			d.y("profiles_block: " + k)
			d.override("profiles_block: "+k, "rename the block to x-runsnip-profiles")
			return
		}
	}
	var ps []string
	for p := range used {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	d.gaps = append(d.gaps, "profiles "+strings.Join(ps, ", ")+" have no switch — fix: x-runsnip-profiles: {<profile>: <VARIABLE that turns it on>} in compose")
}

type comp struct {
	svc, image, tagVar string
	others             []string
}

func (d *draft) components() []comp {
	byVar := map[string]*comp{}
	var order []string
	for _, n := range d.c.Order {
		img, err := compose.ParseImage(d.c.Services[n].Image)
		if err != nil {
			continue
		}
		if x, ok := byVar[img.TagVar]; ok {
			x.others = append(x.others, n)
			continue
		}
		byVar[img.TagVar] = &comp{svc: n, image: img.ShortName(), tagVar: img.TagVar}
		order = append(order, img.TagVar)
	}
	var out []comp
	var odd []string
	for _, v := range order {
		x := byVar[v]
		// The main service is the one named like the image (api for shop-api).
		all := append([]string{x.svc}, x.others...)
		for _, n := range all {
			if x.image == n || strings.HasSuffix(x.image, "-"+n) {
				var rest []string
				for _, o := range all {
					if o != n {
						rest = append(rest, o)
					}
				}
				x.svc, x.others = n, rest
				break
			}
		}
		out = append(out, *x)
		want := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(x.image)) + "_TAG"
		if x.tagVar != want {
			odd = append(odd, x.tagVar+" (standard: "+want+")")
		}
		if len(x.others) > 0 {
			d.notes = append(d.notes, fmt.Sprintf("%s shares its image with %s: they are pulled and recreated before it on pin/restart (migrations) — nothing to write", x.svc, strings.Join(x.others, ", ")))
		}
	}
	if len(out) == 0 {
		d.gaps = append(d.gaps, "no image is tagged ${<IMAGE>_TAG} — fix: <repo>:${<IMAGE>_TAG:-latest} for each released image")
		return nil
	}
	var names []string
	for _, x := range out {
		names = append(names, x.svc)
	}
	d.conforms = append(d.conforms, fmt.Sprintf("%d components found from compose: %s", len(out), strings.Join(names, " ")))
	if len(odd) > 0 {
		d.notes = append(d.notes, "tag variables named otherwise than the standard's (read as they are, no override needed): "+strings.Join(odd, ", "))
	}
	return out
}

var dataRe = regexp.MustCompile(`\$\{([A-Z0-9_]*DATA_DIR):-([^}]+)\}`)

func (d *draft) dataDir(name string, ex exampleFile) {
	b, _ := os.ReadFile(d.c.Path)
	want := "/srv/" + name
	m := dataRe.FindStringSubmatch(string(b))
	if m == nil {
		// The data folder may only be named in the env example (SHOP_DATA_DIR=/srv/shop).
		if mm := regexp.MustCompile(`(?m)^([A-Z0-9_]*DATA_DIR)=(\S+)`).FindStringSubmatch(ex.text); mm != nil {
			m = []string{"", mm[1], mm[2]}
		}
	}
	switch {
	case m == nil:
		d.notes = append(d.notes, "no data folder variable: data lives in Docker volumes; backups go to /srv/"+name+"/backups (DATA_DIR)")
	case m[1] == "DATA_DIR" && m[2] == want:
		d.conforms = append(d.conforms, "data folder ${DATA_DIR:-"+want+"}")
	default:
		v := "${" + m[1] + ":-" + m[2] + "}"
		d.y("data_dir: \"" + v + "\"")
		d.override("data_dir: "+v, "name the variable DATA_DIR (default "+want+") in compose and .env")
	}
}

var urlRe = regexp.MustCompile(`https?://(?:127\.0\.0\.1|localhost|0\.0\.0\.0):(\d+)(/[^\s'"\)\\,]*)`)

func (d *draft) health(comps []comp) {
	var lines, missing []string
	for _, x := range comps {
		s := d.c.Services[x.svc]
		if !s.Healthcheck {
			missing = append(missing, x.svc)
			continue
		}
		m := urlRe.FindStringSubmatch(s.HealthTest)
		if m == nil {
			continue // a command probe: the doctor reads compose's health status
		}
		port, p := m[1], m[2]
		if p == "/health" && (s.Env["PORT"] == port) {
			continue
		}
		h := "path: " + p
		if s.Env["PORT"] != port {
			h += ", port: \"" + port + "\""
		}
		lines = append(lines, "  "+x.svc+": { health: { "+h+" } }")
		d.override(x.svc+": health at GET "+p+" on "+port, "answer GET /health on $PORT (PORT: "+port+" in its environment)")
	}
	if len(lines) > 0 {
		d.y("services:")
		d.y(lines...)
	}
	if len(missing) > 0 {
		d.gaps = append(d.gaps, "components without a compose healthcheck: "+strings.Join(missing, ", ")+
			" — fix: healthcheck: {test: [CMD, wget, -q, -T, \"2\", -O, /dev/null, \"http://127.0.0.1:$PORT/health\"]}, or services.<name>.health.none: <why> for a worker with nothing to ask")
	} else if len(comps) > 0 {
		d.conforms = append(d.conforms, "every component declares a compose healthcheck")
	}
}

func (d *draft) postgres() {
	for _, n := range d.c.Order {
		s := d.c.Services[n]
		img := s.Image
		if !(strings.HasPrefix(img, "postgres:") || strings.Contains(img, "/postgres:") || strings.Contains(img, "pgvector") || strings.Contains(img, "postgis")) {
			continue
		}
		user, db := s.Env["POSTGRES_USER"], s.Env["POSTGRES_DB"]
		if user == "" {
			for _, a := range regexp.MustCompile(`-U\s+(\S+)`).FindAllStringSubmatch(s.HealthTest, -1) {
				user = a[1]
			}
		}
		literal := user != "" && !strings.Contains(user, "$")
		if n != "postgres" || literal {
			d.y("postgres:", "  service: "+n)
			if literal {
				d.y("  user: "+user, "  db: "+or(strings.Trim(db, "'\""), user))
			}
			d.override("postgres: "+n+" (user "+or(user, "from .env")+")", "name the service postgres and take POSTGRES_USER / POSTGRES_DB from .env")
		} else {
			d.conforms = append(d.conforms, "postgres service found (psql, backups, connection budget)")
		}
		return
	}
}

func (d *draft) backups() {
	var extra []string
	for _, n := range d.c.Order {
		img := d.c.Services[n].Image
		switch {
		case strings.Contains(img, "clickhouse"):
			extra = append(extra, n+" (ClickHouse: BACKUP DATABASE … TO File(…) as an exec target)")
		case strings.Contains(img, "mysql"), strings.Contains(img, "mariadb"):
			extra = append(extra, n+" (mysqldump as an exec target)")
		case strings.Contains(img, "mongo"):
			extra = append(extra, n+" (mongodump --archive as an exec target)")
		}
	}
	if len(extra) > 0 {
		d.notes = append(d.notes, "other databases to back up — add backup.targets (type: exec): "+strings.Join(extra, "; "))
	}
}

func (d *draft) envRules(ex exampleFile) {
	var change []string
	for _, l := range strings.Split(ex.text, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && !strings.HasPrefix(k, "#") && strings.Contains(v, "CHANGE_ME") {
			change = append(change, k)
		}
	}
	var pairs []string
	for _, k := range ex.keys {
		if strings.HasSuffix(k, "_JWT_KEY") {
			pairs = append(pairs, strings.TrimSuffix(k, "_JWT_KEY"))
		}
	}
	if len(change) == 0 && len(pairs) == 0 {
		return
	}
	d.y("env_rules:")
	if len(change) > 0 {
		d.y("  placeholder: CHANGE_ME")
	}
	d.y("  generate:")
	for _, k := range change {
		typ := "random_hex"
		// Values the example says to make with base64 (keys of a cipher, signing keys).
		if commentAbove(ex.text, k, "base64") || regexp.MustCompile(`base64[^\n]*\b`+k+`\b|base64[^\n]*\n#[^\n]*\b`+k+`\b`).MatchString(ex.text) {
			typ = "random_base64"
		}
		d.y("    - { type: " + typ + ", key: " + k + ", bytes: 32 }")
	}
	if len(pairs) > 0 {
		d.y("    - { type: ed25519_pair, keys: [" + strings.Join(pairs, ", ") + "] }")
	}
	d.conforms = append(d.conforms, fmt.Sprintf("%d generated value(s) declared (runsnip env generate fills them)", len(change)+len(pairs)))
}

func (d *draft) shield() {
	b, err := os.ReadFile(filepath.Join(d.infra, "Caddyfile"))
	if err != nil {
		return
	}
	cf := string(b)
	m := regexp.MustCompile(`import shield_\{\$(\w+):(on|off)\}`).FindStringSubmatch(cf)
	if m == nil {
		// A shield is optional: a Caddyfile without one is not a gap.
		return
	}
	// Where Caddy forwards: forward_auth {$VAR:default} or forward_auth host:port, with its uri and copy_headers.
	lines := []string{"shield:"}
	if m[1] != "SHIELD" {
		lines = append(lines, "  switch: "+m[1])
		d.override("shield.switch: "+m[1], "call the switch SHIELD")
	}
	if m[2] == "on" {
		lines = append(lines, "  switch_default: \"on\"")
		d.override("shield.switch_default: on (Caddy asks the shield unless .env says off)", "`import shield_{$"+m[1]+":off}` — the standard keeps the shield opt-in")
	} else {
		d.conforms = append(d.conforms, "request shield opt-in through "+m[1])
	}
	if fa := regexp.MustCompile(`forward_auth\s+(?:\{\$(\w+):([^}\s]+)\}|(\S+))\s*\{`).FindStringSubmatch(cf); fa != nil {
		if fa[1] != "" {
			lines = append(lines, "  addr_env: "+fa[1], "  addr: \""+fa[2]+"\"")
		} else {
			lines = append(lines, "  addr: \""+fa[3]+"\"")
		}
	} else {
		lines = append(lines, "  addr: \"TODO host:port\"")
		d.gaps = append(d.gaps, "the Caddyfile imports a shield but has no forward_auth address — fix: set shield.addr")
	}
	if u := regexp.MustCompile(`(?m)^\s*uri\s+(\S+)`).FindStringSubmatch(cf); u != nil && u[1] != "/check" {
		lines = append(lines, "  check_path: "+u[1])
	}
	if h := regexp.MustCompile(`(?m)^\s*copy_headers\s+(.+)$`).FindStringSubmatch(cf); h != nil {
		lines = append(lines, "  copy_headers: ["+strings.Join(strings.Fields(h[1]), ", ")+"]")
	}
	d.y(lines...)
}

func (d *draft) release(rel string, comps []comp) {
	var lines []string
	script := func(names ...string) string {
		for _, n := range names {
			if _, err := os.Stat(filepath.Join(d.top, "scripts", n)); err == nil {
				return "scripts/" + n
			}
		}
		return ""
	}
	build := script("build.sh", "release.sh")
	switch build {
	case "":
		d.gaps = append(d.gaps, "no build script (scripts/build.sh) — `runsnip build` has nothing to run")
	case "scripts/build.sh":
		d.conforms = append(d.conforms, "scripts/build.sh")
	default:
		lines = append(lines, "  build: [bash, "+build+"]")
		d.override("release.build: "+build, "rename it scripts/build.sh")
	}
	body := ""
	if build != "" {
		b, _ := os.ReadFile(filepath.Join(d.top, build))
		body = string(b)
		if !strings.Contains(body, "--list") {
			var names []string
			for _, x := range comps {
				names = append(names, x.svc)
			}
			lines = append(lines, "  components: ["+strings.Join(names, ", ")+"]")
			d.override("release.components listed by hand", "teach "+build+" `--list` (print the components it builds)")
		}
		if !strings.Contains(body, "▶ bundle") && strings.Contains(body, "▶") {
			lines = append(lines, "  progress: { building: '^▶ ([a-z0-9.-]+)\\s' }")
			d.override("release.progress.building", "print `▶ bundle <component> v<version>` when a component starts")
		}
	}
	if p := script("publish-infra.sh"); p == "" {
		d.gaps = append(d.gaps, "no scripts/publish-infra.sh — the infra branch and its binaries have no publisher")
	} else {
		d.conforms = append(d.conforms, "scripts/publish-infra.sh")
		pb, _ := os.ReadFile(filepath.Join(d.top, p))
		if m := regexp.MustCompile(`(?m)^BRANCH=["']?([\w./-]+)`).FindStringSubmatch(string(pb)); m != nil && m[1] != "infra" {
			d.y("infra:", "  branch: "+m[1])
			d.override("infra.branch: "+m[1], "call it infra")
		}
		if !strings.Contains(string(pb), "runsnip") {
			d.gaps = append(d.gaps, p+" does not ship runsnip — fix: build infra/ops for linux/amd64 and arm64 into bin/runsnip-linux-<arch> and copy the standard shim as runsnip")
		}
	}
	// Versions: the standard's places first, else the first file that has one.
	var vf []string
	for _, x := range comps {
		def1 := x.svc + "/package.json"
		if strings.HasPrefix(x.svc, "service-") {
			def1 = "services/" + strings.TrimPrefix(x.svc, "service-") + "/package.json"
		}
		if fileHas(d.top, def1) {
			continue
		}
		found := ""
		for _, c := range []string{"services/" + x.svc + "/package.json", "apps/" + x.svc + "/package.json", "packages/" + x.svc + "/package.json", rel + "/" + x.svc + "/VERSION", "infra/" + x.svc + "/VERSION", x.svc + "/VERSION"} {
			if fileHas(d.top, c) {
				found = c
				break
			}
		}
		if found == "" {
			d.gaps = append(d.gaps, x.svc+": no version file found — fix: \"version\" in "+def1)
			continue
		}
		vf = append(vf, x.svc+"\x00"+found)
	}
	vf = compressRules(vf)
	if len(vf) > 0 {
		lines = append(lines, "  version_files:")
		lines = append(lines, vf...)
		d.override(fmt.Sprintf("release.version_files for %d component(s)", len(vf)), "keep each version in <component>/package.json (services/<name>/package.json for service-<name>), or accept the override")
	}
	if len(lines) > 0 {
		d.y("release:")
		d.y(lines...)
	}
}

func fileHas(top, rel string) bool {
	_, err := os.Stat(filepath.Join(top, rel))
	return err == nil
}

func (d *draft) doctorChecks(ex exampleFile) {
	var checks []string
	for _, n := range d.c.Order {
		img := d.c.Services[n].Image
		if strings.Contains(img, "opensearch") || strings.Contains(img, "elasticsearch") {
			checks = append(checks, "        - { type: sysctl, key: vm.max_map_count, min: 262144, fail: \"vm.max_map_count = {value} < 262144: "+n+" will not start (sysctl -w vm.max_map_count=262144)\" }")
			break
		}
	}
	checks = append(checks, "        - { type: registry }")
	d.y("doctor:", "  sections:", "    - title: Host and registry", "      checks:")
	d.y(checks...)
}

func (d *draft) shim() {
	b, err := os.ReadFile(filepath.Join(d.infra, "runsnip"))
	if err == nil && string(b) == string(shimScript()) {
		d.conforms = append(d.conforms, "the standard runsnip shim")
		return
	}
	if d.write {
		_ = os.WriteFile(filepath.Join(d.infra, "runsnip.draft"), shimScript(), 0o755)
		d.gaps = append(d.gaps, "no standard runsnip shim — fix: runsnip.draft (written beside the compose file) → "+filepath.Join(filepath.Base(d.infra), "runsnip"))
		return
	}
	d.gaps = append(d.gaps, "no standard runsnip shim — fix: copy shim/runsnip of runsnip/ops to "+filepath.Join(filepath.Base(d.infra), "runsnip"))
}

func (d *draft) report(path string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "runsnip adopt — %s\n\n", path)
	sec := func(title string, xs []string) {
		fmt.Fprintf(&b, "%s (%d)\n", title, len(xs))
		for _, x := range xs {
			fmt.Fprintf(&b, "  - %s\n", x)
		}
		if len(xs) == 0 {
			fmt.Fprintln(&b, "  (none)")
		}
		fmt.Fprintln(&b)
	}
	sec("Already conforms", d.conforms)
	sec("Overridden in the draft — and the change that drops each override", d.overrides)
	sec("Gaps — required rules or missing pieces, each with its fix", d.gaps)
	sec("Notes", d.notes)
	return b.String()
}

// commentAbove reports whether the comment block right above key's line in an env example mentions word.
func commentAbove(text, key, word string) bool {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, key+"=") {
			continue
		}
		for j := i - 1; j >= 0 && strings.HasPrefix(strings.TrimSpace(lines[j]), "#"); j-- {
			if strings.Contains(lines[j], word) {
				return true
			}
		}
	}
	return false
}

// compressRules turns "component\x00file" pairs into version_files lines: the template most components share (their
// name replaced by {name}) becomes one `*` rule at the end, the others stay explicit before it.
func compressRules(pairs []string) []string {
	count := map[string]int{}
	tmpl := make([]string, len(pairs))
	for i, p := range pairs {
		c, f, _ := strings.Cut(p, "\x00")
		tmpl[i] = strings.Replace(f, "/"+c+"/", "/{name}/", 1)
		count[tmpl[i]]++
	}
	best, n := "", 1
	for t, k := range count {
		if k > n && strings.Contains(t, "{name}") {
			best, n = t, k
		}
	}
	var out []string
	for i, p := range pairs {
		c, f, _ := strings.Cut(p, "\x00")
		if tmpl[i] != best {
			out = append(out, "    - { match: "+c+", file: "+strconv.Quote(f)+" }")
		}
	}
	if best != "" {
		out = append(out, "    - { match: \"*\", file: "+strconv.Quote(best)+" }")
	}
	return out
}

var _ = def.FileName
