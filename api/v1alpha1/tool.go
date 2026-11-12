// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ToolProtocol identifies the tool interface contract exposed to agents.
type ToolProtocol string

const (
	ToolProtocolMCP     ToolProtocol = "MCP"
	ToolProtocolOpenAPI ToolProtocol = "OpenAPI"
	ToolProtocolBrowser ToolProtocol = "browser"
	ToolProtocolShell   ToolProtocol = "shell"
)

// ToolClass defines a class of tool provisioner.
type ToolClass struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ToolClassSpec   `json:"spec" yaml:"spec"`
	Status     ToolClassStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (t *ToolClass) GetTypeMeta() TypeMeta      { return t.TypeMeta }
func (t *ToolClass) GetObjectMeta() *ObjectMeta { return &t.ObjectMeta }

// ExecutionTier defines where a tool executes relative to the agent runtime.
type ExecutionTier string

const (
	ExecutionTierProcess ExecutionTier = "process"
	ExecutionTierSidecar ExecutionTier = "sidecar"
	ExecutionTierPod     ExecutionTier = "pod"
	ExecutionTierVM      ExecutionTier = "vm"
)

// Transport defines the communication protocol between agent and tool.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportSSE   Transport = "sse"
	TransportHTTP  Transport = "http"
)

// ToolClassSpec defines the provisioner for a class of tools.
type ToolClassSpec struct {
	// Provisioner is the CPI provider name (e.g., "mcp", "openapi").
	Provisioner string `json:"provisioner" yaml:"provisioner"`

	// Parameters are provisioner-specific configuration.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// ExecutionTier defines the isolation level for tools of this class.
	ExecutionTier ExecutionTier `json:"executionTier,omitempty" yaml:"executionTier,omitempty"`

	// SandboxImage is the class-approved OCI image used for one-shot exec
	// health probes when executionTier is pod.
	SandboxImage string `json:"sandboxImage,omitempty" yaml:"sandboxImage,omitempty"`

	// StdioCommand is the class-approved direct argv that starts the MCP stdio
	// server in SandboxImage when executionTier is pod.
	StdioCommand []string `json:"stdioCommand,omitempty" yaml:"stdioCommand,omitempty"`

	// Transport defines the communication protocol for tools of this class.
	Transport Transport `json:"transport,omitempty" yaml:"transport,omitempty"`

	// SandboxDefaults provides default sandbox limits for tools of this class.
	SandboxDefaults *ToolSandboxSpec `json:"sandboxDefaults,omitempty" yaml:"sandboxDefaults,omitempty"`

	// ResourceLimits defines maximum resource limits for tools of this class.
	ResourceLimits *ToolClassResourceLimits `json:"resourceLimits,omitempty" yaml:"resourceLimits,omitempty"`
}

// ToolClassResourceLimits defines maximum resource limits for a tool class.
type ToolClassResourceLimits struct {
	// MaxCPUMilli is the maximum CPU in millicores any tool in this class may use.
	MaxCPUMilli int64 `json:"maxCpuMilli,omitempty" yaml:"maxCpuMilli,omitempty"`

	// MaxMemoryMB is the maximum memory in megabytes any tool in this class may use.
	MaxMemoryMB int64 `json:"maxMemoryMB,omitempty" yaml:"maxMemoryMB,omitempty"`

	// MaxTimeout is the maximum execution timeout for tools of this class.
	MaxTimeout string `json:"maxTimeout,omitempty" yaml:"maxTimeout,omitempty"`
}

