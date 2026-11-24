// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package main implements the Kubernetes-native controller manager binary.
// This binary runs all CRD reconcilers and admission webhooks under a single
// controller-runtime manager, providing a standalone K8s control plane for Kognition.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	anthropicdrv "github.com/kognition-ai/kognition/internal/cpi/drivers/anthropic"
	bedrockdrv "github.com/kognition-ai/kognition/internal/cpi/drivers/bedrock"
	foundrydrv "github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	ollamadrv "github.com/kognition-ai/kognition/internal/cpi/drivers/ollama"
	openaidrv "github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/invocation"
	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/k8s/webhook"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	cognamespace "github.com/kognition-ai/kognition/internal/namespace"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/plugin"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/toolgateway"
)

type managerReconciler interface {
	SetupWithManager(ctrl.Manager) error
}

type controllerAssembly struct {
	reconcilers []managerReconciler
	mutating    []admission.MutatingHook
	validating  []admission.ValidatingHook
	pluginHost  *plugin.Host
}

type controllerOptions struct {
	metricsAddr     string
	probeAddr       string
	leaderElect     bool
	leaderElectID   string
	leaderElectNS   string
	disableWebhooks bool
}

func requiresAutomaticIdentity(agentDispatcher *dispatcher.K8sDispatcher) bool {
	return agentDispatcher.UsesModelGateway() || agentDispatcher.UsesAgentCommunicationGateway() || agentDispatcher.UsesKnowledgeGateway()
}

type autonomyDefaultsRunnable struct {
	store       store.Store
	logger      *zap.Logger
	ensure      func(context.Context, store.Store, *zap.Logger) error
	retryPeriod time.Duration
}

type runtimeApprovalBindingKeysRunnable struct {
	store *k8sctrl.StoreAdapter
	keys  runtimeapproval.BindingKeyStore
}

type pluginHostRunnable struct {
	host  *plugin.Host
	ready atomic.Bool
}

func (r *runtimeApprovalBindingKeysRunnable) Start(ctx context.Context) error {
	if err := r.store.ActivateRuntimeApprovalBindingKeys(ctx, r.keys); err != nil {
		return fmt.Errorf("activate RuntimeApproval binding keys: %w", err)
	}
	<-ctx.Done()
	return nil
}

func (*runtimeApprovalBindingKeysRunnable) NeedLeaderElection() bool { return false }

func (r *pluginHostRunnable) Start(ctx context.Context) error {
	if err := r.host.Start(ctx); err != nil {
		return fmt.Errorf("start plugin host: %w", err)
	}
	r.ready.Store(true)
	defer func() {
		r.ready.Store(false)
		r.host.Stop()
	}()
	<-ctx.Done()
	return nil
}

func (r *pluginHostRunnable) CheckReady(*http.Request) error {
	if !r.ready.Load() {
		return errors.New("plugin host is not running")
	}
	return nil
}

func (*pluginHostRunnable) NeedLeaderElection() bool { return false }

func (r *autonomyDefaultsRunnable) Start(ctx context.Context) error {
	ensure := r.ensureFunc()
	retryPeriod := r.retryInterval()

	for {
		if err := ensure(ctx, r.store, r.logger); err == nil {
			break
		} else {
			r.logger.Warn("unable to ensure default autonomy classes; retrying", zap.Error(err), zap.Duration("retry-after", retryPeriod))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryPeriod):
		}
	}
	<-ctx.Done()
	return nil
}

func (r *autonomyDefaultsRunnable) ensureFunc() func(context.Context, store.Store, *zap.Logger) error {
	if r.ensure != nil {
		return r.ensure
	}
	return intctrl.EnsureDefaultAutonomyClasses
}

func (r *autonomyDefaultsRunnable) retryInterval() time.Duration {
	if r.retryPeriod > 0 {
		return r.retryPeriod
	}
	return 2 * time.Second
}

