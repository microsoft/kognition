// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/mcp"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/store"
)

const toolKind = "Tool"
const toolClassKind = "ToolClass"
const toolFinalizer = "kognition.dev/tool-cleanup"

// conditionToolsReady is the agent condition type set when required Tool refs
// are checked for readiness.
const conditionToolsReady lifecycle.ConditionType = "ToolsReady"

// Annotation keys for tracking consecutive probe results.
const (
	annotationProbeFailures  = "kognition.dev/probe-failures"
	annotationProbeSuccesses = "kognition.dev/probe-successes"
)

// Default probe thresholds.
const (
	defaultProbeFailureThreshold int32 = 3
	defaultProbeSuccessThreshold int32 = 1
	defaultProbeInterval               = 30 * time.Second
	defaultProbeTimeout                = 5 * time.Second
)

type toolProbeClient interface {
	Do(*http.Request) (*http.Response, error)
}

type toolProbeDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type toolMCPClient interface {
	Call(context.Context, string, string, any, any) error
}

// ExecProbeRunner executes health checks inside a provisioned tool sandbox.
// Implementations must execute command directly as argv without shell expansion.
type ExecProbeRunner interface {
	RunExecProbe(context.Context, *v1alpha1.Tool, ExecProbeTarget, []string) error
}

// ExecProbeTarget is the trusted one-shot OCI sandbox resolved from a
// ToolClass. Tool authors cannot select or override its image.
type ExecProbeTarget struct {
	Image    string
	CPUMilli int64
	MemoryMB int64
	Timeout  time.Duration
}

// ToolReconcilerOption configures optional Tool reconciler integrations.
type ToolReconcilerOption func(*ToolReconciler)

// WithExecProbeRunner configures sandbox-local execution for exec health probes.
func WithExecProbeRunner(runner ExecProbeRunner) ToolReconcilerOption {
	return func(reconciler *ToolReconciler) {
		reconciler.execRunner = runner
	}
}

// WithToolClock overrides the clock used to schedule health probes.
func WithToolClock(now func() time.Time) ToolReconcilerOption {
	return func(reconciler *ToolReconciler) {
		if now != nil {
			reconciler.now = now
		}
	}
}

// ToolReconciler reconciles Tool resources.
//
// It validates endpoint health, adapts OpenAPI tools to MCP status, and
// enforces optional digest pinning for reproducible tool behavior.
// It implements the full lifecycle state machine:
// Registered → Validating → Ready/Failed/Degraded, and handles deletion
// via finalizer semantics (Terminated phase).
type ToolReconciler struct {
	store       store.Store
	logger      *zap.Logger
	probeClient toolProbeClient
	probeDialer toolProbeDialer
	mcpClient   toolMCPClient
	execRunner  ExecProbeRunner
	now         func() time.Time
}

// NewToolReconciler creates a new ToolReconciler.
func NewToolReconciler(s store.Store, logger *zap.Logger, options ...ToolReconcilerOption) *ToolReconciler {
	reconciler := &ToolReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "tool")),
		now:    time.Now,
	}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

