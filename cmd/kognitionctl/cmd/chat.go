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
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var chatSessionID string

var chatCmd = &cobra.Command{
	Use:   "chat <agent-name> <message...>",
	Short: "Send a message to an Agent",
	Long:  `Send a chat message to a named Agent and receive a response.`,
	Args:  cobra.MinimumNArgs(2),
	RunE:  runChat,
}

func init() {
	chatCmd.Flags().StringVar(&chatSessionID, "session", "", "Session ID for multi-turn conversation")
	rootCmd.AddCommand(chatCmd)
}

func runChat(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	if cmd != nil {
		ctx = cmd.Context()
	}
	agentName := args[0]
	message := strings.Join(args[1:], " ")

	if k8sMode {
		return runChatK8s(cmd, agentName, message)
	}
	return runChatStandalone(ctx, agentName, message)
}

// runChatStandalone sends a synchronous chat request to the kognition-apiserver.
func runChatStandalone(ctx context.Context, agentName, message string) error {
	payload := map[string]string{
		"message": message,
	}
	if chatSessionID != "" {
		payload["sessionID"] = chatSessionID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/agents/%s/chat",
		serverURL, namespace, agentName)
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

	var result struct {
		SessionID string `json:"sessionID"`
		Response  string `json:"response"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Println(string(respBody))
		return nil
	}
	if result.SessionID != "" && chatSessionID == "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", result.SessionID)
	}
	if result.Response != "" {
		fmt.Println(result.Response)
	} else {
		fmt.Println(string(respBody))
	}
	return nil
}

// runChatK8s routes the turn through the AgentInstance controller so the
// runtime receives the same scoped capabilities as production invocations.
func runChatK8s(cmd *cobra.Command, agentName, message string) error {
	result, err := invokeInteractiveAgentK8s(cmd.Context(), namespace, agentName, message, chatSessionID, "cli")
	if err != nil {
		return err
	}
	if outputFormat == "json" || outputFormat == "yaml" {
		out := map[string]any{
			"output":          result.Output,
			"sessionID":       result.SessionID,
			"conversationID":  chatSessionID,
			"toolCalls":       result.ToolCalls,
			"toolCallsDetail": result.ToolCallsDetail,
		}
		if result.TokenUsage != nil {
			out["usage"] = result.TokenUsage
		}
		jsonOut, _ := json.Marshal(out)
		return printOutput(jsonOut)
	}
	fmt.Println(result.Output)
	return nil
}

// runChatRuntimeDirect drives the runtime's async invoke/status loop.
func runChatRuntimeDirect(cmd *cobra.Command, agentName, message string) error {
	ctx := cmd.Context()
	sessionID := fmt.Sprintf("cli-%d", time.Now().UnixNano())
	conversationID := chatSessionID // reuse --session as conversation ID for multi-turn

	invokePayload, _ := json.Marshal(map[string]string{
		"message":         message,
		"session_id":      sessionID,
		"conversation_id": conversationID,
	})

	baseURL, err := resolveAgentBaseURL(ctx, namespace, agentName)
	if err != nil {
		return fmt.Errorf("resolve agent URL: %w", err)
	}
	authorization, err := resolveAgentRuntimeAuthorization(ctx, namespace, agentName)
	if err != nil {
		return err
	}
	invokeURL := baseURL + "/invoke"
	statusURL := baseURL + "/status"

	// 1. Submit the invoke, retrying through 409 (runtime busy).
	invokeDeadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, invokeURL, bytes.NewReader(invokePayload))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("invoke agent: %w", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusConflict {
			if time.Now().After(invokeDeadline) {
				return fmt.Errorf("agent is still processing a previous turn; please wait and try again")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode >= 400 {
			return fmt.Errorf("agent returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		break
	}

	// 2. Poll /status until our session completes or fails.
	// If the port-forward tunnel dies (pod replaced), try to re-establish once.
	pollDeadline := time.Now().Add(5 * time.Minute)
	tunnelRetried := false
	var st struct {
		Phase           string           `json:"phase"`
		SessionID       string           `json:"session_id"`
		TokenUsage      map[string]any   `json:"token_usage"`
		ToolCalls       int              `json:"tool_calls"`
		ToolCallsDetail []map[string]any `json:"tool_calls_detail"`
		Output          string           `json:"output"`
		Error           string           `json:"error"`
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return fmt.Errorf("build status request: %w", err)
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			// Connection error — the port-forward tunnel may have died.
			// Try to re-establish it once.
			if !tunnelRetried {
				tunnelRetried = true
				invalidatePortForward(namespace, agentName)
				newBase, pfErr := resolveAgentBaseURL(ctx, namespace, agentName)
				if pfErr == nil {
					statusURL = newBase + "/status"
				}
			}
			if time.Now().After(pollDeadline) {
				return fmt.Errorf("status poll failed: %w", err)
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return fmt.Errorf("status returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if err := json.Unmarshal(body, &st); err != nil {
			continue
		}
		if st.SessionID != sessionID {
			if time.Now().After(pollDeadline) {
				return fmt.Errorf("timed out waiting for agent to start our turn")
			}
			continue
		}
		switch st.Phase {
		case "Completed":
			if outputFormat == "json" || outputFormat == "yaml" {
				out := map[string]any{
					"output":          st.Output,
					"sessionID":       st.SessionID,
					"conversationID":  conversationID,
					"toolCalls":       st.ToolCalls,
					"toolCallsDetail": st.ToolCallsDetail,
				}
				if st.TokenUsage != nil {
					out["usage"] = st.TokenUsage
				}
				jsonOut, _ := json.Marshal(out)
				return printOutput(jsonOut)
			}
			fmt.Println(st.Output)
			return nil
		case "Failed":
			msg := st.Error
			if msg == "" {
				msg = "agent run failed"
			}
			return fmt.Errorf("%s", msg)
		}
		if time.Now().After(pollDeadline) {
			return fmt.Errorf("timed out waiting for agent response")
		}
	}
}