func (*autonomyDefaultsRunnable) NeedLeaderElection() bool { return true }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admission-guard" {
		os.Exit(runAdmissionGuardCommand(os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "resource-webhooks" {
		os.Exit(runControllerCommand(os.Args[2:], os.Stderr, newControllerLogger, runResourceWebhooks))
	}
	os.Exit(runControllerCommand(os.Args[1:], os.Stderr, newControllerLogger, runController))
}

func newControllerLogger() (*zap.Logger, error) {
	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig.TimeKey = "ts"
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	return zapCfg.Build()
}

func runControllerCommand(
	args []string,
	stderr io.Writer,
	newLogger func() (*zap.Logger, error),
	run func(controllerOptions, *zap.Logger) error,
) int {
	options, err := parseControllerOptions(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger, err := newLogger()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = logger.Sync() }()
	logger.Info("starting kognition-k8s-controller",
		zap.String("metrics", options.metricsAddr),
		zap.String("probes", options.probeAddr),
		zap.Bool("leader-elect", options.leaderElect),
	)
	if err := run(options, logger); err != nil {
		logger.Error("controller failed", zap.Error(err))
		return 1
	}
	return 0
}

func runController(options controllerOptions, logger *zap.Logger) error {
	return runControllerWithManager(options, logger, k8sctrl.NewManager)
}

func runControllerWithManager(
	options controllerOptions,
	logger *zap.Logger,
	newManager func(k8sctrl.ManagerOptions) (ctrl.Manager, error),
) error {
	mgr, err := newManager(managerOptions(options, logger))
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	pluginHost, err := configurePluginHost(mgr, logger)
	if err != nil {
		return err
	}

	controllerNamespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	if controllerNamespace == "" {
		return fmt.Errorf("KOGNITION_CONTROLLER_NAMESPACE is required")
	}
	st := k8sctrl.NewStoreAdapterWithControlNamespace(mgr.GetClient(), controllerNamespace)
	st.ConfigureRuntimeApprovalReader(mgr.GetAPIReader())
	quotaLimits, err := runtimeApprovalQuotaLimitsFromEnv()
	if err != nil {
		return err
	}
	if err := st.ConfigureRuntimeApprovalQuota(quotaLimits); err != nil {
		return fmt.Errorf("configure RuntimeApproval quota: %w", err)
	}
	maximumClockSkew, err := runtimeApprovalClockSkewFromEnv()
	if err != nil {
		return err
	}
	if err := st.ConfigureRuntimeApprovalClock(maximumClockSkew, k8sctrl.NewHostClockHealthCheck(maximumClockSkew)); err != nil {
		return fmt.Errorf("configure RuntimeApproval host clock: %w", err)
	}
	executionRecords, err := k8sctrl.NewWorkflowExecutorRecordStore(mgr.GetClient(), mgr.GetAPIReader(), controllerNamespace, func() (time.Time, error) { return st.WorkflowExecutorClock(context.Background()) })
	if err != nil {
		return fmt.Errorf("configure protected Workflow execution records: %w", err)
	}
	if err := st.ConfigureWorkflowExecutorRecords(executionRecords); err != nil {
		return err
	}
	if err := st.ConfigureWorkflowExecutorReader(mgr.GetAPIReader()); err != nil {
		return err
	}
	executorConfig, err := workflowExecutorAuthorizationFromEnv(context.Background(), mgr.GetConfig(), mgr.GetAPIReader(), controllerNamespace)
	if err != nil {
		return fmt.Errorf("configure native Workflow executor identity: %w", err)
	}
	if executorConfig != nil {
		if options.disableWebhooks {
			return fmt.Errorf("configured Workflow executor requires admission protection")
		}
		if err := st.ConfigureWorkflowExecutorAuthorization(*executorConfig); err != nil {
			return err
		}
	}
	if err := mgr.AddReadyzCheck("runtime-approval-clock", func(*http.Request) error {
		return st.RuntimeApprovalClockReady()
	}); err != nil {
		return fmt.Errorf("register RuntimeApproval clock readiness: %w", err)
	}
	if err := mgr.Add(&autonomyDefaultsRunnable{store: st, logger: logger.Named("autonomy-defaults")}); err != nil {
		return fmt.Errorf("register autonomy defaults: %w", err)
	}
	if err := mgr.Add(k8sctrl.NewCRStateMetrics(mgr.GetClient(), ctrlmetrics.Registry, logger.Named("cr-state-metrics"), 30*time.Second)); err != nil {
		return fmt.Errorf("register CR state metrics: %w", err)
	}
	bindingKeys, err := runtimeapproval.BindingKeyStoreFromJSON(os.Getenv(runtimeapproval.BindingKeysEnvironmentVariable))
	if err != nil {
		return fmt.Errorf("configure RuntimeApproval binding keys: %w", err)
	}
	if err := mgr.Add(&runtimeApprovalBindingKeysRunnable{store: st, keys: bindingKeys}); err != nil {
		return fmt.Errorf("register RuntimeApproval binding-key activation: %w", err)
	}
	if err := mgr.AddReadyzCheck("runtime-approval-binding-keys", func(request *http.Request) error {
		return st.RuntimeApprovalBindingKeysReady(request.Context())
	}); err != nil {
		return fmt.Errorf("register RuntimeApproval binding-key readiness: %w", err)
	}
	var workflowSubmissions *webhook.WorkflowRunSubmissionService
	if executorConfig != nil {
		reviews, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			return fmt.Errorf("configure Workflow submission access reviews: %w", err)
		}
		workflowSubmissions = &webhook.WorkflowRunSubmissionService{
			Authorizer: &webhook.WorkflowUseAdmissionAuthorizer{Reader: mgr.GetAPIReader(), AccessReviews: reviews.AuthorizationV1().SubjectAccessReviews()},
			Bindings:   executionRecords, Receipts: executionRecords, Keys: bindingKeys, RecordNamespace: controllerNamespace,
			CurrentWriter: st.AuthenticatedWorkflowExecutorUser,
			Clock: func() (time.Time, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := st.RuntimeApprovalBindingKeysReady(ctx); err != nil {
					return time.Time{}, err
				}
				return st.WorkflowExecutorClock(ctx)
			},
		}
		if err := st.ConfigureWorkflowRunSubmission(workflowSubmissions); err != nil {
			return fmt.Errorf("configure verified Workflow submission finalizer: %w", err)
		}
	}
	if err := configureCanaryOperationRetention(mgr, st, logger, os.Getenv("KOGNITION_CANARY_OPERATION_RETENTION")); err != nil {
		return err
	}
	auditStore, err := k8saudit.NewConfigMapStore(mgr.GetClient(), controllerNamespace)
	if err != nil {
		return fmt.Errorf("configure Kubernetes audit store: %w", err)
	}
	if err := mgr.Add(k8saudit.NewRetentionRunnable(mgr.GetClient(), auditStore, logger, time.Hour)); err != nil {
		return fmt.Errorf("register audit retention: %w", err)
	}
	costStore, err := k8scost.NewConfigMapStore(mgr.GetClient(), controllerNamespace)
	if err != nil {
		return fmt.Errorf("configure Kubernetes cost store: %w", err)
	}
	if err := mgr.Add(&recovery.Retention{Storage: costStore}); err != nil {
		return fmt.Errorf("configure runtime recovery retention: %w", err)
	}
	if err := mgr.Add(k8scost.NewRetentionRunnable(mgr.GetClient(), costStore, logger, time.Hour)); err != nil {
		return fmt.Errorf("register cost retention: %w", err)
	}
	usageWriter := observability.NewModelUsageMetrics(ctrlmetrics.Registry).Wrap(costStore)
	observability.NewWorkflowMetrics(ctrlmetrics.Registry, st)
	meteredAuditStore := observability.NewTrustDecisionMetrics(ctrlmetrics.Registry).Wrap(auditStore)
	auditLog := observability.NewCognitiveAuditLog(meteredAuditStore)
	tracePipeline, err := observability.NewTracePipelineFromEnv(context.Background())
	if err != nil {
		return fmt.Errorf("configure OpenTelemetry tracing: %w", err)
	}
	defer shutdownTracePipeline(tracePipeline, logger)
	if tracePipeline.Enabled() {
		logger.Info("OpenTelemetry trace export enabled", zap.String("endpoint", tracePipeline.Config.Endpoint), zap.String("protocol", string(tracePipeline.Config.Protocol)))
	}
	tracePipeline.Tracer.SetFallibleCostSink(func(record observability.CostRecord) error {
		writeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var err error
		if record.OperationType == observability.CostOpModelInference {
			_, err = usageWriter.ApplyUsageEvent(writeCtx, record)
		} else {
			err = costStore.RecordCost(writeCtx, record)
		}
		if err != nil {
			logger.Warn("persist Kubernetes cost record", zap.Error(err), zap.String("workspace", record.Workspace), zap.String("trace_id", record.TraceID), zap.String("span_id", record.SpanID))
		}
		return err
	})

	opaEngine := opa.NewEngine()
	cpiRegistry := newCPIRegistry()
	dispatcherConfig, err := dispatcherConfigFromEnv()
	if err != nil {
		return fmt.Errorf("configure runtime dispatcher: %w", err)
	}
	agentDispatcher := dispatcher.NewK8sDispatcher(mgr.GetClient(), st, logger, dispatcherConfig)
	assembly := newControllerAssembly(st, mgr.GetClient(), mgr.GetConfig(), agentDispatcher, pluginHost, logger, cpiRegistry, opaEngine, ingesterOptionsFromEnv(), auditLog, tracePipeline, usageWriter, workflowToolTracingOptions(tracePipeline)...)
	identityContext, cancelIdentityContext := context.WithCancel(context.Background())
	defer cancelIdentityContext()
	identityReader, closeIdentityReader, err := runtimeauth.LoadWorkloadIdentityStatusRegistry(identityContext, os.Getenv)
	if err != nil {
		return fmt.Errorf("configure workload identity status reporting: %w", err)
	}
	defer closeIdentityReader()
	if identityReader != nil {
		if workflowSubmissions != nil {
			assembly.reconcilers = append(assembly.reconcilers, k8sctrl.NewWorkflowRunSubmissionReconciler(mgr.GetAPIReader(), st))
		}
		for _, reconciler := range assembly.reconcilers {
			if identityAdapter, ok := reconciler.(*k8sctrl.AgentIdentityReconcilerAdapter); ok {
				identityAdapter.ConfigureWorkloadIdentityReader(st, identityReader)
			}
		}
	}
	collaborationRunner, err := workflowCollaborationRunnerFromEnv(mgr, st, intctrl.NewCommunicationPolicyEngine(st, logger.Named("workflow-collaboration-policy")), logger)
	if err != nil {
		return fmt.Errorf("configure workflow collaboration: %w", err)
	}
	if collaborationRunner != nil {
		for _, reconciler := range assembly.reconcilers {
			switch adapter := reconciler.(type) {
			case *k8sctrl.WorkflowReconcilerAdapter:
				adapter.SetCollaborationRunner(collaborationRunner)
			case *k8sctrl.WorkflowRunReconcilerAdapter:
				adapter.SetCollaborationRunner(collaborationRunner)
			}
		}
		enableCollaborationAdmission(assembly.validating)
	}
	runtimeApprovalRetention, err := k8sctrl.RuntimeApprovalTombstoneRetention(os.Getenv("KOGNITION_RUNTIME_APPROVAL_TOMBSTONE_RETENTION"))
	if err != nil {
		return fmt.Errorf("configure RuntimeApproval tombstone retention: %w", err)
	}
	assembly.reconcilers = append(assembly.reconcilers,
		k8sctrl.NewRuntimeApprovalToolPreparationReconciler(mgr.GetClient(), st),
		k8sctrl.NewRuntimeApprovalClaimRecoveryReconciler(mgr.GetClient(), st),
		k8sctrl.NewRuntimeApprovalCompactionReconciler(mgr.GetClient(), st, logger, runtimeApprovalRetention),
		k8sctrl.NewRuntimeApprovalRetentionReconciler(mgr.GetClient(), st, logger),
		k8sctrl.NewRuntimeApprovalQuotaTicketRetirementReconciler(mgr.GetClient(), st, logger),
	)
	if err := setupReconcilers(mgr, assembly.reconcilers); err != nil {
		return err
	}

	chain := admission.NewChain(assembly.mutating, assembly.validating)
	bindingOptions, err := workflowBindingWebhookOptionsFromEnv(controllerNamespace, mgr.GetAPIReader())
	if err != nil {
		return err
	}
	if options.disableWebhooks && os.Getenv(runtimeapproval.WorkflowExecutorPrincipalsEnvironmentVariable) != "" {
		return fmt.Errorf("configured Workflow execution bindings cannot disable admission webhooks")
	}
	bindingOptions = append(bindingOptions, webhook.WithAuditLog(auditLog), webhook.WithRuntimeApprovalDecisionStore(st, runtimeApprovalAdmissionUsernamesFromEnv()...), webhook.WithWorkflowRunSubmissions(workflowSubmissions))
	if err := setupWebhooks(options.disableWebhooks, func() error {
		return webhook.SetupWithManager(mgr, chain, logger, bindingOptions...)
	}); err != nil {
		return fmt.Errorf("setup webhooks: %w", err)
	}
	if options.disableWebhooks {
		logger.Info("admission webhooks disabled via --disable-webhooks flag")
	}

	logger.Info("starting controller-runtime manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("start manager: %w", err)
	}
	return nil
}

