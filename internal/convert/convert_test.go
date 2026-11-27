// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

func TestAgentRoundTrip(t *testing.T) {
	replicas := int32(3)
	in := &api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "my-agent", Namespace: "prod"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt4o"},
			Instructions: api.AgentInstructions{Inline: "You are helpful"},
			Tools: []api.AgentToolRef{
				{Ref: api.LocalObjectReference{Name: "search"}, Required: true},
			},
			Replicas: &replicas,
		},
		Status: api.AgentStatus{
			Phase:         api.PhaseReady,
			ReadyReplicas: 2,
		},
	}

	k8sObj := convert.AgentToK8s(in)
	out := convert.AgentFromK8s(k8sObj)

	if out.ObjectMeta.Name != in.ObjectMeta.Name {
		t.Errorf("Name: got %q, want %q", out.ObjectMeta.Name, in.ObjectMeta.Name)
	}
	if out.Spec.ModelRef.Name != "gpt4o" {
		t.Errorf("ModelRef: got %q, want %q", out.Spec.ModelRef.Name, "gpt4o")
	}
	if out.Spec.Replicas == nil || *out.Spec.Replicas != 3 {
		t.Error("Replicas lost in round-trip")
	}
	if out.Status.Phase != api.PhaseReady {
		t.Errorf("Phase: got %q, want %q", out.Status.Phase, api.PhaseReady)
	}
}

func TestAgentExecutionDependenciesRoundTrip(t *testing.T) {
	maxTokens := int64(8192)
	replicas := int32(2)
	in := &api.Agent{
		TypeMeta: api.TypeMeta{Kind: "Agent", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "incident-commander",
			Namespace:   "operations",
			Labels:      map[string]string{"team": "response"},
			Annotations: map[string]string{"owner": "sre"},
			Finalizers:  []string{"kognition.dev/agent-cleanup"},
		},
		Spec: api.AgentSpec{
			ModelRef: api.LocalObjectReference{Name: "reasoning-model"},
			Instructions: api.AgentInstructions{
				Inline: "Coordinate incident response.",
				Ref:    &api.LocalObjectReference{Name: "incident-policy"},
			},
			Tools: []api.AgentToolRef{
				{Ref: api.LocalObjectReference{Name: "status-page"}, Required: true},
				{Ref: api.LocalObjectReference{Name: "log-search"}},
			},
			Memory: &api.AgentMemorySpec{Claims: []api.LocalObjectReference{
				{Name: "incident-history"},
				{Name: "operator-notes"},
			}},
			Runtime: &api.AgentRuntimeSpec{
				Class:     "isolated",
				Timeout:   "5m",
				MaxTokens: &maxTokens,
			},
			Companions: []api.LocalObjectReference{
				{Name: "communications-agent"},
				{Name: "diagnostics-agent"},
			},
			KnowledgeBaseClaims: []api.KnowledgeBaseClaimRef{
				{Name: "runbooks", Required: true},
				{Name: "past-incidents"},
			},
			AutonomyClassRef: &api.LocalObjectReference{Name: "supervised-response"},
			Replicas:         &replicas,
		},
		Status: api.AgentStatus{Phase: api.PhaseReady, Replicas: 2, ReadyReplicas: 2, RevisionCount: 4},
	}

	out := convert.AgentFromK8s(convert.AgentToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("agent identity changed: got %#v, want %#v", out, in)
	}
	if !reflect.DeepEqual(out.ObjectMeta.Labels, in.ObjectMeta.Labels) || !reflect.DeepEqual(out.ObjectMeta.Annotations, in.ObjectMeta.Annotations) || !reflect.DeepEqual(out.ObjectMeta.Finalizers, in.ObjectMeta.Finalizers) {
		t.Errorf("agent metadata changed: got %#v, want %#v", out.ObjectMeta, in.ObjectMeta)
	}
	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("agent execution dependencies changed:\n got: %#v\nwant: %#v", out.Spec, in.Spec)
	}
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Errorf("agent status changed:\n got: %#v\nwant: %#v", out.Status, in.Status)
	}
}

func TestAgentInstanceRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	agentRevision := int64(7)
	in := &api.AgentInstance{
		TypeMeta:   api.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "my-agent-abc", Namespace: "prod"},
		Spec: api.AgentInstanceSpec{
			AgentRef:      api.LocalObjectReference{Name: "my-agent"},
			Revision:      1,
			AgentRevision: &agentRevision,
		},
		Status: api.AgentInstanceStatus{
			Phase:     api.PhaseRunning,
			StartTime: &now,
		},
	}

	k8sObj := convert.AgentInstanceToK8s(in)
	out := convert.AgentInstanceFromK8s(k8sObj)

	if out.Spec.AgentRef.Name != "my-agent" {
		t.Errorf("AgentRef: got %q, want %q", out.Spec.AgentRef.Name, "my-agent")
	}
	if out.Spec.AgentRevision == nil || *out.Spec.AgentRevision != agentRevision {
		t.Errorf("AgentRevision: got %v, want %d", out.Spec.AgentRevision, agentRevision)
	}
	if out.Status.Phase != api.PhaseRunning {
		t.Errorf("Phase: got %q, want %q", out.Status.Phase, api.PhaseRunning)
	}
}

func TestAgentInstanceProbeStatusRoundTrip(t *testing.T) {
	healthy := true
	ready := false
	reasoningHealthy := true
	lastProbeTime := "2026-08-01T16:45:00Z"
	lastReasoningScheduleSlot := "2026-08-01T16:40:00Z"
	lastLadderAction := api.RestartActionRollbackRevision
	rolledBackToRevision := int64(6)
	bindingRevision := int64(9)
	startTime := time.Date(2026, time.August, 1, 16, 30, 0, 0, time.UTC)
	transitionTime := startTime.Add(15 * time.Minute)
	in := &api.AgentInstance{
		TypeMeta: api.TypeMeta{Kind: "AgentInstance", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "support-agent-7",
			Namespace:   "production",
			Labels:      map[string]string{"agent": "support-agent"},
			Annotations: map[string]string{"trace": "trace-7"},
			Finalizers:  []string{"kognition.dev/agentinstance-cleanup"},
		},
		Spec: api.AgentInstanceSpec{
			AgentRef: api.LocalObjectReference{Name: "support-agent"},
			Revision: 7,
			SessionContext: map[string]string{
				"input":  "summarize the incident",
				"locale": "en-US",
			},
		},
		Status: api.AgentInstanceStatus{
			Phase:            api.PhaseRunning,
			DispatchAttempts: 3,
			SessionID:        "session-7",
			StartTime:        &startTime,
			TokenUsage: &api.TokenUsage{
				PromptTokens:     1200,
				CompletionTokens: 300,
				TotalTokens:      1500,
			},
			ToolCalls:    4,
			Output:       "incident summary",
			RestartCount: 2,
			Conditions: []api.Condition{{
				Type:               "ProbeHealthy",
				Status:             "True",
				LastTransitionTime: transitionTime,
				Reason:             "ProbeSucceeded",
				Message:            "health probe recovered",
			}},
			ProbeStatus: &api.ProbeStatus{
				Healthy:                       &healthy,
				Ready:                         &ready,
				ReasoningHealthy:              &reasoningHealthy,
				ConsecutiveHealthFailures:     1,
				ConsecutiveReadinessFailures:  2,
				ConsecutiveReasoningFailures:  3,
				ConsecutiveHealthSuccesses:    4,
				ConsecutiveReadinessSuccesses: 5,
				ConsecutiveReasoningSuccesses: 6,
				LadderStep:                    2,
				LadderAttempts:                1,
				LastProbeTime:                 &lastProbeTime,
				LastReasoningScheduleSlot:     &lastReasoningScheduleSlot,
				ReasoningOperationID:          "canary-operation-7",
				ReasoningRequestDigest:        "sha256:request-7",
				ReasoningBindings: []api.CanaryResourceBinding{{
					Kind:            "Model",
					Namespace:       "production",
					Name:            "model-7",
					UID:             "model-uid-7",
					ResourceVersion: "27",
					Revision:        &bindingRevision,
				}},
				LastReasoningResult: &api.ReasoningCanaryResult{
					OperationID:            "canary-operation-7",
					ExpectedBytes:          19,
					ActualBytes:            18,
					ExpectedSHA256:         "sha256:expected-7",
					ActualSHA256:           "sha256:actual-7",
					PromptTokens:           31,
					CompletionTokens:       7,
					TotalTokens:            38,
					ProviderDurationMillis: 125,
					TraceID:                "trace-7",
					SpanID:                 "span-7",
					Outcome:                api.CanaryOutcomeExecutionFailed,
					ClosedReason:           api.CanaryFailureProviderFailed,
				},
				LastLadderAction:     &lastLadderAction,
				RolledBackToRevision: &rolledBackToRevision,
			},
		},
	}

	out := convert.AgentInstanceFromK8s(convert.AgentInstanceToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("agent instance identity changed: got %#v, want %#v", out, in)
	}
	if !reflect.DeepEqual(out.ObjectMeta.Labels, in.ObjectMeta.Labels) || !reflect.DeepEqual(out.ObjectMeta.Annotations, in.ObjectMeta.Annotations) || !reflect.DeepEqual(out.ObjectMeta.Finalizers, in.ObjectMeta.Finalizers) {
		t.Errorf("agent instance metadata changed: got %#v, want %#v", out.ObjectMeta, in.ObjectMeta)
	}
	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("agent instance spec changed:\n got: %#v\nwant: %#v", out.Spec, in.Spec)
	}
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Errorf("agent instance status changed:\n got: %#v\nwant: %#v", out.Status, in.Status)
	}
}

