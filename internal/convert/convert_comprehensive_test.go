// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert_test

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

// ======================== Agent: Probes ========================

func TestAgentProbesRoundTrip(t *testing.T) {
	replicas := int32(1)
	in := &api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "probe-agent", Namespace: "ns"},
		Spec: api.AgentSpec{
			ModelRef: api.LocalObjectReference{Name: "m"},
			Replicas: &replicas,
			Probes: &api.AgentProbeSpec{
				HealthProbe: &api.AgentHealthProbe{
					IntervalSeconds:  30,
					TimeoutSeconds:   5,
					FailureThreshold: 3,
					SuccessThreshold: 2,
				},
				ReadinessProbe: &api.AgentReadinessProbe{
					IntervalSeconds:  60,
					TimeoutSeconds:   10,
					FailureThreshold: 5,
					SuccessThreshold: 1,
				},
				ReasoningProbe: &api.AgentReasoningProbe{
					CanaryTask:       "What is 2+2?",
					ExpectedOutcome:  "4",
					IntervalSeconds:  300,
					TimeoutSeconds:   60,
					MaxTokens:        256,
					MaxInputTokens:   4096,
					FailureThreshold: 2,
					SuccessThreshold: 3,
				},
			},
		},
	}

	k8sObj := convert.AgentToK8s(in)
	out := convert.AgentFromK8s(k8sObj)

	if out.Spec.Probes == nil {
		t.Fatal("Probes lost")
	}
	hp := out.Spec.Probes.HealthProbe
	if hp == nil {
		t.Fatal("HealthProbe lost")
	}
	if hp.IntervalSeconds != 30 || hp.TimeoutSeconds != 5 || hp.FailureThreshold != 3 || hp.SuccessThreshold != 2 {
		t.Errorf("HealthProbe fields wrong: %+v", hp)
	}

	rp := out.Spec.Probes.ReadinessProbe
	if rp == nil {
		t.Fatal("ReadinessProbe lost")
	}
	if rp.IntervalSeconds != 60 || rp.TimeoutSeconds != 10 || rp.FailureThreshold != 5 || rp.SuccessThreshold != 1 {
		t.Errorf("ReadinessProbe fields wrong: %+v", rp)
	}

	reas := out.Spec.Probes.ReasoningProbe
	if reas == nil {
		t.Fatal("ReasoningProbe lost")
	}
	if reas.CanaryTask != "What is 2+2?" || reas.ExpectedOutcome != "4" || reas.IntervalSeconds != 300 || reas.TimeoutSeconds != 60 || reas.MaxTokens != 256 || reas.MaxInputTokens != 4096 || reas.FailureThreshold != 2 || reas.SuccessThreshold != 3 {
		t.Errorf("ReasoningProbe fields wrong: %+v", reas)
	}
}

// ======================== Agent: RestartLadder ========================

func TestAgentRestartLadderRoundTrip(t *testing.T) {
	in := &api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "ladder-agent", Namespace: "ns"},
		Spec: api.AgentSpec{
			ModelRef: api.LocalObjectReference{Name: "m"},
			RestartLadder: &api.RestartLadder{
				Steps: []api.RestartLadderStep{
					{Action: api.RestartActionRestart, MaxAttempts: 3},
					{Action: api.RestartActionDegradeModel, MaxAttempts: 1},
					{Action: api.RestartActionQuarantine, MaxAttempts: 1},
				},
			},
		},
	}

	k8sObj := convert.AgentToK8s(in)
	out := convert.AgentFromK8s(k8sObj)

	if out.Spec.RestartLadder == nil {
		t.Fatal("RestartLadder lost")
	}
	steps := out.Spec.RestartLadder.Steps
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(steps))
	}
	if steps[0].Action != api.RestartActionRestart || steps[0].MaxAttempts != 3 {
		t.Errorf("step[0]: got %+v", steps[0])
	}
	if steps[1].Action != api.RestartActionDegradeModel || steps[1].MaxAttempts != 1 {
		t.Errorf("step[1]: got %+v", steps[1])
	}
	if steps[2].Action != api.RestartActionQuarantine {
		t.Errorf("step[2]: got action %q", steps[2].Action)
	}
}

// ======================== Agent: Full Status ========================

func TestAgentStatusRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := &api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "s-agent", Namespace: "ns"},
		Spec:       api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "m"}},
		Status: api.AgentStatus{
			Phase:         api.PhaseReady,
			Replicas:      5,
			ReadyReplicas: 3,
			RevisionCount: 7,
			Conditions: []api.Condition{
				{Type: "Ready", Status: "True", LastTransitionTime: now, Message: "all good"},
			},
		},
	}

	k8sObj := convert.AgentToK8s(in)
	out := convert.AgentFromK8s(k8sObj)

	if out.Status.Replicas != 5 || out.Status.ReadyReplicas != 3 || out.Status.RevisionCount != 7 {
		t.Errorf("status counts: %d/%d/%d", out.Status.Replicas, out.Status.ReadyReplicas, out.Status.RevisionCount)
	}
	if len(out.Status.Conditions) != 1 {
		t.Fatalf("conditions: got %d", len(out.Status.Conditions))
	}
	c := out.Status.Conditions[0]
	if c.Type != "Ready" || c.Status != "True" || c.Message != "all good" {
		t.Errorf("condition: %+v", c)
	}
}

// ======================== AgentInstance: TokenUsage ========================

func TestAgentInstanceTokenUsageRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := &api.AgentInstance{
		TypeMeta:   api.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "inst-1", Namespace: "ns"},
		Spec: api.AgentInstanceSpec{
			AgentRef: api.LocalObjectReference{Name: "a"},
			Revision: 2,
		},
		Status: api.AgentInstanceStatus{
			Phase:        api.PhaseRunning,
			SessionID:    "sess-abc",
			StartTime:    &now,
			ToolCalls:    42,
			RestartCount: 1,
			TokenUsage: &api.TokenUsage{
				PromptTokens:     1000,
				CompletionTokens: 500,
				TotalTokens:      1500,
			},
		},
	}

	k8sObj := convert.AgentInstanceToK8s(in)
	out := convert.AgentInstanceFromK8s(k8sObj)

	if out.Spec.Revision != 2 {
		t.Errorf("Revision: got %d", out.Spec.Revision)
	}
	if out.Status.SessionID != "sess-abc" {
		t.Errorf("SessionID: got %q", out.Status.SessionID)
	}
	if out.Status.ToolCalls != 42 || out.Status.RestartCount != 1 {
		t.Errorf("ToolCalls/RestartCount: %d/%d", out.Status.ToolCalls, out.Status.RestartCount)
	}
	if out.Status.TokenUsage == nil {
		t.Fatal("TokenUsage lost")
	}
	tu := out.Status.TokenUsage
	if tu.PromptTokens != 1000 || tu.CompletionTokens != 500 || tu.TotalTokens != 1500 {
		t.Errorf("TokenUsage: %+v", tu)
	}
	if out.Status.StartTime == nil || !out.Status.StartTime.Equal(now) {
		t.Error("StartTime lost or changed")
	}
}

// ======================== Tool: Sandbox & SecretRef ========================

