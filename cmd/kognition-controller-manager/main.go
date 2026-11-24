// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	anthropicdrv "github.com/kognition-ai/kognition/internal/cpi/drivers/anthropic"
	bedrockdrv "github.com/kognition-ai/kognition/internal/cpi/drivers/bedrock"
	foundrydrv "github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	ollamadrv "github.com/kognition-ai/kognition/internal/cpi/drivers/ollama"
	openaidrv "github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/ingestion"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	pgstore "github.com/kognition-ai/kognition/internal/store/postgres"
)

func main() {
	logger := newControllerLogger()
	defer func() { _ = logger.Sync() }()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Connect to PostgreSQL.
	dsn := controllerDSN()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		logger.Fatal("failed to connect to database", zap.Error(err))
	}

	st := pgstore.New(pool, logger.Named("store"))
	metricsRegistry := prometheus.NewRegistry()
	observability.NewWorkflowMetrics(metricsRegistry, st)
	usageWriter := observability.NewModelUsageMetrics(metricsRegistry).Wrap(st)
	auditStore := observability.NewTrustDecisionMetrics(metricsRegistry).Wrap(st)
	metricsServer := newControllerMetricsServer(metricsRegistry)
	go func() {
		logger.Info("controller manager metrics starting", zap.String("addr", metricsServer.Addr))
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("controller manager metrics server error", zap.Error(err))
			cancel()
		}
	}()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("controller manager metrics shutdown error", zap.Error(err))
		}
	}()
	if err := controller.EnsureDefaultAutonomyClasses(ctx, st, logger.Named("autonomy-defaults")); err != nil {
		logger.Fatal("failed to ensure default autonomy classes", zap.Error(err))
	}

	// Start the pg_notify listener for watch events.
	if err := st.StartNotifyListener(ctx); err != nil {
		logger.Error("notify listener error", zap.Error(err))
	}

	tracePipeline, err := observability.NewTracePipelineFromEnv(ctx)
	if err != nil {
		logger.Fatal("failed to configure OpenTelemetry tracing", zap.Error(err))
	}
	defer shutdownTracePipeline(tracePipeline, logger)
	if tracePipeline.Enabled() {
		logger.Info("OpenTelemetry trace export enabled", zap.String("endpoint", tracePipeline.Config.Endpoint), zap.String("protocol", string(tracePipeline.Config.Protocol)))
	}
	tracePipeline.Tracer.SetFallibleCostSink(func(record observability.CostRecord) error {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		if record.OperationType == observability.CostOpModelInference {
			_, err = usageWriter.ApplyUsageEvent(writeCtx, record)
		} else {
			err = st.RecordCost(writeCtx, record)
		}
		if err != nil {
			logger.Warn("persist cost record", zap.Error(err), zap.String("workspace", record.Workspace), zap.String("trace_id", record.TraceID), zap.String("span_id", record.SpanID))
		}
		return err
	})

	identityReader, closeIdentityReader, err := runtimeauth.LoadWorkloadIdentityStatusRegistry(ctx, os.Getenv)
	if err != nil {
		logger.Fatal("configure workload identity status reporting", zap.Error(err))
	}
	defer closeIdentityReader()
	mgr := newControllerManagerWithWorkloadIdentity(st, logger, tracePipeline, usageWriter, auditStore, identityReader, workflowToolTracingOptions(tracePipeline)...)
	if err := mgr.Start(ctx); err != nil {
		logger.Fatal("controller manager error", zap.Error(err))
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := st.StopNotifyListener(shutdownCtx); err != nil {
		logger.Error("notify listener shutdown error", zap.Error(err))
		return
	}
	pool.Close()
}

func newControllerLogger() *zap.Logger {
	logger, _ := zap.NewProduction()
	return logger
}

