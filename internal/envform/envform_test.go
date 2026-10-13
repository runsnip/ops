package envform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const example = `# Example App — header text

# ---- image -------------------------------------------------------------------
# Lower-case registry account.
REGISTRY=ghcr.io/example-org/example-app
WEB_TAG=latest

# ---- secrets -----------------------------------------------------------------
# Signs sessions.
# Changing it signs everyone out.
JWT_SECRET=
# R2_BUCKET=
# R2_ENDPOINT=https://default.example
# SMTP_SECURE=false          # true for port 465
API_PUBLIC_KEY=
`

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func load(t *testing.T, dir string) *EnvForm {
	t.Helper()
	form, err := Load(filepath.Join(dir, ".env.example"), filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	return form
}

func field(t *testing.T, f *EnvForm, key string) *Field {
	t.Helper()
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return &f.Fields[i]
		}
	}
	t.Fatalf("no field %s", key)
	return nil
}

func render(t *testing.T, f *EnvForm) string {
	t.Helper()
	text, err := f.Render()
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestFieldsComeFromTheExampleWithTheirHelpAndSection(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	form := load(t, dir)

	var keys []string
	for _, f := range form.Fields {
		keys = append(keys, f.Key)
	}
	if got := strings.Join(keys, " "); got != "REGISTRY WEB_TAG JWT_SECRET R2_BUCKET R2_ENDPOINT SMTP_SECURE API_PUBLIC_KEY" {
		t.Fatalf("fields: %s", got)
	}

	jwt := field(t, form, "JWT_SECRET")
	if jwt.Section != "secrets" || jwt.Help != "Signs sessions.\nChanging it signs everyone out." || !jwt.Secret {
		t.Fatalf("JWT_SECRET: %+v", jwt)
	}
	if prefix := field(t, form, "REGISTRY"); prefix.Help != "Lower-case registry account." || prefix.Value != "ghcr.io/example-org/example-app" {
		t.Fatalf("REGISTRY: %+v", prefix)
	}
	if tag := field(t, form, "WEB_TAG"); tag.Help != "" {
		t.Fatalf("help belongs to the line right under it only: %q", tag.Help)
	}
	if r2 := field(t, form, "R2_ENDPOINT"); !r2.Optional || r2.Default != "https://default.example" {
		t.Fatalf("R2_ENDPOINT: %+v", r2)
	}
	if smtp := field(t, form, "SMTP_SECURE"); smtp.Value != "false" || smtp.Help != "true for port 465" {
		t.Fatalf("a comment after the value is help, not value: %+v", smtp)
	}
	if public := field(t, form, "API_PUBLIC_KEY"); public.Secret {
		t.Fatal("a public key is not a secret")
	}
}

func TestAnUntouchedFormWritesTheSameValues(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	write(t, dir, ".env", "JWT_SECRET=old\nexport R2_BUCKET='my bucket'\nJWT_SECRET=newest # rotated\nCUSTOM_THING=keep me\n")
	form := load(t, dir)

	if v := field(t, form, "JWT_SECRET").Value; v != "newest" {
		t.Fatalf("a later line wins, as compose reads it: %q", v)
	}
	if v := field(t, form, "R2_BUCKET").Value; v != "my bucket" {
		t.Fatalf("quotes are not part of the value: %q", v)
	}
	if form.Edited() != 0 {
		t.Fatal("nothing edited yet")
	}

	rendered := render(t, form)
	for _, want := range []string{
		"JWT_SECRET=newest # rotated\n",
		"R2_BUCKET='my bucket'\n",
		"# R2_ENDPOINT=https://default.example\n",
		"# SMTP_SECURE=false          # true for port 465\n",
		"\nREGISTRY=\n",
		"# Signs sessions.\n",
		extraHeader + "\n" + extraNote + "\nCUSTOM_THING=keep me\n",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in:\n%s", want, rendered)
		}
	}
	if strings.Count(rendered, "JWT_SECRET=") != 1 {
		t.Fatal("one line per variable")
	}
}

func TestEditedValuesAreQuotedSoBashAndComposeReadThemAlike(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	form := load(t, dir)

	field(t, form, "JWT_SECRET").Value = `a b$HOME;rm -rf "x"`
	field(t, form, "REGISTRY").Value = "ghcr.io/other/example-app"
	rendered := render(t, form)

	if !strings.Contains(rendered, `JWT_SECRET='a b$HOME;rm -rf "x"'`+"\n") {
		t.Fatalf("unsafe value quoted:\n%s", rendered)
	}
	if !strings.Contains(rendered, "REGISTRY=ghcr.io/other/example-app\n") {
		t.Fatal("a safe value is written as it is")
	}

	// bash reads it back as the value typed, and runs nothing.
	write(t, dir, ".env", rendered)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	sh := exec.Command("bash", "-c", `set -a; . ./.env; set +a; printf %s "$JWT_SECRET"`)
	sh.Dir = dir
	out, _ := sh.CombinedOutput()
	if string(out) != `a b$HOME;rm -rf "x"` {
		t.Fatalf("bash read %q", out)
	}

	field(t, form, "JWT_SECRET").Value = "it's"
	if _, err := form.Render(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("a quote is refused, naming the variable: %v", err)
	}
}