func TestToolSandboxRoundTrip(t *testing.T) {
	in := &api.Tool{
		TypeMeta:   api.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "sandboxed-tool", Namespace: "ns"},
		Spec: api.ToolSpec{
			ClassName:   "mcp",
			Description: "A sandboxed tool",
			Endpoint:    "https://t.io",
			Protocol:    "mcp",
			Sandbox: &api.ToolSandboxSpec{
				Timeout:       "30s",
				CPUMilli:      500,
				MemoryMB:      256,
				NetworkPolicy: "egress-only",
			},
			SecretRefs: []api.ToolSecretRef{
				{Name: "my-secret", Key: "api-key"},
				{Name: "second-secret", Key: "token"}, // lost
			},
			Metadata: map[string]string{"foo": "bar"},
			Schema: map[string]any{
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
			},
		},
		Status: api.ToolStatus{
			Phase:              api.PhaseReady,
			Available:          true,
			LastHealthCheck:    "2026-07-27T00:00:00Z",
			ResolvedProtocol:   api.ToolProtocolMCP,
			ObservedDigest:     "sha256:contract",
			ObservedSpecDigest: "sha256:spec",
			CompiledMCP: &api.MCPToolDefinition{
				Name:        "sandboxed-tool",
				Description: "A sandboxed tool",
				InputSchema: map[string]any{"type": "object"},
				Transport:   api.TransportHTTP,
				Endpoint:    "https://t.io",
			},
		},
	}

	k8sObj := convert.ToolToK8s(in)
	out := convert.ToolFromK8s(k8sObj)
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Fatalf("Status round trip mismatch: got %+v, want %+v", out.Status, in.Status)
	}
	k8sObj.Spec.Metadata["foo"] = "changed"
	k8sObj.Spec.Schema["properties"].(map[string]any)["query"].(map[string]any)["type"] = "number"
	k8sObj.Status.CompiledMCP.InputSchema["type"] = "array"
	if in.Spec.Metadata["foo"] != "bar" {
		t.Fatal("Kubernetes metadata mutation aliased the canonical Tool spec")
	}
	if in.Spec.Schema["properties"].(map[string]any)["query"].(map[string]any)["type"] != "string" {
		t.Fatal("Kubernetes schema mutation aliased the canonical Tool spec")
	}
	if in.Status.CompiledMCP.InputSchema["type"] != "object" {
		t.Fatal("Kubernetes compiled schema mutation aliased canonical Tool status")
	}

	// Sandbox
	if out.Spec.Sandbox == nil {
		t.Fatal("Sandbox lost")
	}
	if out.Spec.Sandbox.Timeout != "30s" {
		t.Errorf("Timeout: got %q", out.Spec.Sandbox.Timeout)
	}
	if out.Spec.Sandbox.CPUMilli != 500 || out.Spec.Sandbox.MemoryMB != 256 {
		t.Errorf("Sandbox resources: %d/%d", out.Spec.Sandbox.CPUMilli, out.Spec.Sandbox.MemoryMB)
	}
	if out.Spec.Sandbox.NetworkPolicy != "egress-only" {
		t.Errorf("Sandbox.NetworkPolicy: got %q", out.Spec.Sandbox.NetworkPolicy)
	}

	// SecretRefs: all refs survive with Name and Key
	if len(out.Spec.SecretRefs) != 2 {
		t.Fatalf("SecretRefs: expected 2, got %d", len(out.Spec.SecretRefs))
	}
	if out.Spec.SecretRefs[0].Name != "my-secret" {
		t.Errorf("SecretRef[0] name: got %q", out.Spec.SecretRefs[0].Name)
	}
	if out.Spec.SecretRefs[0].Key != "api-key" {
		t.Errorf("SecretRef[0] key: got %q", out.Spec.SecretRefs[0].Key)
	}
	if out.Spec.SecretRefs[1].Name != "second-secret" {
		t.Errorf("SecretRef[1] name: got %q", out.Spec.SecretRefs[1].Name)
	}
	if out.Spec.SecretRefs[1].Key != "token" {
		t.Errorf("SecretRef[1] key: got %q", out.Spec.SecretRefs[1].Key)
	}

	// Metadata survives as Parameters
	if out.Spec.Metadata["foo"] != "bar" {
		t.Errorf("Metadata: got %v", out.Spec.Metadata)
	}

	// Status
	if !out.Status.Available {
		t.Error("Available lost")
	}
}

// ======================== ToolClass: Full ========================

func TestToolClassFullRoundTrip(t *testing.T) {
	in := &api.ToolClass{
		TypeMeta:   api.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "mcp-class", Namespace: "default"},
		Spec: api.ToolClassSpec{
			Provisioner:   "mcp",
			Parameters:    map[string]string{"transport": "sse"},
			ExecutionTier: "standard",
			SandboxImage:  "registry.example/probe:1.0",
			StdioCommand:  []string{"/usr/local/bin/mcp-server", "--stdio"},
			Transport:     "sse",
			SandboxDefaults: &api.ToolSandboxSpec{
				Timeout:       "60s",
				CPUMilli:      1000,
				MemoryMB:      512,
				NetworkPolicy: "none",
			},
			ResourceLimits: &api.ToolClassResourceLimits{
				MaxCPUMilli: 2000,
				MaxMemoryMB: 1024,
				MaxTimeout:  "2m",
			},
		},
		Status: api.ToolClassStatus{
			Provisioned: true,
		},
	}

	k8sObj := convert.ToolClassToK8s(in)
	out := convert.ToolClassFromK8s(k8sObj)
	k8sObj.Spec.Parameters["transport"] = "http"
	if in.Spec.Parameters["transport"] != "sse" {
		t.Fatal("Kubernetes parameters mutation aliased the canonical ToolClass spec")
	}

	if out.Spec.ExecutionTier != "standard" {
		t.Errorf("ExecutionTier: got %q", out.Spec.ExecutionTier)
	}
	if out.Spec.SandboxImage != "registry.example/probe:1.0" {
		t.Errorf("SandboxImage: got %q", out.Spec.SandboxImage)
	}
	if len(out.Spec.StdioCommand) != 2 || out.Spec.StdioCommand[0] != "/usr/local/bin/mcp-server" || out.Spec.StdioCommand[1] != "--stdio" {
		t.Errorf("StdioCommand: got %q", out.Spec.StdioCommand)
	}
	if out.Spec.Transport != "sse" {
		t.Errorf("Transport: got %q", out.Spec.Transport)
	}
	if out.Spec.SandboxDefaults == nil {
		t.Fatal("SandboxDefaults lost")
	}
	if out.Spec.SandboxDefaults.Timeout != "60s" {
		t.Errorf("SandboxDefaults.Timeout: got %q", out.Spec.SandboxDefaults.Timeout)
	}
	if out.Spec.SandboxDefaults.NetworkPolicy != "none" {
		t.Errorf("SandboxDefaults.NetworkPolicy: got %q", out.Spec.SandboxDefaults.NetworkPolicy)
	}
	if out.Spec.ResourceLimits == nil {
		t.Fatal("ResourceLimits lost")
	}
	if out.Spec.ResourceLimits.MaxCPUMilli != 2000 || out.Spec.ResourceLimits.MaxMemoryMB != 1024 || out.Spec.ResourceLimits.MaxTimeout != "2m" {
		t.Errorf("ResourceLimits: got %+v", out.Spec.ResourceLimits)
	}
	if !out.Status.Provisioned {
		t.Error("Provisioned lost")
	}
}