func runResourceWebhooks(options controllerOptions, logger *zap.Logger) error {
	return runResourceWebhooksWithManager(options, logger, k8sctrl.NewManager)
}

func runResourceWebhooksWithManager(options controllerOptions, logger *zap.Logger, newManager func(k8sctrl.ManagerOptions) (ctrl.Manager, error)) error {
	if options.disableWebhooks {
		return fmt.Errorf("resource-webhooks cannot disable its admission server")
	}
	namespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	if namespace == "" {
		return fmt.Errorf("KOGNITION_CONTROLLER_NAMESPACE is required")
	}
	options.leaderElect = false
	mgr, err := newManager(managerOptions(options, logger))
	if err != nil {
		return fmt.Errorf("create resource admission manager: %w", err)
	}
	direct, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return fmt.Errorf("create resource admission client: %w", err)
	}
	st := k8sctrl.NewStoreAdapterWithControlNamespace(direct, namespace)
	st.ConfigureRuntimeApprovalReader(direct)
	limits, err := runtimeApprovalQuotaLimitsFromEnv()
	if err != nil {
		return err
	}
	if err := st.ConfigureRuntimeApprovalQuota(limits); err != nil {
		return fmt.Errorf("configure admission RuntimeApproval quota: %w", err)
	}
	skew, err := runtimeApprovalClockSkewFromEnv()
	if err != nil {
		return err
	}
	if err := st.ConfigureRuntimeApprovalClock(skew, k8sctrl.NewHostClockHealthCheck(skew)); err != nil {
		return fmt.Errorf("configure admission RuntimeApproval clock: %w", err)
	}
	executionRecords, err := k8sctrl.NewWorkflowExecutorRecordStore(direct, direct, namespace, func() (time.Time, error) { return st.WorkflowExecutorClock(context.Background()) })
	if err != nil {
		return fmt.Errorf("configure protected admission Workflow execution records: %w", err)
	}
	if err := st.ConfigureWorkflowExecutorRecords(executionRecords); err != nil {
		return err
	}
	if err := st.ConfigureWorkflowExecutorReader(direct); err != nil {
		return err
	}
	keys, err := runtimeapproval.BindingKeyStoreFromJSON(os.Getenv(runtimeapproval.BindingKeysEnvironmentVariable))
	if err != nil {
		return fmt.Errorf("configure admission binding keys: %w", err)
	}
	if err := mgr.Add(&runtimeApprovalBindingKeysRunnable{store: st, keys: keys}); err != nil {
		return fmt.Errorf("register admission binding-key activation: %w", err)
	}
	if err := mgr.AddReadyzCheck("runtime-approval-binding-keys", func(request *http.Request) error {
		return st.RuntimeApprovalBindingKeysReady(request.Context())
	}); err != nil {
		return fmt.Errorf("register admission binding-key readiness: %w", err)
	}
	if err := mgr.AddReadyzCheck("runtime-approval-clock", func(*http.Request) error { return st.RuntimeApprovalClockReady() }); err != nil {
		return fmt.Errorf("register admission clock readiness: %w", err)
	}
	auditStore, err := k8saudit.NewConfigMapStore(direct, namespace)
	if err != nil {
		return fmt.Errorf("configure admission audit store: %w", err)
	}
	mutating, validating := admission.ProductionHooksWithExecutionAuthority(st, opa.NewEngine(), runtimeApprovalDecisionAuthorityFromEnv(), runtimeApprovalExecutionWritersFromEnv(), runtimeApprovalAdmissionUsernamesFromEnv()...)
	validating = append(validating, cognamespace.NewProductionCrossNamespaceHook(st))
	if enabled, err := workflowCollaborationEnabled(os.Getenv); err != nil {
		return err
	} else if enabled {
		enableCollaborationAdmission(validating)
	}
	bindingOptions, err := workflowBindingWebhookOptionsFromEnv(namespace, direct)
	if err != nil {
		return err
	}
	bindingOptions = append(bindingOptions, webhook.WithAuditLog(observability.NewCognitiveAuditLog(auditStore)), webhook.WithRuntimeApprovalDecisionStore(st, runtimeApprovalAdmissionUsernamesFromEnv()...))
	if err := webhook.SetupWithManager(mgr, admission.NewChain(mutating, validating), logger, bindingOptions...); err != nil {
		return fmt.Errorf("setup resource admission: %w", err)
	}
	if err := mgr.AddReadyzCheck("resource-webhooks", mgr.GetWebhookServer().StartedChecker()); err != nil {
		return fmt.Errorf("register resource admission readiness: %w", err)
	}
	logger.Info("starting resource admission without reconciliation")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("start resource admission manager: %w", err)
	}
	return nil
}

func workflowBindingWebhookOptionsFromEnv(namespace string, reader client.Reader) ([]webhook.Option, error) {
	principals, err := runtimeapproval.WorkflowExecutorPrincipalsFromJSON(os.Getenv(runtimeapproval.WorkflowExecutorPrincipalsEnvironmentVariable))
	if err != nil {
		return nil, fmt.Errorf("configure Workflow executor principals: %w", err)
	}
	var subjects, groups []string
	for _, principal := range principals {
		parts := strings.Split(principal.Subject, ":")
		if principal.Method != "tokenreview" || principal.Issuer != "kubernetes.io/tokenreview" || len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 {
			return nil, fmt.Errorf("native Workflow executor must identify an exact TokenReview ServiceAccount")
		}
		subjects = append(subjects, principal.Subject)
	}
	for _, group := range strings.Split(os.Getenv("KOGNITION_WORKFLOW_BINDING_ADMIN_GROUPS"), ",") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if strings.Contains(group, "*") || group == "system:authenticated" || group == "system:unauthenticated" || group == "system:serviceaccounts" || strings.HasPrefix(group, "system:serviceaccounts:") || len(group) > 256 || len(groups) >= 16 {
			return nil, fmt.Errorf("Workflow binding administrators require bounded explicit operator groups")
		}
		groups = append(groups, group)
	}
	return []webhook.Option{webhook.WithWorkflowBindingAdministrators(namespace, groups, subjects), webhook.WithWorkflowBindingReader(reader)}, nil
}