// ToolClassStatus describes the observed state of a ToolClass.
type ToolClassStatus struct {
	// Phase is the lifecycle phase of the ToolClass.
	Phase Phase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// Message is a human-readable message describing the current phase.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Provisioned indicates that the execution tier has been created.
	Provisioned bool `json:"provisioned,omitempty" yaml:"provisioned,omitempty"`

	// Conditions contains the current conditions of the ToolClass.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Validate validates the ToolClassSpec fields.
func (s ToolClassSpec) Validate() error {
	if strings.TrimSpace(s.Provisioner) == "" {
		return fmt.Errorf("toolClass.spec.provisioner is required")
	}
	if s.ExecutionTier != "" {
		switch s.ExecutionTier {
		case ExecutionTierProcess, ExecutionTierSidecar, ExecutionTierPod, ExecutionTierVM:
		default:
			return fmt.Errorf("toolClass.spec.executionTier must be one of process, sidecar, pod, vm")
		}
	}
	if strings.TrimSpace(s.SandboxImage) != "" && s.ExecutionTier != ExecutionTierPod {
		return fmt.Errorf("toolClass.spec.sandboxImage requires executionTier pod")
	}
	if len(s.StdioCommand) > 0 {
		if s.ExecutionTier != ExecutionTierPod {
			return fmt.Errorf("toolClass.spec.stdioCommand requires executionTier pod")
		}
		if strings.TrimSpace(s.SandboxImage) == "" {
			return fmt.Errorf("toolClass.spec.stdioCommand requires sandboxImage")
		}
		for index, arg := range s.StdioCommand {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("toolClass.spec.stdioCommand[%d] must not be empty", index)
			}
		}
	}
	if s.Transport != "" {
		switch s.Transport {
		case TransportStdio, TransportSSE, TransportHTTP:
		default:
			return fmt.Errorf("toolClass.spec.transport must be one of stdio, sse, http")
		}
	}
	if s.SandboxDefaults != nil {
		if s.SandboxDefaults.Timeout != "" {
			if _, err := time.ParseDuration(s.SandboxDefaults.Timeout); err != nil {
				return fmt.Errorf("toolClass.spec.sandboxDefaults.timeout must be a valid duration: %w", err)
			}
		}
		if s.SandboxDefaults.CPUMilli < 0 {
			return fmt.Errorf("toolClass.spec.sandboxDefaults.cpuMilli must be >= 0")
		}
		if s.SandboxDefaults.MemoryMB < 0 {
			return fmt.Errorf("toolClass.spec.sandboxDefaults.memoryMB must be >= 0")
		}
	}
	if s.ResourceLimits != nil {
		if s.ResourceLimits.MaxCPUMilli < 0 {
			return fmt.Errorf("toolClass.spec.resourceLimits.maxCpuMilli must be >= 0")
		}
		if s.ResourceLimits.MaxMemoryMB < 0 {
			return fmt.Errorf("toolClass.spec.resourceLimits.maxMemoryMB must be >= 0")
		}
		if s.ResourceLimits.MaxTimeout != "" {
			if _, err := time.ParseDuration(s.ResourceLimits.MaxTimeout); err != nil {
				return fmt.Errorf("toolClass.spec.resourceLimits.maxTimeout must be a valid duration: %w", err)
			}
		}
	}
	return nil
}

// Tool declares an individual tool available to agents.
type Tool struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ToolSpec   `json:"spec" yaml:"spec"`
	Status     ToolStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (t *Tool) GetTypeMeta() TypeMeta      { return t.TypeMeta }
func (t *Tool) GetObjectMeta() *ObjectMeta { return &t.ObjectMeta }

