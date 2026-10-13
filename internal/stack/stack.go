// Package stack operates one compose stack on its server: a checkout of the project's infra branch holding
// stack.yaml, the compose file, .env and the proxy configs. Services, components, versions, profiles and the data
// directory come from the compose file and the stack definition together; every external program goes through a
// run.Runner, so the logic is tested without Docker.
package stack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/runsnip/ops/internal/compose"
	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/dockerapi"
	"github.com/runsnip/ops/internal/envfile"
	"github.com/runsnip/ops/internal/expand"
	"github.com/runsnip/ops/internal/run"
)

// Stack is one stack directory.
type Stack struct {
	Dir         string
	Def         *def.Stack
	ComposePath string
	EnvPath     string
	ExamplePath string
	Compose     *compose.File
	Run         run.Runner
	// Getenv reads this process's environment (tests replace it).
	Getenv func(string) string
	// GitEnv is added to every git command (a registered stack's deploy key: GIT_SSH_COMMAND).
	GitEnv []string

	dockerOnce   sync.Once
	dockerClient *dockerapi.Client
}

// Open reads the stack in dir. RUNSNIP_COMPOSE_FILE and RUNSNIP_ENV_FILE point at other files (development).
func Open(dir string) (*Stack, error) {
	d, err := def.Load(dir)
	if err != nil {
		return nil, err
	}
	return New(dir, d)
}