func TestAgentInstanceProbeStatusRejectsMalformedTerminalResult(t *testing.T) {
	tests := []struct {
		name   string
		result k8s.ReasoningCanaryResult
	}{
		{
			name: "unknown outcome",
			result: k8s.ReasoningCanaryResult{
				Outcome:      k8s.CanaryOutcome("Unknown"),
				ClosedReason: k8s.CanaryFailureProviderFailed,
			},
		},
		{
			name: "unknown reason",
			result: k8s.ReasoningCanaryResult{
				Outcome:      k8s.CanaryOutcomeExecutionFailed,
				ClosedReason: k8s.CanaryFailureReason("UnknownReason"),
			},
		},
		{
			name: "execution failure without reason",
			result: k8s.ReasoningCanaryResult{
				Outcome: k8s.CanaryOutcomeExecutionFailed,
			},
		},
		{
			name: "passing outcome with failure reason",
			result: k8s.ReasoningCanaryResult{
				Outcome:      k8s.CanaryOutcomePassed,
				ClosedReason: k8s.CanaryFailureProviderFailed,
			},
		},
		{
			name: "mismatch outcome with failure reason",
			result: k8s.ReasoningCanaryResult{
				Outcome:      k8s.CanaryOutcomeMismatch,
				ClosedReason: k8s.CanaryFailureProviderFailed,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := &k8s.AgentInstance{
				Status: k8s.AgentInstanceStatus{
					ProbeStatus: &k8s.ProbeStatus{LastReasoningResult: &test.result},
				},
			}

			out := convert.AgentInstanceFromK8s(in)
			if out.Status.ProbeStatus == nil {
				t.Fatal("probe status was discarded with malformed terminal result")
			}
			if out.Status.ProbeStatus.LastReasoningResult != nil {
				t.Fatalf("malformed terminal result was preserved: %#v", out.Status.ProbeStatus.LastReasoningResult)
			}
		})
	}
}

func TestAgentInstanceProbeStatusRejectsMalformedInternalTerminalResult(t *testing.T) {
	tests := []struct {
		name   string
		result api.ReasoningCanaryResult
	}{
		{name: "unknown outcome", result: api.ReasoningCanaryResult{Outcome: api.CanaryOutcome("Unknown")}},
		{name: "unknown reason", result: api.ReasoningCanaryResult{Outcome: api.CanaryOutcomeExecutionFailed, ClosedReason: api.CanaryFailureReason(255)}},
		{name: "execution failure without reason", result: api.ReasoningCanaryResult{Outcome: api.CanaryOutcomeExecutionFailed}},
		{name: "passing outcome with failure reason", result: api.ReasoningCanaryResult{Outcome: api.CanaryOutcomePassed, ClosedReason: api.CanaryFailureProviderFailed}},
		{name: "mismatch outcome with failure reason", result: api.ReasoningCanaryResult{Outcome: api.CanaryOutcomeMismatch, ClosedReason: api.CanaryFailureProviderFailed}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := &api.AgentInstance{
				Status: api.AgentInstanceStatus{
					ProbeStatus: &api.ProbeStatus{LastReasoningResult: &test.result},
				},
			}

			out := convert.AgentInstanceToK8s(in)
			if out.Status.ProbeStatus == nil {
				t.Fatal("probe status was discarded with malformed terminal result")
			}
			if out.Status.ProbeStatus.LastReasoningResult != nil {
				t.Fatalf("malformed terminal result was preserved: %#v", out.Status.ProbeStatus.LastReasoningResult)
			}
		})
	}
}

func TestAgentInstanceProbeStatusConversionDoesNotAliasCanaryPointers(t *testing.T) {
	internalScheduleSlot := "2026-08-01T16:40:00Z"
	internalRevision := int64(9)
	internalObject := &api.AgentInstance{
		Status: api.AgentInstanceStatus{
			ProbeStatus: &api.ProbeStatus{
				LastReasoningScheduleSlot: &internalScheduleSlot,
				ReasoningBindings: []api.CanaryResourceBinding{{
					Revision: &internalRevision,
				}},
			},
		},
	}

	k8sObject := convert.AgentInstanceToK8s(internalObject)
	*k8sObject.Status.ProbeStatus.LastReasoningScheduleSlot = "mutated"
	*k8sObject.Status.ProbeStatus.ReasoningBindings[0].Revision = 10
	if *internalObject.Status.ProbeStatus.LastReasoningScheduleSlot != "2026-08-01T16:40:00Z" {
		t.Fatal("AgentInstanceToK8s aliases the reasoning schedule slot")
	}
	if *internalObject.Status.ProbeStatus.ReasoningBindings[0].Revision != 9 {
		t.Fatal("AgentInstanceToK8s aliases the reasoning binding revision")
	}

	k8sScheduleSlot := "2026-08-01T17:40:00Z"
	k8sRevision := int64(11)
	k8sSource := &k8s.AgentInstance{
		Status: k8s.AgentInstanceStatus{
			ProbeStatus: &k8s.ProbeStatus{
				LastReasoningScheduleSlot: &k8sScheduleSlot,
				ReasoningBindings: []k8s.CanaryResourceBinding{{
					Revision: &k8sRevision,
				}},
			},
		},
	}

	internalResult := convert.AgentInstanceFromK8s(k8sSource)
	*internalResult.Status.ProbeStatus.LastReasoningScheduleSlot = "mutated"
	*internalResult.Status.ProbeStatus.ReasoningBindings[0].Revision = 12
	if *k8sSource.Status.ProbeStatus.LastReasoningScheduleSlot != "2026-08-01T17:40:00Z" {
		t.Fatal("AgentInstanceFromK8s aliases the reasoning schedule slot")
	}
	if *k8sSource.Status.ProbeStatus.ReasoningBindings[0].Revision != 11 {
		t.Fatal("AgentInstanceFromK8s aliases the reasoning binding revision")
	}
}

func TestAgentJobRoundTrip(t *testing.T) {
	backoffLimit := int32(4)
	agentRevision := int64(7)
	completionTime := time.Date(2026, time.August, 1, 12, 30, 0, 0, time.UTC)
	startTime := completionTime.Add(-2 * time.Second)
	transitionTime := completionTime.Add(-time.Minute)
	in := &api.AgentJob{
		TypeMeta: api.TypeMeta{Kind: "AgentJob", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "summarize-report",
			Namespace:   "operations",
			Labels:      map[string]string{"workflow": "daily-report"},
			Annotations: map[string]string{"trace": "trace-123"},
			Finalizers:  []string{"kognition.dev/agentjob-cleanup"},
		},
		Spec: api.AgentJobSpec{
			AgentRef:      &api.LocalObjectReference{Name: "report-agent"},
			AgentRevision: &agentRevision,
			AgentSpec: &api.AgentSpec{
				ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
				Instructions: api.AgentInstructions{Inline: "Summarize the report."},
			},
			CompletionPolicy: api.CompletionPolicyAny,
			BackoffLimit:     &backoffLimit,
			Input:            "quarterly results",
			MemoryClaimRef:   &api.LocalObjectReference{Name: "report-memory"},
		},
		Status: api.AgentJobStatus{
			Phase: api.PhaseReady,
			Conditions: []api.Condition{{
				Type:               "Complete",
				Status:             "True",
				LastTransitionTime: transitionTime,
				Reason:             "ExecutionSucceeded",
				Message:            "agent execution completed",
			}},
			Succeeded:      1,
			Failed:         2,
			StartTime:      &startTime,
			CompletionTime: &completionTime,
			TokenUsage:     &api.TokenUsage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
			ToolCalls:      3,
			Output:         "Revenue increased by 12%.",
		},
	}

	out := convert.AgentJobFromK8s(convert.AgentJobToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("identity metadata changed: got %#v, want %#v", out, in)
	}
	if out.ObjectMeta.Labels["workflow"] != "daily-report" || out.ObjectMeta.Annotations["trace"] != "trace-123" || len(out.ObjectMeta.Finalizers) != 1 {
		t.Errorf("object metadata changed: %#v", out.ObjectMeta)
	}
	if out.Spec.AgentRef == nil || out.Spec.AgentRef.Name != "report-agent" || out.Spec.MemoryClaimRef == nil || out.Spec.MemoryClaimRef.Name != "report-memory" {
		t.Errorf("resource references changed: %#v", out.Spec)
	}
	if out.Spec.AgentSpec == nil || out.Spec.AgentSpec.ModelRef.Name != "gpt-4o" || out.Spec.AgentSpec.Instructions.Inline != "Summarize the report." {
		t.Errorf("inline agent spec changed: %#v", out.Spec.AgentSpec)
	}
	if out.Spec.CompletionPolicy != api.CompletionPolicyAny || out.Spec.BackoffLimit == nil || *out.Spec.BackoffLimit != 4 || out.Spec.Input != "quarterly results" {
		t.Errorf("job execution spec changed: %#v", out.Spec)
	}
	if out.Status.Phase != api.PhaseReady || out.Status.Succeeded != 1 || out.Status.Failed != 2 || out.Status.Output != "Revenue increased by 12%." || out.Status.ToolCalls != 3 {
		t.Errorf("terminal status changed: %#v", out.Status)
	}
	if out.Status.StartTime == nil || !out.Status.StartTime.Equal(startTime) {
		t.Errorf("start time changed: %#v", out.Status.StartTime)
	}
	if out.Status.CompletionTime == nil || !out.Status.CompletionTime.Equal(completionTime) {
		t.Errorf("completion time changed: %#v", out.Status.CompletionTime)
	}
	if out.Status.TokenUsage == nil || out.Status.TokenUsage.TotalTokens != 150 {
		t.Errorf("token usage changed: %#v", out.Status.TokenUsage)
	}
	if len(out.Status.Conditions) != 1 || out.Status.Conditions[0] != in.Status.Conditions[0] {
		t.Errorf("conditions changed: %#v", out.Status.Conditions)
	}
}

