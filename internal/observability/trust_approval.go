// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TrustApproval(approval *v1alpha1.RuntimeApproval) *TrustDecisionApproval {
	if approval == nil {
		return &TrustDecisionApproval{Status: "not_required"}
	}
	status := "unavailable"
	switch approval.Status.DecisionState {
	case v1alpha1.RuntimeApprovalDecisionNotRequired:
		status = "not_required"
	case v1alpha1.RuntimeApprovalDecisionApproved:
		status = "approved"
	case v1alpha1.RuntimeApprovalDecisionPending:
		status = "pending"
	case v1alpha1.RuntimeApprovalDecisionRejected, v1alpha1.RuntimeApprovalDecisionExpired:
		status = "rejected"
	}
	evidence := &TrustDecisionApproval{Status: status}
	if approval.ObjectMeta.Name != "" {
		evidence.DecisionRef = &TrustDecisionResourceRef{Kind: "RuntimeApproval", Namespace: approval.ObjectMeta.Namespace, Name: approval.ObjectMeta.Name, UID: approval.ObjectMeta.UID, ResourceVersion: fmt.Sprint(approval.ObjectMeta.ResourceVersion)}
	}
	return evidence
}
