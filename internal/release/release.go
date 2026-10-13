// Package release backs `runsnip build` on the build machine: which components there are, their versions, whether
// the tree can be released, what changed since each one's last release tag, the steps a release runs (the project's
// build script, then its infra publisher) and the CI runs that turn bundles into images. Building itself stays in the
// project's scripts.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/run"
)

// Infra is the pseudo-component publishing the server's infra branch.
const Infra = "infra"

// Repo is the source checkout on the build machine.
type Repo struct {
	Dir      string // repository root
	StackDir string // the infra folder holding stack.yaml
	Def      *def.Stack
	Run      run.Runner
}

// Find returns the stack file of a repository: path when given, else infra/stack.yaml, infra/*/stack.yaml.
func Find(top, path string) (string, error) {
	if path != "" {
		return path, nil
	}
	cands := []string{filepath.Join(top, "infra", def.FileName)}
	more, _ := filepath.Glob(filepath.Join(top, "infra", "*", def.FileName))
	for _, c := range append(cands, more...) {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no %s in %s/infra (RUNSNIP_STACK names another; runsnip adopt writes a draft)", def.FileName, top)
}

// Open finds the repository containing dir and its stack definition.
func Open(dir, stackPath string) (*Repo, error) {
	r := &Repo{Dir: dir, Run: run.Exec{}}
	top, err := r.out(context.Background(), "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository: %v", err)
	}
	r.Dir = strings.TrimSpace(top)
	p, err := Find(r.Dir, stackPath)
	if err != nil {
		return nil, err
	}
	r.StackDir = filepath.Dir(p)
	d, err := def.Load(r.StackDir)
	if err != nil {
		return nil, err
	}
	r.Def = d
	return r, nil
}

