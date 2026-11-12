// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"errors"
)

var (
	// ErrNoToken indicates the request carried no bearer token.
	ErrNoToken = errors.New("auth: no bearer token")
	// ErrInvalidToken indicates the token failed verification.
	ErrInvalidToken = errors.New("auth: invalid token")
)

// Authenticator verifies a raw bearer token and returns the caller identity.
// Implementations return ErrNoToken when token is empty, and an error wrapping
// ErrInvalidToken when verification fails.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Identity, error)
}

// Chain tries each authenticator in order and returns the first success.
// An authenticator returning ErrNoToken is skipped; the chain fails with the
// last meaningful error (or ErrNoToken if none matched).
type Chain []Authenticator

// Authenticate implements Authenticator.
func (c Chain) Authenticate(ctx context.Context, token string) (*Identity, error) {
	lastErr := error(ErrNoToken)
	for _, a := range c {
		id, err := a.Authenticate(ctx, token)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, ErrNoToken) {
			lastErr = err
		}
	}
	return nil, lastErr
}