func TestToolInvocationRunRoundTrip(t *testing.T) {
	startedAt := time.Now().UTC().Truncate(time.Second)
	completedAt := startedAt.Add(time.Second)
	in := &api.ToolInvocationRun{
		TypeMeta:   api.TypeMeta{Kind: "ToolInvocationRun", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "run", Namespace: "team-a"},
		Spec: api.ToolInvocationRunSpec{
			ToolName: "shell", ToolResourceVersion: 7, InvocationID: "invoke-1",
			Input: map[string]any{"command": "status", "nested": map[string]any{"id": "a"}},
		},
		Status: api.ToolInvocationRunStatus{
			Phase: api.PhaseReady, Output: map[string]any{"result": "ok"}, StartedAt: &startedAt, CompletedAt: &completedAt,
			ExecutionMode: "sandbox", DurationMillis: 1000,
		},
	}
	out := convert.ToolInvocationRunFromK8s(convert.ToolInvocationRunToK8s(in))
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("ToolInvocationRun round trip = %#v, want %#v", out, in)
	}
}

// ======================== Workspace ========================

func TestWorkspaceRoundTrip(t *testing.T) {
	in := &api.Workspace{
		TypeMeta:   api.TypeMeta{Kind: "Workspace", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "team-alpha", Namespace: ""},
		Spec: api.WorkspaceSpec{
			DisplayName:            "Team Alpha",
			RequireSignedArtifacts: true,
			Retention: &api.RetentionPolicy{
				TraceTTL:      api.Duration{Duration: 720 * time.Hour},
				AuditLogTTL:   api.Duration{Duration: 2160 * time.Hour},
				CostRecordTTL: api.Duration{Duration: 8760 * time.Hour},
				EventTTL:      api.Duration{Duration: 168 * time.Hour},
			},
		},
		Status: api.WorkspaceStatus{Phase: api.PhaseReady},
	}

	k8sObj := convert.WorkspaceToK8s(in)
	out := convert.WorkspaceFromK8s(k8sObj)

	if out.Spec.DisplayName != "Team Alpha" {
		t.Errorf("DisplayName: got %q", out.Spec.DisplayName)
	}
	if !out.Spec.RequireSignedArtifacts {
		t.Error("RequireSignedArtifacts lost")
	}
	if out.Spec.Retention == nil {
		t.Fatal("Retention lost")
	}
	if out.Spec.Retention.TraceTTL != (api.Duration{Duration: 720 * time.Hour}) {
		t.Errorf("TraceTTL: got %v", out.Spec.Retention.TraceTTL)
	}
	if out.Spec.Retention.AuditLogTTL != (api.Duration{Duration: 2160 * time.Hour}) {
		t.Errorf("AuditLogTTL: got %v", out.Spec.Retention.AuditLogTTL)
	}
	if out.Status.Phase != api.PhaseReady {
		t.Errorf("Phase: got %q", out.Status.Phase)
	}
}

// ======================== AgentRole ========================

func TestAgentRoleRoundTrip(t *testing.T) {
	in := &api.AgentRole{
		TypeMeta:   api.TypeMeta{Kind: "AgentRole", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "reader-role", Namespace: "ns"},
		Rules: []api.PolicyRule{
			{Resources: []string{"tools", "models"}, Verbs: []string{"get", "list"}},
			{Resources: []string{"workflows"}, Verbs: []string{"get", "list", "create"}},
		},
	}

	k8sObj := convert.AgentRoleToK8s(in)
	out := convert.AgentRoleFromK8s(k8sObj)

	if len(out.Rules) != 2 {
		t.Fatalf("Rules: got %d", len(out.Rules))
	}
	if out.Rules[0].Resources[0] != "tools" || out.Rules[0].Verbs[1] != "list" {
		t.Errorf("Rule[0]: %+v", out.Rules[0])
	}
	if out.Rules[1].Resources[0] != "workflows" || len(out.Rules[1].Verbs) != 3 {
		t.Errorf("Rule[1]: %+v", out.Rules[1])
	}
}

// ======================== AgentRoleBinding ========================

func TestAgentRoleBindingRoundTrip(t *testing.T) {
	in := &api.AgentRoleBinding{
		TypeMeta:   api.TypeMeta{Kind: "AgentRoleBinding", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "binding-1", Namespace: "ns"},
		Spec: api.AgentRoleBindingSpec{
			RoleRef: api.LocalObjectReference{Name: "reader-role"},
			Subjects: []api.Subject{
				{Kind: "Agent", Name: "agent-a"},
				{Kind: "AgentIdentity", Name: "id-b"},
			},
		},
	}

	k8sObj := convert.AgentRoleBindingToK8s(in)
	out := convert.AgentRoleBindingFromK8s(k8sObj)

	if out.Spec.RoleRef.Name != "reader-role" {
		t.Errorf("RoleRef: got %q", out.Spec.RoleRef.Name)
	}
	if len(out.Spec.Subjects) != 2 {
		t.Fatalf("Subjects: got %d", len(out.Spec.Subjects))
	}
	if out.Spec.Subjects[0].Kind != "Agent" || out.Spec.Subjects[0].Name != "agent-a" {
		t.Errorf("Subject[0]: %+v", out.Spec.Subjects[0])
	}
	if out.Spec.Subjects[1].Kind != "AgentIdentity" || out.Spec.Subjects[1].Name != "id-b" {
		t.Errorf("Subject[1]: %+v", out.Spec.Subjects[1])
	}
}

// ======================== AgentPolicy: Full ========================

func TestAgentPolicyFullRoundTrip(t *testing.T) {
	in := &api.AgentPolicy{
		TypeMeta:   api.TypeMeta{Kind: "AgentPolicy", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "full-policy", Namespace: "ns"},
		Spec: api.AgentPolicySpec{
			Rules:        "package kognition\ndefault allow = false",
			Enforce:      true,
			Scope:        "namespace",
			TargetAgents: []string{"agent-a", "agent-b"},
			ToolGrants: []api.PolicyToolGrant{
				{ToolRef: "web-search", Allow: true, Conditions: "input.context == 'research'"},
				{ToolRef: "database", Allow: false},
			},
			SafetyRules: []api.GovernanceRule{
				{Name: "no-pii", Rego: "deny { contains(input, \"SSN\") }", Severity: "critical"},
			},
			ComplianceRules: []api.GovernanceRule{
				{Name: "audit-trail", Rego: "audit { true }", Severity: "warning"},
			},
		},
	}

	k8sObj := convert.AgentPolicyToK8s(in)
	out := convert.AgentPolicyFromK8s(k8sObj)

	if out.Spec.Scope != "namespace" {
		t.Errorf("Scope: got %q", out.Spec.Scope)
	}
	if len(out.Spec.TargetAgents) != 2 || out.Spec.TargetAgents[1] != "agent-b" {
		t.Errorf("TargetAgents: %v", out.Spec.TargetAgents)
	}
	// ToolGrants
	if len(out.Spec.ToolGrants) != 2 {
		t.Fatalf("ToolGrants: got %d", len(out.Spec.ToolGrants))
	}
	if out.Spec.ToolGrants[0].ToolRef != "web-search" || !out.Spec.ToolGrants[0].Allow {
		t.Errorf("ToolGrant[0]: %+v", out.Spec.ToolGrants[0])
	}
	if out.Spec.ToolGrants[0].Conditions != "input.context == 'research'" {
		t.Errorf("ToolGrant[0].Conditions: %q", out.Spec.ToolGrants[0].Conditions)
	}
	// SafetyRules
	if len(out.Spec.SafetyRules) != 1 {
		t.Fatalf("SafetyRules: got %d", len(out.Spec.SafetyRules))
	}
	if out.Spec.SafetyRules[0].Name != "no-pii" || out.Spec.SafetyRules[0].Severity != "critical" {
		t.Errorf("SafetyRule[0]: %+v", out.Spec.SafetyRules[0])
	}
	// ComplianceRules
	if len(out.Spec.ComplianceRules) != 1 || out.Spec.ComplianceRules[0].Severity != "warning" {
		t.Errorf("ComplianceRules: %+v", out.Spec.ComplianceRules)
	}
}

