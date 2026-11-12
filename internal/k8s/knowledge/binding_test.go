// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledge_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var configurationKind = schema.GroupVersionKind{Group: "pgvector.knowledge.kognition.dev", Version: "v1alpha1", Kind: "PgvectorBackend"}
var knowledgeKey = types.NamespacedName{Namespace: "tenant-a", Name: "manuals"}

func bindingObjects(t *testing.T) (*k8s.KnowledgeBase, *k8s.KnowledgeBaseClass, *unstructured.Unstructured) {
	t.Helper()
	configuration := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"table": "chunks", "credentialSecretRef": map[string]interface{}{"name": "database"}},
	}}
	configuration.SetGroupVersionKind(configurationKind)
	configuration.SetNamespace(knowledgeKey.Namespace)
	configuration.SetName("shared")
	configuration.SetUID("config-uid")
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	class := &k8s.KnowledgeBaseClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "class-uid", Generation: 2},
		Spec: k8s.KnowledgeBaseClassSpec{Provisioner: "pgvector", BackendRef: &k8s.KnowledgeBackendReference{
			APIGroup: configurationKind.Group, Kind: configurationKind.Kind, Name: "shared",
		}},
	}
	knowledgeBase := &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: knowledgeKey.Name, Namespace: knowledgeKey.Namespace, UID: "kb-uid", Generation: 10},
		Spec:       k8s.KnowledgeBaseSpec{KnowledgeBaseClassName: class.Name},
		Status: k8s.KnowledgeBaseStatus{Phase: "Ready", Generation: 3, BackendBinding: &k8s.KnowledgeBackendBinding{
			Generation: 3, ProviderName: "pgvector", ClassUID: "class-uid", ClassGeneration: 2,
			ReclaimPolicy:         k8s.ReclaimPolicyRetain,
			ConfigurationAPIGroup: configurationKind.Group, ConfigurationKind: configurationKind.Kind, ConfigurationName: "shared",
			ConfigurationUID: "config-uid", ConfigurationDigest: digest, OperationID: "team-a/kb-run-3", BackendOperationID: "backend-operation",
		}},
	}
	return knowledgeBase, class, configuration
}

func bindingResolver(t *testing.T, objects ...client.Object) (*knowledge.BindingResolver, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&k8s.KnowledgeBase{}).Build()
	resolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{{ProviderName: "pgvector", ConfigurationKind: configurationKind}})
	if err != nil {
		t.Fatal(err)
	}
	return resolver, reader
}

func TestBindingResolvesCommittedGeneration(t *testing.T) {
	for _, phase := range []string{"Ready", "Refreshing", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			knowledgeBase, class, configuration := bindingObjects(t)
			knowledgeBase.Status.Phase = phase
			resolver, _ := bindingResolver(t, knowledgeBase, class, configuration)
			binding, err := resolver.Resolve(context.Background(), knowledgeKey)
			if err != nil {
				t.Fatal(err)
			}
			if binding.ProviderName != "pgvector" || binding.Scope.Generation != 3 || binding.Scope.KnowledgeBaseUid != "kb-uid" || binding.Scope.ConfigurationUid != "config-uid" || binding.Scope.OperationId != "team-a/kb-run-3" {
				t.Fatalf("incorrect committed binding: %+v", binding)
			}
			if err := resolver.AuthorizeScope(context.Background(), knowledgeKey, binding.Scope); err != nil {
				t.Fatal(err)
			}
			binding.Scope.Generation = 99
			if status.Code(resolver.AuthorizeScope(context.Background(), knowledgeKey, binding.Scope)) != codes.PermissionDenied {
				t.Fatal("accepted uncommitted scope generation")
			}
		})
	}
}

