// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// newTestOIDCProvider spins up an httptest server exposing an OIDC discovery
// document and JWKS backed by a freshly generated RSA key.
func newTestOIDCProvider(t *testing.T) (issuer string, priv *rsa.PrivateKey, kid string) {
	t.Helper()
	var err error
	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	kid = "test-key-1"

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"jwks_uri":                              issuer + "/keys",
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       priv.Public(),
			KeyID:     kid,
			Algorithm: "RS256",
			Use:       "sig",
		}}}
		_ = json.NewEncoder(w).Encode(set)
	})
	return issuer, priv, kid
}

func signJWT(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestOIDCAuthenticator_ValidToken(t *testing.T) {
	issuer, priv, kid := newTestOIDCProvider(t)
	ctx := context.Background()

	a, err := NewOIDCAuthenticator(ctx, []ProviderConfig{{
		IssuerURL:     issuer,
		Audience:      "my-client",
		UsernameClaim: "preferred_username",
		GroupsClaim:   "groups",
	}})
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}

	token := signJWT(t, priv, kid, jwt.MapClaims{
		"iss":                issuer,
		"aud":                "my-client",
		"sub":                "user-123",
		"email":              "alice@example.com",
		"preferred_username": "alice",
		"groups":             []string{"admins", "devs"},
		"iat":                time.Now().Unix(),
		"exp":                time.Now().Add(time.Hour).Unix(),
	})

	id, err := a.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Subject != "user-123" {
		t.Errorf("Subject = %q, want user-123", id.Subject)
	}
	if id.Username != "alice" {
		t.Errorf("Username = %q, want alice", id.Username)
	}
	if id.Email != "alice@example.com" {
		t.Errorf("Email = %q", id.Email)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "admins" {
		t.Errorf("Groups = %v", id.Groups)
	}
	if id.Method != "oidc" || id.Issuer != issuer {
		t.Errorf("Method=%q Issuer=%q", id.Method, id.Issuer)
	}
}

func TestNewEnvironmentAuthenticatorRejectsOIDCIssuerWithoutAudience(t *testing.T) {
	t.Setenv("KOGNITION_OIDC_ISSUER", "https://issuer.example")
	t.Setenv("KOGNITION_OIDC_AUDIENCE", "")
	if _, _, err := NewEnvironmentAuthenticator(context.Background()); err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("NewEnvironmentAuthenticator() error = %v, want required audience error", err)
	}
}

func TestOIDCAuthenticator_UsernameFallsBackToEmailThenSub(t *testing.T) {
	issuer, priv, kid := newTestOIDCProvider(t)
	ctx := context.Background()
	a, err := NewOIDCAuthenticator(ctx, []ProviderConfig{{IssuerURL: issuer, Audience: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	// No email, no configured username claim value -> falls back to sub.
	token := signJWT(t, priv, kid, jwt.MapClaims{
		"iss": issuer, "aud": "c", "sub": "sub-only",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	id, err := a.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "sub-only" {
		t.Errorf("Username = %q, want sub-only", id.Username)
	}
}

func TestOIDCAuthenticator_Rejects(t *testing.T) {
	issuer, priv, kid := newTestOIDCProvider(t)
	ctx := context.Background()
	a, err := NewOIDCAuthenticator(ctx, []ProviderConfig{{IssuerURL: issuer, Audience: "my-client"}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.Authenticate(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty token: want ErrNoToken, got %v", err)
	}

	cases := map[string]jwt.MapClaims{
		"wrong audience": {"iss": issuer, "aud": "other", "sub": "x", "exp": time.Now().Add(time.Hour).Unix()},
		"expired":        {"iss": issuer, "aud": "my-client", "sub": "x", "exp": time.Now().Add(-time.Hour).Unix()},
		"wrong issuer":   {"iss": "https://evil.example", "aud": "my-client", "sub": "x", "exp": time.Now().Add(time.Hour).Unix()},
	}
	for name, claims := range cases {
		tok := signJWT(t, priv, kid, claims)
		if _, err := a.Authenticate(ctx, tok); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	if _, err := a.Authenticate(ctx, "not.a.jwt"); err == nil {
		t.Error("garbage token should be rejected")
	}
}

func TestNewOIDCAuthenticator_NoProviders(t *testing.T) {
	if _, err := NewOIDCAuthenticator(context.Background(), nil); err == nil {
		t.Error("expected error with no providers")
	}
}

func TestNewOIDCAuthenticator_RequiresAudience(t *testing.T) {
	issuer, _, _ := newTestOIDCProvider(t)
	_, err := NewOIDCAuthenticator(context.Background(), []ProviderConfig{{IssuerURL: issuer}})
	if err == nil || !strings.Contains(err.Error(), "OIDC audience is required") {
		t.Fatalf("NewOIDCAuthenticator() error = %v, want audience validation", err)
	}
}