// New builds a stack from a definition already read.
func New(dir string, d *def.Stack) (*Stack, error) {
	s := &Stack{Dir: dir, Def: d, Run: run.Exec{}, Getenv: os.Getenv}
	switch {
	case os.Getenv("RUNSNIP_COMPOSE_FILE") != "":
		s.ComposePath = os.Getenv("RUNSNIP_COMPOSE_FILE")
	case d.Compose != "":
		s.ComposePath = filepath.Join(dir, d.Compose)
	default:
		p, err := compose.Find(dir)
		if err != nil {
			return nil, err
		}
		s.ComposePath = p
	}
	s.EnvPath = filepath.Join(dir, or(d.Env, ".env"))
	if p := os.Getenv("RUNSNIP_ENV_FILE"); p != "" {
		s.EnvPath = p
	}
	s.ExamplePath = filepath.Join(dir, or(d.EnvExample, ".env.example"))
	c, err := compose.Load(s.ComposePath)
	if err != nil {
		return nil, err
	}
	s.Compose = c
	return s, nil
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Name is the stack's name: the definition's, else compose's `name:`, else the folder's.
func (s *Stack) Name() string {
	if s.Def.Name != "" {
		return s.Def.Name
	}
	if s.Compose.Name != "" {
		return s.Compose.Name
	}
	return filepath.Base(s.Dir)
}

// Title is the name shown in headers.
func (s *Stack) Title() string { return or(s.Def.Title, s.Name()) }

// ProjectName is the compose project (data directory default, backups): compose's `name:`, else the folder's.
func (s *Stack) ProjectName() string {
	// Compose's own order: COMPOSE_PROJECT_NAME from the environment, then from .env, then the file's name:, then the
	// folder. Reading containers by their project label must agree with what compose started.
	if v := s.Getenv("COMPOSE_PROJECT_NAME"); v != "" {
		return v
	}
	if v := s.Values()["COMPOSE_PROJECT_NAME"]; v != "" {
		return v
	}
	if s.Compose.Name != "" {
		return s.Compose.Name
	}
	return filepath.Base(s.Dir)
}

// Env loads .env.
func (s *Stack) Env() (*envfile.File, error) { return envfile.Load(s.EnvPath) }

// Values is .env's variables, empty when .env is missing.
func (s *Stack) Values() map[string]string {
	if e, err := s.Env(); err == nil {
		return e.Values()
	}
	return map[string]string{}
}

// Lookup reads a variable: .env first, then this process's environment.
func (s *Stack) lookup(vals map[string]string) expand.Lookup {
	return func(k string) (string, bool) {
		if v, ok := vals[k]; ok {
			return v, true
		}
		if v := s.Getenv(k); v != "" {
			return v, true
		}
		return "", false
	}
}

// Expand substitutes ${VAR} with .env's values.
func (s *Stack) Expand(tmpl string) string { return expand.String(tmpl, s.lookup(s.Values())) }

// ExpandWith substitutes with values already read.
func (s *Stack) ExpandWith(tmpl string, vals map[string]string) string {
	return expand.String(tmpl, s.lookup(vals))
}

// DataDir is where the stack's data lives (default ${DATA_DIR:-/srv/<compose name>}).
func (s *Stack) DataDir() string {
	t := s.Def.DataDir
	if t == "" {
		t = "${DATA_DIR:-/srv/" + s.ProjectName() + "}"
	}
	return s.Expand(t)
}

// ───────────────────────── profiles ─────────────────────────

// ProfilesBlock is the compose block mapping profiles to the variables turning them on.
const ProfilesBlock = "x-runsnip-profiles"

// Profiles maps each profile to the .env variable that turns it on.
func (s *Stack) Profiles() map[string]string {
	if len(s.Def.Profiles) > 0 {
		return s.Def.Profiles
	}
	return s.Compose.StringMap(or(s.Def.ProfilesBlock, ProfilesBlock))
}

// ActiveProfiles are the profiles whose variable has a value, sorted.
func (s *Stack) ActiveProfiles(vals map[string]string) []string {
	var out []string
	for p, v := range s.Profiles() {
		if vals[v] != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// ProfileOn reports whether profile p is on.
func (s *Stack) ProfileOn(p string, vals map[string]string) bool {
	v := s.Profiles()[p]
	return v != "" && vals[v] != ""
}

// Enabled reports whether a service runs with the profiles on, and why not.
func (s *Stack) Enabled(name string, vals map[string]string) (bool, string) {
	svc, ok := s.Compose.Services[name]
	if !ok {
		return false, "not in " + filepath.Base(s.ComposePath)
	}
	if len(svc.Profiles) == 0 {
		return true, ""
	}
	for _, p := range svc.Profiles {
		if s.ProfileOn(p, vals) {
			return true, ""
		}
	}
	for _, p := range svc.Profiles {
		if v := s.Profiles()[p]; v != "" {
			return false, "off, " + v + " is not set"
		}
	}
	return false, "off — profile " + strings.Join(svc.Profiles, ",") + " is not in " + or(s.Def.ProfilesBlock, ProfilesBlock) + " of " + filepath.Base(s.ComposePath)
}

// EnabledServices are the services compose runs with the profiles on, in file order.
func (s *Stack) EnabledServices(vals map[string]string) []string {
	var out []string
	for _, n := range s.Compose.Order {
		if ok, _ := s.Enabled(n, vals); ok {
			out = append(out, n)
		}
	}
	return out
}

// HasService reports whether compose defines name.
func (s *Stack) HasService(name string) bool { _, ok := s.Compose.Services[name]; return ok }

// ───────────────────────── docker compose ─────────────────────────

// composeArgs: the files and the env file named explicitly, so the command means the same from any directory.
func (s *Stack) composeArgs(args ...string) []string {
	out := []string{"compose", "--project-directory", s.Dir}
	for _, f := range s.Compose.Files {
		out = append(out, "-f", f)
	}
	out = append(out, "--env-file", s.EnvPath)
	return append(out, args...)
}

// composeEnv turns on the profiles .env enables: every compose command sees the same services (a `stop` that did
// not see a profiled service would leave it running, and `ps` would lie).
func (s *Stack) composeEnv() []string {
	return []string{"COMPOSE_PROFILES=" + strings.Join(s.ActiveProfiles(s.Values()), ",")}
}

// DC runs `docker compose …` with output to out (nil discards it).
func (s *Stack) DC(ctx context.Context, out io.Writer, args ...string) error {
	if out == nil {
		out = io.Discard
	}
	return s.Run.Run(ctx, run.Call{Dir: s.Dir, Env: s.composeEnv(), Stdout: out, Stderr: out, Name: "docker", Args: s.composeArgs(args...)})
}

// DCOutput runs `docker compose …` and returns stdout.
func (s *Stack) DCOutput(ctx context.Context, args ...string) (string, error) {
	return run.Output(ctx, s.Run, run.Call{Dir: s.Dir, Env: s.composeEnv(), Name: "docker", Args: s.composeArgs(args...)})
}

// Attach runs `docker compose …` on this terminal (psql, a shell, logs -f).
func (s *Stack) Attach(ctx context.Context, args ...string) error {
	err := s.Run.Run(ctx, run.Call{Dir: s.Dir, Env: s.composeEnv(), Attach: true, Name: "docker", Args: s.composeArgs(args...)})
	if ctx.Err() != nil {
		return nil // Ctrl-C ends logs -f
	}
	return err
}

// Exec runs a command in a service's container and returns its output (stdout, then stderr).
func (s *Stack) Exec(ctx context.Context, svc string, env map[string]string, cmd ...string) (string, error) {
	args := []string{"exec", "-T"}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}
	args = append(append(args, svc), cmd...)
	return s.DCOutput(ctx, args...)
}

// ExecCombined runs a command in a service's container and returns stdout and stderr together, whatever its exit
// status (busybox wget prints the answer's headers on stderr and exits 1 on every non-2xx code).
func (s *Stack) ExecCombined(ctx context.Context, svc string, cmd ...string) string {
	var buf strings.Builder
	_ = s.Run.Run(ctx, run.Call{Dir: s.Dir, Env: s.composeEnv(), Stdout: &buf, Stderr: &buf, Name: "docker", Args: s.composeArgs(append([]string{"exec", "-T", svc}, cmd...)...)})
	return buf.String()
}

// ───────────────────────── components ─────────────────────────

// Component is a released part: one image whose tag is a .env variable, run by one or more services.
type Component struct {
	Name     string // the main service's name
	Service  string
	Services []string // every service running this image (migrations first, then Service)
	Image    compose.Image
	Before   []string
}

// ImageName is the image's own name (api, shop-web).
func (c Component) ImageName() string { return c.Image.ShortName() }

// Components lists the released components in compose order.
func (s *Stack) Components() []Component {
	byVar := map[string]*Component{}
	var order []string
	for _, n := range s.Compose.Order {
		img, err := compose.ParseImage(s.Compose.Services[n].Image)
		if err != nil {
			continue
		}
		c, ok := byVar[img.TagVar]
		if !ok {
			c = &Component{Image: img}
			byVar[img.TagVar] = c
			order = append(order, img.TagVar)
		}
		c.Services = append(c.Services, n)
	}
	var out []Component
	for _, v := range order {
		c := byVar[v]
		c.Service = mainService(c.Services, c.Image.ShortName(), s.Compose)
		c.Name = c.Service
		for _, n := range c.Services {
			if n != c.Service {
				c.Before = append(c.Before, n)
			}
		}
		if o, ok := s.Def.Services[c.Service]; ok && o.Before != nil {
			c.Before = o.Before
		}
		if !s.included(c) {
			continue
		}
		out = append(out, *c)
	}
	return out
}

// mainService picks, among services sharing an image, the one that is the component: named like the image, else
// the first with a health check, else the first.
func mainService(names []string, image string, f *compose.File) string {
	if len(names) == 1 {
		return names[0]
	}
	for _, n := range names {
		if image == n || strings.HasSuffix(image, "-"+n) {
			return n
		}
	}
	for _, n := range names {
		if f.Services[n].Healthcheck {
			return n
		}
	}
	return names[0]
}

func (s *Stack) included(c *Component) bool {
	in := func(list []string) bool {
		for _, x := range list {
			if x == c.Name || x == c.ImageName() {
				return true
			}
		}
		return false
	}
	if len(s.Def.Components.Only) > 0 && !in(s.Def.Components.Only) {
		return false
	}
	return !in(s.Def.Components.Exclude)
}

// Component finds a component by its service name or its image name.
func (s *Stack) Component(name string) (Component, bool) {
	for _, c := range s.Components() {
		if c.Name == name || c.ImageName() == name {
			return c, true
		}
	}
	return Component{}, false
}

// ComponentNames lists the components' names.
func (s *Stack) ComponentNames() []string {
	var out []string
	for _, c := range s.Components() {
		out = append(out, c.Name)
	}
	return out
}

// DefaultComponent is the one `pin <tag>` and `rollback` act on without a name.
func (s *Stack) DefaultComponent() string {
	if s.Def.Components.Default != "" {
		return s.Def.Components.Default
	}
	if cs := s.Components(); len(cs) > 0 {
		return cs[0].Name
	}
	return ""
}

// Tag is the version .env pins for c (its variable, else the compose default, else latest).
func (s *Stack) Tag(c Component, vals map[string]string) string {
	if v := vals[c.Image.TagVar]; v != "" {
		return v
	}
	if d := s.ExpandWith(c.Image.TagDefault, vals); d != "" {
		return d
	}
	return "latest"
}

// ImageRef is c's image at tag.
func (s *Stack) ImageRef(c Component, vals map[string]string, tag string) string {
	return s.ExpandWith(c.Image.Repo, vals) + ":" + tag
}

// ───────────────────────── checks on names ─────────────────────────

// ErrUnknown names what exists when a name does not.
type ErrUnknown struct{ Kind, Name, Known string }

func (e ErrUnknown) Error() string {
	return fmt.Sprintf("no %s %q. Known: %s", e.Kind, e.Name, e.Known)
}

// RequireService fails on a name compose does not define.
func (s *Stack) RequireService(name string) error {
	if !s.HasService(name) {
		return ErrUnknown{"service", name, strings.Join(s.Compose.Order, " ")}
	}
	return nil
}

// RequireEnabled fails on a service that is off because its profile's variable is unset, saying how to turn it on
// (instead of letting compose start it without the configuration it needs).
func (s *Stack) RequireEnabled(name string) error {
	ok, why := s.Enabled(name, s.Values())
	if ok {
		return nil
	}
	return fmt.Errorf("%s is %s — set it in .env (see %s; runsnip env generate fills generated values)", name, why, filepath.Base(s.ExamplePath))
}

var secretRe = regexp.MustCompile(`(SECRET|PASSWORD|TOKEN|_JWT_KEY|API_KEY|ACCESS_KEY|PRIVATE|SIGNING_KEYS)`)

// IsSecret reports whether a variable holds a secret (the stack's pattern, else the standard's).
func (s *Stack) IsSecret(key string) bool {
	if s.Def.EnvRules.Secrets != "" {
		if re, err := regexp.Compile(s.Def.EnvRules.Secrets); err == nil {
			return re.MatchString(key)
		}
	}
	return secretRe.MatchString(key) && !strings.HasSuffix(key, "_PUBLIC_KEY") && !strings.HasSuffix(key, "_PUBLIC_KEYS") && !strings.HasSuffix(key, "_URL")
}

// FileExists reports whether a path relative to the stack exists.
func (s *Stack) FileExists(rel string) bool {
	_, err := os.Stat(filepath.Join(s.Dir, rel))
	return err == nil
}

// IsMissingEnv reports whether err is a missing .env.
func IsMissingEnv(err error) bool { return errors.Is(err, envfile.ErrMissing) }
