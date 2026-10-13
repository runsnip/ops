// Package def is the stack definition: the stack.yaml a project keeps in its infra folder (runsnip stack standard
// v1, schema/stack.v1.json). Everything has a default from the standard or from the compose file, so a conforming
// project's file is a few lines; every rule a project does not follow has a key to override it.
package def

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Standard is the version of the stack standard this tool implements.
const Standard = 1

// FileName is the stack definition's name in a project's infra folder (and on its infra branch).
const FileName = "stack.yaml"

// LocalName is an optional overlay beside it (a replica, a staging host): its keys replace the stack file's.
const LocalName = "stack.local.yaml"

// Stack is stack.yaml.
type Stack struct {
	Standard   int    `yaml:"standard"`
	Name       string `yaml:"name,omitempty"`
	Title      string `yaml:"title,omitempty"`
	Compose    string `yaml:"compose,omitempty"`
	Env        string `yaml:"env,omitempty"`
	EnvExample string `yaml:"env_example,omitempty"`
	DataDir    string `yaml:"data_dir,omitempty"`
	// ProfilesBlock is the compose x-… block naming, per profile, the .env variable that turns it on.
	ProfilesBlock string            `yaml:"profiles_block,omitempty"`
	Profiles      map[string]string `yaml:"profiles,omitempty"`

	Components Components         `yaml:"components,omitempty"`
	Services   map[string]Service `yaml:"services,omitempty"`
	Proxies    []string           `yaml:"proxies,omitempty"`
	Graph      *Graph             `yaml:"graph,omitempty"`
	Postgres   *Postgres          `yaml:"postgres,omitempty"`
	Database   *Database          `yaml:"database,omitempty"`
	Registry   Registry           `yaml:"registry,omitempty"`
	Start      Start              `yaml:"start,omitempty"`
	Backup     Backup             `yaml:"backup,omitempty"`
	EnvRules   EnvRules           `yaml:"env_rules,omitempty"`
	Init       Init               `yaml:"init,omitempty"`
	Downloads  []Download         `yaml:"downloads,omitempty"`
	Infra      Infra              `yaml:"infra,omitempty"`
	Release    Release            `yaml:"release,omitempty"`
	Shield     *Shield            `yaml:"shield,omitempty"`
	Doctor     Doctor             `yaml:"doctor,omitempty"`
	Serve      Serve              `yaml:"serve,omitempty"`
}

// Components says which compose services are released components and how their versions are kept.
type Components struct {
	// Default is the component `pin <tag>` and `rollback` act on without a name (default: the first in compose).
	Default string   `yaml:"default,omitempty"`
	Only    []string `yaml:"only,omitempty"`
	Exclude []string `yaml:"exclude,omitempty"`
	// Previous is where the version before the last pin is kept: env (<TAG_VAR>_PREVIOUS in .env, the standard) or
	// file (.last-tag-<component> beside .env).
	Previous string `yaml:"previous,omitempty"`
}

// Service overrides what the standard expects of one service.
type Service struct {
	Health *Health `yaml:"health,omitempty"`
	// Graph places the service in `runsnip graph` when the topology alone does not say it well.
	Graph *GraphNode `yaml:"graph,omitempty"`
	// Before lists services recreated before this one when it is pinned or restarted (migrations).
	Before []string `yaml:"before,omitempty"`
}

// GraphNode overrides how a service is drawn: its kind (app, proxy, data, worker), its layer (ingress, edge, app,
// data), a group it is boxed with, and its @runsnip/icons name.
type GraphNode struct {
	Kind  string `yaml:"kind,omitempty"`
	Layer string `yaml:"layer,omitempty"`
	Group string `yaml:"group,omitempty"`
	Icon  string `yaml:"icon,omitempty"`
	Hide  bool   `yaml:"hide,omitempty"`
}

// Graph is the stack-wide part of `runsnip graph`: the accent colour of the project, which images are datastores
// beyond the known ones, and external hosts to leave out.
type Graph struct {
	Accent     string   `yaml:"accent,omitempty"`
	Datastores []string `yaml:"datastores,omitempty"` // image names (the part after the last /, before :)
	Ignore     []string `yaml:"ignore,omitempty"`     // external hosts or registrable domains not drawn
}

// Health is how to ask a service whether it is ready, from inside its container.
type Health struct {
	Path string   `yaml:"path,omitempty"`
	Port string   `yaml:"port,omitempty"`
	Cmd  []string `yaml:"cmd,omitempty"`
	// None says why the service has no health endpoint at all (a worker that only calls out).
	None string `yaml:"none,omitempty"`
	// Image says the service's image declares its own HEALTHCHECK (compose does not show it).
	Image bool `yaml:"image,omitempty"`
}

