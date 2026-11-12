// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

func TestKnowledgeReclaimPolicy(t *testing.T) {
	tests := map[api.ReclaimPolicy]protocol.KnowledgeReclaimPolicy{
		api.ReclaimDelete:  protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE,
		api.ReclaimRetain:  protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN,
		api.ReclaimArchive: protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE,
	}
	for input, expected := range tests {
		t.Run(string(input), func(t *testing.T) {
			actual, err := knowledgeReclaimPolicy(input)
			if err != nil || actual != expected {
				t.Fatalf("knowledgeReclaimPolicy(%q) = %v, %v; want %v", input, actual, err, expected)
			}
		})
	}
	if _, err := knowledgeReclaimPolicy(api.ReclaimPolicy("Unknown")); err == nil {
		t.Fatal("unknown reclaim policy was accepted")
	}
}

func TestKnowledgeCleanupRequestUsesDurableBinding(t *testing.T) {
	resolved := &knowledge.ResolvedBinding{
		ProviderName: "azuresearch", BackendOperationID: "v1.locator",
		Scope: &protocol.KnowledgeScope{Namespace: "team-a", KnowledgeBaseUid: "kb-uid", Generation: 7, OperationId: "team-a/job-7"},
	}
	request := knowledgeCleanupRequest(resolved, protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE)
	if request.Scope != resolved.Scope || request.CleanupId != "cleanup-kb-uid-7" || request.BackendOperationId != resolved.BackendOperationID ||
		request.ReclaimPolicy != protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE {
		t.Fatalf("cleanup request = %+v", request)
	}
	second := knowledgeCleanupRequest(resolved, protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE)
	if second.CleanupId != request.CleanupId {
		t.Fatalf("cleanup id changed across retry: %q != %q", second.CleanupId, request.CleanupId)
	}
}

func TestKnowledgeBackendCleanerRejectsProviderMismatch(t *testing.T) {
	cleaner := &knowledgeBackendCleaner{
		provider: KBIngesterProviderOptions{Name: "azuresearch"},
		resolver: cleanupBindingResolver(t, "other"),
	}
	err := cleaner.Cleanup(context.Background(), cleanupKnowledgeBase(), api.ReclaimDelete)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Cleanup() error = %v, want provider mismatch", err)
	}
}

func TestKnowledgeBackendCleanerRejectsInvalidTLSMaterial(t *testing.T) {
	tests := []struct {
		name       string
		secretData map[string][]byte
		want       string
	}{
		{name: "missing secret", want: "read knowledge provider TLS secret"},
		{name: "invalid key pair", secretData: map[string][]byte{"tls.crt": []byte("invalid"), "tls.key": []byte("invalid")}, want: "parse knowledge provider client certificate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var objects []runtime.Object
			if test.secretData != nil {
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "provider-tls", Namespace: "kognition-system"}, Data: test.secretData})
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cleaner := &knowledgeBackendCleaner{
				reader:    fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(),
				namespace: "kognition-system",
				provider:  KBIngesterProviderOptions{Name: "azuresearch", TLSSecretName: "provider-tls", TLSCertificateKey: "tls.crt", TLSPrivateKeyKey: "tls.key", TLSTrustBundleKey: "ca.crt"},
				resolver:  cleanupBindingResolver(t, "azuresearch"),
			}
			err := cleaner.Cleanup(context.Background(), cleanupKnowledgeBase(), api.ReclaimDelete)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Cleanup() error = %v, want %q", err, test.want)
			}
		})
	}
}

func cleanupKnowledgeBase() *api.KnowledgeBase {
	return &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "kb", Namespace: "team-a"}}
}

func cleanupBindingResolver(t *testing.T, providerName string) *knowledge.BindingResolver {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	stamp := metav1.Now()
	knowledgeBase := &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: "team-a", UID: "kb-uid", DeletionTimestamp: &stamp, Finalizers: []string{"test"}},
		Status: k8s.KnowledgeBaseStatus{Generation: 1, BackendBinding: &k8s.KnowledgeBackendBinding{
			Generation: 1, ProviderName: providerName, ReclaimPolicy: k8s.ReclaimPolicyDelete,
			ConfigurationAPIGroup: "search.knowledge.kognition.dev", ConfigurationKind: "AzureSearchBackend", ConfigurationName: "shared",
			ConfigurationUID: "configuration-uid", ConfigurationDigest: strings.Repeat("a", 64), OperationID: "team-a/index-1", BackendOperationID: "backend-operation",
		}},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(knowledgeBase).Build()
	resolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{{
		ProviderName: providerName, ConfigurationKind: schema.GroupVersionKind{Group: "search.knowledge.kognition.dev", Version: "v1alpha1", Kind: "AzureSearchBackend"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}