func TestApprovalDecisionRoundTrip(t *testing.T) {
	observedAt := time.Date(2026, time.August, 2, 9, 15, 0, 0, time.UTC)
	in := &api.ApprovalDecision{
		TypeMeta: api.TypeMeta{Kind: "ApprovalDecision", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "approve-deploy-alice",
			Namespace:   "production",
			Labels:      map[string]string{"gate": "deploy"},
			Annotations: map[string]string{"ticket": "CHG-1234"},
		},
		Spec: api.ApprovalDecisionSpec{
			WorkflowRunRef: api.LocalObjectReference{Name: "release-2026-08-02"},
			Step:           "deploy",
			Iteration:      3,
			Decision:       api.ApprovalDecisionApprove,
			Approver:       "alice@example.com",
			Reason:         "change window approved",
		},
		Status: api.ApprovalDecisionStatus{
			Observed:   true,
			ObservedAt: &observedAt,
		},
	}

	out := convert.ApprovalDecisionFromK8s(convert.ApprovalDecisionToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("identity metadata changed: got %#v, want %#v", out, in)
	}
	if out.ObjectMeta.Labels["gate"] != "deploy" || out.ObjectMeta.Annotations["ticket"] != "CHG-1234" {
		t.Errorf("object metadata changed: %#v", out.ObjectMeta)
	}
	if out.Spec != in.Spec {
		t.Errorf("approval scope or decision changed: got %#v, want %#v", out.Spec, in.Spec)
	}
	if !out.Status.Observed || out.Status.ObservedAt == nil || !out.Status.ObservedAt.Equal(observedAt) {
		t.Errorf("observation audit status changed: %#v", out.Status)
	}
	if convert.ApprovalDecisionToK8s(nil) != nil || convert.ApprovalDecisionFromK8s(nil) != nil {
		t.Error("nil approval decisions must remain nil")
	}
}

func TestModelAutoscalerRoundTrip(t *testing.T) {
	lastScaleTime := time.Date(2026, time.August, 1, 10, 45, 0, 0, time.UTC)
	transitionTime := lastScaleTime.Add(30 * time.Second)
	in := &api.ModelAutoscaler{
		TypeMeta: api.TypeMeta{Kind: "ModelAutoscaler", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "writer-model-scale",
			Namespace:   "production",
			Labels:      map[string]string{"agent": "writer"},
			Annotations: map[string]string{"policy": "fallback-on-degradation"},
			Finalizers:  []string{"kognition.dev/model-autoscaler-cleanup"},
		},
		Spec: api.ModelAutoscalerSpec{
			AgentRef:                   api.LocalObjectReference{Name: "writer"},
			PreferredModelRef:          api.LocalObjectReference{Name: "model-primary"},
			StabilizationWindowSeconds: 300,
		},
		Status: api.ModelAutoscalerStatus{
			CurrentModelRef: "model-fallback",
			DesiredModelRef: "model-primary",
			LastScaleTime:   &lastScaleTime,
			Conditions: []api.Condition{{
				Type:               "Ready",
				Status:             "False",
				LastTransitionTime: transitionTime,
				Reason:             "ScalingBlocked",
				Message:            "stabilization window active",
			}},
		},
	}

	out := convert.ModelAutoscalerFromK8s(convert.ModelAutoscalerToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("identity metadata changed: got %#v, want %#v", out, in)
	}
	if out.ObjectMeta.Labels["agent"] != "writer" || out.ObjectMeta.Annotations["policy"] != "fallback-on-degradation" || len(out.ObjectMeta.Finalizers) != 1 {
		t.Errorf("object metadata changed: %#v", out.ObjectMeta)
	}
	if out.Spec != in.Spec {
		t.Errorf("autoscaling policy changed: got %#v, want %#v", out.Spec, in.Spec)
	}
	if out.Status.CurrentModelRef != "model-fallback" || out.Status.DesiredModelRef != "model-primary" {
		t.Errorf("model transition changed: %#v", out.Status)
	}
	if out.Status.LastScaleTime == nil || !out.Status.LastScaleTime.Equal(lastScaleTime) {
		t.Errorf("last scale time changed: %#v", out.Status.LastScaleTime)
	}
	if len(out.Status.Conditions) != 1 || out.Status.Conditions[0] != in.Status.Conditions[0] {
		t.Errorf("autoscaler conditions changed: %#v", out.Status.Conditions)
	}
}

func TestToolRoundTrip(t *testing.T) {
	in := &api.Tool{
		TypeMeta:   api.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec: api.ToolSpec{
			ClassName:   "mcp-class",
			Description: "Searches the web",
			Endpoint:    "https://api.search.io",
		},
	}

	k8sObj := convert.ToolToK8s(in)
	out := convert.ToolFromK8s(k8sObj)

	if out.Spec.ClassName != "mcp-class" {
		t.Errorf("ClassName: got %q, want %q", out.Spec.ClassName, "mcp-class")
	}
	if out.Spec.Description != in.Spec.Description {
		t.Errorf("Description: got %q, want %q", out.Spec.Description, in.Spec.Description)
	}
}

// TestToolRoundTrip_PreservesAllFields guards against silent field-dropping in
// the converter (the cause of OpenAPISchemaMissing for tools with a valid schema).
func TestToolRoundTrip_PreservesAllFields(t *testing.T) {
	in := &api.Tool{
		TypeMeta:   api.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "weather-api", Namespace: "default"},
		Spec: api.ToolSpec{
			ClassName:   "openapi-class",
			Protocol:    api.ToolProtocolOpenAPI,
			Description: "Weather",
			Endpoint:    "https://api.weather.example.com/v1",
			Schema: map[string]any{
				"openapi": "3.0.0",
				"info":    map[string]any{"title": "Weather API", "version": "1.0.0"},
			},
			InputSchema: map[string]any{
				"type":     "object",
				"required": []any{"location"},
			},
			DigestPin: "sha256:deadbeef",
			Signature: "cosign-sig",
			Trusted:   true,
			HealthProbe: &api.ToolHealthProbe{
				Type:             api.ProbeTypeExec,
				Command:          []string{"healthcheck", "--ready"},
				Interval:         "30s",
				Timeout:          "5s",
				FailureThreshold: 3,
				SuccessThreshold: 1,
			},
			ExecutionPolicy: &api.ToolExecutionPolicy{
				Timeout: "3s", MaxConcurrent: 4, Idempotent: true, RetryMaxAttempts: 2,
				CircuitBreaker: &api.ToolCircuitBreaker{FailureThreshold: 3, Cooldown: "1m"},
			},
		},
	}

	out := convert.ToolFromK8s(convert.ToolToK8s(in))

	if got := out.Spec.Schema["openapi"]; got != "3.0.0" {
		t.Errorf("Schema dropped/changed: got %v", got)
	}
	if out.Spec.InputSchema["type"] != "object" {
		t.Errorf("InputSchema dropped/changed: got %v", out.Spec.InputSchema)
	}
	if out.Spec.DigestPin != "sha256:deadbeef" {
		t.Errorf("DigestPin dropped: got %q", out.Spec.DigestPin)
	}
	if out.Spec.Signature != "cosign-sig" {
		t.Errorf("Signature dropped: got %q", out.Spec.Signature)
	}
	if !out.Spec.Trusted {
		t.Errorf("Trusted dropped")
	}
	if out.Spec.HealthProbe == nil {
		t.Fatalf("HealthProbe dropped")
	}
	if out.Spec.HealthProbe.Type != api.ProbeTypeExec || out.Spec.HealthProbe.FailureThreshold != 3 {
		t.Errorf("HealthProbe fields not preserved: %+v", out.Spec.HealthProbe)
	}
	if got := out.Spec.HealthProbe.Command; len(got) != 2 || got[0] != "healthcheck" || got[1] != "--ready" {
		t.Errorf("HealthProbe command not preserved: %v", got)
	}
	if out.Spec.ExecutionPolicy == nil || out.Spec.ExecutionPolicy.MaxConcurrent != 4 || out.Spec.ExecutionPolicy.CircuitBreaker == nil || out.Spec.ExecutionPolicy.CircuitBreaker.Cooldown != "1m" {
		t.Errorf("ExecutionPolicy not preserved: %+v", out.Spec.ExecutionPolicy)
	}
}

func TestAgentIdentityRoundTrip(t *testing.T) {
	in := &api.AgentIdentity{
		TypeMeta:   api.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "my-id", Namespace: "default"},
		Spec: api.AgentIdentitySpec{
			AgentRef: api.LocalObjectReference{Name: "my-agent"},
			SPIFFEID: "spiffe://cluster/ns/default/agent/my-agent",
		},
		Status: api.AgentIdentityStatus{WorkloadIdentity: &api.WorkloadIdentityStatus{
			AgentUID: "70000000-0000-4000-8000-000000000001", SPIFFEID: "spiffe://cluster/ns/default/agent/my-agent/incarnation/70000000-0000-4000-8000-000000000001",
			TrustDomain: "cluster", SourceGeneration: 2, Ready: true, Reason: "Ready",
		}},
	}

	k8sObj := convert.AgentIdentityToK8s(in)
	out := convert.AgentIdentityFromK8s(k8sObj)

	if out.Spec.AgentRef.Name != "my-agent" {
		t.Errorf("AgentRef: got %q, want %q", out.Spec.AgentRef.Name, "my-agent")
	}
	if out.Spec.SPIFFEID != "spiffe://cluster/ns/default/agent/my-agent" {
		t.Errorf("SPIFFEID: got %q, want %q", out.Spec.SPIFFEID, "spiffe://cluster/ns/default/agent/my-agent")
	}
	if out.Status.WorkloadIdentity == nil || *out.Status.WorkloadIdentity != *in.Status.WorkloadIdentity {
		t.Fatal("certificate metadata lost during conversion")
	}
	k8sObj.Status.WorkloadIdentity.Ready = false
	if !in.Status.WorkloadIdentity.Ready || !out.Status.WorkloadIdentity.Ready {
		t.Fatal("conversion shared mutable certificate metadata")
	}
	in.Status.WorkloadIdentity = nil
	if convert.AgentIdentityFromK8s(convert.AgentIdentityToK8s(in)).Status.WorkloadIdentity != nil {
		t.Fatal("legacy identity gained certificate evidence during conversion")
	}
}

