// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func compareGeneratedBindings(expected, actual string) error {
	read := func(root string) (map[string][]byte, error) {
		files := map[string][]byte{}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("nonregular generated file: %s", path)
			}
			name, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			content, err := os.ReadFile(path)
			files[filepath.ToSlash(name)] = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
			return err
		})
		return files, err
	}
	wanted, err := read(expected)
	if err != nil {
		return err
	}
	found, err := read(actual)
	if err != nil {
		return err
	}
	if len(wanted) == 0 || len(wanted) != len(found) {
		return fmt.Errorf("generated file count differs: %d versus %d", len(wanted), len(found))
	}
	for name, content := range wanted {
		other, exists := found[name]
		if !exists || !bytes.Equal(content, other) {
			return fmt.Errorf("generated binding differs: %s", name)
		}
	}
	return nil
}

func TestGeneratedBindingsRejectLiteralDrift(test *testing.T) {
	for _, scenario := range []string{"equal", "line endings", "literal", "missing", "extra", "wrong layout"} {
		test.Run(scenario, func(test *testing.T) {
			expected, actual := test.TempDir(), test.TempDir()
			original := "package fixture\n\nconst Descriptor = \"original\"\n"
			write := func(root, name, content string) {
				test.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
					test.Fatal(err)
				}
			}
			write(expected, "fixture.pb.go", original)
			content, name := original, "fixture.pb.go"
			switch scenario {
			case "line endings":
				content = strings.ReplaceAll(content, "\n", "\r\n")
			case "literal":
				content = strings.ReplaceAll(content, "original", "modified")
			case "extra":
				write(actual, "extra.pb.go", original)
			case "wrong layout":
				name = "wrong.pb.go"
			}
			if scenario != "missing" {
				write(actual, name, content)
			}
			err := compareGeneratedBindings(expected, actual)
			shouldPass := scenario == "equal" || scenario == "line endings"
			if (err == nil) != shouldPass {
				test.Fatalf("unexpected parity result: %v", err)
			}
		})
	}
}

func TestPinnedGeneratedBindingsParity(test *testing.T) {
	if os.Getenv("KOGNITION_PROTO_ACCEPTANCE") != "true" {
		test.Skip("set KOGNITION_PROTO_ACCEPTANCE=true to regenerate pinned bindings in a temporary directory")
	}
	directory := test.TempDir()
	command := exec.Command("buf", "generate", "--output", directory)
	command.Dir = "../.."
	if output, err := command.CombinedOutput(); err != nil {
		test.Fatalf("pinned generation: %v\n%s", err, output)
	}
	if err := compareGeneratedBindings("../../proto/gen", filepath.Join(directory, "proto/gen")); err != nil {
		test.Fatal(err)
	}
}
