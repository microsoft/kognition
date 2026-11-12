// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestSchemeRegistration verifies that all CRD types are properly registered.
func TestSchemeRegistration(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	tests := []struct {
		obj     runtime.Object
		gvk     schema.GroupVersionKind
		listObj runtime.Object
		listGVK schema.GroupVersionKind
	}{
		{
			obj:     &Agent{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			listObj: &AgentList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentList"},
		},
		{
			obj:     &AgentInstance{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentInstance"},
			listObj: &AgentInstanceList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentInstanceList"},
		},
		{
			obj:     &Tool{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Tool"},
			listObj: &ToolList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolList"},
		},
		{
			obj:     &ToolClass{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolClass"},
			listObj: &ToolClassList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolClassList"},
		},
		{
			obj:     &ModelClass{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ModelClass"},
			listObj: &ModelClassList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ModelClassList"},
		},
		{
			obj:     &ModelAutoscaler{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ModelAutoscaler"},
			listObj: &ModelAutoscalerList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ModelAutoscalerList"},
		},
		{
			obj:     &MemoryClass{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "MemoryClass"},
			listObj: &MemoryClassList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "MemoryClassList"},
		},
		{
			obj:     &Workflow{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			listObj: &WorkflowList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "WorkflowList"},
		},
		{
			obj:     &WorkflowRun{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "WorkflowRun"},
			listObj: &WorkflowRunList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "WorkflowRunList"},
		},
		{
			obj:     &Evaluation{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Evaluation"},
			listObj: &EvaluationList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "EvaluationList"},
		},
		{
			obj:     &EvaluationRun{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "EvaluationRun"},
			listObj: &EvaluationRunList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "EvaluationRunList"},
		},
		{
			obj:     &AgentPolicy{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentPolicy"},
			listObj: &AgentPolicyList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentPolicyList"},
		},
		{
			obj:     &CognitiveAdmissionPolicy{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "CognitiveAdmissionPolicy"},
			listObj: &CognitiveAdmissionPolicyList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "CognitiveAdmissionPolicyList"},
		},
		{
			obj:     &CognitiveQuota{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "CognitiveQuota"},
			listObj: &CognitiveQuotaList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "CognitiveQuotaList"},
		},
		{
			obj:     &CognitiveLimitRange{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "CognitiveLimitRange"},
			listObj: &CognitiveLimitRangeList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "CognitiveLimitRangeList"},
		},
		{
			obj:     &Workspace{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workspace"},
			listObj: &WorkspaceList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "WorkspaceList"},
		},
		{
			obj:     &AgentIdentity{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentIdentity"},
			listObj: &AgentIdentityList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentIdentityList"},
		},
		{
			obj:     &AgentRole{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentRole"},
			listObj: &AgentRoleList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentRoleList"},
		},
		{
			obj:     &AgentRoleBinding{},
			gvk:     schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentRoleBinding"},
			listObj: &AgentRoleBindingList{},
			listGVK: schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentRoleBindingList"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.gvk.Kind, func(t *testing.T) {
			gvks, _, err := s.ObjectKinds(tt.obj)
			if err != nil {
				t.Fatalf("ObjectKinds(%T) error = %v", tt.obj, err)
			}
			found := false
			for _, gvk := range gvks {
				if gvk == tt.gvk {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected GVK %v for %T, got %v", tt.gvk, tt.obj, gvks)
			}
		})

		t.Run(tt.listGVK.Kind, func(t *testing.T) {
			gvks, _, err := s.ObjectKinds(tt.listObj)
			if err != nil {
				t.Fatalf("ObjectKinds(%T) error = %v", tt.listObj, err)
			}
			found := false
			for _, gvk := range gvks {
				if gvk == tt.listGVK {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected GVK %v for %T, got %v", tt.listGVK, tt.listObj, gvks)
			}
		})
	}
}

// TestRuntimeObjectInterface verifies all types implement runtime.Object via DeepCopyObject.
func TestRuntimeObjectInterface(t *testing.T) {
	objects := []runtime.Object{
		&Agent{},
		&AgentList{},
		&AgentInstance{},
		&AgentInstanceList{},
		&Tool{},
		&ToolList{},
		&ToolClass{},
		&ToolClassList{},
		&ModelClass{},
		&ModelClassList{},
		&ModelAutoscaler{},
		&ModelAutoscalerList{},
		&MemoryClass{},
		&MemoryClassList{},
		&Workflow{},
		&WorkflowList{},
		&WorkflowRun{},
		&WorkflowRunList{},
		&Evaluation{},
		&EvaluationList{},
		&AgentPolicy{},
		&AgentPolicyList{},
		&CognitiveAdmissionPolicy{},
		&CognitiveAdmissionPolicyList{},
		&CognitiveQuota{},
		&CognitiveQuotaList{},
		&CognitiveLimitRange{},
		&CognitiveLimitRangeList{},
		&Workspace{},
		&WorkspaceList{},
		&AgentIdentity{},
		&AgentIdentityList{},
		&AgentRole{},
		&AgentRoleList{},
		&AgentRoleBinding{},
		&AgentRoleBindingList{},
	}

	for _, obj := range objects {
		t.Run(obj.GetObjectKind().GroupVersionKind().Kind, func(t *testing.T) {
			copy := obj.DeepCopyObject()
			if copy == nil {
				t.Errorf("DeepCopyObject() returned nil for %T", obj)
			}
		})
	}
}

// TestAgentJSONRoundtrip verifies Agent serializes and deserializes correctly.
func TestAgentJSONRoundtrip(t *testing.T) {
	replicas := int32(3)
	maxTokens := int64(4096)
	agent := &Agent{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "Agent",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: AgentSpec{
			ModelRef:     LocalObjectReference{Name: "gpt-4o"},
			Instructions: AgentInstructions{Inline: "You are a helpful assistant."},
			Tools: []AgentToolRef{
				{Ref: LocalObjectReference{Name: "web-search"}, Required: true},
			},
			Memory: &AgentMemorySpec{
				Claims: []LocalObjectReference{{Name: "episodic-mem"}},
			},
			Runtime: &AgentRuntimeSpec{
				Class:     "gpu",
				Timeout:   "30s",
				MaxTokens: &maxTokens,
			},
			Companions:          []LocalObjectReference{{Name: "reviewer-agent"}},
			KnowledgeBaseClaims: []KnowledgeBaseClaimRef{{Name: "docs-kb-claim", Required: true}},
			AutonomyClassRef:    &LocalObjectReference{Name: "supervised"},
			Replicas:            &replicas,
			Probes: &AgentProbeSpec{
				HealthProbe:    &AgentHealthProbe{IntervalSeconds: 10, TimeoutSeconds: 5, FailureThreshold: 3},
				ReadinessProbe: &AgentReadinessProbe{IntervalSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 2},
			},
			RestartLadder: &RestartLadder{
				Steps: []RestartLadderStep{
					{Action: "restart", MaxAttempts: 3},
					{Action: "reschedule", MaxAttempts: 5},
				},
			},
		},
		Status: AgentStatus{
			Phase:         "Running",
			Replicas:      3,
			ReadyReplicas: 2,
			RevisionCount: 5,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllReplicasReady"},
			},
		},
	}

	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got Agent
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Name != "test-agent" {
		t.Errorf("Name = %q, want %q", got.Name, "test-agent")
	}
	if got.Spec.ModelRef.Name != "gpt-4o" {
		t.Errorf("ModelRef.Name = %q, want %q", got.Spec.ModelRef.Name, "gpt-4o")
	}
	if got.Spec.Instructions.Inline != "You are a helpful assistant." {
		t.Errorf("Instructions.Inline = %q, want %q", got.Spec.Instructions.Inline, "You are a helpful assistant.")
	}
	if len(got.Spec.Tools) != 1 || got.Spec.Tools[0].Ref.Name != "web-search" || !got.Spec.Tools[0].Required {
		t.Errorf("Tools = %+v, unexpected", got.Spec.Tools)
	}
	if got.Spec.Memory == nil || len(got.Spec.Memory.Claims) != 1 {
		t.Errorf("Memory.Claims unexpected: %+v", got.Spec.Memory)
	}
	if got.Spec.Runtime == nil || got.Spec.Runtime.Class != "gpu" || *got.Spec.Runtime.MaxTokens != 4096 {
		t.Errorf("Runtime unexpected: %+v", got.Spec.Runtime)
	}
	if *got.Spec.Replicas != 3 {
		t.Errorf("Replicas = %d, want 3", *got.Spec.Replicas)
	}
	if got.Status.Phase != "Running" || got.Status.ReadyReplicas != 2 {
		t.Errorf("Status = %+v, unexpected", got.Status)
	}
}

// TestAgentInstanceJSONRoundtrip verifies AgentInstance serialization.
func TestAgentInstanceJSONRoundtrip(t *testing.T) {
	now := metav1.Now()
	inst := &AgentInstance{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "AgentInstance",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent-abc123",
			Namespace: "default",
		},
		Spec: AgentInstanceSpec{
			AgentRef:       LocalObjectReference{Name: "test-agent"},
			Revision:       7,
			SessionContext: map[string]string{"user": "alice", "org": "acme"},
		},
		Status: AgentInstanceStatus{
			Phase:        "Running",
			SessionID:    "sess-xyz",
			StartTime:    &now,
			TokenUsage:   &TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			ToolCalls:    3,
			RestartCount: 1,
		},
	}

	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got AgentInstance
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.AgentRef.Name != "test-agent" {
		t.Errorf("AgentRef.Name = %q, want %q", got.Spec.AgentRef.Name, "test-agent")
	}
	if got.Spec.Revision != 7 {
		t.Errorf("Revision = %d, want 7", got.Spec.Revision)
	}
	if got.Status.SessionID != "sess-xyz" {
		t.Errorf("SessionID = %q, want %q", got.Status.SessionID, "sess-xyz")
	}
	if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 150 {
		t.Errorf("TokenUsage unexpected: %+v", got.Status.TokenUsage)
	}
	if got.Status.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", got.Status.ToolCalls)
	}
}

// TestToolJSONRoundtrip verifies Tool and ToolClass serialization.
func TestToolJSONRoundtrip(t *testing.T) {
	tool := &Tool{
		TypeMeta: metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "Tool"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-search",
			Namespace: "agents",
		},
		Spec: ToolSpec{
			ClassName:   "mcp-class",
			Protocol:    ToolProtocolMCP,
			Description: "Search the web",
			Endpoint:    "http://tool-svc:8080",
			Metadata:    map[string]string{"maxResults": "10"},
			Sandbox: &ToolSandboxSpec{
				CPUMilli:      500,
				MemoryMB:      256,
				Timeout:       "30s",
				NetworkPolicy: "egress-only",
			},
			SecretRefs: []ToolSecretRef{{Name: "tool-creds", Key: "password"}},
		},
		Status: ToolStatus{
			Phase: "Ready",
			Ready: true,
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got Tool
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.Protocol != ToolProtocolMCP {
		t.Errorf("Protocol = %q, want %q", got.Spec.Protocol, ToolProtocolMCP)
	}
	if got.Spec.Sandbox == nil || got.Spec.Sandbox.CPUMilli != 500 {
		t.Errorf("Sandbox unexpected: %+v", got.Spec.Sandbox)
	}
	if !got.Status.Ready {
		t.Error("Status.Ready = false, want true")
	}
}

// TestToolClassJSONRoundtrip verifies ToolClass serialization.
func TestToolClassJSONRoundtrip(t *testing.T) {
	tc := &ToolClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "ToolClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-class"},
		Spec: ToolClassSpec{
			Provisioner:   "mcp",
			Parameters:    map[string]string{"version": "2024-11"},
			ExecutionTier: ExecutionTierSidecar,
			SandboxImage:  "",
			Transport:     TransportSSE,
			SandboxDefaults: &ToolSandboxSpec{
				CPUMilli: 250,
				MemoryMB: 128,
				Timeout:  "60s",
			},
		},
		Status: ToolClassStatus{Provisioned: true},
	}

	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got ToolClass
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.ExecutionTier != ExecutionTierSidecar {
		t.Errorf("ExecutionTier = %q, want %q", got.Spec.ExecutionTier, ExecutionTierSidecar)
	}
	if got.Spec.Transport != TransportSSE {
		t.Errorf("Transport = %q, want %q", got.Spec.Transport, TransportSSE)
	}
	if !got.Status.Provisioned {
		t.Error("Provisioned = false, want true")
	}
}