// Reconcile implements the Reconciler interface for Tool resources.
func (r *ToolReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	// Dispatch on Kind — adapters for both Tool and ToolClass share this reconciler.
	if req.Kind == toolClassKind {
		return r.reconcileToolClass(ctx, req)
	}

	raw, err := r.store.Get(ctx, toolKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get tool: %w", err)
	}

	var tool v1alpha1.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		return Result{}, fmt.Errorf("unmarshal tool: %w", err)
	}

	// Handle deletion.
	if tool.ObjectMeta.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, &tool)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(tool.ObjectMeta.Finalizers, toolFinalizer) {
		tool.ObjectMeta.Finalizers, _ = lifecycle.AddFinalizer(tool.ObjectMeta.Finalizers, toolFinalizer)
		tool.Status.Phase = v1alpha1.PhaseRegistered
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(tool.Status.Conditions),
			lifecycle.ConditionProgressing,
			lifecycle.ConditionTrue,
			"Registered",
			"tool registered, awaiting validation",
		)
		tool.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateTool(ctx, &tool); err != nil {
			return Result{}, fmt.Errorf("update tool: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Early return for stable terminal states — avoid re-evaluation loop.
	// Re-evaluate if the spec changed (different digest) or a health probe is configured.
	if tool.Status.Phase == v1alpha1.PhaseReady || tool.Status.Phase == v1alpha1.PhaseFailed {
		if tool.Spec.HealthProbe == nil && computeObservedSpecDigest(tool.Spec) == tool.Status.ObservedSpecDigest {
			return Result{}, nil
		}
	}
	if delay, waiting := r.nextProbeDelay(&tool); waiting {
		return Result{RequeueAfter: delay}, nil
	}

	previousPhase := tool.Status.Phase
	previousAvailable := tool.Status.Available

	// Transition to Validating.
	tool.Status.Phase = v1alpha1.PhaseValidating

	available, probeExecuted, resolvedProtocol, observedDigest, reason, message := r.evaluate(ctx, &tool)
	tool.Status.Available = available
	tool.Status.ResolvedProtocol = resolvedProtocol
	tool.Status.ObservedDigest = observedDigest
	tool.Status.ObservedSpecDigest = computeObservedSpecDigest(tool.Spec)
	tool.Status.LastHealthCheck = r.now().UTC().Format(time.RFC3339)
	tool.Status.CompiledMCP = nil

	readyStatus := lifecycle.ConditionFalse
	if available {
		tool.Status.Phase = v1alpha1.PhaseReady
		readyStatus = lifecycle.ConditionTrue

		// Protocol compilation: produce MCP tool definition for ready tools.
		compiled, compileErr := mcp.Compile(&tool)
		if compileErr != nil {
			r.logger.Warn("mcp compilation failed", zap.Error(compileErr), requestid.ZapField(ctx))
		} else {
			tool.Status.CompiledMCP = compiled
		}
	} else {
		tool.Status.Phase = v1alpha1.PhaseFailed
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(tool.Status.Conditions),
		lifecycle.ConditionReady,
		readyStatus,
		reason,
		message,
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, "ValidationComplete", "tool validation finished")
	tool.Status.Conditions = lifecycleToAPIConditions(lc)

	// Health probe threshold evaluation (may override phase to Degraded).
	var requeueAfter time.Duration
	if tool.Spec.HealthProbe != nil {
		requeueAfter = probeInterval(tool.Spec.HealthProbe)
		if probeExecuted {
			r.applyHealthProbe(&tool, available, previousPhase, previousAvailable)
		}
	}

	if err := r.updateTool(ctx, &tool); err != nil {
		return Result{}, fmt.Errorf("update tool: %w", err)
	}

	// Update ToolsReady condition on agents that reference this tool as required.
	if err := r.updateAgentToolConditions(ctx, &tool); err != nil {
		r.logger.Warn("failed to update agent tool conditions", zap.Error(err), requestid.ZapField(ctx))
	}

	if requeueAfter > 0 {
		return Result{RequeueAfter: requeueAfter}, nil
	}
	return Result{}, nil
}

func (r *ToolReconciler) reconcileDelete(ctx context.Context, tool *v1alpha1.Tool) (Result, error) {
	tool.Status.Phase = v1alpha1.PhaseTerminated
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(tool.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"Terminated",
		"tool is being deleted",
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, "Terminated", "tool is being deleted")
	tool.Status.Conditions = lifecycleToAPIConditions(lc)
	tool.ObjectMeta.Finalizers, _ = lifecycle.RemoveFinalizer(tool.ObjectMeta.Finalizers, toolFinalizer)

	if err := r.updateTool(ctx, tool); err != nil {
		return Result{}, fmt.Errorf("update tool: %w", err)
	}
	return Result{}, nil
}

func (r *ToolReconciler) evaluate(ctx context.Context, tool *v1alpha1.Tool) (bool, bool, v1alpha1.ToolProtocol, string, string, string) {
	resolvedProtocol := tool.Spec.EffectiveProtocol()
	if resolvedProtocol == v1alpha1.ToolProtocolShell {
		if _, err := resolveClassStdioInvocationTarget(ctx, r.store, tool); err == nil {
			observedDigest := computeObservedDigest(resolvedProtocol, tool.Spec.Endpoint, tool.Spec.EffectiveSchema())
			return true, false, resolvedProtocol, observedDigest, "StdioSandboxReady", "tool class supplies a class-owned stdio sandbox"
		}
		observedDigest := computeObservedDigest(resolvedProtocol, tool.Spec.Endpoint, tool.Spec.EffectiveSchema())
		return false, false, resolvedProtocol, observedDigest, "ExecutorUnavailable", fmt.Sprintf("tool protocol %q has no production runtime executor", resolvedProtocol)
	}
	if resolvedProtocol == v1alpha1.ToolProtocolOpenAPI {
		resolvedProtocol = v1alpha1.ToolProtocolMCP
		if len(tool.Spec.EffectiveSchema()) == 0 {
			return false, false, resolvedProtocol, "", "OpenAPISchemaMissing", "OpenAPI tools require spec.schema for MCP adaptation"
		}
	}

	if tool.Spec.Endpoint == "" {
		return false, false, resolvedProtocol, "", "EndpointMissing", "tool endpoint is required for health checks"
	}
	parsed, err := url.Parse(tool.Spec.Endpoint)
	if err != nil || parsed.Scheme == "" {
		return false, false, resolvedProtocol, "", "EndpointInvalid", "tool endpoint must be a valid URI"
	}

	observedDigest := computeObservedDigest(tool.Spec.EffectiveProtocol(), tool.Spec.Endpoint, tool.Spec.EffectiveSchema())
	if tool.Spec.DigestPin != "" && tool.Spec.DigestPin != observedDigest {
		return false, false, resolvedProtocol, observedDigest, "DigestMismatch", fmt.Sprintf("digest pin %q did not match observed digest %q", tool.Spec.DigestPin, observedDigest)
	}
	if tool.Spec.HealthProbe != nil && tool.Spec.HealthProbe.Type == v1alpha1.ProbeTypeHTTPGet {
		if err := r.probeHTTP(ctx, tool, tool.Spec.HealthProbe.Timeout); err != nil {
			return false, true, resolvedProtocol, observedDigest, "HealthProbeFailed", err.Error()
		}
		return true, true, resolvedProtocol, observedDigest, "Healthy", "tool endpoint passed health checks"
	}
	if tool.Spec.HealthProbe != nil && tool.Spec.HealthProbe.Type == v1alpha1.ProbeTypeTCP {
		if err := r.probeTCP(ctx, tool.Spec.Endpoint, tool.Spec.HealthProbe.Timeout, toolAllowsPrivateNetwork(tool)); err != nil {
			return false, true, resolvedProtocol, observedDigest, "HealthProbeFailed", err.Error()
		}
		return true, true, resolvedProtocol, observedDigest, "Healthy", "tool endpoint passed health checks"
	}
	if tool.Spec.HealthProbe != nil && tool.Spec.HealthProbe.Type == v1alpha1.ProbeTypeMCPPing {
		if err := r.probeMCP(ctx, tool, tool.Spec.HealthProbe.Timeout); err != nil {
			return false, true, resolvedProtocol, observedDigest, "HealthProbeFailed", err.Error()
		}
		return true, true, resolvedProtocol, observedDigest, "Healthy", "tool endpoint passed health checks"
	}
	if tool.Spec.HealthProbe != nil && tool.Spec.HealthProbe.Type == v1alpha1.ProbeTypeExec {
		target, err := r.resolveExecProbeTarget(ctx, tool)
		if err != nil {
			return false, true, resolvedProtocol, observedDigest, "ExecProbeTargetUnavailable", err.Error()
		}
		if err := r.probeExec(ctx, tool, tool.Spec.HealthProbe, target); err != nil {
			return false, true, resolvedProtocol, observedDigest, "HealthProbeFailed", err.Error()
		}
		return true, true, resolvedProtocol, observedDigest, "Healthy", "tool sandbox passed health checks"
	}

	if tool.Spec.EffectiveProtocol() == v1alpha1.ToolProtocolOpenAPI {
		return true, false, resolvedProtocol, observedDigest, "AdaptedToMCP", "OpenAPI schema adapted to MCP contract"
	}
	return true, false, resolvedProtocol, observedDigest, "Validated", "tool endpoint configuration is valid"
}

func (r *ToolReconciler) probeHTTP(ctx context.Context, tool *v1alpha1.Tool, configuredTimeout string) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout(configuredTimeout))
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, tool.Spec.Endpoint, nil)
	if err != nil {
		return fmt.Errorf("create HTTP health probe: %w", err)
	}
	var client toolProbeClient
	if r.probeClient == nil {
		secured, err := toolexec.SecureToolHTTPClient(probeCtx, http.DefaultClient, tool)
		if err != nil {
			return fmt.Errorf("HTTP health probe endpoint denied: %w", err)
		}
		client = secured
	} else {
		if err := toolexec.ValidateToolEndpoint(probeCtx, tool.Spec.Endpoint, toolAllowsPrivateNetwork(tool)); err != nil {
			return fmt.Errorf("HTTP health probe endpoint denied: %w", err)
		}
		client = r.probeClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP health probe failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("HTTP health probe returned status %d", resp.StatusCode)
	}
	return nil
}

