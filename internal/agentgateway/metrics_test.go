// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func TestMetricsRecordRequest(t *testing.T) {
	// TC-0304
	InitMetrics()
	recordRequest("exchange", "success", time.Millisecond)
	recordRequest("invoke", "denied", time.Millisecond)
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seenRequests, seenDurations := false, false
	for _, family := range families {
		switch family.GetName() {
		case "kognition_agent_communication_gateway_requests_total":
			seenRequests = len(family.GetMetric()) > 0
		case "kognition_agent_communication_gateway_request_duration_seconds":
			seenDurations = len(family.GetMetric()) > 0
		}
	}
	if !seenRequests || !seenDurations {
		t.Fatalf("gateway metric families missing requests=%t durations=%t", seenRequests, seenDurations)
	}
}

func TestRequestOutcome(t *testing.T) {
	tests := map[int]string{200: "success", 202: "success", 400: "rejected", 401: "denied", 403: "denied", 500: "failed"}
	for status, expected := range tests {
		if actual := requestOutcome(status); actual != expected {
			t.Errorf("requestOutcome(%d) = %q, want %q", status, actual, expected)
		}
	}
}

func TestServerRecordsExchangeRequestOutcome(t *testing.T) {
	server, sessionIssuer, _, _, _, _ := delegationGatewayFixture(t, true)
	session, err := sessionIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: "team-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: "instance-uid", SessionID: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, delegationRequest(t, "/v1/agent-services/capabilities", session, `{"target_namespace":"team-b","target_agent_service":"review-service","invocation_id":"metric-exchange","message":"review this"}`))
	if response.Code != http.StatusOK {
		t.Fatalf("exchange status = %d, body=%s", response.Code, response.Body.String())
	}
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "kognition_agent_communication_gateway_requests_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["operation"] == "exchange" && labels["outcome"] == "success" {
				return
			}
		}
	}
	encoded, _ := json.Marshal(families)
	t.Fatalf("HTTP exchange metric with operation=exchange outcome=success not found: %s", encoded)
}
