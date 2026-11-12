// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Registration struct {
	ProviderName       string
	ConfigurationKind  schema.GroupVersionKind
	EmbeddingDimension func(*unstructured.Unstructured) (int, error)
}

type BindingResolver struct {
	reader        client.Reader
	registrations map[schema.GroupKind]Registration
}

type ResolvedBinding struct {
	ProviderName       string
	BackendOperationID string
	ReclaimPolicy      k8s.ReclaimPolicy
	Scope              *protocol.KnowledgeScope
	EmbeddingDimension int
}

type PendingBinding struct {
	ProviderName       string
	ClassUID           string
	ClassGeneration    int64
	EmbeddingDimension int
	Scope              *protocol.KnowledgeScope
}

func NewBindingResolver(reader client.Reader, registrations []Registration) (*BindingResolver, error) {
	if reader == nil {
		return nil, fmt.Errorf("knowledge binding reader is required")
	}
	resolver := &BindingResolver{reader: reader, registrations: make(map[schema.GroupKind]Registration)}
	for _, registration := range registrations {
		kind := registration.ConfigurationKind
		if len(validation.IsDNS1123Subdomain(registration.ProviderName)) != 0 || len(validation.IsDNS1123Subdomain(kind.Group)) != 0 ||
			len(validation.IsDNS1123Label(kind.Version)) != 0 || !regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`).MatchString(kind.Kind) {
			return nil, fmt.Errorf("invalid knowledge provider registration")
		}
		if _, exists := resolver.registrations[kind.GroupKind()]; exists {
			return nil, fmt.Errorf("ambiguous knowledge configuration registration")
		}
		resolver.registrations[kind.GroupKind()] = registration
	}
	return resolver, nil
}

func ConfigurationDigest(configuration *unstructured.Unstructured) (string, error) {
	if configuration == nil {
		return "", status.Error(codes.FailedPrecondition, "knowledge configuration is required")
	}
	spec, found, err := unstructured.NestedMap(configuration.Object, "spec")
	if err != nil || !found {
		return "", status.Error(codes.FailedPrecondition, "knowledge configuration spec is required")
	}
	encoded, err := json.Marshal(spec)
	if err != nil || len(encoded) > 1<<20 {
		return "", status.Error(codes.FailedPrecondition, "invalid knowledge configuration spec")
	}
	digest := sha256.Sum256(append([]byte("kognition.knowledge/configuration-spec/v1\n"), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func (resolver *BindingResolver) ResolvePending(ctx context.Context, key types.NamespacedName) (*PendingBinding, error) {
	if len(validation.IsDNS1123Label(key.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid knowledge base identity")
	}
	knowledgeBase := &k8s.KnowledgeBase{}
	if err := resolver.reader.Get(ctx, key, knowledgeBase); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge base is unavailable")
	}
	if knowledgeBase.UID == "" || !knowledgeBase.DeletionTimestamp.IsZero() || knowledgeBase.Status.Generation < 0 || knowledgeBase.Status.Generation == math.MaxInt64 {
		return nil, status.Error(codes.FailedPrecondition, "knowledge base cannot start provider indexing")
	}
	class := &k8s.KnowledgeBaseClass{}
	if err := resolver.reader.Get(ctx, types.NamespacedName{Name: knowledgeBase.Spec.KnowledgeBaseClassName}, class); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge class is unavailable")
	}
	reference := class.Spec.BackendRef
	if reference == nil || class.Spec.VectorStore != nil || class.UID == "" || !class.DeletionTimestamp.IsZero() || class.Generation < 1 {
		return nil, status.Error(codes.FailedPrecondition, "knowledge class cannot start provider indexing")
	}
	registration, found := resolver.registrations[schema.GroupKind{Group: reference.APIGroup, Kind: reference.Kind}]
	if !found || class.Spec.Provisioner != registration.ProviderName || len(validation.IsDNS1123Subdomain(reference.Name)) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "knowledge provider is not registered for this class")
	}
	configuration := &unstructured.Unstructured{}
	configuration.SetGroupVersionKind(registration.ConfigurationKind)
	if err := resolver.reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: reference.Name}, configuration); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge configuration is unavailable")
	}
	if configuration.GetNamespace() != key.Namespace || configuration.GetName() != reference.Name ||
		configuration.GroupVersionKind() != registration.ConfigurationKind || configuration.GetUID() == "" || !configuration.GetDeletionTimestamp().IsZero() {
		return nil, status.Error(codes.FailedPrecondition, "knowledge configuration cannot start provider indexing")
	}
	digest, err := ConfigurationDigest(configuration)
	if err != nil {
		return nil, err
	}
	dimension := 0
	if registration.EmbeddingDimension != nil {
		dimension, err = registration.EmbeddingDimension(configuration)
		if err != nil || dimension < 1 || dimension > 4096 {
			return nil, status.Error(codes.FailedPrecondition, "knowledge configuration cannot start provider indexing")
		}
	}
	return &PendingBinding{
		ProviderName: registration.ProviderName, ClassUID: string(class.UID), ClassGeneration: class.Generation,
		EmbeddingDimension: dimension,
		Scope: &protocol.KnowledgeScope{
			Namespace: key.Namespace, KnowledgeBaseUid: string(knowledgeBase.UID), Generation: knowledgeBase.Status.Generation + 1,
			ConfigurationApiGroup: reference.APIGroup, ConfigurationKind: reference.Kind, ConfigurationName: reference.Name,
			ConfigurationUid: string(configuration.GetUID()), ConfigurationDigest: digest,
		},
	}, nil
}

func (resolver *BindingResolver) Resolve(ctx context.Context, key types.NamespacedName) (*ResolvedBinding, error) {
	if len(validation.IsDNS1123Label(key.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid knowledge base identity")
	}
	knowledgeBase := &k8s.KnowledgeBase{}
	if err := resolver.reader.Get(ctx, key, knowledgeBase); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge base is unavailable")
	}
	binding := knowledgeBase.Status.BackendBinding
	if knowledgeBase.UID == "" || !knowledgeBase.DeletionTimestamp.IsZero() || binding == nil ||
		knowledgeBase.Status.Generation < 1 || binding.Generation != knowledgeBase.Status.Generation ||
		strings.TrimSpace(binding.OperationID) == "" || len(binding.OperationID) > 128 {
		return nil, status.Error(codes.FailedPrecondition, "knowledge base has no committed backend binding")
	}
	switch knowledgeBase.Status.Phase {
	case "Ready", "Refreshing", "Failed":
	default:
		return nil, status.Error(codes.FailedPrecondition, "knowledge base has no queryable generation")
	}
	class := &k8s.KnowledgeBaseClass{}
	if err := resolver.reader.Get(ctx, types.NamespacedName{Name: knowledgeBase.Spec.KnowledgeBaseClassName}, class); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge class is unavailable")
	}
	reference := class.Spec.BackendRef
	if reference == nil || class.Spec.VectorStore != nil || class.UID == "" || !class.DeletionTimestamp.IsZero() ||
		string(class.UID) != binding.ClassUID || class.Generation < 1 || class.Generation != binding.ClassGeneration {
		return nil, status.Error(codes.FailedPrecondition, "knowledge class differs from committed binding")
	}
	registration, found := resolver.registrations[schema.GroupKind{Group: reference.APIGroup, Kind: reference.Kind}]
	if !found || registration.ProviderName != binding.ProviderName || class.Spec.Provisioner != registration.ProviderName {
		return nil, status.Error(codes.FailedPrecondition, "knowledge provider is not registered for this binding")
	}
	if len(validation.IsDNS1123Subdomain(reference.Name)) != 0 {
		return nil, status.Error(codes.FailedPrecondition, "invalid knowledge configuration reference")
	}
	configuration := &unstructured.Unstructured{}
	configuration.SetGroupVersionKind(registration.ConfigurationKind)
	if err := resolver.reader.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: reference.Name}, configuration); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge configuration is unavailable")
	}
	if configuration.GetNamespace() != key.Namespace || configuration.GetName() != reference.Name ||
		configuration.GroupVersionKind() != registration.ConfigurationKind || configuration.GetUID() == "" ||
		string(configuration.GetUID()) != binding.ConfigurationUID || !configuration.GetDeletionTimestamp().IsZero() {
		return nil, status.Error(codes.FailedPrecondition, "knowledge configuration differs from committed binding")
	}
	digest, err := ConfigurationDigest(configuration)
	if err != nil {
		return nil, err
	}
	if digest != binding.ConfigurationDigest {
		return nil, status.Error(codes.FailedPrecondition, "knowledge configuration differs from committed binding")
	}
	dimension := 0
	if registration.EmbeddingDimension != nil {
		dimension, err = registration.EmbeddingDimension(configuration)
		if err != nil || dimension < 1 || dimension > 4096 {
			return nil, status.Error(codes.FailedPrecondition, "knowledge embedding dimension is unavailable")
		}
	}
	return &ResolvedBinding{
		ProviderName:       registration.ProviderName,
		EmbeddingDimension: dimension,
		Scope: &protocol.KnowledgeScope{
			Namespace: key.Namespace, KnowledgeBaseUid: string(knowledgeBase.UID), Generation: binding.Generation,
			OperationId:           binding.OperationID,
			ConfigurationApiGroup: reference.APIGroup, ConfigurationKind: reference.Kind, ConfigurationName: reference.Name,
			ConfigurationUid: binding.ConfigurationUID, ConfigurationDigest: digest,
		},
	}, nil
}

func (resolver *BindingResolver) ResolveCleanup(ctx context.Context, key types.NamespacedName) (*ResolvedBinding, error) {
	if len(validation.IsDNS1123Label(key.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(key.Name)) != 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid knowledge base identity")
	}
	knowledgeBase := &k8s.KnowledgeBase{}
	if err := resolver.reader.Get(ctx, key, knowledgeBase); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "knowledge base is unavailable")
	}
	binding := knowledgeBase.Status.BackendBinding
	if knowledgeBase.UID == "" || knowledgeBase.DeletionTimestamp.IsZero() || binding == nil ||
		knowledgeBase.Status.Generation < 1 || binding.Generation != knowledgeBase.Status.Generation ||
		!validReclaimPolicy(binding.ReclaimPolicy) ||
		len(validation.IsDNS1123Subdomain(binding.ProviderName)) != 0 ||
		len(validation.IsDNS1123Subdomain(binding.ConfigurationAPIGroup)) != 0 ||
		!regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`).MatchString(binding.ConfigurationKind) ||
		len(validation.IsDNS1123Subdomain(binding.ConfigurationName)) != 0 ||
		strings.TrimSpace(binding.ConfigurationUID) == "" || len(binding.ConfigurationUID) > 128 ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(binding.ConfigurationDigest) ||
		strings.TrimSpace(binding.OperationID) == "" || len(binding.OperationID) > 128 ||
		strings.TrimSpace(binding.BackendOperationID) == "" || len(binding.BackendOperationID) > 1024 {
		return nil, status.Error(codes.FailedPrecondition, "knowledge base has no cleanup-safe backend binding")
	}
	registration, found := resolver.registrations[schema.GroupKind{
		Group: binding.ConfigurationAPIGroup,
		Kind:  binding.ConfigurationKind,
	}]
	if !found || registration.ProviderName != binding.ProviderName {
		return nil, status.Error(codes.FailedPrecondition, "knowledge provider is not registered for cleanup")
	}
	return &ResolvedBinding{
		ProviderName: binding.ProviderName, BackendOperationID: binding.BackendOperationID, ReclaimPolicy: binding.ReclaimPolicy,
		Scope: &protocol.KnowledgeScope{
			Namespace: key.Namespace, KnowledgeBaseUid: string(knowledgeBase.UID), Generation: binding.Generation,
			OperationId:           binding.OperationID,
			ConfigurationApiGroup: binding.ConfigurationAPIGroup, ConfigurationKind: binding.ConfigurationKind,
			ConfigurationName: binding.ConfigurationName, ConfigurationUid: binding.ConfigurationUID,
			ConfigurationDigest: binding.ConfigurationDigest,
		},
	}, nil
}

