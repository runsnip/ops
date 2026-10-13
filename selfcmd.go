package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/runsnip/ops/internal/host"
	"github.com/runsnip/ops/internal/jobs"
)

const selfUsage = `runsnip self — runsnip itself

  runsnip self check [--json]              this version, the latest release and its changes
  runsnip self update [--version vX.Y.Z]   download, verify the checksum, swap in atomically (the old one kept as
                                           runsnip.previous), restart runsnip serve, roll back if it does not answer
  runsnip self rollback                    back to runsnip.previous
  runsnip self health                      whether runsnip serve answers /healthz (and its version)

Releases: RUNSNIP_RELEASES (default https://api.github.com/repos/runsnip/ops/releases; GITHUB_TOKEN when private).`

// Release is a GitHub release, as the API returns it.
type Release struct {
	Tag    string  `json:"tag_name"`
	Name   string  `json:"name"`
	Body   string  `json:"body"`
	Date   string  `json:"published_at"`
	Assets []Asset `json:"assets"`
}

// Asset is one file of a release.
type Asset struct {
	Name     string `json:"name"`
	Download string `json:"browser_download_url"` // public releases
	API      string `json:"url"`                  // the API's own URL: with GITHUB_TOKEN, the one a private repository serves
}

// URL is where the asset downloads from: the API URL when a token is set (a private repository), else the public one.
func (a Asset) URL() string {
	if os.Getenv("GITHUB_TOKEN") != "" && a.API != "" || a.Download == "" {
		return a.API
	}
	return a.Download
}

func releasesURL() string {
	if u := os.Getenv("RUNSNIP_RELEASES"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	return "https://api.github.com/repos/runsnip/ops/releases"
}

func httpGet(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" && strings.HasPrefix(url, "https://api.github.com/") {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if strings.Contains(url, "/releases/assets/") {
		req.Header.Set("Accept", "application/octet-stream")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 && strings.Contains(url, "/releases") {
		return fmt.Errorf("GET %s: 404 — no release published there yet, or the repository is private (set GITHUB_TOKEN, or RUNSNIP_RELEASES to another release API)", url)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 200<<20))
	return err
}

func fetchRelease(ctx context.Context, tag string) (Release, error) {
	url := releasesURL() + "/latest"
	if tag != "" {
		url = releasesURL() + "/tags/" + tag
	}
	var b strings.Builder
	if err := httpGet(ctx, url, &b); err != nil {
		return Release{}, err
	}
	var r Release
	return r, json.Unmarshal([]byte(b.String()), &r)
}

var semverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// compareVersions returns -1, 0 or 1; ok is false when either is not a release version (vX.Y.Z).
func compareVersions(a, b string) (int, bool) {
	ma, mb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0, false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// assetName is the binary of this machine in a release.
func assetName() string { return "runsnip-" + runtime.GOOS + "-" + runtime.GOARCH }

// checksumOf finds a file's SHA-256 in a SHA256SUMS file.
func checksumOf(sums, name string) (string, error) {
	for _, l := range strings.Split(sums, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no line for %s", name)
}

func selfCmd(ctx context.Context, g globals, args []string, out io.Writer) (int, error) {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "check":
		r, err := fetchRelease(ctx, "")
		if err != nil {
			return 1, err
		}
		cmp, ok := compareVersions(version, r.Tag)
		newer := !ok || cmp < 0
		if len(args) > 0 && args[0] == "--json" {
			return 0, json.NewEncoder(out).Encode(map[string]any{"current": version, "latest": r.Tag, "newer": newer, "published": r.Date, "changes": r.Body})
		}
		fmt.Fprintf(out, "runsnip %s · latest release %s (%s)\n", version, r.Tag, r.Date)
		if newer {
			fmt.Fprintf(out, "\n%s\n\nrunsnip self update installs it\n", strings.TrimSpace(r.Body))
		} else {
			fmt.Fprintln(out, "up to date")
		}
		return 0, nil
	case "health":
		c := serveConfig(g.state)
		v, ok := waitHealth(ctx, c.Listen, "", 2*time.Second)
		if !ok {
			return 1, fmt.Errorf("runsnip serve does not answer on %s", c.Listen)
		}
		fmt.Fprintf(out, "runsnip serve %s answers on %s\n", v, c.Listen)
		return 0, nil
	case "update", "rollback":
		return runJob(ctx, g, nil, "_runsnip", "self", append([]string{sub}, args...), out, func(w io.Writer) (int, error) {
			if sub == "rollback" {
				return selfRollback(ctx, g, w)
			}
			return selfUpdate(ctx, g, args, w)
		})
	}
	fmt.Fprintln(out, selfUsage)
	if sub == "" || sub == "help" {
		return 0, nil
	}
	return 2, fmt.Errorf("unknown: runsnip self %s", sub)
}

// exePath is the installed binary (links resolved).
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p := os.Getenv("RUNSNIP_EXE"); p != "" { // tests
		exe = p
	}
	return filepath.EvalSymlinks(exe)
}

