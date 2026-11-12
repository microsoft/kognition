// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolProtocol identifies the tool interface contract.
// +kubebuilder:validation:Enum=MCP;OpenAPI;browser;shell
type ToolProtocol string

const (
	ToolProtocolMCP     ToolProtocol = "MCP"
	ToolProtocolOpenAPI ToolProtocol = "OpenAPI"
	ToolProtocolBrowser ToolProtocol = "browser"
	ToolProtocolShell   ToolProtocol = "shell"
)

// ExecutionTier defines where a tool executes relative to the agent runtime.
// +kubebuilder:validation:Enum=process;sidecar;pod;vm
type ExecutionTier string

const (
	ExecutionTierProcess ExecutionTier = "process"
	ExecutionTierSidecar ExecutionTier = "sidecar"
	ExecutionTierPod     ExecutionTier = "pod"
	ExecutionTierVM      ExecutionTier = "vm"
)

// Transport defines the communication protocol between agent and tool.
// +kubebuilder:validation:Enum=stdio;sse;http
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportSSE   Transport = "sse"
	TransportHTTP  Transport = "http"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tool declares an executable tool that agents can invoke.
type Tool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ToolSpec   `json:"spec,omitempty"`
	Status            ToolStatus `json:"status,omitempty"`
}

// ToolSpec defines the desired state of a Tool.
type ToolSpec struct {
	// Protocol is the tool interface protocol. Defaults to MCP.
	// +optional
	Protocol ToolProtocol `json:"protocol,omitempty"`

	// ClassName references the ToolClass provisioner.
	ClassName string `json:"className"`

	// Description is a human-readable description of the tool.
	// +optional
	Description string `json:"description,omitempty"`

	// Endpoint is the tool's network endpoint (URL or address).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// SecretRefs references Secrets containing tool credentials.
	// Each entry specifies a Secret name and a key within that Secret.
	// +optional
	SecretRefs []ToolSecretRef `json:"secretRefs,omitempty"`

	// Schema defines the tool contract. For OpenAPI tools this is the source
	// OpenAPI document used for MCP adaptation.
	// +optional
	Schema map[string]any `json:"schema,omitempty"`

	// InputSchema defines the expected input as a JSON Schema.
	// Deprecated: use Schema.
	// +optional
	InputSchema map[string]any `json:"inputSchema,omitempty"`

	// DigestPin is an optional pinned tool digest in "sha256:<hex>" format.
	// +optional
	DigestPin string `json:"digestPin,omitempty"`

	// Signature is a cosign-compatible supply-chain signature for verification.
	// +optional
	Signature string `json:"signature,omitempty"`

	// Metadata contains additional implementation metadata for the tool.
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`

	// Trusted enables in-process execution for low-latency trusted tools.
	// +optional
	Trusted bool `json:"trusted,omitempty"`

	// Sandbox configures the tool's execution sandbox.
	// +optional
	Sandbox *ToolSandboxSpec `json:"sandbox,omitempty"`

	// HealthProbe configures periodic health checks for the tool endpoint.
	// +optional
	HealthProbe *ToolHealthProbe `json:"healthProbe,omitempty"`

	// ExecutionPolicy controls gateway-enforced resilience behavior.
	// +optional
	ExecutionPolicy *ToolExecutionPolicy `json:"executionPolicy,omitempty"`
}

// ToolExecutionPolicy controls timeout, concurrency, retry, and circuit
// breaker behavior at the Tool Gateway.
type ToolExecutionPolicy struct {
	// +optional
	Timeout string `json:"timeout,omitempty"`
	// +optional
	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`
	// +optional
	Idempotent bool `json:"idempotent,omitempty"`
	// +optional
	RetryMaxAttempts int32 `json:"retryMaxAttempts,omitempty"`
	// +optional
	CircuitBreaker *ToolCircuitBreaker `json:"circuitBreaker,omitempty"`
}

// ToolCircuitBreaker opens after transient failures until cooldown expires.
type ToolCircuitBreaker struct {
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
	// +optional
	Cooldown string `json:"cooldown,omitempty"`
}

// ToolSecretRef references a Secret by name and a specific key within it.
type ToolSecretRef struct {
	// Name is the name of the Secret resource.
	Name string `json:"name"`

	// Key is the key within the Secret to use.
	// +optional
	Key string `json:"key,omitempty"`
}

