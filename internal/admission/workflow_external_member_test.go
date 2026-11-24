// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func externalMemberWorkflow(members ...v1alpha1.CollaborationMember) v1alpha1.Workflow {
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{{
		Name: "review", Type: v1alpha1.WorkflowStepTypeCollaboration, Input: "{{ .Input }}", Timeout: "4m",
		Collaboration: &v1alpha1.WorkflowCollaboration{
			Strategy: v1alpha1.CollaborationStrategySupervisor, Protocol: v1alpha1.CollaborationProtocolA2A,
			Members:             members,
			AllowedInteractions: []v1alpha1.CollaborationInteraction{{From: members[0].Name, To: members[1].Name}},
			Supervisor:          &v1alpha1.CollaborationSupervisor{Coordinator: members[0].Name},
			Limits:              v1alpha1.CollaborationLimits{MaxTurns: 4, MaxMessages: 16, MaxDelegations: 1, MaxDelegationDepth: 1, MaxParallelTasks: 2, MaxTokens: 8192},
		},
	}}
	wf.Spec.Edges = nil
	return wf
}

func TestWorkflowValidator_ExternalAgentMembers(t *testing.T) {
	internal := v1alpha1.CollaborationMember{Name: "writer", AgentRef: v1alpha1.AgentReference{Name: "writer-agent"}}
	external := func(alias, ref string) v1alpha1.CollaborationMember {
		return v1alpha1.CollaborationMember{Name: alias, ExternalAgentRef: &v1alpha1.LocalObjectReference{Name: ref}}
	}
	both := external("both", "remote")
	both.AgentRef = v1alpha1.AgentReference{Name: "local"}
	pinned := external("pinned", "remote")
	pinned.ServiceRef = &v1alpha1.LocalObjectReference{Name: "svc"}
	crossNamespace := external("cross", "remote")
	crossNamespace.AgentRef = v1alpha1.AgentReference{Namespace: "other"}

	tests := []struct {
		name    string
		enabled bool
		members []v1alpha1.CollaborationMember
		want    string // empty means the workflow is admitted
	}{
		{"mixed members", true, []v1alpha1.CollaborationMember{external("remote", "remote-agent"), internal}, ""},
		{"all external", true, []v1alpha1.CollaborationMember{external("a", "one"), external("b", "two")}, ""},
		{"feature disabled", false, []v1alpha1.CollaborationMember{external("remote", "remote-agent"), internal}, "externalAgentRef is not enabled"},
		{"both references", true, []v1alpha1.CollaborationMember{both, internal}, "exactly one of agentRef and externalAgentRef"},
		{"neither reference", true, []v1alpha1.CollaborationMember{{Name: "empty"}, internal}, "agentRef.name is required"},
		{"empty name", true, []v1alpha1.CollaborationMember{{Name: "empty", ExternalAgentRef: &v1alpha1.LocalObjectReference{}}, internal}, "externalAgentRef.name is required"},
		{"serviceRef", true, []v1alpha1.CollaborationMember{pinned, internal}, "serviceRef is not valid with externalAgentRef"},
		{"agent namespace", true, []v1alpha1.CollaborationMember{crossNamespace, internal}, "agentRef must be empty"},
		{"duplicate external agent", true, []v1alpha1.CollaborationMember{external("a", "same"), external("b", "same")}, "already used by member"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := NewWorkflowValidator(WithCollaborationEnabled(true), WithExternalAgentsEnabled(tc.enabled))
			response := v.Validate(context.Background(), workflowRequest(externalMemberWorkflow(tc.members...)))
			if tc.want == "" {
				if !response.Allowed {
					t.Fatalf("expected admission, got: %s", response.Reason)
				}
				return
			}
			if response.Allowed || !strings.Contains(response.Reason, tc.want) {
				t.Fatalf("expected rejection containing %q, got allowed=%t reason=%q", tc.want, response.Allowed, response.Reason)
			}
		})
	}
}

func TestWorkflowValidator_ExternalAgentsAreOffByDefault(t *testing.T) {
	member := v1alpha1.CollaborationMember{Name: "remote", ExternalAgentRef: &v1alpha1.LocalObjectReference{Name: "remote-agent"}}
	internal := v1alpha1.CollaborationMember{Name: "writer", AgentRef: v1alpha1.AgentReference{Name: "writer-agent"}}
	response := NewWorkflowValidator(WithCollaborationEnabled(true)).Validate(context.Background(), workflowRequest(externalMemberWorkflow(member, internal)))
	if response.Allowed {
		t.Fatal("an ExternalAgent member was admitted without the operator enabling external agents")
	}
}
