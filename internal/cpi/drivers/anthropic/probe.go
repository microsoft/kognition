// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

const (
	messagesPath    = "/v1/messages"
	countTokensPath = "/v1/messages/count_tokens"

	maxProbeBodyBytes   = 4096
	maxProbeMessageSize = 200
)

// Probe checks connectivity, authentication and model availability. It uses the
// free token counting endpoint so health checks are not billed and do not
// consume Messages rate limit. Anthropic-compatible endpoints that do not
// implement token counting (404 or 405) fall back to a minimal messages request.
func (d *Driver) Probe(ctx context.Context, model *v1alpha1.ModelSpec, creds *cpi.Credentials) (*cpi.HealthResult, error) {
	start := time.Now()

	baseURL := model.Endpoint
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	probeMessages := []message{{Role: "user", Content: "hi"}}

	response, failure := d.sendProbe(ctx, start, model, creds, baseURL+countTokensPath,
		countTokensRequest{Model: model.ModelID, Messages: probeMessages})
	if failure == nil && (response.status == http.StatusNotFound || response.status == http.StatusMethodNotAllowed) {
		response, failure = d.sendProbe(ctx, start, model, creds, baseURL+messagesPath,
			messagesRequest{Model: model.ModelID, MaxTokens: 1, Messages: probeMessages})
	}
	if failure != nil {
		return failure, nil
	}

	latency := time.Since(start).String()
	if reason, message := classifyProbeResponse(response); reason != "" {
		return &cpi.HealthResult{
			Available: false,
			Latency:   latency,
			Reason:    reason,
			Message:   message,
		}, nil
	}

	return &cpi.HealthResult{
		Available:    true,
		Latency:      latency,
		Reason:       "Healthy",
		Message:      "Anthropic API reachable",
		Capabilities: []string{"chat"},
	}, nil
}

type countTokensRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
}

type probeResponse struct {
	status     int
	retryAfter string
	body       []byte
}

// sendProbe posts the payload and returns the response, or a ready-to-return
// unavailable result when the request cannot be built or sent.
func (d *Driver) sendProbe(ctx context.Context, start time.Time, model *v1alpha1.ModelSpec, creds *cpi.Credentials, endpoint string, payload any) (*probeResponse, *cpi.HealthResult) {
	// The probe payloads are fixed structs of strings and always marshal.
	encoded, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, &cpi.HealthResult{Available: false, Reason: "RequestBuildFailed", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)
	if model.InvocationProfileRef != nil {
		req.Header.Set("anthropic-version", invocationAPIVersion(model.Parameters))
	}
	applyAuth(req, creds)

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, &cpi.HealthResult{
			Available: false,
			Latency:   time.Since(start).String(),
			Reason:    "EndpointUnreachable",
			Message:   err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProbeBodyBytes))
	_, _ = io.Copy(io.Discard, resp.Body)

	return &probeResponse{
		status:     resp.StatusCode,
		retryAfter: resp.Header.Get("Retry-After"),
		body:       body,
	}, nil
}

// classifyProbeResponse maps a probe response to an unhealthy reason and message.
// It returns an empty reason when the endpoint is alive: any 2xx, or a 400
// validation error that is not a billing or spend-limit failure.
func classifyProbeResponse(response *probeResponse) (reason, message string) {
	status := response.status
	switch {
	case status >= 200 && status < 300:
		return "", ""
	case status == http.StatusBadRequest:
		if detail, exhausted := quotaOrBillingError(response.body); exhausted {
			return "QuotaOrBillingError", fmt.Sprintf("HTTP %d from Anthropic: %s", status, detail)
		}
		return "", ""
	case status == http.StatusPaymentRequired:
		return "QuotaOrBillingError", fmt.Sprintf("HTTP %d from Anthropic: billing error", status)
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "AuthenticationFailed", fmt.Sprintf("HTTP %d", status)
	case status == http.StatusTooManyRequests:
		message = fmt.Sprintf("HTTP %d from Anthropic: rate limited", status)
		if response.retryAfter != "" {
			message += fmt.Sprintf(" (Retry-After: %s)", response.retryAfter)
		}
		return "RateLimited", message
	default:
		return "EndpointError", fmt.Sprintf("HTTP %d from Anthropic", status)
	}
}

type errorEnvelope struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Anthropic reports exhausted credit and organization or workspace spend limits
// as 400 invalid_request_error, so the message text is the only discriminator.
var quotaOrBillingMarkers = []string{"credit balance", "billing", "usage limit", "spend limit"}

func quotaOrBillingError(body []byte) (string, bool) {
	var envelope errorEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return "", false
	}
	lowered := strings.ToLower(envelope.Error.Message)
	for _, marker := range quotaOrBillingMarkers {
		if strings.Contains(lowered, marker) {
			message := envelope.Error.Message
			if runes := []rune(message); len(runes) > maxProbeMessageSize {
				message = string(runes[:maxProbeMessageSize])
			}
			return message, true
		}
	}
	return "", false
}