func TestAgentPolicyRoundTrip(t *testing.T) {
	in := &api.AgentPolicy{
		TypeMeta:   api.TypeMeta{Kind: "AgentPolicy", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "no-prod-writes", Namespace: "default"},
		Spec: api.AgentPolicySpec{
			Rules:   "package kognition\ndeny_writes { true }",
			Enforce: true,
			Scope:   "workspace",
		},
	}

	k8sObj := convert.AgentPolicyToK8s(in)
	out := convert.AgentPolicyFromK8s(k8sObj)

	if out.Spec.Rules != in.Spec.Rules {
		t.Errorf("Rules: got %q, want %q", out.Spec.Rules, in.Spec.Rules)
	}
	if out.Spec.Enforce != true {
		t.Error("Enforce lost in round-trip")
	}
}

func TestWorkflowRunRoundTrip(t *testing.T) {
	in := &api.WorkflowRun{
		TypeMeta:   api.TypeMeta{Kind: "WorkflowRun", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "run-1", Namespace: "default"},
		Spec: api.WorkflowRunSpec{
			WorkflowRef: api.LocalObjectReference{Name: "my-workflow"},
		},
		Status: api.WorkflowRunStatus{
			Phase: api.PhaseRunning,
		},
	}

	k8sObj := convert.WorkflowRunToK8s(in)
	out := convert.WorkflowRunFromK8s(k8sObj)

	if out.Spec.WorkflowRef.Name != "my-workflow" {
		t.Errorf("WorkflowRef: got %q, want %q", out.Spec.WorkflowRef.Name, "my-workflow")
	}
	if out.Status.Phase != api.PhaseRunning {
		t.Errorf("Phase: got %q, want %q", out.Status.Phase, api.PhaseRunning)
	}
}

func TestModelClassRoundTrip(t *testing.T) {
	threshold := int32(5)
	in := &api.ModelClass{
		TypeMeta:   api.TypeMeta{Kind: "ModelClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "gpt4o", Namespace: "default"},
		Spec: api.ModelClassSpec{
			Provisioner:   "openai",
			Parameters:    map[string]string{"region": "eastus"},
			LatencyClass:  "low",
			CostTier:      "premium",
			ContextWindow: 128000,
			Modalities:    []string{"text", "vision"},
			VersionPin:    "2024-05-13",
			HealthProbe: &api.HealthProbeSpec{
				Endpoint:           "https://api.openai.com/health",
				Type:               "latency-threshold",
				IntervalSeconds:    30,
				TimeoutSeconds:     5,
				LatencyThresholdMs: 2000,
				ErrorRateThreshold: &threshold,
				FailureThreshold:   3,
				SuccessThreshold:   2,
			},
		},
		Status: api.ModelClassStatus{
			Phase:         api.PhaseReady,
			Provisioned:   true,
			ActiveVersion: "2024-05-13",
			Health:        "Healthy",
			FallbackClass: "gpt35",
		},
	}

	binding := api.ModelVersionBinding{Namespace: "tenant", ModelName: "candidate", ModelUID: "uid", ModelResourceVersion: 7, SpecDigest: "digest"}
	in.Spec.VersionCandidates = map[string]api.ModelVersionBinding{"version": binding}
	in.Status.ActiveBinding = &binding
	k8sObj := convert.ModelClassToK8s(in)
	out := convert.ModelClassFromK8s(k8sObj)
	if !reflect.DeepEqual(in.Spec.VersionCandidates, out.Spec.VersionCandidates) || !reflect.DeepEqual(in.Status.ActiveBinding, out.Status.ActiveBinding) {
		t.Fatal("candidate binding lost in ModelClass conversion")
	}

	if out.Spec.Provisioner != in.Spec.Provisioner {
		t.Errorf("Provisioner: got %q, want %q", out.Spec.Provisioner, in.Spec.Provisioner)
	}
	if out.Spec.HealthProbe == nil {
		t.Fatal("HealthProbe lost in round-trip")
	}
	if !reflect.DeepEqual(out.Spec.HealthProbe.ErrorRateThreshold, in.Spec.HealthProbe.ErrorRateThreshold) {
		t.Errorf("ErrorRateThreshold: got %v, want %v", out.Spec.HealthProbe.ErrorRateThreshold, in.Spec.HealthProbe.ErrorRateThreshold)
	}
	if out.Status.Phase != in.Status.Phase {
		t.Errorf("Phase: got %q, want %q", out.Status.Phase, in.Status.Phase)
	}
	if out.Status.FallbackClass != in.Status.FallbackClass {
		t.Errorf("FallbackClass: got %q, want %q", out.Status.FallbackClass, in.Status.FallbackClass)
	}
}

func TestModelClassErrorRateThresholdSerializationAndConversion(t *testing.T) {
	zero, fifty, hundred := int32(0), int32(50), int32(100)
	for _, tc := range []struct {
		name      string
		field     string
		threshold *int32
	}{
		{"omitted", "", nil},
		{"zero", `,"errorRateThreshold":0`, &zero},
		{"fifty", `,"errorRateThreshold":50`, &fifty},
		{"hundred", `,"errorRateThreshold":100`, &hundred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in api.ModelClass
			raw := []byte(`{"spec":{"healthProbe":{"endpoint":"https://api.example/health"` + tc.field + `}}}`)
			if err := json.Unmarshal(raw, &in); err != nil {
				t.Fatal(err)
			}
			k8sObj := convert.ModelClassToK8s(&in)
			copied := k8sObj.DeepCopy()
			out := convert.ModelClassFromK8s(k8sObj)
			if !reflect.DeepEqual(in.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) ||
				!reflect.DeepEqual(k8sObj.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) ||
				!reflect.DeepEqual(copied.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) ||
				!reflect.DeepEqual(out.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) {
				t.Fatal("threshold presence or value changed in conversion or deepcopy")
			}
			for _, object := range []any{&in, k8sObj, copied, out} {
				data, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				var decoded api.ModelClass
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(decoded.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) {
					t.Fatalf("threshold presence or value changed in JSON: %s", data)
				}
			}
			data, err := yaml.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			var decoded api.ModelClass
			if err := yaml.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) {
				t.Fatalf("threshold presence or value changed in YAML: %s", data)
			}
			if tc.threshold != nil {
				*k8sObj.Spec.HealthProbe.ErrorRateThreshold = 99
				if !reflect.DeepEqual(in.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) ||
					!reflect.DeepEqual(copied.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) ||
					!reflect.DeepEqual(out.Spec.HealthProbe.ErrorRateThreshold, tc.threshold) {
					t.Fatal("conversion or deepcopy shared the threshold pointer")
				}
			}
		})
	}
}

func TestAutonomyClassRoundTrip(t *testing.T) {
	in := &api.AutonomyClass{
		TypeMeta:   api.TypeMeta{Kind: "AutonomyClass", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "supervised", Labels: map[string]string{"tier": "safe"}},
		Spec: api.AutonomyClassSpec{
			Level:            10,
			RequiresApproval: true,
			MaxCostPerAction: 250,
			AllowedTools:     []string{"readonly", "search"},
			ApprovalPolicy: &api.ApprovalPolicy{
				ToolInvocation:        "high-risk-only",
				MemoryWrite:           "always",
				ExternalCommunication: "always",
				CostThreshold:         100,
			},
			Constraints: &api.AutonomyConstraints{
				MaxConcurrentActions:    2,
				MaxTokenBudgetPerAction: 4096,
			},
		},
	}

	k8sObj := convert.AutonomyClassToK8s(in)
	k8sObj.Spec.AllowedTools[0] = "mutated"
	if in.Spec.AllowedTools[0] != "readonly" {
		t.Fatalf("AutonomyClassToK8s aliases AllowedTools storage: got source %q", in.Spec.AllowedTools[0])
	}
	k8sObj.Spec.AllowedTools[0] = "readonly"
	out := convert.AutonomyClassFromK8s(k8sObj)

	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	out.Spec.AllowedTools[0] = "mutated"
	if k8sObj.Spec.AllowedTools[0] != "readonly" {
		t.Fatalf("AutonomyClassFromK8s aliases AllowedTools storage: got source %q", k8sObj.Spec.AllowedTools[0])
	}
}

func TestTrustProfileRoundTrip(t *testing.T) {
	in := &api.TrustProfile{
		TypeMeta:   api.TypeMeta{Kind: "TrustProfile", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "agent-trust", Namespace: "demo"},
		Spec: api.TrustProfileSpec{
			AgentRef: api.LocalObjectReference{Name: "agent"}, InitialScore: 70,
			InitialClass: "supervised", MaxClass: "autonomous", EvaluationInterval: "1h",
			ScoringWeights:  &api.ScoringWeights{SuccessWeight: 1, FailureWeight: -2},
			PromotionPolicy: &api.PromotionPolicy{RequireReview: true, MinActions: 10},
			DemotionPolicy:  &api.DemotionPolicy{Mode: "immediate", FallbackClass: "restricted"},
		},
		Status: api.TrustProfileStatus{
			CurrentScore: 75, CurrentTier: "trusted", CurrentClass: "supervised", TotalEvaluations: 3,
			ScoreHistory:  []api.ScoreEntry{{Timestamp: "2026-01-01T00:00:00Z", Score: 75, Reason: "success"}},
			ApprovalStats: &api.ApprovalStats{TotalRequested: 2, TotalApproved: 1},
			Conditions:    []api.StatusCondition{{Type: "Ready", Status: "True"}},
		},
	}

	k8sObj := convert.TrustProfileToK8s(in)
	out := convert.TrustProfileFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Status.ScoreHistory[0].Reason = "mutated"
	k8sObj.Status.Conditions[0].Status = "False"
	if in.Status.ScoreHistory[0].Reason != "success" || in.Status.Conditions[0].Status != "True" {
		t.Fatal("TrustProfileToK8s aliases status slice storage")
	}
	out.Status.ScoreHistory[0].Reason = "roundtrip-mutated"
	if k8sObj.Status.ScoreHistory[0].Reason != "mutated" {
		t.Fatal("TrustProfileFromK8s aliases status slice storage")
	}
}

