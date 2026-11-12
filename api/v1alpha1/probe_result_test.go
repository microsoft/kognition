// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestCanaryFailureReasonJSONClosedSet(t *testing.T) {
	allowed := []CanaryFailureReason{
		0,
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
		t.Run(reason.String(), func(t *testing.T) {
			data, err := json.Marshal(reason)
			if err != nil {
				t.Fatal(err)
			}
			var decoded CanaryFailureReason
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error = %v", data, err)
			}
			if decoded != reason {
				t.Fatalf("decoded reason = %q, want %q", decoded, reason)
			}
		})
	}

	var reason CanaryFailureReason
	if err := json.Unmarshal([]byte(`"InputTokenLimitExceeded"`), &reason); err == nil {
		t.Fatal("unknown failure reason decoded successfully")
	}
	if err := json.Unmarshal([]byte(`42`), &reason); err == nil {
		t.Fatal("non-string failure reason decoded successfully")
	}
	if _, err := json.Marshal(CanaryFailureReason(255)); err == nil {
		t.Fatal("unknown in-memory failure reason encoded successfully")
	}
}

func TestCanaryOutcomeJSONClosedSet(t *testing.T) {
	for _, outcome := range []CanaryOutcome{CanaryOutcomePassed, CanaryOutcomeMismatch, CanaryOutcomeExecutionFailed} {
		data, err := json.Marshal(outcome)
		if err != nil {
			t.Fatal(err)
		}
		var decoded CanaryOutcome
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("UnmarshalJSON(%s) error = %v", data, err)
		}
		if decoded != outcome {
			t.Fatalf("decoded outcome = %q, want %q", decoded, outcome)
		}
	}

	var outcome CanaryOutcome
	if err := json.Unmarshal([]byte(`"Unknown"`), &outcome); err == nil {
		t.Fatal("unknown outcome decoded successfully")
	}
	if err := json.Unmarshal([]byte(`42`), &outcome); err == nil {
		t.Fatal("non-string outcome decoded successfully")
	}
}

func TestReasoningCanaryResultRejectsUnknownFailureReason(t *testing.T) {
	var result ReasoningCanaryResult
	if err := json.Unmarshal([]byte(`{"operationID":"operation-1","outcome":"ExecutionFailed","closedReason":"PreflightFailed"}`), &result); err == nil {
		t.Fatal("result with unknown failure reason decoded successfully")
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
		{Outcome: CanaryOutcomeExecutionFailed, ClosedReason: CanaryFailureReason(255)},
	}
	for _, result := range invalidResults {
		if data, err := json.Marshal(result); err == nil {
			t.Errorf("invalid terminal result encoded successfully: %s", data)
		}
	}
}