// TestModelClassJSONRoundtrip verifies ModelClass serialization.
func TestModelClassJSONRoundtrip(t *testing.T) {
	mc := &ModelClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "ModelClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "openai-gpt4"},
		Spec: ModelClassSpec{
			Provisioner:   "openai",
			Parameters:    map[string]string{"api_version": "2024-02"},
			LatencyClass:  "low",
			CostTier:      "premium",
			ContextWindow: 128000,
			Modalities:    []string{"text", "image"},
			HealthProbe: &ModelHealthProbeSpec{
				Endpoint:           "https://api.openai.com/v1/health",
				Type:               "latency",
				IntervalSeconds:    30,
				TimeoutSeconds:     5,
				LatencyThresholdMs: 2000,
				FailureThreshold:   3,
				SuccessThreshold:   1,
			},
			VersionPin: "gpt-4o-2024-05-13",
		},
		Status: ModelClassStatus{
			Phase:         "Ready",
			Provisioned:   true,
			ActiveVersion: "gpt-4o-2024-05-13",
			Health:        "Healthy",
			FallbackClass: "openai-gpt35",
		},
	}

	data, err := json.Marshal(mc)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got ModelClass
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", got.Spec.ContextWindow)
	}
	if len(got.Spec.Modalities) != 2 {
		t.Errorf("Modalities len = %d, want 2", len(got.Spec.Modalities))
	}
	if got.Spec.HealthProbe == nil || got.Spec.HealthProbe.LatencyThresholdMs != 2000 {
		t.Errorf("HealthProbe unexpected: %+v", got.Spec.HealthProbe)
	}
	if got.Status.FallbackClass != "openai-gpt35" {
		t.Errorf("FallbackClass = %q, want %q", got.Status.FallbackClass, "openai-gpt35")
	}
}