func (r *ToolReconciler) probeExec(ctx context.Context, tool *v1alpha1.Tool, probe *v1alpha1.ToolHealthProbe, target ExecProbeTarget) error {
	if r.execRunner == nil {
		return fmt.Errorf("exec health probe unavailable: no sandbox runner is configured")
	}

	probeCtx, cancel := context.WithTimeout(ctx, target.Timeout)
	defer cancel()

	if err := r.execRunner.RunExecProbe(probeCtx, tool, target, append([]string(nil), probe.Command...)); err != nil {
		return fmt.Errorf("exec health probe failed: %w", err)
	}
	return nil
}

func (r *ToolReconciler) resolveExecProbeTarget(ctx context.Context, tool *v1alpha1.Tool) (ExecProbeTarget, error) {
	if strings.TrimSpace(tool.Spec.ClassName) == "" {
		return ExecProbeTarget{}, fmt.Errorf("exec health probe requires tool.spec.className")
	}
	raw, err := r.store.Get(ctx, toolClassKind, "", tool.Spec.ClassName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ExecProbeTarget{}, fmt.Errorf("exec health probe ToolClass %q was not found", tool.Spec.ClassName)
		}
		return ExecProbeTarget{}, fmt.Errorf("get exec health probe ToolClass %q: %w", tool.Spec.ClassName, err)
	}
	var class v1alpha1.ToolClass
	if err := json.Unmarshal(raw, &class); err != nil {
		return ExecProbeTarget{}, fmt.Errorf("unmarshal exec health probe ToolClass %q: %w", tool.Spec.ClassName, err)
	}
	if class.Status.Phase != v1alpha1.PhaseReady {
		return ExecProbeTarget{}, fmt.Errorf("exec health probe ToolClass %q is not Ready", tool.Spec.ClassName)
	}
	if class.Spec.ExecutionTier != v1alpha1.ExecutionTierPod {
		return ExecProbeTarget{}, fmt.Errorf("exec health probe ToolClass %q must use executionTier pod", tool.Spec.ClassName)
	}
	image := strings.TrimSpace(class.Spec.SandboxImage)
	if image == "" {
		return ExecProbeTarget{}, fmt.Errorf("exec health probe ToolClass %q must define sandboxImage", tool.Spec.ClassName)
	}

	target := ExecProbeTarget{
		Image:    image,
		CPUMilli: defaultExecProbeCPUMilli,
		MemoryMB: defaultExecProbeMemoryMB,
	}
	if class.Spec.SandboxDefaults != nil {
		if class.Spec.SandboxDefaults.CPUMilli > 0 {
			target.CPUMilli = class.Spec.SandboxDefaults.CPUMilli
		}
		if class.Spec.SandboxDefaults.MemoryMB > 0 {
			target.MemoryMB = class.Spec.SandboxDefaults.MemoryMB
		}
	}
	if tool.Spec.Sandbox != nil {
		if tool.Spec.Sandbox.CPUMilli != 0 {
			target.CPUMilli = tool.Spec.Sandbox.CPUMilli
		}
		if tool.Spec.Sandbox.MemoryMB != 0 {
			target.MemoryMB = tool.Spec.Sandbox.MemoryMB
		}
	}
	if limits := class.Spec.ResourceLimits; limits != nil {
		if limits.MaxCPUMilli > 0 && target.CPUMilli > limits.MaxCPUMilli {
			return ExecProbeTarget{}, fmt.Errorf("exec health probe CPU %d exceeds ToolClass limit %d", target.CPUMilli, limits.MaxCPUMilli)
		}
		if limits.MaxMemoryMB > 0 && target.MemoryMB > limits.MaxMemoryMB {
			return ExecProbeTarget{}, fmt.Errorf("exec health probe memory %d exceeds ToolClass limit %d", target.MemoryMB, limits.MaxMemoryMB)
		}
	}
	timeout, err := resolveExecProbeTimeout(tool.Spec.HealthProbe, class.Spec.SandboxDefaults, tool.Spec.Sandbox, class.Spec.ResourceLimits)
	if err != nil {
		return ExecProbeTarget{}, err
	}
	target.Timeout = timeout
	return target, nil
}

