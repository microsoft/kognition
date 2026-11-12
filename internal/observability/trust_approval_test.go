// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestTrustApprovalEvidence(t *testing.T) {
	if got := TrustApproval(nil); got.Status != "not_required" || got.DecisionRef != nil {
		t.Fatalf("ungated operation: %+v", got)
	}
	for _, test := range []struct{ state, want string }{
		{v1alpha1.RuntimeApprovalDecisionNotRequired, "not_required"},
		{v1alpha1.RuntimeApprovalDecisionApproved, "approved"},
		{v1alpha1.RuntimeApprovalDecisionPending, "pending"},
		{v1alpha1.RuntimeApprovalDecisionRejected, "rejected"},
		{v1alpha1.RuntimeApprovalDecisionExpired, "rejected"},
		{"", "unavailable"},
	} {
		t.Run(test.want+test.state, func(t *testing.T) {
			approval := &v1alpha1.RuntimeApproval{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "approval-1", Namespace: "team-a", UID: "approval-uid", ResourceVersion: 7},
				Status:     v1alpha1.RuntimeApprovalStatus{DecisionState: test.state},
			}
			got := TrustApproval(approval)
			if got.Status != test.want || got.DecisionRef == nil || got.DecisionRef.Kind != "RuntimeApproval" || got.DecisionRef.Name != approval.Name || got.DecisionRef.Namespace != approval.Namespace || got.DecisionRef.UID != approval.UID || got.DecisionRef.ResourceVersion != "7" {
				t.Fatalf("approval evidence: %+v", got)
			}
		})
	}
}
