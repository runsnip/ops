package stack

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/run"
)

// Say prints a step.
func Say(out io.Writer, format string, a ...any) { fmt.Fprintf(out, "▶ "+format+"\n", a...) }

// Proxies are the services whose whole configuration is a file mounted from the stack (Caddy, nginx…): the
// definition's list, else every service running a known proxy image.
func (s *Stack) Proxies() []string {
	if s.Def.Proxies != nil {
		return s.Def.Proxies
	}
	var out []string
	for _, n := range s.Compose.Order {
		img := s.Compose.Services[n].Image
		base := img
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		base, _, _ = strings.Cut(base, ":")
		switch base {
		case "caddy", "nginx", "traefik", "haproxy", "envoy":
			out = append(out, n)
		}
	}
	return out
}

func (s *Stack) enabledOf(names []string) []string {
	vals := s.Values()
	var out []string
	for _, n := range names {
		if ok, _ := s.Enabled(n, vals); ok {
			out = append(out, n)
		}
	}
	return out
}

// Start pulls every image and starts or refreshes the whole stack: git pull of the infra checkout, then pull, then
// up (or the definition's order), then the proxies recreated (they read their config file only when created, and
// `up -d` does not look at a mounted file's content), then unused images pruned.
func (s *Stack) Start(ctx context.Context, out io.Writer) error {
	vals := s.Values()
	if _, err := s.Env(); err != nil {
		return err
	}
	if def.Truth(s.Def.Start.GitPull, true) && s.FileExists(".git") {
		Say(out, "git pull")
		if err := s.Run.Run(ctx, run.Call{Dir: s.Dir, Env: s.GitEnv, Stdout: out, Stderr: out, Name: "git", Args: []string{"pull", "--ff-only"}}); err != nil {
			fmt.Fprintln(out, "! git pull did not run; going on with this checkout")
		}
	}
	var parts, off []string
	for _, c := range s.Components() {
		if ok, _ := s.Enabled(c.Service, vals); ok {
			parts = append(parts, c.Name+" "+s.Tag(c, vals))
		} else {
			off = append(off, c.Name)
		}
	}
	if s.Def.Start.CheckImages {
		if err := s.CheckImages(ctx, out); err != nil {
			return err
		}
	}
	pull := def.Truth(s.Def.Start.Pull, true)
	if pull {
		Say(out, "pull images (%s)", strings.Join(parts, ", "))
		if len(off) > 0 {
			fmt.Fprintf(out, "  off: %s — runsnip version\n", strings.Join(off, " "))
		}
		if err := s.DC(ctx, out, "pull"); err != nil {
			return err
		}
	}
	extra := []string{}
	if !pull {
		extra = []string{"--no-build", "--pull", "never"}
	}
	if len(s.Def.Start.Order) > 0 {
		for _, group := range s.Def.Start.Order {
			g := s.enabledOf(group)
			if len(g) == 0 {
				continue
			}
			Say(out, "up %s", strings.Join(g, " "))
			if err := s.DC(ctx, out, append(append([]string{"up", "-d", "--no-deps"}, extra...), g...)...); err != nil {
				return err
			}
		}
	} else {
		Say(out, "up")
		if err := s.DC(ctx, out, append([]string{"up", "-d", "--remove-orphans"}, extra...)...); err != nil {
			return err
		}
	}
	recreate := s.Def.Start.Recreate
	if recreate == nil {
		recreate = s.Proxies()
	}
	if r := s.enabledOf(recreate); len(r) > 0 {
		Say(out, "reload the configuration of %s", strings.Join(r, " and "))
		if err := s.DC(ctx, out, append(append([]string{"up", "-d", "--no-deps", "--force-recreate"}, extra...), r...)...); err != nil {
			return err
		}
	}
	if def.Truth(s.Def.Start.Prune, true) {
		_ = s.Run.Run(ctx, run.Call{Name: "docker", Args: []string{"image", "prune", "-f"}, Stdout: io.Discard, Stderr: io.Discard})
	}
	fmt.Fprintln(out)
	return s.DC(ctx, out, "ps")
}