func workflowExecutorAuthorizationFromEnv(ctx context.Context, config *rest.Config, reader client.Reader, namespace string) (*k8sctrl.WorkflowExecutorKubernetesAuthorization, error) {
	principals, err := runtimeapproval.WorkflowExecutorPrincipalsFromJSON(os.Getenv(runtimeapproval.WorkflowExecutorPrincipalsEnvironmentVariable))
	if err != nil {
		return nil, err
	}
	if len(principals) == 0 {
		return nil, nil
	}
	podName := os.Getenv("KOGNITION_WORKFLOW_EXECUTOR_POD_NAME")
	tokenFile := os.Getenv("KOGNITION_WORKFLOW_EXECUTOR_TOKEN_FILE")
	if config == nil || reader == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(podName)) != 0 || !filepath.IsAbs(tokenFile) || len(tokenFile) > 4096 {
		return nil, fmt.Errorf("native executor requires its exact pod, uncached reader and explicit absolute credential file")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); err != nil {
		return nil, fmt.Errorf("read executor pod identity: %w", err)
	}
	if pod.UID == "" || pod.DeletionTimestamp != nil || pod.Namespace != namespace || pod.Name != podName || len(validation.IsDNS1123Subdomain(pod.Spec.ServiceAccountName)) != 0 {
		return nil, fmt.Errorf("executor pod has no exact live ServiceAccount identity")
	}
	subject := api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "system:serviceaccount:" + namespace + ":" + pod.Spec.ServiceAccountName}
	approved := false
	for _, principal := range principals {
		approved = approved || principal == subject
	}
	if !approved {
		return nil, fmt.Errorf("executor pod does not match an explicitly configured native principal")
	}
	account := &corev1.ServiceAccount{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod.Spec.ServiceAccountName}, account); err != nil {
		return nil, fmt.Errorf("read executor ServiceAccount identity: %w", err)
	}
	if account.UID == "" || account.DeletionTimestamp != nil || account.Namespace != namespace || account.Name != pod.Spec.ServiceAccountName {
		return nil, fmt.Errorf("executor ServiceAccount has no immutable live identity")
	}
	credential := boundedTokenFileCredential(tokenFile)
	if _, err := credential(ctx); err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create executor identity review client: %w", err)
	}
	return &k8sctrl.WorkflowExecutorKubernetesAuthorization{TokenReviews: clientset.AuthenticationV1().TokenReviews(), AccessReviews: clientset.AuthorizationV1().SubjectAccessReviews(), Credential: credential, Subject: subject.Subject, WorkloadUID: string(pod.UID), ServiceAccountUID: string(account.UID), PodNamespace: namespace, PodName: podName, Pods: reader}, nil
}

func boundedTokenFileCredential(tokenFile string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file, err := os.Open(tokenFile)
		if err != nil {
			return "", fmt.Errorf("executor credential file unavailable")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 65537))
		if err != nil || len(raw) > 65536 {
			return "", fmt.Errorf("executor credential exceeds its finite bound or is unreadable")
		}
		token := strings.TrimSpace(string(raw))
		if token == "" || strings.ContainsAny(token, " \t\r\n") {
			return "", fmt.Errorf("executor credential is empty or malformed")
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return token, nil
	}
}

func configurePluginHost(mgr ctrl.Manager, logger *zap.Logger) (*plugin.Host, error) {
	cfg, enabled, err := pluginHostConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, nil
	}
	host := plugin.NewHost(cfg, logger.Named("plugin-host"))
	runnable := &pluginHostRunnable{host: host}
	if err := mgr.Add(runnable); err != nil {
		return nil, fmt.Errorf("register plugin host: %w", err)
	}
	if err := mgr.AddReadyzCheck("plugin-host", runnable.CheckReady); err != nil {
		return nil, fmt.Errorf("register plugin host readiness: %w", err)
	}
	logger.Info("plugin host enabled", zap.String("socket-dir", cfg.SocketDir))
	return host, nil
}

func pluginHostConfigFromEnv() (plugin.HostConfig, bool, error) {
	cfg := plugin.DefaultHostConfig()
	enabled, err := strconv.ParseBool(getEnvOrDefault("KOGNITION_PLUGIN_HOST_ENABLED", "false"))
	if err != nil {
		return plugin.HostConfig{}, false, fmt.Errorf("KOGNITION_PLUGIN_HOST_ENABLED must be a boolean")
	}
	if !enabled {
		return cfg, false, nil
	}

	cfg.SocketDir = getEnvOrDefault("KOGNITION_PLUGIN_SOCKET_DIR", cfg.SocketDir)
	cfg.HostVersion = getEnvOrDefault("KOGNITION_PLUGIN_HOST_VERSION", cfg.HostVersion)
	durations := []struct {
		name  string
		value *time.Duration
	}{
		{"KOGNITION_PLUGIN_DISCOVERY_INTERVAL", &cfg.DiscoveryInterval},
		{"KOGNITION_PLUGIN_PROBE_INTERVAL", &cfg.ProbeInterval},
		{"KOGNITION_PLUGIN_PROBE_TIMEOUT", &cfg.ProbeTimeout},
		{"KOGNITION_PLUGIN_CONNECT_TIMEOUT", &cfg.ConnectTimeout},
		{"KOGNITION_PLUGIN_RECONNECT_BACKOFF", &cfg.ReconnectBackoff},
		{"KOGNITION_PLUGIN_MAX_RECONNECT_BACKOFF", &cfg.MaxReconnectBackoff},
	}
	for _, configured := range durations {
		value, parseErr := time.ParseDuration(getEnvOrDefault(configured.name, configured.value.String()))
		if parseErr != nil || value <= 0 {
			return plugin.HostConfig{}, false, fmt.Errorf("%s must be a positive duration", configured.name)
		}
		*configured.value = value
	}
	if strings.TrimSpace(cfg.SocketDir) == "" {
		return plugin.HostConfig{}, false, fmt.Errorf("KOGNITION_PLUGIN_SOCKET_DIR must not be empty")
	}
	if cfg.MaxReconnectBackoff < cfg.ReconnectBackoff {
		return plugin.HostConfig{}, false, fmt.Errorf("KOGNITION_PLUGIN_MAX_RECONNECT_BACKOFF must not be less than KOGNITION_PLUGIN_RECONNECT_BACKOFF")
	}
	return cfg, true, nil
}

func runtimeApprovalClockSkewFromEnv() (time.Duration, error) {
	configured := getEnvOrDefault("KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW", "30s")
	skew, err := time.ParseDuration(configured)
	if err != nil || skew < 30*time.Second {
		return 0, fmt.Errorf("KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW must be a duration of at least 30s")
	}
	return skew, nil
}

func runtimeApprovalQuotaLimitsFromEnv() (k8sctrl.RuntimeApprovalQuotaLimits, error) {
	defaults := k8sctrl.DefaultRuntimeApprovalQuotaLimits()
	values := []struct {
		name     string
		value    *int64
		fallback int64
	}{
		{"KOGNITION_RUNTIME_APPROVAL_WORKSPACE_RECORDS", &defaults.WorkspaceRecords, defaults.WorkspaceRecords},
		{"KOGNITION_RUNTIME_APPROVAL_WORKSPACE_BYTES", &defaults.WorkspaceBytes, defaults.WorkspaceBytes},
		{"KOGNITION_RUNTIME_APPROVAL_AGENT_RECORDS", &defaults.AgentRecords, defaults.AgentRecords},
		{"KOGNITION_RUNTIME_APPROVAL_AGENT_BYTES", &defaults.AgentBytes, defaults.AgentBytes},
	}
	for _, configured := range values {
		value, err := strconv.ParseInt(getEnvOrDefault(configured.name, strconv.FormatInt(configured.fallback, 10)), 10, 64)
		if err != nil {
			return k8sctrl.RuntimeApprovalQuotaLimits{}, fmt.Errorf("%s must be a positive base-10 integer", configured.name)
		}
		*configured.value = value
	}
	if err := k8sctrl.ValidateRuntimeApprovalQuotaLimits(defaults); err != nil {
		return k8sctrl.RuntimeApprovalQuotaLimits{}, fmt.Errorf("configure RuntimeApproval quota: %w", err)
	}
	return defaults, nil
}

