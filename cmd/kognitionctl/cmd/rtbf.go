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

	"github.com/spf13/cobra"
)

var rtbfCmd = &cobra.Command{
	Use:   "rtbf <memory-record-name>",
	Short: "Invoke Right To Be Forgotten on a memory record",
	Long:  `Submit a Right To Be Forgotten (RTBF) request to erase a specific memory record.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runRTBF,
}

func init() {
	rootCmd.AddCommand(rtbfCmd)
}

func runRTBF(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	name := args[0]

	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/memoryrecords/%s/rtbf",
		serverURL, namespace, name)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
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
		var data json.RawMessage
		if json.Unmarshal(respBody, &data) == nil && len(respBody) > 2 {
			return printOutput(respBody)
		}
	}

	fmt.Printf("RTBF request submitted for memory record %q\n", name)
	return nil
}