// Postgres is the stack's database, for psql, backups and the connection budget.
type Postgres struct {
	Service string `yaml:"service,omitempty"`
	User    string `yaml:"user,omitempty"`
	DB      string `yaml:"db,omitempty"`
	// The connection budget (doctor): the variables carrying each service's pool size and its Postgres URL, and the
	// connections kept for psql, backups and the doctor. Without pool_env and url_env there is no budget check.
	PoolEnv string `yaml:"pool_env,omitempty"`
	URLEnv  string `yaml:"url_env,omitempty"`
	Reserve string `yaml:"reserve,omitempty"`
}

// Database names the stack's database for `runsnip db` (pull, tunnel, restore) when its image does not say it: the
// service, and the engine (postgres, mysql or mariadb). Without it, the service running a postgres, mysql or mariadb
// image is the database (postgres.service when set).
type Database struct {
	Service string `yaml:"service,omitempty"`
	Kind    string `yaml:"kind,omitempty"`
}

// Registry is where the images come from.
type Registry struct {
	Host    string `yaml:"host,omitempty"`
	UserEnv string `yaml:"user_env,omitempty"`
}

// Start is `start`: what a full start does, in order.
type Start struct {
	GitPull     *bool      `yaml:"git_pull,omitempty"`
	Pull        *bool      `yaml:"pull,omitempty"`
	CheckImages bool       `yaml:"check_images,omitempty"`
	Order       [][]string `yaml:"order,omitempty"`
	Recreate    []string   `yaml:"recreate,omitempty"`
	Prune       *bool      `yaml:"prune,omitempty"`
}

// Backup is `backup`.
type Backup struct {
	Dir     string         `yaml:"dir,omitempty"`
	Keep    string         `yaml:"keep,omitempty"`
	Targets []BackupTarget `yaml:"targets,omitempty"`
	// Legacy are older backup folders whose files keep falling under the same retention (relative to the stack).
	Legacy []string `yaml:"legacy,omitempty"`
}

// BackupTarget is one thing backed up.
type BackupTarget struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"` // pg_dump, pg_dumpall, exec
	Service string   `yaml:"service,omitempty"`
	User    string   `yaml:"user,omitempty"`
	DB      string   `yaml:"db,omitempty"`
	Cmd     []string `yaml:"cmd,omitempty"`
	// Ext is the file's extension for exec targets whose output is saved (default .gz, gzip applied); empty output
	// is not saved (a backup the program writes elsewhere).
	Ext  string `yaml:"ext,omitempty"`
	Save *bool  `yaml:"save,omitempty"`
}

// EnvRules are the .env conventions of the stack.
type EnvRules struct {
	Generate    []Generator `yaml:"generate,omitempty"`
	Manual      []Manual    `yaml:"manual,omitempty"`
	Placeholder string      `yaml:"placeholder,omitempty"`
	Secrets     string      `yaml:"secrets,omitempty"` // regexp naming secret variables
	// Vars validates variables: what a value must look like, checked by env set, the wizard and the doctor.
	Vars map[string]VarRule `yaml:"vars,omitempty"`
}

// VarRule is what one variable's value must be. Every field set must hold; an empty value is checked only when
// Required.
type VarRule struct {
	Type     string   `yaml:"type,omitempty"` // url, email, domain, port, int, bool, hex, base64, duration, cidrs
	Pattern  string   `yaml:"pattern,omitempty"`
	Min      int      `yaml:"min,omitempty"`   // the value's length at least (characters)
	Bytes    int      `yaml:"bytes,omitempty"` // hex/base64: decoded length at least
	Enum     []string `yaml:"enum,omitempty"`
	Required bool     `yaml:"required,omitempty"`
	Help     string   `yaml:"help,omitempty"` // shown when the example has no comment for it
}

// Generator fills one or more variables in `env generate` (never overwriting a value).
type Generator struct {
	Type string `yaml:"type"` // random_hex, random_base64, hkdf, literal, ed25519_pair, ed25519_kid_pair
	Key  string `yaml:"key,omitempty"`
	// Keys is the list of prefixes for ed25519_pair (<P>_JWT_KEY / <P>_PUBLIC_KEY).
	Keys          []string `yaml:"keys,omitempty"`
	Bytes         int      `yaml:"bytes,omitempty"`
	From          string   `yaml:"from,omitempty"` // hkdf: the variable holding the input key
	Info          string   `yaml:"info,omitempty"` // hkdf: the info string
	Value         string   `yaml:"value,omitempty"`
	Private       string   `yaml:"private,omitempty"`
	Public        string   `yaml:"public,omitempty"`
	PrivateSuffix string   `yaml:"private_suffix,omitempty"`
	PublicSuffix  string   `yaml:"public_suffix,omitempty"`
}

