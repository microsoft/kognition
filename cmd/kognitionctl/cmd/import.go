// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"
	strictjson "sigs.k8s.io/json"
)

type kagentImportSource struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Labels      map[string]string `yaml:"labels"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Description   string `yaml:"description"`
		SystemMessage string `yaml:"systemMessage"`
		Tools         []struct {
			Provider    string         `yaml:"provider"`
			Description string         `yaml:"description"`
			Config      map[string]any `yaml:"config"`
		} `yaml:"tools"`
	} `yaml:"spec"`
	Status map[string]any `yaml:"status"`
}

func init() {
	rootCmd.AddCommand(newImportCommand())
}

func newImportCommand() *cobra.Command {
	command := &cobra.Command{Use: "import", Short: "Review foreign workload compatibility offline", PersistentPreRunE: func(*cobra.Command, []string) error { return nil }}
	kagent := &cobra.Command{Use: "kagent", Short: "Review AutoGen-era kagent.dev/v1alpha1 Agent manifests"}
	var filename, expectedDigest, targetNamespace, modelName string
	var bindings map[string]string
	plan := &cobra.Command{Use: "plan -f PATH", Short: "Write a hash-bound compatibility plan without executing providers or contacting a server", Args: cobra.NoArgs}
	plan.Flags().StringVarP(&filename, "file", "f", "", "One YAML/JSON Agent file, or - for stdin")
	plan.Flags().StringVar(&expectedDigest, "sha256", "", "Required SHA-256 of exact source bytes")
	plan.Flags().StringVar(&targetNamespace, "target-namespace", "", "Explicit target workspace namespace")
	plan.Flags().StringVar(&modelName, "model", "", "Explicit operator-managed Kognition Model reference")
	plan.Flags().StringToStringVar(&bindings, "tool-binding", nil, "Explicit upstream-provider=Kognition-Tool mappings")
	plan.RunE = func(command *cobra.Command, _ []string) error {
		if filename == "" || expectedDigest == "" || targetNamespace == "" || modelName == "" {
			return errors.New("import plan requires --file, --sha256, --target-namespace and --model")
		}
		reader := command.InOrStdin()
		if filename != "-" {
			file, err := os.Open(filename)
			if err != nil {
				return fmt.Errorf("open import source: %w", err)
			}
			defer func() { _ = file.Close() }()
			reader = file
		}
		content, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
		if err != nil || len(content) > 1<<20 {
			return errors.New("import source is unreadable or exceeds 1 MiB")
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(content))
		if expectedDigest != digest {
			return errors.New("import source SHA-256 does not match reviewed bytes")
		}
		source, err := decodeKagentImportSource(content)
		if err != nil {
			return err
		}
		blockers := []string{"Provider semantics, tool capabilities and live grants require separate operator review; this plan is not an executable import."}
		if len(source.Spec.Tools) > 20 {
			blockers = append(blockers, "Source exceeds the pinned AutoGen-era Agent schema limit of 20 tools.")
		}
		tools := make([]map[string]any, 0, len(source.Spec.Tools))
		used := map[string]bool{}
		for index, tool := range source.Spec.Tools {
			if strings.TrimSpace(tool.Provider) == "" {
				return fmt.Errorf("source tool %d requires an explicit provider", index)
			}
			config, err := json.Marshal(tool.Config)
			if err != nil {
				return fmt.Errorf("source tool %d configuration cannot be fingerprinted", index)
			}
			binding := bindings[tool.Provider]
			if strings.TrimSpace(binding) == "" {
				blockers = append(blockers, fmt.Sprintf("Source tool %d requires an explicit operator Tool binding.", index))
			}
			used[tool.Provider] = true
			tools = append(tools, map[string]any{"index": index, "provider": tool.Provider, "configSHA256": fmt.Sprintf("%x", sha256.Sum256(config)), "targetTool": binding})
		}
		for provider := range bindings {
			if !used[provider] {
				return errors.New("tool binding refers to a provider absent from the reviewed source")
			}
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{
			"version": "kognition.dev/kagent-import-plan/v1", "sourceSHA256": digest,
			"sourceAPIVersion": source.APIVersion, "sourceName": source.Metadata.Name, "sourceNamespace": source.Metadata.Namespace,
			"targetNamespace": targetNamespace, "targetModel": modelName, "runtimeClass": "autogen",
			"systemMessage": source.Spec.SystemMessage, "tools": tools, "blockers": blockers,
			"readyForApply": false, "requiresGrantReview": true,
		})
	}
	kagent.AddCommand(plan, newKagentRenderCommand())
	command.AddCommand(kagent)
	return command
}

func decodeKagentImportSource(content []byte) (kagentImportSource, error) {
	var source kagentImportSource
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&source); err != nil {
		return source, fmt.Errorf("decode import source: %w", err)
	}
	for {
		var extra yaml.Node
		if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
			break
		} else if err != nil || len(extra.Content) != 1 || extra.Content[0].Tag != "!!null" || extra.Content[0].Value != "" {
			return source, errors.New("import exactly one Agent document")
		}
	}
	if source.APIVersion != "kagent.dev/v1alpha1" || source.Kind != "Agent" || source.Metadata.Name == "" || strings.TrimSpace(source.Spec.SystemMessage) == "" {
		return source, errors.New("import requires a named kagent.dev/v1alpha1 Agent with a systemMessage")
	}
	return source, nil
}

type kagentToolReview struct {
	Index                int    `json:"index"`
	Provider             string `json:"provider"`
	ConfigSHA256         string `json:"configSHA256"`
	TargetTool           string `json:"targetTool"`
	RuntimeName          string `json:"runtimeName"`
	SchemaSHA256         string `json:"schemaSHA256"`
	ImplementationSHA256 string `json:"implementationSHA256"`
}

type kagentImportReview struct {
	Version         string             `json:"version"`
	Decision        string             `json:"decision"`
	Approver        string             `json:"approver"`
	SourceSHA256    string             `json:"sourceSHA256"`
	TargetNamespace string             `json:"targetNamespace"`
	TargetModel     string             `json:"targetModel"`
	Tools           []kagentToolReview `json:"tools"`
}

func newKagentRenderCommand() *cobra.Command {
	var filename, expectedSource, reviewFile, expectedReview string
	command := &cobra.Command{Use: "render -f PATH", Short: "Render only an Agent from exact operator-reviewed source and tool mappings", Args: cobra.NoArgs}
	command.Flags().StringVarP(&filename, "file", "f", "", "Exact reviewed source, or - for stdin")
	command.Flags().StringVar(&expectedSource, "sha256", "", "Exact source SHA-256")
	command.Flags().StringVar(&reviewFile, "review", "", "Strict operator review JSON")
	command.Flags().StringVar(&expectedReview, "review-sha256", "", "Exact review SHA-256")
	command.RunE = func(command *cobra.Command, _ []string) error {
		if filename == "" || expectedSource == "" || reviewFile == "" || expectedReview == "" {
			return errors.New("render requires exact source and operator review files and SHA-256 values")
		}
		read := func(filename string, reader io.Reader) ([]byte, error) {
			if filename != "-" {
				file, err := os.Open(filename)
				if err != nil {
					return nil, err
				}
				defer func() { _ = file.Close() }()
				reader = file
			}
			content, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
			if err != nil || len(content) > 1<<20 {
				return nil, errors.New("reviewed import input exceeds 1 MiB or is unreadable")
			}
			return content, nil
		}
		content, err := read(filename, command.InOrStdin())
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(content)) != expectedSource {
			return errors.New("source SHA-256 differs from review")
		}
		source, err := decodeKagentImportSource(content)
		if err != nil {
			return err
		}
		encoded, err := read(reviewFile, nil)
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(encoded)) != expectedReview {
			return errors.New("operator review SHA-256 differs")
		}
		var review kagentImportReview
		strictErrors, err := strictjson.UnmarshalStrict(encoded, &review)
		if err != nil || len(strictErrors) != 0 {
			return fmt.Errorf("invalid operator review: %w", errors.Join(append(strictErrors, err)...))
		}
		if review.Version != "kognition.dev/kagent-import-review/v1" || review.Decision != "Approved" || strings.TrimSpace(review.Approver) == "" || review.SourceSHA256 != expectedSource || len(validation.IsDNS1123Label(review.TargetNamespace)) != 0 || len(validation.IsDNS1123Subdomain(review.TargetModel)) != 0 || len(validation.IsDNS1123Subdomain(source.Metadata.Name)) != 0 || len(source.Spec.Tools) > 20 || len(review.Tools) != len(source.Spec.Tools) {
			return errors.New("operator review does not authorize this source, target or complete tool set")
		}
		validDigest := func(value string) bool {
			decoded, err := hex.DecodeString(value)
			return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
		}
		validName := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
		tools := make([]map[string]any, 0, len(review.Tools))
		aliases := map[string]string{}
		seen := map[string]bool{}
		for index, tool := range source.Spec.Tools {
			binding := review.Tools[index]
			config, err := json.Marshal(tool.Config)
			if err != nil {
				return err
			}
			if binding.Index != index || binding.Provider != tool.Provider || strings.TrimSpace(tool.Provider) == "" || binding.ConfigSHA256 != fmt.Sprintf("%x", sha256.Sum256(config)) || len(validation.IsDNS1123Subdomain(binding.TargetTool)) != 0 || !validName.MatchString(binding.RuntimeName) || !validDigest(binding.SchemaSHA256) || !validDigest(binding.ImplementationSHA256) || aliases[binding.TargetTool] != "" || seen[binding.RuntimeName] {
				return fmt.Errorf("operator review tool %d differs from exact source or lacks reviewed semantics", index)
			}
			aliases[binding.TargetTool], seen[binding.RuntimeName] = binding.RuntimeName, true
			tools = append(tools, map[string]any{"ref": map[string]string{"name": binding.TargetTool}, "required": true})
		}
		aliasJSON, err := json.Marshal(aliases)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(map[string]any{
			"apiVersion": "kognition.dev/v1alpha1", "kind": "Agent",
			"metadata": map[string]any{"name": source.Metadata.Name, "namespace": review.TargetNamespace, "annotations": map[string]string{"kognition.dev/kagent-source-sha256": expectedSource, "kognition.dev/kagent-review-sha256": expectedReview, "kognition.dev/kagent-tool-aliases": string(aliasJSON)}},
			"spec":     map[string]any{"modelRef": map[string]string{"name": review.TargetModel}, "instructions": map[string]string{"inline": source.Spec.SystemMessage}, "tools": tools, "runtime": map[string]string{"class": "autogen"}},
		})
	}
	return command
}
