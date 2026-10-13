package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/envfile"
	"github.com/runsnip/ops/internal/envform"
	"github.com/runsnip/ops/internal/envmodel"
	"github.com/runsnip/ops/internal/keygen"
	"github.com/runsnip/ops/internal/stack"
)

const envUsage = `runsnip env — the stack's .env, laid over its example (the example's order and comments, .env's values)

  runsnip env list [--filter new|missing|secrets|extra|problems] [--search text] [--reveal] [--json]
  runsnip env combine                  write .env.combined for review (.env untouched) — the default
  runsnip env combine --diff           show exactly what would change in .env
  runsnip env combine --apply          write it into .env (atomic; the old one kept as .env.bak-<time>)
  runsnip env set KEY=VALUE… [--off KEY] [--on KEY] [--restart]
  runsnip env set --stdin [--restart]  the same, read from stdin as KEY=VALUE lines or a JSON object
                                       (values never on the command line, in ps or in job logs)
  runsnip env generate [KEY…] [--force]  fill generated values .env lacks (all, or the keys named; --force replaces)

Every write validates against stack.yaml's env_rules.vars, refuses to overwrite a .env changed since it was read,
names the keys it changed (never their values) and the services that read them; --restart recreates those.`

func envCmd(ctx context.Context, st *stack.Stack, args []string, out io.Writer) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list":
		return envList(st, args, out)
	case "combine":
		return envCombine(st, args, out)
	case "set":
		return envSet(ctx, st, args, os.Stdin, out)
	case "generate", "services":
		return envGenerate(st, args, out)
	case "", "help", "-h", "--help":
		fmt.Fprintln(out, envUsage)
		return nil
	}
	return errors.New(envUsage)
}

func envList(st *stack.Stack, args []string, out io.Writer) error {
	filter, search, reveal, asJSON := "", "", false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--filter":
			if i+1 < len(args) {
				filter, i = args[i+1], i+1
			}
		case "--search":
			if i+1 < len(args) {
				search, i = args[i+1], i+1
			}
		case "--reveal":
			reveal = true
		case "--json":
			asJSON = true
		}
	}
	m, err := envmodel.Build(st)
	if err != nil {
		return err
	}
	vars := m.Filter(filter, search)
	if !reveal {
		for i := range vars {
			if vars[i].Secret {
				vars[i].Value = envmodel.Mask(vars[i].Value)
			}
		}
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(vars)
	}
	for _, v := range vars {
		state := "on "
		switch {
		case v.New:
			state = "NEW"
		case v.Extra:
			state = "XTR"
		case !v.Enabled:
			state = "off"
		}
		line := fmt.Sprintf("%s  %-32s %s", state, v.Key, v.Value)
		if v.Problem != "" {
			line += "   ✗ " + v.Problem
		}
		if len(v.Services) > 0 {
			line += "   ← " + strings.Join(v.Services, " ")
		}
		fmt.Fprintln(out, line)
	}
	return nil
}

func envCombine(st *stack.Stack, args []string, out io.Writer) error {
	mode := ""
	for _, a := range args {
		switch a {
		case "--diff", "--apply":
			mode = a
		default:
			return fmt.Errorf("unknown option %s (--diff or --apply)", a)
		}
	}
	if _, err := os.Stat(st.EnvPath); err != nil {
		return fmt.Errorf("no .env yet — the first time: cp %s .env && $EDITOR .env (or the Env tab)", filepath.Base(st.ExamplePath))
	}
	form, err := envform.Load(st.ExamplePath, st.EnvPath)
	if err != nil {
		return err
	}
	text, err := form.Render()
	if err != nil {
		return err
	}
	c := form.Combined()
	switch mode {
	case "--diff":
		d := envform.Diff(form.Current(), text)
		if d == "" {
			fmt.Fprintln(out, ".env already follows the example: nothing would change.")
			return nil
		}
		fmt.Fprint(out, maskDiff(st, d))
		fmt.Fprintf(out, "\n%d new left empty, %d opened, %d only in .env (moved to the end). runsnip env combine --apply writes it.\n", len(c.New), c.Opened, len(c.Extra))
		return nil
	case "--apply":
		if envform.Diff(form.Current(), text) == "" {
			fmt.Fprintln(out, ".env already follows the example: nothing written.")
			return nil
		}
		backup, err := form.Save(st.EnvPath, time.Now())
		if err != nil {
			return err
		}
		stack.Say(out, "wrote %s (atomic; the old one: %s)", st.EnvPath, backup)
		if len(c.New) > 0 {
			fmt.Fprintf(out, "  new, need a value: %s\n", strings.Join(c.New, " "))
		}
		return nil
	}
	target := st.EnvPath + ".combined"
	if err := envfile.WriteAtomic(target, []byte(text)); err != nil {
		return err
	}
	stack.Say(out, "wrote %s (mode 600; .env untouched)", target)
	fmt.Fprintf(out, "  values kept from .env:          %d\n", c.Kept)
	fmt.Fprintf(out, "  opened because .env sets them:  %d\n", c.Opened)
	if c.Off > 0 {
		fmt.Fprintf(out, "  kept off (# in .env):           %d\n", c.Off)
	}
	fmt.Fprintf(out, "  left empty — .env lacks them:   %d %s\n", len(c.New), strings.Join(c.New, " "))
	if len(c.Extra) > 0 {
		fmt.Fprintf(out, "  only in .env, moved to the end: %d %s\n", len(c.Extra), strings.Join(c.Extra, " "))
	}
	fmt.Fprintf(out, "\n  Review:   runsnip env combine --diff\n  Then:     runsnip env combine --apply && runsnip doctor\n")
	return nil
}