// ToolSpec defines the desired state of a Tool.
type ToolSpec struct {
	// Protocol is the declared tool interface contract. Defaults to MCP.
	Protocol ToolProtocol `json:"protocol,omitempty" yaml:"protocol,omitempty"`

	// ClassName references the ToolClass provisioner.
	ClassName string `json:"className" yaml:"className"`

	// Description is human-readable documentation for the tool.
	Description string `json:"description,omitempty" yaml:"description,omitempty"`

	// Endpoint is the connection string or URI for the tool server.
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`

	// SecretRefs inject credentials needed by the tool's backing service.
	SecretRefs []ToolSecretRef `json:"secretRefs,omitempty" yaml:"secretRefs,omitempty"`

	// Schema defines the tool contract. For OpenAPI tools this is the source
	// OpenAPI document used for MCP adaptation.
	Schema map[string]any `json:"schema,omitempty" yaml:"schema,omitempty"`

	// InputSchema defines the expected input as a JSON Schema.
	// Deprecated: use Schema.
	InputSchema map[string]any `json:"inputSchema,omitempty" yaml:"inputSchema,omitempty"`

	// DigestPin is an optional pinned tool digest in "sha256:<hex>" format.
	DigestPin string `json:"digestPin,omitempty" yaml:"digestPin,omitempty"`

	// Signature is a cosign-compatible supply-chain signature for verification.
	Signature string `json:"signature,omitempty" yaml:"signature,omitempty"`

	// Metadata contains additional implementation metadata for the tool.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Trusted enables in-process execution for low-latency trusted tools.
	// By default, tools execute in a process-isolated sandbox.
	Trusted bool `json:"trusted,omitempty" yaml:"trusted,omitempty"`

	// Sandbox configures resource limits for process-isolated execution.
	Sandbox *ToolSandboxSpec `json:"sandbox,omitempty" yaml:"sandbox,omitempty"`

	// HealthProbe configures periodic health checks for the tool endpoint.
	HealthProbe *ToolHealthProbe `json:"healthProbe,omitempty" yaml:"healthProbe,omitempty"`

	// ExecutionPolicy controls gateway-enforced timeout, concurrency, retries,
	// and circuit breaking. Zero values preserve the existing behavior.
	ExecutionPolicy *ToolExecutionPolicy `json:"executionPolicy,omitempty" yaml:"executionPolicy,omitempty"`
}

// ToolExecutionPolicy controls resilience behavior at the Tool Gateway.
type ToolExecutionPolicy struct {
	Timeout          string              `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxConcurrent    int32               `json:"maxConcurrent,omitempty" yaml:"maxConcurrent,omitempty"`
	Idempotent       bool                `json:"idempotent,omitempty" yaml:"idempotent,omitempty"`
	RetryMaxAttempts int32               `json:"retryMaxAttempts,omitempty" yaml:"retryMaxAttempts,omitempty"`
	CircuitBreaker   *ToolCircuitBreaker `json:"circuitBreaker,omitempty" yaml:"circuitBreaker,omitempty"`
}

// ToolCircuitBreaker opens after transient target failures and fails fast
// until the configured cooldown expires.
type ToolCircuitBreaker struct {
	FailureThreshold int32  `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`
	Cooldown         string `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`
}

// ProbeType identifies the mechanism used for tool health checks.
type ProbeType string

const (
	ProbeTypeMCPPing ProbeType = "mcp-ping"
	ProbeTypeHTTPGet ProbeType = "http-get"
	ProbeTypeTCP     ProbeType = "tcp"
	ProbeTypeExec    ProbeType = "exec"
)

// ToolHealthProbe defines how and when to probe tool liveness.
type ToolHealthProbe struct {
	// Type selects the probe mechanism.
	Type ProbeType `json:"type" yaml:"type"`

	// Command is the argv executed inside the tool sandbox for exec probes.
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`

	// Interval is the period between probes (Go duration, e.g. "30s").
	Interval string `json:"interval,omitempty" yaml:"interval,omitempty"`

	// Timeout is the per-probe timeout (Go duration, e.g. "5s").
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// FailureThreshold is consecutive failures before marking Degraded.
	FailureThreshold int32 `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`

	// SuccessThreshold is consecutive successes before marking Ready.
	SuccessThreshold int32 `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
}

// ToolSandboxSpec configures resource limits for sandboxed tool execution.
type ToolSandboxSpec struct {
	// Timeout is the maximum wall-clock duration for one sandboxed invocation.
	// Uses Go duration syntax (for example, "2s", "500ms").
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// CPUMilli is the CPU limit in millicores (for example, 500 = 0.5 CPU).
	CPUMilli int64 `json:"cpuMilli,omitempty" yaml:"cpuMilli,omitempty"`

	// MemoryMB is the memory limit in megabytes.
	MemoryMB int64 `json:"memoryMB,omitempty" yaml:"memoryMB,omitempty"`

	// NetworkPolicy restricts network access ("none", "egress-only", "full").
	NetworkPolicy string `json:"networkPolicy,omitempty" yaml:"networkPolicy,omitempty"`
}

// ToolSecretRef references a SecretRef and the runtime injection key for that secret.
type ToolSecretRef struct {
	Name string `json:"name" yaml:"name"`
	Key  string `json:"key" yaml:"key"`
}

