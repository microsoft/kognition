// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"reflect"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestWorkflowCollaborationGuidanceAndDescriptionRoundTrip(t *testing.T) {
	in := &api.WorkflowCollaboration{
		Guidance: "Ask the analyst first.",
		Limits:   api.CollaborationLimits{MaxContentBytes: 131072},
		Members: []api.CollaborationMember{
			{Name: "writer", Description: "drafts the answer", AgentRef: api.AgentReference{Name: "agent-a"}},
		},
	}
	k := workflowCollaborationToK8s(in)
	if k.Guidance != in.Guidance || k.Members[0].Description != "drafts the answer" || k.Limits.MaxContentBytes != in.Limits.MaxContentBytes {
		t.Fatalf("guidance or description lost converting to Kubernetes: %+v", k)
	}
	if out := workflowCollaborationFromK8s(k); !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip changed the collaboration:\n got %+v\nwant %+v", out, in)
	}
}

func TestWorkflowCollaborationExternalAgentRefRoundTrip(t *testing.T) {
	in := &api.WorkflowCollaboration{Members: []api.CollaborationMember{
		{Name: "local", AgentRef: api.AgentReference{Name: "agent-a"}},
		{Name: "partner", ExternalAgentRef: &api.LocalObjectReference{Name: "partner-agent"}},
	}}
	k := workflowCollaborationToK8s(in)
	if k.Members[1].ExternalAgentRef == nil || k.Members[1].ExternalAgentRef.Name != "partner-agent" {
		t.Fatalf("externalAgentRef lost converting to Kubernetes: %+v", k.Members[1])
	}
	if k.Members[0].ExternalAgentRef != nil {
		t.Fatal("externalAgentRef invented for an Agent member")
	}
	out := workflowCollaborationFromK8s(k)
	if !reflect.DeepEqual(out.Members, in.Members) {
		t.Fatalf("round trip changed members:\n got %+v\nwant %+v", out.Members, in.Members)
	}
}