func TestBindingResolvesTrustedPendingGeneration(t *testing.T) {
	knowledgeBase, class, configuration := bindingObjects(t)
	knowledgeBase.Status.Phase = "Refreshing"
	resolver, _ := bindingResolver(t, knowledgeBase, class, configuration)

	binding, err := resolver.ResolvePending(context.Background(), knowledgeKey)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if binding.ProviderName != "pgvector" || binding.ClassUID != "class-uid" || binding.ClassGeneration != 2 {
		t.Fatalf("incorrect pending identity: %+v", binding)
	}
	wantScope := &protocol.KnowledgeScope{
		Namespace: knowledgeKey.Namespace, KnowledgeBaseUid: "kb-uid", Generation: 4,
		ConfigurationApiGroup: configurationKind.Group, ConfigurationKind: configurationKind.Kind,
		ConfigurationName: "shared", ConfigurationUid: "config-uid", ConfigurationDigest: digest,
	}
	if !proto.Equal(binding.Scope, wantScope) {
		t.Fatalf("pending scope = %+v, want %+v", binding.Scope, wantScope)
	}
	if err := resolver.AuthorizePendingScope(context.Background(), knowledgeKey, binding.Scope); err != nil {
		t.Fatal(err)
	}
	binding.Scope.Generation++
	if status.Code(resolver.AuthorizePendingScope(context.Background(), knowledgeKey, binding.Scope)) != codes.PermissionDenied {
		t.Fatal("accepted mismatched pending scope generation")
	}
}

func TestBindingResolvesCleanupFromDeletingKnowledgeBase(t *testing.T) {
	knowledgeBase, _, _ := bindingObjects(t)
	stamp := metav1.Now()
	knowledgeBase.DeletionTimestamp = &stamp
	knowledgeBase.Finalizers = []string{"test"}
	resolver, _ := bindingResolver(t, knowledgeBase)

	binding, err := resolver.ResolveCleanup(context.Background(), knowledgeKey)
	if err != nil {
		t.Fatal(err)
	}
	wantScope := &protocol.KnowledgeScope{
		Namespace: knowledgeKey.Namespace, KnowledgeBaseUid: "kb-uid", Generation: 3,
		OperationId: "team-a/kb-run-3", ConfigurationApiGroup: configurationKind.Group,
		ConfigurationKind: configurationKind.Kind, ConfigurationName: "shared",
		ConfigurationUid: "config-uid", ConfigurationDigest: knowledgeBase.Status.BackendBinding.ConfigurationDigest,
	}
	if binding.ProviderName != "pgvector" || !proto.Equal(binding.Scope, wantScope) {
		t.Fatalf("cleanup binding = %+v, want provider pgvector and scope %+v", binding, wantScope)
	}
	if err := resolver.AuthorizeCleanupScope(context.Background(), knowledgeKey, binding.Scope); err != nil {
		t.Fatal(err)
	}
	forged := proto.Clone(binding.Scope).(*protocol.KnowledgeScope)
	forged.OperationId = "other-operation"
	if status.Code(resolver.AuthorizeCleanupScope(context.Background(), knowledgeKey, forged)) != codes.PermissionDenied {
		t.Fatal("authorized forged cleanup scope")
	}
}

func TestCleanupRequestRequiresCommittedOperationAndPolicy(t *testing.T) {
	knowledgeBase, _, _ := bindingObjects(t)
	stamp := metav1.Now()
	knowledgeBase.DeletionTimestamp = &stamp
	knowledgeBase.Finalizers = []string{"test"}
	resolver, _ := bindingResolver(t, knowledgeBase)
	binding, err := resolver.ResolveCleanup(context.Background(), knowledgeKey)
	if err != nil {
		t.Fatal(err)
	}
	valid := &protocol.KnowledgeCleanupRequest{
		Scope: binding.Scope, CleanupId: "cleanup",
		BackendOperationId: binding.BackendOperationID,
		ReclaimPolicy:      protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN,
	}
	if err := resolver.AuthorizeCleanupRequest(context.Background(), knowledgeKey, valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*protocol.KnowledgeCleanupRequest){
		"operation": func(request *protocol.KnowledgeCleanupRequest) { request.BackendOperationId = "foreign" },
		"policy": func(request *protocol.KnowledgeCleanupRequest) {
			request.ReclaimPolicy = protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE
		},
		"namespace":  func(request *protocol.KnowledgeCleanupRequest) { request.Scope.Namespace = "other" },
		"generation": func(request *protocol.KnowledgeCleanupRequest) { request.Scope.Generation++ },
		"scope":      func(request *protocol.KnowledgeCleanupRequest) { request.Scope = nil },
	} {
		t.Run(name, func(t *testing.T) {
			request := proto.Clone(valid).(*protocol.KnowledgeCleanupRequest)
			mutate(request)
			if err := resolver.AuthorizeCleanupRequest(context.Background(), knowledgeKey, request); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("forged cleanup was not denied: %v", err)
			}
		})
	}
}