// Validate validates Tool secret bindings.
func (s ToolSpec) Validate() error {
	if s.Protocol != "" && s.Protocol != ToolProtocolMCP && s.Protocol != ToolProtocolOpenAPI && s.Protocol != ToolProtocolBrowser && s.Protocol != ToolProtocolShell {
		return fmt.Errorf("tool.spec.protocol must be one of MCP, OpenAPI, browser, shell")
	}

	for i, secretRef := range s.SecretRefs {
		if secretRef.Name == "" {
			return fmt.Errorf("tool.spec.secretRefs[%d].name is required", i)
		}
		if secretRef.Key == "" {
			return fmt.Errorf("tool.spec.secretRefs[%d].key is required", i)
		}
	}

	if s.DigestPin != "" {
		const prefix = "sha256:"
		if len(s.DigestPin) <= len(prefix) || s.DigestPin[:len(prefix)] != prefix {
			return fmt.Errorf("tool.spec.digestPin must use format sha256:<hex>")
		}
	}

	if s.Protocol == ToolProtocolOpenAPI && len(s.EffectiveSchema()) == 0 {
		return fmt.Errorf("tool.spec.schema is required when protocol is OpenAPI")
	}

	if s.Sandbox != nil {
		if s.Sandbox.Timeout != "" {
			if _, err := time.ParseDuration(s.Sandbox.Timeout); err != nil {
				return fmt.Errorf("tool.spec.sandbox.timeout must be a valid duration: %w", err)
			}
		}
		if s.Sandbox.CPUMilli < 0 {
			return fmt.Errorf("tool.spec.sandbox.cpuMilli must be >= 0")
		}
		if s.Sandbox.MemoryMB < 0 {
			return fmt.Errorf("tool.spec.sandbox.memoryMB must be >= 0")
		}
	}
	if s.ExecutionPolicy != nil {
		policy := s.ExecutionPolicy
		if policy.Timeout != "" {
			if _, err := time.ParseDuration(policy.Timeout); err != nil {
				return fmt.Errorf("tool.spec.executionPolicy.timeout must be a valid duration: %w", err)
			}
		}
		if policy.MaxConcurrent < 0 || policy.RetryMaxAttempts < 0 {
			return fmt.Errorf("tool.spec.executionPolicy concurrency and retry values must be >= 0")
		}
		if policy.RetryMaxAttempts > 1 && !policy.Idempotent {
			return fmt.Errorf("tool.spec.executionPolicy.retryMaxAttempts requires idempotent=true")
		}
		if policy.CircuitBreaker != nil {
			if policy.CircuitBreaker.FailureThreshold < 1 {
				return fmt.Errorf("tool.spec.executionPolicy.circuitBreaker.failureThreshold must be >= 1")
			}
			if policy.CircuitBreaker.Cooldown == "" {
				return fmt.Errorf("tool.spec.executionPolicy.circuitBreaker.cooldown is required")
			}
			if _, err := time.ParseDuration(policy.CircuitBreaker.Cooldown); err != nil {
				return fmt.Errorf("tool.spec.executionPolicy.circuitBreaker.cooldown must be a valid duration: %w", err)
			}
		}
	}

	if s.HealthProbe != nil {
		switch s.HealthProbe.Type {
		case ProbeTypeMCPPing, ProbeTypeHTTPGet, ProbeTypeTCP, ProbeTypeExec:
		default:
			return fmt.Errorf("tool.spec.healthProbe.type must be one of mcp-ping, http-get, tcp, exec")
		}
		if s.HealthProbe.Type == ProbeTypeExec {
			if len(s.HealthProbe.Command) == 0 {
				return fmt.Errorf("tool.spec.healthProbe.command is required when type is exec")
			}
			for i, arg := range s.HealthProbe.Command {
				if arg == "" {
					return fmt.Errorf("tool.spec.healthProbe.command[%d] must not be empty", i)
				}
			}
		} else if len(s.HealthProbe.Command) > 0 {
			return fmt.Errorf("tool.spec.healthProbe.command is only valid when type is exec")
		}
		if s.HealthProbe.Interval != "" {
			interval, err := time.ParseDuration(s.HealthProbe.Interval)
			if err != nil {
				return fmt.Errorf("tool.spec.healthProbe.interval must be a valid duration: %w", err)
			}
			if interval <= 0 {
				return fmt.Errorf("tool.spec.healthProbe.interval must be greater than zero")
			}
		}
		if s.HealthProbe.Timeout != "" {
			timeout, err := time.ParseDuration(s.HealthProbe.Timeout)
			if err != nil {
				return fmt.Errorf("tool.spec.healthProbe.timeout must be a valid duration: %w", err)
			}
			if timeout <= 0 {
				return fmt.Errorf("tool.spec.healthProbe.timeout must be greater than zero")
			}
		}
		if s.HealthProbe.FailureThreshold < 0 {
			return fmt.Errorf("tool.spec.healthProbe.failureThreshold must be >= 0")
		}
		if s.HealthProbe.SuccessThreshold < 0 {
			return fmt.Errorf("tool.spec.healthProbe.successThreshold must be >= 0")
		}
	}

	return nil
}

