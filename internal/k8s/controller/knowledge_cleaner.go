// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

type knowledgeBackendCleaner struct {
	reader    client.Reader
	namespace string
	provider  KBIngesterProviderOptions
	resolver  *knowledge.BindingResolver
}

func (cleaner *knowledgeBackendCleaner) Cleanup(ctx context.Context, kb *api.KnowledgeBase, policy api.ReclaimPolicy) error {
	resolved, err := cleaner.resolver.ResolveCleanup(ctx, types.NamespacedName{Namespace: kb.ObjectMeta.Namespace, Name: kb.ObjectMeta.Name})
	if err != nil {
		return fmt.Errorf("resolve cleanup-safe knowledge binding: %w", err)
	}
	if resolved.ProviderName != cleaner.provider.Name {
		return fmt.Errorf("resolved knowledge provider does not match the registered transport")
	}
	reclaimPolicy, err := knowledgeReclaimPolicy(policy)
	if err != nil {
		return err
	}
	tlsSecret := &corev1.Secret{}
	if err := cleaner.reader.Get(ctx, types.NamespacedName{Namespace: cleaner.namespace, Name: cleaner.provider.TLSSecretName}, tlsSecret); err != nil {
		return fmt.Errorf("read knowledge provider TLS secret: %w", err)
	}
	certificate, err := tls.X509KeyPair(tlsSecret.Data[cleaner.provider.TLSCertificateKey], tlsSecret.Data[cleaner.provider.TLSPrivateKeyKey])
	if err != nil {
		return fmt.Errorf("parse knowledge provider client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(tlsSecret.Data[cleaner.provider.TLSTrustBundleKey]) {
		return fmt.Errorf("parse knowledge provider trust bundle")
	}
	providerClient, err := pluginsdk.NewKnowledgeClient(pluginsdk.KnowledgeClientConfig{
		Name: cleaner.provider.Name, Address: cleaner.provider.Address,
		TLS: pluginsdk.KnowledgeTLSConfig{Certificate: certificate, Roots: roots, PeerURI: cleaner.provider.PeerURI, ServerName: cleaner.provider.ServerName},
	})
	if err != nil {
		return fmt.Errorf("create knowledge provider cleanup client: %w", err)
	}
	defer func() { _ = providerClient.Close() }()
	_, err = providerClient.Cleanup(ctx, knowledgeCleanupRequest(resolved, reclaimPolicy))
	if err != nil {
		return fmt.Errorf("clean knowledge provider backend: %w", err)
	}
	return nil
}

func knowledgeReclaimPolicy(policy api.ReclaimPolicy) (protocol.KnowledgeReclaimPolicy, error) {
	switch policy {
	case api.ReclaimDelete:
		return protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_DELETE, nil
	case api.ReclaimRetain:
		return protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_RETAIN, nil
	case api.ReclaimArchive:
		return protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_ARCHIVE, nil
	default:
		return protocol.KnowledgeReclaimPolicy_KNOWLEDGE_RECLAIM_POLICY_UNSPECIFIED, fmt.Errorf("unsupported knowledge reclaim policy %q", policy)
	}
}

func knowledgeCleanupRequest(resolved *knowledge.ResolvedBinding, policy protocol.KnowledgeReclaimPolicy) *protocol.KnowledgeCleanupRequest {
	return &protocol.KnowledgeCleanupRequest{
		Scope: resolved.Scope, CleanupId: "cleanup-" + resolved.Scope.KnowledgeBaseUid + "-" + strconv.FormatInt(resolved.Scope.Generation, 10),
		ReclaimPolicy: policy, BackendOperationId: resolved.BackendOperationID,
	}
}
