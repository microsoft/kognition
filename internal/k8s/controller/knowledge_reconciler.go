// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/providers/azuresearch"
	"github.com/kognition-ai/kognition/providers/pgvector"
)

// KBIngesterOptions configures the KB ingester Job launcher. When Image is
// empty the adapter runs without a launcher and the reconciler falls back to
// its simulated bookkeeping (backward compatible with existing installs and
// unit tests).
type KBIngesterOptions struct {
	// Image is the fully-qualified ingester container image.
	Image string
	// ServiceAccount is the ServiceAccount the ingester Pod runs as.
	// Leave empty to use the namespace default.
	ServiceAccount string
	// ControllerNamespace is used as a fallback lookup namespace when a
	// SecretRef can't be found in the KB's own namespace.
	ControllerNamespace string
	Provider            *KBIngesterProviderOptions
}

// KBIngesterProviderOptions contains installation-owned provider transport
// settings. Tenant resources cannot override these values.
type KBIngesterProviderOptions struct {
	Name              string
	Address           string
	ServerName        string
	PeerURI           string
	ConfigurationKind schema.GroupVersionKind
	TLSSecretName     string
	TLSCertificateKey string
	TLSPrivateKeyKey  string
	TLSTrustBundleKey string
}

func newConfiguredKBIngesterLauncher(mgr ctrl.Manager, s store.Store, logger *zap.Logger, options KBIngesterOptions) (*KBIngesterLauncher, error) {
	if options.ServiceAccount != "" && options.ServiceAccount != "default" {
		return nil, fmt.Errorf("ingester ServiceAccount must be empty or default; workload admission prohibits elevated identities")
	}
	launcher := NewKBIngesterLauncher(mgr.GetClient(), s, logger, options.Image, options.ServiceAccount, options.ControllerNamespace)
	if options.Provider == nil {
		return launcher, nil
	}
	resolver, err := newKnowledgeBindingResolver(mgr, options)
	if err != nil {
		return nil, err
	}
	return launcher.WithProvider(*options.Provider, resolver, mgr.GetAPIReader()), nil
}

func newKnowledgeBindingResolver(mgr ctrl.Manager, options KBIngesterOptions) (*knowledge.BindingResolver, error) {
	provider := options.Provider
	if provider == nil {
		return nil, fmt.Errorf("knowledge provider registration is required")
	}
	if provider.Name == "" || provider.Address == "" || provider.ServerName == "" || provider.PeerURI == "" || provider.TLSSecretName == "" ||
		provider.TLSCertificateKey == "" || provider.TLSPrivateKeyKey == "" || provider.TLSTrustBundleKey == "" || provider.ConfigurationKind.Empty() || options.ControllerNamespace == "" {
		return nil, fmt.Errorf("knowledge provider ingestion registration is incomplete")
	}
	azureSearchKind := schema.GroupVersionKind{Group: azuresearch.APIGroup, Version: azuresearch.APIVersion, Kind: azuresearch.Kind}
	pgvectorKind := schema.GroupVersionKind{Group: pgvector.APIGroup, Version: pgvector.APIVersion, Kind: pgvector.Kind}
	if provider.ConfigurationKind != azureSearchKind && provider.ConfigurationKind != pgvectorKind {
		return nil, fmt.Errorf("knowledge provider ingestion configuration kind is unsupported")
	}
	resolver, err := knowledge.NewBindingResolver(mgr.GetAPIReader(), []knowledge.Registration{{
		ProviderName:      provider.Name,
		ConfigurationKind: provider.ConfigurationKind,
		EmbeddingDimension: func(configuration *unstructured.Unstructured) (int, error) {
			if provider.ConfigurationKind == pgvectorKind {
				spec, err := pgvector.DecodeBackend(configuration)
				return int(spec.EmbeddingDimensions), err
			}
			spec, err := azuresearch.DecodeBackend(configuration)
			return int(spec.EmbeddingDimensions), err
		},
	}})
	if err != nil {
		return nil, fmt.Errorf("configure knowledge provider binding resolver: %w", err)
	}
	return resolver, nil
}

// KnowledgeBaseReconcilerAdapter wraps the internal KnowledgeBaseReconciler
// for KnowledgeBase resources under controller-runtime.
type KnowledgeBaseReconcilerAdapter struct {
	internal   *intctrl.KnowledgeBaseReconciler
	logger     *zap.Logger
	events     eventEmitter
	store      store.Store
	ingesterOp KBIngesterOptions
}

// NewKnowledgeBaseReconcilerAdapter creates an adapter that delegates to the
// internal KnowledgeBaseReconciler.
func NewKnowledgeBaseReconcilerAdapter(s store.Store, logger *zap.Logger) *KnowledgeBaseReconcilerAdapter {
	return &KnowledgeBaseReconcilerAdapter{
		internal: intctrl.NewKnowledgeBaseReconciler(s, logger),
		logger:   logger,
		store:    s,
	}
}

// WithIngesterOptions configures the Job-based ingester launcher used for
// non-inline KnowledgeBaseClasses (e.g. pgvector).
func (a *KnowledgeBaseReconcilerAdapter) WithIngesterOptions(opts KBIngesterOptions) *KnowledgeBaseReconcilerAdapter {
	a.ingesterOp = opts
	return a
}

// Reconcile implements reconcile.Reconciler for controller-runtime.
func (a *KnowledgeBaseReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	internalReq := intctrl.Request{
		Namespace: req.Namespace,
		Name:      req.Name,
		Kind:      "KnowledgeBase",
	}
	result, err := a.internal.Reconcile(ctx, internalReq)
	a.events.emit(ctx, req, err)
	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, err
}

// SetupWithManager registers the KnowledgeBase reconciler with the controller-runtime manager.
func (a *KnowledgeBaseReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "KnowledgeBase", "kognition-knowledgebase-controller")
	if a.ingesterOp.Provider != nil {
		resolver, err := newKnowledgeBindingResolver(mgr, a.ingesterOp)
		if err != nil {
			return err
		}
		a.internal.WithBackendCleaner(&knowledgeBackendCleaner{
			reader: mgr.GetAPIReader(), namespace: a.ingesterOp.ControllerNamespace,
			provider: *a.ingesterOp.Provider, resolver: resolver,
		}).WithRetainedBindingRecorder(&retainedKnowledgeBindingRecorder{client: mgr.GetClient()})
	}
	if a.ingesterOp.Image != "" {
		launcher, err := newConfiguredKBIngesterLauncher(mgr, a.store, a.logger, a.ingesterOp)
		if err != nil {
			return err
		}
		a.internal.WithIngesterLauncher(launcher)
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("knowledgebase").
		For(&k8s.KnowledgeBase{}).
		Complete(a)
}
