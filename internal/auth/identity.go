// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package auth provides request authentication for the Kognition API server.
//
// The primary mechanism is OIDC: bearer JWTs are verified against one or more
// trusted providers (Microsoft Entra ID, Google, Auth0, Keycloak/Dex, or any
// spec-compliant issuer) using discovery + JWKS. A static bearer-token
// authenticator is also provided for bootstrap/service-to-service use.
//
// Authenticated callers are represented by an Identity carried in the request
// context; handlers derive the acting principal from it (e.g. for approval
// quorum and audit) instead of trusting caller-asserted fields.
package auth

import "context"

// Identity is an authenticated caller of the API.
type Identity struct {
	// Subject is the stable, unique principal id (OIDC `sub`, or token subject).
	Subject string
	// Username is a human-friendly identifier (email / preferred_username / sub).
	Username string
	// Email is the caller's email, when present.
	Email string
	// Groups are the caller's group/role memberships, when present.
	Groups []string
	// Issuer is the OIDC issuer that vouched for this identity (empty for tokens).
	Issuer string
	// Method is how the caller authenticated: "oidc", "token", or "anonymous".
	Method string
	// Claims holds the raw verified token claims (OIDC only).
	Claims map[string]any
}

type contextKey struct{}

// WithIdentity returns a context carrying the authenticated identity.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// IdentityFromContext returns the authenticated identity, if any.
func IdentityFromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(*Identity)
	return id, ok
}

// Anonymous is the identity used when authentication is disabled.
func Anonymous() *Identity {
	return &Identity{Subject: "anonymous", Username: "anonymous", Method: "anonymous"}
}

// IsAuthenticated reports whether ctx carries a non-anonymous identity.
func IsAuthenticated(ctx context.Context) bool {
	id, ok := IdentityFromContext(ctx)
	return ok && id != nil && id.Method != "" && id.Method != "anonymous"
}