// EffectiveProtocol returns the configured protocol or the default MCP.
func (s ToolSpec) EffectiveProtocol() ToolProtocol {
	if s.Protocol == "" {
		return ToolProtocolMCP
	}
	return s.Protocol
}

// EffectiveSchema returns Schema when present, falling back to InputSchema.
func (s ToolSpec) EffectiveSchema() map[string]any {
	if len(s.Schema) > 0 {
		return s.Schema
	}
	return s.InputSchema
}

// ToolStatus describes the observed state of a Tool.
type ToolStatus struct {
	Phase              Phase              `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions         []Condition        `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Available          bool               `json:"available,omitempty" yaml:"available,omitempty"`
	LastHealthCheck    string             `json:"lastHealthCheck,omitempty" yaml:"lastHealthCheck,omitempty"`
	ResolvedProtocol   ToolProtocol       `json:"resolvedProtocol,omitempty" yaml:"resolvedProtocol,omitempty"`
	ObservedDigest     string             `json:"observedDigest,omitempty" yaml:"observedDigest,omitempty"`
	ObservedSpecDigest string             `json:"observedSpecDigest,omitempty" yaml:"observedSpecDigest,omitempty"`
	CompiledMCP        *MCPToolDefinition `json:"compiledMCP,omitempty" yaml:"compiledMCP,omitempty"`
}

