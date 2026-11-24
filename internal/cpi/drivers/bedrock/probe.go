// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bedrock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	maxProbeBodyBytes   = 4096
	maxProbeMessageSize = 200
)

// classifyProbeResponse maps a Converse probe response to an unhealthy reason and
// message. It returns an empty reason when the endpoint is alive: any 2xx, or a
// 400 ValidationException that does not reject the model itself.
func classifyProbeResponse(status int, header http.Header, body []byte) (reason, message string) {
	switch {
	case status >= 200 && status < 300:
		return "", ""
	case status == http.StatusBadRequest:
		if detail, rejected := modelRejection(body); rejected {
			return "ModelUnavailable", fmt.Sprintf("HTTP %d from Bedrock: %s", status, detail)
		}
		return "", ""
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return "AuthenticationFailed", fmt.Sprintf("HTTP %d from Bedrock", status)
	case status == http.StatusTooManyRequests:
		message = fmt.Sprintf("HTTP %d from Bedrock: throttled", status)
		if retryAfter := header.Get("Retry-After"); retryAfter != "" {
			message += fmt.Sprintf(" (Retry-After: %s)", retryAfter)
		}
		return "RateLimited", message
	default:
		return "EndpointError", fmt.Sprintf("HTTP %d from Bedrock", status)
	}
}

// Converse returns 400 only for ValidationException. Besides malformed input,
// that covers requests that name a model which cannot serve this call, and the
// message text is the only discriminator.
var modelRejectionMarkers = []string{
	"model identifier is invalid",
	"on-demand throughput",
	"doesn't support the model",
}

func modelRejection(body []byte) (string, bool) {
	var envelope struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "", false
	}
	// Normalize typographic apostrophes, which some responses use ("isn’t").
	lowered := strings.ReplaceAll(strings.ToLower(envelope.Message), "’", "'")
	for _, marker := range modelRejectionMarkers {
		if strings.Contains(lowered, marker) {
			detail := envelope.Message
			if runes := []rune(detail); len(runes) > maxProbeMessageSize {
				detail = string(runes[:maxProbeMessageSize])
			}
			return detail, true
		}
	}
	return "", false
}
