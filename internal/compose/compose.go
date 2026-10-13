// Package compose reads a stack's compose file (and its override) without docker: the services, their images,
// profiles, health checks, logging and bind mounts. Everything runsnip detects instead of asking for comes from here.
package compose

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/runsnip/ops/internal/expand"
)

// File is a compose project as written (variables not expanded).
type File struct {
	Path     string   // the main file
	Files    []string // the main file, then the override when present
	Name     string   // top-level `name:`
	Services map[string]*Service
	Order    []string // services in the order the main file lists them (override-only ones after)
	// Extensions are the top-level x-… keys (x-runsnip-profiles among them).
	Extensions map[string]yaml.Node
}

// Service is one service.
type Service struct {
	Name          string
	Image         string
	ContainerName string
	Profiles      []string
	Healthcheck   bool   // a healthcheck test is declared (and not disabled)
	HealthTest    string // the test, joined
	LoggingDriver string
	MaxSize       string
	MaxFile       string
	Binds         []Bind   // bind mounts (host path → container path)
	EnvKeys       []string // names in `environment:`
	// Vars are the variables the service's definition interpolates (${VAR}, anchors merged in included), and
	// EnvFiles the files it loads whole (env_file): together, what in .env reaches it.
	Vars      []string
	EnvFiles  []string
	Env       map[string]string
	DependsOn []string
	// Networks the service joins, and the extra names it answers to on them (aliases, hostname).
	Networks []string
	Aliases  []string
}

// Bind is one bind mount.
type Bind struct{ Source, Target string }

type rawService struct {
	Image         string    `yaml:"image"`
	ContainerName string    `yaml:"container_name"`
	Profiles      []string  `yaml:"profiles"`
	Healthcheck   yaml.Node `yaml:"healthcheck"`
	Logging       *struct {
		Driver  string            `yaml:"driver"`
		Options map[string]string `yaml:"options"`
	} `yaml:"logging"`
	Volumes     []yaml.Node `yaml:"volumes"`
	Environment yaml.Node   `yaml:"environment"`
	DependsOn   yaml.Node   `yaml:"depends_on"`
	EnvFile     yaml.Node   `yaml:"env_file"`
	Networks    yaml.Node   `yaml:"networks"`
	Hostname    string      `yaml:"hostname"`
}

// Load reads path and, beside it, the override compose reads by default (<base>.override.yml).
func Load(path string) (*File, error) {
	f := &File{Path: path, Services: map[string]*Service{}, Extensions: map[string]yaml.Node{}}
	files := []string{path}
	ext := filepath.Ext(path)
	if o := strings.TrimSuffix(path, ext) + ".override" + ext; fileExists(o) {
		files = append(files, o)
	}
	if strings.HasSuffix(path, ".yaml") {
		if o := strings.TrimSuffix(path, ".yaml") + ".override.yml"; fileExists(o) && len(files) == 1 {
			files = append(files, o)
		}
	}
	for i, p := range files {
		if err := f.merge(p, i == 0); err != nil {
			return nil, err
		}
	}
	f.Files = files
	return f, nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Find returns the compose file in dir: compose.yaml, compose.yml, docker-compose.yaml or docker-compose.yml, as
// docker compose looks for them.
func Find(dir string) (string, error) {
	for _, n := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
		if p := filepath.Join(dir, n); fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no compose file in %s (compose.yaml, compose.yml, docker-compose.yaml, docker-compose.yml)", dir)
}

// stripCustomTags drops compose's own tags (!override, !reset) so the node decodes as plain YAML.
func stripCustomTags(n *yaml.Node) {
	if strings.HasPrefix(n.Tag, "!") && !strings.HasPrefix(n.Tag, "!!") {
		n.Tag = ""
	}
	for _, c := range n.Content {
		stripCustomTags(c)
	}
}

func (f *File) merge(path string, main bool) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return nil
	}
	stripCustomTags(&doc)
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: not a compose file", path)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i].Value, root.Content[i+1]
		switch {
		case k == "name" && main:
			f.Name = v.Value
		case strings.HasPrefix(k, "x-"):
			f.Extensions[k] = *v
		case k == "services":
			for j := 0; j+1 < len(v.Content); j += 2 {
				name := v.Content[j].Value
				var r rawService
				if err := v.Content[j+1].Decode(&r); err != nil {
					return fmt.Errorf("%s: service %s: %w", path, name, err)
				}
				s, ok := f.Services[name]
				if !ok {
					s = &Service{Name: name, Env: map[string]string{}}
					f.Services[name] = s
					f.Order = append(f.Order, name)
				}
				s.apply(r)
				s.addVars(v.Content[j+1])
			}
		}
	}
	return nil
}

