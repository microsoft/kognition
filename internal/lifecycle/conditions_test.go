// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

import "testing"

func TestSetConditionAddsNew(t *testing.T) {
	var conditions []Condition
	conditions = SetCondition(conditions, ConditionReady, ConditionTrue, "AllGood", "")
	if len(conditions) != 1 {
		t.Fatalf("want 1 condition, got %d", len(conditions))
	}
	if conditions[0].Type != string(ConditionReady) {
		t.Fatalf("want type Ready, got %s", conditions[0].Type)
	}
}

func TestSetConditionUpdatesExisting(t *testing.T) {
	conditions := SetCondition(nil, ConditionReady, ConditionFalse, "NotReady", "")
	conditions = SetCondition(conditions, ConditionReady, ConditionTrue, "NowReady", "")
	if len(conditions) != 1 {
		t.Fatalf("want 1 condition, got %d", len(conditions))
	}
	if conditions[0].Status != string(ConditionTrue) {
		t.Fatal("want status True")
	}
	if conditions[0].Reason != "NowReady" {
		t.Fatalf("want reason NowReady, got %s", conditions[0].Reason)
	}
}

func TestGetCondition(t *testing.T) {
	conditions := SetCondition(nil, ConditionReady, ConditionTrue, "OK", "")
	conditions = SetCondition(conditions, ConditionHealthy, ConditionFalse, "Bad", "")

	c := GetCondition(conditions, ConditionHealthy)
	if c == nil {
		t.Fatal("want non-nil condition")
	}
	if c.Status != string(ConditionFalse) {
		t.Fatal("want status False")
	}

	if GetCondition(conditions, ConditionProgressing) != nil {
		t.Fatal("want nil for missing condition")
	}
}

func TestRemoveCondition(t *testing.T) {
	conditions := SetCondition(nil, ConditionReady, ConditionTrue, "OK", "")
	conditions = SetCondition(conditions, ConditionHealthy, ConditionTrue, "OK", "")
	conditions = RemoveCondition(conditions, ConditionReady)
	if len(conditions) != 1 {
		t.Fatalf("want 1 condition, got %d", len(conditions))
	}
	if conditions[0].Type != string(ConditionHealthy) {
		t.Fatal("remaining condition should be Healthy")
	}
}

func TestIsConditionTrue(t *testing.T) {
	conditions := SetCondition(nil, ConditionReady, ConditionTrue, "OK", "")
	if !IsConditionTrue(conditions, ConditionReady) {
		t.Fatal("want true")
	}
	if IsConditionTrue(conditions, ConditionHealthy) {
		t.Fatal("want false for missing")
	}
}

func TestIsConditionFalse(t *testing.T) {
	conditions := SetCondition(nil, ConditionReady, ConditionFalse, "Bad", "")
	if !IsConditionFalse(conditions, ConditionReady) {
		t.Fatal("want true")
	}
	if IsConditionFalse(conditions, ConditionHealthy) {
		t.Fatal("want false for missing")
	}
}

func TestSetConditionPreservesTransitionTime(t *testing.T) {
	// Set condition with status True — records a transition time.
	conditions := SetCondition(nil, ConditionReady, ConditionTrue, "Ready", "")
	originalTime := conditions[0].LastTransitionTime
	if originalTime == "" {
		t.Fatal("expected LastTransitionTime to be set")
	}

	// Set the same condition with the same status — LastTransitionTime should be preserved.
	conditions = SetCondition(conditions, ConditionReady, ConditionTrue, "StillReady", "")
	if conditions[0].LastTransitionTime != originalTime {
		t.Fatalf("LastTransitionTime changed: want %q, got %q", originalTime, conditions[0].LastTransitionTime)
	}

	// Manually set a known past timestamp so we can detect the change reliably.
	conditions[0].LastTransitionTime = "2020-01-01T00:00:00Z"

	// Change the status — LastTransitionTime should be updated (no longer the old value).
	conditions = SetCondition(conditions, ConditionReady, ConditionFalse, "NotReady", "")
	if conditions[0].LastTransitionTime == "2020-01-01T00:00:00Z" {
		t.Fatal("LastTransitionTime should have changed on status transition")
	}
}

func TestSetConditionMessage(t *testing.T) {
	conditions := SetCondition(nil, ConditionReady, ConditionTrue, "OK", "all systems operational")
	if conditions[0].Message != "all systems operational" {
		t.Fatalf("want message 'all systems operational', got %q", conditions[0].Message)
	}
}

func TestIsConditionTrueWithFalseStatus(t *testing.T) {
	// Condition exists but status is False — IsConditionTrue should return false.
	conditions := SetCondition(nil, ConditionReady, ConditionFalse, "Bad", "")
	if IsConditionTrue(conditions, ConditionReady) {
		t.Fatal("IsConditionTrue should return false when status is False")
	}
}

func TestIsConditionFalseWithTrueStatus(t *testing.T) {
	// Condition exists but status is True — IsConditionFalse should return false.
	conditions := SetCondition(nil, ConditionReady, ConditionTrue, "OK", "")
	if IsConditionFalse(conditions, ConditionReady) {
		t.Fatal("IsConditionFalse should return false when status is True")
	}
}