func TestOnAndOffAreWhatTheFileSays(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	write(t, dir, ".env", "R2_BUCKET=b\n# JWT_SECRET=kept-while-off\n  #SMTP_SECURE=true\n")
	form := load(t, dir)

	if f := field(t, form, "R2_BUCKET"); !f.Enabled || f.Value != "b" {
		t.Fatalf("active in .env is on: %+v", f)
	}
	if f := field(t, form, "JWT_SECRET"); f.Enabled || f.Value != "kept-while-off" || !f.Set {
		t.Fatalf("commented in .env is off, its value kept: %+v", f)
	}
	if f := field(t, form, "SMTP_SECURE"); f.Enabled || f.Value != "true" {
		t.Fatalf("however it is commented: %+v", f)
	}
	if f := field(t, form, "R2_ENDPOINT"); f.Enabled || f.Set || f.Value != "https://default.example" {
		t.Fatalf("not in .env, commented in the example: off, as the example has it: %+v", f)
	}
	if c := form.Combined(); c.Off != 2 || c.Opened != 1 {
		t.Fatalf("report: %+v", c)
	}

	field(t, form, "R2_BUCKET").Enabled = false
	field(t, form, "JWT_SECRET").Enabled = true
	endpoint := field(t, form, "R2_ENDPOINT")
	endpoint.Value, endpoint.Enabled = "https://eu.example", true
	field(t, form, "SMTP_SECURE").Value = "false"
	rendered := render(t, form)

	for _, want := range []string{
		"\n# R2_BUCKET=b\n",             // turned off: commented, value kept
		"\nJWT_SECRET=kept-while-off\n", // turned on: the value it kept
		"\nR2_ENDPOINT=https://eu.example\n",
		"\n# SMTP_SECURE=false\n", // edited while off stays off
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q in:\n%s", want, rendered)
		}
	}

	// Saved, and read again: the file says what the form said.
	if _, err := form.Save(filepath.Join(dir, ".env"), time.Now()); err != nil {
		t.Fatal(err)
	}
	again := load(t, dir)
	for key, on := range map[string]bool{"R2_BUCKET": false, "JWT_SECRET": true, "R2_ENDPOINT": true, "SMTP_SECURE": false} {
		if f := field(t, again, key); f.Enabled != on || f.Edited() {
			t.Fatalf("%s after save: %+v", key, f)
		}
	}
}

func TestSaveKeepsTheOldFileAndWritesMode600(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	write(t, dir, ".env", "JWT_SECRET=before\n")
	os.Chmod(filepath.Join(dir, ".env"), 0o644)

	form := load(t, dir)
	field(t, form, "JWT_SECRET").Value = "after"

	backup, err := form.Save(filepath.Join(dir, ".env"), time.Date(2026, 10, 3, 10, 11, 12, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(backup) != ".env.bak-20261003-101112" {
		t.Fatalf("backup: %s", backup)
	}
	if old := read(t, dir, filepath.Base(backup)); old != "JWT_SECRET=before\n" {
		t.Fatalf("backup holds %q", old)
	}
	for _, name := range []string{".env", filepath.Base(backup)} {
		info, _ := os.Stat(filepath.Join(dir, name))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", name, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".env.tmp")); !os.IsNotExist(err) {
		t.Fatal("no temporary file is left behind")
	}
	if field(t, load(t, dir), "JWT_SECRET").Value != "after" {
		t.Fatal("the saved value reads back")
	}
}

func TestSaveRefusesWhenEnvChangedSinceItWasRead(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	write(t, dir, ".env", "WEB_TAG=1.0.0\n")

	form := load(t, dir)
	field(t, form, "JWT_SECRET").Value = "x"

	// `runsnip pin` writes .env while the form is open.
	write(t, dir, ".env", "WEB_TAG=1.1.0\n")

	if _, err := form.Save(filepath.Join(dir, ".env"), time.Now()); !errors.Is(err, ErrEnvChanged) {
		t.Fatalf("expected refusal, got %v", err)
	}
	if read(t, dir, ".env") != "WEB_TAG=1.1.0\n" {
		t.Fatal("the newer file is untouched")
	}
}

func TestNoEnvYetIsTheExample(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, ".env.example", example)
	form := load(t, dir)

	if field(t, form, "WEB_TAG").Value != "latest" {
		t.Fatal("defaults from the example")
	}
	if backup, err := form.Save(filepath.Join(dir, ".env"), time.Now()); err != nil || backup != "" {
		t.Fatalf("no backup of a file that did not exist: %q %v", backup, err)
	}
}

// Goldens: testdata/combine/<case>/{example,env} and the .env.combined the shell script runsnip replaced wrote
// for them before the Go tool replaced it (its two Vietnamese header lines translated). The tool must keep writing
// exactly that.
func TestUntouchedTheFormWritesWhatTheScriptsCombineWrote(t *testing.T) {
	cases, _ := filepath.Glob(filepath.Join("testdata", "combine", "*"))
	if len(cases) < 6 {
		t.Fatalf("goldens missing: %v", cases)
	}
	for _, dir := range cases {
		form, err := Load(filepath.Join(dir, "example"), filepath.Join(dir, "env"))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(filepath.Join(dir, "combined"))
		if got := render(t, form); got != string(want) {
			t.Errorf("%s: %s", filepath.Base(dir), firstDiff(string(want), got))
		}
	}
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) || i < len(bl); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + strconv.Quote(x) + "\n  got:  " + strconv.Quote(y)
		}
	}
	return "same lines, different bytes"
}
