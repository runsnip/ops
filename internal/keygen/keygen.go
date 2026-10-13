// Package keygen is `runsnip env generate`: it fills every value the stack definition can generate and .env still
// lacks — random secrets, Ed25519 key pairs, keys derived from another secret — and never overwrites a value that is
// there. Everything is computed here (Go's crypto), so the server needs no Node or OpenSSL for it.
package keygen

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/envfile"
)

// Line is one generated variable.
type Line struct {
	Key, Value string
	// Pair names the other half: both are filled together or not at all.
	Pair string
	// Derived values must equal what is computed; a different one in .env is reported, not kept silently.
	Derived bool
	// Skip says why no value could be computed (a derived value whose source is empty).
	Skip string
}

// Rand is the randomness source (tests replace it).
var Rand io.Reader = rand.Reader

// Now is the clock for key ids (tests replace it).
var Now = time.Now

// Derive is HKDF-SHA256 with an empty salt, base64url without padding: what Node's
// crypto.hkdfSync("sha256", secret, "", info, n) gives, the derivation the services use.
func Derive(secret, info string, n int) (string, error) {
	if n == 0 {
		n = 32
	}
	k, err := hkdf.Key(sha256.New, []byte(secret), nil, info, n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(k), nil
}

// Ed25519 returns a key pair as the services read it: private "<d>.<x>" (the JWK's d and x), public "<x>".
func Ed25519() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(Rand)
	if err != nil {
		return "", "", err
	}
	x := base64.RawURLEncoding.EncodeToString(pub)
	d := base64.RawURLEncoding.EncodeToString(priv.Seed())
	return d + "." + x, x, nil
}

// PublicOf returns the public half of a private "<d>.<x>" key (what follows the dot).
func PublicOf(private string) string {
	_, x, _ := strings.Cut(private, ".")
	return x
}

func random(n int) ([]byte, error) {
	if n == 0 {
		n = 32
	}
	b := make([]byte, n)
	_, err := io.ReadFull(Rand, b)
	return b, err
}

// Lines computes every generator's values; vals is .env as it is (hkdf reads its input from it). A source that is
// empty in .env but generated earlier in the same list is derived from that new value (on a fresh server a root secret
// and what derives from it come out of one run); a source empty in both is skipped with its reason, not an error, so
// every other value is still generated.
func Lines(gens []def.Generator, vals map[string]string) ([]Line, error) {
	var out []Line
	next := map[string]string{}
	for k, v := range vals {
		next[k] = v
	}
	for _, g := range gens {
		n0 := len(out)
		switch g.Type {
		case "random_hex":
			b, err := random(g.Bytes)
			if err != nil {
				return nil, err
			}
			out = append(out, Line{Key: g.Key, Value: hex.EncodeToString(b)})
		case "random_base64":
			b, err := random(g.Bytes)
			if err != nil {
				return nil, err
			}
			out = append(out, Line{Key: g.Key, Value: base64.StdEncoding.EncodeToString(b)})
		case "literal":
			out = append(out, Line{Key: g.Key, Value: g.Value})
		case "hkdf":
			if next[g.From] == "" {
				out = append(out, Line{Key: g.Key, Derived: true, Skip: fmt.Sprintf("%s is empty: %s is derived from it — set %s, then generate again", g.From, g.Key, g.From)})
				continue
			}
			v, err := Derive(next[g.From], g.Info, g.Bytes)
			if err != nil {
				return nil, err
			}
			out = append(out, Line{Key: g.Key, Value: v, Derived: true})
		case "ed25519_pair":
			ps, pp := or(g.PrivateSuffix, "_JWT_KEY"), or(g.PublicSuffix, "_PUBLIC_KEY")
			for _, p := range g.Keys {
				priv, pub, err := Ed25519()
				if err != nil {
					return nil, err
				}
				out = append(out, Line{Key: p + ps, Value: priv, Pair: p + pp}, Line{Key: p + pp, Value: pub, Pair: p + ps})
			}
		case "ed25519_kid_pair":
			priv, pub, err := Ed25519()
			if err != nil {
				return nil, err
			}
			kid := Now().UTC().Format("2006-01-02")
			out = append(out, Line{Key: g.Private, Value: kid + ":" + priv, Pair: g.Public}, Line{Key: g.Public, Value: kid + ":" + pub, Pair: g.Private})
		default:
			return nil, fmt.Errorf("unknown generator type %q (random_hex, random_base64, literal, hkdf, ed25519_pair, ed25519_kid_pair)", g.Type)
		}
		// What this run will write is what later generators derive from (only where .env has nothing).
		for _, l := range out[n0:] {
			if l.Skip == "" && next[l.Key] == "" {
				next[l.Key] = l.Value
			}
		}
	}
	return out, nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// Result is what Fill did.
type Result struct {
	Filled, Kept []string
	Warnings     []string
}

// Options narrow Fill: Only the keys named (a pair is generated whole), and Force to replace values already there.
type Options struct {
	Only  map[string]bool
	Force bool
}

// Fill writes into env every generated value it lacks (placeholder counts as empty) and saves it with mode 600.
func Fill(env *envfile.File, rules def.EnvRules, out io.Writer) (Result, error) {
	return FillWith(env, rules, Options{}, out)
}

// FillWith is Fill with options.
func FillWith(env *envfile.File, rules def.EnvRules, opt Options, out io.Writer) (Result, error) {
	var res Result
	cur := func(k string) string {
		v := env.Get(k)
		if rules.Placeholder != "" && v == rules.Placeholder {
			return ""
		}
		return v
	}
	vals := env.Values()
	for k := range vals {
		vals[k] = cur(k)
	}
	lines, err := Lines(rules.Generate, vals)
	if err != nil {
		return res, err
	}
	before := map[string]string{}
	for _, l := range lines {
		before[l.Key] = cur(l.Key)
	}
	for _, l := range lines {
		if opt.Only != nil && !opt.Only[l.Key] && !(l.Pair != "" && opt.Only[l.Pair]) {
			continue
		}
		if l.Skip != "" {
			if before[l.Key] == "" {
				res.Warnings = append(res.Warnings, l.Skip)
			}
			continue
		}
		// A pair is filled only when both halves were empty: a private key of this run is never matched with a
		// public key of another. Force replaces both halves together.
		if !opt.Force && (before[l.Key] != "" || l.Pair != "" && before[l.Pair] != "") {
			res.Kept = append(res.Kept, l.Key)
			continue
		}
		env.SetLine(l.Key, l.Value)
		res.Filled = append(res.Filled, l.Key)
		fmt.Fprintln(out, "  + "+l.Key)
	}
	if len(res.Filled) > 0 {
		if err := env.Save(); err != nil {
			return res, err
		}
	}
	for _, l := range lines {
		if l.Derived && l.Skip == "" && cur(l.Key) != l.Value {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s in .env is NOT the value derived from the secret it comes from — set it to:\n    %s=%s", l.Key, l.Key, l.Value))
		}
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if l.Pair == "" || seen[l.Key] || seen[l.Pair] {
			continue
		}
		seen[l.Key], seen[l.Pair] = true, true
		if (cur(l.Key) == "") != (cur(l.Pair) == "") {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s / %s: only one half is set — delete both lines and run this again", l.Key, l.Pair))
		}
	}
	for _, m := range rules.Manual {
		var missing []string
		for _, k := range m.Keys {
			if cur(k) == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			w := m.Title + " — fill in by hand: " + strings.Join(missing, " ")
			if m.Hint != "" {
				w += "\n    " + m.Hint
			}
			res.Warnings = append(res.Warnings, w)
		}
	}
	return res, nil
}
