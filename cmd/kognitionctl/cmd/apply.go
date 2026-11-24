// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var applyCmd = &cobra.Command{
	Use:   "apply -f <file|->",
	Short: "Apply a resource definition to the Kognition API server",
	Long:  "Apply a resource definition from a file or stdin (use -f - to read from stdin).",
	RunE:  runApply,
}

var applyFile string

func init() {
	applyCmd.Flags().StringVarP(&applyFile, "file", "f", "", "Path to resource definition file (YAML or JSON), or '-' for stdin")
	rootCmd.AddCommand(applyCmd)
}

func runApply(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	var data []byte
	var err error

	switch {
	case applyFile == "-":
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
	case applyFile != "":
		data, err = os.ReadFile(applyFile)
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
	default:
		// No -f flag — try stdin if it's piped (not a terminal).
		stat, _ := os.Stdin.Stat()
		if stat.Mode()&os.ModeCharDevice == 0 {
			data, err = io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
		} else {
			return fmt.Errorf("must specify -f <file> or pipe input via stdin")
		}
	}

	if len(data) == 0 {
		return fmt.Errorf("empty input")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	applied := 0
	for document := 1; ; document++ {
		var resource map[string]any
		if err := decoder.Decode(&resource); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("parse resource document %d: %w", document, err)
		}
		if len(resource) == 0 {
			continue
		}
		if err := applyResource(ctx, resource); err != nil {
			return fmt.Errorf("apply resource document %d: %w", document, err)
		}
		applied++
	}
	if applied == 0 {
		return fmt.Errorf("input contains no resources")
	}
	return nil
}

func applyResource(ctx context.Context, resource map[string]any) error {
	if rawVersion, present := resource["apiVersion"]; present {
		version, valid := rawVersion.(string)
		if !valid || version != "kognition.dev/v1alpha1" {
			return fmt.Errorf("unsupported apiVersion %v: apply accepts kognition.dev/v1alpha1 resources; foreign manifests require an explicit compatibility import", rawVersion)
		}
	}

	// Extract kind, metadata.namespace, metadata.name.
	kind, _ := resource["kind"].(string)
	metadata, _ := resource["metadata"].(map[string]any)
	targetNamespace, _ := metadata["namespace"].(string)
	name, _ := metadata["name"].(string)

	if kind == "" || name == "" {
		return fmt.Errorf("resource must have kind and metadata.name")
	}
	if targetNamespace == "" {
		targetNamespace = namespace
		if targetNamespace == "" {
			targetNamespace = "default"
		}
	}

	// --- K8s mode: use client-go dynamic client ---
	if k8sMode {
		// Try Get to detect if it already exists.
		existing, getErr := k8sGet(targetNamespace, kind, name)
		if getErr == nil && len(existing) > 0 {
			// Exists → inject resourceVersion and Update.
			var existObj map[string]any
			if err := json.Unmarshal(existing, &existObj); err == nil {
				if eMeta, ok := existObj["metadata"].(map[string]any); ok {
					if rv, ok := eMeta["resourceVersion"]; ok {
						metadata["resourceVersion"] = rv
						resource["metadata"] = metadata
					}
				}
			}
			if _, err := k8sUpdate(targetNamespace, resource); err != nil {
				return err
			}
		} else {
			// Does not exist → Create.
			if _, err := k8sCreate(targetNamespace, resource); err != nil {
				return err
			}
		}
		fmt.Printf("%s/%s applied\n", strings.ToLower(kind), name)
		return nil
	}

	// --- Standalone mode: HTTP API ---
	jsonData, err := json.Marshal(resource)
	if err != nil {
		return fmt.Errorf("marshal resource: %w", err)
	}

	// Build URL — pluralize kind using the known-resources map.
	plural := pluralize(kind)
	resourceURL := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/%s/%s",
		serverURL, targetNamespace, plural, name)

	// GET to determine if the resource already exists; if so, inject its
	// current resourceVersion into the body for optimistic concurrency.
	var resp *http.Response
	getResp, err := doHTTP(ctx, "GET", resourceURL, nil)
	if err != nil {
		return err
	}
	getBody, _ := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()

	if getResp.StatusCode == http.StatusOK {
		// Resource exists — extract resourceVersion and PUT.
		var existing map[string]any
		if err := json.Unmarshal(getBody, &existing); err != nil {
			return fmt.Errorf("parse existing resource: %w", err)
		}
		if existMeta, ok := existing["metadata"].(map[string]any); ok {
			if rv, ok := existMeta["resourceVersion"]; ok {
				metadata["resourceVersion"] = rv
				resource["metadata"] = metadata
				jsonData, err = json.Marshal(resource)
				if err != nil {
					return fmt.Errorf("marshal resource: %w", err)
				}
			}
		}
		resp, err = doHTTP(ctx, "PUT", resourceURL, jsonData)
		if err != nil {
			return err
		}
	} else if getResp.StatusCode == http.StatusNotFound {
		// Resource does not exist — POST to create.
		createURL := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/%s",
			serverURL, targetNamespace, plural)
		resp, err = doHTTP(ctx, "POST", createURL, jsonData)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("server error (%d): %s", getResp.StatusCode, string(getBody))
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server error (%d): %s", resp.StatusCode, string(body))
	}

	fmt.Printf("%s/%s applied\n", strings.ToLower(kind), name)
	return nil
}

func doHTTP(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return httpClient.Do(req)
}