// CheckImages makes sure every enabled component's pinned image is published before anything is pulled: compose
// stops the whole pull at the first missing image, with an error that does not say which version to fix.
func (s *Stack) CheckImages(ctx context.Context, out io.Writer) error {
	vals := s.Values()
	var missing []string
	for _, c := range s.Components() {
		if ok, _ := s.Enabled(c.Service, vals); !ok {
			continue
		}
		ref := s.ImageRef(c, vals, s.Tag(c, vals))
		if !s.ImageExists(ctx, ref) {
			missing = append(missing, ref)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintln(out, "✓ every pinned image is published")
		return nil
	}
	return fmt.Errorf("not published or not readable: %s. Nothing was started: pin each to a released version (runsnip pin <component> <version>); none readable at all? runsnip login", strings.Join(missing, ", "))
}

// Restart pulls (unless noPull) and recreates one service, without touching what it depends on (`--no-deps`:
// restarting the app must not restart the database). It recreates instead of `docker compose restart`, which would
// bring back the same container with the environment it was created with: an edited .env would silently not apply.
func (s *Stack) Restart(ctx context.Context, out io.Writer, svc string, noPull bool) error {
	if err := s.RequireService(svc); err != nil {
		return err
	}
	if _, err := s.Env(); err != nil {
		return err
	}
	if err := s.RequireEnabled(svc); err != nil {
		return err
	}
	targets := []string{svc}
	if c, ok := s.Component(svc); ok && c.Service == svc {
		targets = append(s.enabledOf(c.Before), svc)
	}
	if noPull {
		Say(out, "recreate %s from the configuration as it is now (same image)", svc)
	} else {
		Say(out, "pull %s", strings.Join(targets, " "))
		if err := s.DC(ctx, out, append([]string{"pull"}, targets...)...); err != nil {
			return err
		}
		Say(out, "recreate %s alone", svc)
	}
	for _, t := range targets {
		if err := s.DC(ctx, out, "up", "-d", "--no-deps", "--force-recreate", t); err != nil {
			return err
		}
	}
	if !noPull {
		_ = s.Run.Run(ctx, run.Call{Name: "docker", Args: []string{"image", "prune", "-f"}, Stdout: io.Discard, Stderr: io.Discard})
	}
	fmt.Fprintln(out)
	return s.DC(ctx, out, "ps", svc)
}

// StartService starts one service and what it depends on.
func (s *Stack) StartService(ctx context.Context, out io.Writer, svc string) error {
	if err := s.RequireEnabled(svc); err != nil {
		return err
	}
	return s.DC(ctx, out, "up", "-d", svc)
}

// Stop stops one service (its container stays: `start <component>` or `restart` brings it back), or brings the
// whole stack down when svc is empty.
func (s *Stack) Stop(ctx context.Context, out io.Writer, svc string) error {
	if _, err := s.Env(); err != nil {
		return err
	}
	if svc == "" {
		return s.DC(ctx, out, "down")
	}
	if err := s.RequireService(svc); err != nil {
		return err
	}
	Say(out, "stop %s alone", svc)
	return s.DC(ctx, out, "stop", svc)
}

// StopAll stops every service; containers, networks and volumes stay.
func (s *Stack) StopAll(ctx context.Context, out io.Writer) error { return s.DC(ctx, out, "stop") }

// Reload recreates the proxies so they read their configuration files again (nothing else is touched).
func (s *Stack) Reload(ctx context.Context, out io.Writer) error {
	if _, err := s.Env(); err != nil {
		return err
	}
	p := s.enabledOf(s.Proxies())
	if len(p) == 0 {
		return fmt.Errorf("no proxy service to reload (proxies: in %s)", def.FileName)
	}
	Say(out, "recreate %s from the configuration on disk", strings.Join(p, " and "))
	if err := s.DC(ctx, out, append([]string{"up", "-d", "--no-deps", "--force-recreate"}, p...)...); err != nil {
		return err
	}
	fmt.Fprintln(out)
	return s.DC(ctx, out, append([]string{"ps"}, p...)...)
}

// Pin sets component name ("all" for every enabled one) to tag, keeps the version it had for Rollback, then pulls
// and recreates it.
func (s *Stack) Pin(ctx context.Context, out io.Writer, name, tag string) error {
	env, err := s.Env()
	if err != nil {
		return err
	}
	if tag == "" {
		return fmt.Errorf("no version: runsnip pin <component> <version>")
	}
	if _, ok := s.Component(tag); ok {
		return fmt.Errorf("the order is: runsnip pin <component> <version>")
	}
	var targets []Component
	if name == "all" {
		vals := env.Values()
		for _, c := range s.Components() {
			if ok, _ := s.Enabled(c.Service, vals); ok {
				targets = append(targets, c)
			}
		}
	} else {
		c, ok := s.Component(name)
		if !ok {
			return ErrUnknown{"component", name, strings.Join(s.ComponentNames(), " ")}
		}
		if err := s.RequireEnabled(c.Service); err != nil {
			return err
		}
		targets = []Component{c}
	}
	if s.Def.Start.CheckImages {
		vals := env.Values()
		var missing []string
		for _, c := range targets {
			if !s.ImageExists(ctx, s.ImageRef(c, vals, tag)) {
				missing = append(missing, c.Name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("no image %s for: %s (not released, or runsnip login missing). Nothing changed", tag, strings.Join(missing, " "))
		}
	}
	for _, c := range targets {
		if err := s.setTag(env, c, tag, out); err != nil {
			return err
		}
	}
	if name == "all" {
		return s.Start(ctx, out)
	}
	return s.Restart(ctx, out, targets[0].Service, false)
}

// setTag writes c's pin and keeps the one before it.
func (s *Stack) setTag(env interface {
	Get(string) string
	Set(string, string) error
}, c Component, tag string, out io.Writer) error {
	old := env.Get(c.Image.TagVar)
	if old == "" {
		old = "latest"
	}
	if old != tag {
		if s.Def.Components.Previous == "file" {
			if err := os.WriteFile(s.lastTagFile(c), []byte(old+"\n"), 0o600); err != nil {
				return err
			}
		} else if err := env.Set(c.Image.TagVar+"_PREVIOUS", old); err != nil {
			return err
		}
	}
	if err := env.Set(c.Image.TagVar, tag); err != nil {
		return err
	}
	Say(out, "%s: %s → %s", c.Image.TagVar, old, tag)
	return nil
}

// Rollback pins name back to the version recorded before its last pin.
func (s *Stack) Rollback(ctx context.Context, out io.Writer, name string) error {
	env, err := s.Env()
	if err != nil {
		return err
	}
	c, ok := s.Component(name)
	if !ok {
		return ErrUnknown{"component", name, strings.Join(s.ComponentNames(), " ")}
	}
	prev := s.previous(c, env)
	if prev == "" {
		return fmt.Errorf("no earlier version of %s to go back to", name)
	}
	return s.Pin(ctx, out, c.Name, prev)
}

// ───────────────────────── infra branch ─────────────────────────

// InfraState is the checkout of the infra branch.
type InfraState struct {
	Head, Subject, When string
	Behind              int
	Incoming            []string
}

// InfraBranch is the branch the server checks out.
func (s *Stack) InfraBranch() string { return or(s.Def.Infra.Branch, "infra") }

func (s *Stack) git(ctx context.Context, args ...string) (string, error) {
	return run.Output(ctx, s.Run, run.Call{Dir: s.Dir, Env: s.GitEnv, Name: "git", Args: args})
}

// Infra reads the checkout; fetch also asks the remote for new commits.
func (s *Stack) Infra(ctx context.Context, fetch bool) (InfraState, error) {
	var st InfraState
	remote, branch := or(s.Def.Infra.Remote, "origin"), s.InfraBranch()
	if fetch {
		if _, err := s.git(ctx, "fetch", "--quiet", remote, branch); err != nil {
			return st, err
		}
	}
	b, err := s.git(ctx, "log", "-1", "--format=%h%x09%s%x09%cr")
	if err != nil {
		return st, err
	}
	parts := strings.SplitN(strings.TrimSpace(b), "\t", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	st.Head, st.Subject, st.When = parts[0], parts[1], parts[2]
	if b, err := s.git(ctx, "log", "--format=%h %s", "HEAD.."+remote+"/"+branch); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(b), "\n") {
			if l != "" {
				st.Incoming = append(st.Incoming, l)
			}
		}
		st.Behind = len(st.Incoming)
	}
	return st, nil
}

// DefaultHints say what to run after `infra pull` changed a file.
func (s *Stack) hints() []def.Hint {
	if s.Def.Infra.Hints != nil {
		return s.Def.Infra.Hints
	}
	h := []def.Hint{{Match: filepath.Base(s.ComposePath), Say: "runsnip start"}, {Match: def.FileName, Say: "runsnip doctor (the stack definition changed)"}}
	seen := map[string]bool{}
	for _, p := range s.Proxies() {
		for _, b := range s.Compose.Services[p].Binds {
			src := strings.TrimPrefix(b.Source, "./")
			if strings.HasPrefix(b.Source, "./") && !seen[src] {
				seen[src] = true
				h = append(h, def.Hint{Match: src, Say: "runsnip reload (" + p + " reads it only when recreated)"})
			}
		}
	}
	h = append(h,
		def.Hint{Match: "bin/", Say: "runsnip itself changed: the next call already runs the new one"},
		def.Hint{Match: filepath.Base(s.ExamplePath), Say: "new variables: .env is never overwritten — runsnip env combine, review, then runsnip doctor"})
	return h
}

// InfraPull fast-forwards the checkout and says what each changed file needs.
func (s *Stack) InfraPull(ctx context.Context, out io.Writer) error {
	if !s.FileExists(".git") {
		return fmt.Errorf("%s is not a git checkout: nothing to pull", s.Dir)
	}
	before, err := s.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	before = strings.TrimSpace(before)
	remote, branch := or(s.Def.Infra.Remote, "origin"), s.InfraBranch()
	Say(out, "git pull --ff-only %s %s", remote, branch)
	if err := s.Run.Run(ctx, run.Call{Dir: s.Dir, Env: s.GitEnv, Stdout: out, Stderr: out, Name: "git", Args: []string{"pull", "--ff-only", remote, branch}}); err != nil {
		return fmt.Errorf("pull failed (see above)")
	}
	after, _ := s.git(ctx, "rev-parse", "HEAD")
	after = strings.TrimSpace(after)
	if after == before {
		fmt.Fprintf(out, "  already up to date (%s)\n", short(before))
		return nil
	}
	changed, _ := s.git(ctx, "diff", "--name-only", before, after)
	files := strings.Fields(changed)
	fmt.Fprintf(out, "\nUpdated %s → %s\n", short(before), short(after))
	for _, f := range files {
		fmt.Fprintln(out, "  "+f)
	}
	fmt.Fprintln(out)
	said := false
	for _, h := range s.hints() {
		for _, f := range files {
			if f == h.Match || strings.HasSuffix(h.Match, "/") && strings.HasPrefix(f, h.Match) {
				fmt.Fprintf(out, "  → %-28s %s\n", h.Match+" changed:", h.Say)
				said = true
				break
			}
		}
	}
	if !said {
		fmt.Fprintln(out, "  → nothing to reload")
	}
	// The example changed: name what .env now lacks and what the example dropped.
	exName := filepath.Base(s.ExamplePath)
	for _, f := range files {
		if filepath.Base(f) != exName {
			continue
		}
		old, _ := s.git(ctx, "show", before+":"+f)
		cur, _ := os.ReadFile(s.ExamplePath)
		envText, _ := os.ReadFile(s.EnvPath)
		added, dropped := ExampleChanges(old, string(cur), string(envText))
		if len(added) > 0 {
			fmt.Fprintf(out, "\n  new in %s, needs a value in .env: %s\n", exName, strings.Join(added, " "))
		}
		if len(dropped) > 0 {
			fmt.Fprintf(out, "  no longer in %s (still in .env): %s\n", exName, strings.Join(dropped, " "))
		}
		if len(added)+len(dropped) > 0 {
			fmt.Fprintln(out, "  review: runsnip env combine --diff · apply: runsnip env combine --apply (or the Env tab)")
		}
	}
	return nil
}

var exampleKeyRe = regexp.MustCompile(`(?m)^(#\s?)?([A-Za-z_][A-Za-z0-9_]*)=`)

// exampleKeys lists an example's variables: active ones, and commented `# KEY=` ones (optional).
func exampleKeys(text string) (active, all map[string]bool) {
	active, all = map[string]bool{}, map[string]bool{}
	for _, m := range exampleKeyRe.FindAllStringSubmatch(text, -1) {
		all[m[2]] = true
		if m[1] == "" {
			active[m[2]] = true
		}
	}
	return active, all
}

// ExampleChanges compares an example before and after a pull with .env: the active variables the new example adds
// that .env does not name at all (they need a value), and the variables the old example had, the new one does not,
// and .env still sets.
func ExampleChanges(oldExample, newExample, env string) (added, dropped []string) {
	_, oldAll := exampleKeys(oldExample)
	newActive, newAll := exampleKeys(newExample)
	_, inEnv := exampleKeys(env)
	for _, m := range exampleKeyRe.FindAllStringSubmatch(newExample, -1) {
		k := m[2]
		if newActive[k] && !oldAll[k] && !inEnv[k] && !contains(added, k) {
			added = append(added, k)
		}
	}
	for _, m := range exampleKeyRe.FindAllStringSubmatch(oldExample, -1) {
		k := m[2]
		if !newAll[k] && inEnv[k] && !contains(dropped, k) {
			dropped = append(dropped, k)
		}
	}
	return added, dropped
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