func TestRuntimeApprovalRoundTripPreservesFoundationFields(t *testing.T) {
	decidedAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	preparationDeadline := decidedAt.Add(30 * time.Second)
	leaseDeadline := decidedAt.Add(time.Minute)
	ref := func(kind, scope, namespace, name string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: scope, Namespace: namespace, Name: name, UID: name + "-uid", ResourceVersion: 7}
	}
	in := &api.RuntimeApproval{
		TypeMeta:   api.TypeMeta{Kind: "RuntimeApproval", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "ra-test", Namespace: "team-a", UID: "approval-uid", ResourceVersion: 3},
		Spec: api.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: "invocation-a", InitialDecision: api.RuntimeApprovalDecisionPending, Plane: api.RuntimeApprovalPlaneTool, Operation: "invoke",
			AgentRef: ref("Agent", "Namespaced", "team-a", "agent"), AgentIdentityRef: ref("AgentIdentity", "Namespaced", "team-a", "identity"),
			AgentInstanceRef: ref("AgentInstance", "Namespaced", "team-a", "instance"), SessionRef: "session-a",
			TargetRef: ref("Tool", "Namespaced", "team-a", "tool"), TrustProfileRef: pointerToReference(ref("TrustProfile", "Namespaced", "team-a", "profile")),
			AutonomyClassRef: ref("AutonomyClass", "Cluster", "", "supervised"), AuthorizationRefs: []api.RuntimeApprovalReference{ref("ToolGrant", "Namespaced", "team-a", "grant")},
			Binding:   api.RuntimeActionBinding{Format: "kognition-runtime-action-v1", Algorithm: "HMAC-SHA-256", Domain: "kognition.dev/runtime-approval/action/v1", Epoch: 2, KeyVersion: 2, Tag: "tag"},
			CreatedAt: decidedAt.Add(-time.Minute), ExpiresAt: decidedAt.Add(time.Minute),
		},
		Status: api.RuntimeApprovalStatus{
			DecisionState: api.RuntimeApprovalDecisionApproved, DecisionEvidenceDelivered: true,
			DecidedByIssuer: "issuer", DecidedBySubject: "subject", DecidedAt: &decidedAt,
			ExecutionState: api.RuntimeApprovalExecutionClaimed, ExecutionDisposition: "dispatching",
			PreparationToken: "preparation-token", PreparationDeadline: &preparationDeadline,
			Claimant:     &api.RuntimeApprovalClaimant{WorkloadUID: "workload-uid", ContainerID: "container-id", ProcessStartNonce: "process-nonce", ClaimedAt: decidedAt, LastHeartbeat: decidedAt, LeaseDeadline: &leaseDeadline},
			FencingToken: 4, TerminationProofRef: "termination-proof", ReconciliationEvidenceRef: "reconciliation-evidence",
			ReconciledByIssuer: "reconcile-issuer", ReconciledBySubject: "reconcile-subject",
			DecisionAudit: &api.RuntimeApprovalDecisionAudit{ID: "rad-test", Payload: api.RuntimeApprovalDecisionAuditPayload{Workspace: "team-a", InvocationID: "invocation-a", ApprovalUID: "approval-uid", Decision: api.RuntimeApprovalDecisionApproved, Issuer: "issuer", Subject: "subject", DecidedAt: decidedAt}},
		},
	}
	out := convert.RuntimeApprovalFromK8s(convert.RuntimeApprovalToK8s(in))
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("RuntimeApproval conversion round trip mismatch:\nwant %#v\n got %#v", in, out)
	}
	copy := convert.RuntimeApprovalToK8s(in).DeepCopy()
	copy.Spec.AuthorizationRefs[0].Name = "changed"
	copy.Status.DecisionAudit.ID = "changed"
	copy.Status.Claimant.WorkloadUID = "changed"
	copy.Status.PreparationDeadline.Time = copy.Status.PreparationDeadline.Add(time.Hour)
	if in.Spec.AuthorizationRefs[0].Name == "changed" || in.Status.DecisionAudit.ID == "changed" || in.Status.Claimant.WorkloadUID == "changed" || !in.Status.PreparationDeadline.Equal(preparationDeadline) {
		t.Fatal("RuntimeApproval conversion or deepcopy aliases mutable fields")
	}
}

func TestRuntimeApprovalRoundTripPreservesEmptyAuthorizationRefs(t *testing.T) {
	k8sObject := &k8s.RuntimeApproval{Spec: k8s.RuntimeApprovalSpec{AuthorizationRefs: []k8s.RuntimeApprovalReference{}}}
	roundTrip := convert.RuntimeApprovalToK8s(convert.RuntimeApprovalFromK8s(k8sObject))
	if roundTrip.Spec.AuthorizationRefs == nil {
		t.Fatal("RuntimeApproval conversion changed empty authorizationRefs to nil")
	}
}

func pointerToReference(value api.RuntimeApprovalReference) *api.RuntimeApprovalReference {
	return &value
}

func TestAgentServiceRoundTrip(t *testing.T) {
	revision := int64(7)
	in := &api.AgentService{
		TypeMeta:   api.TypeMeta{Kind: "AgentService", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "support", Namespace: "demo", Labels: map[string]string{"team": "care"}},
		Spec: api.AgentServiceSpec{
			AgentRef: api.LocalObjectReference{Name: "support-agent"}, RevisionPin: &revision,
			LoadBalancingStrategy: "random", Selector: map[string]string{"region": "west"}, Port: 8080,
		},
		Status: api.AgentServiceStatus{
			Phase: api.PhaseReady, Endpoint: "support.demo.svc:8080", ActiveRevision: 7, ReadyInstances: 2,
			Conditions: []api.Condition{{Type: "Ready", Status: "True", Reason: "InstancesAvailable"}},
		},
	}

	k8sObj := convert.AgentServiceToK8s(in)
	out := convert.AgentServiceFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Selector["region"] = "east"
	*k8sObj.Spec.RevisionPin = 8
	k8sObj.Status.Conditions[0].Status = "False"
	if in.Spec.Selector["region"] != "west" || *in.Spec.RevisionPin != 7 || in.Status.Conditions[0].Status != "True" {
		t.Fatal("AgentServiceToK8s aliases caller-owned storage")
	}
	out.Spec.Selector["region"] = "south"
	*out.Spec.RevisionPin = 9
	out.Status.Conditions[0].Reason = "Mutated"
	if k8sObj.Spec.Selector["region"] != "east" || *k8sObj.Spec.RevisionPin != 8 || k8sObj.Status.Conditions[0].Reason != "InstancesAvailable" {
		t.Fatal("AgentServiceFromK8s aliases caller-owned storage")
	}
}

func TestIntentGatewayRoundTrip(t *testing.T) {
	in := &api.IntentGateway{
		TypeMeta:   api.TypeMeta{Kind: "IntentGateway", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "entrypoint", Namespace: "demo"},
		Spec: api.IntentGatewaySpec{Protocol: "http", AuthConfig: &api.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &api.LocalObjectReference{Name: "gateway-key"}, Header: "X-Gateway-Key"}, Routes: []api.IntentRoute{
			{Pattern: "support.*", TargetRef: api.LocalObjectReference{Name: "support-agent"}, TargetKind: "Agent"},
			{Pattern: "order.*", TargetRef: api.LocalObjectReference{Name: "order-flow"}, TargetKind: "Workflow"},
		}},
		Status: api.IntentGatewayStatus{
			Phase: api.PhaseReady, Endpoint: "/apis/kognition.dev/v1alpha1/namespaces/demo/intentgateways/entrypoint/goals", Conditions: []api.Condition{{Type: "Ready", Status: "True"}},
		},
	}

	k8sObj := convert.IntentGatewayToK8s(in)
	out := convert.IntentGatewayFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Routes[0].Pattern = "mutated.*"
	k8sObj.Status.Conditions[0].Status = "False"
	if in.Spec.Routes[0].Pattern != "support.*" || in.Status.Conditions[0].Status != "True" {
		t.Fatal("IntentGatewayToK8s aliases caller-owned storage")
	}
	out.Spec.Routes[1].TargetKind = "Agent"
	out.Status.Conditions[0].Status = "Unknown"
	if k8sObj.Spec.Routes[1].TargetKind != "Workflow" || k8sObj.Status.Conditions[0].Status != "False" {
		t.Fatal("IntentGatewayFromK8s aliases caller-owned storage")
	}
}

func TestPromptRoundTrip(t *testing.T) {
	in := &api.Prompt{
		TypeMeta:   api.TypeMeta{Kind: "Prompt", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "assistant", Namespace: "demo"},
		Spec: api.PromptSpec{
			Content: "Hello {{name}}", Instructions: "legacy",
			Parameters: []api.PromptParameter{{Name: "name", Required: true, Default: "world"}},
			Metadata:   map[string]string{"owner": "platform"},
		},
		Status: api.PromptStatus{Hash: "sha256:abc", Version: 3},
	}

	k8sObj := convert.PromptToK8s(in)
	out := convert.PromptFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Parameters[0].Default = "mutated"
	k8sObj.Spec.Metadata["owner"] = "other"
	if in.Spec.Parameters[0].Default != "world" || in.Spec.Metadata["owner"] != "platform" {
		t.Fatal("PromptToK8s aliases caller-owned storage")
	}
	out.Spec.Parameters[0].Name = "mutated"
	out.Spec.Metadata["owner"] = "roundtrip"
	if k8sObj.Spec.Parameters[0].Name != "name" || k8sObj.Spec.Metadata["owner"] != "other" {
		t.Fatal("PromptFromK8s aliases caller-owned storage")
	}
}

