// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestCanaryOutcomeJSONClosedSet(t *testing.T) {
	for _, outcome := range []CanaryOutcome{CanaryOutcomePassed, CanaryOutcomeMismatch, CanaryOutcomeExecutionFailed} {
		data, err := json.Marshal(outcome)
		if err != nil {
			t.Fatalf("Marshal(%q) error = %v", outcome, err)
		}
		var decoded CanaryOutcome
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", data, err)
		}
		if decoded != outcome {
			t.Errorf("round trip = %q, want %q", decoded, outcome)
		}
	}

	var outcome CanaryOutcome
	if err := json.Unmarshal([]byte(`"Unexpected"`), &outcome); err == nil {
		t.Fatal("UnmarshalJSON accepted an unknown reasoning canary outcome")
	}
}

func TestCanaryFailureReasonJSONClosedSet(t *testing.T) {
	allowed := []CanaryFailureReason{
		CanaryFailureInvalidConfiguration,
		CanaryFailureBindingChanged,
		CanaryFailureAuthorizationDenied,
		CanaryFailureBudgetDenied,
		CanaryFailureTimedOut,
		CanaryFailureProviderFailed,
		CanaryFailureToolCallRequested,
		CanaryFailureInvalidEncoding,
		CanaryFailureOutputLimitExceeded,
		CanaryFailureAccountingUncertain,
		CanaryFailurePersistenceFailed,
	}
	for _, reason := range allowed {
		data, err := json.Marshal(reason)
		if err != nil {
			t.Fatalf("Marshal(%q) error = %v", reason, err)
		}
		var decoded CanaryFailureReason
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", data, err)
		}
		if decoded != reason {
			t.Errorf("round trip = %q, want %q", decoded, reason)
		}
	}

	var reason CanaryFailureReason
	if err := json.Unmarshal([]byte(`"Unexpected"`), &reason); err == nil {
		t.Fatal("UnmarshalJSON accepted an unknown reasoning canary failure reason")
	}
}

func TestReasoningCanaryResultRejectsInvalidOutcomeReasonCombination(t *testing.T) {
	validResults := []ReasoningCanaryResult{
		{OperationID: "passed", Outcome: CanaryOutcomePassed},
		{OperationID: "mismatch", Outcome: CanaryOutcomeMismatch},
		{OperationID: "failed", Outcome: CanaryOutcomeExecutionFailed, ClosedReason: CanaryFailureProviderFailed},
	}
	for _, result := range validResults {
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("encode valid terminal result: %v", err)
		}
		var decoded ReasoningCanaryResult
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("decode valid terminal result: %v", err)
		}
	}

	tests := []string{
		`{"operationID":"operation-1","outcome":"ExecutionFailed"}`,
		`{"operationID":"operation-1","outcome":"Passed","closedReason":"ProviderFailed"}`,
		`{"operationID":"operation-1","outcome":"OutcomeMismatch","closedReason":"ProviderFailed"}`,
	}
	for _, data := range tests {
		var result ReasoningCanaryResult
		if err := json.Unmarshal([]byte(data), &result); err == nil {
			t.Errorf("invalid terminal result decoded successfully: %s", data)
		}
	}

	invalidResults := []ReasoningCanaryResult{
		{Outcome: CanaryOutcomeExecutionFailed},
		{Outcome: CanaryOutcomePassed, ClosedReason: CanaryFailureProviderFailed},
		{Outcome: CanaryOutcomeMismatch, ClosedReason: CanaryFailureProviderFailed},
		{Outcome: CanaryOutcomeExecutionFailed, ClosedReason: CanaryFailureReason("Unknown")},
	}
	for _, result := range invalidResults {
		if data, err := json.Marshal(result); err == nil {
			t.Errorf("invalid terminal result encoded successfully: %s", data)
		}
	}
}
