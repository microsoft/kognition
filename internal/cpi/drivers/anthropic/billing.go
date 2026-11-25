// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"net/http"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// refineFailureReason separates billing and spend-limit failures, which arrive
// as 400 like any invalid request, from requests that are genuinely malformed.
// It keeps passive health from treating an exhausted account as a caller error.
// It uses the markers of the health probe, so both report the same failures.
func refineFailureReason(status int, body []byte) string {
	if status == http.StatusBadRequest {
		if _, exhausted := quotaOrBillingError(body); exhausted {
			return cpi.ReasonQuotaOrBilling
		}
	}
	return ""
}
