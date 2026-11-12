// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bedrock

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

const driverName = "aws-bedrock"

// Driver implements the CPI for AWS Bedrock's Converse API.
type Driver struct {
	client *http.Client
}

// New creates an AWS Bedrock CPI driver.
func New(client *http.Client) *Driver {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Driver{client: client}
}

func (d *Driver) Name() string { return driverName }

func (d *Driver) Validate(_ context.Context, model *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	if model.ModelID == "" {
		return fmt.Errorf("aws-bedrock: modelID is required (e.g., anthropic.claude-3-haiku-20240307-v1:0)")
	}
	if model.Endpoint == "" {
		return fmt.Errorf("aws-bedrock: endpoint is required (e.g., https://bedrock-runtime.us-east-1.amazonaws.com)")
	}
	return nil
}

func (d *Driver) Probe(ctx context.Context, model *v1alpha1.ModelSpec, creds *cpi.Credentials) (*cpi.HealthResult, error) {
	start := time.Now()

	// Send a minimal Converse request to validate connectivity and auth.
	converseURL := fmt.Sprintf("%s/model/%s/converse", model.Endpoint, model.ModelID)
	body := converseRequest{
		Messages: []converseMessage{
			{Role: "user", Content: []converseContent{{Text: stringPtr("hi")}}},
		},
		InferenceConfig: &inferenceConfig{MaxTokens: intPtr(1)},
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, converseURL, bytes.NewReader(payload))
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Reason:    "RequestBuildFailed",
			Message:   err.Error(),
		}, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	signRequest(req, payload, creds)

	resp, err := d.client.Do(req)
	if err != nil {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointUnreachable",
			Message:   err.Error(),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "AuthenticationFailed",
			Message:   fmt.Sprintf("HTTP %d from Bedrock", resp.StatusCode),
		}, nil
	}

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusBadRequest {
		return &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointError",
			Message:   fmt.Sprintf("HTTP %d from Bedrock", resp.StatusCode),
		}, nil
	}

	return &cpi.HealthResult{
		Available:    true,
		Latency:      time.Since(start).String(),
		Reason:       "Healthy",
		Message:      "Bedrock endpoint reachable",
		Capabilities: []string{"chat"},
	}, nil
}