func TestCleanupBindingFailsClosedOnIncompleteOrUntrustedState(t *testing.T) {
	cases := map[string]func(*k8s.KnowledgeBase){
		"not deleting":              func(kb *k8s.KnowledgeBase) { kb.DeletionTimestamp = nil },
		"missing KB UID":            func(kb *k8s.KnowledgeBase) { kb.UID = "" },
		"stale generation":          func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.Generation-- },
		"missing API group":         func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ConfigurationAPIGroup = "" },
		"invalid kind":              func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ConfigurationKind = "bad-kind" },
		"missing name":              func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ConfigurationName = "" },
		"missing configuration UID": func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ConfigurationUID = "" },
		"invalid digest":            func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ConfigurationDigest = "invalid" },
		"missing operation":         func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.OperationID = "" },
		"missing backend operation": func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.BackendOperationID = "" },
		"missing reclaim policy":    func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ReclaimPolicy = "" },
		"invalid reclaim policy":    func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ReclaimPolicy = "invalid" },
		"unregistered provider":     func(kb *k8s.KnowledgeBase) { kb.Status.BackendBinding.ProviderName = "other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			knowledgeBase, _, _ := bindingObjects(t)
			stamp := metav1.Now()
			knowledgeBase.DeletionTimestamp = &stamp
			knowledgeBase.Finalizers = []string{"test"}
			mutate(knowledgeBase)
			resolver, _ := bindingResolver(t, knowledgeBase)
			if binding, err := resolver.ResolveCleanup(context.Background(), knowledgeKey); binding != nil || status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("ResolveCleanup() = (%+v, %v), want fail closed", binding, err)
			}
		})
	}
}

