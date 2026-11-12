// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "fmt"

// KindRegistry maps Kind strings to zero-value constructors.
var KindRegistry = map[string]func() Object{
	"Agent":                      func() Object { return &Agent{} },
	"AgentInstance":              func() Object { return &AgentInstance{} },
	"AgentJob":                   func() Object { return &AgentJob{} },
	"Workflow":                   func() Object { return &Workflow{} },
	"WorkflowRun":                func() Object { return &WorkflowRun{} },
	"Memory":                     func() Object { return &Memory{} },
	"MemoryClass":                func() Object { return &MemoryClass{} },
	"MemoryClaim":                func() Object { return &MemoryClaim{} },
	"MemoryVolume":               func() Object { return &MemoryVolume{} },
	"MemoryRecord":               func() Object { return &MemoryRecord{} },
	"KnowledgeBaseClass":         func() Object { return &KnowledgeBaseClass{} },
	"KnowledgeBase":              func() Object { return &KnowledgeBase{} },
	"KnowledgeBaseClaim":         func() Object { return &KnowledgeBaseClaim{} },
	"KnowledgeBaseIndexRun":      func() Object { return &KnowledgeBaseIndexRun{} },
	"KnowledgeBaseIndexSchedule": func() Object { return &KnowledgeBaseIndexSchedule{} },
	"ToolClass":                  func() Object { return &ToolClass{} },
	"Tool":                       func() Object { return &Tool{} },
	"ToolInvocationRun":          func() Object { return &ToolInvocationRun{} },
	"ToolGrant":                  func() Object { return &ToolGrant{} },
	"ModelClass":                 func() Object { return &ModelClass{} },
	"Model":                      func() Object { return &Model{} },
	"ModelInvocationProfile":     func() Object { return &ModelInvocationProfile{} },
	"ModelGrant":                 func() Object { return &ModelGrant{} },
	"AgentPolicy":                func() Object { return &AgentPolicy{} },
	"CognitiveAdmissionPolicy":   func() Object { return &CognitiveAdmissionPolicy{} },
	"CognitiveQuota":             func() Object { return &CognitiveQuota{} },
	"CognitiveLimitRange":        func() Object { return &CognitiveLimitRange{} },
	"Workspace":                  func() Object { return &Workspace{} },
	"AgentIdentity":              func() Object { return &AgentIdentity{} },
	"AgentRole":                  func() Object { return &AgentRole{} },
	"AgentRoleBinding":           func() Object { return &AgentRoleBinding{} },
	"ControlPlaneRole":           func() Object { return &ControlPlaneRole{} },
	"ControlPlaneRoleBinding":    func() Object { return &ControlPlaneRoleBinding{} },
	"AgentCommunicationPolicy":   func() Object { return &AgentCommunicationPolicy{} },
	"MemoryGrant":                func() Object { return &MemoryGrant{} },
	"AutonomyClass":              func() Object { return &AutonomyClass{} },
	"TrustProfile":               func() Object { return &TrustProfile{} },
	"AutonomyReview":             func() Object { return &AutonomyReview{} },
	"RuntimeApproval":            func() Object { return &RuntimeApproval{} },
	"AgentService":               func() Object { return &AgentService{} },
	"IntentGateway":              func() Object { return &IntentGateway{} },
	"Prompt":                     func() Object { return &Prompt{} },
	"SecretRef":                  func() Object { return &SecretRef{} },
	"Solution":                   func() Object { return &Solution{} },
	"Evaluation":                 func() Object { return &Evaluation{} },
	"EvaluationRun":              func() Object { return &EvaluationRun{} },
	"EvaluationDataset":          func() Object { return &EvaluationDataset{} },
	"CrossNamespaceGrant":        func() Object { return &CrossNamespaceGrant{} },
	"AgentAutoscaler":            func() Object { return &AgentAutoscaler{} },
	"ModelAutoscaler":            func() Object { return &ModelAutoscaler{} },
	"ApprovalDecision":           func() Object { return &ApprovalDecision{} },
}

// NewObject creates a new zero-value resource by kind name.
func NewObject(kind string) (Object, error) {
	ctor, ok := KindRegistry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown kind: %s", kind)
	}
	return ctor(), nil
}

// RegisteredKinds returns all registered kind names.
func RegisteredKinds() []string {
	kinds := make([]string, 0, len(KindRegistry))
	for k := range KindRegistry {
		kinds = append(kinds, k)
	}
	return kinds
}

// IsGenericResourceKind reports whether a registered API type is safe to
// expose through unauthenticated standalone generic CRUD surfaces.
func IsGenericResourceKind(kind string) bool {
	return kind != "RuntimeApproval" && kind != "ControlPlaneRole" && kind != "ControlPlaneRoleBinding"
}

// IsKubernetesNativeKind reports whether a canonical kind has a Kubernetes CRD.
// Kubernetes delegates RuntimeApproval authorization to SubjectAccessReview;
// ControlPlaneRole policy is the equivalent standalone-only authority.
func IsKubernetesNativeKind(kind string) bool {
	return kind != "ControlPlaneRole" && kind != "ControlPlaneRoleBinding"
}