func TestSolutionRoundTrip(t *testing.T) {
	seed := int64(42)
	in := &api.Solution{
		TypeMeta:   api.TypeMeta{Kind: "Solution", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "assistant", Namespace: "demo"},
		Spec: api.SolutionSpec{
			Goal: "Help operators", Constraints: []string{"read-only"}, Preferences: map[string]string{"tone": "concise"},
			Seed: &seed, ModelRef: &api.LocalObjectReference{Name: "planner"},
			Target:         &api.SolutionTarget{Kind: "Workflow", Ref: api.LocalObjectReference{Name: "incident-response"}},
			Overrides:      &api.SolutionOverrides{TokenBudget: &api.TokenBudget{MaxTotalTokens: 4096}, Hard: map[string]int64{"agents": 1}},
			PromotionGates: []api.SolutionPromotionGate{{EvaluationRef: api.LocalObjectReference{Name: "quality"}}},
		},
		Status: api.SolutionStatus{
			Phase: api.PhaseReady, ActiveRevision: 3, CandidateRevision: 4,
			Conditions:         []api.Condition{{Type: "Ready", Status: "True", Reason: "Compiled"}},
			CompiledResources:  []api.CompiledResource{{Kind: "Agent", Name: "assistant-agent", Namespace: "demo", Manifest: json.RawMessage(`{"spec":{"instructions":{"inline":"Help operators"}}}`)}},
			GeneratedResources: []api.GeneratedResource{{Kind: "Agent", Name: "assistant-agent"}},
		},
	}

	k8sObj := convert.SolutionToK8s(in)
	out := convert.SolutionFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Constraints[0] = "mutated"
	k8sObj.Spec.Preferences["tone"] = "verbose"
	k8sObj.Spec.Target.Ref.Name = "mutated-target"
	*k8sObj.Spec.Seed = 7
	k8sObj.Spec.Overrides.Hard["agents"] = 2
	k8sObj.Spec.PromotionGates[0].EvaluationRef.Name = "mutated-gate"
	k8sObj.Status.CompiledResources[0].Manifest[0] = '['
	if in.Spec.Constraints[0] != "read-only" || in.Spec.Preferences["tone"] != "concise" || in.Spec.Target.Ref.Name != "incident-response" || *in.Spec.Seed != 42 || in.Spec.Overrides.Hard["agents"] != 1 || in.Spec.PromotionGates[0].EvaluationRef.Name != "quality" || in.Status.CompiledResources[0].Manifest[0] != '{' {
		t.Fatal("SolutionToK8s aliases caller-owned storage")
	}
	out.Spec.Constraints[0] = "roundtrip"
	out.Spec.Overrides.Hard["agents"] = 3
	out.Status.CompiledResources[0].Manifest[0] = '('
	if k8sObj.Spec.Constraints[0] != "mutated" || k8sObj.Spec.Overrides.Hard["agents"] != 2 || k8sObj.Status.CompiledResources[0].Manifest[0] != '[' {
		t.Fatal("SolutionFromK8s aliases caller-owned storage")
	}
}

func TestEvaluationRoundTrip(t *testing.T) {
	revision := int64(9)
	minScore := 0.9
	maxCost := 1.25
	score := 0.95
	startedAt := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(time.Minute)
	in := &api.Evaluation{
		TypeMeta:   api.TypeMeta{Kind: "Evaluation", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "writer-eval", Namespace: "demo", Labels: map[string]string{"suite": "regression"}},
		Spec: api.EvaluationSpec{
			TargetRef:  api.EvaluationTargetRef{Kind: "Solution", Name: "assistant", Revision: &revision},
			DatasetRef: api.EvaluationDatasetRef{Name: "assistant-dataset", Version: "v1"},
			Graders: []api.EvaluationGrader{{
				Name:       "task-success",
				Type:       "task-success",
				Parameters: map[string]string{"rubric": "must answer"},
			}},
			MetricThresholds: []api.EvaluationMetricThreshold{
				{Metric: "task_success", Min: &minScore, Required: true},
				{Metric: "cost_usd", Max: &maxCost},
			},
			Schedule: &api.EvaluationSchedule{Cron: "0 0 * * *", Timezone: "UTC", ConcurrencyPolicy: api.EvaluationConcurrencyForbid, SuccessfulRunHistoryLimit: 5, FailedRunHistoryLimit: 2},
		},
		Status: api.EvaluationStatus{
			Phase:              api.PhaseReady,
			ObservedGeneration: 4,
			LastRunRef:         &api.LocalObjectReference{Name: "writer-eval-202608121000"},
			LastRunTime:        &completedAt,
			RunHistory: []api.EvaluationRunSummary{{
				Name:        "writer-eval-202608121000",
				TargetRef:   api.EvaluationTargetRef{Kind: "Solution", Name: "assistant", Revision: &revision},
				DatasetRef:  api.EvaluationDatasetRef{Name: "assistant-dataset", Version: "v1"},
				Phase:       api.PhaseReady,
				StartedAt:   &startedAt,
				CompletedAt: &completedAt,
				Score:       &score,
				Passed:      true,
				Metrics:     []api.EvaluationMetricResult{{Metric: "task_success", Value: 0.95, Passed: true}},
			}},
		},
	}

	k8sObj := convert.EvaluationToK8s(in)
	out := convert.EvaluationFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Graders[0].Parameters["rubric"] = "mutated"
	*k8sObj.Spec.TargetRef.Revision = 99
	*k8sObj.Spec.MetricThresholds[0].Min = 0.1
	k8sObj.Status.RunHistory[0].Metrics[0].Value = 0.1
	if in.Spec.Graders[0].Parameters["rubric"] != "must answer" || *in.Spec.TargetRef.Revision != 9 || *in.Spec.MetricThresholds[0].Min != 0.9 || in.Status.RunHistory[0].Metrics[0].Value != 0.95 {
		t.Fatal("EvaluationToK8s aliases caller-owned storage")
	}
	out.Spec.Graders[0].Parameters["rubric"] = "roundtrip"
	*out.Spec.TargetRef.Revision = 11
	*out.Status.RunHistory[0].Score = 0.2
	out.Status.RunHistory[0].Metrics[0].Value = 0.2
	if k8sObj.Spec.Graders[0].Parameters["rubric"] != "mutated" || *k8sObj.Spec.TargetRef.Revision != 99 || *k8sObj.Status.RunHistory[0].Score != 0.95 || k8sObj.Status.RunHistory[0].Metrics[0].Value != 0.1 {
		t.Fatal("EvaluationFromK8s aliases caller-owned storage")
	}
	if convert.EvaluationToK8s(nil) != nil || convert.EvaluationFromK8s(nil) != nil {
		t.Fatal("nil evaluations must remain nil")
	}
}

func TestEvaluationDatasetRoundTrip(t *testing.T) {
	validatedAt := time.Date(2026, time.August, 12, 10, 30, 0, 0, time.UTC)
	in := &api.EvaluationDataset{
		TypeMeta:   api.TypeMeta{Kind: "EvaluationDataset", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "writer-dataset", Namespace: "demo", Labels: map[string]string{"suite": "smoke"}},
		Spec: api.EvaluationDatasetSpec{
			Version: "v1",
			Cases: []api.EvaluationDatasetCase{{
				ID:        "greeting",
				Input:     "Say hello.",
				Expected:  "hello",
				Reference: "The assistant should greet the user.",
				Rubric:    "Must be brief.",
				Tags:      []string{"smoke", "greeting"},
				Metadata:  map[string]string{"priority": "p1"},
			}},
		},
		Status: api.EvaluationDatasetStatus{
			Phase:              api.PhaseReady,
			Conditions:         []api.Condition{{Type: "Ready", Status: "True", Reason: "Validated"}},
			ObservedGeneration: 2,
			CaseCount:          1,
			Digest:             "dataset-digest",
			LastValidatedAt:    &validatedAt,
			Message:            "dataset validated",
		},
	}

	k8sObj := convert.EvaluationDatasetToK8s(in)
	out := convert.EvaluationDatasetFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Cases[0].Tags[0] = "mutated"
	k8sObj.Spec.Cases[0].Metadata["priority"] = "p2"
	if in.Spec.Cases[0].Tags[0] != "smoke" || in.Spec.Cases[0].Metadata["priority"] != "p1" {
		t.Fatal("EvaluationDatasetToK8s aliases caller-owned storage")
	}
	out.Spec.Cases[0].Tags[0] = "roundtrip"
	out.Spec.Cases[0].Metadata["priority"] = "p3"
	if k8sObj.Spec.Cases[0].Tags[0] != "mutated" || k8sObj.Spec.Cases[0].Metadata["priority"] != "p2" {
		t.Fatal("EvaluationDatasetFromK8s aliases caller-owned storage")
	}
	if convert.EvaluationDatasetToK8s(nil) != nil || convert.EvaluationDatasetFromK8s(nil) != nil {
		t.Fatal("nil evaluation datasets must remain nil")
	}
}

