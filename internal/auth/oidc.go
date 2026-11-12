// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ProviderConfig configures a single trusted OIDC issuer.
type ProviderConfig struct {
	// IssuerURL is the OIDC issuer, e.g.
	// https://login.microsoftonline.com/<tenant>/v2.0 for Microsoft Entra ID.
	IssuerURL string
	// Audience is the required expected `aud` (the app/client id).
	Audience string
	// UsernameClaim selects the claim mapped to Identity.Username.
	// Defaults to "email".
	UsernameClaim string
	// GroupsClaim selects the claim mapped to Identity.Groups.
	// Defaults to "groups".
	GroupsClaim string
}

func (p ProviderConfig) withDefaults() ProviderConfig {
	if p.UsernameClaim == "" {
		p.UsernameClaim = "email"
	}
	if p.GroupsClaim == "" {
		p.GroupsClaim = "groups"
	}
	return p
}

type providerVerifier struct {
	cfg      ProviderConfig
	verifier *oidc.IDTokenVerifier
}

// OIDCAuthenticator verifies bearer JWTs against one or more OIDC providers.
type OIDCAuthenticator struct {
	verifiers []providerVerifier
}

// NewOIDCAuthenticator performs OIDC discovery for each provider and builds the
// token verifiers. It requires network access to each issuer's discovery
// endpoint at construction time.
func NewOIDCAuthenticator(ctx context.Context, providers []ProviderConfig) (*OIDCAuthenticator, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("auth: no OIDC providers configured")
	}
	a := &OIDCAuthenticator{}
	for _, p := range providers {
		if strings.TrimSpace(p.IssuerURL) == "" {
			return nil, fmt.Errorf("auth: OIDC issuer is required")
		}
		if strings.TrimSpace(p.Audience) == "" {
			return nil, fmt.Errorf("auth: OIDC audience is required for issuer %q", p.IssuerURL)
		}
		provider, err := oidc.NewProvider(ctx, p.IssuerURL)
		if err != nil {
			return nil, fmt.Errorf("auth: OIDC discovery for %q: %w", p.IssuerURL, err)
		}
		a.addVerifier(provider, p)
	}
	return a, nil
}

// addVerifier registers a verifier for the given provider/config. Split out so
// tests can inject a verifier built against a custom key set.
func (a *OIDCAuthenticator) addVerifier(provider *oidc.Provider, p ProviderConfig) {
	oc := &oidc.Config{ClientID: p.Audience}
	a.verifiers = append(a.verifiers, providerVerifier{
		cfg:      p.withDefaults(),
		verifier: provider.Verifier(oc),
	})
}

// Authenticate implements Authenticator.
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	var lastErr error
	for _, pv := range a.verifiers {
		idToken, err := pv.verifier.Verify(ctx, token)
		if err != nil {
			lastErr = err
			continue
		}
		var claims map[string]any
		if err := idToken.Claims(&claims); err != nil {
			return nil, fmt.Errorf("%w: parse claims: %w", ErrInvalidToken, err)
		}
		return identityFromClaims(idToken, claims, pv.cfg), nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidToken, lastErr)
}

func identityFromClaims(idToken *oidc.IDToken, claims map[string]any, cfg ProviderConfig) *Identity {
	id := &Identity{
		Subject: idToken.Subject,
		Issuer:  idToken.Issuer,
		Method:  "oidc",
		Claims:  claims,
	}
	if email, _ := claims["email"].(string); email != "" {
		id.Email = email
	}
	if v, ok := claims[cfg.UsernameClaim].(string); ok && v != "" {
		id.Username = v
	} else if id.Email != "" {
		id.Username = id.Email
	} else {
		id.Username = idToken.Subject
	}
	id.Groups = toStringSlice(claims[cfg.GroupsClaim])
	return id
}

func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
