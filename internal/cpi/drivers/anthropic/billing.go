// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kognition-ai/kognition/internal/cpi"
)

const maxProbeMessageSize = 200

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

// refineFailureReason separates billing and spend-limit failures, which arrive
// as 400 like any invalid request, from requests that are genuinely malformed.
// It keeps passive health from treating an exhausted account as a caller error.
func refineFailureReason(status int, body []byte) string {
	if status == http.StatusBadRequest {
		if _, exhausted := quotaOrBillingError(body); exhausted {
			return cpi.ReasonQuotaOrBilling
		}
	}
	return ""
}
