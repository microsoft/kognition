// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// AgentService exposes an agent for external or inter-agent communication.
type AgentService struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentServiceSpec   `json:"spec" yaml:"spec"`
	Status     AgentServiceStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *AgentService) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentService) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentServiceSpec defines the service routing configuration.
type AgentServiceSpec struct {
	// AgentRef references the Agent this service exposes.
	AgentRef LocalObjectReference `json:"agentRef" yaml:"agentRef"`

	// RevisionPin optionally pins traffic to a specific Agent revision.
	// When nil, traffic is routed to the latest revision.
	RevisionPin *int64 `json:"revisionPin,omitempty" yaml:"revisionPin,omitempty"`

	// LoadBalancingStrategy defines how traffic is distributed across instances.
	// Supported values: "round-robin", "random". Defaults to "round-robin".
	LoadBalancingStrategy string `json:"loadBalancingStrategy,omitempty" yaml:"loadBalancingStrategy,omitempty"`

	// Selector matches agents by label.
	Selector map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`

	// Port is the service port.
	Port int32 `json:"port,omitempty" yaml:"port,omitempty"`
}

// AgentServiceStatus describes the observed state of an AgentService.
type AgentServiceStatus struct {
	Phase          Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Endpoint       string      `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	ActiveRevision int64       `json:"activeRevision,omitempty" yaml:"activeRevision,omitempty"`
	ReadyInstances int32       `json:"readyInstances,omitempty" yaml:"readyInstances,omitempty"`
	Conditions     []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// IntentGateway routes external user intents to the appropriate agent or workflow.
type IntentGateway struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       IntentGatewaySpec   `json:"spec" yaml:"spec"`
	Status     IntentGatewayStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (i *IntentGateway) GetTypeMeta() TypeMeta      { return i.TypeMeta }
func (i *IntentGateway) GetObjectMeta() *ObjectMeta { return &i.ObjectMeta }

// IntentGatewaySpec defines how intents are routed.
type IntentGatewaySpec struct {
	// Protocol selects the externally exposed goal submission protocol. The
	// current implementation supports "http"; an omitted value defaults to it.
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`

	// AuthConfig optionally protects goal submission.
	AuthConfig *IntentGatewayAuthConfig `json:"authConfig,omitempty" yaml:"authConfig,omitempty"`

	// Routes defines intent-to-agent routing rules.
	Routes []IntentRoute `json:"routes" yaml:"routes"`
}

// IntentGatewayAuthConfig configures client authentication for an IntentGateway.
type IntentGatewayAuthConfig struct {
	// Mode is "none" or "apiKey". An empty value is equivalent to "none".
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// APIKeySecretRef resolves the expected key when Mode is "apiKey".
	APIKeySecretRef *LocalObjectReference `json:"apiKeySecretRef,omitempty" yaml:"apiKeySecretRef,omitempty"`

	// Header names the request header containing the API key. Defaults to
	// X-Kognition-API-Key.
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
}

// IntentRoute maps an intent pattern to an Agent, AgentService, or Workflow.
type IntentRoute struct {
	// Pattern is a match expression for incoming intents.
	Pattern string `json:"pattern" yaml:"pattern"`

	// TargetRef references the resource to handle matching intents.
	TargetRef LocalObjectReference `json:"targetRef" yaml:"targetRef"`

	// TargetKind specifies the kind ("Agent", "AgentService", or "Workflow").
	TargetKind string `json:"targetKind" yaml:"targetKind"`
}

// IntentGatewayStatus describes the observed state.
type IntentGatewayStatus struct {
	Phase      Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Endpoint   string      `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}