func controllerDSN() string {
	if dsn := os.Getenv("KOGNITION_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://kognition:kognition@localhost:5432/kognition?sslmode=disable"
}

func workflowToolTracingOptions(pipeline *observability.TracePipeline) []controller.WorkflowToolInvokerOption {
	if pipeline == nil {
		return nil
	}
	return []controller.WorkflowToolInvokerOption{controller.WithWorkflowToolTracing(pipeline.Tracer, pipeline.Collector)}
}

func shutdownTracePipeline(pipeline *observability.TracePipeline, logger *zap.Logger) {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pipeline.Shutdown(shutdownCtx); err != nil {
		logger.Error("OpenTelemetry trace exporter shutdown error", zap.Error(err))
	}
}

func newControllerManager(st store.Store, logger *zap.Logger, tracePipeline *observability.TracePipeline, usageWriter observability.UsageEventWriter, auditStore observability.AuditStore, workflowToolOptions ...controller.WorkflowToolInvokerOption) *controller.Manager {
	return newControllerManagerWithWorkloadIdentity(st, logger, tracePipeline, usageWriter, auditStore, nil, workflowToolOptions...)
}

func newControllerManagerWithWorkloadIdentity(st store.Store, logger *zap.Logger, tracePipeline *observability.TracePipeline, usageWriter observability.UsageEventWriter, auditStore observability.AuditStore, reader *runtimeauth.WorkloadIdentityStatusRegistry, workflowToolOptions ...controller.WorkflowToolInvokerOption) *controller.Manager {
	policyEngine := opa.NewEngine()
	agentPolicyReconciler := controller.NewAgentPolicyReconciler(st, policyEngine, logger.Named("agent-policy-reconciler"))
	quotaReconciler := controller.NewCognitiveQuotaReconciler(st, logger.Named("runtime-quota"))
	runtimeQuota := controller.NewQuotaEnforcer(st, quotaReconciler, logger.Named("runtime-quota"))
	trustProfileReconciler := controller.NewTrustProfileReconciler(st, logger.Named("trust-profile-reconciler"))
	if auditStore != nil {
		trustProfileReconciler.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	}
	// Create the controller manager with leader election.
	mgr := controller.NewManager(controller.ManagerConfig{
		Store:  st,
		Logger: logger.Named("controller-manager"),
		LeaderElection: &controller.LeaderElectionConfig{
			LeaseName:     "controller-manager",
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   2 * time.Second,
		},
	})
	if err := configureCanaryOperationRetention(mgr, st, logger, os.Getenv("KOGNITION_CANARY_OPERATION_RETENTION")); err != nil {
		logger.Fatal("configure canary operation retention", zap.Error(err))
	}
	if lifecycle, ok := st.(runtimeapproval.ExpiredToolPreparationStore); ok {
		mgr.AddRunnable(controller.NewToolPreparationReaperRunnable(lifecycle, logger.Named("tool-preparation-reaper"), controller.DefaultToolPreparationReapInterval, controller.DefaultToolPreparationReapLimit))
	}
	if storage, ok := st.(store.RuntimeRecoveryStore); ok {
		mgr.AddRunnable(&recovery.Retention{Storage: storage})
	}

	var kbIngester *ingestion.HTTPLauncher
	if endpoint := os.Getenv("KOGNITION_KB_INGESTER_URL"); endpoint != "" {
		kbIngester = ingestion.NewHTTPLauncher(st, endpoint, nil, logger).
			WithBearerToken(os.Getenv("KOGNITION_KB_INGESTER_AUTH_TOKEN"))
		logger.Info("standalone knowledge-base ingestion enabled", zap.String("endpoint", endpoint), zap.Bool("authenticated", strings.TrimSpace(os.Getenv("KOGNITION_KB_INGESTER_AUTH_TOKEN")) != ""))
	}
	// Register the AgentIdentity reconciler before the Agent dependency watch.
	agentIdentityReconciler := controller.NewAgentIdentityReconciler(st, logger.Named("agent-identity-reconciler"))
	if reader != nil {
		agentIdentityReconciler = controller.NewAgentIdentityReconcilerWithWorkloadIdentity(st, logger.Named("agent-identity-reconciler"), reader)
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "agent-identity-controller",
		Kind:       "AgentIdentity",
		Reconciler: agentIdentityReconciler,
		Workers:    2,
	})

	// Register the Agent reconciler.
	agentReconciler := controller.NewAgentReconciler(st, logger.Named("agent-reconciler"))
	agentController := mgr.Register(controller.ControllerConfig{
		Name:       "agent-controller",
		Kind:       "Agent",
		Reconciler: agentReconciler,
		Workers:    2,
	})
	if err := mgr.AddEventHandler("AgentIdentity", controller.NewAgentIdentityDependencyHandler(st, agentController, logger)); err != nil {
		panic(err)
	}

	// Register the AgentInstance reconciler.
	registry := cpi.NewRegistry()
	registry.MustRegister(openaidrv.New(nil))
	registry.MustRegister(foundrydrv.New(nil))
	registry.MustRegister(anthropicdrv.New(nil))
	registry.MustRegister(ollamadrv.New(nil))
	registry.MustRegister(bedrockdrv.New(nil))

	cpiDispatcher := controller.NewCPIDispatcher(st, registry, logger.Named("cpi-dispatcher"))
	if usageWriter != nil {
		cpiDispatcher.SetUsageEventWriter(usageWriter)
	}
	if auditStore != nil {
		cpiDispatcher.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	} else {
		logger.Warn("standalone Model audit is unavailable because the store has no audit backend")
	}
	standaloneModelPolicies := controller.NewInvocationPolicyEngine(
		controller.NewToolGrantEngine(st, logger.Named("standalone-model-grant")),
		agentPolicyReconciler,
	)
	var standaloneModelCore *modelgateway.Core
	if st != nil {
		core, coreErr := modelgateway.NewCore(st, standaloneModelPolicies)
		if coreErr != nil {
			logger.Fatal("create standalone Model Gateway core", zap.Error(coreErr))
		}
		standaloneModelCore = core
		cpiDispatcher.SetModelGatewayCore(standaloneModelCore)
	}
	runtimeDispatcher, err := standaloneRuntimeDispatcher(st, logger, cpiDispatcher)
	if err != nil {
		logger.Fatal("configure standalone runtime dispatcher", zap.Error(err))
	}
	instanceReconciler := controller.NewAgentInstanceReconcilerWithDispatcher(st, runtimeDispatcher, logger.Named("agent-instance-reconciler"))
	instanceReconciler.SetRequireAutomaticIdentity(true)
	instanceReconciler.SetQuotaEnforcer(runtimeQuota)
	if auditStore != nil {
		instanceReconciler.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	}
	instanceReconciler.SetTrustRecorder(trustProfileReconciler)
	instanceReconciler.SetModelAuthorizer(standaloneModelPolicies)
	mgr.Register(controller.ControllerConfig{
		Name:       "agent-instance-controller",
		Kind:       "AgentInstance",
		Reconciler: instanceReconciler,
		Workers:    2,
	})
	probeReconciler := controller.NewProbeReconciler(st, runtimeDispatcher, logger.Named("probe-reconciler"))
	if st != nil {
		canaryExecutor, err := controller.NewCPICanaryExecutor(st, registry, standaloneModelCore)
		if err != nil {
			logger.Fatal("create reasoning canary executor", zap.Error(err))
		}
		if usageWriter != nil {
			canaryExecutor.SetUsageEventWriter(usageWriter)
		}
		if tracePipeline.Enabled() {
			canaryExecutor.SetTracing(tracePipeline.Tracer, tracePipeline.Collector)
		}
		probeReconciler.SetCanaryExecutor(canaryExecutor)
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "agent-instance-probe-controller",
		Kind:       "AgentInstance",
		Reconciler: probeReconciler,
		Workers:    2,
	})

	// Register the AgentJob reconciler.
	jobReconciler := controller.NewAgentJobReconciler(st, logger.Named("agent-job-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "agent-job-controller",
		Kind:       "AgentJob",
		Reconciler: jobReconciler,
		Workers:    2,
	})

	// Register the Workflow reconciler.
	workflowReconciler := controller.NewWorkflowReconciler(st, logger.Named("workflow-reconciler"))
	communicationPolicy := controller.NewCommunicationPolicyEngine(st, logger.Named("workflow-communication-policy"))
	workflowReconciler.SetCommunicationPolicyAuthorizer(communicationPolicy)
	if auditStore != nil {
		workflowReconciler.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	}
	if tracePipeline.Enabled() {
		workflowReconciler.SetTracing(tracePipeline.Tracer, tracePipeline.Collector)
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "workflow-controller",
		Kind:       "Workflow",
		Reconciler: workflowReconciler,
		Workers:    2,
	})

	// Register the WorkflowRun reconciler.
	workflowRunReconciler := controller.NewWorkflowRunReconciler(st, logger.Named("workflow-run-reconciler"))
	workflowRunReconciler.SetCommunicationPolicyAuthorizer(communicationPolicy)
	if auditStore != nil {
		workflowRunReconciler.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	}
	if tracePipeline.Enabled() {
		workflowRunReconciler.SetTracing(tracePipeline.Tracer, tracePipeline.Collector)
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "workflow-run-controller",
		Kind:       "WorkflowRun",
		Reconciler: workflowRunReconciler,
		Workers:    2,
	})

	evaluationReconciler := controller.NewEvaluationReconciler(st, logger.Named("evaluation-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "evaluation-controller",
		Kind:       "Evaluation",
		Reconciler: evaluationReconciler,
		Workers:    2,
	})

	evaluationRunReconciler := controller.NewEvaluationRunReconciler(st, logger.Named("evaluation-run-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "evaluation-run-controller",
		Kind:       "EvaluationRun",
		Reconciler: evaluationRunReconciler,
		Workers:    2,
	})

	// Wire the workflow tool invoker. The protocol-multiplexer executor handles
	// OpenAPI (HTTPS) and MCP (JSON-RPC over HTTP) tools; other protocols return
	// a structured unsupported-protocol error.
	toolGrants := controller.NewToolGrantEngine(st, logger.Named("tool-grant-engine"))
	invocationPolicies := controller.NewInvocationPolicyEngine(toolGrants, agentPolicyReconciler).
		WithAgentRBAC(st, controller.NewRBACEngine(st, logger.Named("invocation-rbac")))
	toolExecutor := toolexec.New(nil)
	toolDispatcher := runtime.NewToolDispatcher(toolExecutor, toolExecutor)
	var invocationTracer invocation.Tracer
	if tracePipeline != nil {
		invocationTracer = invocation.NewObservabilityTracer(tracePipeline.Tracer)
	}
	toolInvoker, err := controller.NewToolGatewayInvoker(
		st,
		invocationPolicies,
		runtimeQuota,
		toolDispatcher,
		logger.Named("workflow-tool-invoker"),
		invocationTracer,
	)
	if err != nil {
		logger.Error("failed to create Tool Gateway", zap.Error(err))
		toolInvoker = controller.NewUnavailableToolGatewayInvoker(err)
	}
	workflowReconciler.SetToolInvoker(toolInvoker)
	workflowRunReconciler.SetToolInvoker(toolInvoker)

	// Register the Prompt reconciler.
	promptReconciler := controller.NewPromptReconciler(st, logger.Named("prompt-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "prompt-controller",
		Kind:       "Prompt",
		Reconciler: promptReconciler,
		Workers:    2,
	})

	// Register the MemoryClaim reconciler.
	memoryProvisioner := controller.NewMemoryServiceProvisioner(os.Getenv("KOGNITION_MEMORY_SERVICE_URL"), nil, os.Getenv("MEMORY_SERVICE_INTERNAL_TOKEN"))
	memoryClaimReconciler := controller.NewMemoryClaimReconciler(
		st,
		logger.Named("memory-claim-reconciler"),
		controller.WithMemoryBackendProvisioner(memoryProvisioner),
	)
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-claim-controller",
		Kind:       "MemoryClaim",
		Reconciler: memoryClaimReconciler,
		Workers:    2,
	})

	memoryClassReconciler := controller.NewMemoryClassReconciler(st, logger.Named("memory-class-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-class-controller",
		Kind:       "MemoryClass",
		Reconciler: memoryClassReconciler,
		Workers:    1,
	})

	memoryVolumeReconciler := controller.NewMemoryVolumeReconciler(st, logger.Named("memory-volume-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-volume-controller",
		Kind:       "MemoryVolume",
		Reconciler: memoryVolumeReconciler,
		Workers:    2,
	})

	// Register the MemoryGrant reconciler.
	var memoryGrantOptions []controller.MemoryGrantReconcilerOption
	if auditStore != nil {
		memoryGrantOptions = append(memoryGrantOptions, controller.WithMemoryGrantAuditLog(observability.NewCognitiveAuditLog(auditStore)))
	} else {
		logger.Warn("MemoryGrant lifecycle audit is unavailable because the store has no audit backend")
	}
	memoryGrantReconciler := controller.NewMemoryGrantReconciler(st, logger.Named("memory-grant-reconciler"), memoryGrantOptions...)
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-grant-controller",
		Kind:       "MemoryGrant",
		Reconciler: memoryGrantReconciler,
		Workers:    2,
	})

	// Register the KnowledgeBase reconciler.
	kbReconciler := controller.NewKnowledgeBaseReconciler(st, logger.Named("knowledge-base-reconciler"))
	if kbIngester != nil {
		kbReconciler.WithIngesterLauncher(kbIngester)
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "knowledge-base-controller",
		Kind:       "KnowledgeBase",
		Reconciler: kbReconciler,
		Workers:    2,
	})

	// Register the KnowledgeBaseClaim reconciler.
	kbClaimReconciler := controller.NewKnowledgeBaseClaimReconciler(st, logger.Named("knowledge-base-claim-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "knowledge-base-claim-controller",
		Kind:       "KnowledgeBaseClaim",
		Reconciler: kbClaimReconciler,
		Workers:    2,
	})

	kbIndexRunReconciler := controller.NewKnowledgeBaseIndexRunReconciler(st, logger.Named("knowledge-base-index-run-reconciler"))
	if kbIngester != nil {
		kbIndexRunReconciler.WithIndexRunLauncher(kbIngester)
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "knowledge-base-index-run-controller",
		Kind:       "KnowledgeBaseIndexRun",
		Reconciler: kbIndexRunReconciler,
		Workers:    2,
	})

	kbIndexScheduleReconciler := controller.NewKnowledgeBaseIndexScheduleReconciler(st, logger.Named("knowledge-base-index-schedule-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "knowledge-base-index-schedule-controller",
		Kind:       "KnowledgeBaseIndexSchedule",
		Reconciler: kbIndexScheduleReconciler,
		Workers:    2,
	})

	// Register the Tool reconciler.
	execProbeRunner, err := controller.NewDockerExecProbeRunner()
	if err != nil {
		logger.Fatal("failed to configure Docker exec probe runner", zap.Error(err))
	}
	stdioInvocationRunner, err := controller.NewDockerStdioInvocationRunner()
	if err != nil {
		logger.Fatal("failed to configure Docker stdio invocation runner", zap.Error(err))
	}
	toolReconciler := controller.NewToolReconciler(st, logger.Named("tool-reconciler"), controller.WithExecProbeRunner(execProbeRunner))
	mgr.Register(controller.ControllerConfig{
		Name:       "tool-controller",
		Kind:       "Tool",
		Reconciler: toolReconciler,
		Workers:    2,
	})
	mgr.Register(controller.ControllerConfig{
		Name:       "tool-class-controller",
		Kind:       "ToolClass",
		Reconciler: toolReconciler,
		Workers:    1,
	})
	mgr.Register(controller.ControllerConfig{
		Name:       "tool-invocation-run-controller",
		Kind:       "ToolInvocationRun",
		Reconciler: controller.NewToolInvocationRunReconciler(st, logger.Named("tool-invocation-run-reconciler"), controller.WithStdioInvocationRunner(stdioInvocationRunner)),
		Workers:    2,
	})

	// Register the Model reconciler.
	modelReconciler := controller.NewModelReconciler(st, registry, logger.Named("model-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "model-controller",
		Kind:       "Model",
		Reconciler: modelReconciler,
		Workers:    2,
	})

	// Register the ModelClass reconciler.
	modelClassReconciler := controller.NewModelClassReconciler(st, registry, logger.Named("model-class-reconciler"))
	if ledger, supported := st.(store.ModelDispatchStore); supported && auditStore != nil {
		modelClassReconciler.SetCanaryAccounting(ledger, usageWriter, observability.NewCognitiveAuditLog(auditStore))
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "model-class-controller",
		Kind:       "ModelClass",
		Reconciler: modelClassReconciler,
		Workers:    1,
	})

	// Register the MemoryRecord reconciler.
	memoryRecordReconciler := controller.NewMemoryRecordReconciler(st, logger.Named("memory-record-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-record-controller",
		Kind:       "MemoryRecord",
		Reconciler: memoryRecordReconciler,
		Workers:    2,
	})

	// Register the MemorySummarization reconciler.
	memorySummarizationReconciler := controller.NewMemorySummarizationReconciler(st, logger.Named("memory-summarization-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-summarization-controller",
		Kind:       "Memory",
		Reconciler: memorySummarizationReconciler,
		Workers:    2,
	})

	// Also trigger summarization when MemoryRecord resources change.
	mgr.Register(controller.ControllerConfig{
		Name:       "memory-summarization-from-record-controller",
		Kind:       "MemoryRecord",
		Reconciler: memorySummarizationReconciler,
		Workers:    2,
	})

	autoscalerReconciler := controller.NewAgentAutoscalerReconciler(st, logger.Named("agent-autoscaler-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "agent-autoscaler-controller",
		Kind:       "AgentAutoscaler",
		Reconciler: autoscalerReconciler,
		Workers:    2,
	})

	modelAutoscalerReconciler := controller.NewModelAutoscalerReconciler(st, logger.Named("model-autoscaler-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "model-autoscaler-controller",
		Kind:       "ModelAutoscaler",
		Reconciler: modelAutoscalerReconciler,
		Workers:    2,
	})

	agentServiceReconciler := controller.NewAgentServiceReconciler(st, logger.Named("agent-service-reconciler"))
	agentServiceController := mgr.Register(controller.ControllerConfig{
		Name:       "agent-service-controller",
		Kind:       "AgentService",
		Reconciler: agentServiceReconciler,
		Workers:    2,
	})
	if err := mgr.AddEventHandler("Agent", controller.NewAgentServiceDependencyHandler(st, "Agent", agentServiceController, logger)); err != nil {
		panic(err)
	}
	if err := mgr.AddEventHandler("AgentInstance", controller.NewAgentServiceDependencyHandler(st, "AgentInstance", agentServiceController, logger)); err != nil {
		panic(err)
	}

	// Register the IntentGateway reconciler.
	intentGatewayReconciler := controller.NewIntentGatewayReconciler(st, logger.Named("intent-gateway-reconciler"))
	intentGatewayController := mgr.Register(controller.ControllerConfig{
		Name:       "intent-gateway-controller",
		Kind:       "IntentGateway",
		Reconciler: intentGatewayReconciler,
		Workers:    2,
	})
	if err := mgr.AddEventHandler("Agent", controller.NewIntentGatewayDependencyHandler(st, "Agent", intentGatewayController, logger)); err != nil {
		panic(err)
	}
	if err := mgr.AddEventHandler("Workflow", controller.NewIntentGatewayDependencyHandler(st, "Workflow", intentGatewayController, logger)); err != nil {
		panic(err)
	}
	if err := mgr.AddEventHandler("AgentService", controller.NewIntentGatewayDependencyHandler(st, "AgentService", intentGatewayController, logger)); err != nil {
		panic(err)
	}

	solutionReconciler := controller.NewSolutionReconciler(st, logger.Named("solution-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "solution-controller",
		Kind:       "Solution",
		Reconciler: solutionReconciler,
		Workers:    2,
	})

	secretRefReconciler := controller.NewSecretRefReconciler(st, logger.Named("secret-ref-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "secret-ref-controller",
		Kind:       "SecretRef",
		Reconciler: secretRefReconciler,
		Workers:    2,
	})

	// Register the AgentPolicy reconciler.
	mgr.Register(controller.ControllerConfig{
		Name:       "agent-policy-controller",
		Kind:       "AgentPolicy",
		Reconciler: agentPolicyReconciler,
		Workers:    2,
	})

	// Register the CognitiveQuota reconciler.
	mgr.Register(controller.ControllerConfig{
		Name:       "cognitive-quota-controller",
		Kind:       "CognitiveQuota",
		Reconciler: quotaReconciler,
		Workers:    2,
	})

	// Register the TrustProfile reconciler.
	mgr.Register(controller.ControllerConfig{
		Name:       "trust-profile-controller",
		Kind:       "TrustProfile",
		Reconciler: trustProfileReconciler,
		Workers:    2,
	})

	// Register the AutonomyReview reconciler.
	autonomyReviewReconciler := controller.NewAutonomyReviewReconciler(st, logger.Named("autonomy-review-reconciler"))
	if auditStore != nil {
		autonomyReviewReconciler.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	}
	mgr.Register(controller.ControllerConfig{
		Name:       "autonomy-review-controller",
		Kind:       "AutonomyReview",
		Reconciler: autonomyReviewReconciler,
		Workers:    2,
	})

	// Register the Workspace reconciler.
	workspaceReconciler := controller.NewWorkspaceReconciler(st, logger.Named("workspace-reconciler"))
	mgr.Register(controller.ControllerConfig{
		Name:       "workspace-controller",
		Kind:       "Workspace",
		Reconciler: workspaceReconciler,
		Workers:    2,
	})
	if costStore, ok := st.(controller.CostRetentionStore); ok {
		mgr.Register(controller.ControllerConfig{
			Name:       "cost-retention-controller",
			Kind:       "Workspace",
			Reconciler: controller.NewCostRetentionReconciler(st, costStore, logger),
			Workers:    1,
		})
	}

	return mgr
}

func configureCanaryOperationRetention(mgr *controller.Manager, st store.Store, logger *zap.Logger, value string) error {
	retention, enabled, err := controller.CanaryOperationRetention(value)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	operations, ok := st.(store.CanaryOperationStore)
	if !ok {
		logger.Info("canary operation retention unavailable for configured store")
		return nil
	}
	mgr.AddRunnable(controller.NewCanaryOperationRetentionRunnable(operations, logger, retention, controller.DefaultCanaryCompactionInterval))
	return nil
}
