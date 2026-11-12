// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// ActiveCapabilityKeyID identifies newly issued runtime capabilities.
	ActiveCapabilityKeyID = "active"
	// PreviousCapabilityKeyID verifies capabilities issued before a rotation.
	PreviousCapabilityKeyID = "previous"
)

// CapabilityKeyRing signs new capabilities with its active key and verifies
// capabilities signed by the active or previous key during rotation overlap.
type CapabilityKeyRing struct {
	activeID  string
	activeKey []byte
	keys      map[string][]byte
}

// NewCapabilityKeyRing creates a ring with an active signing key and an
// optional previous verification key. The previous key is never used to sign.
func NewCapabilityKeyRing(activeID string, activeKey []byte, previousID string, previousKey []byte) (*CapabilityKeyRing, error) {
	if err := validateCapabilityKey("active", activeID, activeKey); err != nil {
		return nil, err
	}
	ring := &CapabilityKeyRing{
		activeID:  activeID,
		activeKey: append([]byte(nil), activeKey...),
		keys:      map[string][]byte{activeID: append([]byte(nil), activeKey...)},
	}
	if len(previousKey) == 0 {
		if previousID != "" {
			return nil, fmt.Errorf("previous capability key ID requires key material")
		}
		return ring, nil
	}
	if err := validateCapabilityKey("previous", previousID, previousKey); err != nil {
		return nil, err
	}
	if previousID == activeID {
		return nil, fmt.Errorf("active and previous capability key IDs must differ")
	}
	ring.keys[previousID] = append([]byte(nil), previousKey...)
	return ring, nil
}

// NewDefaultCapabilityKeyRing uses the stable active and previous key IDs for
// environment-projected key material. An empty previous key disables overlap.
func NewDefaultCapabilityKeyRing(activeKey, previousKey []byte) (*CapabilityKeyRing, error) {
	return NewConfiguredCapabilityKeyRing(ActiveCapabilityKeyID, activeKey, PreviousCapabilityKeyID, previousKey)
}

// NewConfiguredCapabilityKeyRing accepts stable per-key IDs from deployment
// configuration. The IDs must move with their matching key material during a
// rotation so capabilities retain a resolvable kid throughout the overlap.
func NewConfiguredCapabilityKeyRing(activeID string, activeKey []byte, previousID string, previousKey []byte) (*CapabilityKeyRing, error) {
	if activeID == "" {
		activeID = ActiveCapabilityKeyID
	}
	if len(previousKey) == 0 {
		previousID = ""
	}
	return NewCapabilityKeyRing(activeID, activeKey, previousID, previousKey)
}

func validateCapabilityKey(label, id string, key []byte) error {
	if id == "" {
		return fmt.Errorf("%s capability key ID is required", label)
	}
	if len(key) < minimumHMACKeyBytes {
		return fmt.Errorf("%s capability key must contain at least %d bytes", label, minimumHMACKeyBytes)
	}
	if isZeroKey(key) {
		return fmt.Errorf("%s capability key must not be all zero", label)
	}
	return nil
}

// Sign issues a JWT marked with the active key ID.
func (r *CapabilityKeyRing) Sign(claims jwt.Claims) (string, error) {
	if r == nil {
		return "", fmt.Errorf("capability key ring is required")
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = r.activeID
	return token.SignedString(r.activeKey)
}

// VerificationKeys returns the allowed HMAC keys for a token. Legacy tokens
// without a kid are tried against every configured key during the overlap.
func (r *CapabilityKeyRing) VerificationKeys(token string) ([][]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("capability key ring is required")
	}
	parsed, _, err := new(jwt.Parser).ParseUnverified(token, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("parse capability header: %w", err)
	}
	if keyID, present := parsed.Header["kid"]; present {
		id, ok := keyID.(string)
		if !ok || id == "" {
			return nil, fmt.Errorf("capability key ID is invalid")
		}
		key, found := r.keys[id]
		if !found {
			return nil, fmt.Errorf("capability key ID %q is not trusted", id)
		}
		return [][]byte{key}, nil
	}
	keys := [][]byte{r.activeKey}
	for id, key := range r.keys {
		if id != r.activeID {
			keys = append(keys, key)
		}
	}
	return keys, nil
}
