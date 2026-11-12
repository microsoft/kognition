// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package webhook

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEnvtest_RuntimeApprovalRetentionWorkspaceBinding(t *testing.T) {
	installHooks(nil, nil)
	for _, kind := range []string{"RuntimeApprovalQuotaTicket", "RuntimeApprovalTombstone"} {
		for _, referenceNamespace := range []string{"default", "other", ""} {
			t.Run(kind+"/namespace="+referenceNamespace, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				agentRef := map[string]any{
					"kind": "Agent", "scope": "Namespaced", "name": "agent",
					"uid": "agent-uid", "resourceVersion": int64(1),
				}
				if referenceNamespace != "" {
					agentRef["namespace"] = referenceNamespace
				}
				spec := map[string]any{
					"workspace": "default", "invocationID": "retention-reference",
					"agentRef": agentRef, "createdAt": "2026-01-01T00:00:00Z",
					"binding": map[string]any{
						"format": "kognition-runtime-action-v1", "algorithm": "HMAC-SHA-256",
						"domain": "kognition.dev/runtime-approval/action/v1",
						"epoch":  int64(1), "keyVersion": int64(1), "tag": strings.Repeat("a", 43),
					},
				}
				if kind == "RuntimeApprovalQuotaTicket" {
					spec["phase"] = "Live"
					spec["chargedBytes"] = int64(1)
				} else {
					spec["plane"] = "Tool"
					spec["operation"] = "invoke"
					spec["runtimeApprovalUID"] = "approval-uid"
					spec["initialDecision"] = "Pending"
					spec["decisionState"] = "Rejected"
					spec["executionState"] = "NotStarted"
					spec["expiresAt"] = "2026-01-01T00:05:00Z"
					spec["compactedAt"] = "2026-01-01T00:10:00Z"
					spec["retentionDeadline"] = "2026-01-02T00:00:00Z"
				}
				resource := &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "kognition.dev/v1alpha1", "kind": kind,
					"metadata": map[string]any{"name": "retention-reference", "namespace": "default"},
					"spec":     spec,
				}}
				err := envState.ctrlClient.Create(ctx, resource, client.DryRunAll)
				if referenceNamespace == "default" {
					if err != nil {
						t.Fatalf("valid workspace reference rejected: %v", err)
					}
				} else if !apierrors.IsInvalid(err) {
					t.Fatalf("expected invalid workspace reference, got: %v", err)
				}
			})
		}
	}
}