func configureCanaryOperationRetention(mgr ctrl.Manager, st store.Store, logger *zap.Logger, value string) error {
	retention, enabled, err := intctrl.CanaryOperationRetention(value)
	if err != nil {
		return fmt.Errorf("configure canary operation retention: %w", err)
	}
	if !enabled {
		return nil
	}
	operations, ok := st.(store.CanaryOperationStore)
	if !ok {
		logger.Info("canary operation retention unavailable for configured store")
		return nil
	}
	if err := mgr.Add(intctrl.NewCanaryOperationRetentionRunnable(operations, logger, retention, intctrl.DefaultCanaryCompactionInterval)); err != nil {
		return fmt.Errorf("register canary operation retention: %w", err)
	}
	return nil
}

func managerOptions(options controllerOptions, logger *zap.Logger) k8sctrl.ManagerOptions {
	namespaces := []string{strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))}
	if configured := os.Getenv("KOGNITION_MANAGED_NAMESPACES"); configured != "" {
		for _, namespace := range strings.Split(configured, ",") {
			namespaces = append(namespaces, strings.TrimSpace(namespace))
		}
	}
	if namespace := strings.TrimSpace(os.Getenv("KOGNITION_AGENT_NAMESPACE")); namespace != "" {
		namespaces = append(namespaces, namespace)
	}
	return k8sctrl.ManagerOptions{
		ManagedNamespaces:       namespaces,
		MetricsBindAddress:      options.metricsAddr,
		HealthProbeBindAddress:  options.probeAddr,
		LeaderElection:          options.leaderElect,
		LeaderElectionID:        options.leaderElectID,
		LeaderElectionNamespace: options.leaderElectNS,
		DisableWebhooks:         options.disableWebhooks,
		Logger:                  logger,
	}
}

