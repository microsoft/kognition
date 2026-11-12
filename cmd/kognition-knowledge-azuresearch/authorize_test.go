// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	azuresearchprovider "github.com/kognition-ai/kognition/providers/azuresearch"
)

func TestScopeAuthorizerCleanupRequiresCommittedReclaimPolicy(t *testing.T) {
	deletionTime := metav1.NewTime(time.Now())
	knowledgeBase := &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "tenant-a", Name: "manuals", UID: "kb-uid", Generation: 2,
			DeletionTimestamp: &deletionTime, Finalizers: []string{"knowledge.kognition.dev/finalizer"},
		},
		Status: k8s.KnowledgeBaseStatus{Generation: 1, BackendBinding: &k8s.KnowledgeBackendBinding{
			Generation: 1, ProviderName: "azuresearch", ReclaimPolicy: k8s.ReclaimPolicyDelete,
			ClassUID: "class-uid", ClassGeneration: 1,
			ConfigurationAPIGroup: azuresearchprovider.APIGroup, ConfigurationKind: azuresearchprovider.Kind,
			ConfigurationName: "shared", ConfigurationUID: "config-uid",
			ConfigurationDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			OperationID:         "operation-1", BackendOperationID: "locator-1",
		}},
	}
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(knowledgeBase).WithStatusSubresource(&k8s.KnowledgeBase{}).Build()
	resolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{{
		ProviderName:      "azuresearch",
		ConfigurationKind: schema.GroupVersionKind{Group: azuresearchprovider.APIGroup, Version: azuresearchprovider.APIVersion, Kind: azuresearchprovider.Kind},
	}})
	if err != nil {
		t.Fatal(err)
	}
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.upsert(knowledgeBase)
	index.MarkSynced()
	authorizer := &scopeAuthorizer{index: index, resolver: resolver}
	request := &protocol.KnowledgeCleanupRequest{
		Scope: &protocol.KnowledgeScope{
			Namespace: "tenant-a", KnowledgeBaseUid: "kb-uid", Generation: 1, OperationId: "operation-1",
			ConfigurationApiGroup: azuresearchprovider.APIGroup, ConfigurationKind: azuresearchprovider.Kind,
			ConfigurationName: "shared", ConfigurationUid: "config-uid",
			ConfigurationDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		ReclaimPolicy:      protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE,
		BackendOperationId: "locator-1",
	}
	if err := authorizer.AuthorizeCleanup(context.Background(), request); err != nil {
		t.Fatalf("committed cleanup policy must authorize: %v", err)
	}
	request.ReclaimPolicy = protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN
	if err := authorizer.AuthorizeCleanup(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged cleanup policy must be PermissionDenied, got %v", err)
	}
}