// ======================== CognitiveAdmissionPolicy ========================

func TestCognitiveAdmissionPolicyRoundTrip(t *testing.T) {
	in := &api.CognitiveAdmissionPolicy{
		TypeMeta:   api.TypeMeta{Kind: "CognitiveAdmissionPolicy", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "cap-1", Namespace: "ns"},
		Spec: api.CognitiveAdmissionPolicySpec{
			Rego:            "package admission\ndeny { input.tool.untrusted }",
			MatchKinds:      []string{"Tool", "Agent"},
			MatchOperations: []string{"CREATE", "UPDATE"},
			MutationRules:   "package mutation\npatch[p] { p := {\"op\":\"add\"} }",
			FailurePolicy:   "Fail",
		},
	}

	k8sObj := convert.CognitiveAdmissionPolicyToK8s(in)
	out := convert.CognitiveAdmissionPolicyFromK8s(k8sObj)

	if out.Spec.Rego != in.Spec.Rego {
		t.Errorf("Rego: got %q", out.Spec.Rego)
	}
	if len(out.Spec.MatchKinds) != 2 || out.Spec.MatchKinds[0] != "Tool" {
		t.Errorf("MatchKinds: %v", out.Spec.MatchKinds)
	}
	if len(out.Spec.MatchOperations) != 2 || out.Spec.MatchOperations[1] != "UPDATE" {
		t.Errorf("MatchOperations: %v", out.Spec.MatchOperations)
	}
	if out.Spec.MutationRules != in.Spec.MutationRules {
		t.Errorf("MutationRules: got %q", out.Spec.MutationRules)
	}
	if out.Spec.FailurePolicy != "Fail" {
		t.Errorf("FailurePolicy: got %q", out.Spec.FailurePolicy)
	}
}

// ======================== CognitiveQuota ========================

func TestCognitiveQuotaRoundTrip(t *testing.T) {
	in := &api.CognitiveQuota{
		TypeMeta:   api.TypeMeta{Kind: "CognitiveQuota", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "quota-1", Namespace: "ns"},
		Spec: api.CognitiveQuotaSpec{
			Hard:          map[string]int64{"agents": 10, "tools": 50},
			ResetInterval: "24h",
			TokenBudget: &api.TokenBudget{
				MaxInputTokens:  1000000,
				MaxOutputTokens: 500000,
				MaxTotalTokens:  1500000,
			},
			CostBudget: &api.CostBudget{
				MaxCostPerPeriod:      10000,
				MaxToolCallsPerPeriod: 10000,
				MaxModelTierSeconds:   map[string]int64{"premium": 3600},
			},
		},
		Status: api.CognitiveQuotaStatus{
			Used:          map[string]int64{"agents": 5, "tools": 20},
			LastResetTime: "2024-01-01T00:00:00Z",
			TokenUsage: &api.QuotaTokenUsage{
				InputTokens:  500000,
				OutputTokens: 200000,
				TotalTokens:  700000,
			},
			CostUsage: &api.QuotaCostUsage{
				TotalCost:             4550,
				ToolCalls:             3000,
				ModelTierSeconds:      map[string]int64{"premium": 1250},
				ModelTierMilliseconds: map[string]int64{"premium": 1250000},
			},
		},
	}

	k8sObj := convert.CognitiveQuotaToK8s(in)
	out := convert.CognitiveQuotaFromK8s(k8sObj)

	// Spec
	if out.Spec.Hard["agents"] != 10 || out.Spec.Hard["tools"] != 50 {
		t.Errorf("Hard: %v", out.Spec.Hard)
	}
	if out.Spec.ResetInterval != "24h" {
		t.Errorf("ResetInterval: %q", out.Spec.ResetInterval)
	}
	if out.Spec.TokenBudget == nil {
		t.Fatal("TokenBudget lost")
	}
	if out.Spec.TokenBudget.MaxTotalTokens != 1500000 {
		t.Errorf("MaxTotalTokens: %d", out.Spec.TokenBudget.MaxTotalTokens)
	}
	if out.Spec.CostBudget == nil {
		t.Fatal("CostBudget lost")
	}
	if out.Spec.CostBudget.MaxCostPerPeriod != 10000 {
		t.Errorf("MaxCostPerPeriod: %d", out.Spec.CostBudget.MaxCostPerPeriod)
	}
	if out.Spec.CostBudget.MaxModelTierSeconds["premium"] != 3600 {
		t.Errorf("MaxModelTierSeconds: %v", out.Spec.CostBudget.MaxModelTierSeconds)
	}

	// Status
	if out.Status.Used["agents"] != 5 {
		t.Errorf("Used: %v", out.Status.Used)
	}
	if out.Status.LastResetTime != "2024-01-01T00:00:00Z" {
		t.Errorf("LastResetTime: %q", out.Status.LastResetTime)
	}
	if out.Status.TokenUsage == nil || out.Status.TokenUsage.TotalTokens != 700000 {
		t.Errorf("TokenUsage: %+v", out.Status.TokenUsage)
	}
	if out.Status.CostUsage == nil || out.Status.CostUsage.TotalCost != 4550 || out.Status.CostUsage.ToolCalls != 3000 || out.Status.CostUsage.ModelTierSeconds["premium"] != 1250 || out.Status.CostUsage.ModelTierMilliseconds["premium"] != 1250000 {
		t.Errorf("CostUsage: %+v", out.Status.CostUsage)
	}
}

// ======================== CognitiveLimitRange ========================

func TestCognitiveLimitRangeRoundTrip(t *testing.T) {
	in := &api.CognitiveLimitRange{
		TypeMeta:   api.TypeMeta{Kind: "CognitiveLimitRange", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "limits-1", Namespace: "ns"},
		Spec: api.CognitiveLimitRangeSpec{
			Limits: []api.LimitRangeItem{
				{
					Type:                  "Agent",
					Default:               map[string]int64{"tokens": 10000},
					Max:                   map[string]int64{"tokens": 100000},
					Min:                   map[string]int64{"tokens": 100},
					TokensPerSession:      50000,
					TimeoutSeconds:        300,
					MaxConcurrentSessions: 5,
				},
				{
					Type:             "Tool",
					Default:          map[string]int64{"calls": 100},
					TokensPerSession: 1000,
				},
			},
		},
	}

	k8sObj := convert.CognitiveLimitRangeToK8s(in)
	out := convert.CognitiveLimitRangeFromK8s(k8sObj)

	if len(out.Spec.Limits) != 2 {
		t.Fatalf("Limits: got %d", len(out.Spec.Limits))
	}
	l0 := out.Spec.Limits[0]
	if l0.Type != "Agent" {
		t.Errorf("Type: got %q", l0.Type)
	}
	if l0.Default["tokens"] != 10000 || l0.Max["tokens"] != 100000 || l0.Min["tokens"] != 100 {
		t.Errorf("Limits[0] maps: %v / %v / %v", l0.Default, l0.Max, l0.Min)
	}
	if l0.TokensPerSession != 50000 || l0.TimeoutSeconds != 300 || l0.MaxConcurrentSessions != 5 {
		t.Errorf("Limits[0] scalars: %d/%d/%d", l0.TokensPerSession, l0.TimeoutSeconds, l0.MaxConcurrentSessions)
	}
}