func dispatcherConfigFromEnv() (dispatcher.Config, error) {
	workloadIdentityDelivery, err := dispatcher.WorkloadIdentityDeliveryFromEnv(os.Getenv)
	if err != nil {
		return dispatcher.Config{}, err
	}
	runtimeNetworkPolicyEnabled, err := strconv.ParseBool(getEnvOrDefault("KOGNITION_RUNTIME_NETWORK_POLICY_ENABLED", "false"))
	if err != nil {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_RUNTIME_NETWORK_POLICY_ENABLED must be a boolean")
	}
	ciliumHostProbeIngress, err := strconv.ParseBool(getEnvOrDefault("KOGNITION_CILIUM_HOST_PROBE_INGRESS_ENABLED", "false"))
	if err != nil {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_CILIUM_HOST_PROBE_INGRESS_ENABLED must be a boolean")
	}
	var kubeletProbeIngressCIDRs []string
	for _, rawCIDR := range strings.Split(os.Getenv("KOGNITION_KUBELET_PROBE_INGRESS_CIDRS"), ",") {
		cidr := strings.TrimSpace(rawCIDR)
		if cidr == "" {
			continue
		}
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return dispatcher.Config{}, fmt.Errorf("KOGNITION_KUBELET_PROBE_INGRESS_CIDRS contains invalid CIDR %q: %w", cidr, err)
		}
		kubeletProbeIngressCIDRs = append(kubeletProbeIngressCIDRs, cidr)
	}
	if runtimeNetworkPolicyEnabled && len(kubeletProbeIngressCIDRs) == 0 && !ciliumHostProbeIngress {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_KUBELET_PROBE_INGRESS_CIDRS is required when runtime NetworkPolicies are enabled")
	}
	if workloadIdentityDelivery != nil && (!runtimeNetworkPolicyEnabled || strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE")) == "") {
		return dispatcher.Config{}, fmt.Errorf("managed SPIRE workloads require runtime NetworkPolicies and controller namespace")
	}
	policyGatewayURL := os.Getenv("KOGNITION_MEMORY_POLICY_GATEWAY_URL")
	policyGatewayPort, err := strconv.Atoi(getEnvOrDefault("KOGNITION_MEMORY_POLICY_GATEWAY_PORT", "8091"))
	if err != nil || policyGatewayPort < 1 || policyGatewayPort > 65535 {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_MEMORY_POLICY_GATEWAY_PORT must be a valid TCP port")
	}
	runtimeAuthToken := os.Getenv("KOGNITION_RUNTIME_AUTH_TOKEN")
	runtimeAuthTokenSecretName := os.Getenv("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_NAME")
	if runtimeAuthToken != "" && runtimeAuthTokenSecretName == "" {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_NAME is required when KOGNITION_RUNTIME_AUTH_TOKEN is configured")
	}
	var capabilityIssuer *runtimeauth.MemoryCapabilityIssuer
	if policyGatewayURL != "" {
		keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("KOGNITION_MEMORY_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("KOGNITION_MEMORY_CAPABILITY_HMAC_KEY")), os.Getenv("KOGNITION_MEMORY_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("KOGNITION_MEMORY_CAPABILITY_HMAC_PREVIOUS_KEY")))
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure memory capability key ring: %w", err)
		}
		issuer, err := runtimeauth.NewMemoryCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure memory capability issuer: %w", err)
		}
		capabilityIssuer = issuer
	}
	toolGatewayURL := os.Getenv("KOGNITION_TOOL_GATEWAY_URL")
	toolGatewayPort, err := strconv.Atoi(getEnvOrDefault("KOGNITION_TOOL_GATEWAY_PORT", "8092"))
	if err != nil || toolGatewayPort < 1 || toolGatewayPort > 65535 {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_TOOL_GATEWAY_PORT must be a valid TCP port")
	}
	var toolSessionIssuer *runtimeauth.ToolSessionCapabilityIssuer
	if toolGatewayURL != "" {
		keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("KOGNITION_TOOL_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("KOGNITION_TOOL_CAPABILITY_HMAC_KEY")), os.Getenv("KOGNITION_TOOL_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("KOGNITION_TOOL_CAPABILITY_HMAC_PREVIOUS_KEY")))
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure tool session capability key ring: %w", err)
		}
		issuer, err := runtimeauth.NewToolSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure tool session capability issuer: %w", err)
		}
		toolSessionIssuer = issuer
	}
	modelGatewayURL := os.Getenv("KOGNITION_MODEL_GATEWAY_URL")
	modelGatewayPort, err := strconv.Atoi(getEnvOrDefault("KOGNITION_MODEL_GATEWAY_PORT", "8093"))
	if err != nil || modelGatewayPort < 1 || modelGatewayPort > 65535 {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_MODEL_GATEWAY_PORT must be a valid TCP port")
	}
	var modelSessionIssuer *runtimeauth.ModelSessionCapabilityIssuer
	if modelGatewayURL != "" {
		keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("KOGNITION_MODEL_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("KOGNITION_MODEL_CAPABILITY_HMAC_KEY")), os.Getenv("KOGNITION_MODEL_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("KOGNITION_MODEL_CAPABILITY_HMAC_PREVIOUS_KEY")))
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure model session capability key ring: %w", err)
		}
		issuer, err := runtimeauth.NewModelSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure model session capability issuer: %w", err)
		}
		modelSessionIssuer = issuer
	}
	knowledgeGatewayURL := os.Getenv("KOGNITION_KNOWLEDGE_GATEWAY_URL")
	knowledgeGatewayPort, err := strconv.Atoi(getEnvOrDefault("KOGNITION_KNOWLEDGE_GATEWAY_PORT", "8095"))
	if err != nil || knowledgeGatewayPort < 1 || knowledgeGatewayPort > 65535 {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_KNOWLEDGE_GATEWAY_PORT must be a valid TCP port")
	}
	var knowledgeSessionIssuer *runtimeauth.KnowledgeSessionCapabilityIssuer
	if knowledgeGatewayURL != "" {
		keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_KEY")), os.Getenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_PREVIOUS_KEY")))
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure knowledge session capability key ring: %w", err)
		}
		issuer, err := runtimeauth.NewKnowledgeSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure knowledge session capability issuer: %w", err)
		}
		knowledgeSessionIssuer = issuer
	}
	agentCommunicationGatewayURL := os.Getenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL")
	agentCommunicationGatewayPort, err := strconv.Atoi(getEnvOrDefault("KOGNITION_AGENT_COMMUNICATION_GATEWAY_PORT", "8094"))
	if err != nil || agentCommunicationGatewayPort < 1 || agentCommunicationGatewayPort > 65535 {
		return dispatcher.Config{}, fmt.Errorf("KOGNITION_AGENT_COMMUNICATION_GATEWAY_PORT must be a valid TCP port")
	}
	var agentDelegationSessionIssuer *runtimeauth.AgentDelegationSessionCapabilityIssuer
	if agentCommunicationGatewayURL != "" {
		keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_KEY_ID"), []byte(os.Getenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_KEY")), os.Getenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_PREVIOUS_KEY_ID"), []byte(os.Getenv("KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_PREVIOUS_KEY")))
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure agent delegation capability key ring: %w", err)
		}
		issuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
		if err != nil {
			return dispatcher.Config{}, fmt.Errorf("configure agent delegation session capability issuer: %w", err)
		}
		agentDelegationSessionIssuer = issuer
	}
	return dispatcher.Config{
		AgentImage:              getEnvOrDefault("KOGNITION_AGENT_IMAGE", "localhost:5000/kognition/agent-runtime:latest"),
		RuntimeTLSSecretName:    os.Getenv("KOGNITION_RUNTIME_TLS_SECRET_NAME"),
		RuntimeCAFile:           os.Getenv("KOGNITION_RUNTIME_CA_FILE"),
		RuntimeRecoveryKeysFile: os.Getenv("KOGNITION_RUNTIME_RECOVERY_KEYS_FILE"),
		RuntimeImages: map[string]string{
			"autogen": os.Getenv("KOGNITION_RUNTIME_AUTOGEN_IMAGE"),
			"mcp":     os.Getenv("KOGNITION_RUNTIME_MCP_IMAGE"),
		},
		AgentNamespace:                         os.Getenv("KOGNITION_AGENT_NAMESPACE"),
		MemoryServiceURL:                       os.Getenv("KOGNITION_MEMORY_SERVICE_URL"),
		MemoryPolicyGatewayURL:                 policyGatewayURL,
		MemoryPolicyGatewayPort:                policyGatewayPort,
		MemoryCapabilityIssuer:                 capabilityIssuer,
		ToolGatewayURL:                         toolGatewayURL,
		ToolGatewayPort:                        toolGatewayPort,
		ToolGatewayNamespace:                   os.Getenv("KOGNITION_TOOL_GATEWAY_NAMESPACE"),
		ToolSessionCapabilityIssuer:            toolSessionIssuer,
		ModelGatewayURL:                        modelGatewayURL,
		ModelGatewayPort:                       modelGatewayPort,
		ModelGatewayNamespace:                  os.Getenv("KOGNITION_MODEL_GATEWAY_NAMESPACE"),
		ModelSessionCapabilityIssuer:           modelSessionIssuer,
		KnowledgeGatewayURL:                    knowledgeGatewayURL,
		KnowledgeGatewayPort:                   knowledgeGatewayPort,
		KnowledgeGatewayNamespace:              os.Getenv("KOGNITION_KNOWLEDGE_GATEWAY_NAMESPACE"),
		KnowledgeGatewayCASecretName:           os.Getenv("KOGNITION_KNOWLEDGE_GATEWAY_CA_SECRET_NAME"),
		KnowledgeGatewayCASecretKey:            os.Getenv("KOGNITION_KNOWLEDGE_GATEWAY_CA_SECRET_KEY"),
		KnowledgeSessionCapabilityIssuer:       knowledgeSessionIssuer,
		AgentCommunicationGatewayURL:           agentCommunicationGatewayURL,
		AgentCommunicationGatewayPort:          agentCommunicationGatewayPort,
		AgentCommunicationGatewayNamespace:     os.Getenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_NAMESPACE"),
		AgentDelegationSessionCapabilityIssuer: agentDelegationSessionIssuer,
		RuntimeAuthToken:                       runtimeAuthToken,
		RuntimeAuthTokenSecretName:             runtimeAuthTokenSecretName,
		RuntimeAuthTokenSecretKey:              os.Getenv("KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_KEY"),
		RuntimeNetworkPolicyEnabled:            runtimeNetworkPolicyEnabled,
		WorkloadIdentityMode:                   strings.TrimSpace(os.Getenv("KOGNITION_WORKLOAD_IDENTITY_MODE")),
		WorkloadIdentityDelivery:               workloadIdentityDelivery,
		KubeletProbeIngressCIDRs:               kubeletProbeIngressCIDRs,
		CiliumHostProbeIngress:                 ciliumHostProbeIngress,
		ControllerNamespace:                    os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"),
		MemoryPolicyGatewayNamespace:           os.Getenv("KOGNITION_MEMORY_POLICY_GATEWAY_NAMESPACE"),
	}, nil
}

func ingesterOptionsFromEnv() k8sctrl.KBIngesterOptions {
	options := k8sctrl.KBIngesterOptions{
		Image:               os.Getenv("KOGNITION_KB_INGESTER_IMAGE"),
		ServiceAccount:      os.Getenv("KOGNITION_KB_INGESTER_SERVICE_ACCOUNT"),
		ControllerNamespace: os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"),
	}
	if name := strings.TrimSpace(os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_NAME")); name != "" {
		options.Provider = &k8sctrl.KBIngesterProviderOptions{
			Name: name, Address: os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_ADDRESS"),
			ServerName: os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_SERVER_NAME"), PeerURI: os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_PEER_URI"),
			ConfigurationKind: schema.GroupVersionKind{Group: os.Getenv("KOGNITION_KNOWLEDGE_CONFIGURATION_API_GROUP"), Version: os.Getenv("KOGNITION_KNOWLEDGE_CONFIGURATION_API_VERSION"), Kind: os.Getenv("KOGNITION_KNOWLEDGE_CONFIGURATION_KIND")},
			TLSSecretName:     os.Getenv("KOGNITION_KNOWLEDGE_TLS_SECRET_NAME"), TLSCertificateKey: os.Getenv("KOGNITION_KNOWLEDGE_TLS_CERTIFICATE_KEY"),
			TLSPrivateKeyKey: os.Getenv("KOGNITION_KNOWLEDGE_TLS_PRIVATE_KEY_KEY"), TLSTrustBundleKey: os.Getenv("KOGNITION_KNOWLEDGE_TLS_TRUST_BUNDLE_KEY"),
		}
	}
	return options
}