func TestBindingResolvesTrustedProviderDimension(t *testing.T) {
	knowledgeBase, class, configuration := bindingObjects(t)
	class.Spec.Provisioner = "azuresearch"
	if err := unstructured.SetNestedField(configuration.Object, int64(1536), "spec", "embeddingDimensions"); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(knowledgeBase, class, configuration).Build()
	resolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{{
		ProviderName: "azuresearch", ConfigurationKind: configurationKind,
		EmbeddingDimension: func(object *unstructured.Unstructured) (int, error) {
			dimension, found, err := unstructured.NestedInt64(object.Object, "spec", "embeddingDimensions")
			if err != nil || !found {
				return 0, errors.New("missing embedding dimensions")
			}
			return int(dimension), nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	binding, err := resolver.ResolvePending(context.Background(), knowledgeKey)
	if err != nil {
		t.Fatal(err)
	}
	if binding.EmbeddingDimension != 1536 {
		t.Fatalf("embedding dimension = %d", binding.EmbeddingDimension)
	}
}

func TestPendingBindingFailsClosedOnMutableIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*k8s.KnowledgeBase, *k8s.KnowledgeBaseClass, *unstructured.Unstructured){
		"missing KB UID": func(kb *k8s.KnowledgeBase, _ *k8s.KnowledgeBaseClass, _ *unstructured.Unstructured) { kb.UID = "" },
		"deleting class": func(_ *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, _ *unstructured.Unstructured) {
			stamp := metav1.Now()
			class.DeletionTimestamp = &stamp
			class.Finalizers = []string{"test"}
		},
		"legacy class": func(_ *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, _ *unstructured.Unstructured) {
			class.Spec.BackendRef = nil
		},
		"provider mismatch": func(_ *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, _ *unstructured.Unstructured) {
			class.Spec.Provisioner = "other"
		},
		"missing configuration UID": func(_ *k8s.KnowledgeBase, _ *k8s.KnowledgeBaseClass, configuration *unstructured.Unstructured) {
			configuration.SetUID("")
		},
		"deleting configuration": func(_ *k8s.KnowledgeBase, _ *k8s.KnowledgeBaseClass, configuration *unstructured.Unstructured) {
			stamp := metav1.Now()
			configuration.SetDeletionTimestamp(&stamp)
			configuration.SetFinalizers([]string{"test"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			knowledgeBase, class, configuration := bindingObjects(t)
			mutate(knowledgeBase, class, configuration)
			resolver, _ := bindingResolver(t, knowledgeBase, class, configuration)
			if binding, err := resolver.ResolvePending(context.Background(), knowledgeKey); binding != nil || status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("ResolvePending() = (%+v, %v), want fail closed", binding, err)
			}
		})
	}
}

func TestBindingRejectsUncommittedOrChangedObjects(t *testing.T) {
	cases := map[string]func(*k8s.KnowledgeBase, *k8s.KnowledgeBaseClass, *unstructured.Unstructured){
		"missing binding": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			kb.Status.BackendBinding = nil
		},
		"stale binding": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			kb.Status.Generation++
		},
		"uncommitted": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			kb.Status.Generation = 0
			kb.Status.BackendBinding.Generation = 0
		},
		"indexing": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			kb.Status.Phase = "Indexing"
		},
		"recreated class": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			class.UID = "new-class"
		},
		"changed class": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			class.Generation++
		},
		"recreated config": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			config.SetUID("new-config")
		},
		"changed config": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			config.Object["spec"] = map[string]interface{}{"table": "other"}
		},
		"foreign namespace": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			config.SetNamespace("tenant-b")
		},
		"unregistered kind": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			class.Spec.BackendRef.Kind = "OtherBackend"
		},
		"provider mismatch": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			class.Spec.Provisioner = "other"
		},
		"legacy": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			class.Spec.BackendRef = nil
		},
		"deleting KB": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			stamp := metav1.Now()
			kb.DeletionTimestamp = &stamp
			kb.Finalizers = []string{"test"}
		},
		"deleting config": func(kb *k8s.KnowledgeBase, class *k8s.KnowledgeBaseClass, config *unstructured.Unstructured) {
			stamp := metav1.Now()
			config.SetDeletionTimestamp(&stamp)
			config.SetFinalizers([]string{"test"})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			knowledgeBase, class, configuration := bindingObjects(t)
			mutate(knowledgeBase, class, configuration)
			resolver, _ := bindingResolver(t, knowledgeBase, class, configuration)
			if _, err := resolver.Resolve(context.Background(), knowledgeKey); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("expected closed binding, got %v", err)
			}
		})
	}
}

func TestBindingAuthorizationRechecksCurrentState(t *testing.T) {
	knowledgeBase, class, configuration := bindingObjects(t)
	resolver, reader := bindingResolver(t, knowledgeBase, class, configuration)
	ctx := context.Background()
	binding, err := resolver.Resolve(ctx, knowledgeKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*protocol.KnowledgeScope){
		"namespace":        func(scope *protocol.KnowledgeScope) { scope.Namespace = "tenant-b" },
		"KB identity":      func(scope *protocol.KnowledgeScope) { scope.KnowledgeBaseUid = "recreated" },
		"config identity":  func(scope *protocol.KnowledgeScope) { scope.ConfigurationUid = "recreated" },
		"config reference": func(scope *protocol.KnowledgeScope) { scope.ConfigurationName = "other" },
		"digest":           func(scope *protocol.KnowledgeScope) { scope.ConfigurationDigest = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			scope := proto.Clone(binding.Scope).(*protocol.KnowledgeScope)
			mutate(scope)
			if status.Code(resolver.AuthorizeScope(ctx, knowledgeKey, scope)) != codes.PermissionDenied {
				t.Fatal("accepted forged scope")
			}
		})
	}
	current := &k8s.KnowledgeBase{}
	if err := reader.Get(ctx, knowledgeKey, current); err != nil {
		t.Fatal(err)
	}
	current.Status.Generation++
	current.Status.BackendBinding.Generation++
	if err := reader.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if status.Code(resolver.AuthorizeScope(ctx, knowledgeKey, binding.Scope)) != codes.PermissionDenied {
		t.Fatal("authorized previous committed generation")
	}
	if err := reader.Delete(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(ctx, knowledgeKey); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("authorized removed configuration")
	}
}

