// Package envmodel is the one model of a stack's .env that `runsnip env`, the TUI's Env tab and the web's Env page
// share: every variable of the example, laid over .env (envform: the example's order, sections and comments, .env's
// values and on/off), with what each one needs to be read and edited well — its help, whether it is a secret, which
// services read it, how its value must look (stack.yaml env_rules.vars), whether runsnip can generate it, and whether
// it is new (the example has it, .env does not) or only in .env.
package envmodel

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/envfile"
	"github.com/runsnip/ops/internal/envform"
	"github.com/runsnip/ops/internal/stack"
)

// Var is one variable.
type Var struct {
	Key      string       `json:"key"`
	Section  string       `json:"section,omitempty"`
	Value    string       `json:"value"`             // .env's (also from a commented line), else the example's for an optional one
	Example  string       `json:"example,omitempty"` // the example's value
	Help     string       `json:"help,omitempty"`    // the comment block above it in the example, else runsnip's own
	Enabled  bool         `json:"enabled"`           // an active line in .env
	InEnv    bool         `json:"inEnv"`             // .env names it, on or off
	New      bool         `json:"new,omitempty"`     // the example has it, .env does not: it needs a value
	Extra    bool         `json:"extra,omitempty"`   // only in .env: the example no longer (or never) names it
	Optional bool         `json:"optional,omitempty"`
	Secret   bool         `json:"secret,omitempty"`
	Services []string     `json:"services,omitempty"` // the services reading it (interpolated, or through env_file)
	Generate string       `json:"generate,omitempty"` // how runsnip can make it (random_hex, hkdf…)
	Rule     *def.VarRule `json:"rule,omitempty"`
	Problem  string       `json:"problem,omitempty"` // what is wrong with the value, by its rule
	Written  string       `json:"written,omitempty"` // "runsnip" for variables runsnip writes itself (pins)
}

// Model is a stack's .env.
type Model struct {
	Vars []Var
	Form *envform.EnvForm
}

// Build reads the stack's example and .env.
func Build(st *stack.Stack) (*Model, error) {
	form, err := envform.Load(st.ExamplePath, st.EnvPath)
	if err != nil {
		return nil, err
	}
	m := &Model{Form: form}
	gens := Generators(st.Def.EnvRules)
	pins := map[string]string{}
	for _, c := range st.Components() {
		pins[c.Image.TagVar] = c.Name
	}
	envName := filepath.Base(st.EnvPath)
	for _, f := range form.Fields {
		v := Var{Key: f.Key, Section: f.Section, Value: f.Value, Example: f.Default, Help: f.Help, Enabled: f.Enabled,
			InEnv: f.Set, New: f.New, Extra: f.Extra, Optional: f.Optional, Secret: st.IsSecret(f.Key) || envform.IsSecret(f.Key)}
		if f.Extra {
			v.Help = ""
		}
		v.Services = st.Compose.ServicesReading(f.Key, envName)
		v.Generate = gens[f.Key]
		if r, ok := st.Def.EnvRules.Vars[f.Key]; ok {
			rr := r
			v.Rule = &rr
			if v.Help == "" {
				v.Help = r.Help
			}
		}
		if h, ok := builtinHelp(f.Key, pins); ok {
			v.Written = "runsnip"
			if v.Help == "" {
				v.Help = h
			}
		}
		if v.Enabled {
			if err := Check(v.Rule, v.Value); err != nil {
				v.Problem = err.Error()
			}
		}
		m.Vars = append(m.Vars, v)
	}
	return m, nil
}

// Find returns key's variable.
func (m *Model) Find(key string) (Var, bool) {
	for _, v := range m.Vars {
		if v.Key == key {
			return v, true
		}
	}
	return Var{}, false
}