func (s *Service) apply(r rawService) {
	if r.Image != "" {
		s.Image = r.Image
	}
	if r.ContainerName != "" {
		s.ContainerName = r.ContainerName
	}
	if r.Profiles != nil {
		s.Profiles = r.Profiles
	}
	if r.Healthcheck.Kind == yaml.MappingNode {
		var hc struct {
			Test    yaml.Node `yaml:"test"`
			Disable bool      `yaml:"disable"`
		}
		_ = r.Healthcheck.Decode(&hc)
		test := nodeStrings(hc.Test)
		s.Healthcheck = !hc.Disable && len(test) > 0 && test[0] != "NONE"
		s.HealthTest = strings.Join(test, " ")
	}
	if r.Logging != nil {
		if r.Logging.Driver != "" {
			s.LoggingDriver = r.Logging.Driver
		}
		if v := r.Logging.Options["max-size"]; v != "" {
			s.MaxSize = v
		}
		if v := r.Logging.Options["max-file"]; v != "" {
			s.MaxFile = v
		}
	}
	if len(r.Volumes) > 0 {
		s.Binds = nil
		for _, v := range r.Volumes {
			if b, ok := bindOf(&v); ok {
				s.Binds = append(s.Binds, b)
			}
		}
	}
	switch r.Environment.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(r.Environment.Content); i += 2 {
			k := r.Environment.Content[i].Value
			if _, ok := s.Env[k]; !ok {
				s.EnvKeys = append(s.EnvKeys, k)
			}
			s.Env[k] = r.Environment.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, e := range r.Environment.Content {
			k, v, _ := strings.Cut(e.Value, "=")
			if _, ok := s.Env[k]; !ok {
				s.EnvKeys = append(s.EnvKeys, k)
			}
			s.Env[k] = v
		}
	}
	switch r.EnvFile.Kind {
	case yaml.ScalarNode:
		s.EnvFiles = []string{r.EnvFile.Value}
	case yaml.SequenceNode:
		s.EnvFiles = nil
		for _, e := range r.EnvFile.Content {
			if e.Kind == yaml.MappingNode {
				var long struct{ Path string }
				_ = e.Decode(&long)
				s.EnvFiles = append(s.EnvFiles, long.Path)
			} else {
				s.EnvFiles = append(s.EnvFiles, e.Value)
			}
		}
	}
	if r.Hostname != "" {
		s.Aliases = append(s.Aliases, r.Hostname)
	}
	switch r.Networks.Kind {
	case yaml.SequenceNode:
		s.Networks = nodeStrings(r.Networks)
	case yaml.MappingNode:
		s.Networks = nil
		for i := 0; i+1 < len(r.Networks.Content); i += 2 {
			s.Networks = append(s.Networks, r.Networks.Content[i].Value)
			var n struct {
				Aliases []string `yaml:"aliases"`
			}
			_ = r.Networks.Content[i+1].Decode(&n)
			s.Aliases = append(s.Aliases, n.Aliases...)
		}
	}
	switch r.DependsOn.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(r.DependsOn.Content); i += 2 {
			s.DependsOn = append(s.DependsOn, r.DependsOn.Content[i].Value)
		}
	case yaml.SequenceNode:
		s.DependsOn = nodeStrings(r.DependsOn)
	}
}

func nodeStrings(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	}
	return nil
}

// bindOf reads a volume entry that mounts a host path (./x, /x, ${VAR}/x), short or long syntax.
func bindOf(v *yaml.Node) (Bind, bool) {
	if v.Kind == yaml.ScalarNode {
		parts := strings.Split(v.Value, ":")
		if len(parts) < 2 {
			return Bind{}, false
		}
		src := parts[0]
		if !(strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "$") || strings.HasPrefix(src, "~")) {
			return Bind{}, false // a named volume
		}
		return Bind{Source: src, Target: parts[1]}, true
	}
	var long struct{ Type, Source, Target string }
	if v.Decode(&long) == nil && long.Type == "bind" {
		return Bind{Source: long.Source, Target: long.Target}, true
	}
	return Bind{}, false
}

