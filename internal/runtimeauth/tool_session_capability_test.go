// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"testing"
	"time"
)

func TestToolSessionCapabilityCannotVerifyAsExecutableToolCapability(t *testing.T) {
	sessionIssuer, err := NewToolSessionCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sessionIssuer.now = func() time.Time { return time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC) }
	token, err := sessionIssuer.Issue(ToolSessionCapabilityBinding{Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner", InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionIssuer.VerifyClaims(token); err != nil {
		t.Fatalf("verify session capability: %v", err)
	}
	toolIssuer, err := NewToolCapabilityIssuer(capabilityKey(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := toolIssuer.VerifyClaims(token); err == nil {
		t.Fatal("session capability was accepted as an executable tool capability")
	}
}
