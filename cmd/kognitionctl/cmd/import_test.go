// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestKagentImportPlanIsOfflineAndPreservesPrompt(test *testing.T) {
	input := "apiVersion: kagent.dev/v1alpha1\nkind: Agent\nmetadata: {name: expert, namespace: kagent}\nspec:\n  systemMessage: |\n    Keep the source prompt.\n  tools:\n    - provider: upstream.Command\n      config: {api_key: fixture-sensitive, code: never-execute}\n"
	contacts := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { contacts++ }))
	defer server.Close()
	root := &cobra.Command{Use: "test", PersistentPreRunE: func(*cobra.Command, []string) error { contacts++; return fmt.Errorf("must not initialize clients") }}
	root.AddCommand(newImportCommand())
	root.SetArgs([]string{"import", "kagent", "plan", "-f", "-", "--sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(input))), "--target-namespace", "tenant", "--model", "reviewed-model", "--tool-binding", "upstream.Command=reviewed-tool"})
	root.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		test.Fatal(err)
	}
	var plan struct {
		SystemMessage string                                                `json:"systemMessage"`
		ReadyForApply bool                                                  `json:"readyForApply"`
		Tools         []struct{ Provider, TargetTool, ConfigSHA256 string } `json:"tools"`
	}
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		test.Fatal(err)
	}
	if contacts != 0 || plan.ReadyForApply || plan.SystemMessage != "Keep the source prompt.\n" || len(plan.Tools) != 1 || plan.Tools[0].TargetTool != "reviewed-tool" || len(plan.Tools[0].ConfigSHA256) != 64 {
		test.Fatal("offline planning or exact source preservation contract failed")
	}
	if strings.Contains(output.String(), "fixture-sensitive") || strings.Contains(output.String(), "never-execute") {
		test.Fatal("opaque provider configuration leaked into plan")
	}
}

func TestKagentReviewedRenderPreservesSourceWithoutGrantCreation(test *testing.T) {
	input := "apiVersion: kagent.dev/v1alpha1\nkind: Agent\nmetadata: {name: observability-agent}\nspec:\n  systemMessage: |\n    Preserve the upstream prompt.\n  tools:\n    - provider: kagent.tools.prometheus.QueryTool\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(input)))
	review := map[string]any{
		"version": "kognition.dev/kagent-import-review/v1", "decision": "Approved", "approver": "fixture-operator",
		"sourceSHA256": digest, "targetNamespace": "fixture", "targetModel": "reviewed-model",
		"tools": []any{map[string]any{"index": 0, "provider": "kagent.tools.prometheus.QueryTool", "configSHA256": fmt.Sprintf("%x", sha256.Sum256([]byte("null"))), "targetTool": "query-tool", "runtimeName": "QueryTool", "schemaSHA256": strings.Repeat("a", 64), "implementationSHA256": strings.Repeat("b", 64)}},
	}
	encoded, err := json.Marshal(review)
	if err != nil {
		test.Fatal(err)
	}
	filename := filepath.Join(test.TempDir(), "review.json")
	if err := os.WriteFile(filename, encoded, 0o600); err != nil {
		test.Fatal(err)
	}
	contacts := 0
	root := &cobra.Command{Use: "test", PersistentPreRunE: func(*cobra.Command, []string) error {
		contacts++
		return fmt.Errorf("client initialization prohibited")
	}}
	root.AddCommand(newImportCommand())
	root.SetArgs([]string{"import", "kagent", "render", "-f", "-", "--sha256", digest, "--review", filename, "--review-sha256", fmt.Sprintf("%x", sha256.Sum256(encoded))})
	root.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		test.Fatal(err)
	}
	var resource map[string]any
	if err := json.Unmarshal(output.Bytes(), &resource); err != nil {
		test.Fatal(err)
	}
	spec := resource["spec"].(map[string]any)
	tools := spec["tools"].([]any)
	if contacts != 0 || resource["apiVersion"] != "kognition.dev/v1alpha1" || resource["kind"] != "Agent" || spec["instructions"].(map[string]any)["inline"] != "Preserve the upstream prompt.\n" || len(tools) != 1 || tools[0].(map[string]any)["required"] != true {
		test.Fatal("reviewed offline render changed the source or contacted a server")
	}
	if strings.Contains(output.String(), "ModelGrant") || strings.Contains(output.String(), "AgentPolicy") || strings.Contains(output.String(), "fixture-operator") {
		test.Fatal("render created authority or included review identity as executable configuration")
	}
	for _, scenario := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"rejected", func(value map[string]any) { value["decision"] = "Rejected" }},
		{"wrong source", func(value map[string]any) { value["sourceSHA256"] = strings.Repeat("0", 64) }},
		{"missing identity", func(value map[string]any) { value["approver"] = "" }},
		{"unknown field", func(value map[string]any) { value["executeProviders"] = true }},
		{"missing tools", func(value map[string]any) { value["tools"] = []any{} }},
		{"wrong provider", func(value map[string]any) {
			value["tools"].([]any)[0].(map[string]any)["provider"] = "unreviewed.Provider"
		}},
		{"wrong config", func(value map[string]any) {
			value["tools"].([]any)[0].(map[string]any)["configSHA256"] = strings.Repeat("0", 64)
		}},
		{"bad alias", func(value map[string]any) { value["tools"].([]any)[0].(map[string]any)["runtimeName"] = "bad/name" }},
		{"bad target", func(value map[string]any) { value["tools"].([]any)[0].(map[string]any)["targetTool"] = "../tool" }},
		{"missing implementation", func(value map[string]any) { value["tools"].([]any)[0].(map[string]any)["implementationSHA256"] = "" }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			var changed map[string]any
			if err := json.Unmarshal(encoded, &changed); err != nil {
				test.Fatal(err)
			}
			scenario.edit(changed)
			altered, err := json.Marshal(changed)
			if err != nil {
				test.Fatal(err)
			}
			assertKagentRenderRejected(test, input, altered)
		})
	}
	test.Run("duplicate decision", func(test *testing.T) {
		assertKagentRenderRejected(test, input, []byte(strings.TrimSuffix(string(encoded), "}")+`,"decision":"Approved"}`))
	})
}

