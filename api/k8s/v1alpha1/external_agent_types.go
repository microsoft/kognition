// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ExternalAgentProtocol identifies the protocol used to reach an external agent.
// +kubebuilder:validation:Enum=A2A
type ExternalAgentProtocol string

// ExternalAgentGrantVerb identifies an action permitted by an ExternalAgentGrant.
// +kubebuilder:validation:Enum=invoke
type ExternalAgentGrantVerb string

const (
	ExternalAgentProtocolA2A     ExternalAgentProtocol  = "A2A"
	ExternalAgentGrantVerbInvoke ExternalAgentGrantVerb = "invoke"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="Card",type=string,JSONPath=`.spec.card.digest`,priority=1
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExternalAgent is a remote agent reachable over A2A that a Workflow can use as
// a collaboration member or step target. It is a trust boundary: it is never
// run, scaled or metered by the platform.
type ExternalAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExternalAgentSpec   `json:"spec"`
	Status            ExternalAgentStatus `json:"status,omitempty"`
}

// ExternalAgentSpec defines the remote endpoint, its pinned card and limits.
// Protocol, endpoint, card digest and credential reference are immutable; a new
// endpoint, card or credential is a new resource.
// +kubebuilder:validation:XValidation:rule="self.protocol == oldSelf.protocol && self.endpoint == oldSelf.endpoint && self.card.digest == oldSelf.card.digest && self.credentialRef.name == oldSelf.credentialRef.name",message="protocol, endpoint, card.digest and credentialRef are immutable"
type ExternalAgentSpec struct {
	// Protocol is required. Only A2A is supported in v1.
	Protocol ExternalAgentProtocol `json:"protocol"`

	// Endpoint is a static DNS host:port. TLS is always used. IP literals,
	// schemes, userinfo and paths are rejected.
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=260
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?(\.[A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)+\.?:[0-9]{1,5}$`
	Endpoint string `json:"endpoint"`

	// Card pins the Agent Card the platform will accept.
	Card ExternalAgentCard `json:"card"`

	// CredentialRef names a SecretRef in the same namespace holding a bearer token.
	CredentialRef LocalObjectReference `json:"credentialRef"`

	// Limits bound every task sent to the agent. Unset values take defaults.
	// +optional
	Limits *ExternalAgentLimits `json:"limits,omitempty"`
}

// ExternalAgentCard pins the canonical Agent Card.
type ExternalAgentCard struct {
	// Digest is "sha256:" plus the lowercase hex SHA-256 of the RFC 8785
	// canonical form of the Agent Card with its signatures removed.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`

	// Skills optionally restricts the card skills the platform will use.
	// +kubebuilder:validation:MaxItems=32
	// +listType=set
	// +optional
	Skills []string `json:"skills,omitempty"`
}

// ExternalAgentLimits bounds one task.
type ExternalAgentLimits struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=131072
	// +optional
	MaxTokensPerTask int64 `json:"maxTokensPerTask,omitempty"`
	// TaskTimeout is a Go duration between 1s and 30m.
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +optional
	TaskTimeout string `json:"taskTimeout,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1048576
	// +optional
	MaxRequestBytes int64 `json:"maxRequestBytes,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4194304
	// +optional
	MaxResponseBytes int64 `json:"maxResponseBytes,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=16
	// +optional
	MaxConcurrentTasks int32 `json:"maxConcurrentTasks,omitempty"`
}

// ExternalAgentStatus is the observed state of an ExternalAgent.
type ExternalAgentStatus struct {
	// ObservedCardDigest is the digest of the card last fetched from the endpoint.
	// +optional
	ObservedCardDigest string `json:"observedCardDigest,omitempty"`

	// ObservedGeneration is the spec generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are Ready, CardPinned, Reachable and EgressAllowed.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// ExternalAgentList contains a list of ExternalAgents.
type ExternalAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalAgent `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="External Agent",type=string,JSONPath=`.spec.externalAgentRef.name`
// +kubebuilder:printcolumn:name="Workflow",type=string,JSONPath=`.spec.workflowRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ExternalAgentGrant authorizes Workflows to use an ExternalAgent. There is no
// implicit access.
type ExternalAgentGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ExternalAgentGrantSpec `json:"spec"`
}

// ExternalAgentGrantSpec selects which Workflows may use which ExternalAgent.
// +kubebuilder:validation:XValidation:rule="has(self.workflowRef) != has(self.workflowSelector)",message="exactly one of workflowRef and workflowSelector is required"
type ExternalAgentGrantSpec struct {
	// ExternalAgentRef is the ExternalAgent in the grant's namespace.
	// +kubebuilder:validation:XValidation:rule="size(self.name) > 0",message="externalAgentRef.name must not be empty"
	ExternalAgentRef LocalObjectReference `json:"externalAgentRef"`

	// WorkflowRef directly names a Workflow.
	// +optional
	WorkflowRef *LocalObjectReference `json:"workflowRef,omitempty"`

	// WorkflowSelector selects Workflows by label (matchLabels semantics).
	// +kubebuilder:validation:MinProperties=1
	// +optional
	WorkflowSelector map[string]string `json:"workflowSelector,omitempty"`

	// Verbs is the non-empty set of granted actions; only invoke exists.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Verbs []ExternalAgentGrantVerb `json:"verbs"`

	// Conditions optionally time-bounds the grant.
	// +optional
	Conditions *ExternalAgentGrantConditions `json:"conditions,omitempty"`
}

// ExternalAgentGrantConditions time-bounds a grant.
type ExternalAgentGrantConditions struct {
	// +kubebuilder:validation:Format=date-time
	// +optional
	ValidFrom string `json:"validFrom,omitempty"`
	// +kubebuilder:validation:Format=date-time
	// +optional
	ValidUntil string `json:"validUntil,omitempty"`
}

// +kubebuilder:object:root=true

// ExternalAgentGrantList contains a list of ExternalAgentGrants.
type ExternalAgentGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ExternalAgentGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ExternalAgent{}, &ExternalAgentList{}, &ExternalAgentGrant{}, &ExternalAgentGrantList{})
}
