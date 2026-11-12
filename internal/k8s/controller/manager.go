// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

// ManagerOptions configures the controller-runtime manager.
type ManagerOptions struct {
	ManagedNamespaces []string

	// MetricsBindAddress is the address to bind metrics to (e.g. ":8080").
	MetricsBindAddress string

	// HealthProbeBindAddress is the address for health/readiness probes (e.g. ":8081").
	HealthProbeBindAddress string

	// LeaderElection enables leader election for HA.
	LeaderElection bool

	// LeaderElectionID is the lock name for leader election.
	LeaderElectionID string

	// LeaderElectionNamespace restricts leader election to a namespace.
	LeaderElectionNamespace string

	// DisableWebhooks disables the admission webhook server.
	DisableWebhooks bool

	// Logger is the zap logger to use.
	Logger *zap.Logger
}

// SetupScheme creates the runtime scheme with K8s and Kognition types registered.
func SetupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(k8s.AddToScheme(scheme))
	return scheme
}

// NewManager creates a controller-runtime manager with the given options.
func NewManager(opts ManagerOptions) (ctrl.Manager, error) {
	scheme := SetupScheme()

	// Configure controller-runtime logger from zap.
	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(false)))

	mgrOpts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: opts.MetricsBindAddress,
		},
		HealthProbeBindAddress:        opts.HealthProbeBindAddress,
		LeaderElection:                opts.LeaderElection,
		LeaderElectionID:              opts.LeaderElectionID,
		LeaderElectionNamespace:       opts.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
		LeaderElectionLabels: map[string]string{
			"app.kubernetes.io/managed-by":  "kognition",
			"kognition.dev/leader-election": "true",
		},
	}
	configureConfigMapCache(&mgrOpts)
	if err := configureManagedNamespaces(&mgrOpts, opts.ManagedNamespaces); err != nil {
		return nil, err
	}

	// When webhooks are disabled, set port to 0 so controller-runtime
	// does not start the webhook server or look for TLS certs.
	if opts.DisableWebhooks {
		mgrOpts.WebhookServer = ctrlwebhook.NewServer(ctrlwebhook.Options{Port: 0})
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
	if err != nil {
		return nil, fmt.Errorf("create controller-runtime manager: %w", err)
	}

	// Add health/readiness probes.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("add healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return nil, fmt.Errorf("add readyz check: %w", err)
	}

	return mgr, nil
}

func configureConfigMapCache(options *ctrl.Options) {
	options.Cache = cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {
				Label: labels.SelectorFromSet(labels.Set{
					toolCallLedgerManagedLabel: toolCallLedgerManagedLabelValue,
				}),
				Transform: stripConfigMapPayload,
			},
		},
	}
	options.Client = client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.ConfigMap{}, &coordinationv1.Lease{}, &corev1.Secret{}},
		},
	}
}

func configureManagedNamespaces(options *ctrl.Options, namespaces []string) error {
	if len(namespaces) == 0 {
		return fmt.Errorf("at least one managed namespace is required")
	}
	options.Cache.DefaultNamespaces = make(map[string]cache.Config, len(namespaces))
	for _, namespace := range namespaces {
		if errors := validation.IsDNS1123Label(namespace); len(errors) != 0 {
			return fmt.Errorf("invalid managed namespace %q: %v", namespace, errors)
		}
		options.Cache.DefaultNamespaces[namespace] = cache.Config{}
	}
	if options.LeaderElectionNamespace != "" {
		if _, allowed := options.Cache.DefaultNamespaces[options.LeaderElectionNamespace]; !allowed {
			return fmt.Errorf("leader election namespace must be a managed namespace")
		}
	}
	return nil
}

func stripConfigMapPayload(object interface{}) (interface{}, error) {
	configMap, ok := object.(*corev1.ConfigMap)
	if !ok {
		return object, nil
	}

	metadataOnly := configMap.DeepCopy()
	metadataOnly.Data = nil
	metadataOnly.BinaryData = nil
	return metadataOnly, nil
}

// StartManager starts the controller-runtime manager. Blocks until ctx is canceled.
func StartManager(ctx context.Context, mgr ctrl.Manager, logger *zap.Logger) error {
	logger.Info("starting controller-runtime manager")
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("controller-runtime manager exited: %w", err)
	}
	return nil
}
