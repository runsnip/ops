package main

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runsnip names no project it serves: not in its code, its tests, its fixtures or its docs. What a project needs is
// in that project's stack.yaml. The names to look for belong to those projects, so they are not listed here either:
// RUNSNIP_LINT_DENYLIST points at a file (one literal per line, # comments) kept outside this repository. Without
// it the test is skipped.
func TestTheRepositoryNamesNoProject(t *testing.T) {
	path := os.Getenv("RUNSNIP_LINT_DENYLIST")
	if path == "" {
		t.Skip("RUNSNIP_LINT_DENYLIST is not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var words []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if w := strings.TrimSpace(sc.Text()); w != "" && !strings.HasPrefix(w, "#") {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		t.Fatal("the deny list is empty")
	}
	_ = filepath.WalkDir(".", func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if e.Name() == ".git" || e.Name() == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		// Third-party licence texts are reproduced as they are.
		if e.Name() == "NOTICE" || strings.HasPrefix(e.Name(), "OFL-") || strings.HasSuffix(e.Name(), ".woff2") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, w := range words {
				if strings.Contains(line, w) {
					t.Errorf("%s:%d names %q: %s", p, i+1, w, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
}
