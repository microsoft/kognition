// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// Workspace provides multi-tenant isolation (analogous to K8s Namespace).
type Workspace struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       WorkspaceSpec   `json:"spec" yaml:"spec"`
	Status     WorkspaceStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (w *Workspace) GetTypeMeta() TypeMeta      { return w.TypeMeta }
func (w *Workspace) GetObjectMeta() *ObjectMeta { return &w.ObjectMeta }

// WorkspaceSpec defines workspace configuration.
type WorkspaceSpec struct {
	// DisplayName is a human-friendly name for the workspace.
	DisplayName string `json:"displayName,omitempty" yaml:"displayName,omitempty"`

	// RequireSignedArtifacts enables supply-chain signature verification
	// for Tool and Model resources admitted into this workspace.
	RequireSignedArtifacts bool `json:"requireSignedArtifacts,omitempty" yaml:"requireSignedArtifacts,omitempty"`

	// Retention configures data retention policies for observability data.
	Retention *RetentionPolicy `json:"retention,omitempty" yaml:"retention,omitempty"`
}

// RetentionPolicy defines how long observability data is kept per workspace.
type RetentionPolicy struct {
	// TraceTTL is the duration to retain trace spans (e.g., "720h" = 30 days).
	// Zero means retain indefinitely.
	TraceTTL Duration `json:"traceTTL,omitempty" yaml:"traceTTL,omitempty"`

	// AuditLogTTL is the duration to retain audit log entries.
	// Zero means retain indefinitely.
	AuditLogTTL Duration `json:"auditLogTTL,omitempty" yaml:"auditLogTTL,omitempty"`

	// CostRecordTTL is the duration to retain cost records.
	// Zero means retain indefinitely.
	CostRecordTTL Duration `json:"costRecordTTL,omitempty" yaml:"costRecordTTL,omitempty"`

	// EventTTL is the duration to retain instance events.
	// Zero means retain indefinitely.
	EventTTL Duration `json:"eventTTL,omitempty" yaml:"eventTTL,omitempty"`
}

// WorkspaceStatus describes the observed state of a Workspace.
type WorkspaceStatus struct {
	Phase Phase `json:"phase,omitempty" yaml:"phase,omitempty"`
}

// AgentIdentity provides workload identity (SPIFFE) for an agent.
type AgentIdentity struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentIdentitySpec   `json:"spec" yaml:"spec"`
	Status     AgentIdentityStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *AgentIdentity) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentIdentity) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentIdentitySpec defines the identity configuration.
type AgentIdentitySpec struct {
	// SPIFFEID is the workload identity URI.
	SPIFFEID string `json:"spiffeID,omitempty" yaml:"spiffeID,omitempty"`

	// AgentRef references the agent this identity belongs to.
	AgentRef LocalObjectReference `json:"agentRef" yaml:"agentRef"`
}

// AgentIdentityStatus describes the observed state of an identity.
type AgentIdentityStatus struct {
	Phase            Phase                   `json:"phase,omitempty" yaml:"phase,omitempty"`
	WorkloadIdentity *WorkloadIdentityStatus `json:"workloadIdentity,omitempty" yaml:"workloadIdentity,omitempty"`
}

