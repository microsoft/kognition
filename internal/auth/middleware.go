// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"fmt"
	"net/http"
	"strings"
)

// MiddlewareConfig configures the authentication middleware.
type MiddlewareConfig struct {
	// SkipPaths are exact request paths that bypass authentication (e.g. health).
	SkipPaths map[string]bool
	// RequiredGroups, when non-empty, requires the identity to belong to at
	// least one of these groups (coarse authorization).
	RequiredGroups []string
	// OnError handles rejected requests. Defaults to a JSON error response.
	OnError func(w http.ResponseWriter, r *http.Request, status int, msg string)
}

// Middleware authenticates requests with the given Authenticator and stores the
// resulting Identity in the request context. Requests to SkipPaths bypass
// authentication. Rejections return 401 (no/invalid token) or 403 (missing
// required group).
func Middleware(a Authenticator, cfg MiddlewareConfig) func(http.Handler) http.Handler {
	onError := cfg.OnError
	if onError == nil {
		onError = defaultOnError
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.SkipPaths[r.URL.Path] {
				next.ServeHTTP(w, r)
				return
			}
			id, err := a.Authenticate(r.Context(), bearerToken(r))
			if err != nil {
				onError(w, r, http.StatusUnauthorized, "authentication required")
				return
			}
			if len(cfg.RequiredGroups) > 0 && !hasAnyGroup(id.Groups, cfg.RequiredGroups) {
				onError(w, r, http.StatusForbidden, "forbidden: caller lacks a required group")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

// bearerToken extracts a token from the Authorization: Bearer <token> header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func hasAnyGroup(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, g := range have {
		set[g] = struct{}{}
	}
	for _, g := range want {
		if _, ok := set[g]; ok {
			return true
		}
	}
	return false
}

func defaultOnError(w http.ResponseWriter, _ *http.Request, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":%q}`, msg)
}