// TestMemoryClassJSONRoundtrip verifies MemoryClass serialization.
func TestMemoryClassJSONRoundtrip(t *testing.T) {
	mc := &MemoryClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "MemoryClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "pgvector-default"},
		Spec: MemoryClassSpec{
			BackendType:   "pgvector",
			Parameters:    map[string]string{"connection_string": "postgresql://localhost:5432/memory"},
			ReclaimPolicy: ReclaimPolicyRetain,
			SummarizationConfig: &MemorySummarizationConfig{
				Enabled:          true,
				ModelRef:         &LocalObjectReference{Name: "gpt-4o-mini"},
				ThresholdRecords: 8000,
			},
			ConsistencyMode: ConcurrencyModeCRDT,
		},
		Status: MemoryClassStatus{
			Ready: true,
		},
	}

	data, err := json.Marshal(mc)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got MemoryClass
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.BackendType != "pgvector" {
		t.Errorf("BackendType = %q, want %q", got.Spec.BackendType, "pgvector")
	}
	if got.Spec.ReclaimPolicy != ReclaimPolicyRetain {
		t.Errorf("ReclaimPolicy = %q, want %q", got.Spec.ReclaimPolicy, ReclaimPolicyRetain)
	}
	if got.Spec.SummarizationConfig == nil || !got.Spec.SummarizationConfig.Enabled {
		t.Errorf("SummarizationConfig unexpected: %+v", got.Spec.SummarizationConfig)
	}
	if got.Spec.ConsistencyMode != ConcurrencyModeCRDT {
		t.Errorf("ConsistencyMode = %q, want %q", got.Spec.ConsistencyMode, ConcurrencyModeCRDT)
	}
	if !got.Status.Ready {
		t.Error("Ready = false, want true")
	}
}

