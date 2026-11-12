// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"errors"
	"testing"
	"time"
)

func toolBinding() ToolCapabilityBinding {
	return ToolCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1", Solution: "research", SolutionRev: 5,
		ToolName: "crm-update", ToolResourceVersion: 42, Verb: "invoke", InvocationID: "invocation-1",
		InputDigest: "sha256:af6f5f23a70d6c0b4a4567f635f5694605c3f6f20cfb5c2947df47d585de60df",
	}
}

func TestToolCapabilityIssuerVerifiesExactBinding(t *testing.T) {
	issuer, err := NewToolCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	issuer.now = func() time.Time { return time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC) }
	binding := toolBinding()
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.Verify(token, binding)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ToolName != binding.ToolName || claims.ToolResourceVersion != binding.ToolResourceVersion || claims.Verb != "invoke" || claims.InvocationID != binding.InvocationID || claims.InputDigest != binding.InputDigest || claims.Solution != "research" || claims.SolutionRev != 5 {
		t.Fatalf("claims = %#v", claims)
	}
	binding.ToolResourceVersion = 43
	if _, err := issuer.Verify(token, binding); !errors.Is(err, ErrCapabilityBindingMismatch) {
		t.Fatalf("different tool version error = %v, want binding mismatch", err)
	}
}

func TestToolCapabilityIssuerRejectsChangedInputDigest(t *testing.T) {
	issuer, err := NewToolCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding := toolBinding()
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	binding.InputDigest = "sha256:5dd7d9f54c28134bba6a2c2b5f4f19fd760bc379d0d58a7747ea098f92c7b53d"
	if _, err := issuer.Verify(token, binding); !errors.Is(err, ErrCapabilityBindingMismatch) {
		t.Fatalf("different input digest error = %v, want binding mismatch", err)
	}
}

func TestToolCapabilityIssuerRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := NewToolCapabilityIssuer(make([]byte, 31), time.Minute); err == nil {
		t.Fatal("short key was accepted")
	}
	if _, err := NewToolCapabilityIssuer(capabilityKey(), 0); err == nil {
		t.Fatal("zero TTL was accepted")
	}
	issuer, err := NewToolCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.Issue(ToolCapabilityBinding{}); err == nil {
		t.Fatal("incomplete binding was accepted")
	}
}