func resolveExecProbeTimeout(probe *v1alpha1.ToolHealthProbe, defaults, sandbox *v1alpha1.ToolSandboxSpec, limits *v1alpha1.ToolClassResourceLimits) (time.Duration, error) {
	timeout := probeTimeout(probe.Timeout)
	configuredSandbox := defaults
	if sandbox != nil && strings.TrimSpace(sandbox.Timeout) != "" {
		configuredSandbox = sandbox
	}
	if configuredSandbox != nil && strings.TrimSpace(configuredSandbox.Timeout) != "" {
		sandboxTimeout, err := parsePositiveExecProbeDuration("sandbox timeout", configuredSandbox.Timeout)
		if err != nil {
			return 0, err
		}
		if sandboxTimeout < timeout {
			timeout = sandboxTimeout
		}
	}
	if limits != nil && strings.TrimSpace(limits.MaxTimeout) != "" {
		maxTimeout, err := parsePositiveExecProbeDuration("ToolClass maximum timeout", limits.MaxTimeout)
		if err != nil {
			return 0, err
		}
		if timeout > maxTimeout {
			return 0, fmt.Errorf("exec health probe timeout %s exceeds ToolClass limit %s", timeout, maxTimeout)
		}
	}
	return timeout, nil
}

func parsePositiveExecProbeDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		if err != nil {
			return 0, fmt.Errorf("exec health probe %s %q is invalid: %w", name, value, err)
		}
		return 0, fmt.Errorf("exec health probe %s %q must be greater than zero", name, value)
	}
	return duration, nil
}