// TestWorkflowJSONRoundtrip verifies Workflow serialization.
func TestWorkflowJSONRoundtrip(t *testing.T) {
	wf := &Workflow{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "Workflow"},
		ObjectMeta: metav1.ObjectMeta{Name: "research-workflow", Namespace: "default"},
		Spec: WorkflowSpec{
			Steps: []WorkflowStep{
				{Name: "plan", AgentRef: LocalObjectReference{Name: "planner"}, Input: "research topic"},
				{Name: "execute", AgentRef: LocalObjectReference{Name: "researcher"}},
				{Name: "review", AgentRef: LocalObjectReference{Name: "reviewer"}},
			},
			EntryPoint:    "plan",
			Edges:         []WorkflowEdge{{From: "plan", To: "execute", Condition: "steps.plan.output != ''"}, {From: "execute", To: "review"}},
			ApprovalGates: []WorkflowApprovalGate{{From: "execute", To: "review"}},
			Budgets:       &WorkflowBudget{MaxTokens: 50000, MaxDuration: "1h"},
			Emergent:      &WorkflowEmergentSpec{Enabled: true},
		},
		Status: WorkflowStatus{
			Phase:          "Running",
			CompletedSteps: []string{"plan"},
			ActiveStep:     "execute",
		},
	}

	data, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got Workflow
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.EntryPoint != "plan" {
		t.Errorf("EntryPoint = %q, want %q", got.Spec.EntryPoint, "plan")
	}
	if len(got.Spec.Steps) != 3 {
		t.Fatalf("Steps len = %d, want 3", len(got.Spec.Steps))
	}
	if got.Spec.Edges[0].Condition != "steps.plan.output != ''" {
		t.Errorf("Edges[0].Condition = %q, unexpected", got.Spec.Edges[0].Condition)
	}
	if len(got.Spec.Edges) != 2 {
		t.Errorf("Edges len = %d, want 2", len(got.Spec.Edges))
	}
	if got.Spec.Emergent == nil || !got.Spec.Emergent.Enabled {
		t.Error("Emergent.Enabled = false, want true")
	}
	if got.Spec.Budgets == nil || got.Spec.Budgets.MaxTokens != 50000 {
		t.Errorf("Budgets unexpected: %+v", got.Spec.Budgets)
	}
	if got.Status.ActiveStep != "execute" {
		t.Errorf("ActiveStep = %q, want %q", got.Status.ActiveStep, "execute")
	}
}

