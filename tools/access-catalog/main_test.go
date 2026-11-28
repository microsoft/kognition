// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
)

func expectedTemplate(t *testing.T) []byte {
	t.Helper()
	out, err := accesscatalog.Default().RenderChartTemplate()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunWritesTheGeneratedTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "human-roles.yaml")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--out", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expectedTemplate(t)) {
		t.Fatal("written template differs from the catalog render")
	}
}

func TestRunCheckPassesOnACleanFileAndDoesNotModifyIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "human-roles.yaml")
	if err := os.WriteFile(path, expectedTemplate(t), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--check", "--out", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr.String())
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(info.ModTime()) {
		t.Fatal("--check must not rewrite the file")
	}
}

func TestRunCheckFailsOnDriftAndLeavesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "human-roles.yaml")
	drifted := append(expectedTemplate(t), []byte("# manual edit\n")...)
	if err := os.WriteFile(path, drifted, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--check", "--out", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 for a drifted file", code)
	}
	if !strings.Contains(stderr.String(), "out of date") || !strings.Contains(stderr.String(), "access-catalog") {
		t.Errorf("stderr should say the file is out of date and how to regenerate it, got %q", stderr.String())
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, drifted) {
		t.Fatal("--check must not modify a drifted file")
	}
}

func TestRunCheckFailsWhenTheFileIsMissing(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--check", "--out", filepath.Join(t.TempDir(), "absent.yaml")}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 for a missing file", code)
	}
}

func TestRunRejectsUnknownFlagsAndUnwritablePaths(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("exit %d, want 2 for an unknown flag", code)
	}
	stderr.Reset()
	if code := run([]string{"--out", t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1 when the output path is a directory", code)
	}
}