// ToolSandboxSpec defines sandbox resource limits for tool execution.
type ToolSandboxSpec struct {
	// Timeout is the maximum wall-clock duration for one sandboxed invocation.
	// Uses Go duration syntax (for example, "2s", "500ms").
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// CPUMilli is the CPU limit in millicores.
	// +optional
	CPUMilli int64 `json:"cpuMilli,omitempty"`

	// MemoryMB is the memory limit in megabytes.
	// +optional
	MemoryMB int64 `json:"memoryMB,omitempty"`

	// NetworkPolicy restricts network access ("none", "egress-only", "full").
	// +optional
	NetworkPolicy string `json:"networkPolicy,omitempty"`
}

// ProbeType identifies the mechanism used for tool health checks.
// +kubebuilder:validation:Enum="mcp-ping";"http-get";"tcp";"exec"
type ProbeType string

const (
	ProbeTypeMCPPing ProbeType = "mcp-ping"
	ProbeTypeHTTPGet ProbeType = "http-get"
	ProbeTypeTCP     ProbeType = "tcp"
	ProbeTypeExec    ProbeType = "exec"
)

// ToolHealthProbe defines how and when to probe tool liveness.
// +kubebuilder:validation:XValidation:rule="self.type != 'exec' || (has(self.command) && size(self.command) > 0 && self.command.all(arg, size(arg) > 0))",message="healthProbe.command is required and must contain only non-empty arguments when type is exec"
// +kubebuilder:validation:XValidation:rule="self.type == 'exec' || !has(self.command) || size(self.command) == 0",message="healthProbe.command is only valid when type is exec"
type ToolHealthProbe struct {
	// Type selects the probe mechanism.
	Type ProbeType `json:"type"`

	// Command is the argv executed inside the tool sandbox for exec probes.
	// +optional
	Command []string `json:"command,omitempty"`

	// Interval is the period between probes (Go duration, e.g. "30s").
	// +optional
	Interval string `json:"interval,omitempty"`

	// Timeout is the per-probe timeout (Go duration, e.g. "5s").
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// FailureThreshold is consecutive failures before marking Degraded.
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is consecutive successes before marking Ready.
	// +optional
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// ToolStatus describes the observed state of a Tool.
type ToolStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Ready indicates the tool is provisioned and responding.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// LastHealthCheck is the RFC3339 timestamp of the latest evaluation.
	// +optional
	LastHealthCheck string `json:"lastHealthCheck,omitempty"`

	// ResolvedProtocol is the protocol selected after adaptation.
	// +optional
	ResolvedProtocol ToolProtocol `json:"resolvedProtocol,omitempty"`

	// ObservedDigest is the endpoint contract digest used for pin verification.
	// +optional
	ObservedDigest string `json:"observedDigest,omitempty"`

	// ObservedSpecDigest identifies the complete Tool spec evaluated by the controller.
	// +optional
	ObservedSpecDigest string `json:"observedSpecDigest,omitempty"`

	// CompiledMCP is the invocation definition produced for an available Tool.
	// +optional
	CompiledMCP *MCPToolDefinition `json:"compiledMCP,omitempty"`
}

// MCPToolDefinition is the compiled invocation representation of a Tool.
type MCPToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
	Transport   Transport              `json:"transport"`
	Endpoint    string                 `json:"endpoint,omitempty"`
}

// +kubebuilder:object:root=true

