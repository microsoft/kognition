// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workspace provides multi-tenant isolation (analogous to K8s Namespace).
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkspaceSpec   `json:"spec,omitempty"`
	Status            WorkspaceStatus `json:"status,omitempty"`
}

// WorkspaceSpec defines workspace configuration.
type WorkspaceSpec struct {
	// DisplayName is a human-friendly name for the workspace.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// RequireSignedArtifacts enables supply-chain signature verification
	// for Tool and Model resources admitted into this workspace.
	// +optional
	RequireSignedArtifacts bool `json:"requireSignedArtifacts,omitempty"`

	// Retention configures data retention policies for observability data.
	// +optional
	Retention *RetentionPolicy `json:"retention,omitempty"`
}

// RetentionPolicy defines how long observability data is kept per workspace.
type RetentionPolicy struct {
	// TraceTTL is the duration to retain trace spans (e.g., "720h" = 30 days).
	// Zero means retain indefinitely.
	// +optional
	TraceTTL string `json:"traceTTL,omitempty"`

	// AuditLogTTL is the duration to retain audit log entries.
	// +optional
	AuditLogTTL string `json:"auditLogTTL,omitempty"`

	// CostRecordTTL is the duration to retain cost records.
	// +optional
	CostRecordTTL string `json:"costRecordTTL,omitempty"`

	// EventTTL is the duration to retain instance events.
	// +optional
	EventTTL string `json:"eventTTL,omitempty"`
}

// WorkspaceStatus describes the observed state of a Workspace.
type WorkspaceStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`
}

type WorkloadIdentityStatus struct {
	AgentUID    string      `json:"agentUID,omitempty"`
	SPIFFEID    string      `json:"spiffeID,omitempty"`
	TrustDomain string      `json:"trustDomain,omitempty"`
	NotBefore   metav1.Time `json:"notBefore,omitempty"`
	ExpiresAt   metav1.Time `json:"expiresAt,omitempty"`
	// +kubebuilder:validation:Minimum=0
	SourceGeneration int64  `json:"sourceGeneration,omitempty"`
	Ready            bool   `json:"ready"`
	Reason           string `json:"reason,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspaces.
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentIdentity provides workload identity (SPIFFE) for an agent.
type AgentIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentIdentitySpec   `json:"spec,omitempty"`
	Status            AgentIdentityStatus `json:"status,omitempty"`
}

// AgentIdentitySpec defines the identity configuration.
type AgentIdentitySpec struct {
	// SPIFFEID is the workload identity URI.
	// +optional
	SPIFFEID string `json:"spiffeID,omitempty"`

	// AgentRef references the agent this identity belongs to.
	AgentRef LocalObjectReference `json:"agentRef"`
}

// AgentIdentityStatus describes the observed state of an identity.
type AgentIdentityStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	WorkloadIdentity *WorkloadIdentityStatus `json:"workloadIdentity,omitempty"`
}

// +kubebuilder:object:root=true

// AgentIdentityList contains a list of AgentIdentities.
type AgentIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentIdentity `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRole defines a set of permissions for agents.
type AgentRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Rules defines the permissions in this role.
	Rules []PolicyRule `json:"rules"`
}

// PolicyRule defines a set of permitted actions.
type PolicyRule struct {
	// Resources lists the resource kinds this rule applies to.
	Resources []string `json:"resources"`

	// Verbs lists the permitted actions (get, list, create, update, delete).
	Verbs []string `json:"verbs"`
}

// +kubebuilder:object:root=true

// AgentRoleList contains a list of AgentRoles.
type AgentRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRole `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.roleRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentRoleBinding binds an AgentRole to agent identities.
type AgentRoleBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentRoleBindingSpec `json:"spec,omitempty"`
}

// AgentRoleBindingSpec binds a role to subjects.
type AgentRoleBindingSpec struct {
	// RoleRef references the AgentRole to bind.
	RoleRef LocalObjectReference `json:"roleRef"`

	// Subjects lists the principals this binding applies to.
	Subjects []Subject `json:"subjects"`
}

// Subject identifies a principal for role binding.
type Subject struct {
	// Kind is the principal kind ("Agent" or "AgentIdentity").
	// +kubebuilder:validation:Enum=Agent;AgentIdentity
	Kind string `json:"kind"`

	// Name is the name of the principal.
	Name string `json:"name"`
}

// +kubebuilder:object:root=true

// AgentRoleBindingList contains a list of AgentRoleBindings.
type AgentRoleBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRoleBinding `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Default Deny",type=boolean,JSONPath=`.spec.defaultDeny`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentCommunicationPolicy restricts inter-agent communication.
type AgentCommunicationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentCommunicationPolicySpec `json:"spec"`
}

// AgentCommunicationPolicySpec defines communication rules.
type AgentCommunicationPolicySpec struct {
	AllowFrom   []LocalObjectReference `json:"allowFrom,omitempty"`
	AllowTo     []LocalObjectReference `json:"allowTo,omitempty"`
	DefaultDeny bool                   `json:"defaultDeny"`
}

// +kubebuilder:object:root=true

// AgentCommunicationPolicyList contains a list of AgentCommunicationPolicies.
type AgentCommunicationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentCommunicationPolicy `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Memory Claim",type=string,JSONPath=`.spec.memoryClaimRef.name`
// +kubebuilder:printcolumn:name="Access Mode",type=string,JSONPath=`.spec.accessMode`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryGrant authorizes an agent to access a memory claim.
type MemoryGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemoryGrantSpec `json:"spec"`
}

// MemoryGrantSpec defines which agent can access which memory.
type MemoryGrantSpec struct {
	AgentRef       LocalObjectReference `json:"agentRef"`
	MemoryClaimRef LocalObjectReference `json:"memoryClaimRef"`
	// +kubebuilder:validation:Enum=ReadWrite;ReadOnly
	AccessMode string `json:"accessMode"`
}

// +kubebuilder:object:root=true

// MemoryGrantList contains a list of MemoryGrants.
type MemoryGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workspace{}, &WorkspaceList{})
	SchemeBuilder.Register(&AgentIdentity{}, &AgentIdentityList{})
	SchemeBuilder.Register(&AgentRole{}, &AgentRoleList{})
	SchemeBuilder.Register(&AgentRoleBinding{}, &AgentRoleBindingList{})
	SchemeBuilder.Register(&AgentCommunicationPolicy{}, &AgentCommunicationPolicyList{})
	SchemeBuilder.Register(&MemoryGrant{}, &MemoryGrantList{})
}