// TestWorkflowRunJSONRoundtrip verifies WorkflowRun serialization.
func TestWorkflowRunJSONRoundtrip(t *testing.T) {
	now := metav1.Now()
	run := &WorkflowRun{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "WorkflowRun"},
		ObjectMeta: metav1.ObjectMeta{Name: "research-run-1", Namespace: "default"},
		Spec: WorkflowRunSpec{
			WorkflowRef: LocalObjectReference{Name: "research-workflow"},
			Input:       "research quantum computing",
			Parameters:  map[string]string{"depth": "detailed", "format": "markdown"},
		},
		Status: WorkflowRunStatus{
			Phase:          "Completed",
			CompletedSteps: []string{"plan", "execute", "review"},
			StartTime:      &now,
			CompletionTime: &now,
			Output:         "Research complete.",
		},
	}

	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got WorkflowRun
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.WorkflowRef.Name != "research-workflow" {
		t.Errorf("WorkflowRef.Name = %q, want %q", got.Spec.WorkflowRef.Name, "research-workflow")
	}
	if got.Spec.Parameters["depth"] != "detailed" {
		t.Errorf("Parameters[depth] = %q, want %q", got.Spec.Parameters["depth"], "detailed")
	}
	if len(got.Status.CompletedSteps) != 3 {
		t.Errorf("CompletedSteps len = %d, want 3", len(got.Status.CompletedSteps))
	}
	if got.Status.Output != "Research complete." {
		t.Errorf("Output = %q, want %q", got.Status.Output, "Research complete.")
	}
}

// TestOmitEmptyFields verifies that optional/empty fields are omitted from JSON.
func TestOmitEmptyFields(t *testing.T) {
	agent := &Agent{
		Spec: AgentSpec{
			ModelRef:     LocalObjectReference{Name: "gpt-4o"},
			Instructions: AgentInstructions{Inline: "test"},
		},
	}

	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal to map error: %v", err)
	}

	spec, ok := raw["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec not found or not an object")
	}

	// These optional fields should not appear when empty/nil.
	optionalFields := []string{"tools", "memory", "runtime", "companions", "knowledgeBases", "autonomyClassRef", "replicas", "probes", "restartLadder"}
	for _, f := range optionalFields {
		if _, exists := spec[f]; exists {
			t.Errorf("optional field %q should be omitted when empty, but was present", f)
		}
	}
}

// TestEnumConstants verifies enum type constants are correct strings.
func TestEnumConstants(t *testing.T) {
	// ToolProtocol
	if ToolProtocolMCP != "MCP" {
		t.Errorf("ToolProtocolMCP = %q", ToolProtocolMCP)
	}
	if ToolProtocolOpenAPI != "OpenAPI" {
		t.Errorf("ToolProtocolOpenAPI = %q", ToolProtocolOpenAPI)
	}

	// ExecutionTier
	if ExecutionTierProcess != "process" {
		t.Errorf("ExecutionTierProcess = %q", ExecutionTierProcess)
	}
	if ExecutionTierVM != "vm" {
		t.Errorf("ExecutionTierVM = %q", ExecutionTierVM)
	}

	// Transport
	if TransportStdio != "stdio" {
		t.Errorf("TransportStdio = %q", TransportStdio)
	}
	if TransportSSE != "sse" {
		t.Errorf("TransportSSE = %q", TransportSSE)
	}

	// MemoryType
	if MemoryTypeEpisodic != "episodic" {
		t.Errorf("MemoryTypeEpisodic = %q", MemoryTypeEpisodic)
	}
	if MemoryTypeShared != "shared" {
		t.Errorf("MemoryTypeShared = %q", MemoryTypeShared)
	}

	// ReclaimPolicy
	if ReclaimPolicyDelete != "Delete" {
		t.Errorf("ReclaimPolicyDelete = %q", ReclaimPolicyDelete)
	}
	if ReclaimPolicyArchive != "Archive" {
		t.Errorf("ReclaimPolicyArchive = %q", ReclaimPolicyArchive)
	}

	// ConcurrencyMode
	if ConcurrencyModeLWW != "lww" {
		t.Errorf("ConcurrencyModeLWW = %q", ConcurrencyModeLWW)
	}
	if ConcurrencyModeCRDT != "crdt" {
		t.Errorf("ConcurrencyModeCRDT = %q", ConcurrencyModeCRDT)
	}

	// PolicyScope
	if PolicyScopeWorkspace != "workspace" {
		t.Errorf("PolicyScopeWorkspace = %q", PolicyScopeWorkspace)
	}
	if PolicyScopeAgent != "agent" {
		t.Errorf("PolicyScopeAgent = %q", PolicyScopeAgent)
	}

	// RuleSeverity
	if RuleSeverityError != "error" {
		t.Errorf("RuleSeverityError = %q", RuleSeverityError)
	}
	if RuleSeverityWarning != "warning" {
		t.Errorf("RuleSeverityWarning = %q", RuleSeverityWarning)
	}

	// FailurePolicy
	if FailurePolicyDeny != "Deny" {
		t.Errorf("FailurePolicyDeny = %q", FailurePolicyDeny)
	}
	if FailurePolicyIgnore != "Ignore" {
		t.Errorf("FailurePolicyIgnore = %q", FailurePolicyIgnore)
	}
}