func (r *Repo) out(ctx context.Context, name string, args ...string) (string, error) {
	return run.Output(ctx, r.Run, run.Call{Dir: r.Dir, Name: name, Args: args})
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// ───────────────────────── the release recipe ─────────────────────────

// BuildCmd is the build script (default scripts/build.sh).
func (r *Repo) BuildCmd() []string {
	if len(r.Def.Release.Build) > 0 {
		return r.Def.Release.Build
	}
	return []string{"bash", "scripts/build.sh"}
}

// ListCmd prints the components the build script knows (default: the build script with --list).
func (r *Repo) ListCmd() []string {
	if len(r.Def.Release.List) > 0 {
		return r.Def.Release.List
	}
	return append(append([]string{}, r.BuildCmd()...), "--list")
}

// PublishInfraCmd publishes the infra branch (default scripts/publish-infra.sh).
func (r *Repo) PublishInfraCmd() []string {
	if len(r.Def.Release.PublishInfra) > 0 {
		return r.Def.Release.PublishInfra
	}
	return []string{"bash", "scripts/publish-infra.sh"}
}

// DryRunFlag is what the scripts take for a dry run.
func (r *Repo) DryRunFlag() string { return or(r.Def.Release.DryRunFlag, "--dry-run") }

// Remote is the git remote releases go to.
func (r *Repo) Remote() string { return or(r.Def.Release.Remote, "origin") }

// Main is the branch a released commit must be on.
func (r *Repo) Main() string { return or(r.Def.Release.Main, "main") }

// Components are the releasable components: the definition's list, else what the build script lists.
func (r *Repo) Components(ctx context.Context) ([]string, error) {
	if len(r.Def.Release.Components) > 0 {
		return r.Def.Release.Components, nil
	}
	c := r.ListCmd()
	out, err := r.out(ctx, c[0], c[1:]...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// DefaultVersionFiles: a component keeps its version in services/<name>/package.json for service-<name>, else in
// <name>/package.json (stack standard v1, "release").
var DefaultVersionFiles = []def.PatternRule{{Match: "service-*", File: "services/{*}/package.json"}, {Match: "*", File: "{name}/package.json"}}

// DefaultPaths: what changes a component.
var DefaultPaths = []def.PatternRule{{Match: "service-*", Paths: []string{"services/{*}"}}, {Match: "*", Paths: []string{"{name}/"}}}

func rule(rules []def.PatternRule, c string) (def.PatternRule, string, bool) {
	for _, rl := range rules {
		if star, ok := def.Matches(rl.Match, c); ok {
			return rl, star, true
		}
	}
	return def.PatternRule{}, "", false
}

// VersionFile is where c keeps its version.
func (r *Repo) VersionFile(c string) string {
	rules := append(append([]def.PatternRule{}, r.Def.Release.VersionFiles...), DefaultVersionFiles...)
	rl, star, _ := rule(rules, c)
	return def.Fill(rl.File, c, star)
}

// Paths are the files whose change means c needs a release.
func (r *Repo) Paths(c string) []string {
	if c == Infra {
		if len(r.Def.Release.InfraPaths) > 0 {
			return r.Def.Release.InfraPaths
		}
		rel, _ := filepath.Rel(r.Dir, r.StackDir)
		return []string{rel + "/"}
	}
	rules := append(append([]def.PatternRule{}, r.Def.Release.Paths...), DefaultPaths...)
	rl, star, _ := rule(rules, c)
	var out []string
	for _, p := range rl.Paths {
		out = append(out, def.Fill(p, c, star))
	}
	return out
}

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
var versionRe = regexp.MustCompile(`("version"\s*:\s*")([^"]*)(")`)

// Version reads c's version.
func (r *Repo) Version(c string) (string, error) {
	f := r.VersionFile(c)
	b, err := os.ReadFile(filepath.Join(r.Dir, f))
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(f, ".json") {
		return strings.TrimSpace(string(b)), nil
	}
	var p struct{ Version string }
	if err := json.Unmarshal(b, &p); err != nil {
		return "", fmt.Errorf("%s: %v", f, err)
	}
	return p.Version, nil
}

// Bump raises one part of a version ("patch", "minor", "major").
func Bump(version, part string) (string, error) {
	m := semverRe.FindStringSubmatch(version)
	if m == nil {
		return "", fmt.Errorf("version %q is not semver", version)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	switch part {
	case "major":
		maj, min, pat = maj+1, 0, 0
	case "minor":
		min, pat = min+1, 0
	case "patch":
		pat++
	default:
		return "", fmt.Errorf("unknown part %q", part)
	}
	return fmt.Sprintf("%d.%d.%d", maj, min, pat), nil
}

// SetVersions writes each component's version (only the value: a package.json keeps its formatting), commits those
// files alone (`git commit -- <files>`: whatever else is staged stays staged) and pushes the main branch.
func (r *Repo) SetVersions(ctx context.Context, versions map[string]string) error {
	if len(versions) == 0 {
		return fmt.Errorf("no component to set a version for")
	}
	var comps []string
	for c, v := range versions {
		if !semverRe.MatchString(v) {
			return fmt.Errorf("%s: version %q is not semver (x.y.z)", c, v)
		}
		comps = append(comps, c)
	}
	sort.Strings(comps)
	var files, parts []string
	for _, c := range comps {
		v, f := versions[c], r.VersionFile(c)
		p := filepath.Join(r.Dir, f)
		var nb []byte
		if strings.HasSuffix(f, ".json") {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			loc := versionRe.FindIndex(b)
			if loc == nil {
				return fmt.Errorf("%s has no \"version\"", f)
			}
			nb = append(append(append([]byte{}, b[:loc[0]]...), versionRe.ReplaceAll(b[loc[0]:loc[1]], []byte("${1}"+v+"${3}"))...), b[loc[1]:]...)
		} else {
			nb = []byte(v + "\n")
		}
		if err := os.WriteFile(p, nb, 0o644); err != nil {
			return err
		}
		files = append(files, f)
		parts = append(parts, c+" "+v)
	}
	args := append([]string{"commit", "-m", "chore(release): " + strings.Join(parts, ", "), "--"}, files...)
	if _, err := r.out(ctx, "git", args...); err != nil {
		return err
	}
	_, err := r.out(ctx, "git", "push", r.Remote(), "HEAD:"+r.Main())
	return err
}

// Steps are the commands a release of comps (and infra) runs, in order.
func (r *Repo) Steps(comps []string, infra, dry, verify bool) [][]string {
	var s [][]string
	if len(comps) > 0 {
		c := append([]string{}, r.BuildCmd()...)
		if dry {
			c = append(c, r.DryRunFlag())
		}
		s = append(s, append(c, comps...))
	}
	if infra {
		c := append([]string{}, r.PublishInfraCmd()...)
		if dry {
			c = append(c, r.DryRunFlag())
		}
		s = append(s, c)
	}
	if verify && len(s) > 0 && len(r.Def.Release.Verify) > 0 {
		s = append([][]string{r.Def.Release.Verify}, s...)
	}
	return s
}

// Progress patterns of the build script's output (defaults: scripts/build.sh's).
func (r *Repo) Progress() (building, pushed *regexp.Regexp) {
	b := or(r.Def.Release.Progress.Building, `^▶ bundle ([a-z0-9.-]+) `)
	p := or(r.Def.Release.Progress.Pushed, `→ deploy/([a-z0-9.-]+)/v`)
	return regexp.MustCompile(b), regexp.MustCompile(p)
}

// ───────────────────────── readiness ─────────────────────────

// Tag is a release tag on the remote.
type Tag struct{ Version, Commit string }

// State is what decides whether a release can run now.
type State struct {
	Components     []string
	Versions       map[string]string
	Commit, Branch string
	Dirty          []string
	Pushed         bool
	DockerOK, GhOK bool
	Released       map[string]Tag
	Changed        map[string][]string
	Problems       []string
}

// NeedsBump: c changed since its last release but still carries that release's version — the build would refuse
// it (the tag exists on another commit) until the version is raised.
func (st State) NeedsBump(c string) bool {
	t, ok := st.Released[c]
	return ok && t.Version != "" && t.Version == st.Versions[c] && len(st.Changed[c]) > 0
}

// TagPattern is the release tag of a component (default {component}/v{version}).
func (r *Repo) TagPattern() string { return or(r.Def.Release.Tag, "{component}/v{version}") }

// InfraCommit is the infra branch's commit subject (default "infra from {sha}").
func (r *Repo) InfraCommit() string { return or(r.Def.Release.InfraCommit, "infra from {sha}") }

// Inspect fetches the remote (tags included) and reports the state.
func (r *Repo) Inspect(ctx context.Context) State {
	st := State{Versions: map[string]string{}, Released: map[string]Tag{}, Changed: map[string][]string{}}
	comps, err := r.Components(ctx)
	if err != nil {
		st.Problems = append(st.Problems, "cannot list the components: "+err.Error())
	}
	st.Components = comps
	var bad []string
	for _, c := range comps {
		v, err := r.Version(c)
		st.Versions[c] = v
		if err != nil || !semverRe.MatchString(v) {
			bad = append(bad, fmt.Sprintf("%s (%s: %q)", c, r.VersionFile(c), v))
		}
	}
	if out, err := r.out(ctx, "git", "rev-parse", "--short", "HEAD"); err == nil {
		st.Commit = strings.TrimSpace(out)
	}
	if out, err := r.out(ctx, "git", "branch", "--show-current"); err == nil {
		st.Branch = strings.TrimSpace(out)
	}
	if out, err := r.out(ctx, "git", "status", "--porcelain", "--untracked-files=no"); err == nil {
		for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				st.Dirty = append(st.Dirty, l)
			}
		}
	}
	fetchErr := ""
	if _, err := r.out(ctx, "git", "fetch", r.Remote(), "--quiet", "--tags", "--prune"); err != nil {
		fetchErr = err.Error()
	}
	_, err = r.out(ctx, "git", "merge-base", "--is-ancestor", "HEAD", r.Remote()+"/"+r.Main())
	st.Pushed = err == nil
	_, err = r.out(ctx, "docker", "buildx", "version")
	st.DockerOK = err == nil
	if st.DockerOK {
		_, err = r.out(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
		st.DockerOK = err == nil
	}
	_, err = r.out(ctx, "gh", "auth", "status")
	st.GhOK = err == nil
	if out, err := r.out(ctx, "git", "tag", "--list", "--format=%(refname:short)\t%(objectname:short)\t%(*objectname:short)"); err == nil {
		st.Released = ParseTags(out, r.TagPattern())
	}
	infraBranch := or(r.Def.Infra.Branch, "infra")
	if out, err := r.out(ctx, "git", "log", "-1", "--format=%s", r.Remote()+"/"+infraBranch); err == nil {
		prefix := strings.Split(r.InfraCommit(), "{sha}")[0]
		if sha, ok := strings.CutPrefix(strings.TrimSpace(out), prefix); ok {
			st.Released[Infra] = Tag{Commit: strings.Fields(sha + " ")[0]}
		}
	}
	for _, c := range append(append([]string{}, comps...), Infra) {
		st.Changed[c] = r.changed(ctx, c, st.Released[c])
	}
	if len(bad) > 0 {
		st.Problems = append(st.Problems, "version missing or not semver: "+strings.Join(bad, ", "))
	}
	if len(st.Dirty) > 0 {
		st.Problems = append(st.Problems, fmt.Sprintf("%d uncommitted change(s) — commit or stash them", len(st.Dirty)))
	}
	if !st.Pushed {
		st.Problems = append(st.Problems, "HEAD is not on "+r.Remote()+"/"+r.Main()+" — push first (CI tags the commit it builds from)")
	}
	if !st.DockerOK {
		st.Problems = append(st.Problems, "Docker with buildx is not running")
	}
	if fetchErr != "" {
		st.Problems = append(st.Problems, "cannot reach "+r.Remote()+": "+fetchErr)
	}
	return st
}

// ParseTags keeps, per component, the highest released version (and its commit), reading tags shaped by pattern.
func ParseTags(out, pattern string) map[string]Tag {
	re := regexp.MustCompile("^" + strings.NewReplacer(regexp.QuoteMeta("{component}"), `(.+?)`, regexp.QuoteMeta("{version}"), `(\d+\.\d+\.\d+)`).Replace(regexp.QuoteMeta(pattern)) + "$")
	res := map[string]Tag{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(l, "\t")
		m := re.FindStringSubmatch(f[0])
		if m == nil || len(f) < 2 {
			continue
		}
		comp, ver := m[1], m[2]
		commit := f[1]
		if len(f) > 2 && f[2] != "" {
			commit = f[2]
		}
		if cur, ok := res[comp]; !ok || semverLess(cur.Version, ver) {
			res[comp] = Tag{Version: ver, Commit: commit}
		}
	}
	return res
}

func semverLess(a, b string) bool {
	pa, pb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return a < b
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return false
}

func (r *Repo) changed(ctx context.Context, c string, t Tag) []string {
	if t.Commit == "" {
		return []string{"(never released)"}
	}
	out, err := r.out(ctx, "git", append([]string{"diff", "--name-only", t.Commit, "HEAD", "--"}, r.Paths(c)...)...)
	if err != nil {
		return []string{"(cannot diff against " + t.Commit + ")"}
	}
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files
}

// ───────────────────────── CI ─────────────────────────

// Run is one CI run of a deploy branch.
type Run struct {
	Component, Version, Status, Conclusion, URL string
	Created                                     time.Time
}

// Runs lists recent image builds (deploy branches), newest first.
func (r *Repo) Runs(ctx context.Context) ([]Run, error) {
	out, err := r.out(ctx, "gh", "run", "list", "--limit", "40", "--json", "headBranch,status,conclusion,url,createdAt")
	if err != nil {
		return nil, err
	}
	return ParseRuns(out, or(r.Def.Release.DeployBranch, "deploy/{component}/v{version}"))
}

// ParseRuns reads `gh run list --json` for branches shaped by pattern.
func ParseRuns(out, pattern string) ([]Run, error) {
	var rows []struct {
		HeadBranch, Status, Conclusion, URL string
		CreatedAt                           time.Time
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return nil, err
	}
	re := regexp.MustCompile("^" + strings.NewReplacer(regexp.QuoteMeta("{component}"), `(.+?)`, regexp.QuoteMeta("{version}"), `([0-9][^/]*)`).Replace(regexp.QuoteMeta(pattern)) + "$")
	var runs []Run
	for _, x := range rows {
		m := re.FindStringSubmatch(x.HeadBranch)
		if m == nil {
			continue
		}
		runs = append(runs, Run{Component: m[1], Version: m[2], Status: x.Status, Conclusion: x.Conclusion, URL: x.URL, Created: x.CreatedAt})
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Created.After(runs[j].Created) })
	return runs, nil
}
