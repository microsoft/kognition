// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import "github.com/kognition-ai/kognition/internal/gateway"

// RateLimiter is retained as an alias for the standalone server API.
type RateLimiter = gateway.RateLimiter

// NewRateLimiter creates the shared IntentGateway rate limiter.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return gateway.NewRateLimiter(rate, burst)
}
