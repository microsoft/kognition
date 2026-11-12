// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

var goalCmd = &cobra.Command{
	Use:   "goal <intent-gateway-name> <goal-text>",
	Short: "Submit a goal to an IntentGateway",
	Long:  `Submit a goal (natural language instruction) to a named IntentGateway for processing.`,
	Args:  cobra.MinimumNArgs(2),
	RunE:  runGoal,
}

func init() {
	rootCmd.AddCommand(goalCmd)
}

func runGoal(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	gatewayName := args[0]
	goalText := strings.Join(args[1:], " ")

	payload := map[string]string{
		"goal": goalText,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/intentgateways/%s/goals",
		serverURL, namespace, gatewayName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("server error (%d): %s", resp.StatusCode, string(respBody))
	}

	if outputFormat == "json" || outputFormat == "yaml" {
		return printOutput(respBody)
	}

	fmt.Printf("goal submitted to intentgateway %q\n", gatewayName)
	return nil
}
