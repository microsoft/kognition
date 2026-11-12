// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestGateway_SubmitGoal_Success(t *testing.T) {
	h := newTestHarness(t)

	gw := makeIntentGateway("gw1", testNS, []v1alpha1.IntentRoute{
		{Pattern: "deploy", TargetRef: v1alpha1.LocalObjectReference{Name: "agent1"}, TargetKind: "Agent"},
	}, v1alpha1.PhaseReady)
	seedResource(t, h, "IntentGateway", testNS, "gw1", gw)
	seedResource(t, h, "Agent", testNS, "agent1", makeAgent("agent1", testNS, "model1", v1alpha1.PhaseReady))

	resp := h.doReq(t, http.MethodPost, apiBase+"/intentgateways/gw1/goals", map[string]any{
		"goal": "deploy my application",
	})

	if resp.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", resp.Code, resp.Body.String())
	}
	var response struct {
		SolutionRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"solutionRef"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode goal response: %v", err)
	}
	var solution v1alpha1.Solution
	h.getResource(t, "Solution", response.SolutionRef.Namespace, response.SolutionRef.Name, &solution)
	if solution.Spec.Target == nil || solution.Spec.Target.Kind != "Agent" || solution.Spec.Target.Ref.Name != "agent1" {
		t.Fatalf("solution target = %#v, want Agent/agent1", solution.Spec.Target)
	}
}

func TestGateway_SubmitGoal_GatewayNotFound(t *testing.T) {
	h := newTestHarness(t)

	resp := h.doReq(t, http.MethodPost, apiBase+"/intentgateways/missing/goals", map[string]any{
		"goal": "do something",
	})

	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestGateway_SubmitGoal_GatewayNotReady(t *testing.T) {
	h := newTestHarness(t)

	gw := makeIntentGateway("gw2", testNS, []v1alpha1.IntentRoute{
		{Pattern: "deploy", TargetRef: v1alpha1.LocalObjectReference{Name: "agent1"}, TargetKind: "Agent"},
	}, v1alpha1.PhasePending)
	seedResource(t, h, "IntentGateway", testNS, "gw2", gw)

	resp := h.doReq(t, http.MethodPost, apiBase+"/intentgateways/gw2/goals", map[string]any{
		"goal": "deploy something",
	})

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestGateway_SubmitGoal_EmptyGoal(t *testing.T) {
	h := newTestHarness(t)

	gw := makeIntentGateway("gw3", testNS, []v1alpha1.IntentRoute{
		{Pattern: "deploy", TargetRef: v1alpha1.LocalObjectReference{Name: "agent1"}, TargetKind: "Agent"},
	}, v1alpha1.PhaseReady)
	seedResource(t, h, "IntentGateway", testNS, "gw3", gw)

	resp := h.doReq(t, http.MethodPost, apiBase+"/intentgateways/gw3/goals", map[string]any{
		"goal": "",
	})

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.Code, resp.Body.String())
	}
}

func TestGateway_SubmitGoal_NoRouteMatched(t *testing.T) {
	h := newTestHarness(t)

	gw := makeIntentGateway("gw4", testNS, []v1alpha1.IntentRoute{
		{Pattern: "deploy", TargetRef: v1alpha1.LocalObjectReference{Name: "agent1"}, TargetKind: "Agent"},
	}, v1alpha1.PhaseReady)
	seedResource(t, h, "IntentGateway", testNS, "gw4", gw)

	resp := h.doReq(t, http.MethodPost, apiBase+"/intentgateways/gw4/goals", map[string]any{
		"goal": "analyze some data",
	})

	if resp.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", resp.Code, resp.Body.String())
	}
}