func (r *ToolReconciler) probeTCP(ctx context.Context, endpoint, configuredTimeout string, privateOptIn ...bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse TCP health probe endpoint: %w", err)
	}
	if parsed.Hostname() == "" || parsed.Port() == "" {
		return fmt.Errorf("TCP health probe endpoint must include a host and port")
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout(configuredTimeout))
	defer cancel()

	dialer := r.probeDialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	allowPrivate := len(privateOptIn) > 0 && privateOptIn[0]
	conn, err := toolexec.DialToolEndpoint(probeCtx, endpoint, allowPrivate, dialer)
	if err != nil {
		return fmt.Errorf("TCP health probe failed: %w", err)
	}
	if err := conn.Close(); err != nil {
		return fmt.Errorf("close TCP health probe connection: %w", err)
	}
	return nil
}

func (r *ToolReconciler) probeMCP(ctx context.Context, tool *v1alpha1.Tool, configuredTimeout string) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout(configuredTimeout))
	defer cancel()
	client := r.mcpClient
	if client == nil {
		secured, err := toolexec.SecureToolHTTPClient(probeCtx, http.DefaultClient, tool)
		if err != nil {
			return fmt.Errorf("MCP health probe endpoint denied: %w", err)
		}
		client = mcp.NewClient(secured)
	} else if err := toolexec.ValidateToolEndpoint(probeCtx, tool.Spec.Endpoint, toolAllowsPrivateNetwork(tool)); err != nil {
		return fmt.Errorf("MCP health probe endpoint denied: %w", err)
	}
	var result map[string]any
	if err := client.Call(probeCtx, tool.Spec.Endpoint, "ping", nil, &result); err != nil {
		return fmt.Errorf("MCP ping health probe failed: %w", err)
	}
	return nil
}