// MCPToolDefinition is the compiled MCP representation of a tool,
// produced by protocol compilation during reconciliation.
type MCPToolDefinition struct {
	Name        string         `json:"name" yaml:"name"`
	Description string         `json:"description,omitempty" yaml:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty" yaml:"inputSchema,omitempty"`
	Transport   Transport      `json:"transport" yaml:"transport"`
	Endpoint    string         `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
}

// ToolGrantVerb identifies an action permitted by a ToolGrant.
type ToolGrantVerb string

const (
	ToolGrantVerbInvoke   ToolGrantVerb = "invoke"
	ToolGrantVerbDiscover ToolGrantVerb = "discover"
	ToolGrantVerbInspect  ToolGrantVerb = "inspect"
)

// ToolGrant authorizes an agent to use a tool with specific verbs.
type ToolGrant struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ToolGrantSpec   `json:"spec" yaml:"spec"`
	Status     ToolGrantStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (t *ToolGrant) GetTypeMeta() TypeMeta      { return t.TypeMeta }
func (t *ToolGrant) GetObjectMeta() *ObjectMeta { return &t.ObjectMeta }

// ToolGrantSpec defines fine-grained tool access with verb taxonomy.
type ToolGrantSpec struct {
	// AgentSelector selects agents by label (matchLabels semantics).
	AgentSelector map[string]string `json:"agentSelector,omitempty" yaml:"agentSelector,omitempty"`

	// AgentRef is a direct agent reference (legacy, mutually exclusive with AgentSelector).
	AgentRef LocalObjectReference `json:"agentRef,omitempty" yaml:"agentRef,omitempty"`

	// ToolRef references the target tool.
	ToolRef LocalObjectReference `json:"toolRef" yaml:"toolRef"`

	// Verbs is the set of granted actions. Must be a non-empty subset of {invoke, discover, inspect}.
	Verbs []ToolGrantVerb `json:"verbs" yaml:"verbs"`

	// ArgumentConstraints is a JSON Schema subset that restricts input arguments.
	ArgumentConstraints map[string]any `json:"argumentConstraints,omitempty" yaml:"argumentConstraints,omitempty"`

	// RateLimits defines per-grant call rate caps.
	RateLimits *ToolGrantRateLimits `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty"`

	// Conditions defines time-bound and label-based grant conditions.
	Conditions *ToolGrantConditions `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// ToolGrantRateLimits defines rate limiting for a ToolGrant.
type ToolGrantRateLimits struct {
	CallsPerMinute int64 `json:"callsPerMinute,omitempty" yaml:"callsPerMinute,omitempty"`
	CallsPerHour   int64 `json:"callsPerHour,omitempty" yaml:"callsPerHour,omitempty"`
	CallsPerDay    int64 `json:"callsPerDay,omitempty" yaml:"callsPerDay,omitempty"`
}

// ToolGrantConditions defines time-bound and label conditions for a ToolGrant.
type ToolGrantConditions struct {
	ValidFrom           string            `json:"validFrom,omitempty" yaml:"validFrom,omitempty"`
	ValidUntil          string            `json:"validUntil,omitempty" yaml:"validUntil,omitempty"`
	RequiredAgentLabels map[string]string `json:"requiredAgentLabels,omitempty" yaml:"requiredAgentLabels,omitempty"`
}

// ToolGrantStatus describes the observed state of a ToolGrant.
type ToolGrantStatus struct {
	Active            bool   `json:"active,omitempty" yaml:"active,omitempty"`
	LastEvaluatedTime string `json:"lastEvaluatedTime,omitempty" yaml:"lastEvaluatedTime,omitempty"`
	GrantedAgents     int32  `json:"grantedAgents,omitempty" yaml:"grantedAgents,omitempty"`
}

// Validate validates the ToolGrantSpec fields.
func (s ToolGrantSpec) Validate() error {
	if s.AgentRef.Name != "" && s.AgentSelector != nil {
		return fmt.Errorf("toolGrant.spec.agentRef and agentSelector are mutually exclusive")
	}
	if s.ToolRef.Name == "" {
		return fmt.Errorf("toolGrant.spec.toolRef.name is required")
	}
	if len(s.Verbs) == 0 {
		return fmt.Errorf("toolGrant.spec.verbs must be non-empty")
	}
	seen := make(map[ToolGrantVerb]bool, len(s.Verbs))
	for i, v := range s.Verbs {
		switch v {
		case ToolGrantVerbInvoke, ToolGrantVerbDiscover, ToolGrantVerbInspect:
		default:
			return fmt.Errorf("toolGrant.spec.verbs[%d] must be one of invoke, discover, inspect", i)
		}
		if seen[v] {
			return fmt.Errorf("toolGrant.spec.verbs[%d] duplicate verb %q", i, v)
		}
		seen[v] = true
	}
	if s.RateLimits != nil {
		if s.RateLimits.CallsPerMinute < 0 {
			return fmt.Errorf("toolGrant.spec.rateLimits.callsPerMinute must be >= 0")
		}
		if s.RateLimits.CallsPerHour < 0 {
			return fmt.Errorf("toolGrant.spec.rateLimits.callsPerHour must be >= 0")
		}
		if s.RateLimits.CallsPerDay < 0 {
			return fmt.Errorf("toolGrant.spec.rateLimits.callsPerDay must be >= 0")
		}
	}
	var validFrom time.Time
	if s.Conditions != nil {
		if s.Conditions.ValidFrom != "" {
			parsed, err := time.Parse(time.RFC3339, s.Conditions.ValidFrom)
			if err != nil {
				return fmt.Errorf("toolGrant.spec.conditions.validFrom must be RFC3339: %w", err)
			}
			validFrom = parsed
		}
		if s.Conditions.ValidUntil != "" {
			validUntil, err := time.Parse(time.RFC3339, s.Conditions.ValidUntil)
			if err != nil {
				return fmt.Errorf("toolGrant.spec.conditions.validUntil must be RFC3339: %w", err)
			}
			if !validFrom.IsZero() && !validUntil.After(validFrom) {
				return fmt.Errorf("toolGrant.spec.conditions.validUntil must be after validFrom")
			}
		}
	}
	if len(s.ArgumentConstraints) > 0 {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("toolgrant-argument-constraints.json", s.ArgumentConstraints); err != nil {
			return fmt.Errorf("toolGrant.spec.argumentConstraints is invalid: %w", err)
		}
		if _, err := compiler.Compile("toolgrant-argument-constraints.json"); err != nil {
			return fmt.Errorf("toolGrant.spec.argumentConstraints is invalid: %w", err)
		}
	}
	return nil
}

// HasVerb reports whether the grant includes the specified verb.
func (s ToolGrantSpec) HasVerb(verb ToolGrantVerb) bool {
	for _, v := range s.Verbs {
		if v == verb {
			return true
		}
	}
	return false
}
