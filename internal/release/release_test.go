package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runsnip/ops/internal/def"
	"github.com/runsnip/ops/internal/run"
)

func repo(t *testing.T, stackYAML string) *Repo {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "infra"), 0o755)
	os.MkdirAll(filepath.Join(dir, "services", "orders"), 0o755)
	os.MkdirAll(filepath.Join(dir, "web"), 0o755)
	os.WriteFile(filepath.Join(dir, "services", "orders", "package.json"), []byte("{\n  \"name\": \"orders\",\n  \"version\": \"1.2.3\"\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "web", "package.json"), []byte(`{"version":"0.11.0"}`), 0o644)
	d, err := def.Parse([]byte(stackYAML))
	if err != nil {
		t.Fatal(err)
	}
	return &Repo{Dir: dir, StackDir: filepath.Join(dir, "infra"), Def: d, Run: &run.Fake{}}
}

func TestVersionFilesAndPathsFollowTheStandardUnlessOverridden(t *testing.T) {
	r := repo(t, "standard: 1\nrelease:\n  version_files: [{match: shop, file: apps/shop/package.json}]\n")
	for c, want := range map[string]string{"service-orders": "services/orders/package.json", "web": "web/package.json", "shop": "apps/shop/package.json"} {
		if got := r.VersionFile(c); got != want {
			t.Errorf("%s: %s", c, got)
		}
	}
	if v, _ := r.Version("service-orders"); v != "1.2.3" {
		t.Fatal(v)
	}
	if strings.Join(r.Paths("service-orders"), " ") != "services/orders" || strings.Join(r.Paths(Infra), " ") != "infra/" {
		t.Fatalf("%v %v", r.Paths("service-orders"), r.Paths(Infra))
	}
}

func TestStepsDryRunAndInfra(t *testing.T) {
	r := repo(t, "standard: 1\nrelease:\n  verify: [make, test]\n")
	got := r.Steps([]string{"a", "b"}, true, true, true)
	want := [][]string{{"make", "test"}, {"bash", "scripts/build.sh", "--dry-run", "a", "b"}, {"bash", "scripts/publish-infra.sh", "--dry-run"}}
	if len(got) != 3 || strings.Join(got[0], " ") != strings.Join(want[0], " ") || strings.Join(got[1], " ") != strings.Join(want[1], " ") || strings.Join(got[2], " ") != strings.Join(want[2], " ") {
		t.Fatalf("%v", got)
	}
	g := repo(t, "standard: 1\nrelease:\n  build: [bash, scripts/release.sh]\n  publish_infra: [bash, scripts/publish-infra.sh]\n")
	if s := g.Steps([]string{"api"}, false, false, false); len(s) != 1 || strings.Join(s[0], " ") != "bash scripts/release.sh api" {
		t.Fatalf("%v", s)
	}
}

func TestBumpAndSetVersions(t *testing.T) {
	for in, want := range map[[2]string]string{{"1.2.3", "patch"}: "1.2.4", {"1.2.3", "minor"}: "1.3.0", {"1.2.3", "major"}: "2.0.0"} {
		if got, _ := Bump(in[0], in[1]); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
	r := repo(t, "standard: 1\n")
	f := r.Run.(*run.Fake)
	f.On("", "")
	if err := r.SetVersions(context.Background(), map[string]string{"service-orders": "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, "services/orders/package.json"))
	if string(b) != "{\n  \"name\": \"orders\",\n  \"version\": \"1.3.0\"\n}\n" {
		t.Fatalf("only the value changes:\n%s", b)
	}
	lines := strings.Join(f.Lines(), "\n")
	if !strings.Contains(lines, "git commit -m chore(release): service-orders 1.3.0 -- services/orders/package.json") || !strings.Contains(lines, "git push origin HEAD:main") {
		t.Fatal(lines)
	}
}

func TestParseTagsAndRuns(t *testing.T) {
	tags := ParseTags("web/v0.10.0\taaa\t\nweb/v0.11.0\tbbb\tccc\nservice-orders/v1.0.0\tddd\t\nnot-a-release\teee\t\n", "{component}/v{version}")
	if tags["web"].Version != "0.11.0" || tags["web"].Commit != "ccc" || tags["service-orders"].Version != "1.0.0" || len(tags) != 2 {
		t.Fatalf("%v", tags)
	}
	runs, err := ParseRuns(`[{"headBranch":"deploy/service-orders/v1.0.0","status":"completed","conclusion":"success","url":"u1","createdAt":"2026-10-12T01:00:00Z"},
	  {"headBranch":"main","status":"completed","conclusion":"success","url":"u2","createdAt":"2026-10-12T02:00:00Z"},
	  {"headBranch":"deploy/shop/v0.2.0","status":"in_progress","conclusion":"","url":"u3","createdAt":"2026-10-12T03:00:00Z"}]`, "deploy/{component}/v{version}")
	if err != nil || len(runs) != 2 || runs[0].Component != "shop" || runs[0].Version != "0.2.0" || runs[1].Component != "service-orders" {
		t.Fatalf("%+v %v", runs, err)
	}
}