func toolAllowsPrivateNetwork(tool *v1alpha1.Tool) bool {
	return strings.EqualFold(tool.Spec.Metadata["allowPrivateNetwork"], "true")
}

func probeTimeout(configured string) time.Duration {
	if parsed, err := time.ParseDuration(configured); err == nil && parsed > 0 {
		return parsed
	}
	return defaultProbeTimeout
}

func computeObservedDigest(protocol v1alpha1.ToolProtocol, endpoint string, schema map[string]any) string {
	schemaBytes, _ := json.Marshal(schema)
	payload := fmt.Sprintf("%s|%s|%s", protocol, endpoint, string(schemaBytes))
	h := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(h[:])
}

func computeObservedSpecDigest(spec v1alpha1.ToolSpec) string {
	specBytes, _ := json.Marshal(spec)
	h := sha256.Sum256(specBytes)
	return "sha256:" + hex.EncodeToString(h[:])
}

// applyHealthProbe evaluates probe thresholds and updates annotations and phase.
func (r *ToolReconciler) applyHealthProbe(tool *v1alpha1.Tool, probePass bool, previousPhase v1alpha1.Phase, previousAvailable bool) {
	probe := tool.Spec.HealthProbe
	failThreshold := defaultProbeFailureThreshold
	if probe.FailureThreshold > 0 {
		failThreshold = probe.FailureThreshold
	}
	successThreshold := defaultProbeSuccessThreshold
	if probe.SuccessThreshold > 0 {
		successThreshold = probe.SuccessThreshold
	}

	// Ensure annotations map exists.
	if tool.ObjectMeta.Annotations == nil {
		tool.ObjectMeta.Annotations = make(map[string]string)
	}

	failures := getAnnotationInt32(tool.ObjectMeta.Annotations, annotationProbeFailures)
	successes := getAnnotationInt32(tool.ObjectMeta.Annotations, annotationProbeSuccesses)

	if probePass {
		successes++
		failures = 0
	} else {
		failures++
		successes = 0
	}

	tool.ObjectMeta.Annotations[annotationProbeFailures] = strconv.Itoa(int(failures))
	tool.ObjectMeta.Annotations[annotationProbeSuccesses] = strconv.Itoa(int(successes))

	// Evaluate phase transitions based on thresholds.
	lc := apiToLifecycleConditions(tool.Status.Conditions)
	if failures >= failThreshold {
		tool.Status.Phase = v1alpha1.PhaseDegraded
		tool.Status.Available = false
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionFalse,
			"ProbeFailureThresholdExceeded",
			fmt.Sprintf("consecutive probe failures (%d) reached threshold (%d)", failures, failThreshold))
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionReady, lifecycle.ConditionFalse,
			"ProbeFailureThresholdExceeded", "tool health probe failure threshold exceeded")
	} else if probePass && previousPhase == v1alpha1.PhaseDegraded && successes < successThreshold {
		tool.Status.Phase = v1alpha1.PhaseDegraded
		tool.Status.Available = false
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionTrue,
			"ProbeRecoveryPending",
			fmt.Sprintf("probe passed (%d/%d recovery threshold)", successes, successThreshold))
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionReady, lifecycle.ConditionFalse,
			"ProbeRecoveryPending", "tool has not reached its health probe recovery threshold")
	} else if probePass && previousPhase == v1alpha1.PhaseDegraded {
		// Recover from Degraded → Ready.
		tool.Status.Phase = v1alpha1.PhaseReady
		tool.Status.Available = true
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionTrue,
			"ProbeSuccessThresholdReached",
			fmt.Sprintf("consecutive probe successes (%d) reached threshold (%d)", successes, successThreshold))
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionReady, lifecycle.ConditionTrue, "Healthy", "tool recovered from degraded state")
	} else if !probePass && previousPhase == v1alpha1.PhaseDegraded {
		tool.Status.Phase = v1alpha1.PhaseDegraded
		tool.Status.Available = false
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionFalse,
			"ProbeFail",
			fmt.Sprintf("probe failed (%d/%d threshold)", failures, failThreshold))
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionReady, lifecycle.ConditionFalse,
			"ProbeRecoveryInterrupted", "tool remains degraded after a failed recovery probe")
	} else if !probePass && previousPhase == v1alpha1.PhaseReady {
		tool.Status.Phase = previousPhase
		tool.Status.Available = previousAvailable
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionFalse,
			"ProbeFail",
			fmt.Sprintf("probe failed (%d/%d threshold)", failures, failThreshold))
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionReady, lifecycle.ConditionTrue,
			"ProbeFailureBelowThreshold", "tool remains ready until its health probe failure threshold is reached")
	} else if !probePass {
		tool.Status.Phase = v1alpha1.PhaseValidating
		tool.Status.Available = false
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionFalse,
			"ProbeFail",
			fmt.Sprintf("probe failed (%d/%d threshold)", failures, failThreshold))
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionReady, lifecycle.ConditionFalse,
			"ProbeFailureBelowThreshold", "tool remains validating until its health probe failure threshold is reached")
	} else if probePass {
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionHealthy, lifecycle.ConditionTrue, "ProbePass", "health probe passed")
	}
	tool.Status.Conditions = lifecycleToAPIConditions(lc)
}

