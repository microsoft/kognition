// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCleanBuildPreservesGeneratedBindings(test *testing.T) {
	if os.Getenv("KOGNITION_INSTALL_SHELL_ACCEPTANCE") != "true" {
		test.Skip("set KOGNITION_INSTALL_SHELL_ACCEPTANCE=true to execute both local shells")
	}
	powershell, err := exec.LookPath("pwsh")
	if err != nil {
		test.Fatal(err)
	}
	makeTool := os.Getenv("KOGNITION_INSTALL_MAKE")
	if makeTool == "" {
		makeTool, err = exec.LookPath("make")
		if err != nil {
			test.Fatal(err)
		}
	}
	for _, shell := range []string{"powershell", "make"} {
		test.Run(shell, func(test *testing.T) {
			directory := test.TempDir()
			write := func(path string, content []byte) {
				test.Helper()
				target := filepath.Join(directory, path)
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					test.Fatal(err)
				}
				if err := os.WriteFile(target, content, 0600); err != nil {
					test.Fatal(err)
				}
			}
			for _, path := range []string{"Makefile", "make.ps1"} {
				content, err := os.ReadFile(filepath.Join("../..", path))
				if err != nil {
					test.Fatal(err)
				}
				write(path, content)
			}
			write("go.mod", []byte("module fixture\n\ngo 1.26\n"))
			generated := []byte("package bindings\n\nconst Value = 42\n")
			write("proto/gen/bindings.go", generated)
			for _, name := range []string{"kognition-apiserver", "kognition-controller-manager", "kognitionctl", "kognition-intent-gateway", "kognition-memory-policy-gateway", "kognition-tool-gateway", "kognition-model-gateway", "kognition-agent-communication-gateway", "kognition-knowledge-gateway", "kognition-knowledge-azuresearch"} {
				write("cmd/"+name+"/main.go", []byte("package main\nimport (\"fixture/proto/gen\"; \"fmt\")\nfunc main() { fmt.Println(bindings.Value) }\n"))
			}
			write("bin/stale", []byte("old build"))
			run := func(target string) {
				test.Helper()
				var command *exec.Cmd
				if shell == "powershell" {
					command = exec.Command(powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", filepath.Join(directory, "make.ps1"), target)
				} else {
					arguments := []string{"--no-print-directory", "APP_VERSION=fixture", target}
					if bash := os.Getenv("KOGNITION_INSTALL_BASH"); bash != "" {
						arguments = append(arguments, "SHELL="+bash)
					}
					command = exec.Command(makeTool, arguments...)
				}
				command.Dir = directory
				command.Env = append(os.Environ(), "GOWORK=off")
				if output, err := command.CombinedOutput(); err != nil {
					test.Fatalf("%s: %v\n%s", target, err, output)
				}
			}
			run("clean")
			if _, err := os.Stat(filepath.Join(directory, "bin")); !os.IsNotExist(err) {
				test.Fatalf("clean retained build output: %v", err)
			}
			content, err := os.ReadFile(filepath.Join(directory, "proto/gen/bindings.go"))
			if err != nil || !bytes.Equal(content, generated) {
				test.Fatalf("clean changed generated bindings: %v", err)
			}
			run("build")
			binary := filepath.Join(directory, "bin/kognition-apiserver")
			if shell == "powershell" {
				binary += ".exe"
			} else if runtime.GOOS == "windows" {
				if err := os.Rename(binary, binary+".exe"); err != nil {
					test.Fatal(err)
				}
				binary += ".exe"
			}
			output, err := exec.Command(binary).CombinedOutput()
			if err != nil || string(bytes.TrimSpace(output)) != "42" {
				test.Fatalf("built command did not use preserved bindings: %v, %s", err, output)
			}
		})
	}
}
