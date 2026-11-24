// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// CollaborationStrategy selects how the members of a collaboration step coordinate.
// +kubebuilder:validation:Enum=Supervisor;Parallel;Handoff;Peer
type CollaborationStrategy string

const (
	CollaborationStrategySupervisor CollaborationStrategy = "Supervisor"
	CollaborationStrategyParallel   CollaborationStrategy = "Parallel"
	CollaborationStrategyHandoff    CollaborationStrategy = "Handoff"
	CollaborationStrategyPeer       CollaborationStrategy = "Peer"
)

// CollaborationProtocol selects the inter-agent protocol profile.
// +kubebuilder:validation:Enum=A2A
type CollaborationProtocol string

const (
	// CollaborationProtocolA2A is the pinned A2A 1.0 profile.
	CollaborationProtocolA2A CollaborationProtocol = "A2A"
)

// AgentReference identifies an Agent, optionally in another namespace.
type AgentReference struct {
	// Name is the Agent name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace is the Agent namespace. An empty value means the Workflow namespace.
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// WorkflowCollaboration configures a collaboration step.
// +kubebuilder:validation:XValidation:rule="(self.strategy == 'Supervisor') == has(self.supervisor)",message="supervisor is required exactly when strategy is Supervisor"
// +kubebuilder:validation:XValidation:rule="(self.strategy == 'Parallel') == has(self.parallel)",message="parallel is required exactly when strategy is Parallel"
// +kubebuilder:validation:XValidation:rule="(self.strategy == 'Handoff') == has(self.handoff)",message="handoff is required exactly when strategy is Handoff"
// +kubebuilder:validation:XValidation:rule="(self.strategy == 'Peer') == has(self.peer)",message="peer is required exactly when strategy is Peer"
type WorkflowCollaboration struct {
	// Strategy selects the coordination strategy. Exactly the matching strategy
	// block must be present.
	Strategy CollaborationStrategy `json:"strategy"`

	// Protocol selects the inter-agent protocol profile.
	Protocol CollaborationProtocol `json:"protocol"`

	// Members are the participating Agents, addressed by step-local alias.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=8
	// +listType=map
	// +listMapKey=name
	Members []CollaborationMember `json:"members"`

	// AllowedInteractions lists the delegation requests members may make. It does
	// not replace communication policy or add Workflow dependency edges.
	// +kubebuilder:validation:MaxItems=56
	// +optional
	AllowedInteractions []CollaborationInteraction `json:"allowedInteractions,omitempty"`

	// Limits bounds the step. Every field is required and finite.
	Limits CollaborationLimits `json:"limits"`

	// +optional
	Supervisor *CollaborationSupervisor `json:"supervisor,omitempty"`
	// +optional
	Parallel *CollaborationParallel `json:"parallel,omitempty"`
	// +optional
	Handoff *CollaborationHandoff `json:"handoff,omitempty"`
	// +optional
	Peer *CollaborationPeer `json:"peer,omitempty"`
}

// CollaborationMember binds a step-local alias to an Agent.
type CollaborationMember struct {
	// Name is the stable step-local alias.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	Name string `json:"name"`

	// AgentRef references the member Agent.
	AgentRef AgentReference `json:"agentRef"`

	// ServiceRef optionally pins one AgentService, in the Agent namespace, when
	// the Agent has several serving configurations.
	// +optional
	ServiceRef *LocalObjectReference `json:"serviceRef,omitempty"`
}

// CollaborationInteraction permits one member to delegate to another.
type CollaborationInteraction struct {
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	From string `json:"from"`
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	To string `json:"to"`
}

// CollaborationLimits are the finite bounds of a collaboration step.
type CollaborationLimits struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxTurns int32 `json:"maxTurns"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	MaxMessages int32 `json:"maxMessages"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxDelegations int32 `json:"maxDelegations"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	MaxDelegationDepth int32 `json:"maxDelegationDepth"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxParallelTasks int32 `json:"maxParallelTasks"`
	// +kubebuilder:validation:Minimum=1
	MaxTokens int64 `json:"maxTokens"`
}

// CollaborationSupervisor configures the Supervisor strategy.
type CollaborationSupervisor struct {
	// Coordinator is the member alias that delegates and produces the final artifact.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	Coordinator string `json:"coordinator"`
}

// CollaborationParallel configures the Parallel strategy.
type CollaborationParallel struct {
	// Branches are the members that run concurrently.
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=8
	Branches []CollaborationBranch `json:"branches"`

	// Synthesizer optionally combines the branch results. It must not be a branch.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	// +optional
	Synthesizer string `json:"synthesizer,omitempty"`
}

// CollaborationBranch is one parallel branch.
type CollaborationBranch struct {
	// Member is the member alias that runs the branch.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	Member string `json:"member"`

	// Optional branches may fail without failing the step. They are recorded in
	// the result manifest.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// CollaborationHandoff configures the Handoff strategy.
type CollaborationHandoff struct {
	// EntryMember is the first owner.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	EntryMember string `json:"entryMember"`

	// MaxHandoffs bounds ownership transfers. It cannot exceed limits.maxDelegations.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxHandoffs int32 `json:"maxHandoffs"`
}

// CollaborationPeer configures the Peer strategy.
type CollaborationPeer struct {
	// Finalizer is the only member that can complete the step.
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`
	Finalizer string `json:"finalizer"`
}
