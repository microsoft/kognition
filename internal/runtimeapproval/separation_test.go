// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestIsSelfDecision(t *testing.T) {
	initiator := api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "alice"}
	executorApproval := api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{
		ActorKind:      api.RuntimeApprovalActorExecutor,
		ExecutorAction: &api.WorkflowExecutorActionReference{Initiator: initiator},
	}}
	tests := []struct {
		name     string
		approval api.RuntimeApproval
		issuer   string
		subject  string
		want     bool
	}{
		{name: "initiator deciding its own action", approval: executorApproval, issuer: "kubernetes.io/tokenreview", subject: "alice", want: true},
		{name: "different subject", approval: executorApproval, issuer: "kubernetes.io/tokenreview", subject: "bob"},
		{name: "same subject name from another issuer", approval: executorApproval, issuer: "https://other.example", subject: "alice"},
		{name: "no recorded initiator (agent runtime action)", approval: api.RuntimeApproval{}, issuer: "kubernetes.io/tokenreview", subject: "alice"},
		{name: "empty actor never matches an empty initiator", approval: api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{ExecutorAction: &api.WorkflowExecutorActionReference{}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsSelfDecision(test.approval, test.issuer, test.subject); got != test.want {
				t.Fatalf("IsSelfDecision = %v, want %v", got, test.want)
			}
		})
	}
}
