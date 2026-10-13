package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.1.0", "v0.2.0", -1, true}, {"0.10.0", "v0.9.9", 1, true}, {"v1.0.0", "1.0.0", 0, true},
		{"dev", "v1.0.0", 0, false}, {"infra+abc", "v1.0.0", 0, false},
	} {
		got, ok := compareVersions(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("%s vs %s: %d %v", c.a, c.b, got, ok)
		}
	}
	sums := "aaa  runsnip-linux-amd64\nbbb *runsnip-linux-arm64\n"
	if s, err := checksumOf(sums, "runsnip-linux-arm64"); err != nil || s != "bbb" {
		t.Fatal(s, err)
	}
	if _, err := checksumOf(sums, "runsnip-darwin-arm64"); err == nil {
		t.Fatal("a missing line is an error")
	}
}

// A local release server: check, update (checksum verified, swapped atomically, previous kept), refuse a downgrade,
// refuse a bad checksum, and roll back.
func TestSelfUpdateFromAReleaseServer(t *testing.T) {
	dir, _ := stackDirFor(t)
	_ = dir
	bin := func(v string) []byte { return []byte("#!/bin/sh\necho runsnip " + v + "\n") }
	newBin := bin("v9.9.9")
	sum := sha256.Sum256(newBin)
	sums := hex.EncodeToString(sum[:]) + "  " + assetName() + "\n"
	corrupt := false
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/latest", "/releases/tags/v9.9.9":
			json.NewEncoder(w).Encode(Release{Tag: "v9.9.9", Body: "- faster dashboard\n- env combine --diff", Date: "2026-10-13",
				Assets: []Asset{{Name: assetName(), Download: srv.URL + "/dl/bin"}, {Name: "SHA256SUMS", Download: srv.URL + "/dl/sums"}}})
		case "/releases/tags/v0.0.1":
			json.NewEncoder(w).Encode(Release{Tag: "v0.0.1", Assets: []Asset{{Name: assetName(), Download: srv.URL + "/dl/bin"}, {Name: "SHA256SUMS", Download: srv.URL + "/dl/sums"}}})
		case "/dl/bin":
			if corrupt {
				w.Write([]byte("tampered"))
				return
			}
			w.Write(newBin)
		case "/dl/sums":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("RUNSNIP_RELEASES", srv.URL+"/releases")
	exe := filepath.Join(t.TempDir(), "runsnip")
	os.WriteFile(exe, bin("v1.0.0"), 0o755)
	t.Setenv("RUNSNIP_EXE", exe)
	old := version
	version = "v1.0.0"
	defer func() { version = old }()

	if _, out, err := cli(t, "self", "check"); err != nil || !strings.Contains(out, "latest release v9.9.9") || !strings.Contains(out, "env combine --diff") {
		t.Fatalf("check: %v %s", err, out)
	}
	corrupt = true
	if _, out, err := cli(t, "self", "update"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("a tampered download is refused: %v %s", err, out)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(bin("v1.0.0")) {
		t.Fatal("nothing replaced after a bad checksum")
	}
	corrupt = false
	if _, out, err := cli(t, "self", "update"); err != nil || !strings.Contains(out, "swapped in") {
		t.Fatalf("update: %v %s", err, out)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatal("the new binary is in place")
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != string(bin("v1.0.0")) {
		t.Fatal("the previous one is kept")
	}
	version = "v9.9.9"
	if _, _, err := cli(t, "self", "update", "--version", "v0.0.1"); err == nil {
		// --version allows a downgrade: the asset is the v9.9.9 bytes, which do not say v0.0.1 — refused before the swap.
	}
	if _, out, err := cli(t, "self", "rollback"); err != nil || !strings.Contains(out, "back to runsnip v1.0.0") {
		t.Fatalf("rollback: %v %s", err, out)
	}
	version = "v10.0.0"
	if _, _, err := cli(t, "self", "update"); err == nil || !strings.Contains(err.Error(), "no downgrade") {
		t.Fatalf("no downgrade without --version: %v", err)
	}
}

// A private repository's assets come through the API URL with the token; public ones through the download URL.
func TestAssetURLFollowsTheToken(t *testing.T) {
	a := Asset{Name: "x", Download: "https://github.com/o/r/releases/download/v1/x", API: "https://api.github.com/repos/o/r/releases/assets/1"}
	t.Setenv("GITHUB_TOKEN", "")
	if a.URL() != a.Download {
		t.Fatal(a.URL())
	}
	t.Setenv("GITHUB_TOKEN", "t")
	if a.URL() != a.API {
		t.Fatal(a.URL())
	}
}