// maskDiff hides secret values in a diff: the key and the length stay.
func maskDiff(st *stack.Stack, d string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
		if len(l) > 1 && (l[0] == '+' || l[0] == '-' || l[0] == ' ') {
			body := strings.TrimPrefix(strings.TrimPrefix(l[1:], "# "), "export ")
			if k, v, ok := strings.Cut(body, "="); ok && v != "" && (st.IsSecret(strings.TrimSpace(k)) || envform.IsSecret(strings.TrimSpace(k))) {
				i := strings.Index(l, "=")
				l = l[:i+1] + envmodel.Mask(envfile.Value(v))
			}
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// readAssignments reads KEY=VALUE lines or a JSON object.
func readAssignments(r io.Reader) (map[string]string, []string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	t := strings.TrimSpace(string(b))
	vals := map[string]string{}
	var order []string
	if strings.HasPrefix(t, "{") {
		if err := json.Unmarshal([]byte(t), &vals); err != nil {
			return nil, nil, err
		}
		for k := range vals {
			order = append(order, k)
		}
		sort.Strings(order)
		return vals, order, nil
	}
	sc := bufio.NewScanner(strings.NewReader(t))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok && strings.TrimSpace(k) != "" {
			k = strings.TrimSpace(k)
			if _, seen := vals[k]; !seen {
				order = append(order, k)
			}
			vals[k] = v
		}
	}
	return vals, order, nil
}

func envSet(ctx context.Context, st *stack.Stack, args []string, stdin io.Reader, out io.Writer) error {
	vals := map[string]string{}
	var order, on, off []string
	restart, fromStdin := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--restart":
			restart = true
		case a == "--stdin":
			fromStdin = true
		case (a == "--on" || a == "--off") && i+1 < len(args):
			if a == "--on" {
				on = append(on, args[i+1])
			} else {
				off = append(off, args[i+1])
			}
			i++
		case strings.Contains(a, "="):
			k, v, _ := strings.Cut(a, "=")
			if _, seen := vals[k]; !seen {
				order = append(order, k)
			}
			vals[k] = v
		default:
			return fmt.Errorf("%q: KEY=VALUE, --on KEY, --off KEY, --stdin or --restart", a)
		}
	}
	if fromStdin {
		v, o, err := readAssignments(stdin)
		if err != nil {
			return err
		}
		for _, k := range o {
			if _, seen := vals[k]; !seen {
				order = append(order, k)
			}
			vals[k] = v[k]
		}
	}
	if len(order)+len(on)+len(off) == 0 {
		return errors.New("nothing to set: runsnip env set KEY=VALUE…")
	}
	res, err := envmodel.Apply(st, envmodel.Change{Set: vals, Order: order, On: on, Off: off})
	if err != nil {
		return err
	}
	stack.Say(out, "saved %s: %s (the old one: %s)", filepath.Base(st.EnvPath), strings.Join(res.Keys, " "), filepath.Base(res.Backup))
	svcs := res.Services
	if len(svcs) == 0 {
		fmt.Fprintln(out, "  no service reads them through compose")
		return nil
	}
	if !restart {
		fmt.Fprintf(out, "  read by: %s — they apply once recreated: runsnip env set … --restart, or runsnip restart <svc> --no-pull\n", strings.Join(svcs, " "))
		return nil
	}
	vals2 := st.Values()
	for _, s := range svcs {
		if ok, _ := st.Enabled(s, vals2); !ok {
			continue
		}
		if err := st.Restart(ctx, out, s, true); err != nil {
			return err
		}
	}
	return nil
}

func envGenerate(st *stack.Stack, args []string, out io.Writer) error {
	opt := keygen.Options{}
	for _, a := range args {
		if a == "--force" {
			opt.Force = true
			continue
		}
		if opt.Only == nil {
			opt.Only = map[string]bool{}
		}
		opt.Only[a] = true
	}
	if opt.Force && opt.Only == nil {
		return errors.New("--force replaces values: name the keys (runsnip env generate KEY… --force)")
	}
	env, err := st.Env()
	if err != nil {
		if !stack.IsMissingEnv(err) {
			return err
		}
		if err := envfile.WriteAtomic(st.EnvPath, nil); err != nil {
			return err
		}
		if env, err = st.Env(); err != nil {
			return err
		}
	}
	res, err := keygen.FillWith(env, st.Def.EnvRules, opt, out)
	if err != nil {
		return err
	}
	stack.Say(out, "filled %d value(s), kept %d already set", len(res.Filled), len(res.Kept))
	for _, w := range res.Warnings {
		fmt.Fprintln(out, color("33", "⚠")+"  "+w)
	}
	fmt.Fprintln(out, "  then: runsnip doctor")
	return nil
}
