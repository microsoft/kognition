// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var completedTCRow = regexp.MustCompile(`\|\s*(TC-\d{4})\s*✅\s*\|`)
var tcID = regexp.MustCompile(`TC-\d{4}`)

func TestCompletedEpicTraceabilityCoverage(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	backlogPath := filepath.Join(repoRoot, "BACKLOG.md")
	backlog, err := os.ReadFile(backlogPath)
	if err != nil {
		t.Fatalf("read backlog: %v", err)
	}

	want := make(map[string]struct{})
	for _, m := range completedTCRow.FindAllStringSubmatch(string(backlog), -1) {
		want[m[1]] = struct{}{}
	}
	if len(want) == 0 {
		t.Fatal("no completed TC rows found in BACKLOG.md")
	}

	testFiles, err := collectTestFiles(repoRoot)
	if err != nil {
		t.Fatalf("collect test files: %v", err)
	}

	seen := make(map[string]struct{})
	for _, p := range testFiles {
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for _, id := range tcID.FindAllString(string(b), -1) {
			seen[id] = struct{}{}
		}
	}

	missing := make([]string, 0)
	for tc := range want {
		if _, ok := seen[tc]; !ok {
			missing = append(missing, tc)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Fatalf("missing completed TC trace tags in _test.go files: %s", strings.Join(missing, ", "))
	}
}

func collectTestFiles(root string) ([]string, error) {
	files := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "bin" || strings.HasPrefix(base, ".") && base != ".github" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(wd, "go.mod")); statErr == nil {
			return wd, nil
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", os.ErrNotExist
		}
		wd = parent
	}
}