// ToolList contains a list of Tools.
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Provisioner",type=string,JSONPath=`.spec.provisioner`
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.spec.executionTier`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ToolClass defines a class of tool provisioner (cluster-scoped).
type ToolClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ToolClassSpec   `json:"spec,omitempty"`
	Status            ToolClassStatus `json:"status,omitempty"`
}

// ToolClassSpec defines the provisioner for a class of tools.
type ToolClassSpec struct {
	// Provisioner is the CPI provider name (e.g., "mcp", "openapi").
	Provisioner string `json:"provisioner"`

	// Parameters are provisioner-specific configuration.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ExecutionTier defines the isolation level for tools of this class.
	// +optional
	ExecutionTier ExecutionTier `json:"executionTier,omitempty"`

	// SandboxImage is the class-approved OCI image used for one-shot exec
	// health probes when executionTier is pod.
	// +optional
	SandboxImage string `json:"sandboxImage,omitempty"`

	// StdioCommand is the class-approved direct argv that starts an MCP stdio
	// server in SandboxImage when executionTier is pod.
	// +optional
	StdioCommand []string `json:"stdioCommand,omitempty"`

	// Transport defines the communication protocol for tools of this class.
	// +optional
	Transport Transport `json:"transport,omitempty"`

	// SandboxDefaults provides default sandbox limits for tools of this class.
	// +optional
	SandboxDefaults *ToolSandboxSpec `json:"sandboxDefaults,omitempty"`

	// ResourceLimits defines maximum resource limits for tools of this class.
	// +optional
	ResourceLimits *ToolClassResourceLimits `json:"resourceLimits,omitempty"`
}

// ToolClassResourceLimits defines maximum resource limits for a tool class.
type ToolClassResourceLimits struct {
	// MaxCPUMilli is the maximum CPU in millicores any tool in this class may use.
	// +optional
	MaxCPUMilli int64 `json:"maxCpuMilli,omitempty"`

	// MaxMemoryMB is the maximum memory in megabytes any tool in this class may use.
	// +optional
	MaxMemoryMB int64 `json:"maxMemoryMB,omitempty"`

	// MaxTimeout is the maximum execution timeout for tools of this class.
	// +optional
	MaxTimeout string `json:"maxTimeout,omitempty"`
}

// ToolClassStatus describes the observed state of a ToolClass.
type ToolClassStatus struct {
	// Phase is the lifecycle phase (Pending|Ready|Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message is a human-readable message describing the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Provisioned indicates that the execution tier has been created.
	// +optional
	Provisioned bool `json:"provisioned,omitempty"`

	// Conditions contains the current conditions of the ToolClass.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ToolGrantVerb identifies an action permitted by a ToolGrant.
// +kubebuilder:validation:Enum=invoke;discover;inspect
type ToolGrantVerb string

const (
	ToolGrantVerbInvoke   ToolGrantVerb = "invoke"
	ToolGrantVerbDiscover ToolGrantVerb = "discover"
	ToolGrantVerbInspect  ToolGrantVerb = "inspect"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Tool",type=string,JSONPath=`.spec.toolRef.name`
// +kubebuilder:printcolumn:name="Active",type=boolean,JSONPath=`.status.active`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ToolGrant authorizes agents to use a tool with specific verbs.
type ToolGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ToolGrantSpec   `json:"spec"`
	Status            ToolGrantStatus `json:"status,omitempty"`
}

// ToolGrantSpec defines fine-grained tool access with verb taxonomy.
// +kubebuilder:validation:XValidation:rule="!(has(self.agentRef) && has(self.agentSelector))",message="agentRef and agentSelector are mutually exclusive"
type ToolGrantSpec struct {
	// AgentSelector selects agents by label using matchLabels semantics.
	// +optional
	AgentSelector map[string]string `json:"agentSelector,omitempty"`

	// AgentRef directly references an agent.
	// +optional
	AgentRef LocalObjectReference `json:"agentRef,omitempty"`

	// ToolRef references the target tool.
	ToolRef LocalObjectReference `json:"toolRef"`

	// Verbs is the non-empty set of granted actions.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Verbs []ToolGrantVerb `json:"verbs"`

	// ArgumentConstraints is a JSON Schema subset that restricts input arguments.
	// +optional
	ArgumentConstraints map[string]any `json:"argumentConstraints,omitempty"`

	// RateLimits defines per-grant call rate caps.
	// +optional
	RateLimits *ToolGrantRateLimits `json:"rateLimits,omitempty"`

	// Conditions defines time-bound and label-based grant conditions.
	// +optional
	Conditions *ToolGrantConditions `json:"conditions,omitempty"`
}

// ToolGrantRateLimits defines rate limiting for a ToolGrant.
type ToolGrantRateLimits struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	CallsPerMinute int64 `json:"callsPerMinute,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	CallsPerHour int64 `json:"callsPerHour,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	CallsPerDay int64 `json:"callsPerDay,omitempty"`
}

// ToolGrantConditions defines time-bound and label conditions for a ToolGrant.
type ToolGrantConditions struct {
	// +optional
	ValidFrom string `json:"validFrom,omitempty"`
	// +optional
	ValidUntil string `json:"validUntil,omitempty"`
	// +optional
	RequiredAgentLabels map[string]string `json:"requiredAgentLabels,omitempty"`
}

// ToolGrantStatus describes the observed state of a ToolGrant.
type ToolGrantStatus struct {
	// +optional
	Active bool `json:"active,omitempty"`
	// +optional
	LastEvaluatedTime string `json:"lastEvaluatedTime,omitempty"`
	// +optional
	GrantedAgents int32 `json:"grantedAgents,omitempty"`
}

// +kubebuilder:object:root=true

// ToolGrantList contains a list of ToolGrants.
type ToolGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolGrant `json:"items"`
}

// +kubebuilder:object:root=true

// ToolClassList contains a list of ToolClasses.
type ToolClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tool{}, &ToolList{})
	SchemeBuilder.Register(&ToolClass{}, &ToolClassList{})
	SchemeBuilder.Register(&ToolGrant{}, &ToolGrantList{})
}