// TestAgentPolicyJSONRoundtrip verifies AgentPolicy serialization.
func TestAgentPolicyJSONRoundtrip(t *testing.T) {
	ap := &AgentPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "AgentPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "strict-policy", Namespace: "production"},
		Spec: AgentPolicySpec{
			Rules:        "package kognition.policy\ndefault allow = false",
			Enforce:      true,
			Scope:        PolicyScopeAgent,
			TargetAgents: []string{"agent-a", "agent-b"},
			ToolGrants: []PolicyToolGrant{
				{ToolRef: "web-search", Allow: true, Conditions: "input.user.role == \"admin\""},
				{ToolRef: "dangerous-tool", Allow: false},
			},
			SafetyRules: []GovernanceRule{
				{Name: "no-pii", Rego: "deny[msg] { contains(input.output, \"SSN\") }", Severity: RuleSeverityError},
			},
			ComplianceRules: []GovernanceRule{
				{Name: "audit-trail", Rego: "deny[msg] { not input.audit_enabled }", Severity: RuleSeverityWarning},
			},
		},
	}

	data, err := json.Marshal(ap)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got AgentPolicy
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if !got.Spec.Enforce {
		t.Error("Enforce = false, want true")
	}
	if got.Spec.Scope != PolicyScopeAgent {
		t.Errorf("Scope = %q, want %q", got.Spec.Scope, PolicyScopeAgent)
	}
	if len(got.Spec.TargetAgents) != 2 || got.Spec.TargetAgents[0] != "agent-a" {
		t.Errorf("TargetAgents unexpected: %v", got.Spec.TargetAgents)
	}
	if len(got.Spec.ToolGrants) != 2 || got.Spec.ToolGrants[0].ToolRef != "web-search" {
		t.Errorf("ToolGrants unexpected: %+v", got.Spec.ToolGrants)
	}
	if !got.Spec.ToolGrants[0].Allow {
		t.Error("ToolGrants[0].Allow = false, want true")
	}
	if len(got.Spec.SafetyRules) != 1 || got.Spec.SafetyRules[0].Severity != RuleSeverityError {
		t.Errorf("SafetyRules unexpected: %+v", got.Spec.SafetyRules)
	}
	if len(got.Spec.ComplianceRules) != 1 || got.Spec.ComplianceRules[0].Name != "audit-trail" {
		t.Errorf("ComplianceRules unexpected: %+v", got.Spec.ComplianceRules)
	}
}

// TestCognitiveAdmissionPolicyJSONRoundtrip verifies CognitiveAdmissionPolicy serialization.
func TestCognitiveAdmissionPolicyJSONRoundtrip(t *testing.T) {
	cap := &CognitiveAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "CognitiveAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: "validate-tools"},
		Spec: CognitiveAdmissionPolicySpec{
			Rego:            "package kognition.admission\ndeny[msg] { input.spec.sandbox == null }",
			MatchKinds:      []string{"Tool", "Agent"},
			MatchOperations: []string{"CREATE", "UPDATE"},
			MutationRules:   "package kognition.mutation\npatch[p] { p := {\"op\": \"add\", \"path\": \"/spec/sandbox\"} }",
			FailurePolicy:   FailurePolicyIgnore,
		},
	}

	data, err := json.Marshal(cap)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got CognitiveAdmissionPolicy
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.FailurePolicy != FailurePolicyIgnore {
		t.Errorf("FailurePolicy = %q, want %q", got.Spec.FailurePolicy, FailurePolicyIgnore)
	}
	if len(got.Spec.MatchKinds) != 2 || got.Spec.MatchKinds[0] != "Tool" {
		t.Errorf("MatchKinds unexpected: %v", got.Spec.MatchKinds)
	}
	if len(got.Spec.MatchOperations) != 2 {
		t.Errorf("MatchOperations len = %d, want 2", len(got.Spec.MatchOperations))
	}
	if got.Spec.MutationRules == "" {
		t.Error("MutationRules should not be empty")
	}
}