func probeInterval(probe *v1alpha1.ToolHealthProbe) time.Duration {
	interval := defaultProbeInterval
	if probe.Interval != "" {
		if d, err := time.ParseDuration(probe.Interval); err == nil && d > 0 {
			interval = d
		}
	}
	return interval
}

// nextProbeDelay reports how long to wait before the next health probe. Every
// probe type is debounced because the controller's own status and annotation
// writes re-enqueue the Tool; without it each probe immediately causes another.
func (r *ToolReconciler) nextProbeDelay(tool *v1alpha1.Tool) (time.Duration, bool) {
	if tool.Spec.HealthProbe == nil || tool.Status.LastHealthCheck == "" || computeObservedSpecDigest(tool.Spec) != tool.Status.ObservedSpecDigest {
		return 0, false
	}
	lastCheck, err := time.Parse(time.RFC3339, tool.Status.LastHealthCheck)
	if err != nil {
		return 0, false
	}
	remaining := probeInterval(tool.Spec.HealthProbe) - r.now().Sub(lastCheck)
	if remaining <= 0 {
		return 0, false
	}
	return remaining, true
}

func getAnnotationInt32(annotations map[string]string, key string) int32 {
	v, ok := annotations[key]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return int32(n)
}

func (r *ToolReconciler) updateTool(ctx context.Context, tool *v1alpha1.Tool) error {
	data, err := json.Marshal(tool)
	if err != nil {
		return fmt.Errorf("marshal tool: %w", err)
	}
	return r.store.Update(ctx, toolKind, tool.ObjectMeta.Namespace, tool.ObjectMeta.Name, data, tool.ObjectMeta.ResourceVersion)
}

