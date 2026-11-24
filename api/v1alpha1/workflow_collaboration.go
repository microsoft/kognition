// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// Collaboration schema ceilings. They are the reviewed limits from the decision
// gate resolutions and are mirrored by the CRD validation markers.
const (
	CollaborationMaxMembers          = 8
	CollaborationMaxInteractions     = CollaborationMaxMembers * (CollaborationMaxMembers - 1)
	CollaborationMaxTurns            = 100
	CollaborationMaxMessages         = 500
	CollaborationMaxDelegations      = 100
	CollaborationMaxDelegationDepth  = 5
	CollaborationMaxParallelTasks    = 8
	CollaborationMaxStepTimeoutHours = 24
)

// CollaborationStrategy selects how the members of a collaboration step coordinate.
type CollaborationStrategy string

const (
	CollaborationStrategySupervisor CollaborationStrategy = "Supervisor"
	CollaborationStrategyParallel   CollaborationStrategy = "Parallel"
	CollaborationStrategyHandoff    CollaborationStrategy = "Handoff"
	CollaborationStrategyPeer       CollaborationStrategy = "Peer"
)

// CollaborationProtocol selects the inter-agent protocol profile.
type CollaborationProtocol string

const (
	// CollaborationProtocolA2A is the pinned A2A 1.0 profile.
	CollaborationProtocolA2A CollaborationProtocol = "A2A"
)

// AgentReference identifies an Agent, optionally in another namespace.
type AgentReference struct {
	// Name is the Agent name.
	Name string `json:"name" yaml:"name"`

	// Namespace is the Agent namespace. An empty value means the Workflow namespace.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// WorkflowCollaboration configures a collaboration step.
type WorkflowCollaboration struct {
	// Strategy selects the coordination strategy. Exactly the matching strategy
	// block must be present.
	Strategy CollaborationStrategy `json:"strategy" yaml:"strategy"`

	// Protocol selects the inter-agent protocol profile.
	Protocol CollaborationProtocol `json:"protocol" yaml:"protocol"`

	// Members are the participating Agents, addressed by step-local alias.
	Members []CollaborationMember `json:"members" yaml:"members"`

	// AllowedInteractions lists the delegation requests members may make. It does
	// not replace communication policy or add Workflow dependency edges.
	AllowedInteractions []CollaborationInteraction `json:"allowedInteractions,omitempty" yaml:"allowedInteractions,omitempty"`

	// Limits bounds the step. Every field is required and finite.
	Limits CollaborationLimits `json:"limits" yaml:"limits"`

	Supervisor *CollaborationSupervisor `json:"supervisor,omitempty" yaml:"supervisor,omitempty"`
	Parallel   *CollaborationParallel   `json:"parallel,omitempty" yaml:"parallel,omitempty"`
	Handoff    *CollaborationHandoff    `json:"handoff,omitempty" yaml:"handoff,omitempty"`
	Peer       *CollaborationPeer       `json:"peer,omitempty" yaml:"peer,omitempty"`
}

// CollaborationMember binds a step-local alias to an Agent.
type CollaborationMember struct {
	// Name is the stable step-local alias.
	Name string `json:"name" yaml:"name"`

	// AgentRef references the member Agent.
	AgentRef AgentReference `json:"agentRef" yaml:"agentRef"`

	// ServiceRef optionally pins one AgentService, in the Agent namespace, when
	// the Agent has several serving configurations.
	ServiceRef *LocalObjectReference `json:"serviceRef,omitempty" yaml:"serviceRef,omitempty"`
}

// CollaborationInteraction permits one member to delegate to another.
type CollaborationInteraction struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
}

// CollaborationLimits are the finite bounds of a collaboration step.
type CollaborationLimits struct {
	MaxTurns           int32 `json:"maxTurns" yaml:"maxTurns"`
	MaxMessages        int32 `json:"maxMessages" yaml:"maxMessages"`
	MaxDelegations     int32 `json:"maxDelegations" yaml:"maxDelegations"`
	MaxDelegationDepth int32 `json:"maxDelegationDepth" yaml:"maxDelegationDepth"`
	MaxParallelTasks   int32 `json:"maxParallelTasks" yaml:"maxParallelTasks"`
	MaxTokens          int64 `json:"maxTokens" yaml:"maxTokens"`
}

// CollaborationSupervisor configures the Supervisor strategy.
type CollaborationSupervisor struct {
	// Coordinator is the member alias that delegates and produces the final artifact.
	Coordinator string `json:"coordinator" yaml:"coordinator"`
}

// CollaborationParallel configures the Parallel strategy.
type CollaborationParallel struct {
	// Branches are the members that run concurrently.
	Branches []CollaborationBranch `json:"branches" yaml:"branches"`

	// Synthesizer optionally combines the branch results. It must not be a branch.
	Synthesizer string `json:"synthesizer,omitempty" yaml:"synthesizer,omitempty"`
}

// CollaborationBranch is one parallel branch.
type CollaborationBranch struct {
	// Member is the member alias that runs the branch.
	Member string `json:"member" yaml:"member"`

	// Optional branches may fail without failing the step. They are recorded in
	// the result manifest.
	Optional bool `json:"optional,omitempty" yaml:"optional,omitempty"`
}

// CollaborationHandoff configures the Handoff strategy.
type CollaborationHandoff struct {
	// EntryMember is the first owner.
	EntryMember string `json:"entryMember" yaml:"entryMember"`

	// MaxHandoffs bounds ownership transfers. It cannot exceed limits.maxDelegations.
	MaxHandoffs int32 `json:"maxHandoffs" yaml:"maxHandoffs"`
}

// CollaborationPeer configures the Peer strategy.
type CollaborationPeer struct {
	// Finalizer is the only member that can complete the step.
	Finalizer string `json:"finalizer" yaml:"finalizer"`
}