// TestCognitiveQuotaJSONRoundtrip verifies CognitiveQuota serialization.
func TestCognitiveQuotaJSONRoundtrip(t *testing.T) {
	cq := &CognitiveQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "CognitiveQuota"},
		ObjectMeta: metav1.ObjectMeta{Name: "ns-quota", Namespace: "production"},
		Spec: CognitiveQuotaSpec{
			Hard: map[string]int64{"agents": 10, "tools": 50, "sessions": 100},
			TokenBudget: &TokenBudget{
				MaxInputTokens:  1000000,
				MaxOutputTokens: 500000,
				MaxTotalTokens:  1500000,
			},
			CostBudget: &CostBudget{
				MaxCostPerPeriod:      10000,
				MaxToolCallsPerPeriod: 5000,
			},
			ResetInterval: "24h",
		},
		Status: CognitiveQuotaStatus{
			Used: map[string]int64{"agents": 5, "tools": 20},
			TokenUsage: &QuotaTokenUsage{
				InputTokens:  500000,
				OutputTokens: 200000,
				TotalTokens:  700000,
			},
			CostUsage: &QuotaCostUsage{
				TotalCost: 4500,
				ToolCalls: 2000,
			},
			LastResetTime: "2024-01-15T00:00:00Z",
		},
	}

	data, err := json.Marshal(cq)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got CognitiveQuota
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.Hard["agents"] != 10 {
		t.Errorf("Hard[agents] = %d, want 10", got.Spec.Hard["agents"])
	}
	if got.Spec.TokenBudget == nil || got.Spec.TokenBudget.MaxTotalTokens != 1500000 {
		t.Errorf("TokenBudget unexpected: %+v", got.Spec.TokenBudget)
	}
	if got.Spec.CostBudget == nil || got.Spec.CostBudget.MaxCostPerPeriod != 10000 {
		t.Errorf("CostBudget unexpected: %+v", got.Spec.CostBudget)
	}
	if got.Spec.ResetInterval != "24h" {
		t.Errorf("ResetInterval = %q, want %q", got.Spec.ResetInterval, "24h")
	}
	if got.Status.Used["agents"] != 5 {
		t.Errorf("Status.Used[agents] = %d, want 5", got.Status.Used["agents"])
	}
	if got.Status.TokenUsage == nil || got.Status.TokenUsage.TotalTokens != 700000 {
		t.Errorf("Status.TokenUsage unexpected: %+v", got.Status.TokenUsage)
	}
	if got.Status.CostUsage == nil || got.Status.CostUsage.ToolCalls != 2000 {
		t.Errorf("Status.CostUsage unexpected: %+v", got.Status.CostUsage)
	}
}

// TestCognitiveLimitRangeJSONRoundtrip verifies CognitiveLimitRange serialization.
func TestCognitiveLimitRangeJSONRoundtrip(t *testing.T) {
	clr := &CognitiveLimitRange{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "CognitiveLimitRange"},
		ObjectMeta: metav1.ObjectMeta{Name: "default-limits", Namespace: "production"},
		Spec: CognitiveLimitRangeSpec{
			Limits: []LimitRangeItem{
				{
					Type:                  "Agent",
					Default:               map[string]int64{"maxTokens": 4096, "timeout": 30},
					Max:                   map[string]int64{"maxTokens": 128000, "timeout": 300},
					Min:                   map[string]int64{"maxTokens": 100, "timeout": 5},
					TokensPerSession:      50000,
					TimeoutSeconds:        120,
					MaxConcurrentSessions: 5,
				},
				{
					Type:             "Tool",
					Default:          map[string]int64{"cpuMilli": 250},
					Max:              map[string]int64{"cpuMilli": 2000},
					TokensPerSession: 10000,
				},
			},
		},
	}

	data, err := json.Marshal(clr)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got CognitiveLimitRange
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if len(got.Spec.Limits) != 2 {
		t.Fatalf("Limits len = %d, want 2", len(got.Spec.Limits))
	}
	if got.Spec.Limits[0].Type != "Agent" {
		t.Errorf("Limits[0].Type = %q, want %q", got.Spec.Limits[0].Type, "Agent")
	}
	if got.Spec.Limits[0].Default["maxTokens"] != 4096 {
		t.Errorf("Limits[0].Default[maxTokens] = %d, want 4096", got.Spec.Limits[0].Default["maxTokens"])
	}
	if got.Spec.Limits[0].Max["timeout"] != 300 {
		t.Errorf("Limits[0].Max[timeout] = %d, want 300", got.Spec.Limits[0].Max["timeout"])
	}
	if got.Spec.Limits[0].TokensPerSession != 50000 {
		t.Errorf("TokensPerSession = %d, want 50000", got.Spec.Limits[0].TokensPerSession)
	}
	if got.Spec.Limits[0].MaxConcurrentSessions != 5 {
		t.Errorf("MaxConcurrentSessions = %d, want 5", got.Spec.Limits[0].MaxConcurrentSessions)
	}
}

// TestWorkspaceJSONRoundtrip verifies Workspace serialization.
func TestWorkspaceJSONRoundtrip(t *testing.T) {
	ws := &Workspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-alpha"},
		Spec: WorkspaceSpec{
			DisplayName:            "Team Alpha Workspace",
			RequireSignedArtifacts: true,
			Retention: &RetentionPolicy{
				TraceTTL:      "720h",
				AuditLogTTL:   "2160h",
				CostRecordTTL: "8760h",
				EventTTL:      "168h",
			},
		},
		Status: WorkspaceStatus{Phase: "Active"},
	}

	data, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got Workspace
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.DisplayName != "Team Alpha Workspace" {
		t.Errorf("DisplayName = %q, want %q", got.Spec.DisplayName, "Team Alpha Workspace")
	}
	if !got.Spec.RequireSignedArtifacts {
		t.Error("RequireSignedArtifacts = false, want true")
	}
	if got.Spec.Retention == nil || got.Spec.Retention.TraceTTL != "720h" {
		t.Errorf("Retention.TraceTTL unexpected: %+v", got.Spec.Retention)
	}
	if got.Spec.Retention.AuditLogTTL != "2160h" {
		t.Errorf("Retention.AuditLogTTL = %q, want %q", got.Spec.Retention.AuditLogTTL, "2160h")
	}
	if got.Status.Phase != "Active" {
		t.Errorf("Status.Phase = %q, want %q", got.Status.Phase, "Active")
	}
}

