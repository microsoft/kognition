// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gatewaybackend

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	pgstore "github.com/kognition-ai/kognition/internal/store/postgres"
)

type Store interface {
	store.Store
	store.RuntimeApprovalExecutionStore
	runtimeapproval.Gate3Store
	ActivateRuntimeApprovalBindingKeys(context.Context, runtimeapproval.BindingKeyStore) error
}

type Config struct {
	Mode             string
	DatabaseURL      string
	ControlNamespace string
	AuditNamespace   string
	Hostname         string
	Component        string
	WorkloadUID      string
	MaximumClockSkew time.Duration
}

type Backend struct {
	Store            Store
	Audit            observability.AuditApplyStore
	Cost             observability.UsageEventStore
	KubernetesClient client.Client
	PostgresPool     *pgxpool.Pool
	Claimant         api.RuntimeApprovalClaimant
	Close            func()
}

// ModelCallHealthStore shares a gateway's passive health through its backend.
// A backend without shared health support returns nil.
func (b *Backend) ModelCallHealthStore(namespace, component string, logger *zap.Logger) (store.ModelCallHealthStore, error) {
	if b.KubernetesClient != nil {
		configMaps, err := callhealth.NewConfigMapStore(b.KubernetesClient, namespace, component, callhealth.WithLogger(logger.Named("model-call-health")))
		if err != nil {
			return nil, fmt.Errorf("configure Kubernetes model call health: %w", err)
		}
		return configMaps, nil
	}
	shared, _ := b.Store.(store.ModelCallHealthStore)
	return shared, nil
}

func Open(ctx context.Context, config Config, logger *zap.Logger) (*Backend, error) {
	switch config.Mode {
	case "standalone":
		if config.DatabaseURL == "" {
			return nil, fmt.Errorf("DATABASE_URL is required in standalone mode")
		}
		pool, err := pgxpool.New(ctx, config.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("configure standalone database: %w", err)
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			return nil, fmt.Errorf("connect standalone database: %w", err)
		}
		persistent := pgstore.New(pool, logger.Named("store"))
		return &Backend{Store: persistent, Audit: persistent, Cost: persistent, PostgresPool: pool, Claimant: api.RuntimeApprovalClaimant{WorkloadUID: config.WorkloadUID}, Close: pool.Close}, nil
	case "", "kubernetes":
		if config.ControlNamespace == "" || config.AuditNamespace == "" {
			return nil, fmt.Errorf("control and audit namespaces are required in Kubernetes mode")
		}
		clusterConfig, err := ctrl.GetConfig()
		if err != nil {
			return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
		}
		clusterClient, err := client.New(clusterConfig, client.Options{Scheme: k8sctrl.SetupScheme()})
		if err != nil {
			return nil, fmt.Errorf("create Kubernetes client: %w", err)
		}
		var claimant api.RuntimeApprovalClaimant
		persistent := k8sctrl.NewStoreAdapterWithControlNamespace(clusterClient, config.ControlNamespace)
		if config.Component == "tool-gateway" || config.Component == "agent-communication-gateway" {
			if err := persistent.ConfigureToolInvocationReceiptWriter(config.Component); err != nil {
				return nil, fmt.Errorf("configure isolated gateway receipts: %w", err)
			}
		}
		if config.Component != "" {
			claimant, err = k8sctrl.WaitForRuntimeApprovalClaimant(ctx, clusterClient, config.ControlNamespace, config.Hostname, config.Component, config.WorkloadUID)
			if err != nil {
				return nil, fmt.Errorf("resolve runtime approval claimant: %w", err)
			}
			if err := persistent.ConfigureRuntimeApprovalClock(config.MaximumClockSkew, k8sctrl.NewHostClockHealthCheck(config.MaximumClockSkew)); err != nil {
				return nil, fmt.Errorf("configure runtime approval clock: %w", err)
			}
		}
		audit, err := k8saudit.NewConfigMapStore(clusterClient, config.AuditNamespace)
		if err != nil {
			return nil, fmt.Errorf("configure Kubernetes audit store: %w", err)
		}
		cost, err := k8scost.NewConfigMapStore(clusterClient, config.AuditNamespace)
		if err != nil {
			return nil, fmt.Errorf("configure Kubernetes cost store: %w", err)
		}
		return &Backend{Store: persistent, Audit: audit, Cost: cost, KubernetesClient: clusterClient, Claimant: claimant, Close: func() {}}, nil
	default:
		return nil, fmt.Errorf("unsupported KOGNITION_MODE %q: expected standalone or kubernetes", config.Mode)
	}
}