// ======================== Workflow: Full ========================

func TestWorkflowFullRoundTrip(t *testing.T) {
	maxTokens := int64(50000)
	in := &api.Workflow{
		TypeMeta:   api.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "full-wf", Namespace: "ns"},
		Spec: api.WorkflowSpec{
			Steps: []api.WorkflowStep{
				{Name: "start", AgentRef: api.LocalObjectReference{Name: "planner"}, Input: "plan this"},
				{Name: "exec", AgentRef: api.LocalObjectReference{Name: "executor"}, Condition: "steps.start.success == true"},
			},
			Edges: []api.WorkflowEdge{
				{From: "start", To: "exec", Condition: "steps.start.success"},
			},
			EntryPoint: "start",
			ApprovalGates: []api.WorkflowApprovalGate{
				{From: "start", To: "exec", RequiredApprovers: 2, Timeout: "30m"},
			},
			Budgets: &api.WorkflowBudget{
				MaxDuration:       "1h",
				MaxTokens:         maxTokens,
				MaxIterations:     10,
				MaxTokensPerCycle: 5000,
			},
			Emergent: &api.WorkflowEmergentSpec{
				Enabled: true,
				Budgets: api.WorkflowEmergentBudgets{MaxSteps: 5},
			},
			Parameters:        map[string]string{"env": "prod", "region": "us-east"},
			MaxParallelSteps:  4,
			FailFast:          true,
			ContinueOnError:   true,
			MaxConcurrentRuns: 3,
			HistoryLimit:      10,
		},
		Status: api.WorkflowStatus{
			Phase:          api.PhaseRunning,
			CompletedSteps: []string{"start"},
			ActiveStep:     "exec",
			ActiveSteps:    []string{"exec"},
			EntryPoints:    []string{"start", "audit"},
			StepStatuses: []api.WorkflowStepStatus{
				{Name: "start", Phase: api.PhaseReady, Output: "planned"},
				{Name: "exec", Phase: api.PhaseRunning},
			},
		},
	}

	k8sObj := convert.WorkflowToK8s(in)
	if got := k8sObj.Status.EntryPoints; len(got) != 2 || got[0] != "start" || got[1] != "audit" {
		t.Errorf("k8s EntryPoints: got %v, want [start audit]", got)
	}
	out := convert.WorkflowFromK8s(k8sObj)

	if out.Spec.EntryPoint != "start" {
		t.Errorf("EntryPoint: got %q", out.Spec.EntryPoint)
	}
	// Steps
	if len(out.Spec.Steps) != 2 {
		t.Fatalf("Steps: got %d", len(out.Spec.Steps))
	}
	if out.Spec.Steps[0].Input != "plan this" {
		t.Errorf("Steps[0].Input: got %q", out.Spec.Steps[0].Input)
	}
	// Edges
	if len(out.Spec.Edges) != 1 || out.Spec.Edges[0].From != "start" || out.Spec.Edges[0].To != "exec" {
		t.Errorf("Edges: %+v", out.Spec.Edges)
	}
	if out.Spec.Edges[0].Condition != "steps.start.success" {
		t.Errorf("Edges[0].Condition: got %q", out.Spec.Edges[0].Condition)
	}
	// ApprovalGates
	if len(out.Spec.ApprovalGates) != 1 || out.Spec.ApprovalGates[0].From != "start" || out.Spec.ApprovalGates[0].To != "exec" {
		t.Errorf("ApprovalGates: %+v", out.Spec.ApprovalGates)
	}
	if out.Spec.ApprovalGates[0].RequiredApprovers != 2 {
		t.Errorf("ApprovalGates[0].RequiredApprovers: got %d, want 2", out.Spec.ApprovalGates[0].RequiredApprovers)
	}
	if out.Spec.ApprovalGates[0].Timeout != "30m" {
		t.Errorf("ApprovalGates[0].Timeout: got %q, want %q", out.Spec.ApprovalGates[0].Timeout, "30m")
	}
	// Budgets: only MaxDuration and MaxTokens survive
	if out.Spec.Budgets == nil {
		t.Fatal("Budgets lost")
	}
	if out.Spec.Budgets.MaxDuration != "1h" {
		t.Errorf("MaxDuration: got %q", out.Spec.Budgets.MaxDuration)
	}
	if out.Spec.Budgets.MaxTokens != maxTokens {
		t.Errorf("MaxTokens: got %d", out.Spec.Budgets.MaxTokens)
	}
	if out.Spec.Budgets.MaxIterations != 10 {
		t.Errorf("MaxIterations: got %d, want 10", out.Spec.Budgets.MaxIterations)
	}
	if out.Spec.Budgets.MaxTokensPerCycle != 5000 {
		t.Errorf("MaxTokensPerCycle: got %d, want 5000", out.Spec.Budgets.MaxTokensPerCycle)
	}
	// Emergent: full struct survives
	if out.Spec.Emergent == nil || !out.Spec.Emergent.Enabled {
		t.Error("Emergent.Enabled lost")
	}
	if out.Spec.Emergent.Budgets.MaxSteps != 5 {
		t.Errorf("Emergent.Budgets.MaxSteps: got %d, want 5", out.Spec.Emergent.Budgets.MaxSteps)
	}
	// Status
	if out.Status.Phase != api.PhaseRunning {
		t.Errorf("Phase: %q", out.Status.Phase)
	}
	if len(out.Status.CompletedSteps) != 1 || out.Status.CompletedSteps[0] != "start" {
		t.Errorf("CompletedSteps: %v", out.Status.CompletedSteps)
	}
	if out.Status.ActiveStep != "exec" {
		t.Errorf("ActiveStep: %q", out.Status.ActiveStep)
	}
	// ActiveSteps
	if len(out.Status.ActiveSteps) != 1 || out.Status.ActiveSteps[0] != "exec" {
		t.Errorf("ActiveSteps: got %v, want [exec]", out.Status.ActiveSteps)
	}
	if len(out.Status.EntryPoints) != 2 || out.Status.EntryPoints[0] != "start" || out.Status.EntryPoints[1] != "audit" {
		t.Errorf("EntryPoints: got %v, want [start audit]", out.Status.EntryPoints)
	}
	// StepStatuses
	if len(out.Status.StepStatuses) != 2 {
		t.Fatalf("StepStatuses: got %d, want 2", len(out.Status.StepStatuses))
	}
	if out.Status.StepStatuses[0].Name != "start" || out.Status.StepStatuses[0].Phase != api.PhaseReady || out.Status.StepStatuses[0].Output != "planned" {
		t.Errorf("StepStatuses[0]: %+v", out.Status.StepStatuses[0])
	}
	if out.Status.StepStatuses[1].Name != "exec" || out.Status.StepStatuses[1].Phase != api.PhaseRunning {
		t.Errorf("StepStatuses[1]: %+v", out.Status.StepStatuses[1])
	}
	// New WorkflowSpec fields
	if len(out.Spec.Parameters) != 2 || out.Spec.Parameters["env"] != "prod" || out.Spec.Parameters["region"] != "us-east" {
		t.Errorf("Parameters: got %v", out.Spec.Parameters)
	}
	if out.Spec.MaxParallelSteps != 4 {
		t.Errorf("MaxParallelSteps: got %d, want 4", out.Spec.MaxParallelSteps)
	}
	if !out.Spec.FailFast {
		t.Error("FailFast: got false, want true")
	}
	if !out.Spec.ContinueOnError {
		t.Error("ContinueOnError: got false, want true")
	}
	if out.Spec.MaxConcurrentRuns != 3 {
		t.Errorf("MaxConcurrentRuns: got %d, want 3", out.Spec.MaxConcurrentRuns)
	}
	if out.Spec.HistoryLimit != 10 {
		t.Errorf("HistoryLimit: got %d, want 10", out.Spec.HistoryLimit)
	}
	// Step Condition
	if out.Spec.Steps[1].Condition != "steps.start.success == true" {
		t.Errorf("Steps[1].Condition: got %q, want %q", out.Spec.Steps[1].Condition, "steps.start.success == true")
	}
}

