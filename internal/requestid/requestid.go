// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package requestid provides request-scoped unique identifiers for correlating
// log entries across a single API request or reconciliation cycle.
package requestid

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type contextKey struct{}

// NewContext returns a new context with the given request UID.
func NewContext(ctx context.Context, uid string) context.Context {
	return context.WithValue(ctx, contextKey{}, uid)
}

// FromContext extracts the request UID from the context. Returns empty string if not set.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey{}).(string); ok {
		return v
	}
	return ""
}

// Generate returns a new random UUID suitable for use as a request UID.
func Generate() string {
	return uuid.New().String()
}

// ZapField returns a zap.Field with the request UID from the context.
// If no UID is present, it returns zap.Skip() so logging is unaffected.
func ZapField(ctx context.Context) zap.Field {
	if uid := FromContext(ctx); uid != "" {
		return zap.String("requestUID", uid)
	}
	return zap.Skip()
}

// Middleware is an HTTP middleware that injects a generated request UID into
// the request context for every incoming request.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := Generate()
		ctx := NewContext(r.Context(), uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