func assertKagentRenderRejected(test *testing.T, source string, review []byte) {
	test.Helper()
	filename := filepath.Join(test.TempDir(), "review.json")
	if err := os.WriteFile(filename, review, 0o600); err != nil {
		test.Fatal(err)
	}
	command := newImportCommand()
	command.SetArgs([]string{"kagent", "render", "-f", "-", "--sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(source))), "--review", filename, "--review-sha256", fmt.Sprintf("%x", sha256.Sum256(review))})
	command.SetIn(strings.NewReader(source))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SilenceUsage = true
	if err := command.Execute(); err == nil || output.Len() != 0 {
		test.Fatal("unapproved or ambiguous review rendered an Agent")
	}
}

func TestKagentImportPlanRejectsAmbiguousOrUnreviewedInputs(test *testing.T) {
	valid := "apiVersion: kagent.dev/v1alpha1\nkind: Agent\nmetadata: {name: expert}\nspec: {systemMessage: unchanged}\n"
	for _, scenario := range []struct{ name, input, digest string }{
		{"hash mismatch", valid, strings.Repeat("0", 64)},
		{"foreign API", strings.Replace(valid, "kagent.dev", "foreign.dev", 1), ""},
		{"new schema", strings.Replace(valid, "{systemMessage: unchanged}", "{systemMessage: unchanged, declarative: true}", 1), ""},
		{"duplicate", strings.Replace(valid, "{name: expert}", "{name: first, name: second}", 1), ""},
		{"multiple", valid + "---\n" + valid, ""},
		{"size", strings.Repeat("x", (1<<20)+1), ""},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			command := newImportCommand()
			digest := scenario.digest
			if digest == "" {
				digest = fmt.Sprintf("%x", sha256.Sum256([]byte(scenario.input)))
			}
			command.SetArgs([]string{"kagent", "plan", "-f", "-", "--sha256", digest, "--target-namespace", "tenant", "--model", "model"})
			command.SetIn(strings.NewReader(scenario.input))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&bytes.Buffer{})
			command.SilenceUsage = true
			if err := command.Execute(); err == nil || output.Len() != 0 {
				test.Fatal("invalid import source produced a plan")
			}
		})
	}
}

func TestKagentImportPlanReportsSourceLimitsWithoutDroppingTools(test *testing.T) {
	input := "apiVersion: kagent.dev/v1alpha1\nkind: Agent\nmetadata: {name: expert}\nspec:\n  systemMessage: unchanged\n  tools:\n"
	for index := 0; index < 21; index++ {
		input += fmt.Sprintf("    - provider: upstream.Tool%d\n", index)
	}
	input += "status: {}\n---\n"
	command := newImportCommand()
	command.SetArgs([]string{"kagent", "plan", "-f", "-", "--sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(input))), "--target-namespace", "tenant", "--model", "model"})
	command.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		test.Fatal(err)
	}
	var plan struct {
		ReadyForApply bool              `json:"readyForApply"`
		Blockers      []string          `json:"blockers"`
		Tools         []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		test.Fatal(err)
	}
	if plan.ReadyForApply || len(plan.Tools) != 21 || len(plan.Blockers) != 23 || !strings.Contains(strings.Join(plan.Blockers, " "), "limit of 20") {
		test.Fatal("invalid source was silently truncated or certified")
	}
}
