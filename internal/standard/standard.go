// Package standard is the runsnip stack standard in code: its rules (STANDARD.md is written from the same list), the
// JSON Schema of the stack definition, `runsnip validate` (a stack against both, each gap with its fix) and
// `runsnip adopt` (a draft stack.yaml for a project, with what conforms, what is overridden and how to drop each
// override).
package standard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/runsnip/ops/internal/compose"
	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/doctor"
	"github.com/runsnip/ops/internal/release"
	"github.com/runsnip/ops/internal/stack"
	"github.com/runsnip/ops/schema"
	"github.com/runsnip/ops/shim"
)

// Rule is one rule of the standard.
type Rule struct {
	ID       string
	Area     string
	Text     string
	Required bool // false: a default a stack may override in its stack.yaml
	Why      string
}

// Rules is the standard, v1. STANDARD.md lists the same rules.
var Rules = []Rule{
	{"layout", "Layout", "The infra folder holds stack.yaml, the compose file, .env.example, the proxies' config files and the runsnip shim; it is what the infra branch ships.", true, "The server checks out one folder and needs nothing else to run the stack."},
	{"stack-file", "Layout", "stack.yaml declares `standard: 1` and passes schema/stack.v1.json.", true, "The tool knows which rules the stack follows, and a typo is an error before it is a surprise on a server."},
	{"compose-file", "Compose", "The compose file is compose.yaml, compose.yml, docker-compose.yaml or docker-compose.yml (docker compose's own names), with docker-compose.override.yml beside it for a host's own differences.", false, "docker compose finds it without -f; the override is how a replica differs and nothing else."},
	{"compose-name", "Compose", "The compose file sets the project `name:`.", true, "Container, network and volume names stay the same whatever the checkout's folder is called; the data folder defaults to /srv/<name>."},
	{"service-names", "Compose", "Service names are lowercase words joined by hyphens; container_name, when set, equals the service name.", false, "`docker logs <service>` and `runsnip logs <service>` mean the same container."},
	{"logging", "Logging", "Every service logs with the json-file driver capped by max-size and max-file (default 10m × 3, one x-logging anchor).", true, "Docker's logs have no cap otherwise; the symptom when the disk fills is \"disk full\", not \"too many logs\"."},
	{"profiles", "Compose", "A service behind a profile has that profile in x-runsnip-profiles, naming the .env variable that turns it on.", true, "Every runsnip command turns the same profiles on, so `stop` and `ps` see what `start` started."},
	{"images", "Images", "A released component's image is `<repo>:${<IMAGE>_TAG:-latest}`, the variable named after the image (order-api → ORDER_API_TAG); one variable per image.", true, "The pin is one line of .env; the tool finds every component from compose alone."},
	{"tag-names", "Images", "The tag variable is the image's name upper-cased with _TAG (image-worker → IMAGE_WORKER_TAG).", false, "Anyone can tell the variable from the image without reading compose."},
	{"pins", "Versions", "Versions are pinned in .env; `pin` keeps the one before in <TAG_VAR>_PREVIOUS for `rollback`.", false, "All of the server's state that is not data is one file, backed up and combined like the rest of .env."},
	{"health-check", "Health", "Every component declares a compose healthcheck; a component with nothing to ask (a worker that only calls out) says why in stack.yaml (services.<name>.health.none).", true, "Docker restarts what is unhealthy, `ps` shows it, and the doctor and the dashboard count it."},
	{"health-endpoint", "Health", "The health endpoint is GET /health on $PORT, answering 200 when the service can serve.", false, "One probe for every service: no per-service knowledge in the tools."},
	{"env-files", "Environment", ".env.example names every variable with the comment that explains it; .env is the server's own (mode 600, never in git, never overwritten by a pull).", true, "`env combine` and the Env tab show each variable with its explanation; secrets never leave the server."},
	{"env-generated", "Environment", "Values a machine can make (random secrets, key pairs, values derived from another secret) are declared in env_rules.generate; `runsnip env generate` fills the missing ones and never overwrites.", false, "Nobody types a secret, and a derived value can never drift from its source."},
	{"key-pairs", "Environment", "An Ed25519 key pair is <NAME>_JWT_KEY (\"<d>.<x>\") and <NAME>_PUBLIC_KEY (\"<x>\").", false, "The doctor proves the halves belong together from .env alone."},
	{"data-dir", "Data", "Data lives under ${DATA_DIR:-/srv/<name>}; the checkout holds no data.", false, "A checkout can be deleted and cloned again; the data disk can be a mounted volume."},
	{"backups", "Backups", "`runsnip backup` writes gzipped dumps (pg_dump of the postgres service by default) into <data>/backups, mode 600, keeping the newest BACKUP_KEEP (10) of each.", false, "Backups exist without setup, and they do not fill the disk."},
	{"proxies", "Proxies", "A proxy's configuration is a read-only file mounted from the infra folder; `reload` recreates the proxies.", false, "A proxy reads its config once, at creation: recreating is the only reload that cannot lie."},
	{"release", "Release", "A component is released by scripts/build.sh <component> (with --list and --dry-run) onto deploy/<component>/v<version>, CI turns it into the image and tags <component>/v<version>; scripts/publish-infra.sh publishes the infra branch.", false, "The server never builds; every image traces back to a tag and a commit."},
	{"binaries", "Release", "The infra branch ships bin/runsnip-linux-amd64 and bin/runsnip-linux-arm64 beside the standard runsnip shim.", true, "The server needs neither Go nor a toolchain, only sh and uname."},
	{"doctor", "Doctor", "`runsnip doctor` runs the standard checks and the stack's own sections; a failed probe is reported and the doctor goes on; it never exits silently and exits 1 when anything failed.", true, "A partial report that looks complete is worse than none."},
	{"shield", "Proxies", "An optional request shield (a forward-auth endpoint its operator runs on its own), declared in stack.yaml, sits in front of the sites through Caddy: the Caddyfile defines (shield_on)/(shield_off) and every site imports `shield_{$SHIELD:off}`; SHIELD=on makes Caddy ask the shield's /check before every request.", false, "The shield changes the traffic path and fails closed, so it is a decision, not a default; runsnip wires Caddy to it and checks the route, nothing more."},
	{"serve", "Optional", "`runsnip serve` is off unless started: 127.0.0.1 only, read-only, a token for anything else.", false, "A dashboard must not become a way in."},
}