func validReclaimPolicy(policy k8s.ReclaimPolicy) bool {
	switch policy {
	case k8s.ReclaimPolicyDelete, k8s.ReclaimPolicyRetain, k8s.ReclaimPolicyArchive:
		return true
	default:
		return false
	}
}

func (resolver *BindingResolver) AuthorizeScope(ctx context.Context, key types.NamespacedName, scope *protocol.KnowledgeScope) error {
	if scope == nil || scope.Namespace != key.Namespace {
		return status.Error(codes.PermissionDenied, "knowledge scope does not match committed binding")
	}
	binding, err := resolver.Resolve(ctx, key)
	if err != nil {
		return err
	}
	if !proto.Equal(binding.Scope, scope) {
		return status.Error(codes.PermissionDenied, "knowledge scope does not match committed binding")
	}
	return nil
}

func (resolver *BindingResolver) AuthorizeCleanupScope(ctx context.Context, key types.NamespacedName, scope *protocol.KnowledgeScope) error {
	if scope == nil || scope.Namespace != key.Namespace {
		return status.Error(codes.PermissionDenied, "knowledge scope does not match cleanup binding")
	}
	binding, err := resolver.ResolveCleanup(ctx, key)
	if err != nil {
		return err
	}
	if !proto.Equal(binding.Scope, scope) {
		return status.Error(codes.PermissionDenied, "knowledge scope does not match cleanup binding")
	}
	return nil
}

