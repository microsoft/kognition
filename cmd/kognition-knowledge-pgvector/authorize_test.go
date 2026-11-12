// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	pgvectorprovider "github.com/kognition-ai/kognition/providers/pgvector"
)

// authorizeFixture is the minimal, committed-binding world the pluginsdk
// handshake would produce before invoking a knowledge query: a live
// KnowledgeBase whose status.BackendBinding matches the class + configuration
// currently on the cluster. It is duplicated (rather than reused from the
// binding_test package) because scopeAuthorizer lives in package main.
type authorizeFixture struct {
	knowledgeBase *k8s.KnowledgeBase
	class         *k8s.KnowledgeBaseClass
	configuration *unstructured.Unstructured
	scope         *protocol.KnowledgeScope
}

func newAuthorizeFixture(t *testing.T) authorizeFixture {
	t.Helper()
	kind := schema.GroupVersionKind{Group: pgvectorprovider.APIGroup, Version: pgvectorprovider.APIVersion, Kind: pgvectorprovider.Kind}
	configuration := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"table": "chunks", "credentialSecretRef": map[string]interface{}{"name": "database"}},
	}}
	configuration.SetGroupVersionKind(kind)
	configuration.SetNamespace("tenant-a")
	configuration.SetName("shared")
	configuration.SetUID("config-uid")
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	class := &k8s.KnowledgeBaseClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "class-uid", Generation: 2},
		Spec: k8s.KnowledgeBaseClassSpec{Provisioner: "pgvector", BackendRef: &k8s.KnowledgeBackendReference{
			APIGroup: kind.Group, Kind: kind.Kind, Name: "shared",
		}},
	}
	knowledgeBase := &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "manuals", UID: "kb-uid", Generation: 10},
		Spec:       k8s.KnowledgeBaseSpec{KnowledgeBaseClassName: class.Name},
		Status: k8s.KnowledgeBaseStatus{Phase: "Ready", Generation: 3, BackendBinding: &k8s.KnowledgeBackendBinding{
			OperationID: "index-run-uid",
			Generation:  3, ProviderName: "pgvector", ClassUID: "class-uid", ClassGeneration: 2,
			ConfigurationUID: "config-uid", ConfigurationDigest: digest,
		}},
	}
	return authorizeFixture{
		knowledgeBase: knowledgeBase,
		class:         class,
		configuration: configuration,
		scope: &protocol.KnowledgeScope{
			Namespace:             knowledgeBase.Namespace,
			KnowledgeBaseUid:      string(knowledgeBase.UID),
			Generation:            knowledgeBase.Status.Generation,
			OperationId:           knowledgeBase.Status.BackendBinding.OperationID,
			ConfigurationApiGroup: kind.Group,
			ConfigurationKind:     kind.Kind,
			ConfigurationName:     configuration.GetName(),
			ConfigurationUid:      string(configuration.GetUID()),
			ConfigurationDigest:   digest,
		},
	}
}

func newAuthorizer(t *testing.T, fixture authorizeFixture) (*scopeAuthorizer, *knowledgeBaseUIDIndex) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(fixture.knowledgeBase, fixture.class, fixture.configuration).
		WithStatusSubresource(&k8s.KnowledgeBase{}).
		Build()
	resolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{{
		ProviderName:      "pgvector",
		ConfigurationKind: schema.GroupVersionKind{Group: pgvectorprovider.APIGroup, Version: pgvectorprovider.APIVersion, Kind: pgvectorprovider.Kind},
	}})
	if err != nil {
		t.Fatal(err)
	}
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.upsert(fixture.knowledgeBase)
	index.MarkSynced()
	return &scopeAuthorizer{index: index, resolver: resolver}, index
}

func TestScopeAuthorizerRejectsNilInputs(t *testing.T) {
	if err := (*scopeAuthorizer)(nil).AuthorizeScope(context.Background(), &protocol.KnowledgeScope{}); status.Code(err) != codes.Internal {
		t.Fatalf("nil authorizer must return Internal, got %v", err)
	}
	incomplete := &scopeAuthorizer{index: newKnowledgeBaseUIDIndex(zap.NewNop())}
	if err := incomplete.AuthorizeScope(context.Background(), &protocol.KnowledgeScope{}); status.Code(err) != codes.Internal {
		t.Fatalf("authorizer without resolver must return Internal, got %v", err)
	}
	incomplete = &scopeAuthorizer{resolver: &knowledge.BindingResolver{}}
	if err := incomplete.AuthorizeScope(context.Background(), &protocol.KnowledgeScope{}); status.Code(err) != codes.Internal {
		t.Fatalf("authorizer without index must return Internal, got %v", err)
	}

	fixture := newAuthorizeFixture(t)
	authorizer, _ := newAuthorizer(t, fixture)
	if err := authorizer.AuthorizeScope(context.Background(), nil); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("nil scope must be PermissionDenied, got %v", err)
	}
}

func TestScopeAuthorizerRelaysIndexErrors(t *testing.T) {
	fixture := newAuthorizeFixture(t)
	authorizer, index := newAuthorizer(t, fixture)

	// Unknown UID → PermissionDenied surfaced from the index.
	unknown := proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	unknown.KnowledgeBaseUid = "not-in-index"
	if err := authorizer.AuthorizeScope(context.Background(), unknown); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown UID must be PermissionDenied, got %v", err)
	}
	// Wrong namespace surfaces as PermissionDenied even if the UID is known.
	wrongNs := proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	wrongNs.Namespace = "tenant-b"
	if err := authorizer.AuthorizeScope(context.Background(), wrongNs); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong namespace must be PermissionDenied, got %v", err)
	}
	// Reset the sync bit → Unavailable is propagated (informer not ready).
	index.synced.Store(false)
	if err := authorizer.AuthorizeScope(context.Background(), fixture.scope); status.Code(err) != codes.Unavailable {
		t.Fatalf("desynced index must be Unavailable, got %v", err)
	}
}

func TestScopeAuthorizerRejectsMissingCommittedOperation(t *testing.T) {
	fixture := newAuthorizeFixture(t)
	fixture.knowledgeBase.Status.BackendBinding.OperationID = ""
	authorizer, _ := newAuthorizer(t, fixture)
	if err := authorizer.AuthorizeScope(context.Background(), fixture.scope); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing committed operation must be FailedPrecondition, got %v", err)
	}
}

func TestScopeAuthorizerRejectsForgedOperation(t *testing.T) {
	fixture := newAuthorizeFixture(t)
	authorizer, _ := newAuthorizer(t, fixture)
	fixture.scope.OperationId = "different-index-run"
	if err := authorizer.AuthorizeScope(context.Background(), fixture.scope); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged operation must be PermissionDenied, got %v", err)
	}
}

func TestScopeAuthorizerHappyPathAndResolverDelegation(t *testing.T) {
	fixture := newAuthorizeFixture(t)
	authorizer, _ := newAuthorizer(t, fixture)

	if err := authorizer.AuthorizeScope(context.Background(), fixture.scope); err != nil {
		t.Fatalf("committed scope must authorize: %v", err)
	}
	// The resolver re-checks generation on every call; a forged bump must
	// be rejected even though the index still resolves the UID.
	forged := proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	forged.Generation = fixture.scope.Generation + 1
	if err := authorizer.AuthorizeScope(context.Background(), forged); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged generation must be PermissionDenied, got %v", err)
	}
	// Mutating a scope field the resolver checks (digest) must also be
	// rejected, proving delegation and not just index-only auth.
	forged = proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	forged.ConfigurationDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := authorizer.AuthorizeScope(context.Background(), forged); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged digest must be PermissionDenied, got %v", err)
	}
}
