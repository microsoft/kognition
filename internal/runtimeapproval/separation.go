// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import api "github.com/kognition-ai/kognition/api/v1alpha1"

// IsSelfDecision reports whether the decider is the authenticated principal that
// initiated the action. Only workflow-executor approvals record an initiator;
// approvals raised by an agent runtime record none, so no comparison is possible
// for them (a documented residual of the access-control plan).
func IsSelfDecision(approval api.RuntimeApproval, issuer, subject string) bool {
	action := approval.Spec.ExecutorAction
	if action == nil || issuer == "" || subject == "" {
		return false
	}
	return action.Initiator.Issuer == issuer && action.Initiator.Subject == subject
}