// RuleByID finds a rule.
func RuleByID(id string) Rule {
	for _, r := range Rules {
		if r.ID == id {
			return r
		}
	}
	return Rule{ID: id}
}

// Schema generates the JSON Schema of stack.yaml from the definition's types.
func Schema() ([]byte, error) {
	types := make([]string, 0, len(doctor.Types))
	for t := range doctor.Types {
		types = append(types, t)
	}
	sort.Strings(types)
	enums := map[string][]string{
		"doctor.sections[].checks[].type":            types,
		"doctor.sections[].checks[].level":           {"fail", "note", "warn", "info"},
		"doctor.sections[].checks[].cases[].level":   {"pass", "fail", "note", "warn", "info"},
		"doctor.sections[].checks[].client":          {"wget", "node"},
		"doctor.sections[].checks[].probes[].expect": {"ok", "refuse"},
		"env_rules.generate[].type":                  {"random_hex", "random_base64", "literal", "hkdf", "ed25519_pair", "ed25519_kid_pair"},
		"backup.targets[].type":                      {"pg_dump", "pg_dumpall", "exec"},
		"database.kind":                              {"postgres", "mysql", "mariadb"},
		"components.previous":                        {"env", "file"},
		"shield.switch_default":                      {"on", "off"},
		"standard":                                   nil,
	}
	delete(enums, "standard")
	docs := map[string]string{
		"standard":       "The version of the runsnip stack standard this stack follows (1).",
		"name":           "The stack's name in messages (default: compose's name).",
		"compose":        "The compose file, relative to the stack folder (default: docker compose's own names).",
		"env":            "The env file (default .env).",
		"env_example":    "The env example (default .env.example).",
		"data_dir":       "Where data lives (default ${DATA_DIR:-/srv/<compose name>}).",
		"profiles_block": "The compose block naming each profile's switch (default x-runsnip-profiles).",
		"profiles":       "Profiles and the .env variables that turn them on, instead of the compose block.",
		"services":       "Per-service overrides of the standard: health endpoint, services recreated before it.",
		"proxies":        "The services recreated by reload and start (default: the services running caddy, nginx, traefik, haproxy or envoy).",
		"postgres":       "The database for psql, backups and the connection budget (default: the service running a postgres image).",
		"database":       "The database `runsnip db pull|tunnel|restore` reach from the operator's machine, when its image does not say it: the service and the engine (postgres, mysql, mariadb).",
		"start":          "What `runsnip start` does: git pull, pull, the order services come up in, which are recreated, prune.",
		"backup":         "Backup targets (default: pg_dump of the postgres service), folder and retention.",
		"env_rules":      "Generated values, values to fill by hand, the placeholder that counts as empty, the secrets pattern.",
		"init":           "What `runsnip init` checks and creates: required variables, isolated domains, data folders, notes.",
		"downloads":      "Commands that fetch large files once onto the data disk.",
		"infra":          "The infra branch the server checks out, and what to say when a file changes.",
		"release":        "The build machine's side: scripts, components, version files, paths, tag and branch patterns.",
		"shield":         "How Caddy reaches the request shield: the .env switch, its default, the address variable and its default, the proxy.",
		"doctor":         "The stack's own doctor sections (typed checks), and standard sections to skip.",
		"serve":          "The optional web dashboard.",
	}
	for t, d := range doctor.Types {
		_ = t
		_ = d
	}
	return schema.Generate(reflect.TypeOf(def.Stack{}), enums, docs)
}