func selfUpdate(ctx context.Context, g globals, args []string, w io.Writer) (int, error) {
	want, wait := "", time.Duration(0)
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--version" && i+1 < len(args):
			want, i = args[i+1], i+1
		case args[i] == "--wait" && i+1 < len(args):
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return 2, err
			}
			wait, i = d, i+1
		}
	}
	// Never while a stack's job is running: wait for them, or say whose they are.
	deadline := time.Now().Add(wait)
	for {
		busy := otherBusy(g.state)
		if len(busy) == 0 {
			break
		}
		if time.Now().After(deadline) {
			return 1, fmt.Errorf("not updating while a job runs: %v (--wait 10m waits for it)", busy[0])
		}
		time.Sleep(time.Second)
	}
	r, err := fetchRelease(ctx, want)
	if err != nil {
		return 1, err
	}
	cmp, ok := compareVersions(version, r.Tag)
	if ok && cmp == 0 {
		fmt.Fprintf(w, "runsnip %s is the version asked for: nothing to do\n", version)
		return 0, nil
	}
	if ok && cmp > 0 && want == "" {
		return 1, fmt.Errorf("%s is older than this runsnip (%s): no downgrade unless --version %s", r.Tag, version, r.Tag)
	}
	var binURL, sumsURL string
	for _, a := range r.Assets {
		switch a.Name {
		case assetName():
			binURL = a.URL()
		case "SHA256SUMS":
			sumsURL = a.URL()
		}
	}
	if binURL == "" || sumsURL == "" {
		return 1, fmt.Errorf("release %s has no %s or no SHA256SUMS", r.Tag, assetName())
	}
	exe, err := exePath()
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "▶ runsnip %s → %s (%s)\n", version, r.Tag, exe)
	var sums strings.Builder
	if err := httpGet(ctx, sumsURL, &sums); err != nil {
		return 1, err
	}
	expect, err := checksumOf(sums.String(), assetName())
	if err != nil {
		return 1, err
	}
	next := exe + ".new"
	if err := download(ctx, binURL, next, expect); err != nil {
		os.Remove(next)
		return 1, err
	}
	fmt.Fprintf(w, "  ✓ SHA-256 %s matches the release's SHA256SUMS\n", expect[:16]+"…")
	// The new one must at least run here before it replaces anything.
	if outb, err := exec.CommandContext(ctx, next, "--version").Output(); err != nil || !strings.Contains(string(outb), strings.TrimPrefix(r.Tag, "v")) {
		os.Remove(next)
		return 1, fmt.Errorf("the downloaded binary does not run here or is not %s (%q)", r.Tag, strings.TrimSpace(string(outb)))
	}
	if err := swap(exe, next); err != nil {
		return 1, err
	}
	fmt.Fprintf(w, "  ✓ swapped in; the previous one is %s.previous\n", exe)
	_ = host.Audit(g.state, map[string]any{"event": "self.update", "from": version, "to": r.Tag, "user": who(), "via": via()})
	return restartAndWatch(ctx, g, exe, r.Tag, w)
}

func download(ctx context.Context, url, path, sha string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	if err := httpGet(ctx, url, io.MultiWriter(f, h)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return fmt.Errorf("checksum mismatch: the download is %s, SHA256SUMS says %s — nothing replaced", got, sha)
	}
	return os.Chmod(path, 0o755)
}

// swap puts next in exe's place atomically, keeping the current one as exe.previous: a hard link keeps the old
// binary, then a rename replaces the name in one step (a running runsnip keeps its own file).
func swap(exe, next string) error {
	prev := exe + ".previous"
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		// Another filesystem than expected: copy instead.
		b, err := os.ReadFile(exe)
		if err != nil {
			return err
		}
		if err := os.WriteFile(prev, b, 0o755); err != nil {
			return err
		}
	}
	return os.Rename(next, exe)
}

