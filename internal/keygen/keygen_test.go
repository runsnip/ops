package keygen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/envfile"
)

// The vectors are Node's (the services' runtime): crypto.hkdfSync("sha256", "test-secret", "", info, 32) as
// base64url, and the JWK of the Ed25519 key whose seed is 32 bytes of 7.
func TestDeriveMatchesNode(t *testing.T) {
	for info, want := range map[string]string{
		"runsnip/billing/custom-data": "k1ugjGjUKUC4mJUYcuyxhW5leqI4PsG4PdghXnhQlyY",
		"runsnip/domains/txt":         "8HTby0yzuJdZS9s9zTyLvyunH75MnGeL77-vmTbYOQk",
	} {
		if got, _ := Derive("test-secret", info, 32); got != want {
			t.Errorf("%s: %s, want %s", info, got, want)
		}
	}
}

func TestEd25519MatchesNodesJWK(t *testing.T) {
	Rand = bytes.NewReader(bytes.Repeat([]byte{7}, 32))
	defer func() { Rand = nil }()
	priv, pub, err := Ed25519()
	if err != nil {
		t.Fatal(err)
	}
	if priv != "BwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwc.6kpsY-KcUgq-9VB7Ey7F-ZVHdq6-vnuSQh7qaRRG0iw" || pub != PublicOf(priv) {
		t.Fatalf("%s %s", priv, pub)
	}
}

func rules() def.EnvRules {
	return def.EnvRules{
		Placeholder: "CHANGE_ME",
		Generate: []def.Generator{
			{Type: "random_hex", Key: "FILE_SECRET", Bytes: 32},
			{Type: "hkdf", Key: "BILLING_KEY", From: "JWT_SECRET", Info: "runsnip/billing/custom-data"},
			{Type: "literal", Key: "PLAN_URL", Value: "http://plan-api:4040"},
			{Type: "ed25519_pair", Keys: []string{"SERVICE_AUTH", "SERVICE_PLAN"}},
			{Type: "ed25519_kid_pair", Private: "SIGNING_KEYS", Public: "PUBLIC_KEYS"},
		},
		Manual: []def.Manual{{Keys: []string{"PAYMENT_API_KEY"}, Title: "Payments", Hint: "Developer tools → Authentication"}},
	}
}

func TestFillNeverOverwritesAndFillsPairsTogether(t *testing.T) {
	Rand = randReader{}
	Now = func() time.Time { return time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) }
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	os.WriteFile(p, []byte("# keep me\nJWT_SECRET=test-secret\nFILE_SECRET=\nPLAN_URL=CHANGE_ME\nSERVICE_PLAN_JWT_KEY=half\nBILLING_KEY=wrong\n"), 0o644)
	env, _ := envfile.Load(p)
	var out bytes.Buffer
	res, err := Fill(env, rules(), &out)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := envfile.Load(p)
	if v := got.Get("FILE_SECRET"); len(v) != 64 {
		t.Fatalf("an empty line is filled in place: %q", v)
	}
	if got.Get("PLAN_URL") != "http://plan-api:4040" {
		t.Fatal("a placeholder counts as empty")
	}
	if got.Get("BILLING_KEY") != "wrong" {
		t.Fatal("an existing value is never overwritten")
	}
	if a := got.Get("SERVICE_AUTH_JWT_KEY"); PublicOf(a) != got.Get("SERVICE_AUTH_PUBLIC_KEY") || a == "" {
		t.Fatal("a pair is generated together")
	}
	if got.Get("SERVICE_PLAN_PUBLIC_KEY") != "" {
		t.Fatal("half a pair is not completed with a key from another run")
	}
	if !strings.HasPrefix(got.Get("SIGNING_KEYS"), "2026-10-12:") || !strings.HasSuffix(got.Get("SIGNING_KEYS"), "."+strings.TrimPrefix(got.Get("PUBLIC_KEYS"), "2026-10-12:")) {
		t.Fatalf("kid pair: %s / %s", got.Get("SIGNING_KEYS"), got.Get("PUBLIC_KEYS"))
	}
	w := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"BILLING_KEY in .env is NOT", "BILLING_KEY=k1ugjGjUKUC4mJUYcuyxhW5leqI4PsG4PdghXnhQlyY", "SERVICE_PLAN_JWT_KEY / SERVICE_PLAN_PUBLIC_KEY: only one half", "Payments — fill in by hand: PAYMENT_API_KEY"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q missing in:\n%s", want, w)
		}
	}
	if b, _ := os.ReadFile(p); !strings.HasPrefix(string(b), "# keep me\n") {
		t.Fatal("comments stay")
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}

	// Run again: nothing left to fill.
	env, _ = envfile.Load(p)
	res, _ = Fill(env, rules(), &out)
	if len(res.Filled) != 0 {
		t.Fatalf("second run filled %v", res.Filled)
	}
}

type randReader struct{}

func (randReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i*31 + 1)
	}
	return len(p), nil
}

// A derived value follows a source generated in the same run; with no source at all it is skipped, not fatal.
func TestDerivedFollowsASourceGeneratedInTheSameRun(t *testing.T) {
	gens := []def.Generator{{Type: "random_hex", Key: "JWT_SECRET", Bytes: 32}, {Type: "hkdf", Key: "DERIVED", From: "JWT_SECRET", Info: "x"}, {Type: "hkdf", Key: "ORPHAN", From: "NOPE", Info: "x"}, {Type: "random_hex", Key: "OTHER", Bytes: 8}}
	lines, err := Lines(gens, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Line{}
	for _, l := range lines {
		got[l.Key] = l
	}
	want, _ := Derive(got["JWT_SECRET"].Value, "x", 0)
	if got["DERIVED"].Value != want || got["DERIVED"].Skip != "" {
		t.Fatalf("derived from this run's JWT_SECRET: %+v", got["DERIVED"])
	}
	if got["ORPHAN"].Skip == "" || got["OTHER"].Value == "" {
		t.Fatalf("an empty source skips only its own key: %+v %+v", got["ORPHAN"], got["OTHER"])
	}
	// .env's own source wins over a generated one.
	lines, _ = Lines(gens, map[string]string{"JWT_SECRET": "kept"})
	want, _ = Derive("kept", "x", 0)
	for _, l := range lines {
		if l.Key == "DERIVED" && l.Value != want {
			t.Fatal("derived from .env's JWT_SECRET")
		}
	}
}