// Finding is one result of validate or adopt.
type Finding struct {
	Rule  string
	Level string // ok, override, gap, error, info
	Text  string
	Fix   string
}

// Result is a validation.
type Result struct {
	Dir      string
	Findings []Finding
}

// Errors counts what fails validation: errors, and gaps against required rules.
func (r Result) Errors() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == "error" || f.Level == "gap" && RuleByID(f.Rule).Required {
			n++
		}
	}
	return n
}

func (r *Result) add(rule, level, text, fix string) {
	r.Findings = append(r.Findings, Finding{Rule: rule, Level: level, Text: text, Fix: fix})
}

// InfraDir finds a project's stack folder from dir: dir itself, or the folder of its infra/stack.yaml.
func InfraDir(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, def.FileName)); err == nil {
		return dir, nil
	}
	p, err := release.Find(dir, "")
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// CheckSchema validates stack.yaml's content against the JSON Schema.
func CheckSchema(b []byte) []string {
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return []string{err.Error()}
	}
	doc = jsonable(doc)
	var sch any
	if err := json.Unmarshal(schema.V1, &sch); err != nil {
		return []string{"schema: " + err.Error()}
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(schema.ID, sch); err != nil {
		return []string{err.Error()}
	}
	s, err := c.Compile(schema.ID)
	if err != nil {
		return []string{err.Error()}
	}
	if err := s.Validate(doc); err != nil {
		var out []string
		if ve, ok := err.(*jsonschema.ValidationError); ok {
			for _, l := range strings.Split(ve.Error(), "\n") {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "jsonschema validation failed") {
					out = append(out, strings.TrimPrefix(l, "- "))
				}
			}
			return out
		}
		return []string{err.Error()}
	}
	return nil
}

// jsonable turns YAML's decoded values into JSON's (string keys, float64 numbers).
func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = jsonable(e)
		}
		return x
	case map[any]any:
		m := map[string]any{}
		for k, e := range x {
			m[fmt.Sprint(k)] = jsonable(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = jsonable(e)
		}
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return v
}

var nameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Validate checks the stack in dir against the schema and the standard.
func Validate(dir string) Result {
	r := Result{Dir: dir}
	infra, err := InfraDir(dir)
	if err != nil {
		r.add("layout", "error", err.Error(), "runsnip adopt writes a draft stack.yaml")
		return r
	}
	r.Dir = infra
	b, _ := os.ReadFile(filepath.Join(infra, def.FileName))
	if errs := CheckSchema(b); len(errs) > 0 {
		for _, e := range errs {
			r.add("stack-file", "error", "stack.yaml: "+e, "fix the key (schema/stack.v1.json)")
		}
	}
	d, err := def.Load(infra)
	if err != nil {
		r.add("stack-file", "error", err.Error(), "")
		return r
	}
	st, err := stack.New(infra, d)
	if err != nil {
		r.add("compose-file", "error", err.Error(), "add the compose file (compose.yaml or docker-compose.yml)")
		return r
	}
	r.add("stack-file", "ok", fmt.Sprintf("stack.yaml follows standard v%d and passes the schema", d.Standard), "")
	checkStack(&r, st)
	return r
}

func checkStack(r *Result, st *stack.Stack) {
	c := st.Compose
	d := st.Def
	// layout
	if _, err := os.Stat(st.ExamplePath); err != nil {
		r.add("env-files", "gap", filepath.Base(st.ExamplePath)+" is missing", "write one: every variable, with the comment that explains it")
	} else {
		r.add("env-files", "ok", filepath.Base(st.ExamplePath)+" is there", "")
	}
	switch b, err := os.ReadFile(filepath.Join(st.Dir, "runsnip")); {
	case err != nil:
		r.add("binaries", "gap", "no runsnip shim in "+st.Dir, "copy the standard shim (runsnip adopt writes it as runsnip.draft)")
	case !bytes.Equal(b, shim.Script):
		r.add("binaries", "info", "the runsnip shim differs from the standard one", "replace it with shim/runsnip of runsnip/ops unless the difference is meant")
	default:
		r.add("binaries", "ok", "the runsnip shim is the standard one", "")
	}
	// compose
	if c.Name == "" {
		r.add("compose-name", "gap", filepath.Base(st.ComposePath)+" has no `name:`", "add `name: "+filepath.Base(st.Dir)+"` at the top")
	} else {
		r.add("compose-name", "ok", "compose project: "+c.Name, "")
	}
	var badNames, badCN, noLog []string
	for _, n := range c.Order {
		s := c.Services[n]
		if !nameRe.MatchString(n) {
			badNames = append(badNames, n)
		}
		if s.ContainerName != "" && s.ContainerName != n {
			badCN = append(badCN, n+" ("+s.ContainerName+")")
		}
		if s.LoggingDriver != "json-file" && s.LoggingDriver != "local" || s.MaxSize == "" || s.MaxFile == "" {
			noLog = append(noLog, n)
		}
	}
	report := func(rule string, bad []string, ok, gap, fix string) {
		if len(bad) == 0 {
			r.add(rule, "ok", ok, "")
		} else {
			r.add(rule, "gap", gap+": "+strings.Join(bad, ", "), fix)
		}
	}
	report("service-names", badNames, "service names are lowercase-hyphenated", "service names that are not lowercase-hyphenated", "rename them")
	report("service-names", badCN, "container names equal service names", "container_name differs from the service name", "set container_name to the service name, or drop it")
	report("logging", noLog, "every service caps its logs", "services without a log cap", "add `logging: *logging` with x-logging: {driver: json-file, options: {max-size: 10m, max-file: \"3\"}}")
	// profiles
	prof := st.Profiles()
	var missing []string
	for _, n := range c.Order {
		for _, p := range c.Services[n].Profiles {
			if _, ok := prof[p]; !ok {
				missing = append(missing, n+" ("+p+")")
			}
		}
	}
	report("profiles", missing, "every profile has its switch in "+or(d.ProfilesBlock, stack.ProfilesBlock), "profiles without a switch", "add `"+or(d.ProfilesBlock, stack.ProfilesBlock)+":` to the compose file, `<profile>: <VARIABLE>` per profile")
	// images
	comps := st.Components()
	byVar := map[string]string{}
	var shared, oddVar []string
	for _, n := range c.Order {
		img, err := compose.ParseImage(c.Services[n].Image)
		if err != nil {
			continue
		}
		if prev, ok := byVar[img.TagVar]; ok && prev != img.Repo {
			shared = append(shared, img.TagVar)
		}
		byVar[img.TagVar] = img.Repo
	}
	for _, cp := range comps {
		want := strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(cp.ImageName())) + "_TAG"
		if cp.Image.TagVar != want {
			oddVar = append(oddVar, cp.Name+": "+cp.Image.TagVar+" (standard: "+want+")")
		}
	}
	if len(comps) == 0 {
		r.add("images", "gap", "no released component: no image is tagged ${<IMAGE>_TAG}", "write each of the project's images as <repo>:${<IMAGE>_TAG:-latest}")
	} else {
		report("images", shared, fmt.Sprintf("%d components, one tag variable each", len(comps)), "one tag variable for different images", "give each image its own variable")
	}
	if len(oddVar) > 0 {
		r.add("tag-names", "info", "tag variables named otherwise: "+strings.Join(oddVar, ", "), "rename them in compose and .env (the tool reads any name)")
	} else if len(comps) > 0 {
		r.add("tag-names", "ok", "tag variables follow the image names", "")
	}
	// health
	var noHC []string
	for _, cp := range comps {
		svc := c.Services[cp.Service]
		o := d.Services[cp.Service]
		if !svc.Healthcheck && (o.Health == nil || o.Health.None == "" && !o.Health.Image) {
			noHC = append(noHC, cp.Service)
		}
	}
	report("health-check", noHC, "every component declares a health check", "components without a compose healthcheck", "add healthcheck: {test: [CMD, wget, -q, -T, \"2\", -O, /dev/null, \"http://127.0.0.1:<port>/health\"]} — or services.<name>.health.none: <why> in stack.yaml")
	for n, o := range d.Services {
		if o.Health != nil && (o.Health.Path != "" || o.Health.Port != "" || len(o.Health.Cmd) > 0) {
			r.add("health-endpoint", "override", n+": health at "+describeHealth(o.Health), "answer GET /health on $PORT and drop the override")
		}
		if o.Health != nil && o.Health.None != "" {
			r.add("health-check", "override", n+": no health endpoint — "+o.Health.None, "")
		}
	}
	// key pairs
	if ex, err := os.ReadFile(st.ExamplePath); err == nil {
		keys := map[string]bool{}
		for _, m := range regexp.MustCompile(`(?m)^#? ?([A-Z0-9_]+)=`).FindAllStringSubmatch(string(ex), -1) {
			keys[m[1]] = true
		}
		var half []string
		for k := range keys {
			if strings.HasSuffix(k, "_JWT_KEY") && !keys[strings.TrimSuffix(k, "_JWT_KEY")+"_PUBLIC_KEY"] {
				half = append(half, k)
			}
		}
		sort.Strings(half)
		report("key-pairs", half, "key pairs are named <NAME>_JWT_KEY / <NAME>_PUBLIC_KEY", "private keys without their _PUBLIC_KEY", "add the public half to the example")
	}
	// data dir
	want := "/srv/" + st.ProjectName()
	var odd []string
	dre := regexp.MustCompile(`^\$\{([A-Z_]*DATA_DIR):-([^}]*)\}`)
	for _, n := range c.Order {
		for _, b := range c.Services[n].Binds {
			if m := dre.FindStringSubmatch(b.Source); m != nil && (m[1] != "DATA_DIR" || m[2] != want) {
				odd = append(odd, n+" ("+m[0]+")")
			}
		}
	}
	if d.DataDir != "" {
		r.add("data-dir", "override", "data_dir: "+d.DataDir, "use ${DATA_DIR:-"+want+"} in compose and drop data_dir")
	}
	report("data-dir", dedupe(odd), "data folders use ${DATA_DIR:-"+want+"}", "binds under another data folder", "use ${DATA_DIR:-"+want+"} (or set data_dir in stack.yaml)")
	// proxies
	var rw []string
	for _, p := range st.Proxies() {
		for _, b := range c.Services[p].Binds {
			if strings.HasPrefix(b.Source, "./") && !strings.HasSuffix(b.Target, ":ro") && !roMount(c, p, b) {
				rw = append(rw, p+" "+b.Source)
			}
		}
	}
	report("proxies", rw, "proxies read their config from the infra folder, read-only", "proxy configs mounted writable", "mount them with :ro")
	// shield
	if cf, err := os.ReadFile(filepath.Join(st.Dir, "Caddyfile")); err == nil && d.Shield != nil {
		sw := d.Shield.SwitchVar()
		has := strings.Contains(string(cf), "(shield_on)") && strings.Contains(string(cf), "(shield_off)")
		imp := regexp.MustCompile(`import shield_\{\$` + sw + `:(on|off)\}`).FindStringSubmatch(string(cf))
		switch {
		case !has || imp == nil:
			r.add("shield", "gap", "the Caddyfile has no opt-in request shield", "add the (shield_on)/(shield_off) snippets (runsnip shield snippet) and `import shield_{$"+sw+":off}` in each site")
		case imp[1] != or(d.Shield.SwitchDefault, "off"):
			r.add("shield", "gap", "the Caddyfile turns the shield on by default ("+imp[0]+") but stack.yaml says "+or(d.Shield.SwitchDefault, "off"), "make them agree (shield.switch_default)")
		default:
			r.add("shield", "ok", "request shield switched by "+sw+" in .env (unset: "+imp[1]+")", "")
		}
	}
	// release
	if top := repoTop(st.Dir); top != "" {
		rp := &release.Repo{Dir: top, StackDir: st.Dir, Def: d}
		for _, c := range [][]string{rp.BuildCmd(), rp.PublishInfraCmd()} {
			script := c[len(c)-1]
			if c[0] == "bash" || c[0] == "sh" {
				script = c[1]
			}
			if _, err := os.Stat(filepath.Join(top, script)); err != nil {
				r.add("release", "gap", script+" is missing", "add it, or name the project's own in release: of stack.yaml")
			}
		}
	}
	// overrides of defaults
	for _, o := range Overrides(d) {
		r.add(o.Rule, "override", o.Text, o.Fix)
	}
	// doctor sections reference known types (the schema checks the names too)
	for _, s := range d.Doctor.Sections {
		for _, ch := range s.Checks {
			if _, ok := doctor.Types[ch.Type]; !ok {
				r.add("doctor", "error", "doctor section "+s.Title+": unknown check type "+ch.Type, "one of: runsnip validate --types")
			}
		}
	}
}

