// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestInstallationEntrypointCommands(test *testing.T) {
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
	root, err := filepath.Abs("../..")
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	source := filepath.Join(directory, "recorder.go")
	if err := os.WriteFile(source, []byte(`package main
import ("encoding/json"; "os"; "path/filepath"; "strconv")
func main() {
 file, err := os.OpenFile(os.Getenv("INSTALL_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
 if err != nil { panic(err) }
 if err := json.NewEncoder(file).Encode(append([]string{filepath.Base(os.Args[0])}, os.Args[1:]...)); err != nil { panic(err) }
 if err := file.Close(); err != nil { panic(err) }
 code, err := strconv.Atoi(os.Getenv("INSTALL_EXIT")); if err != nil { panic(err) }; os.Exit(code)
}
`), 0600); err != nil {
		test.Fatal(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	recorder := filepath.Join(directory, "go"+suffix)
	build := exec.Command("go", "build", "-o", recorder, source)
	if output, err := build.CombinedOutput(); err != nil {
		test.Fatalf("build recorder: %v\n%s", err, output)
	}
	binary, err := os.ReadFile(recorder)
	if err != nil {
		test.Fatal(err)
	}
	for _, tool := range []string{"helm", "kubectl", "docker", "kind", "az", "azd", "curl"} {
		if err := os.WriteFile(filepath.Join(directory, tool+suffix), binary, 0700); err != nil {
			test.Fatal(err)
		}
	}
	for _, shell := range []string{"powershell", "make"} {
		for _, scenario := range []struct {
			name, plan, frozen, exit string
			invoked, success         bool
		}{
			{name: "missing-plan", frozen: "true", exit: "0"},
			{name: "missing-freeze", plan: "reviewed plan.json", exit: "0"},
			{name: "bootstrap", plan: "reviewed plan.json", frozen: "true", exit: "0", invoked: true, success: true},
			{name: "bootstrap-failure", plan: "reviewed plan.json", frozen: "true", exit: "17", invoked: true},
		} {
			test.Run(shell+"/"+scenario.name, func(test *testing.T) {
				record := filepath.Join(test.TempDir(), "commands.jsonl")
				var command *exec.Cmd
				arguments := []string{"deploy-kubernetes", "INSTALL_PLAN=" + scenario.plan, "INSTALL_WRITERS_FROZEN=" + scenario.frozen}
				if shell == "powershell" {
					command = exec.Command(powershell, append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-File", filepath.Join(root, "make.ps1")}, arguments...)...)
				} else {
					arguments = append([]string{"--no-print-directory", "-f", filepath.Join(root, "Makefile"), "GOCMD=go", "APP_VERSION=acceptance"}, arguments...)
					if bash := os.Getenv("KOGNITION_INSTALL_BASH"); bash != "" {
						arguments = append(arguments, "SHELL="+bash)
					}
					command = exec.Command(makeTool, arguments...)
				}
				command.Dir = root
				command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "INSTALL_RECORD="+record, "INSTALL_EXIT="+scenario.exit)
				output, runErr := command.CombinedOutput()
				if (runErr == nil) != scenario.success {
					test.Fatalf("exit mismatch: %v\n%s", runErr, output)
				}
				content, err := os.ReadFile(record)
				if !scenario.invoked {
					if !os.IsNotExist(err) {
						test.Fatalf("effects before prerequisites: %s, %v", content, err)
					}
					return
				}
				if err != nil {
					test.Fatalf("no recorded invocation: %v\n%s", err, output)
				}
				var actual []string
				if err := json.Unmarshal(content, &actual); err != nil {
					test.Fatalf("expected exactly one invocation: %s, %v", content, err)
				}
				expected := []string{"go" + suffix, "run", "./tools/kognition-install", "bootstrap", "--plan", scenario.plan, "--writers-frozen"}
				if !reflect.DeepEqual(actual, expected) {
					test.Fatalf("unexpected commands: %v", actual)
				}
			})
		}
	}
}

func TestInstallationEntrypointsRequireSafeWorkflow(test *testing.T) {
	for _, path := range []string{"../../make.ps1", "../../Makefile"} {
		content, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		text := string(content)
		start, end := "function Target-DeployKubernetes {", "function Target-UndeployKubernetes"
		if strings.HasSuffix(path, "Makefile") {
			start, end = "deploy-kubernetes:", "## cert-manager:"
		}
		_, block, found := strings.Cut(text, start)
		if !found {
			test.Fatalf("deployment entry point missing in %s", path)
		}
		block, _, _ = strings.Cut(block, end)
		if !strings.Contains(block, "kognition-install") {
			test.Errorf("%s bypasses the checked installation workflow", path)
		}
		for _, forbidden := range []string{"cert-manager kind-load", "Ensure-CertManager", "Target-KindLoad", "Target-FoundryCreds", "foundry-creds", "Refresh-AgentRuntimes", "rollout", "helm upgrade"} {
			if strings.Contains(block, forbidden) {
				test.Errorf("%s performs unchecked deployment effect %q", path, forbidden)
			}
		}
	}
}