// Manual names variables that cannot be generated, with where to get them.
type Manual struct {
	Keys  []string `yaml:"keys"`
	Title string   `yaml:"title"`
	Hint  string   `yaml:"hint,omitempty"`
}

// Init is `init`: what a first install checks and creates.
type Init struct {
	Require []string `yaml:"require,omitempty"`
	// Isolated are domains that must not share a registrable domain with Base (they run other people's code).
	Isolated *Isolated `yaml:"isolated,omitempty"`
	Dirs     []Dir     `yaml:"dirs,omitempty"`
	Notes    []Note    `yaml:"notes,omitempty"`
}

// Isolated is a base domain and the domains that must stay apart from it.
type Isolated struct {
	Base   string   `yaml:"base"`
	Others []string `yaml:"others"`
}

// Dir is a data directory `init` creates, and its owner.
type Dir struct {
	Path  string `yaml:"path"`
	Owner string `yaml:"owner,omitempty"`
	Why   string `yaml:"why,omitempty"`
}

// Note is a line `init` prints, when If holds.
type Note struct {
	Text string `yaml:"text"`
	If   *Cond  `yaml:"if,omitempty"`
}

// Download is a command fetching files once onto the data disk (models too large for an image).
type Download struct {
	Command string   `yaml:"command"`
	Title   string   `yaml:"title,omitempty"`
	Dir     string   `yaml:"dir"`
	URL     string   `yaml:"url"`  // with {item}
	File    string   `yaml:"file"` // with {item}
	Items   []string `yaml:"items"`
	Default string   `yaml:"default,omitempty"`
	Owner   string   `yaml:"owner,omitempty"`
	OwnDir  string   `yaml:"own_dir,omitempty"` // chown -R this folder after a download
}

// Infra is the infra branch the server checks out.
type Infra struct {
	Branch string `yaml:"branch,omitempty"`
	Remote string `yaml:"remote,omitempty"`
	Hints  []Hint `yaml:"hints,omitempty"`
}

// Hint says what to run when a changed file matches.
type Hint struct {
	Match string `yaml:"match"` // a path, or a prefix ending in /
	Say   string `yaml:"say"`
}

// Release is the build machine's side (`runsnip build`).
type Release struct {
	Build        []string      `yaml:"build,omitempty"`
	List         []string      `yaml:"list,omitempty"`
	Components   []string      `yaml:"components,omitempty"`
	PublishInfra []string      `yaml:"publish_infra,omitempty"`
	DryRunFlag   string        `yaml:"dry_run_flag,omitempty"`
	Verify       []string      `yaml:"verify,omitempty"`
	Remote       string        `yaml:"remote,omitempty"`
	Main         string        `yaml:"main,omitempty"`
	DeployBranch string        `yaml:"deploy_branch,omitempty"`
	Tag          string        `yaml:"tag,omitempty"`
	InfraCommit  string        `yaml:"infra_commit,omitempty"`
	InfraPaths   []string      `yaml:"infra_paths,omitempty"`
	VersionFiles []PatternRule `yaml:"version_files,omitempty"`
	Paths        []PatternRule `yaml:"paths,omitempty"`
	Progress     Progress      `yaml:"progress,omitempty"`
}

// PatternRule maps a component name pattern (`*` matches anything; `{*}` repeats it) to files.
type PatternRule struct {
	Match string   `yaml:"match"`
	File  string   `yaml:"file,omitempty"`
	Paths []string `yaml:"paths,omitempty"`
}

// Progress reads the build script's output: which component it is on, which one it pushed.
type Progress struct {
	Building string `yaml:"building,omitempty"`
	Pushed   string `yaml:"pushed,omitempty"`
}