// TC-0265: WorkflowStep.Type and ToolRef survive a full to-k8s/from-k8s round trip.
func TestWorkflowStepToolRoundTrip(t *testing.T) {
	in := &api.Workflow{
		TypeMeta:   api.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "tool-wf", Namespace: "ns"},
		Spec: api.WorkflowSpec{
			EntryPoint: "fetch",
			Steps: []api.WorkflowStep{
				{
					Name:    "fetch",
					Type:    api.WorkflowStepTypeTool,
					ToolRef: api.LocalObjectReference{Name: "http-get"},
					Input:   `{"url":"https://example.com"}`,
				},
				{
					Name:     "summarize",
					Type:     api.WorkflowStepTypeAgent,
					AgentRef: api.LocalObjectReference{Name: "summarizer"},
					Input:    `{"text":"{{ .Steps.fetch.Output }}"}`,
				},
				{
					Name:     "implicit-agent",
					AgentRef: api.LocalObjectReference{Name: "planner"},
				},
			},
		},
	}

	out := convert.WorkflowFromK8s(convert.WorkflowToK8s(in))

	if len(out.Spec.Steps) != 3 {
		t.Fatalf("Steps: got %d, want 3", len(out.Spec.Steps))
	}
	if out.Spec.Steps[0].Type != api.WorkflowStepTypeTool {
		t.Errorf("Steps[0].Type: got %q, want %q", out.Spec.Steps[0].Type, api.WorkflowStepTypeTool)
	}
	if out.Spec.Steps[0].ToolRef.Name != "http-get" {
		t.Errorf("Steps[0].ToolRef.Name: got %q, want %q", out.Spec.Steps[0].ToolRef.Name, "http-get")
	}
	if out.Spec.Steps[0].AgentRef.Name != "" {
		t.Errorf("Steps[0].AgentRef.Name: got %q, want empty", out.Spec.Steps[0].AgentRef.Name)
	}
	if out.Spec.Steps[1].Type != api.WorkflowStepTypeAgent {
		t.Errorf("Steps[1].Type: got %q, want %q", out.Spec.Steps[1].Type, api.WorkflowStepTypeAgent)
	}
	if out.Spec.Steps[1].AgentRef.Name != "summarizer" {
		t.Errorf("Steps[1].AgentRef.Name: got %q, want %q", out.Spec.Steps[1].AgentRef.Name, "summarizer")
	}
	if out.Spec.Steps[1].ToolRef.Name != "" {
		t.Errorf("Steps[1].ToolRef.Name: got %q, want empty", out.Spec.Steps[1].ToolRef.Name)
	}
	// Step with no explicit Type stays empty (defaults to agent at validator/dispatch level).
	if out.Spec.Steps[2].Type != "" {
		t.Errorf("Steps[2].Type: got %q, want empty", out.Spec.Steps[2].Type)
	}
	if out.Spec.Steps[2].AgentRef.Name != "planner" {
		t.Errorf("Steps[2].AgentRef.Name: got %q", out.Spec.Steps[2].AgentRef.Name)
	}
}

// ======================== WorkflowRun: Steps ========================

func TestWorkflowRunStepsRoundTrip(t *testing.T) {
	in := &api.WorkflowRun{
		TypeMeta:   api.TypeMeta{Kind: "WorkflowRun", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "run-full", Namespace: "ns"},
		Spec: api.WorkflowRunSpec{
			WorkflowRef: api.LocalObjectReference{Name: "wf"},
			Input:       "process this",
		},
		Status: api.WorkflowRunStatus{
			Phase: api.PhaseRunning,
			StepStatuses: []api.WorkflowStepStatus{
				{Name: "step-a", Phase: "Completed", Output: "done"},
				{Name: "step-b", Phase: "Running", Output: "in progress"},
				{Name: "step-c", Phase: "Pending"},
			},
			CompletedSteps: []string{"step-a"},
			ActiveStep:     "step-b",
			ActiveSteps:    []string{"step-b"},
			SnapshotSpec: &api.WorkflowSpec{
				EntryPoint:        "step-a",
				Parameters:        map[string]string{"key": "val"},
				MaxConcurrentRuns: 2,
				HistoryLimit:      5,
			},
		},
	}

	k8sObj := convert.WorkflowRunToK8s(in)
	out := convert.WorkflowRunFromK8s(k8sObj)

	if out.Spec.Input != "process this" {
		t.Errorf("Input: got %q", out.Spec.Input)
	}
	// All step statuses are now properly round-tripped
	found := map[string]string{}
	for _, s := range out.Status.StepStatuses {
		found[s.Name] = string(s.Phase)
	}
	if found["step-a"] != "Completed" {
		t.Errorf("step-a: got %q", found["step-a"])
	}
	if found["step-b"] != "Running" {
		t.Errorf("step-b: got %q", found["step-b"])
	}
	if found["step-c"] != "Pending" {
		t.Errorf("step-c: got %q, want Pending", found["step-c"])
	}
	// Verify Output is preserved
	for _, s := range out.Status.StepStatuses {
		if s.Name == "step-a" && s.Output != "done" {
			t.Errorf("step-a output: got %q, want %q", s.Output, "done")
		}
	}
	// ActiveSteps round-trip
	if len(out.Status.ActiveSteps) != 1 || out.Status.ActiveSteps[0] != "step-b" {
		t.Errorf("ActiveSteps: got %v, want [step-b]", out.Status.ActiveSteps)
	}
	// SnapshotSpec round-trip
	if out.Status.SnapshotSpec == nil {
		t.Fatal("SnapshotSpec lost in round-trip")
	}
	if out.Status.SnapshotSpec.EntryPoint != "step-a" {
		t.Errorf("SnapshotSpec.EntryPoint: got %q, want %q", out.Status.SnapshotSpec.EntryPoint, "step-a")
	}
	if out.Status.SnapshotSpec.Parameters["key"] != "val" {
		t.Errorf("SnapshotSpec.Parameters: got %v", out.Status.SnapshotSpec.Parameters)
	}
	if out.Status.SnapshotSpec.MaxConcurrentRuns != 2 {
		t.Errorf("SnapshotSpec.MaxConcurrentRuns: got %d, want 2", out.Status.SnapshotSpec.MaxConcurrentRuns)
	}
	if out.Status.SnapshotSpec.HistoryLimit != 5 {
		t.Errorf("SnapshotSpec.HistoryLimit: got %d, want 5", out.Status.SnapshotSpec.HistoryLimit)
	}
}