func TestEvaluationRunRoundTrip(t *testing.T) {
	revision := int64(10)
	minScore := 0.8
	score := 1.0
	startedAt := time.Date(2026, time.August, 12, 11, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(45 * time.Second)
	in := &api.EvaluationRun{
		TypeMeta: api.TypeMeta{Kind: "EvaluationRun", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "writer-eval-g4",
			Namespace:   "demo",
			Labels:      map[string]string{"evaluation": "writer-eval"},
			Annotations: map[string]string{"dataset": "v1"},
		},
		Spec: api.EvaluationRunSpec{
			EvaluationRef: api.LocalObjectReference{Name: "writer-eval"},
			TargetRef:     api.EvaluationTargetRef{Kind: "Agent", Name: "writer", Revision: &revision},
			DatasetRef:    api.EvaluationDatasetRef{Name: "writer-dataset", Version: "v1"},
			Graders:       []api.EvaluationGrader{{Name: "task-success", Type: "task-success", Parameters: map[string]string{"expected": "done"}}},
			MetricThresholds: []api.EvaluationMetricThreshold{{
				Metric:   "task_success",
				Min:      &minScore,
				Required: true,
			}},
		},
		Status: api.EvaluationRunStatus{
			Phase:          api.PhaseReady,
			StartedAt:      &startedAt,
			CompletedAt:    &completedAt,
			DatasetDigest:  "dataset-digest",
			CaseCount:      1,
			CompletedCases: 1,
			CaseResults: []api.EvaluationCaseResult{{
				ID:           "case-1",
				TargetJobRef: &api.LocalObjectReference{Name: "writer-eval-g4-case-case-1"},
				TargetOutput: "done",
				StartedAt:    &startedAt,
				CompletedAt:  &completedAt,
				Passed:       true,
				Metrics:      []api.EvaluationMetricResult{{Metric: "task_success", Value: 1, Passed: true}},
				Message:      "case passed",
			}},
			TargetJobRef: &api.LocalObjectReference{Name: "writer-eval-g4-target"},
			TargetOutput: "done",
			Score:        &score,
			Passed:       true,
			Metrics:      []api.EvaluationMetricResult{{Metric: "task_success", Value: 1, Passed: true}},
			Message:      "evaluation passed",
		},
	}

	k8sObj := convert.EvaluationRunToK8s(in)
	out := convert.EvaluationRunFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Graders[0].Parameters["expected"] = "mutated"
	*k8sObj.Spec.TargetRef.Revision = 99
	*k8sObj.Spec.MetricThresholds[0].Min = 0.1
	k8sObj.Status.CaseResults[0].TargetJobRef.Name = "mutated-case-job"
	k8sObj.Status.CaseResults[0].Metrics[0].Value = 0.2
	k8sObj.Status.Metrics[0].Value = 0.1
	if in.Spec.Graders[0].Parameters["expected"] != "done" || *in.Spec.TargetRef.Revision != 10 || *in.Spec.MetricThresholds[0].Min != 0.8 || in.Status.CaseResults[0].TargetJobRef.Name != "writer-eval-g4-case-case-1" || in.Status.CaseResults[0].Metrics[0].Value != 1 || in.Status.Metrics[0].Value != 1 {
		t.Fatal("EvaluationRunToK8s aliases caller-owned storage")
	}
	out.Spec.Graders[0].Parameters["expected"] = "roundtrip"
	*out.Spec.TargetRef.Revision = 11
	*out.Status.Score = 0.2
	out.Status.CaseResults[0].TargetJobRef.Name = "roundtrip-case-job"
	out.Status.CaseResults[0].Metrics[0].Value = 0.3
	out.Status.Metrics[0].Value = 0.2
	if k8sObj.Spec.Graders[0].Parameters["expected"] != "mutated" || *k8sObj.Spec.TargetRef.Revision != 99 || *k8sObj.Status.Score != 1 || k8sObj.Status.CaseResults[0].TargetJobRef.Name != "mutated-case-job" || k8sObj.Status.CaseResults[0].Metrics[0].Value != 0.2 || k8sObj.Status.Metrics[0].Value != 0.1 {
		t.Fatal("EvaluationRunFromK8s aliases caller-owned storage")
	}
	if convert.EvaluationRunToK8s(nil) != nil || convert.EvaluationRunFromK8s(nil) != nil {
		t.Fatal("nil evaluation runs must remain nil")
	}
}

func TestCrossNamespaceGrantRoundTrip(t *testing.T) {
	in := &api.CrossNamespaceGrant{
		TypeMeta:   api.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "shared-tools", Namespace: "platform"},
		Spec:       api.CrossNamespaceGrantSpec{From: "platform", To: "apps", Resources: []api.GrantResource{{Kind: "Tool", Name: "search"}}},
	}
	k8sObj := convert.CrossNamespaceGrantToK8s(in)
	out := convert.CrossNamespaceGrantFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Resources[0].Name = "mutated"
	if in.Spec.Resources[0].Name != "search" {
		t.Fatal("CrossNamespaceGrantToK8s aliases caller-owned storage")
	}
	out.Spec.Resources[0].Kind = "Model"
	if k8sObj.Spec.Resources[0].Kind != "Tool" {
		t.Fatal("CrossNamespaceGrantFromK8s aliases caller-owned storage")
	}
}

func TestModelGrantRoundTrip(t *testing.T) {
	in := &api.ModelGrant{
		TypeMeta:   api.TypeMeta{Kind: "ModelGrant", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "assistant-gpt", Namespace: "apps"},
		Spec: api.ModelGrantSpec{
			AgentRef: api.LocalObjectReference{Name: "assistant"},
			ModelRef: api.LocalObjectReference{Name: "gpt"},
		},
	}
	out := convert.ModelGrantFromK8s(convert.ModelGrantToK8s(in))
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
}

func TestMemoryGrantRoundTrip(t *testing.T) {
	in := &api.MemoryGrant{
		TypeMeta:   api.TypeMeta{Kind: "MemoryGrant", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "assistant-memory", Namespace: "apps"},
		Spec: api.MemoryGrantSpec{
			AgentRef:       api.LocalObjectReference{Name: "assistant"},
			MemoryClaimRef: api.LocalObjectReference{Name: "conversation"},
			AccessMode:     "ReadWrite",
		},
	}
	out := convert.MemoryGrantFromK8s(convert.MemoryGrantToK8s(in))
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
}

func TestAgentCommunicationPolicyRoundTrip(t *testing.T) {
	in := &api.AgentCommunicationPolicy{
		TypeMeta:   api.TypeMeta{Kind: "AgentCommunicationPolicy", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "assistant-policy", Namespace: "apps"},
		Spec: api.AgentCommunicationPolicySpec{
			AllowFrom:          []api.LocalObjectReference{{Name: "planner"}},
			AllowFromQualified: []api.AgentReference{{Namespace: "other-team", Name: "planner"}},
			AllowTo:            []api.LocalObjectReference{{Name: "executor"}},
			DefaultDeny:        true,
		},
	}
	k8sObj := convert.AgentCommunicationPolicyToK8s(in)
	out := convert.AgentCommunicationPolicyFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.AllowFrom[0].Name = "mutated"
	if in.Spec.AllowFrom[0].Name != "planner" {
		t.Fatal("AgentCommunicationPolicyToK8s aliases caller-owned storage")
	}
	if k8sObj.Spec.AllowFromQualified[0].Namespace != "other-team" || k8sObj.Spec.AllowFromQualified[0].Name != "planner" {
		t.Fatal("native policy lost qualified source identity")
	}
	copy := k8sObj.DeepCopy()
	copy.Spec.AllowFromQualified[0].Namespace = "changed"
	if k8sObj.Spec.AllowFromQualified[0].Namespace != "other-team" {
		t.Fatal("qualified references share deepcopy storage")
	}
	k8sObj.Spec.AllowFromQualified[0].Namespace = "mutated"
	if in.Spec.AllowFromQualified[0].Namespace != "other-team" || out.Spec.AllowFromQualified[0].Namespace != "other-team" {
		t.Fatal("qualified policy conversion aliases source storage")
	}
}

func TestAgentAutoscalerRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &api.AgentAutoscaler{
		TypeMeta: api.TypeMeta{Kind: "AgentAutoscaler", APIVersion: api.APIVersion}, ObjectMeta: api.ObjectMeta{Name: "worker", Namespace: "apps"},
		Spec: api.AgentAutoscalerSpec{AgentRef: api.LocalObjectReference{Name: "worker"}, MinReplicas: 1, MaxReplicas: 8,
			Metrics:       []api.AutoscalerMetric{{Type: api.MetricQueueDepth, TargetValue: 2}},
			ScaleUpPolicy: &api.ScalingPolicy{StabilizationWindowSeconds: 30}, ScaleDownPolicy: &api.ScalingPolicy{StabilizationWindowSeconds: 60}},
		Status: api.AgentAutoscalerStatus{CurrentReplicas: 2, DesiredReplicas: 4, LastScaleTime: &now, Conditions: []api.Condition{{Type: "Ready", Status: "True"}}},
	}
	k8sObj := convert.AgentAutoscalerToK8s(in)
	out := convert.AgentAutoscalerFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
	k8sObj.Spec.Metrics[0].TargetValue = 99
	k8sObj.Spec.ScaleUpPolicy.StabilizationWindowSeconds = 99
	if in.Spec.Metrics[0].TargetValue != 2 || in.Spec.ScaleUpPolicy.StabilizationWindowSeconds != 30 {
		t.Fatal("AgentAutoscalerToK8s aliases caller-owned storage")
	}
}