func intentGatewayServingConfigFromEnv() k8sctrl.IntentGatewayServingConfig {
	return k8sctrl.IntentGatewayServingConfig{
		Namespace:  os.Getenv("KOGNITION_INTENT_GATEWAY_NAMESPACE"),
		Deployment: os.Getenv("KOGNITION_INTENT_GATEWAY_DEPLOYMENT"),
		Endpoint:   os.Getenv("KOGNITION_INTENT_GATEWAY_ENDPOINT"),
	}
}

func parseControllerOptions(args []string) (controllerOptions, error) {
	options := controllerOptions{}
	flags := flag.NewFlagSet("kognition-k8s-controller", flag.ContinueOnError)
	flags.StringVar(&options.metricsAddr, "metrics-bind-address", ":8080", "Address to bind the metrics endpoint")
	flags.StringVar(&options.probeAddr, "health-probe-bind-address", ":8081", "Address to bind health probes")
	flags.BoolVar(&options.leaderElect, "leader-elect", true, "Enable leader election for high availability")
	flags.StringVar(&options.leaderElectID, "leader-election-id", "kognition-k8s-controller", "Leader election lock name")
	flags.StringVar(&options.leaderElectNS, "leader-election-namespace", "", "Namespace for leader election (empty = infer from pod)")
	flags.BoolVar(&options.disableWebhooks, "disable-webhooks", false, "Disable admission webhook server (use when cert-manager is not available)")
	if err := flags.Parse(args); err != nil {
		return controllerOptions{}, err
	}
	return options, nil
}

func newCPIRegistry() *cpi.Registry {
	registry := cpi.NewRegistry()
	registry.MustRegister(openaidrv.New(nil))
	registry.MustRegister(foundrydrv.New(nil))
	registry.MustRegister(anthropicdrv.New(nil))
	registry.MustRegister(ollamadrv.New(nil))
	registry.MustRegister(bedrockdrv.New(nil))
	return registry
}

func setupReconcilers(manager ctrl.Manager, reconcilers []managerReconciler) error {
	for _, reconciler := range reconcilers {
		if err := reconciler.SetupWithManager(manager); err != nil {
			return fmt.Errorf("setup %T: %w", reconciler, err)
		}
	}
	return nil
}

func setupWebhooks(disabled bool, setup func() error) error {
	if disabled {
		return nil
	}
	return setup()
}

