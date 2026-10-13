// Package paths decides whether a folder may hold a stack's checkout or its data — before anything is created. Every
// refusal says why. The web asks through `runsnip stack check-paths`: the CLI decides, the page only shows it.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/runsnip/ops/internal/host"
)

// Kind is what a path is for.
type Kind string

const (
	Clone Kind = "clone" // the infra checkout: must be empty or absent
	Data  Kind = "data"  // the data folder: empty, absent, or this stack's already
)

// Result is one path's verdict.
type Result struct {
	Kind     Kind     `json:"kind"`
	Path     string   `json:"path"`     // as typed
	Resolved string   `json:"resolved"` // symlinks resolved
	OK       bool     `json:"ok"`
	Reasons  []string `json:"reasons,omitempty"`  // why it is refused
	Warnings []string `json:"warnings,omitempty"` // allowed, but worth knowing
	FreeGB   float64  `json:"freeGB,omitempty"`
}

// Options are the thresholds and the world the checks look at (tests replace them).
type Options struct {
	State      string                            // runsnip's state folder (its stacks are read from it)
	WarnFreeGB float64                           // warn below (default 10)
	MinFreeGB  float64                           // refuse below (default 1)
	FS         func(path string) (FSInfo, error) // the filesystem of an existing path
	Writable   func(path string) bool            // whether runsnip can write in an existing folder
	Marker     func(dir string) (string, error)  // the stack a data folder was made for (.runsnip-stack)
	EvalLinks  func(path string) (string, error) // filepath.EvalSymlinks
}

// FSInfo is what the checks need from a filesystem.
type FSInfo struct {
	Type      string // ext4, xfs, btrfs, tmpfs, ramfs, nfs, overlay…
	FreeBytes uint64
}

// MarkerFile is written into a data folder when a stack takes it, so a re-adopt can be recognised.
const MarkerFile = ".runsnip-stack"

// systemExact are folders that may never be a stack's path themselves.
var systemExact = []string{"/", "/bin", "/sbin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/proc",
	"/root", "/run", "/sys", "/usr", "/usr/local", "/var", "/var/lib", "/var/log", "/var/run", "/tmp", "/var/tmp",
	"/home", "/mnt", "/media", "/opt", "/srv", "/snap", "/lost+found", "/nix", "/Users", "/private", "/System", "/Library", "/Applications", "/Volumes"}

// systemTrees are folders nothing of a stack may be inside.
var systemTrees = []string{"/bin", "/sbin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/proc", "/run",
	"/sys", "/usr", "/var/run", "/var/lib/docker", "/var/lib/containerd", "/var/lib/runsnip", "/tmp", "/var/tmp", "/snap", "/System"}

