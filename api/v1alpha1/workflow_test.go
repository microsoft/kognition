// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestWorkflowSpec_JSONRoundtrip_ExpandedFields(t *testing.T) {
	spec := WorkflowSpec{
		Steps: []WorkflowStep{
			{Name: "plan", AgentRef: LocalObjectReference{Name: "planner"}},
			{Name: "execute", AgentRef: LocalObjectReference{Name: "executor"}},
		},
		Edges:         []WorkflowEdge{{From: "plan", To: "execute"}},
		ApprovalGates: []WorkflowApprovalGate{{From: "plan", To: "execute"}},
		Budgets: &WorkflowBudget{
			MaxDuration:       "30m",
			MaxTokens:         10000,
			MaxIterations:     3,
			MaxTokensPerCycle: 2500,
		},
		Emergent: &WorkflowEmergentSpec{
			Enabled: true,
			Budgets: WorkflowEmergentBudgets{
				MaxSteps:  10,
				MaxTokens: 5000,
				MaxCost:   "12.50",
			},
		},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got WorkflowSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(got.Steps) != 2 {
		t.Fatalf("steps len = %d, want 2", len(got.Steps))
	}
	if len(got.Edges) != 1 || got.Edges[0].From != "plan" || got.Edges[0].To != "execute" {
		t.Fatalf("edges = %+v, want [{plan execute}]", got.Edges)
	}
	if len(got.ApprovalGates) != 1 || got.ApprovalGates[0].From != "plan" || got.ApprovalGates[0].To != "execute" {
		t.Fatalf("approvalGates = %+v, want edge plan->execute", got.ApprovalGates)
	}
	if got.Budgets == nil || got.Budgets.MaxDuration != "30m" || got.Budgets.MaxTokens != 10000 || got.Budgets.MaxIterations != 3 || got.Budgets.MaxTokensPerCycle != 2500 {
		t.Fatalf("budgets = %+v, want {MaxDuration:30m MaxTokens:10000}", got.Budgets)
	}
	if got.Emergent == nil || !got.Emergent.Enabled || got.Emergent.Budgets.MaxSteps != 10 || got.Emergent.Budgets.MaxTokens != 5000 || got.Emergent.Budgets.MaxCost != "12.50" {
		t.Fatalf("emergent = %+v, want enabled with bounded budgets", got.Emergent)
	}
}

func TestWorkflowStatus_JSONRoundtrip_TracksProgress(t *testing.T) {
	status := WorkflowStatus{
		Phase:          PhaseRunning,
		CompletedSteps: []string{"plan"},
		ActiveStep:     "execute",
		Conditions: []Condition{{
			Type:   "Ready",
			Status: "False",
			Reason: "Executing",
		}},
	}

	data, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got WorkflowStatus
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseRunning)
	}
	if len(got.CompletedSteps) != 1 || got.CompletedSteps[0] != "plan" {
		t.Fatalf("completedSteps = %+v, want [plan]", got.CompletedSteps)
	}
	if got.ActiveStep != "execute" {
		t.Fatalf("activeStep = %q, want %q", got.ActiveStep, "execute")
	}
}