// selfRollback puts runsnip.previous back.
func selfRollback(ctx context.Context, g globals, w io.Writer) (int, error) {
	exe, err := exePath()
	if err != nil {
		return 1, err
	}
	prev := exe + ".previous"
	if _, err := os.Stat(prev); err != nil {
		return 1, fmt.Errorf("no %s to go back to", prev)
	}
	if err := os.Rename(prev, exe); err != nil {
		return 1, err
	}
	outb, _ := exec.CommandContext(ctx, exe, "--version").Output()
	fmt.Fprintf(w, "▶ back to %s\n", strings.TrimSpace(string(outb)))
	_ = host.Audit(g.state, map[string]any{"event": "self.rollback", "to": strings.TrimSpace(string(outb)), "user": who(), "via": via()})
	return restartAndWatch(ctx, g, exe, "", w)
}

// serveUnit is the systemd unit install.sh writes for runsnip serve.
const serveUnit = "runsnip-serve.service"

func hasUnit() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "cat", serveUnit).Run() == nil
}

// restartAndWatch restarts runsnip serve and waits for the new version on /healthz; if it does not answer in time,
// the previous binary goes back and serve is restarted again.
func restartAndWatch(ctx context.Context, g globals, exe, want string, w io.Writer) (int, error) {
	if !hasUnit() {
		fmt.Fprintln(w, "  no runsnip-serve unit here: nothing to restart")
		return 0, nil
	}
	fmt.Fprintln(w, "▶ systemctl restart "+serveUnit)
	if err := exec.CommandContext(ctx, "systemctl", "restart", serveUnit).Run(); err != nil {
		fmt.Fprintln(w, "  ✗ restart failed: "+err.Error())
	}
	cfg := serveConfig(g.state)
	timeout := 30 * time.Second
	if v, err := time.ParseDuration(os.Getenv("RUNSNIP_HEALTH_TIMEOUT")); err == nil {
		timeout = v
	}
	if got, ok := waitHealth(ctx, cfg.Listen, want, timeout); ok {
		fmt.Fprintf(w, "  ✓ runsnip serve answers /healthz as %s\n", got)
		return 0, nil
	}
	// It did not come back: the previous binary goes back.
	fmt.Fprintf(w, "  ✗ runsnip serve did not answer /healthz as %s within %s — rolling back\n", want, timeout)
	prev := exe + ".previous"
	if _, err := os.Stat(prev); err == nil && want != "" {
		bad := exe + ".failed"
		_ = os.Rename(exe, bad)
		_ = os.Rename(prev, exe)
		_ = exec.CommandContext(ctx, "systemctl", "restart", serveUnit).Run()
		got, ok := waitHealth(ctx, cfg.Listen, "", timeout)
		_ = host.Audit(g.state, map[string]any{"event": "self.update.rolled-back", "to": got, "failed": want, "user": who(), "via": via(), "ok": ok})
		if !ok {
			fmt.Fprintf(w, "  ✗ restored the previous runsnip, but serve does not answer either: systemctl status %s; the failed one is %s\n", serveUnit, bad)
			return 1, errors.New("the update did not come up, and neither did the previous runsnip")
		}
		fmt.Fprintf(w, "  ↺ restored the previous runsnip: serve answers as %s; the failed one is %s\n", got, bad)
	}
	return 1, errors.New("the update did not come up; rolled back")
}

// ServeConfig is what serve writes on start: where it listens.
type ServeConfig struct {
	Listen string `json:"listen"`
}

func serveConfig(state string) ServeConfig {
	c := ServeConfig{Listen: "127.0.0.1:7070"}
	_ = host.ReadJSON(filepath.Join(state, "serve.json"), &c)
	return c
}

func waitHealth(ctx context.Context, listen, want string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var b strings.Builder
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := httpGet(cctx, "http://"+listen+"/healthz", &b)
		cancel()
		if err == nil {
			var h struct{ Version string }
			if json.Unmarshal([]byte(b.String()), &h) == nil && (want == "" || strings.TrimPrefix(h.Version, "v") == strings.TrimPrefix(want, "v")) {
				return h.Version, true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", false
}

// otherBusy lists the stack jobs holding a lock (this update's own lock aside).
func otherBusy(state string) []jobs.ErrBusy {
	var out []jobs.ErrBusy
	for _, b := range jobs.Busy(state) {
		if b.Stack != "_runsnip" {
			out = append(out, b)
		}
	}
	return out
}
