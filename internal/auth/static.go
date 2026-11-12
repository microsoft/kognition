// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"crypto/subtle"
)

// StaticTokenAuthenticator authenticates against a fixed set of bearer tokens.
// It is intended for bootstrap and service-to-service use; OIDC is preferred
// for human callers. Tokens are compared in constant time.
type StaticTokenAuthenticator struct {
	// tokens maps a bearer token to the subject it authenticates as.
	tokens map[string]*Identity
}

// NewStaticTokenAuthenticator builds an authenticator from a token->subject map.
func NewStaticTokenAuthenticator(tokens map[string]string) *StaticTokenAuthenticator {
	m := make(map[string]*Identity, len(tokens))
	for tok, subject := range tokens {
		m[tok] = &Identity{Subject: subject, Username: subject, Method: "token"}
	}
	return &StaticTokenAuthenticator{tokens: m}
}

// Authenticate implements Authenticator.
func (s *StaticTokenAuthenticator) Authenticate(_ context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	for known, id := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(known)) == 1 {
			return id, nil
		}
	}
	return nil, ErrInvalidToken
}