func TestWorkflowRunStatusLifecycleFieldsRoundTrip(t *testing.T) {
	startTime := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	completionTime := startTime.Add(2 * time.Minute)
	in := &k8s.WorkflowRun{
		Status: k8s.WorkflowRunStatus{
			StartTime:      &metav1.Time{Time: startTime},
			CompletionTime: &metav1.Time{Time: completionTime},
			Output:         "final-result",
			ApprovalRecords: []k8s.WorkflowApprovalRecord{{
				Step:      "publish",
				Decision:  "approved",
				Approver:  "operator@example.com",
				Comment:   "ship it",
				Timestamp: startTime.Format(time.RFC3339),
			}},
		},
	}

	out := convert.WorkflowRunToK8s(convert.WorkflowRunFromK8s(in))

	if out.Status.StartTime == nil || !out.Status.StartTime.Time.Equal(startTime) {
		t.Fatalf("start time = %v, want %v", out.Status.StartTime, startTime)
	}
	if out.Status.CompletionTime == nil || !out.Status.CompletionTime.Time.Equal(completionTime) {
		t.Fatalf("completion time = %v, want %v", out.Status.CompletionTime, completionTime)
	}
	if out.Status.Output != "final-result" {
		t.Fatalf("output = %q, want final-result", out.Status.Output)
	}
	if !reflect.DeepEqual(out.Status.ApprovalRecords, in.Status.ApprovalRecords) {
		t.Fatalf("approval records = %#v, want %#v", out.Status.ApprovalRecords, in.Status.ApprovalRecords)
	}
}

// ======================== ModelClass: Full HealthProbe ========================

func TestModelClassHealthProbeFullRoundTrip(t *testing.T) {
	in := &api.ModelClass{
		TypeMeta:   api.TypeMeta{Kind: "ModelClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "model-1"},
		Spec: api.ModelClassSpec{
			Provisioner:   "azure-openai",
			LatencyClass:  "medium",
			CostTier:      "standard",
			ContextWindow: 32000,
			Modalities:    []string{"text"},
			VersionPin:    "2024-01",
			Parameters:    map[string]string{"region": "westus"},
			HealthProbe: &api.HealthProbeSpec{
				Endpoint:           "https://health.api",
				Type:               "latency-threshold",
				IntervalSeconds:    60,
				TimeoutSeconds:     10,
				LatencyThresholdMs: 3000,
				ErrorRateThreshold: 10,
				FailureThreshold:   5,
				SuccessThreshold:   3,
			},
		},
		Status: api.ModelClassStatus{
			Phase:                api.PhaseReady,
			Provisioned:          true,
			ActiveVersion:        "2024-01",
			Health:               "Healthy",
			LastProbeTime:        "2026-02-15T12:30:00Z",
			ConsecutiveFailures:  2,
			ConsecutiveSuccesses: 4,
			FallbackClass:        "fallback-model",
		},
	}

	k8sObj := convert.ModelClassToK8s(in)
	out := convert.ModelClassFromK8s(k8sObj)

	if out.Spec.ContextWindow != 32000 {
		t.Errorf("ContextWindow: %d", out.Spec.ContextWindow)
	}
	if len(out.Spec.Modalities) != 1 || out.Spec.Modalities[0] != "text" {
		t.Errorf("Modalities: %v", out.Spec.Modalities)
	}
	if out.Spec.HealthProbe.LatencyThresholdMs != 3000 {
		t.Errorf("LatencyThresholdMs: %d", out.Spec.HealthProbe.LatencyThresholdMs)
	}
	if out.Spec.HealthProbe.ErrorRateThreshold != 10 {
		t.Errorf("ErrorRateThreshold: %d", out.Spec.HealthProbe.ErrorRateThreshold)
	}
	if out.Spec.HealthProbe.SuccessThreshold != 3 {
		t.Errorf("SuccessThreshold: %d", out.Spec.HealthProbe.SuccessThreshold)
	}
	if out.Status.ActiveVersion != "2024-01" || out.Status.Health != "Healthy" {
		t.Errorf("Status: %+v", out.Status)
	}
	if out.Status.LastProbeTime != "2026-02-15T12:30:00Z" || out.Status.ConsecutiveFailures != 2 || out.Status.ConsecutiveSuccesses != 4 {
		t.Errorf("probe status: %+v", out.Status)
	}
}

// ======================== Edge Cases ========================

func TestNilInputsDoNotPanic(t *testing.T) {
	// Ensure nil probes/restart ladder don't cause panics in Agent conversion
	in := &api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "nil-agent"},
		Spec:       api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "m"}},
	}
	k8s := convert.AgentToK8s(in)
	out := convert.AgentFromK8s(k8s)
	if out.Spec.Probes != nil {
		t.Error("nil Probes should stay nil")
	}
	if out.Spec.RestartLadder != nil {
		t.Error("nil RestartLadder should stay nil")
	}
}

func TestToolNoSandboxNoSecretRef(t *testing.T) {
	in := &api.Tool{
		TypeMeta:   api.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "bare-tool"},
		Spec: api.ToolSpec{
			ClassName: "basic",
			Endpoint:  "http://t",
		},
	}
	k8s := convert.ToolToK8s(in)
	out := convert.ToolFromK8s(k8s)
	if out.Spec.Sandbox != nil {
		t.Error("nil Sandbox should stay nil")
	}
	if len(out.Spec.SecretRefs) != 0 {
		t.Error("empty SecretRefs should stay empty")
	}
}

func TestWorkflowNoBudgetsNoEmergent(t *testing.T) {
	in := &api.Workflow{
		TypeMeta:   api.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "simple-wf"},
		Spec: api.WorkflowSpec{
			Steps: []api.WorkflowStep{
				{Name: "only", AgentRef: api.LocalObjectReference{Name: "a"}},
			},
		},
	}
	k8s := convert.WorkflowToK8s(in)
	out := convert.WorkflowFromK8s(k8s)
	if out.Spec.Budgets != nil {
		t.Error("nil Budgets should stay nil")
	}
	if out.Spec.Emergent != nil {
		t.Error("nil Emergent should stay nil")
	}
}

func TestCognitiveQuotaNoBudgets(t *testing.T) {
	in := &api.CognitiveQuota{
		TypeMeta:   api.TypeMeta{Kind: "CognitiveQuota", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "bare-quota"},
		Spec: api.CognitiveQuotaSpec{
			Hard: map[string]int64{"agents": 5},
		},
	}
	k8s := convert.CognitiveQuotaToK8s(in)
	out := convert.CognitiveQuotaFromK8s(k8s)
	if out.Spec.TokenBudget != nil {
		t.Error("nil TokenBudget should stay nil")
	}
	if out.Spec.CostBudget != nil {
		t.Error("nil CostBudget should stay nil")
	}
	if out.Status.TokenUsage != nil {
		t.Error("nil TokenUsage should stay nil")
	}
}

