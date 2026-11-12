// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestSolutionValidator_Handles(t *testing.T) {
	v := NewSolutionValidator()
	if !v.Handles("Solution", OperationCreate) {
		t.Fatal("expected to handle Solution CREATE")
	}
	if !v.Handles("Solution", OperationUpdate) {
		t.Fatal("expected to handle Solution UPDATE")
	}
	if v.Handles("Agent", OperationCreate) {
		t.Fatal("should not handle Agent")
	}
}

func TestSolutionValidator_RejectsEmptyGoal(t *testing.T) {
	v := NewSolutionValidator()
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec:       v1alpha1.SolutionSpec{Goal: ""},
	}
	data, _ := json.Marshal(sol)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data})
	if resp.Allowed {
		t.Fatal("expected rejection for empty goal")
	}
}

func TestSolutionValidator_RejectsEmptyConstraint(t *testing.T) {
	v := NewSolutionValidator()
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec:       v1alpha1.SolutionSpec{Goal: "do stuff", Constraints: []string{"valid", ""}},
	}
	data, _ := json.Marshal(sol)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data})
	if resp.Allowed {
		t.Fatal("expected rejection for empty constraint")
	}
}

func TestSolutionValidator_RejectsNegativeSeed(t *testing.T) {
	v := NewSolutionValidator()
	seed := int64(-1)
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec:       v1alpha1.SolutionSpec{Goal: "do stuff", Seed: &seed},
	}
	data, _ := json.Marshal(sol)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data})
	if resp.Allowed {
		t.Fatal("expected rejection for negative seed")
	}
}

func TestSolutionValidator_RejectsInvalidTarget(t *testing.T) {
	v := NewSolutionValidator()
	for _, target := range []*v1alpha1.SolutionTarget{
		{Kind: "Agent"},
		{Kind: "Service", Ref: v1alpha1.LocalObjectReference{Name: "dispatcher"}},
	} {
		sol := v1alpha1.Solution{Spec: v1alpha1.SolutionSpec{Goal: "do stuff", Target: target}}
		data, _ := json.Marshal(sol)
		if resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data}); resp.Allowed {
			t.Fatalf("target %#v was accepted", target)
		}
	}
}

func TestSolutionValidator_AcceptsAgentServiceTarget(t *testing.T) {
	v := NewSolutionValidator()
	solution := v1alpha1.Solution{Spec: v1alpha1.SolutionSpec{
		Goal:   "do stuff",
		Target: &v1alpha1.SolutionTarget{Kind: "AgentService", Ref: v1alpha1.LocalObjectReference{Name: "dispatcher"}},
	}}
	data, _ := json.Marshal(solution)
	if response := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data}); !response.Allowed {
		t.Fatalf("AgentService target was rejected: %s", response.Reason)
	}
}

func TestSolutionValidator_RejectsNegativeTokenBudget(t *testing.T) {
	v := NewSolutionValidator()
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec: v1alpha1.SolutionSpec{
			Goal: "do stuff",
			Overrides: &v1alpha1.SolutionOverrides{
				TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: -100},
			},
		},
	}
	data, _ := json.Marshal(sol)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data})
	if resp.Allowed {
		t.Fatal("expected rejection for negative token budget")
	}
}

func TestSolutionValidator_RejectsInvalidPromotionGates(t *testing.T) {
	v := NewSolutionValidator()
	solution := v1alpha1.Solution{
		Spec: v1alpha1.SolutionSpec{
			Goal: "ship safely",
			PromotionGates: []v1alpha1.SolutionPromotionGate{
				{EvaluationRef: v1alpha1.LocalObjectReference{Name: "quality"}},
				{EvaluationRef: v1alpha1.LocalObjectReference{Name: "quality"}},
				{},
			},
		},
	}
	data, _ := json.Marshal(solution)
	response := v.Validate(context.Background(), &Request{Operation: OperationCreate, Object: data})
	if response.Allowed || !strings.Contains(response.Reason, "duplicates") || !strings.Contains(response.Reason, "is required") {
		t.Fatalf("invalid promotion gates response = %#v", response)
	}
}

func TestSolutionValidator_RejectsNegativeHardQuota(t *testing.T) {
	v := NewSolutionValidator()
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec: v1alpha1.SolutionSpec{
			Goal: "do stuff",
			Overrides: &v1alpha1.SolutionOverrides{
				Hard: map[string]int64{"agents": -1},
			},
		},
	}
	data, _ := json.Marshal(sol)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data})
	if resp.Allowed {
		t.Fatal("expected rejection for negative hard quota")
	}
}

func TestSolutionValidator_AcceptsValidSolution(t *testing.T) {
	v := NewSolutionValidator()
	seed := int64(42)
	sol := v1alpha1.Solution{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "s1", Namespace: "default"},
		Spec: v1alpha1.SolutionSpec{
			Goal:        "summarize documents",
			Constraints: []string{"use only public data"},
			Seed:        &seed,
			Overrides: &v1alpha1.SolutionOverrides{
				TokenBudget: &v1alpha1.TokenBudget{MaxTotalTokens: 1000},
				Hard:        map[string]int64{"agents": 3},
			},
		},
	}
	data, _ := json.Marshal(sol)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "Solution", Object: data})
	if !resp.Allowed {
		t.Fatalf("expected valid solution to be accepted, got: %s", resp.Reason)
	}
}
