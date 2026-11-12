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

var approveCmd = &cobra.Command{
	Use:   "approve <approval-id> --decision <approve|deny> [--reason <reason>]",
	Short: "Submit an approval decision",
	Long:  `Submit an approve or deny decision for a pending approval request.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runApprove,
}

var approvalDecision string
var approvalReason string

func init() {
	approveCmd.Flags().StringVar(&approvalDecision, "decision", "", "Decision: approve or deny (required)")
	approveCmd.Flags().StringVar(&approvalReason, "reason", "", "Reason for the decision")
	_ = approveCmd.MarkFlagRequired("decision")
	rootCmd.AddCommand(approveCmd)
}

func runApprove(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	approvalID := args[0]

	if approvalDecision != "approve" && approvalDecision != "deny" {
		return fmt.Errorf("decision must be 'approve' or 'deny', got %q", approvalDecision)
	}

	payload := map[string]string{
		"decision": approvalDecision,
	}
	if approvalReason != "" {
		payload["reason"] = approvalReason
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/approvals/%s/decision", serverURL, approvalID)
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

	fmt.Printf("approval %s: decision %q submitted\n", approvalID, approvalDecision)
	return nil
}
