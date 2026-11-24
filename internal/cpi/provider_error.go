// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Failure reasons shared by CPI drivers. They match the reasons drivers report
// from Probe so that health checks and real calls describe failures consistently.
const (
	ReasonAuthenticationFailed = "AuthenticationFailed"
	ReasonQuotaOrBilling       = "QuotaOrBillingError"
	ReasonRateLimited          = "RateLimited"
	ReasonInvalidRequest       = "InvalidRequest"
	ReasonEndpointError        = "EndpointError"
)

const maxProviderErrorBodyBytes = 1024

// ProviderError is returned by drivers when a provider answers a Complete or
// Embed call with a non-success HTTP status. It keeps the status and a coarse
// reason machine-readable; Error() preserves the historical message format.
type ProviderError struct {
	// Provider is the driver name used as the message prefix (for example "anthropic").
	Provider string
	// Operation qualifies the call, such as "embed". It is empty for chat completions.
	Operation string
	// Status is the HTTP status code returned by the provider.
	Status int
	// Reason is the coarse failure classification (one of the Reason constants).
	Reason string
	// RetryAfter is the provider's Retry-After header value, if present.
	RetryAfter string
	// Body is the start of the provider's response body.
	Body string
}

func (e *ProviderError) Error() string {
	operation := ""
	if e.Operation != "" {
		operation = e.Operation + " "
	}
	return fmt.Sprintf("%s: %sHTTP %d: %s", e.Provider, operation, e.Status, e.Body)
}

// NewProviderError reads the start of the response body and classifies the
// status. It does not close the body.
func NewProviderError(provider, operation string, resp *http.Response) *ProviderError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorBodyBytes))
	return &ProviderError{
		Provider:   provider,
		Operation:  operation,
		Status:     resp.StatusCode,
		Reason:     ClassifyStatus(resp.StatusCode),
		RetryAfter: resp.Header.Get("Retry-After"),
		Body:       string(body),
	}
}

// ClassifyStatus maps a non-success HTTP status to a failure reason.
func ClassifyStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ReasonAuthenticationFailed
	case http.StatusPaymentRequired:
		return ReasonQuotaOrBilling
	case http.StatusTooManyRequests:
		return ReasonRateLimited
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return ReasonInvalidRequest
	default:
		return ReasonEndpointError
	}
}

// ProviderFailure extracts the ProviderError from err, if any.
func ProviderFailure(err error) (*ProviderError, bool) {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr, true
	}
	return nil, false
}
