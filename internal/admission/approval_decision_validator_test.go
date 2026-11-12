// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func makeApprovalDecisionForTest(name, approver, decision string) v1alpha1.ApprovalDecision {
	return v1alpha1.ApprovalDecision{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ApprovalDecision"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1alpha1.ApprovalDecisionSpec{
			WorkflowRunRef: v1alpha1.LocalObjectReference{Name: "wf-run"},
			Step:           "review",
			Iteration:      0,
			Decision:       decision,
			Approver:       approver,
		},
	}
}

func TestApprovalDecisionValidator_CreateAllowedWhenApproverMatchesUser(t *testing.T) {
	v := NewApprovalDecisionValidator()
	ad := makeApprovalDecisionForTest("ad-1", "alice", "approve")
	data, _ := json.Marshal(ad)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "ApprovalDecision",
		Namespace: "default",
		Name:      "ad-1",
		Object:    data,
		UserInfo:  UserInfo{Username: "alice"},
	}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected create to be allowed, got reason: %s", resp.Reason)
	}
}

func TestApprovalDecisionValidator_CreateAllowedWhenUserInfoEmpty(t *testing.T) {
	v := NewApprovalDecisionValidator()
	ad := makeApprovalDecisionForTest("ad-2", "alice", "approve")
	data, _ := json.Marshal(ad)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "ApprovalDecision",
		Namespace: "default",
		Name:      "ad-2",
		Object:    data,
		// UserInfo intentionally zero-valued (apiserver path).
	}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected create to be allowed when UserInfo not plumbed, got reason: %s", resp.Reason)
	}
}

func TestApprovalDecisionValidator_CreateDeniedWhenApproverMismatch(t *testing.T) {
	v := NewApprovalDecisionValidator()
	ad := makeApprovalDecisionForTest("ad-3", "alice", "approve")
	data, _ := json.Marshal(ad)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "ApprovalDecision",
		Namespace: "default",
		Name:      "ad-3",
		Object:    data,
		UserInfo:  UserInfo{Username: "mallory"},
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected create to be denied when approver != user")
	}
	if resp.Reason == "" {
		t.Fatal("expected denial reason")
	}
}

func TestApprovalDecisionValidator_CreateDeniedWhenApproverEmpty(t *testing.T) {
	v := NewApprovalDecisionValidator()
	ad := makeApprovalDecisionForTest("ad-4", "", "approve")
	data, _ := json.Marshal(ad)
	req := &Request{
		Operation: OperationCreate,
		Kind:      "ApprovalDecision",
		Namespace: "default",
		Name:      "ad-4",
		Object:    data,
		UserInfo:  UserInfo{Username: "alice"},
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected create to be denied when approver is empty")
	}
}

func TestApprovalDecisionValidator_UpdateAllowedWhenSpecUnchanged(t *testing.T) {
	v := NewApprovalDecisionValidator()
	ad := makeApprovalDecisionForTest("ad-5", "alice", "approve")
	old := ad
	ad.ObjectMeta.Labels = map[string]string{"updated": "true"}

	newData, _ := json.Marshal(ad)
	oldData, _ := json.Marshal(old)
	req := &Request{
		Operation: OperationUpdate,
		Kind:      "ApprovalDecision",
		Namespace: "default",
		Name:      "ad-5",
		Object:    newData,
		OldObject: oldData,
		UserInfo:  UserInfo{Username: "alice"},
	}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected update to be allowed when spec unchanged, got reason: %s", resp.Reason)
	}
}

func TestApprovalDecisionValidator_UpdateDeniedWhenSpecChanges(t *testing.T) {
	v := NewApprovalDecisionValidator()
	old := makeApprovalDecisionForTest("ad-6", "alice", "approve")
	updated := old
	updated.Spec.Decision = "deny"

	newData, _ := json.Marshal(updated)
	oldData, _ := json.Marshal(old)
	req := &Request{
		Operation: OperationUpdate,
		Kind:      "ApprovalDecision",
		Namespace: "default",
		Name:      "ad-6",
		Object:    newData,
		OldObject: oldData,
		UserInfo:  UserInfo{Username: "alice"},
	}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected update to be denied when spec changes")
	}
	if resp.Reason == "" {
		t.Fatal("expected denial reason")
	}
}