// Generators names, per key, how env_rules.generate can make it.
func Generators(r def.EnvRules) map[string]string {
	out := map[string]string{}
	for _, g := range r.Generate {
		switch g.Type {
		case "ed25519_pair":
			ps, pp := or(g.PrivateSuffix, "_JWT_KEY"), or(g.PublicSuffix, "_PUBLIC_KEY")
			for _, k := range g.Keys {
				out[k+ps], out[k+pp] = g.Type, g.Type
			}
		case "ed25519_kid_pair":
			out[g.Private], out[g.Public] = g.Type, g.Type
		default:
			out[g.Key] = g.Type
		}
	}
	return out
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// builtinHelp explains the variables runsnip writes itself: the pins and the version before them.
func builtinHelp(key string, pins map[string]string) (string, bool) {
	if c, ok := pins[strings.TrimSuffix(key, "_PREVIOUS")]; ok && strings.HasSuffix(key, "_TAG_PREVIOUS") {
		return "The version " + c + " ran before its last pin: `runsnip rollback " + c + "` goes back to it. Written by runsnip.", true
	}
	if c, ok := pins[key]; ok {
		return "Pinned version of " + c + " (`runsnip pin " + c + " <version>`, the Versions tab). Written by runsnip.", true
	}
	return "", false
}

// Check validates a value against its rule (nil rule: anything goes).
func Check(r *def.VarRule, v string) error {
	if r == nil {
		return nil
	}
	if v == "" {
		if r.Required {
			return fmt.Errorf("a value is required")
		}
		return nil
	}
	if r.Min > 0 && len(v) < r.Min {
		return fmt.Errorf("at least %d characters (has %d)", r.Min, len(v))
	}
	if len(r.Enum) > 0 {
		ok := false
		for _, e := range r.Enum {
			ok = ok || e == v
		}
		if !ok {
			return fmt.Errorf("one of: %s", strings.Join(r.Enum, ", "))
		}
	}
	if r.Pattern != "" {
		re, err := regexp.Compile("^(?:" + r.Pattern + ")$")
		if err != nil {
			return fmt.Errorf("bad pattern in stack.yaml: %v", err)
		}
		if !re.MatchString(v) {
			return fmt.Errorf("does not match %s", r.Pattern)
		}
	}
	switch r.Type {
	case "", "string":
	case "url":
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("not a URL (scheme://host…)")
		}
	case "email":
		if a, err := mail.ParseAddress(v); err != nil || a.Address != v {
			return fmt.Errorf("not an email address")
		}
	case "domain":
		if !domainRe.MatchString(v) {
			return fmt.Errorf("not a domain name")
		}
	case "port":
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("not a port (1–65535)")
		}
	case "int":
		if _, err := strconv.Atoi(v); err != nil {
			return fmt.Errorf("not a whole number")
		}
	case "bool":
		switch strings.ToLower(v) {
		case "true", "false", "1", "0", "yes", "no", "on", "off":
		default:
			return fmt.Errorf("not true/false")
		}
	case "hex":
		b, err := hex.DecodeString(v)
		if err != nil {
			return fmt.Errorf("not hex")
		}
		if r.Bytes > 0 && len(b) < r.Bytes {
			return fmt.Errorf("%d bytes of hex, needs %d (openssl rand -hex %d)", len(b), r.Bytes, r.Bytes)
		}
	case "base64":
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			if b, err = base64.RawURLEncoding.DecodeString(v); err != nil {
				return fmt.Errorf("not base64")
			}
		}
		if r.Bytes > 0 && len(b) < r.Bytes {
			return fmt.Errorf("%d bytes of base64, needs %d (openssl rand -base64 %d)", len(b), r.Bytes, r.Bytes)
		}
	case "duration":
		if _, err := time.ParseDuration(v); err != nil {
			return fmt.Errorf("not a duration (30s, 5m, 2h)")
		}
	case "cidrs":
		for _, c := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			if _, _, err := net.ParseCIDR(c); err != nil && net.ParseIP(c) == nil {
				return fmt.Errorf("%q is not an address or CIDR range", c)
			}
		}
	default:
		return fmt.Errorf("unknown type %q in stack.yaml", r.Type)
	}
	return nil
}

var domainRe = regexp.MustCompile(`^(?i)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// Mask hides a secret's value: its length only.
func Mask(v string) string {
	if v == "" {
		return ""
	}
	return fmt.Sprintf("•••••••• (%d chars)", len(v))
}

// Filter keeps the variables a filter names: new, missing (on and empty), secrets, extra, problems,
// or a search text (key, help, section).
func (m *Model) Filter(filter, search string) []Var {
	var out []Var
	search = strings.ToLower(search)
	for _, v := range m.Vars {
		switch filter {
		case "new":
			if !v.New {
				continue
			}
		case "missing":
			if !(v.Enabled && v.Value == "" && !v.Optional && !v.Extra) {
				continue
			}
		case "secrets":
			if !v.Secret {
				continue
			}
		case "extra":
			if !v.Extra {
				continue
			}
		case "problems":
			if v.Problem == "" {
				continue
			}
		}
		if search != "" && !strings.Contains(strings.ToLower(v.Key+" "+v.Help+" "+v.Section), search) {
			continue
		}
		out = append(out, v)
	}
	return out
}

// Change is an edit of .env: values to set (each turned on), keys to turn on and off.
type Change struct {
	Set   map[string]string
	Order []string // Set's keys in the order given
	On    []string
	Off   []string
}

// Result is what Apply did: the keys changed (never their values), the old file kept, the services reading them.
type Result struct {
	Keys     []string
	Backup   string
	Services []string
}

// Apply validates a change, writes .env (atomic, mode kept, the old one kept as .env.bak-<time>, refused when .env
// changed since it was read) and says which services read what changed.
func Apply(st *stack.Stack, c Change) (Result, error) {
	var res Result
	if _, err := os.Stat(st.EnvPath); errors.Is(err, os.ErrNotExist) {
		if err := envfile.WriteAtomic(st.EnvPath, nil); err != nil {
			return res, err
		}
	}
	m, err := Build(st)
	if err != nil {
		return res, err
	}
	var bad []string
	for _, k := range c.Order {
		v := c.Set[k]
		switch {
		case !keyOK(k):
			bad = append(bad, k+": not a variable name")
		case strings.ContainsAny(v, "\n\r"):
			bad = append(bad, k+": a value cannot hold a line break")
		default:
			rule := st.Def.EnvRules.Vars[k]
			if err := Check(&rule, v); err != nil {
				bad = append(bad, k+": "+err.Error())
			}
		}
	}
	if len(bad) > 0 {
		return res, fmt.Errorf("nothing written:\n  %s", strings.Join(bad, "\n  "))
	}
	for _, k := range c.Order {
		m.Form.Set(k, c.Set[k])
	}
	for _, k := range c.On {
		if !m.Form.Toggle(k, true) {
			return res, fmt.Errorf("%s is not in .env or the example", k)
		}
	}
	for _, k := range c.Off {
		if !m.Form.Toggle(k, false) {
			return res, fmt.Errorf("%s is not in .env or the example", k)
		}
	}
	if res.Backup, err = m.Form.Save(st.EnvPath, time.Now()); err != nil {
		return res, err
	}
	res.Keys = append(append(append([]string{}, c.Order...), c.On...), c.Off...)
	seen := map[string]bool{}
	for _, k := range res.Keys {
		for _, s := range st.Compose.ServicesReading(k, filepath.Base(st.EnvPath)) {
			if !seen[s] {
				seen[s] = true
				res.Services = append(res.Services, s)
			}
		}
	}
	return res, nil
}

func keyOK(k string) bool {
	if k == "" || k[0] >= '0' && k[0] <= '9' {
		return false
	}
	for _, c := range k {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
