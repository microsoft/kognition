// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

func TestStandaloneModelEvidenceDoesNotInventCapability(t *testing.T) {
	store := &modelAuditStore{}
	audit := observability.NewCognitiveAuditLog(store)
	ctx := observability.ContextWithTraceHeaders(context.Background(), http.Header{"Traceparent": {"00-11111111111111111111111111111111-2222222222222222-01"}})
	input := StandaloneDecisionInput{Workspace: "team-a", SessionID: "session-1", ModelName: "model-1", Agent: &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "target", Namespace: "team-a", UID: "target-uid", ResourceVersion: 3}}}
	if err := RecordStandaloneDecision(ctx, audit, input, "allowed", "authorized", "not_started", false, true); err != nil {
		t.Fatal(err)
	}
	if err := RecordStandaloneDecision(ctx, audit, input, "completed", "target_completed", "completed", true, false); err != nil {
		t.Fatal(err)
	}
	entries, err := audit.Query(ctx, observability.AuditFilter{})
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	for _, entry := range entries {
		var decision observability.TrustDecisionV1
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatal(err)
		}
		if decision.Session == nil || decision.Session.SessionID != input.SessionID || decision.Session.CapabilityID != "" {
			t.Fatalf("incorrect session evidence: %+v", decision.Session)
		}
		if decision.Approval == nil || decision.Approval.Status != "not_required" {
			t.Fatalf("incorrect approval evidence: %+v", decision.Approval)
		}
		if decision.Identity != nil || decision.AgentRef == nil || decision.AgentRef.Kind != "Agent" || decision.AgentRef.Name != "target" || decision.AgentRef.UID != "target-uid" || decision.AgentRef.ResourceVersion != "3" || decision.Session.AuthorityMode != "in_process" {
			t.Fatalf("incorrect in-process attribution: %+v", decision)
		}
		if decision.Trace == nil || decision.Trace.TraceID != "11111111111111111111111111111111" {
			t.Fatalf("missing trace correlation: %+v", decision.Trace)
		}
	}
}