func schedulerPluginNamesFromEnv() []string {
	raw := strings.TrimSpace(os.Getenv("KOGNITION_SCHEDULER_PLUGINS"))
	if raw == "" {
		return nil
	}
	names := make([]string, 0)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func newControllerAssembly(
	st store.Store,
	kubeClient client.Client,
	config *rest.Config,
	agentDispatcher *dispatcher.K8sDispatcher,
	pluginHost *plugin.Host,
	logger *zap.Logger,
	cpiRegistry *cpi.Registry,
	opaEngine *opa.Engine,
	ingesterOptions k8sctrl.KBIngesterOptions,
	auditLog *observability.CognitiveAuditLog,
	tracePipeline *observability.TracePipeline,
	usageWriter observability.UsageEventWriter,
	workflowToolOptions ...intctrl.WorkflowToolInvokerOption,
) controllerAssembly {
	if agentDispatcher != nil {
		if pluginHost != nil {
			agentDispatcher.SetRuntimePluginSelector(pluginHost)
		} else {
			agentDispatcher.SetRuntimePluginSelector(nil)
		}
	}
	workflowAdapter := k8sctrl.NewWorkflowReconcilerAdapter(st, logger)
	workflowRunAdapter := k8sctrl.NewWorkflowRunReconcilerAdapter(st, logger)
	if tracePipeline != nil {
		workflowAdapter.SetTracing(tracePipeline.Tracer, tracePipeline.Collector)
		workflowRunAdapter.SetTracing(tracePipeline.Tracer, tracePipeline.Collector)
	}

	// Wire the workflow tool invoker so tool-typed steps execute synchronously.
	// Backed by the protocol-multiplexer executor (OpenAPI + MCP over HTTP);
	// other protocols return a structured unsupported-protocol error.
	toolGrants := intctrl.NewToolGrantEngine(st, logger.Named("tool-grant-engine"))
	toolExecutor := toolexec.New(nil)
	toolDispatcher := runtime.NewToolDispatcher(toolExecutor, toolExecutor)
	runtimeQuota := intctrl.NewQuotaEnforcer(
		st,
		intctrl.NewCognitiveQuotaReconciler(st, logger.Named("runtime-quota")),
		logger.Named("runtime-quota"),
	)
	trustRecorder := intctrl.NewTrustProfileReconciler(st, logger.Named("runtime-trust"))
	trustRecorder.SetAuditLog(auditLog)
	agentPolicies := intctrl.NewAgentPolicyReconciler(st, opaEngine, logger.Named("workflow-agent-policy"))
	communicationPolicy := intctrl.NewCommunicationPolicyEngine(st, logger.Named("workflow-communication-policy"))
	invocationPolicies := intctrl.NewInvocationPolicyEngine(toolGrants, agentPolicies).
		WithAgentRBAC(st, intctrl.NewRBACEngine(st, logger.Named("invocation-rbac")))
	var probeAdapter managerReconciler
	if st != nil {
		modelCore, err := modelgateway.NewCore(st, invocationPolicies)
		if err != nil {
			logger.Panic("create Model Gateway core for reasoning probes", zap.Error(err))
		}
		probeReconciler := intctrl.NewProbeReconciler(st, agentDispatcher, logger.Named("probe-reconciler"))
		canaryExecutor, err := intctrl.NewCPICanaryExecutor(st, cpiRegistry, modelCore)
		if err != nil {
			logger.Warn("reasoning canary execution unavailable for configured store", zap.Error(err))
		} else {
			if usageWriter != nil {
				canaryExecutor.SetUsageEventWriter(usageWriter)
			}
			if tracePipeline != nil && tracePipeline.Enabled() {
				canaryExecutor.SetTracing(tracePipeline.Tracer, tracePipeline.Collector)
			}
			probeReconciler.SetCanaryExecutor(canaryExecutor)
		}
		probeAdapter = k8sctrl.NewProbeReconcilerAdapter(probeReconciler)
	}
	var invocationTracer invocation.Tracer
	if tracePipeline != nil {
		invocationTracer = invocation.NewObservabilityTracer(tracePipeline.Tracer)
	}
	toolInvoker, err := intctrl.NewToolGatewayInvoker(
		st,
		invocationPolicies,
		runtimeQuota,
		toolDispatcher,
		logger.Named("workflow-tool-invoker"),
		invocationTracer,
		toolgateway.WithLedger(toolgateway.NewStoreLedgerWithAudit(st, auditLog)),
	)
	if err != nil {
		logger.Error("failed to create Tool Gateway", zap.Error(err))
		toolInvoker = intctrl.NewUnavailableToolGatewayInvoker(err)
	}
	workflowAdapter.SetToolInvoker(toolInvoker)
	workflowRunAdapter.SetToolInvoker(toolInvoker)
	workflowAdapter.SetCommunicationPolicyAuthorizer(communicationPolicy)
	workflowRunAdapter.SetCommunicationPolicyAuthorizer(communicationPolicy)
	workflowAdapter.SetAuditLog(auditLog)
	workflowRunAdapter.SetAuditLog(auditLog)
	mutatingHooks, validatingHooks := admission.ProductionHooksWithExecutionAuthority(st, opaEngine, runtimeApprovalDecisionAuthorityFromEnv(), runtimeApprovalExecutionWritersFromEnv(), runtimeApprovalAdmissionUsernamesFromEnv()...)
	validatingHooks = append(validatingHooks, cognamespace.NewProductionCrossNamespaceHook(st))
	memoryProvisioner := intctrl.NewMemoryServiceProvisioner(os.Getenv("KOGNITION_MEMORY_SERVICE_URL"), nil, os.Getenv("MEMORY_SERVICE_INTERNAL_TOKEN"))
	scheduler := intctrl.NewConfiguredScheduler(st, logger.Named("cognitive-scheduler"), schedulerPluginNamesFromEnv())
	modelClassAdapter := k8sctrl.NewModelClassReconcilerAdapter(st, cpiRegistry, logger)
	if kubeClient != nil && auditLog != nil && usageWriter != nil {
		ledger, err := k8scost.NewConfigMapStore(kubeClient, strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE")))
		if err != nil {
			logger.Error("canary accounting unavailable; candidate promotion remains disabled", zap.Error(err))
		} else {
			modelClassAdapter.SetCanaryAccounting(ledger, usageWriter, auditLog)
		}
	}

	reconcilers := []managerReconciler{
		k8sctrl.NewAgentReconcilerAdapter(st, agentDispatcher, logger),
		k8sctrl.NewAgentInstanceReconcilerAdapter(st, agentDispatcher, logger).
			SetScheduler(scheduler).
			SetQuotaEnforcer(runtimeQuota).
			SetAuditLog(auditLog).
			SetTrustRecorder(trustRecorder).
			SetModelAuthorizer(invocationPolicies).
			SetRequireAutomaticIdentity(requiresAutomaticIdentity(agentDispatcher)),
		k8sctrl.NewAgentJobReconcilerAdapter(st, logger),
		k8sctrl.NewAgentPolicyReconcilerAdapter(st, opaEngine, logger),
		k8sctrl.NewCognitiveQuotaReconcilerAdapter(st, logger),
		k8sctrl.NewAgentIdentityReconcilerAdapter(st, logger),
		k8sctrl.NewKnowledgeBaseReconcilerAdapter(st, logger).WithIngesterOptions(ingesterOptions),
		k8sctrl.NewKnowledgeBaseIndexRunReconcilerAdapter(st, logger).WithIngesterOptions(ingesterOptions),
		k8sctrl.NewKnowledgeBaseIndexScheduleReconcilerAdapter(st, logger),
		k8sctrl.NewKnowledgeBaseClaimReconcilerAdapter(st, logger),
		k8sctrl.NewMemoryClaimReconcilerAdapter(st, logger, intctrl.WithMemoryBackendProvisioner(memoryProvisioner)),
		k8sctrl.NewMemoryClassReconcilerAdapter(st, logger),
		k8sctrl.NewMemoryVolumeReconcilerAdapter(st, logger),
		k8sctrl.NewMemoryGrantReconcilerAdapter(st, logger, intctrl.WithMemoryGrantAuditLog(auditLog)),
		k8sctrl.NewMemoryRecordReconcilerAdapter(st, logger),
		k8sctrl.NewMemorySummarizationReconcilerAdapter(st, logger),
		k8sctrl.NewMemoryRecordSummarizationReconcilerAdapter(st, logger),
		modelClassAdapter,
		k8sctrl.NewModelReconcilerAdapter(st, cpiRegistry, logger),
		k8sctrl.NewModelAutoscalerReconcilerAdapter(st, logger),
		k8sctrl.NewAgentAutoscalerReconcilerAdapter(st, logger),
		k8sctrl.NewAutonomyReviewReconcilerAdapter(st, logger).SetAuditLog(auditLog),
		k8sctrl.NewTrustProfileReconcilerAdapter(st, logger).SetAuditLog(auditLog),
		k8sctrl.NewTrustProfileIndexReconciler(kubeClient),
		func() managerReconciler {
			adapter := k8sctrl.NewAgentServiceReconcilerAdapter(st, logger, os.Getenv("KOGNITION_AGENT_NAMESPACE"))
			adapter.SetDispatcher(agentDispatcher)
			return adapter
		}(),
		k8sctrl.NewIntentGatewayReconcilerAdapter(st, logger, intentGatewayServingConfigFromEnv()),
		k8sctrl.NewPromptReconcilerAdapter(st, logger),
		k8sctrl.NewSolutionReconcilerAdapter(st, logger),
		k8sctrl.NewEvaluationReconcilerAdapter(st, logger),
		k8sctrl.NewEvaluationRunReconcilerAdapter(st, logger),
		k8sctrl.NewSecretRefReconcilerAdapter(st, logger),
		k8sctrl.NewToolReconcilerAdapter(st, logger, intctrl.WithExecProbeRunner(k8sctrl.NewKubernetesExecProbeRunner(kubeClient))),
		k8sctrl.NewToolClassReconcilerAdapter(st, logger),
		k8sctrl.NewToolInvocationRunReconcilerAdapter(st, logger, intctrl.WithStdioInvocationRunner(k8sctrl.NewKubernetesStdioInvocationRunner(kubeClient, config))),
		workflowAdapter,
		workflowRunAdapter,
		k8sctrl.NewWorkspaceReconcilerAdapter(st, logger),
	}
	if probeAdapter != nil {
		reconcilers = append(reconcilers, probeAdapter)
	}

	return controllerAssembly{
		reconcilers: reconcilers,
		mutating:    mutatingHooks,
		validating:  validatingHooks,
		pluginHost:  pluginHost,
	}
}

func runtimeApprovalAdmissionUsernamesFromEnv() []string {
	namespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	serviceAccount := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_SERVICE_ACCOUNT"))
	if namespace == "" || serviceAccount == "" {
		return nil
	}
	serviceAccounts := []string{serviceAccount}
	for _, configured := range strings.Split(os.Getenv("KOGNITION_RUNTIME_APPROVAL_WRITER_SERVICE_ACCOUNTS"), ",") {
		if configured = strings.TrimSpace(configured); configured != "" {
			serviceAccounts = append(serviceAccounts, configured)
		}
	}
	usernames := make([]string, 0, len(serviceAccounts))
	for _, serviceAccount := range serviceAccounts {
		usernames = append(usernames, fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount))
	}
	return usernames
}

func runtimeApprovalDecisionAuthorityFromEnv() string {
	namespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	serviceAccount := strings.TrimSpace(os.Getenv("KOGNITION_RUNTIME_APPROVAL_DECISION_SERVICE_ACCOUNT"))
	if namespace == "" || serviceAccount == "" {
		return ""
	}
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, serviceAccount)
}

func runtimeApprovalExecutionWritersFromEnv() map[string]string {
	result := make(map[string]string)
	namespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	if namespace == "" {
		return result
	}
	for _, entry := range []struct{ key, plane string }{
		{"KOGNITION_RUNTIME_APPROVAL_TOOL_SERVICE_ACCOUNT", "Tool"},
		{"KOGNITION_RUNTIME_APPROVAL_MEMORY_SERVICE_ACCOUNT", "Memory"},
		{"KOGNITION_RUNTIME_APPROVAL_COMMUNICATION_SERVICE_ACCOUNT", "Communication"},
	} {
		account := strings.TrimSpace(os.Getenv(entry.key))
		if account == "" {
			continue
		}
		username := fmt.Sprintf("system:serviceaccount:%s:%s", namespace, account)
		if _, duplicate := result[username]; duplicate {
			result[username] = ""
		} else {
			result[username] = entry.plane
		}
	}
	return result
}

func getEnvOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func workflowToolTracingOptions(pipeline *observability.TracePipeline) []intctrl.WorkflowToolInvokerOption {
	if pipeline == nil {
		return nil
	}
	return []intctrl.WorkflowToolInvokerOption{intctrl.WithWorkflowToolTracing(pipeline.Tracer, pipeline.Collector)}
}

func shutdownTracePipeline(pipeline *observability.TracePipeline, logger *zap.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pipeline.Shutdown(shutdownCtx); err != nil {
		logger.Error("OpenTelemetry trace exporter shutdown error", zap.Error(err))
	}
}