type WorkloadIdentityStatus struct {
	AgentUID         string    `json:"agentUID,omitempty" yaml:"agentUID,omitempty"`
	SPIFFEID         string    `json:"spiffeID,omitempty" yaml:"spiffeID,omitempty"`
	TrustDomain      string    `json:"trustDomain,omitempty" yaml:"trustDomain,omitempty"`
	NotBefore        time.Time `json:"notBefore,omitempty" yaml:"notBefore,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt,omitempty" yaml:"expiresAt,omitempty"`
	SourceGeneration int64     `json:"sourceGeneration,omitempty" yaml:"sourceGeneration,omitempty"`
	Ready            bool      `json:"ready" yaml:"ready"`
	Reason           string    `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// AgentRole defines a set of permissions for agents.
type AgentRole struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Rules      []PolicyRule `json:"rules" yaml:"rules"`
}

func (a *AgentRole) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentRole) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// PolicyRule defines a set of permitted actions.
type PolicyRule struct {
	// Resources lists the resource kinds this rule applies to.
	Resources []string `json:"resources" yaml:"resources"`

	// Verbs lists the permitted actions (get, list, create, update, delete).
	Verbs []string `json:"verbs" yaml:"verbs"`
}

// AgentRoleBinding binds an AgentRole to an agent identity.
type AgentRoleBinding struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentRoleBindingSpec `json:"spec" yaml:"spec"`
}

func (a *AgentRoleBinding) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentRoleBinding) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentRoleBindingSpec binds a role to subjects.
type AgentRoleBindingSpec struct {
	RoleRef  LocalObjectReference `json:"roleRef" yaml:"roleRef"`
	Subjects []Subject            `json:"subjects" yaml:"subjects"`
}

// ControlPlaneRole defines standalone decision and execution permissions.
type ControlPlaneRole struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ControlPlaneRoleSpec `json:"spec" yaml:"spec"`
}

func (c *ControlPlaneRole) GetTypeMeta() TypeMeta      { return c.TypeMeta }
func (c *ControlPlaneRole) GetObjectMeta() *ObjectMeta { return &c.ObjectMeta }

// ControlPlaneRoleSpec defines ordered-independent allow and deny rules.
type ControlPlaneRoleSpec struct {
	Rules          []ControlPlanePolicyRule    `json:"rules" yaml:"rules"`
	ExecutionRules []ControlPlaneExecutionRule `json:"executionRules,omitempty" yaml:"executionRules,omitempty"`
}

type ControlPlaneExecutionOperation string

const (
	ControlPlaneWorkflowUse ControlPlaneExecutionOperation = "Workflow/use"
	ControlPlaneAgentInvoke ControlPlaneExecutionOperation = "Agent/invoke"
)

type ControlPlaneExecutionRule struct {
	Effect         string                         `json:"effect" yaml:"effect"`
	Operation      ControlPlaneExecutionOperation `json:"operation" yaml:"operation"`
	Workspace      string                         `json:"workspace" yaml:"workspace"`
	ResourceName   string                         `json:"resourceName" yaml:"resourceName"`
	ResourceUID    string                         `json:"resourceUID" yaml:"resourceUID"`
	RevisionDigest string                         `json:"revisionDigest,omitempty" yaml:"revisionDigest,omitempty"`
}

// ControlPlanePolicyRule authorizes matching RuntimeApproval operations.
type ControlPlanePolicyRule struct {
	Effect                      string                              `json:"effect" yaml:"effect"`
	Resources                   []string                            `json:"resources" yaml:"resources"`
	Verbs                       []string                            `json:"verbs" yaml:"verbs"`
	Workspaces                  []string                            `json:"workspaces" yaml:"workspaces"`
	RuntimeApprovalNames        []string                            `json:"runtimeApprovalNames,omitempty" yaml:"runtimeApprovalNames,omitempty"`
	RuntimeApprovalNameSelector *RuntimeApprovalBoundedNameSelector `json:"runtimeApprovalNameSelector,omitempty" yaml:"runtimeApprovalNameSelector,omitempty"`
}

// RuntimeApprovalBoundedNameSelector matches names with one explicit prefix.
type RuntimeApprovalBoundedNameSelector struct {
	Prefix string `json:"prefix" yaml:"prefix"`
}

// ControlPlaneRoleBinding binds a standalone role to canonical identities.
type ControlPlaneRoleBinding struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ControlPlaneRoleBindingSpec `json:"spec" yaml:"spec"`
}

func (c *ControlPlaneRoleBinding) GetTypeMeta() TypeMeta      { return c.TypeMeta }
func (c *ControlPlaneRoleBinding) GetObjectMeta() *ObjectMeta { return &c.ObjectMeta }

// ControlPlaneRoleBindingSpec binds one role to authenticated principals.
type ControlPlaneRoleBindingSpec struct {
	RoleRef  LocalObjectReference      `json:"roleRef" yaml:"roleRef"`
	Subjects []ControlPlaneRoleSubject `json:"subjects" yaml:"subjects"`
}

// ControlPlaneRoleSubject identifies a principal by verified auth fields.
type ControlPlaneRoleSubject struct {
	Method  string `json:"method" yaml:"method"`
	Issuer  string `json:"issuer" yaml:"issuer"`
	Subject string `json:"subject" yaml:"subject"`
}

// Subject identifies a principal for role binding.
type Subject struct {
	Kind string `json:"kind" yaml:"kind"` // "Agent", "AgentIdentity"
	Name string `json:"name" yaml:"name"`
}

// AgentCommunicationPolicy restricts inter-agent communication.
type AgentCommunicationPolicy struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentCommunicationPolicySpec `json:"spec" yaml:"spec"`
}

func (a *AgentCommunicationPolicy) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentCommunicationPolicy) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentCommunicationPolicySpec defines communication rules.
type AgentCommunicationPolicySpec struct {
	// AllowFrom lists agents permitted to communicate.
	AllowFrom          []LocalObjectReference `json:"allowFrom,omitempty" yaml:"allowFrom,omitempty"`
	AllowFromQualified []AgentReference       `json:"allowFromQualified,omitempty" yaml:"allowFromQualified,omitempty"`

	// AllowTo lists agents that may be communicated with.
	AllowTo []LocalObjectReference `json:"allowTo,omitempty" yaml:"allowTo,omitempty"`

	// DefaultDeny blocks all communication unless explicitly allowed.
	DefaultDeny bool `json:"defaultDeny" yaml:"defaultDeny"`
}

// MemoryGrant authorizes an agent to access a memory claim.
type MemoryGrant struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       MemoryGrantSpec `json:"spec" yaml:"spec"`
}

func (m *MemoryGrant) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *MemoryGrant) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// MemoryGrantSpec defines which agent can access which memory.
type MemoryGrantSpec struct {
	AgentRef       LocalObjectReference `json:"agentRef" yaml:"agentRef"`
	MemoryClaimRef LocalObjectReference `json:"memoryClaimRef" yaml:"memoryClaimRef"`
	AccessMode     string               `json:"accessMode" yaml:"accessMode"` // "ReadWrite", "ReadOnly"
}