func roMount(c *compose.File, svc string, b compose.Bind) bool {
	// Short syntax keeps the mode as a third field, which Bind does not carry: read it back from the raw image of
	// the volume in the compose file is not possible here, so accept targets whose mount is declared :ro in the
	// service's volumes as written.
	b2, _ := os.ReadFile(c.Path)
	return strings.Contains(string(b2), b.Source+":"+b.Target+":ro")
}

func describeHealth(h *def.Health) string {
	if len(h.Cmd) > 0 {
		return "`" + strings.Join(h.Cmd, " ") + "`"
	}
	return "GET " + or(h.Path, "/health") + " on " + or(h.Port, "$PORT")
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
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

// repoTop walks up from dir to the folder holding .git.
func repoTop(dir string) string {
	dir, _ = filepath.Abs(dir)
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// Overrides lists the keys of d that replace a default of the standard, each with how to drop it.
func Overrides(d *def.Stack) []Finding {
	var out []Finding
	add := func(rule, text, fix string) {
		out = append(out, Finding{Rule: rule, Level: "override", Text: text, Fix: fix})
	}
	if d.Compose != "" {
		add("compose-file", "compose: "+d.Compose, "rename it to compose.yaml or docker-compose.yml")
	}
	if d.Env != "" {
		add("env-files", "env: "+d.Env, "use .env")
	}
	if d.EnvExample != "" {
		add("env-files", "env_example: "+d.EnvExample, "rename it to .env.example")
	}
	if d.ProfilesBlock != "" || len(d.Profiles) > 0 {
		add("profiles", "profiles declared in stack.yaml or another compose block", "move them to x-runsnip-profiles in compose")
	}
	if d.Components.Previous == "file" {
		add("pins", "components.previous: file", "keep previous versions in .env (drop the key; .last-tag-* files are still read)")
	}
	if d.Components.Default != "" {
		add("images", "components.default: "+d.Components.Default, "list that component first in compose")
	}
	if d.Start.Recreate != nil {
		add("proxies", "start.recreate: ["+strings.Join(d.Start.Recreate, ", ")+"]", "let start recreate the proxies (their config is read only when they are created)")
	}
	if len(d.Components.Only) > 0 || len(d.Components.Exclude) > 0 {
		add("images", "components.only/exclude", "tag only released images with ${<IMAGE>_TAG}")
	}
	if d.Proxies != nil {
		add("proxies", "proxies: "+strings.Join(d.Proxies, " "), "run the proxies from their official images (caddy, nginx, traefik, haproxy, envoy)")
	}
	if d.Postgres != nil && (d.Postgres.Service != "" || d.Postgres.User != "" || d.Postgres.DB != "") {
		add("backups", "postgres: "+d.Postgres.Service, "run Postgres from a postgres image with POSTGRES_USER and POSTGRES_DB in .env")
	}
	if len(d.Backup.Targets) > 0 || d.Backup.Dir != "" || d.Backup.Keep != "" {
		add("backups", "backup: own targets, folder or retention", "pg_dump of the postgres service into <data>/backups is the default")
	}
	if d.Infra.Branch != "" && d.Infra.Branch != "infra" {
		add("release", "infra.branch: "+d.Infra.Branch, "call the branch infra")
	}
	rel := d.Release
	for _, kv := range []struct{ k, v string }{
		{"build", strings.Join(rel.Build, " ")}, {"list", strings.Join(rel.List, " ")}, {"publish_infra", strings.Join(rel.PublishInfra, " ")},
		{"deploy_branch", rel.DeployBranch}, {"tag", rel.Tag}, {"infra_commit", rel.InfraCommit},
	} {
		if kv.v != "" {
			add("release", "release."+kv.k+": "+kv.v, "follow the standard's (scripts/build.sh, deploy/{component}/v{version}, {component}/v{version}, \"infra from {sha}\")")
		}
	}
	for _, v := range rel.VersionFiles {
		add("release", "release.version_files: "+v.Match+" → "+v.File, "keep the version in <component>/package.json (services/<name>/package.json for service-<name>)")
	}
	if len(rel.Components) > 0 {
		add("release", "release.components listed by hand", "let the build script print them (--list)")
	}
	if d.Shield != nil && d.Shield.SwitchDefault == "on" {
		add("shield", "shield.switch_default: on (the shield is on unless .env turns it off)", "keep the shield opt-in (off by default)")
	}
	return out
}

// PrintValidation writes a result.
func PrintValidation(w io.Writer, r Result, color bool) {
	c := func(code, s string) string {
		if !color {
			return s
		}
		return "\033[" + code + "m" + s + "\033[0m"
	}
	fmt.Fprintf(w, "runsnip stack standard v%d — %s\n\n", def.Standard, r.Dir)
	marks := map[string]string{"ok": c("32", "✓"), "override": c("36", "≈"), "gap": c("33", "✗"), "error": c("31", "✗"), "info": c("2", "·")}
	counts := map[string]int{}
	for _, f := range r.Findings {
		counts[f.Level]++
		lvl := f.Level
		if lvl == "gap" && !RuleByID(f.Rule).Required {
			lvl = "gap (default)"
		}
		fmt.Fprintf(w, "  %s %-15s %s", marks[f.Level], f.Rule, f.Text)
		if f.Level != "ok" && f.Level != "info" {
			fmt.Fprintf(w, "  [%s]", lvl)
		}
		fmt.Fprintln(w)
		if f.Fix != "" && f.Level != "ok" {
			fmt.Fprintf(w, "      → %s\n", f.Fix)
		}
	}
	fmt.Fprintf(w, "\n%d ok · %d override(s) · %d gap(s) · %d error(s)\n", counts["ok"], counts["override"], counts["gap"], counts["error"])
	if r.Errors() == 0 {
		fmt.Fprintln(w, c("32", "Conforms (overrides are allowed; each says how to drop it)."))
	} else {
		fmt.Fprintln(w, c("31", fmt.Sprintf("%d required rule(s) not met.", r.Errors())))
	}
}
