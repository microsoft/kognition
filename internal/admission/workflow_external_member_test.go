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
	coordinator, other := members[0].Name, members[1].Name
	for i, m := range members {
		if m.ExternalAgentRef == nil {
			coordinator = m.Name
			other = members[(i+1)%len(members)].Name
			break
		}
	}
	wf := validWorkflow()
	wf.Spec.Steps = []v1alpha1.WorkflowStep{{
		Name: "review", Type: v1alpha1.WorkflowStepTypeCollaboration, Input: "{{ .Input }}", Timeout: "4m",
		Collaboration: &v1alpha1.WorkflowCollaboration{
			Strategy: v1alpha1.CollaborationStrategySupervisor, Protocol: v1alpha1.CollaborationProtocolA2A,
			Members:             members,
			AllowedInteractions: []v1alpha1.CollaborationInteraction{{From: coordinator, To: other}},
			Supervisor:          &v1alpha1.CollaborationSupervisor{Coordinator: coordinator},
			Limits:              v1alpha1.CollaborationLimits{MaxTurns: 4, MaxMessages: 16, MaxDelegations: 1, MaxDelegationDepth: 1, MaxParallelTasks: 2, MaxTokens: 8192, MaxContentBytes: 8192},
		},
	}}
	wf.Spec.Edges = nil
	return wf
}

func TestWorkflowValidator_CollaborationStepRequiresInput(t *testing.T) {
	local := func(name string) v1alpha1.CollaborationMember {
		return v1alpha1.CollaborationMember{Name: name, AgentRef: v1alpha1.AgentReference{Name: name + "-agent"}}
	}

	wf := externalMemberWorkflow(local("lead"), local("other"))
	wf.Spec.Steps[0].Input = "  "
	response := NewWorkflowValidator(WithCollaborationEnabled(true)).Validate(context.Background(), workflowRequest(wf))
	if response.Allowed || !strings.Contains(response.Reason, "input is required for collaboration steps") {
		t.Fatalf("expected rejection for empty input, got allowed=%t reason=%q", response.Allowed, response.Reason)
	}
}

func TestWorkflowValidator_CollaborationRequiresUserContentLimit(t *testing.T) {
	for _, limit := range []int64{0, -1, 1, 131072, 1<<63 - 1} {
		wf := externalMemberWorkflow(
			v1alpha1.CollaborationMember{Name: "lead", AgentRef: v1alpha1.AgentReference{Name: "lead-agent"}},
			v1alpha1.CollaborationMember{Name: "other", AgentRef: v1alpha1.AgentReference{Name: "other-agent"}},
		)
		wf.Spec.Steps[0].Collaboration.Limits.MaxContentBytes = limit
		response := NewWorkflowValidator(WithCollaborationEnabled(true)).Validate(context.Background(), workflowRequest(wf))
		if limit < 1 {
			if response.Allowed || !strings.Contains(response.Reason, "limits.maxContentBytes") {
				t.Fatalf("invalid limit %d was accepted: %+v", limit, response)
			}
		} else if !response.Allowed {
			t.Fatalf("positive user-defined limit %d was rejected: %s", limit, response.Reason)
		}
	}
}

