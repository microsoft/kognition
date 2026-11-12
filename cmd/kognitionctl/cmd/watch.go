// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var watchCmd = &cobra.Command{
	Use:   "watch <resource>",
	Short: "Watch for changes to resources",
	Long:  `Stream real-time events for a resource type in a namespace.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runWatch,
}

func init() {
	rootCmd.AddCommand(watchCmd)
}

func runWatch(_ *cobra.Command, args []string) error {
	resource := args[0]

	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/watch/namespaces/%s/%s",
		serverURL, namespace, pluralize(resource))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create watch request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("watch request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server error (%d)", resp.StatusCode)
	}

	fmt.Fprintf(os.Stderr, "Watching %s in namespace %s...\n", pluralize(resource), namespace)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			fmt.Println(line)
			continue
		}

		eventType, _ := event["type"].(string)
		obj, _ := event["object"].(map[string]any)
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		kind, _ := obj["kind"].(string)

		switch outputFormat {
		case "json":
			out, _ := json.MarshalIndent(event, "", "  ")
			fmt.Println(string(out))
		case "yaml":
			out, _ := yaml.Marshal(event)
			fmt.Print(string(out))
			fmt.Println("---")
		default:
			fmt.Printf("%s\t%s/%s\n", eventType, kind, name)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("watch stream error: %w", err)
	}
	return nil
}