// Shield is an optional request shield in front of the sites: a forward-auth endpoint Caddy asks before every request.
// A stack without this block has none; runsnip only wires Caddy to it and checks the route, never the shield itself.
type Shield struct {
	Switch string `yaml:"switch,omitempty"` // the .env variable turning Caddy's ask on (default SHIELD)
	// SwitchDefault is the switch's value when .env leaves it unset: off (the standard) or on.
	SwitchDefault string   `yaml:"switch_default,omitempty"`
	AddrEnv       string   `yaml:"addr_env,omitempty"`     // the .env variable with the address Caddy forwards to
	Addr          string   `yaml:"addr"`                   // its default (host:port)
	CheckPath     string   `yaml:"check_path,omitempty"`   // the path Caddy asks (default /check)
	CopyHeaders   []string `yaml:"copy_headers,omitempty"` // headers of the shield's answer passed on to the sites
	Proxy         string   `yaml:"proxy,omitempty"`        // the service that forwards (default: caddy)
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// SwitchVar is the .env variable turning it on.
func (s *Shield) SwitchVar() string { return or(s.Switch, "SHIELD") }

// On reports whether .env turns the shield on.
func (s *Shield) On(vals map[string]string) bool {
	return strings.EqualFold(or(vals[s.SwitchVar()], or(s.SwitchDefault, "off")), "on")
}

// Address is where Caddy forwards to, as .env sets it.
func (s *Shield) Address(vals map[string]string) string {
	if s.AddrEnv != "" && vals[s.AddrEnv] != "" {
		return vals[s.AddrEnv]
	}
	return s.Addr
}

// Path is the path Caddy asks.
func (s *Shield) Path() string { return or(s.CheckPath, "/check") }

// ProxyService is the service that forwards.
func (s *Shield) ProxyService() string { return or(s.Proxy, "caddy") }

// Doctor is the project's own doctor sections, run after the standard ones.
type Doctor struct {
	Sections []Section `yaml:"sections,omitempty"`
	// Skip turns standard sections off by title.
	Skip []string `yaml:"skip,omitempty"`
}

// Section is a titled group of checks.
type Section struct {
	Title  string  `yaml:"title"`
	If     *Cond   `yaml:"if,omitempty"`
	Off    string  `yaml:"off,omitempty"` // printed (dim) when If does not hold
	Checks []Check `yaml:"checks"`
}

// Cond is a condition on the stack: every field set must hold.
type Cond struct {
	Env     string   `yaml:"env,omitempty"`     // non-empty
	Unset   string   `yaml:"unset,omitempty"`   // empty
	Differs []string `yaml:"differs,omitempty"` // two expressions with different values
	Profile string   `yaml:"profile,omitempty"` // turned on
	Running string   `yaml:"running,omitempty"` // has a container
	Stopped string   `yaml:"stopped,omitempty"` // has no container
}

// Check is one doctor check. Type picks the check; the other fields are its parameters (internal/doctor lists which
// each type reads). Messages are templates: ${VAR} from .env, {name} from the check's own values.
type Check struct {
	Type  string `yaml:"type"`
	If    *Cond  `yaml:"if,omitempty"`
	Level string `yaml:"level,omitempty"` // what a failure is: fail (default), note, warn
	Label string `yaml:"label,omitempty"`

	Key         string            `yaml:"key,omitempty"`
	Keys        []string          `yaml:"keys,omitempty"`
	Any         []string          `yaml:"any,omitempty"`
	Other       string            `yaml:"other,omitempty"`
	Min         int               `yaml:"min,omitempty"`
	Suffix      string            `yaml:"suffix,omitempty"`
	From        string            `yaml:"from,omitempty"`
	Info        string            `yaml:"info,omitempty"`
	Service     string            `yaml:"service,omitempty"`
	Services    []string          `yaml:"services,omitempty"`
	Cmd         []string          `yaml:"cmd,omitempty"`
	URL         string            `yaml:"url,omitempty"`
	Host        string            `yaml:"host,omitempty"`
	Client      string            `yaml:"client,omitempty"`
	Expect      []int             `yaml:"expect,omitempty"`
	Reject      []int             `yaml:"reject,omitempty"`
	Codes       map[int]string    `yaml:"codes,omitempty"`
	Query       string            `yaml:"query,omitempty"`
	Pattern     string            `yaml:"pattern,omitempty"`
	Tail        int               `yaml:"tail,omitempty"`
	Present     *bool             `yaml:"present,omitempty"`
	Path        string            `yaml:"path,omitempty"`
	UID         string            `yaml:"uid,omitempty"`
	Glob        string            `yaml:"glob,omitempty"`
	Left        string            `yaml:"left,omitempty"`
	Op          string            `yaml:"op,omitempty"`
	Right       string            `yaml:"right,omitempty"`
	Factor      float64           `yaml:"factor,omitempty"`
	Base        string            `yaml:"base,omitempty"`
	Others      []string          `yaml:"others,omitempty"`
	Hosts       []HostIf          `yaml:"hosts,omitempty"`
	Proxied     []string          `yaml:"proxied,omitempty"`
	SkipEnv     string            `yaml:"skip_env,omitempty"`
	ConnectEnv  string            `yaml:"connect_env,omitempty"`
	Absent      []string          `yaml:"absent,omitempty"`
	Contains    map[string]string `yaml:"contains,omitempty"`
	Cases       []Case            `yaml:"cases,omitempty"`
	Probes      []Probe           `yaml:"probes,omitempty"`
	Items       []SettingItem     `yaml:"items,omitempty"`
	Modes       map[string]string `yaml:"modes,omitempty"`
	Notes       map[string]string `yaml:"notes,omitempty"`
	Reserve     string            `yaml:"reserve,omitempty"`
	PoolEnv     string            `yaml:"pool_env,omitempty"`
	URLEnv      string            `yaml:"url_env,omitempty"`
	DefaultPool int               `yaml:"default_pool,omitempty"`
	Reads       []string          `yaml:"reads,omitempty"`
	Want        string            `yaml:"want,omitempty"`

	Pass string `yaml:"pass,omitempty"`
	Fail string `yaml:"fail,omitempty"`
	Note string `yaml:"note,omitempty"`
	None string `yaml:"none,omitempty"`
}

// HostIf is a host to resolve, when If holds.
type HostIf struct {
	Host string `yaml:"host"`
	If   string `yaml:"if,omitempty"` // a variable that must be set
}

// Case maps program output (a regexp) to a verdict.
type Case struct {
	Match string `yaml:"match"`
	Level string `yaml:"level"` // pass, fail, note, warn
	Say   string `yaml:"say"`
}

// Probe is one question to an `ask` endpoint (Caddy's on-demand TLS check).
type Probe struct {
	Query  string `yaml:"query,omitempty"` // picks a label; empty answer = None
	Host   string `yaml:"host"`            // {value} is the query's answer
	Expect string `yaml:"expect"`          // ok or refuse
	If     string `yaml:"if,omitempty"`
	Unset  string `yaml:"unset,omitempty"` // printed (dim) when If is empty
	None   string `yaml:"none,omitempty"`
	Pass   string `yaml:"pass"`
	Fail   string `yaml:"fail"`
}

// SettingItem is one key the app may keep in its settings table or in .env.
type SettingItem struct {
	Setting string `yaml:"setting"`
	Env     string `yaml:"env"`
	Name    string `yaml:"name"`
}

// Serve is the optional web dashboard (`runsnip serve`).
type Serve struct {
	Listen string `yaml:"listen,omitempty"`
}

// Load reads dir/stack.yaml and, when present, dir/stack.local.yaml over it.
func Load(dir string) (*Stack, error) {
	var s Stack
	p := filepath.Join(dir, FileName)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s missing: every stack keeps one (runsnip adopt writes a draft)", p)
	}
	if err != nil {
		return nil, err
	}
	if err := decode(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, LocalName)); err == nil {
		if err := decode(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, LocalName), err)
		}
	}
	if s.Standard == 0 {
		return nil, fmt.Errorf("%s: `standard: %d` is required (the version of the stack standard it follows)", p, Standard)
	}
	if s.Standard > Standard {
		return nil, fmt.Errorf("%s follows stack standard v%d; this runsnip knows v%d — update it (runsnip infra pull)", p, s.Standard, Standard)
	}
	return &s, nil
}

// Parse reads one stack definition (strict: unknown keys are errors).
func Parse(b []byte) (*Stack, error) {
	var s Stack
	if err := decode(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func decode(b []byte, s *Stack) error {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		return err
	}
	return nil
}

// Marshal writes s as YAML.
func Marshal(s *Stack) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Truth reads an optional bool with its default.
func Truth(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// Matches reports whether name matches pattern (one `*` wildcard at most) and what the wildcard took.
func Matches(pattern, name string) (string, bool) {
	before, after, ok := strings.Cut(pattern, "*")
	if !ok {
		return "", pattern == name
	}
	if len(name) < len(before)+len(after) || !strings.HasPrefix(name, before) || !strings.HasSuffix(name, after) {
		return "", false
	}
	return name[len(before) : len(name)-len(after)], true
}

// Fill replaces {name} and {*} in a pattern rule's file.
func Fill(tmpl, name, star string) string {
	return strings.NewReplacer("{name}", name, "{component}", name, "{*}", star).Replace(tmpl)
}