func TestEmptySlicesPreserved(t *testing.T) {
	in := &api.AgentRole{
		TypeMeta:   api.TypeMeta{Kind: "AgentRole", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "empty-role"},
		Rules:      nil,
	}
	k8s := convert.AgentRoleToK8s(in)
	out := convert.AgentRoleFromK8s(k8s)
	if out.Rules != nil {
		t.Error("nil Rules should stay nil")
	}
}

// ======================== Model: Round-trip ========================

func TestModelRoundTrip(t *testing.T) {
	rateLimit := int64(60)
	in := &api.Model{
		TypeMeta:   api.TypeMeta{Kind: "Model", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "gpt-4o", Namespace: "ns"},
		Spec: api.ModelSpec{
			Provider:      "openai-compatible",
			ClassName:     "fast",
			ModelID:       "gpt-4o",
			Endpoint:      "https://api.example.com/v1",
			Capabilities:  []string{"chat", "tool-calling"},
			CredentialRef: &api.LocalObjectReference{Name: "creds"},
			RateLimit:     &rateLimit,
			Parameters:    map[string]string{"temperature": "0.7", "max_tokens": "1024"},
			Signature:     "sig-abc",
		},
		Status: api.ModelStatus{
			Phase:        api.PhaseReady,
			Available:    true,
			Latency:      "120ms",
			LastCheck:    "2024-01-01T00:00:00Z",
			Capabilities: []string{"chat"},
			Conditions: []api.Condition{
				{Type: "Ready", Status: "True", Reason: "OK", Message: "healthy"},
			},
		},
	}

	out := convert.ModelFromK8s(convert.ModelToK8s(in))

	if out.Spec.Provider != in.Spec.Provider {
		t.Errorf("Provider: got %q want %q", out.Spec.Provider, in.Spec.Provider)
	}
	if out.Spec.ClassName != in.Spec.ClassName || out.Spec.ModelID != in.Spec.ModelID {
		t.Errorf("ClassName/ModelID: got %q/%q", out.Spec.ClassName, out.Spec.ModelID)
	}
	if out.Spec.Endpoint != in.Spec.Endpoint {
		t.Errorf("Endpoint: got %q", out.Spec.Endpoint)
	}
	if len(out.Spec.Capabilities) != 2 || out.Spec.Capabilities[1] != "tool-calling" {
		t.Errorf("Capabilities: %+v", out.Spec.Capabilities)
	}
	if out.Spec.CredentialRef == nil || out.Spec.CredentialRef.Name != "creds" {
		t.Errorf("CredentialRef: %+v", out.Spec.CredentialRef)
	}
	if out.Spec.RateLimit == nil || *out.Spec.RateLimit != 60 {
		t.Errorf("RateLimit: %+v", out.Spec.RateLimit)
	}
	if out.Spec.Parameters["temperature"] != "0.7" || out.Spec.Parameters["max_tokens"] != "1024" {
		t.Errorf("Parameters: %+v", out.Spec.Parameters)
	}
	if out.Spec.Signature != "sig-abc" {
		t.Errorf("Signature: %q", out.Spec.Signature)
	}
	if out.Status.Phase != api.PhaseReady {
		t.Errorf("Status.Phase: %q", out.Status.Phase)
	}
	if !out.Status.Available || out.Status.Latency != "120ms" {
		t.Errorf("Status flags: %+v", out.Status)
	}
	if len(out.Status.Capabilities) != 1 || out.Status.Capabilities[0] != "chat" {
		t.Errorf("Status.Capabilities: %+v", out.Status.Capabilities)
	}
	if len(out.Status.Conditions) != 1 || out.Status.Conditions[0].Type != "Ready" || out.Status.Conditions[0].Reason != "OK" {
		t.Errorf("Status.Conditions: %+v", out.Status.Conditions)
	}
}

func TestModelRoundTripNilOptionalFields(t *testing.T) {
	in := &api.Model{
		TypeMeta:   api.TypeMeta{Kind: "Model", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "minimal"},
		Spec: api.ModelSpec{
			ClassName: "fast",
			ModelID:   "m",
		},
	}
	out := convert.ModelFromK8s(convert.ModelToK8s(in))
	if out.Spec.CredentialRef != nil {
		t.Errorf("CredentialRef should stay nil, got %+v", out.Spec.CredentialRef)
	}
	if out.Spec.RateLimit != nil {
		t.Errorf("RateLimit should stay nil, got %+v", out.Spec.RateLimit)
	}
	if out.Spec.Capabilities != nil {
		t.Errorf("Capabilities should stay nil, got %+v", out.Spec.Capabilities)
	}
	if out.Spec.Parameters != nil {
		t.Errorf("Parameters should stay nil, got %+v", out.Spec.Parameters)
	}
}

// ======================== SecretRef: Round-trip ========================

func TestSecretRefRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &api.SecretRef{
		TypeMeta:   api.TypeMeta{Kind: "SecretRef", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "sr-1", Namespace: "ns"},
		Spec: api.SecretRefSpec{
			BackendType: "vault",
			BackendRef:  "secret/data/app",
			Key:         "apiKey",
			Provider:    "legacy-provider",
			Path:        "legacy/path",
			LegacyKey:   "legacy-key",
		},
		Status: api.SecretRefStatus{
			Synced:       true,
			LastSyncTime: &now,
		},
	}

	out := convert.SecretRefFromK8s(convert.SecretRefToK8s(in))

	if out.Spec.BackendType != "vault" || out.Spec.BackendRef != "secret/data/app" || out.Spec.Key != "apiKey" {
		t.Errorf("canonical spec mismatch: %+v", out.Spec)
	}
	if out.Spec.Provider != "legacy-provider" || out.Spec.Path != "legacy/path" || out.Spec.LegacyKey != "legacy-key" {
		t.Errorf("legacy spec mismatch: %+v", out.Spec)
	}
	if !out.Status.Synced {
		t.Error("Status.Synced lost")
	}
	if out.Status.LastSyncTime == nil || !out.Status.LastSyncTime.Equal(now) {
		t.Errorf("LastSyncTime round-trip failed: got %+v want %+v", out.Status.LastSyncTime, now)
	}
}

func TestSecretRefRoundTripLegacyKeyMapping(t *testing.T) {
	// Verifies the api.LegacyKey <-> k8s.SecretKey field-name mapping.
	in := &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Name: "legacy"},
		Spec: api.SecretRefSpec{
			BackendType: "env",
			BackendRef:  "MY_VAR",
			LegacyKey:   "only-legacy",
		},
	}
	k8sObj := convert.SecretRefToK8s(in)
	if k8sObj.Spec.SecretKey != "only-legacy" {
		t.Errorf("LegacyKey should map to k8s SecretKey, got %q", k8sObj.Spec.SecretKey)
	}
	out := convert.SecretRefFromK8s(k8sObj)
	if out.Spec.LegacyKey != "only-legacy" {
		t.Errorf("SecretKey should map back to LegacyKey, got %q", out.Spec.LegacyKey)
	}
}

func TestSecretRefRoundTripNilLastSyncTime(t *testing.T) {
	in := &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Name: "no-sync"},
		Spec:       api.SecretRefSpec{BackendType: "env", BackendRef: "X", Key: "k"},
	}
	out := convert.SecretRefFromK8s(convert.SecretRefToK8s(in))
	if out.Status.LastSyncTime != nil {
		t.Errorf("LastSyncTime should stay nil, got %+v", out.Status.LastSyncTime)
	}
	if out.Status.Synced {
		t.Error("Synced should stay false")
	}
}