func TestConfigurationDigestTracksOnlySpec(t *testing.T) {
	_, _, configuration := bindingObjects(t)
	original, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	configuration.SetLabels(map[string]string{"changed": "metadata"})
	configuration.Object["status"] = map[string]interface{}{"ready": true}
	configuration.Object["spec"] = map[string]interface{}{"credentialSecretRef": map[string]interface{}{"name": "database"}, "table": "chunks"}
	unchanged, err := knowledge.ConfigurationDigest(configuration)
	if err != nil || original != unchanged {
		t.Fatalf("metadata or map order changed spec digest: %v", err)
	}
	configuration.Object["spec"] = map[string]interface{}{"table": "other"}
	changed, err := knowledge.ConfigurationDigest(configuration)
	if err != nil || original == changed {
		t.Fatalf("spec change did not change digest: %v", err)
	}
}

type failingReader struct {
	client.Reader
}

func (reader failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("private connection credentials")
}

func TestBindingFailsClosedOnMissingObjectsAndReadErrors(t *testing.T) {
	for _, missing := range []string{"KB", "class", "configuration"} {
		t.Run(missing, func(t *testing.T) {
			knowledgeBase, class, configuration := bindingObjects(t)
			objects := []client.Object{}
			if missing != "KB" {
				objects = append(objects, knowledgeBase)
			}
			if missing != "class" {
				objects = append(objects, class)
			}
			if missing != "configuration" {
				objects = append(objects, configuration)
			}
			resolver, _ := bindingResolver(t, objects...)
			if result, err := resolver.Resolve(context.Background(), knowledgeKey); result != nil || status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("resolved missing %s: %v", missing, err)
			}
		})
	}
	resolver, err := knowledge.NewBindingResolver(failingReader{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), knowledgeKey); status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe read error: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), types.NamespacedName{Name: "manuals"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("accepted namespace-free resolution: %v", err)
	}
	if status.Code(resolver.AuthorizeScope(context.Background(), knowledgeKey, nil)) != codes.PermissionDenied {
		t.Fatal("accepted nil scope")
	}
}

func TestBindingRequiresUnambiguousAllowedRegistration(t *testing.T) {
	knowledgeBase, class, configuration := bindingObjects(t)
	_, reader := bindingResolver(t, knowledgeBase, class, configuration)
	registration := knowledge.Registration{ProviderName: "pgvector", ConfigurationKind: configurationKind}
	if _, err := knowledge.NewBindingResolver(nil, []knowledge.Registration{registration}); err == nil {
		t.Fatal("accepted missing reader")
	}
	if _, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{registration, registration}); err == nil {
		t.Fatal("accepted ambiguous registration")
	}
	for _, invalid := range []knowledge.Registration{
		{ConfigurationKind: configurationKind},
		{ProviderName: "pgvector"},
		{ProviderName: "pgvector", ConfigurationKind: schema.GroupVersionKind{Group: configurationKind.Group, Version: "v1", Kind: "bad-kind"}},
	} {
		if _, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{invalid}); err == nil {
			t.Fatal("accepted invalid registration")
		}
	}
	registrations := []knowledge.Registration{registration}
	resolver, err := knowledge.NewBindingResolver(reader, registrations)
	if err != nil {
		t.Fatal(err)
	}
	registrations[0].ProviderName = "changed"
	if _, err := resolver.Resolve(context.Background(), knowledgeKey); err != nil {
		t.Fatalf("registration was not snapshotted: %v", err)
	}
	unregistered, err := knowledge.NewBindingResolver(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unregistered.Resolve(context.Background(), knowledgeKey); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("resolved removed provider registration: %v", err)
	}
}

func TestConfigurationDigestRejectsInvalidSpec(t *testing.T) {
	for _, configuration := range []*unstructured.Unstructured{
		nil,
		{Object: map[string]interface{}{}},
		{Object: map[string]interface{}{"spec": "invalid"}},
		{Object: map[string]interface{}{"spec": map[string]interface{}{"value": strings.Repeat("x", 1<<20)}}},
	} {
		if _, err := knowledge.ConfigurationDigest(configuration); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("accepted invalid spec: %v", err)
		}
	}
}