func under(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// resolve follows symlinks of the deepest existing part of p and appends the rest.
func resolve(p string, eval func(string) (string, error)) (string, error) {
	cur, rest := p, ""
	for {
		if r, err := eval(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// existing is p's deepest existing ancestor (p itself when it exists).
func existing(p string) string {
	for cur := p; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(cur); err == nil {
			return cur
		}
		if filepath.Dir(cur) == cur {
			return cur
		}
	}
}

// Check decides on a stack's checkout and data folders.
func Check(name, clonePath, dataPath string, o Options) []Result {
	res := []Result{one(name, Clone, clonePath, o), one(name, Data, dataPath, o)}
	// The two must not hold each other: the checkout holds no data, and data never sits in a checkout.
	if res[0].Resolved != "" && res[1].Resolved != "" && (under(res[0].Resolved, res[1].Resolved) || under(res[1].Resolved, res[0].Resolved)) {
		for i := range res {
			res[i].Reasons = append(res[i].Reasons, "the checkout and the data folder must not be inside each other")
			res[i].OK = false
		}
	}
	return res
}

func (o *Options) defaults() {
	if o.WarnFreeGB == 0 {
		o.WarnFreeGB = 10
	}
	if o.MinFreeGB == 0 {
		o.MinFreeGB = 1
	}
	if o.EvalLinks == nil {
		o.EvalLinks = filepath.EvalSymlinks
	}
	if o.FS == nil {
		o.FS = fsInfo
	}
	if o.Writable == nil {
		o.Writable = writable
	}
	if o.Marker == nil {
		o.Marker = func(dir string) (string, error) {
			b, err := os.ReadFile(filepath.Join(dir, MarkerFile))
			return strings.TrimSpace(string(b)), err
		}
	}
}

func one(name string, kind Kind, p string, o Options) Result {
	o.defaults()
	r := Result{Kind: kind, Path: p}
	refuse := func(f string, a ...any) { r.Reasons = append(r.Reasons, fmt.Sprintf(f, a...)) }
	switch {
	case p == "":
		refuse("no path given")
	case !filepath.IsAbs(p):
		refuse("not an absolute path (it must start with /)")
	case strings.Contains("/"+p+"/", "/../") || strings.Contains("/"+p+"/", "/./"):
		refuse("contains . or .. — write the folder itself")
	case filepath.Clean(p) != p:
		refuse("not normalised: write %s", filepath.Clean(p))
	}
	if len(r.Reasons) > 0 {
		return r
	}
	res, err := resolve(p, o.EvalLinks)
	if err != nil {
		refuse("cannot resolve its links: %v", err)
		return r
	}
	r.Resolved = res
	if res != p {
		r.Warnings = append(r.Warnings, "it is a link to "+res+": the checks below are on "+res)
	}
	// The path as typed and as resolved: /etc typed, or a link that lands in /etc, are both refused.
	seen := map[string]bool{}
	for _, cand := range []string{p, res} {
		for _, s := range systemExact {
			if cand == s && !seen[s] {
				seen[s] = true
				refuse("%s is a system folder: choose a folder of its own inside one (e.g. %s/%s)", s, map[bool]string{true: "/srv", false: s}[s == "/" || s == "/etc"], name)
			}
		}
		for _, s := range systemTrees {
			if under(cand, s) && cand != s && !seen[s] {
				seen[s] = true
				refuse("inside %s, a system folder", s)
			}
		}
	}
	if o.State != "" {
		if under(res, o.State) || under(o.State, res) {
			refuse("holds or is inside runsnip's own state (%s)", o.State)
		}
		if all, err := host.Stacks(o.State); err == nil {
			for _, s := range all {
				if s.Name == name {
					continue
				}
				for _, other := range []string{s.Dir, s.Data} {
					if other == "" {
						continue
					}
					ro, _ := resolve(other, o.EvalLinks)
					if under(res, ro) || under(ro, res) {
						refuse("inside or holding %s, a path of the stack %s", other, s.Name)
					}
				}
			}
		}
	}
	ex := existing(res)
	if ex == res {
		st, err := os.Stat(res)
		switch {
		case err != nil:
		case !st.IsDir():
			refuse("exists and is not a folder")
		default:
			entries, _ := os.ReadDir(res)
			if len(entries) > 0 {
				switch kind {
				case Clone:
					refuse("is not empty (%d entries): the checkout needs an empty or absent folder", len(entries))
				case Data:
					if m, err := o.Marker(res); err == nil && m == name {
						r.Warnings = append(r.Warnings, "already the data folder of "+name+": it is taken back as it is (re-adopt)")
					} else if err == nil {
						refuse("is the data folder of another stack (%s)", m)
					} else {
						refuse("is not empty (%d entries) and not this stack's data folder", len(entries))
					}
				}
			}
		}
	}
	if !o.Writable(ex) {
		refuse("%s cannot be written by runsnip (owner or mode)", ex)
	}
	if fi, err := o.FS(ex); err == nil {
		switch fi.Type {
		case "tmpfs", "ramfs", "proc", "sysfs", "devtmpfs":
			refuse("on %s: it is gone at the next boot", fi.Type)
		case "nfs", "nfs4", "cifs", "smb3", "fuse.sshfs", "9p":
			r.Warnings = append(r.Warnings, "on "+fi.Type+": a network filesystem breaks database locks and Docker's volume permissions")
		case "overlay":
			if kind == Data {
				r.Warnings = append(r.Warnings, "on overlay (inside a container?): Docker cannot bind-mount it reliably")
			}
		}
		gb := float64(fi.FreeBytes) / (1 << 30)
		r.FreeGB = float64(int(gb*10)) / 10
		switch {
		case gb < o.MinFreeGB:
			refuse("only %.1f GB free on its disk (at least %.0f GB)", gb, o.MinFreeGB)
		case gb < o.WarnFreeGB && kind == Data:
			r.Warnings = append(r.Warnings, fmt.Sprintf("%.1f GB free on its disk — data grows: %.0f GB or more is advised", gb, o.WarnFreeGB))
		}
	}
	r.OK = len(r.Reasons) == 0
	return r
}

// OK reports whether every result passed.
func OK(rs []Result) bool {
	for _, r := range rs {
		if !r.OK {
			return false
		}
	}
	return true
}

// Errors joins the refusals.
func Errors(rs []Result) error {
	var msgs []string
	for _, r := range rs {
		for _, x := range r.Reasons {
			msgs = append(msgs, string(r.Kind)+" "+r.Path+": "+x)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "\n"))
}