func (d *Driver) Complete(ctx context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	prepared, err := cpi.PrepareCompletion(req, d)
	if err != nil {
		return nil, fmt.Errorf("aws-bedrock: invocation: %w", err)
	}
	req = prepared
	converseURL := fmt.Sprintf("%s/model/%s/converse", req.Endpoint, req.Model)

	body := buildConverseRequest(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aws-bedrock: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, converseURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("aws-bedrock: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	signRequest(httpReq, payload, req.Creds)

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("aws-bedrock: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("aws-bedrock: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var converseResp converseResponse
	if err := json.NewDecoder(resp.Body).Decode(&converseResp); err != nil {
		return nil, fmt.Errorf("aws-bedrock: decode response: %w", err)
	}

	return parseConverseResponse(&converseResp), nil
}

func (d *Driver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	// Bedrock embedding uses InvokeModel with model-specific payloads.
	// Not implemented in the generic Converse path.
	return nil, fmt.Errorf("aws-bedrock: embedding via Converse API not supported; use model-specific InvokeModel")
}

// --- Bedrock Converse wire types ---

type converseRequest struct {
	Messages        []converseMessage `json:"messages"`
	System          []converseContent `json:"system,omitempty"`
	InferenceConfig *inferenceConfig  `json:"inferenceConfig,omitempty"`
	ToolConfig      *toolConfig       `json:"toolConfig,omitempty"`
}

type converseMessage struct {
	Role    string            `json:"role"`
	Content []converseContent `json:"content"`
}

type converseContent struct {
	Text       *string     `json:"text,omitempty"`
	ToolUse    *toolUse    `json:"toolUse,omitempty"`
	ToolResult *toolResult `json:"toolResult,omitempty"`
}

type toolUse struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
	Input     any    `json:"input"`
}

type toolResult struct {
	ToolUseID string            `json:"toolUseId"`
	Content   []converseContent `json:"content"`
}

type inferenceConfig struct {
	MaxTokens   *int     `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

type toolConfig struct {
	Tools []toolSpec `json:"tools"`
}

type toolSpec struct {
	ToolSpec *toolSpecInner `json:"toolSpec"`
}

type toolSpecInner struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type converseResponse struct {
	Output struct {
		Message converseMessage `json:"message"`
	} `json:"output"`
	StopReason string         `json:"stopReason"`
	Usage      *converseUsage `json:"usage"`
}

type converseUsage struct {
	InputTokens  *int `json:"inputTokens"`
	OutputTokens *int `json:"outputTokens"`
	TotalTokens  *int `json:"totalTokens"`
}

// --- Helpers ---

func buildConverseRequest(req *cpi.CompletionRequest) converseRequest {
	cr := converseRequest{
		InferenceConfig: &inferenceConfig{},
	}

	for _, m := range req.Messages {
		if m.Role == "system" {
			cr.System = append(cr.System, converseContent{Text: stringPtr(m.Content)})
			continue
		}

		msg := converseMessage{Role: m.Role}

		if m.Content != "" {
			msg.Content = append(msg.Content, converseContent{Text: stringPtr(m.Content)})
		}

		// Handle tool results from assistant tool calls.
		if m.ToolCallID != "" {
			if req.Invocation != nil {
				msg.Role = "user"
			}
			msg.Content = append(msg.Content, converseContent{
				ToolResult: &toolResult{
					ToolUseID: m.ToolCallID,
					Content:   []converseContent{{Text: stringPtr(m.Content)}},
				},
			})
		}

		for _, tc := range m.ToolCalls {
			var input any
			if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
				continue
			}
			msg.Content = append(msg.Content, converseContent{
				ToolUse: &toolUse{
					ToolUseID: tc.ID,
					Name:      tc.Name,
					Input:     input,
				},
			})
		}

		cr.Messages = append(cr.Messages, msg)
	}

	if len(req.Tools) > 0 {
		cr.ToolConfig = &toolConfig{}
		for _, t := range req.Tools {
			cr.ToolConfig.Tools = append(cr.ToolConfig.Tools, toolSpec{
				ToolSpec: &toolSpecInner{
					Name:        t.Name,
					Description: t.Description,
					InputSchema: t.Parameters,
				},
			})
		}
	}

	if v, ok := req.Parameters["max_tokens"]; ok {
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		cr.InferenceConfig.MaxTokens = &n
	}
	if v, ok := req.Parameters["temperature"]; ok {
		var f float64
		_, _ = fmt.Sscanf(v, "%f", &f)
		cr.InferenceConfig.Temperature = &f
	}

	return cr
}

func parseConverseResponse(resp *converseResponse) *cpi.CompletionResponse {
	cr := &cpi.CompletionResponse{
		FinishReason: resp.StopReason,
	}
	if resp.Usage != nil && resp.Usage.InputTokens != nil && resp.Usage.OutputTokens != nil {
		if usage, valid := cpi.NormalizeCompletionUsage(*resp.Usage.InputTokens, *resp.Usage.OutputTokens, resp.Usage.TotalTokens); valid {
			cr.Usage = usage
		}
	}

	for _, block := range resp.Output.Message.Content {
		if block.Text != nil {
			cr.Content += *block.Text
		}
		if block.ToolUse != nil {
			args, _ := json.Marshal(block.ToolUse.Input)
			cr.ToolCalls = append(cr.ToolCalls, cpi.ToolCall{
				ID:        block.ToolUse.ToolUseID,
				Name:      block.ToolUse.Name,
				Arguments: string(args),
			})
		}
	}

	return cr
}

// signRequest applies AWS Signature Version 4 to the request.
// This is a minimal implementation for Bedrock API calls.
func signRequest(req *http.Request, payload []byte, creds *cpi.Credentials) {
	if creds == nil {
		return
	}
	accessKey := creds.Data["accessKeyId"]
	secretKey := creds.Data["secretAccessKey"]
	sessionToken := creds.Data["sessionToken"]
	region := creds.Data["region"]

	if accessKey == "" || secretKey == "" || region == "" {
		return
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")

	req.Header.Set("X-Amz-Date", amzDate)
	if sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", sessionToken)
	}

	service := "bedrock"
	credentialScope := dateStamp + "/" + region + "/" + service + "/aws4_request"

	// Canonical request
	payloadHash := sha256Hex(payload)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signedHeaders, canonicalHeaders := buildCanonicalHeaders(req)

	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.Path,
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// String to sign
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	// Signing key
	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))

	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
}

func buildCanonicalHeaders(req *http.Request) (signedHeaders, canonicalHeaders string) {
	headers := make(map[string]string)
	var keys []string
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-type" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.TrimSpace(v[0])
			keys = append(keys, lk)
		}
	}
	headers["host"] = req.URL.Host
	keys = append(keys, "host")

	sort.Strings(keys)
	// Deduplicate
	seen := map[string]bool{}
	unique := keys[:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}
	keys = unique

	var canonical strings.Builder
	for _, k := range keys {
		canonical.WriteString(k)
		canonical.WriteByte(':')
		canonical.WriteString(headers[k])
		canonical.WriteByte('\n')
	}

	return strings.Join(keys, ";"), canonical.String()
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func stringPtr(s string) *string { return &s }
func intPtr(n int) *int          { return &n }