// updateAgentToolConditions iterates all agents in the same namespace and sets
// the ToolsReady condition on agents that reference this tool with required=true.
func (r *ToolReconciler) updateAgentToolConditions(ctx context.Context, tool *v1alpha1.Tool) error {
	log := r.logger.With(zap.String("tool", tool.ObjectMeta.Name), zap.String("namespace", tool.ObjectMeta.Namespace), requestid.ZapField(ctx))

	items, err := r.store.List(ctx, "Agent", store.ListOptions{Namespace: tool.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	toolReady := tool.Status.Phase == v1alpha1.PhaseReady

	for _, raw := range items {
		var agent v1alpha1.Agent
		if err := json.Unmarshal(raw, &agent); err != nil {
			log.Warn("failed to unmarshal agent during tool condition update", zap.Error(err))
			continue
		}

		needsUpdate := false
		for _, toolRef := range agent.Spec.Tools {
			if toolRef.Ref.Name != tool.ObjectMeta.Name || !toolRef.Required {
				continue
			}

			// This agent has a Required reference to the tool — update its condition.
			condStatus := lifecycle.ConditionTrue
			reason := "ToolReady"
			message := fmt.Sprintf("required Tool %q is Ready", tool.ObjectMeta.Name)
			if !toolReady {
				condStatus = lifecycle.ConditionFalse
				reason = "ToolNotReady"
				message = fmt.Sprintf("required Tool %q is not in Ready phase (phase: %s)", tool.ObjectMeta.Name, tool.Status.Phase)
			}

			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(agent.Status.Conditions),
				conditionToolsReady,
				condStatus,
				reason,
				message,
			)
			agent.Status.Conditions = lifecycleToAPIConditions(lc)
			needsUpdate = true
			break
		}

		if !needsUpdate {
			continue
		}

		if err := r.updateAgentWithRetry(ctx, &agent); err != nil {
			log.Warn("failed to update agent tool condition",
				zap.String("agent", agent.ObjectMeta.Name),
				zap.Error(err))
		}
	}
	return nil
}

// updateAgentWithRetry updates an Agent with retry-on-conflict logic.
func (r *ToolReconciler) updateAgentWithRetry(ctx context.Context, agent *v1alpha1.Agent) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal agent: %w", err)
		}
		err = r.store.Update(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.Agent
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		latest.Status.Conditions = mergeAgentCondition(latest.Status.Conditions, agent.Status.Conditions, string(conditionToolsReady))
		*agent = latest
	}
	return store.ErrConflict
}

// reconcileToolClass validates a ToolClass spec and reports Ready/Failed phase.
// ToolClass is cluster-scoped (empty namespace).
func (r *ToolReconciler) reconcileToolClass(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, toolClassKind, "", req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get ToolClass: %w", err)
	}

	var tc v1alpha1.ToolClass
	if err := json.Unmarshal(raw, &tc); err != nil {
		return Result{}, fmt.Errorf("unmarshal ToolClass: %w", err)
	}

	oldPhase := tc.Status.Phase
	oldMessage := tc.Status.Message

	var (
		newPhase   v1alpha1.Phase
		newMessage string
		readyState lifecycle.ConditionStatus
		reason     string
	)
	if err := tc.Spec.Validate(); err != nil {
		newPhase = v1alpha1.PhaseFailed
		newMessage = err.Error()
		readyState = lifecycle.ConditionFalse
		reason = "ValidationFailed"
	} else {
		newPhase = v1alpha1.PhaseReady
		newMessage = "tool class spec validated"
		readyState = lifecycle.ConditionTrue
		reason = "Validated"
	}

	if oldPhase == newPhase && oldMessage == newMessage {
		return Result{}, nil
	}

	tc.Status.Phase = newPhase
	tc.Status.Message = newMessage
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(tc.Status.Conditions),
		lifecycle.ConditionReady,
		readyState,
		reason,
		newMessage,
	)
	tc.Status.Conditions = lifecycleToAPIConditions(lc)

	data, err := json.Marshal(tc)
	if err != nil {
		return Result{}, fmt.Errorf("marshal ToolClass: %w", err)
	}
	if err := r.store.Update(ctx, toolClassKind, "", req.Name, data, tc.ObjectMeta.ResourceVersion); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return Result{Requeue: true}, nil
		}
		return Result{}, fmt.Errorf("update ToolClass status: %w", err)
	}
	r.logger.Info("ToolClass reconciled",
		zap.String("name", req.Name),
		zap.String("phase", string(newPhase)),
		requestid.ZapField(ctx))
	return Result{}, nil
}