func TestMemoryClassRoundTrip(t *testing.T) {
	in := &api.MemoryClass{
		TypeMeta:   api.TypeMeta{Kind: "MemoryClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "pgvector-class", Namespace: ""},
		Spec: api.MemoryClassSpec{
			BackendType:     "pgvector",
			Parameters:      map[string]string{"host": "pg.svc"},
			ReclaimPolicy:   api.ReclaimRetain,
			ConsistencyMode: "lww",
			SummarizationConfig: &api.MemorySummarizationConfig{
				Enabled:          true,
				ThresholdRecords: 100,
				ModelRef:         &api.LocalObjectReference{Name: "summarizer"},
			},
		},
	}

	k8sObj := convert.MemoryClassToK8s(in)
	out := convert.MemoryClassFromK8s(k8sObj)

	if out.Spec.BackendType != in.Spec.BackendType {
		t.Errorf("BackendType: got %q, want %q", out.Spec.BackendType, in.Spec.BackendType)
	}
	if string(out.Spec.ReclaimPolicy) != string(in.Spec.ReclaimPolicy) {
		t.Errorf("ReclaimPolicy: got %q, want %q", out.Spec.ReclaimPolicy, in.Spec.ReclaimPolicy)
	}
	if out.Spec.SummarizationConfig == nil {
		t.Fatal("SummarizationConfig lost in round-trip")
	}
	if out.Spec.SummarizationConfig.ThresholdRecords != in.Spec.SummarizationConfig.ThresholdRecords {
		t.Errorf("ThresholdRecords: got %d, want %d", out.Spec.SummarizationConfig.ThresholdRecords, in.Spec.SummarizationConfig.ThresholdRecords)
	}
	if out.Spec.SummarizationConfig.ModelRef == nil || out.Spec.SummarizationConfig.ModelRef.Name != "summarizer" {
		t.Error("ModelRef lost in round-trip")
	}
	if out.Spec.ConsistencyMode != in.Spec.ConsistencyMode {
		t.Errorf("ConsistencyMode: got %q, want %q", out.Spec.ConsistencyMode, in.Spec.ConsistencyMode)
	}
}

func TestMemoryRoundTrip(t *testing.T) {
	in := &api.Memory{
		TypeMeta:   api.TypeMeta{Kind: "Memory", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "conversation", Namespace: "tenant-a"},
		Spec: api.MemorySpec{
			ClaimRef:        api.LocalObjectReference{Name: "conversation-storage"},
			Type:            api.MemoryTypeEpisodic,
			TTL:             "24h",
			RetentionPolicy: "summarize",
		},
		Status: api.MemoryStatus{
			Phase:       api.PhaseReady,
			RecordCount: 12,
			BytesUsed:   4096,
			Conditions:  []api.Condition{{Type: "Ready", Status: "True", Reason: "Reconciled"}},
		},
	}

	out := convert.MemoryFromK8s(convert.MemoryToK8s(in))
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}
}

func TestMemoryRecordRoundTripDoesNotAlias(t *testing.T) {
	now := time.Date(2026, time.August, 2, 9, 15, 0, 0, time.UTC)
	in := &api.MemoryRecord{
		TypeMeta:   api.TypeMeta{Kind: "MemoryRecord", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "turn-1", Namespace: "tenant-a"},
		Spec: api.MemoryRecordSpec{
			MemoryRef: api.LocalObjectReference{Name: "conversation"},
			Content:   "customer preference",
			Metadata:  map[string]string{"channel": "chat"},
			Provenance: &api.MemoryProvenance{
				SourceAgentIdentity:    "assistant",
				SessionID:              "session-1",
				ReasoningStepID:        "step-1",
				Timestamp:              now,
				GrantID:                "grant-1",
				DerivedFromRecordNames: []string{"turn-0"},
			},
		},
		Status: api.MemoryRecordStatus{
			Phase:      api.PhaseReady,
			Conditions: []api.Condition{{Type: "Ready", Status: "True", Reason: "ProvenanceVerified"}},
		},
	}

	k8sObj := convert.MemoryRecordToK8s(in)
	out := convert.MemoryRecordFromK8s(k8sObj)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", out, in)
	}

	k8sObj.Spec.Metadata["channel"] = "mutated"
	k8sObj.Spec.Provenance.DerivedFromRecordNames[0] = "mutated"
	if in.Spec.Metadata["channel"] != "chat" || in.Spec.Provenance.DerivedFromRecordNames[0] != "turn-0" {
		t.Fatal("MemoryRecordToK8s aliases caller-owned storage")
	}
}

func TestMemoryClaimRoundTrip(t *testing.T) {
	transitionTime := time.Date(2026, time.August, 2, 9, 15, 0, 0, time.UTC)
	in := &api.MemoryClaim{
		TypeMeta: api.TypeMeta{Kind: "MemoryClaim", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:       "shared-context",
			Namespace:  "production",
			Finalizers: []string{"kognition.dev/memoryclaim-cleanup"},
		},
		Spec: api.MemoryClaimSpec{
			MemoryClassName: "pgvector-shared",
			Type:            api.MemoryTypeShared,
			Capacity:        "2Gi",
			ConsistencyMode: "strong",
			ConcurrencyMode: api.ConcurrencyCRDT,
			AccessModes:     []string{"ReadWrite", "Shared"},
		},
		Status: api.MemoryClaimStatus{
			Phase:           api.PhaseBound,
			BoundVolumeName: "production-shared-context",
			Conditions: []api.Condition{{
				Type:               "Ready",
				Status:             "True",
				LastTransitionTime: transitionTime,
				Reason:             "Bound",
				Message:            "bound to provisioned memory volume",
			}},
		},
	}

	out := convert.MemoryClaimFromK8s(convert.MemoryClaimToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace || len(out.ObjectMeta.Finalizers) != 1 {
		t.Fatalf("claim identity metadata changed: got %#v, want %#v", out, in)
	}
	if out.Spec.MemoryClassName != in.Spec.MemoryClassName || out.Spec.Type != in.Spec.Type || out.Spec.Capacity != in.Spec.Capacity || out.Spec.ConsistencyMode != in.Spec.ConsistencyMode || out.Spec.ConcurrencyMode != in.Spec.ConcurrencyMode {
		t.Errorf("claim allocation policy changed: got %#v, want %#v", out.Spec, in.Spec)
	}
	if len(out.Spec.AccessModes) != 2 || out.Spec.AccessModes[0] != "ReadWrite" || out.Spec.AccessModes[1] != "Shared" {
		t.Errorf("claim access modes changed: %#v", out.Spec.AccessModes)
	}
	if out.Status.Phase != api.PhaseBound || out.Status.BoundVolumeName != "production-shared-context" {
		t.Errorf("claim binding changed: %#v", out.Status)
	}
	if len(out.Status.Conditions) != 1 || out.Status.Conditions[0] != in.Status.Conditions[0] {
		t.Errorf("claim conditions changed: %#v", out.Status.Conditions)
	}
}

func TestMemoryVolumeRoundTrip(t *testing.T) {
	transitionTime := time.Date(2026, time.August, 2, 9, 16, 0, 0, time.UTC)
	in := &api.MemoryVolume{
		TypeMeta: api.TypeMeta{Kind: "MemoryVolume", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:      "production-shared-context",
			Namespace: "production",
			OwnerReferences: []api.OwnerReference{{
				APIVersion: api.APIVersion,
				Kind:       "MemoryClaim",
				Name:       "shared-context",
				UID:        "claim-uid",
				Controller: true,
			}},
		},
		Spec: api.MemoryVolumeSpec{
			MemoryClassName: "pgvector-shared",
			ClaimRef:        &api.LocalObjectReference{Name: "shared-context"},
			BackendHandle:   "pgvector-7c5f",
			Capacity:        "2Gi",
		},
		Status: api.MemoryVolumeStatus{
			Phase:    api.PhaseBound,
			Capacity: "2Gi",
			Conditions: []api.Condition{{
				Type:               "Ready",
				Status:             "True",
				LastTransitionTime: transitionTime,
				Reason:             "Bound",
				Message:            "backing store is provisioned",
			}},
		},
	}

	out := convert.MemoryVolumeFromK8s(convert.MemoryVolumeToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("volume identity metadata changed: got %#v, want %#v", out, in)
	}
	if len(out.ObjectMeta.OwnerReferences) != 1 || out.ObjectMeta.OwnerReferences[0] != in.ObjectMeta.OwnerReferences[0] {
		t.Errorf("volume ownership changed: %#v", out.ObjectMeta.OwnerReferences)
	}
	if out.Spec.ClaimRef == nil || out.Spec.ClaimRef.Name != "shared-context" || out.Spec.MemoryClassName != in.Spec.MemoryClassName || out.Spec.BackendHandle != in.Spec.BackendHandle || out.Spec.Capacity != in.Spec.Capacity {
		t.Errorf("volume binding changed: got %#v, want %#v", out.Spec, in.Spec)
	}
	if out.Status.Phase != api.PhaseBound || out.Status.Capacity != "2Gi" {
		t.Errorf("volume lifecycle changed: %#v", out.Status)
	}
	if len(out.Status.Conditions) != 1 || out.Status.Conditions[0] != in.Status.Conditions[0] {
		t.Errorf("volume conditions changed: %#v", out.Status.Conditions)
	}
}

func TestToolClassRoundTrip(t *testing.T) {
	in := &api.ToolClass{
		TypeMeta:   api.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "mcp-class"},
		Spec: api.ToolClassSpec{
			Provisioner: "mcp",
			Parameters:  map[string]string{"transport": "stdio"},
		},
	}

	k8sObj := convert.ToolClassToK8s(in)
	out := convert.ToolClassFromK8s(k8sObj)

	if out.Spec.Provisioner != in.Spec.Provisioner {
		t.Errorf("Provisioner: got %q, want %q", out.Spec.Provisioner, in.Spec.Provisioner)
	}
}

func TestWorkflowRoundTrip(t *testing.T) {
	in := &api.Workflow{
		TypeMeta:   api.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "my-workflow", Namespace: "default"},
		Spec: api.WorkflowSpec{
			Steps: []api.WorkflowStep{
				{Name: "step1", AgentRef: api.LocalObjectReference{Name: "agent1"}},
				{Name: "step2", AgentRef: api.LocalObjectReference{Name: "agent2"}},
			},
		},
	}

	k8sObj := convert.WorkflowToK8s(in)
	out := convert.WorkflowFromK8s(k8sObj)

	if len(out.Spec.Steps) != 2 {
		t.Fatalf("Steps: got %d, want 2", len(out.Spec.Steps))
	}
	if out.Spec.Steps[0].Name != "step1" {
		t.Errorf("Step[0].Name: got %q, want %q", out.Spec.Steps[0].Name, "step1")
	}
}

// Verify unused import is consumed
var _ = time.Second