func TestWorkflowValidator_GuidanceAndDescriptionLimits(t *testing.T) {
	local := func(name, description string) v1alpha1.CollaborationMember {
		return v1alpha1.CollaborationMember{Name: name, Description: description, AgentRef: v1alpha1.AgentReference{Name: name + "-agent"}}
	}
	tests := []struct {
		name     string
		guidance string
		desc     string
		want     string
	}{
		{"accepted", "Ask the analyst.\nThen the reviewer.", "checks facts", ""},
		{"guidance at the limit", strings.Repeat("\u00e9", v1alpha1.CollaborationMaxGuidanceChars), "", ""},
		{"guidance too long", strings.Repeat("g", v1alpha1.CollaborationMaxGuidanceChars+1), "", "guidance must not exceed"},
		{"guidance control character", "bad\x07text", "", "guidance must not contain control characters"},
		{"description too long", "", strings.Repeat("d", v1alpha1.CollaborationMaxMemberDescriptionChars+1), "description must not exceed"},
		{"description newline", "", "two\nlines", "description must not contain control characters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wf := externalMemberWorkflow(local("lead", tc.desc), local("other", ""))
			wf.Spec.Steps[0].Collaboration.Guidance = tc.guidance
			response := NewWorkflowValidator(WithCollaborationEnabled(true)).Validate(context.Background(), workflowRequest(wf))
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
		{"all external", true, []v1alpha1.CollaborationMember{external("a", "one"), external("b", "two")}, "cannot hold this role"},
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

func TestWorkflowValidator_ExternalAgentsCannotHoldDecidingRoles(t *testing.T) {
	local := func(name string) v1alpha1.CollaborationMember {
		return v1alpha1.CollaborationMember{Name: name, AgentRef: v1alpha1.AgentReference{Name: name + "-agent"}}
	}
	remote := func(name string) v1alpha1.CollaborationMember {
		return v1alpha1.CollaborationMember{Name: name, ExternalAgentRef: &v1alpha1.LocalObjectReference{Name: name + "-ext"}}
	}
	tests := []struct {
		name      string
		strategy  v1alpha1.CollaborationStrategy
		members   []v1alpha1.CollaborationMember
		edges     []v1alpha1.CollaborationInteraction
		configure func(*v1alpha1.WorkflowCollaboration)
		want      string
	}{
		{"supervisor coordinator", v1alpha1.CollaborationStrategySupervisor, []v1alpha1.CollaborationMember{remote("lead"), local("a")},
			[]v1alpha1.CollaborationInteraction{{From: "lead", To: "a"}},
			func(c *v1alpha1.WorkflowCollaboration) {
				c.Supervisor = &v1alpha1.CollaborationSupervisor{Coordinator: "lead"}
			},
			"supervisor.coordinator \"lead\" must be an agentRef member"},
		{"parallel synthesizer", v1alpha1.CollaborationStrategyParallel, []v1alpha1.CollaborationMember{local("a"), local("b"), remote("s")}, nil,
			func(c *v1alpha1.WorkflowCollaboration) {
				c.Parallel = &v1alpha1.CollaborationParallel{Synthesizer: "s", Branches: []v1alpha1.CollaborationBranch{{Member: "a"}, {Member: "b"}}}
			},
			"parallel.synthesizer \"s\" must be an agentRef member"},
		{"peer finalizer", v1alpha1.CollaborationStrategyPeer, []v1alpha1.CollaborationMember{remote("fin"), local("b")},
			[]v1alpha1.CollaborationInteraction{{From: "b", To: "fin"}},
			func(c *v1alpha1.WorkflowCollaboration) { c.Peer = &v1alpha1.CollaborationPeer{Finalizer: "fin"} },
			"peer.finalizer \"fin\" must be an agentRef member"},
		{"external branch with local synthesizer", v1alpha1.CollaborationStrategyParallel, []v1alpha1.CollaborationMember{local("a"), remote("x"), local("s")}, nil,
			func(c *v1alpha1.WorkflowCollaboration) {
				c.Parallel = &v1alpha1.CollaborationParallel{Synthesizer: "s", Branches: []v1alpha1.CollaborationBranch{{Member: "a"}, {Member: "x"}}}
			},
			""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wf := externalMemberWorkflow(local("seed"), local("other"))
			c := wf.Spec.Steps[0].Collaboration
			c.Strategy, c.Members, c.AllowedInteractions, c.Supervisor = tc.strategy, tc.members, tc.edges, nil
			tc.configure(c)
			v := NewWorkflowValidator(WithCollaborationEnabled(true), WithExternalAgentsEnabled(true))
			response := v.Validate(context.Background(), workflowRequest(wf))
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

func TestWorkflowValidator_ExternalAgentsNeedCollaboration(t *testing.T) {
	member := v1alpha1.CollaborationMember{Name: "remote", ExternalAgentRef: &v1alpha1.LocalObjectReference{Name: "remote-agent"}}
	internal := v1alpha1.CollaborationMember{Name: "writer", AgentRef: v1alpha1.AgentReference{Name: "writer-agent"}}
	response := NewWorkflowValidator(WithExternalAgentsEnabled(true)).Validate(context.Background(), workflowRequest(externalMemberWorkflow(member, internal)))
	if response.Allowed {
		t.Fatal("a collaboration step was admitted where collaboration is disabled")
	}
	if !strings.Contains(response.Reason, "collaboration") {
		t.Fatalf("reason = %q, want it to name collaboration", response.Reason)
	}
}
