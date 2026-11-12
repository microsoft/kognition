// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityFixtureDispositionsRequireExactReviewedInputs(t *testing.T) {
	for _, name := range []string{"reviewed", "CRLF", "changed fixture", "broader path", "vulnerability suppression", "missing policy", "nonregular fixture", "output failure"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for _, target := range []string{identityFixtureTarget, identityIgnoreTarget} {
				content, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(target)))
				if err != nil {
					t.Fatal(err)
				}
				content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
				switch name {
				case "CRLF":
					content = bytes.ReplaceAll(content, []byte("\n"), []byte("\r\n"))
				case "changed fixture":
					if target == identityFixtureTarget {
						content = bytes.Replace(content, []byte("hostPID: true"), []byte("hostPID: false"), 1)
					}
				case "broader path":
					if target == identityIgnoreTarget {
						content = bytes.Replace(content, []byte(identityFixtureTarget), []byte("**/*.yaml"), 1)
					}
				case "vulnerability suppression":
					if target == identityIgnoreTarget {
						content = bytes.Replace(content, []byte("misconfigurations:"), []byte("vulnerabilities:"), 1)
					}
				case "missing policy":
					if target == identityIgnoreTarget {
						continue
					}
				}
				path := filepath.Join(root, filepath.FromSlash(target))
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if name == "nonregular fixture" && target == identityFixtureTarget {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output, errorOutput bytes.Buffer
			writer := &output
			if name == "output failure" {
				code := runIdentityFixture([]string{"--root", root}, failingDispositionWriter{err: errors.New("verification output failed")}, &errorOutput)
				if code != 1 || !strings.Contains(errorOutput.String(), "verification output failed") {
					t.Fatalf("output failure was not propagated: code=%d stderr=%q", code, errorOutput.String())
				}
				return
			}
			code := runIdentityFixture([]string{"--root", root}, writer, &errorOutput)
			if name == "reviewed" || name == "CRLF" {
				if code != 0 || !strings.Contains(output.String(), "deployed acceptance remains separate") || errorOutput.Len() != 0 {
					t.Fatalf("reviewed inputs rejected: code=%d stdout=%q stderr=%q", code, output.String(), errorOutput.String())
				}
			} else if code != 1 || output.Len() != 0 || errorOutput.Len() == 0 {
				t.Fatalf("unreviewed inputs accepted or failure hidden: code=%d stdout=%q stderr=%q", code, output.String(), errorOutput.String())
			}
		})
	}
}

func TestIdentityFixtureCommandRejectsUnexpectedArguments(t *testing.T) {
	var output, errorOutput bytes.Buffer
	if code := runIdentityFixture([]string{"unexpected"}, &output, &errorOutput); code != 2 || output.Len() != 0 || errorOutput.Len() == 0 {
		t.Fatalf("unexpected arguments accepted: code=%d stdout=%q stderr=%q", code, output.String(), errorOutput.String())
	}
}

func TestIdentityFixtureCommandCannotHideDiagnosticWriteFailure(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"--root", t.TempDir()}} {
		var output bytes.Buffer
		code := runIdentityFixture(args, &output, failingDispositionWriter{err: errors.New("diagnostic writer failed")})
		if code != 1 || output.Len() != 0 {
			t.Fatalf("diagnostic failure appeared successful: code=%d stdout=%q", code, output.String())
		}
	}
}