// TestAgentIdentityJSONRoundtrip verifies AgentIdentity serialization.
func TestAgentIdentityJSONRoundtrip(t *testing.T) {
	ai := &AgentIdentity{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "AgentIdentity"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-a-identity", Namespace: "production"},
		Spec: AgentIdentitySpec{
			SPIFFEID: "spiffe://cluster.local/ns/production/agent/agent-a",
			AgentRef: LocalObjectReference{Name: "agent-a"},
		},
		Status: AgentIdentityStatus{Phase: "Issued", WorkloadIdentity: &WorkloadIdentityStatus{AgentUID: "agent-uid", SourceGeneration: 2, Ready: true, Reason: "Ready"}},
	}

	data, err := json.Marshal(ai)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got AgentIdentity
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.SPIFFEID != "spiffe://cluster.local/ns/production/agent/agent-a" {
		t.Errorf("SPIFFEID = %q", got.Spec.SPIFFEID)
	}
	if got.Spec.AgentRef.Name != "agent-a" {
		t.Errorf("AgentRef.Name = %q, want %q", got.Spec.AgentRef.Name, "agent-a")
	}
	if got.Status.Phase != "Issued" {
		t.Errorf("Status.Phase = %q, want %q", got.Status.Phase, "Issued")
	}
	if got.Status.WorkloadIdentity == nil || *got.Status.WorkloadIdentity != *ai.Status.WorkloadIdentity {
		t.Fatal("certificate metadata changed during JSON roundtrip")
	}
}

// TestAgentRoleJSONRoundtrip verifies AgentRole serialization.
func TestAgentRoleJSONRoundtrip(t *testing.T) {
	ar := &AgentRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "AgentRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "tool-user"},
		Rules: []PolicyRule{
			{Resources: []string{"tools", "toolclasses"}, Verbs: []string{"get", "list"}},
			{Resources: []string{"agents"}, Verbs: []string{"get", "list", "create", "update"}},
		},
	}

	data, err := json.Marshal(ar)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got AgentRole
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if len(got.Rules) != 2 {
		t.Fatalf("Rules len = %d, want 2", len(got.Rules))
	}
	if got.Rules[0].Resources[0] != "tools" {
		t.Errorf("Rules[0].Resources[0] = %q, want %q", got.Rules[0].Resources[0], "tools")
	}
	if len(got.Rules[0].Verbs) != 2 {
		t.Errorf("Rules[0].Verbs len = %d, want 2", len(got.Rules[0].Verbs))
	}
	if got.Rules[1].Verbs[2] != "create" {
		t.Errorf("Rules[1].Verbs[2] = %q, want %q", got.Rules[1].Verbs[2], "create")
	}
}

// TestAgentRoleBindingJSONRoundtrip verifies AgentRoleBinding serialization.
func TestAgentRoleBindingJSONRoundtrip(t *testing.T) {
	arb := &AgentRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "AgentRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "team-alpha-binding", Namespace: "production"},
		Spec: AgentRoleBindingSpec{
			RoleRef: LocalObjectReference{Name: "tool-user"},
			Subjects: []Subject{
				{Kind: "Agent", Name: "agent-a"},
				{Kind: "AgentIdentity", Name: "agent-b-identity"},
			},
		},
	}

	data, err := json.Marshal(arb)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got AgentRoleBinding
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if got.Spec.RoleRef.Name != "tool-user" {
		t.Errorf("RoleRef.Name = %q, want %q", got.Spec.RoleRef.Name, "tool-user")
	}
	if len(got.Spec.Subjects) != 2 {
		t.Fatalf("Subjects len = %d, want 2", len(got.Spec.Subjects))
	}
	if got.Spec.Subjects[0].Kind != "Agent" || got.Spec.Subjects[0].Name != "agent-a" {
		t.Errorf("Subjects[0] = %+v", got.Spec.Subjects[0])
	}
	if got.Spec.Subjects[1].Kind != "AgentIdentity" {
		t.Errorf("Subjects[1].Kind = %q, want %q", got.Spec.Subjects[1].Kind, "AgentIdentity")
	}
}
