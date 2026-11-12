// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIdentityContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := IdentityFromContext(ctx); ok {
		t.Fatal("empty context should have no identity")
	}
	if IsAuthenticated(ctx) {
		t.Error("empty context should not be authenticated")
	}

	id := &Identity{Subject: "u1", Username: "alice", Method: "oidc"}
	ctx = WithIdentity(ctx, id)
	got, ok := IdentityFromContext(ctx)
	if !ok || got.Username != "alice" {
		t.Fatalf("IdentityFromContext = %v, %v", got, ok)
	}
	if !IsAuthenticated(ctx) {
		t.Error("should be authenticated")
	}

	anon := WithIdentity(context.Background(), Anonymous())
	if IsAuthenticated(anon) {
		t.Error("anonymous should not count as authenticated")
	}
}

func TestStaticTokenAuthenticator(t *testing.T) {
	a := NewStaticTokenAuthenticator(map[string]string{"secret-abc": "svc-1"})
	ctx := context.Background()

	if _, err := a.Authenticate(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty token: want ErrNoToken, got %v", err)
	}
	id, err := a.Authenticate(ctx, "secret-abc")
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if id.Subject != "svc-1" || id.Method != "token" {
		t.Errorf("identity = %+v", id)
	}
	if _, err := a.Authenticate(ctx, "wrong"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong token: want ErrInvalidToken, got %v", err)
	}
}

func TestChain(t *testing.T) {
	static := NewStaticTokenAuthenticator(map[string]string{"tok": "svc"})
	chain := Chain{static}
	ctx := context.Background()

	if id, err := chain.Authenticate(ctx, "tok"); err != nil || id.Subject != "svc" {
		t.Errorf("valid: %v, %v", id, err)
	}
	if _, err := chain.Authenticate(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty: want ErrNoToken, got %v", err)
	}
	if _, err := chain.Authenticate(ctx, "nope"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("invalid: want ErrInvalidToken, got %v", err)
	}
}

// fakeAuth is a test Authenticator: empty token -> ErrNoToken, otherwise returns
// the configured identity/error.
type fakeAuth struct {
	id  *Identity
	err error
}

func (f fakeAuth) Authenticate(_ context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.id, nil
}

func newRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestMiddleware_RejectsWithoutToken(t *testing.T) {
	h := Middleware(fakeAuth{id: &Identity{Username: "x"}}, MiddlewareConfig{})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
}

func TestMiddleware_PassesAndSetsIdentity(t *testing.T) {
	want := &Identity{Subject: "u1", Username: "alice", Method: "oidc"}
	var seen *Identity
	h := Middleware(fakeAuth{id: want}, MiddlewareConfig{})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen, _ = IdentityFromContext(r.Context())
		}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("valid"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if seen == nil || seen.Username != "alice" {
		t.Errorf("identity in context = %v", seen)
	}
}

func TestMiddleware_SkipPaths(t *testing.T) {
	called := false
	h := Middleware(fakeAuth{}, MiddlewareConfig{SkipPaths: map[string]bool{"/healthz": true}})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("health path should bypass auth: called=%v code=%d", called, rec.Code)
	}
}

func TestMiddleware_RequiredGroups(t *testing.T) {
	authr := fakeAuth{id: &Identity{Username: "bob", Groups: []string{"devs"}, Method: "oidc"}}

	// Caller lacks the required group -> 403.
	denied := Middleware(authr, MiddlewareConfig{RequiredGroups: []string{"admins"}})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") }),
	)
	rec := httptest.NewRecorder()
	denied.ServeHTTP(rec, newRequest("valid"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}

	// Caller has a required group -> allowed.
	ok := false
	allowed := Middleware(authr, MiddlewareConfig{RequiredGroups: []string{"devs", "admins"}})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ok = true }),
	)
	rec2 := httptest.NewRecorder()
	allowed.ServeHTTP(rec2, newRequest("valid"))
	if !ok || rec2.Code != http.StatusOK {
		t.Errorf("caller with required group should pass: ok=%v code=%d", ok, rec2.Code)
	}
}