// addVars collects the variables a service's node interpolates, following aliases (`<<: *app-env`).
func (s *Service) addVars(n *yaml.Node) {
	seen := map[string]bool{}
	for _, v := range s.Vars {
		seen[v] = true
	}
	var walk func(n *yaml.Node, depth int)
	walk = func(n *yaml.Node, depth int) {
		if n == nil || depth > 50 {
			return
		}
		if n.Kind == yaml.AliasNode {
			walk(n.Alias, depth+1)
			return
		}
		if n.Kind == yaml.ScalarNode && strings.Contains(n.Value, "$") {
			for _, name := range expand.Names(n.Value) {
				if !seen[name] {
					seen[name] = true
					s.Vars = append(s.Vars, name)
				}
			}
		}
		for _, c := range n.Content {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
}

// ServicesReading lists, in file order, the services a variable of .env reaches: interpolated in their definition,
// or loaded whole through an env_file that is envName.
func (f *File) ServicesReading(key, envName string) []string {
	var out []string
	for _, n := range f.Order {
		s := f.Services[n]
		hit := false
		for _, v := range s.Vars {
			if v == key {
				hit = true
			}
		}
		for _, e := range s.EnvFiles {
			if filepath.Base(e) == envName {
				hit = true
			}
		}
		if hit {
			out = append(out, n)
		}
	}
	return out
}

// ServiceNames lists every service in file order.
func (f *File) ServiceNames() []string { return append([]string(nil), f.Order...) }

// StringMap reads an x-… block of `key: value` scalars (x-runsnip-profiles).
func (f *File) StringMap(key string) map[string]string {
	n, ok := f.Extensions[key]
	if !ok || n.Kind != yaml.MappingNode {
		return nil
	}
	out := map[string]string{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1].Value
	}
	return out
}

// Image is a released component's image as compose names it: <repo>:${<TagVar>:-<default>}.
type Image struct {
	Repo       string // as written, e.g. ${REGISTRY}-api
	TagVar     string // SERVICE_AUTH_TAG
	TagDefault string // as written, e.g. latest or ${SHOP_TAG:-latest}
}

// ErrNotPinned: the image's tag is not a variable (a third-party image like postgres:16-alpine).
var ErrNotPinned = errors.New("tag is not a variable")

// ParseImage splits an image whose tag is a variable.
func ParseImage(image string) (Image, error) {
	if !strings.HasSuffix(image, "}") {
		return Image{}, ErrNotPinned
	}
	// The tag variable is the ${…} that ends the string: walk back to its opening brace.
	depth, open := 0, -1
	for i := len(image) - 1; i >= 0; i-- {
		switch image[i] {
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				open = i
			}
		}
		if open >= 0 {
			break
		}
	}
	if open < 2 || image[open-1] != '$' || image[open-2] != ':' {
		return Image{}, ErrNotPinned
	}
	body := image[open+1 : len(image)-1]
	n := 0
	for n < len(body) && (body[n] == '_' || body[n] >= 'A' && body[n] <= 'Z' || body[n] >= 'a' && body[n] <= 'z' || n > 0 && body[n] >= '0' && body[n] <= '9') {
		n++
	}
	if n == 0 {
		return Image{}, ErrNotPinned
	}
	img := Image{Repo: image[:open-2], TagVar: body[:n]}
	rest := body[n:]
	switch {
	case strings.HasPrefix(rest, ":-"):
		img.TagDefault = rest[2:]
	case strings.HasPrefix(rest, "-"):
		img.TagDefault = rest[1:]
	}
	return img, nil
}

// ShortName is the image's own name: what follows the last `/` of the repository, and a leading `${PREFIX}-`
// removed (${REGISTRY}-api → api, ghcr.io/x/shop-web → shop-web).
func (i Image) ShortName() string {
	r := i.Repo
	if j := strings.LastIndex(r, "}-"); j >= 0 && !strings.Contains(r[j:], "/") {
		return r[j+2:]
	}
	if j := strings.LastIndex(r, "/"); j >= 0 {
		return r[j+1:]
	}
	return r
}

// Sorted returns names sorted (compose prints them in another order every time).
func Sorted(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}