func (resolver *BindingResolver) AuthorizeCleanupRequest(ctx context.Context, key types.NamespacedName, request *protocol.KnowledgeCleanupRequest) error {
	if request == nil || request.Scope == nil || request.Scope.Namespace != key.Namespace {
		return status.Error(codes.PermissionDenied, "knowledge cleanup request is required")
	}
	binding, err := resolver.ResolveCleanup(ctx, key)
	if err != nil {
		return err
	}
	policies := map[k8s.ReclaimPolicy]protocol.KnowledgeReclaimPolicy{
		k8s.ReclaimPolicyDelete:  protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE,
		k8s.ReclaimPolicyRetain:  protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN,
		k8s.ReclaimPolicyArchive: protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE,
	}
	policy, found := policies[binding.ReclaimPolicy]
	if !found || policy != request.ReclaimPolicy || binding.BackendOperationID != request.BackendOperationId || !proto.Equal(binding.Scope, request.Scope) {
		return status.Error(codes.PermissionDenied, "knowledge cleanup differs from committed scope, operation or policy")
	}
	return nil
}

func (resolver *BindingResolver) AuthorizePendingScope(ctx context.Context, key types.NamespacedName, scope *protocol.KnowledgeScope) error {
	if scope == nil || scope.Namespace != key.Namespace {
		return status.Error(codes.PermissionDenied, "knowledge scope does not match pending binding")
	}
	binding, err := resolver.ResolvePending(ctx, key)
	if err != nil {
		return err
	}
	if !proto.Equal(binding.Scope, scope) {
		return status.Error(codes.PermissionDenied, "knowledge scope does not match pending binding")
	}
	return nil
}
