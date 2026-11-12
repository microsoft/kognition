// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func capabilityKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func TestMemoryCapabilityIssuerVerifiesExactBinding(t *testing.T) {
	issuer, err := NewMemoryCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.now = func() time.Time { return time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC) }
	binding := MemoryCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", VolumeHandle: "team-a-memory",
	}
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.Verify(token, binding)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "" || claims.VolumeHandle != binding.VolumeHandle || len(claims.RegisteredClaims.Audience) != 1 || claims.RegisteredClaims.Audience[0] != MemoryCapabilityAudience {
		t.Fatalf("claims = %#v", claims)
	}

	binding.VolumeHandle = "another-volume"
	if _, err := issuer.Verify(token, binding); !errors.Is(err, ErrCapabilityBindingMismatch) {
		t.Fatalf("different volume error = %v, want binding mismatch", err)
	}
}

func TestMemoryCapabilityIssuerRejectsExpiredAndWrongKeyTokens(t *testing.T) {
	issuer, err := NewMemoryCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding := MemoryCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", VolumeHandle: "team-a-memory",
	}
	issuer.ttl = -time.Minute
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Verify(token, binding); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("expired token error = %v, want invalid capability", err)
	}

	otherIssuer, err := NewMemoryCapabilityIssuer([]byte("abcdef0123456789abcdef0123456789"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherIssuer.Verify(token, binding); !errors.Is(err, ErrInvalidCapability) {
		t.Fatalf("wrong key error = %v, want invalid capability", err)
	}
}

func TestMemoryCapabilityIssuerRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := NewMemoryCapabilityIssuer(make([]byte, 31), time.Minute); err == nil {
		t.Fatal("short key was accepted")
	}
	if _, err := NewMemoryCapabilityIssuer(capabilityKey(), 0); err == nil {
		t.Fatal("zero TTL was accepted")
	}
	if _, err := NewMemoryCapabilityIssuer(make([]byte, 32), time.Minute); err == nil {
		t.Fatal("all-zero key was accepted")
	}
	issuer, err := NewMemoryCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(MemoryCapabilityBinding{}); err == nil {
		t.Fatal("incomplete binding was accepted")
	}
}

func TestMemoryCapabilityIssuerRejectsInvalidJWTClaims(t *testing.T) {
	issuer, err := NewMemoryCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding := MemoryCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", VolumeHandle: "team-a-memory",
	}
	now := time.Now().UTC()
	baseClaims := MemoryCapabilityClaims{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID, VolumeHandle: binding.VolumeHandle,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: memoryCapabilityIssuer, Audience: jwt.ClaimStrings{MemoryCapabilityAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}
	sign := func(claims MemoryCapabilityClaims, method jwt.SigningMethod) string {
		t.Helper()
		token, signErr := jwt.NewWithClaims(method, claims).SignedString(capabilityKey())
		if signErr != nil {
			t.Fatal(signErr)
		}
		return token
	}
	tests := []struct {
		name  string
		token string
	}{
		{name: "wrong audience", token: sign(func() MemoryCapabilityClaims {
			claims := baseClaims
			claims.Audience = jwt.ClaimStrings{"other"}
			return claims
		}(), jwt.SigningMethodHS256)},
		{name: "wrong issuer", token: sign(func() MemoryCapabilityClaims { claims := baseClaims; claims.Issuer = "other"; return claims }(), jwt.SigningMethodHS256)},
		{name: "missing expiration", token: sign(func() MemoryCapabilityClaims { claims := baseClaims; claims.ExpiresAt = nil; return claims }(), jwt.SigningMethodHS256)},
		{name: "modified binding", token: sign(func() MemoryCapabilityClaims {
			claims := baseClaims
			claims.InstanceName = "other-instance"
			return claims
		}(), jwt.SigningMethodHS256)},
		{name: "wrong algorithm", token: sign(baseClaims, jwt.SigningMethodHS384)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := issuer.Verify(test.token, binding); err == nil {
				t.Fatal("invalid capability was accepted")
			}
		})
	}
}
