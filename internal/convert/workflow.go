// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- Workflow ---

func WorkflowToK8s(in *api.Workflow) *k8s.Workflow {
	out := &k8s.Workflow{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Workflow",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Status: k8s.WorkflowStatus{
			Phase:          phaseToString(in.Status.Phase),
			Conditions:     conditionsToK8s(in.Status.Conditions),
			CompletedSteps: in.Status.CompletedSteps,
			ActiveStep:     in.Status.ActiveStep,
			ActiveSteps:    in.Status.ActiveSteps,
			EntryPoints:    in.Status.EntryPoints,
			StepStatuses:   workflowStepStatusesToK8s(in.Status.StepStatuses),
		},
	}
	if spec := workflowSpecToK8s(&in.Spec); spec != nil {
		out.Spec = *spec
	}
	return out
}

func workflowStepRetryToK8s(r *api.WorkflowStepRetry) *k8s.WorkflowStepRetry {
	if r == nil {
		return nil
	}
	return &k8s.WorkflowStepRetry{
		MaxAttempts:       r.MaxAttempts,
		Backoff:           r.Backoff,
		BackoffMultiplier: r.BackoffMultiplier,
		MaxBackoff:        r.MaxBackoff,
	}
}

func workflowApprovalGatesToK8s(gates []api.WorkflowApprovalGate) []k8s.WorkflowApprovalGate {
	if gates == nil {
		return nil
	}
	out := make([]k8s.WorkflowApprovalGate, len(gates))
	for i, g := range gates {
		out[i] = k8s.WorkflowApprovalGate{
			From:              g.From,
			To:                g.To,
			RequiredApprovers: g.RequiredApprovers,
			Timeout:           g.Timeout,
		}
	}
	return out
}

func workflowMemoryClaimToK8s(c *api.WorkflowMemoryClaim) *k8s.WorkflowMemoryClaim {
	if c == nil {
		return nil
	}
	return &k8s.WorkflowMemoryClaim{MemoryClassName: c.MemoryClassName, AccessMode: c.AccessMode}
}

func workflowMemoryClaimFromK8s(c *k8s.WorkflowMemoryClaim) *api.WorkflowMemoryClaim {
	if c == nil {
		return nil
	}
	return &api.WorkflowMemoryClaim{MemoryClassName: c.MemoryClassName, AccessMode: c.AccessMode}
}

func WorkflowFromK8s(in *k8s.Workflow) *api.Workflow {
	out := &api.Workflow{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Status: api.WorkflowStatus{
			Phase:          phaseFromString(in.Status.Phase),
			Conditions:     conditionsFromK8s(in.Status.Conditions),
			CompletedSteps: in.Status.CompletedSteps,
			ActiveStep:     in.Status.ActiveStep,
			ActiveSteps:    in.Status.ActiveSteps,
			EntryPoints:    in.Status.EntryPoints,
			StepStatuses:   workflowStepStatusesFromK8s(in.Status.StepStatuses),
		},
	}
	if spec := workflowSpecFromK8s(&in.Spec); spec != nil {
		out.Spec = *spec
	}
	return out
}

func workflowStepRetryFromK8s(r *k8s.WorkflowStepRetry) *api.WorkflowStepRetry {
	if r == nil {
		return nil
	}
	return &api.WorkflowStepRetry{
		MaxAttempts:       r.MaxAttempts,
		Backoff:           r.Backoff,
		BackoffMultiplier: r.BackoffMultiplier,
		MaxBackoff:        r.MaxBackoff,
	}
}

func workflowApprovalGatesFromK8s(gates []k8s.WorkflowApprovalGate) []api.WorkflowApprovalGate {
	if gates == nil {
		return nil
	}
	out := make([]api.WorkflowApprovalGate, len(gates))
	for i, g := range gates {
		out[i] = api.WorkflowApprovalGate{
			From:              g.From,
			To:                g.To,
			RequiredApprovers: g.RequiredApprovers,
			Timeout:           g.Timeout,
		}
	}
	return out
}

// --- WorkflowRun ---

func WorkflowRunToK8s(in *api.WorkflowRun) *k8s.WorkflowRun {
	out := &k8s.WorkflowRun{
		TypeMeta: metav1.TypeMeta{
			Kind:       "WorkflowRun",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.WorkflowRunSpec{
			WorkflowRef: localRefToK8s(in.Spec.WorkflowRef.Name),
			Input:       in.Spec.Input,
			Parameters:  in.Spec.Parameters,
		},
		Status: k8s.WorkflowRunStatus{
			Phase:           phaseToString(in.Status.Phase),
			Conditions:      conditionsToK8s(in.Status.Conditions),
			CompletedSteps:  in.Status.CompletedSteps,
			ActiveStep:      in.Status.ActiveStep,
			ActiveSteps:     in.Status.ActiveSteps,
			StepStatuses:    workflowStepStatusesToK8s(in.Status.StepStatuses),
			StartTime:       timeToMetav1(in.Status.StartTime),
			CompletionTime:  timeToMetav1(in.Status.CompletionTime),
			Output:          in.Status.Output,
			ApprovalRecords: workflowApprovalRecordsToK8s(in.Status.ApprovalRecords),
		},
	}
	if in.Status.SnapshotSpec != nil {
		out.Status.SnapshotSpec = workflowSpecToK8s(in.Status.SnapshotSpec)
	}
	return out
}

func WorkflowRunFromK8s(in *k8s.WorkflowRun) *api.WorkflowRun {
	out := &api.WorkflowRun{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.WorkflowRunSpec{
			WorkflowRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.WorkflowRef)},
			Input:       in.Spec.Input,
			Parameters:  in.Spec.Parameters,
		},
		Status: api.WorkflowRunStatus{
			Phase:           phaseFromString(in.Status.Phase),
			Conditions:      conditionsFromK8s(in.Status.Conditions),
			CompletedSteps:  in.Status.CompletedSteps,
			ActiveStep:      in.Status.ActiveStep,
			ActiveSteps:     in.Status.ActiveSteps,
			StepStatuses:    workflowStepStatusesFromK8s(in.Status.StepStatuses),
			StartTime:       timeFromMetav1(in.Status.StartTime),
			CompletionTime:  timeFromMetav1(in.Status.CompletionTime),
			Output:          in.Status.Output,
			ApprovalRecords: workflowApprovalRecordsFromK8s(in.Status.ApprovalRecords),
		},
	}
	if in.Status.SnapshotSpec != nil {
		out.Status.SnapshotSpec = workflowSpecFromK8s(in.Status.SnapshotSpec)
	}
	return out
}

func workflowApprovalRecordsToK8s(in []api.WorkflowApprovalRecord) []k8s.WorkflowApprovalRecord {
	if in == nil {
		return nil
	}
	out := make([]k8s.WorkflowApprovalRecord, len(in))
	for i, record := range in {
		out[i] = k8s.WorkflowApprovalRecord{
			Step:      record.Step,
			Decision:  record.Decision,
			Approver:  record.Approver,
			Comment:   record.Comment,
			Timestamp: record.Timestamp,
		}
	}
	return out
}

func workflowApprovalRecordsFromK8s(in []k8s.WorkflowApprovalRecord) []api.WorkflowApprovalRecord {
	if in == nil {
		return nil
	}
	out := make([]api.WorkflowApprovalRecord, len(in))
	for i, record := range in {
		out[i] = api.WorkflowApprovalRecord{
			Step:      record.Step,
			Decision:  record.Decision,
			Approver:  record.Approver,
			Comment:   record.Comment,
			Timestamp: record.Timestamp,
		}
	}
	return out
}

func workflowStepStatusesToK8s(in []api.WorkflowStepStatus) []k8s.WorkflowStepStatus {
	if in == nil {
		return nil
	}
	out := make([]k8s.WorkflowStepStatus, len(in))
	for i, s := range in {
		out[i] = k8s.WorkflowStepStatus{
			Name:     s.Name,
			Phase:    string(s.Phase),
			Output:   s.Output,
			Attempts: s.Attempts,
		}
		if s.StartedAt != nil {
			t := metav1.NewTime(*s.StartedAt)
			out[i].StartedAt = &t
		}
		if s.NextRetryAt != nil {
			t := metav1.NewTime(*s.NextRetryAt)
			out[i].NextRetryAt = &t
		}
	}
	return out
}

func workflowStepStatusesFromK8s(in []k8s.WorkflowStepStatus) []api.WorkflowStepStatus {
	if in == nil {
		return nil
	}
	out := make([]api.WorkflowStepStatus, len(in))
	for i, s := range in {
		out[i] = api.WorkflowStepStatus{
			Name:     s.Name,
			Phase:    api.Phase(s.Phase),
			Output:   s.Output,
			Attempts: s.Attempts,
		}
		if s.StartedAt != nil {
			t := s.StartedAt.Time
			out[i].StartedAt = &t
		}
		if s.NextRetryAt != nil {
			t := s.NextRetryAt.Time
			out[i].NextRetryAt = &t
		}
	}
	return out
}

func workflowSpecToK8s(in *api.WorkflowSpec) *k8s.WorkflowSpec {
	if in == nil {
		return nil
	}
	out := &k8s.WorkflowSpec{
		EntryPoint:            in.EntryPoint,
		ApprovalGates:         workflowApprovalGatesToK8s(in.ApprovalGates),
		Parameters:            in.Parameters,
		MaxParallelSteps:      in.MaxParallelSteps,
		FailFast:              in.FailFast,
		ContinueOnError:       in.ContinueOnError,
		MaxConcurrentRuns:     in.MaxConcurrentRuns,
		HistoryLimit:          in.HistoryLimit,
		MemoryClaim:           workflowMemoryClaimToK8s(in.MemoryClaim),
		MemoryRetentionPolicy: k8s.WorkflowMemoryRetentionPolicy(in.MemoryRetentionPolicy),
	}
	if in.Emergent != nil {
		out.Emergent = &k8s.WorkflowEmergentSpec{
			Enabled: in.Emergent.Enabled,
			Budgets: k8s.WorkflowEmergentBudgets{
				MaxSteps:  in.Emergent.Budgets.MaxSteps,
				MaxTokens: in.Emergent.Budgets.MaxTokens,
				MaxCost:   in.Emergent.Budgets.MaxCost,
			},
		}
	}
	if len(in.Steps) > 0 {
		out.Steps = make([]k8s.WorkflowStep, len(in.Steps))
		for i, s := range in.Steps {
			out.Steps[i] = k8s.WorkflowStep{
				Name:            s.Name,
				Type:            k8s.WorkflowStepType(s.Type),
				AgentRef:        localRefToK8s(s.AgentRef.Name),
				ToolRef:         localRefToK8s(s.ToolRef.Name),
				Input:           s.Input,
				Timeout:         s.Timeout,
				Condition:       s.Condition,
				Retry:           workflowStepRetryToK8s(s.Retry),
				ContinueOnError: s.ContinueOnError,
				MemoryClaim:     workflowMemoryClaimToK8s(s.MemoryClaim),
				Collaboration:   workflowCollaborationToK8s(s.Collaboration),
			}
		}
	}
	if len(in.Edges) > 0 {
		out.Edges = make([]k8s.WorkflowEdge, len(in.Edges))
		for i, e := range in.Edges {
			out.Edges[i] = k8s.WorkflowEdge{
				From:      e.From,
				To:        e.To,
				Condition: e.Condition,
			}
		}
	}
	if in.Budgets != nil {
		out.Budgets = &k8s.WorkflowBudget{
			MaxDuration:       in.Budgets.MaxDuration,
			MaxTokens:         in.Budgets.MaxTokens,
			MaxIterations:     in.Budgets.MaxIterations,
			MaxTokensPerCycle: in.Budgets.MaxTokensPerCycle,
		}
	}
	return out
}

func workflowSpecFromK8s(in *k8s.WorkflowSpec) *api.WorkflowSpec {
	if in == nil {
		return nil
	}
	out := &api.WorkflowSpec{
		EntryPoint:            in.EntryPoint,
		ApprovalGates:         workflowApprovalGatesFromK8s(in.ApprovalGates),
		Parameters:            in.Parameters,
		MaxParallelSteps:      in.MaxParallelSteps,
		FailFast:              in.FailFast,
		ContinueOnError:       in.ContinueOnError,
		MaxConcurrentRuns:     in.MaxConcurrentRuns,
		HistoryLimit:          in.HistoryLimit,
		MemoryClaim:           workflowMemoryClaimFromK8s(in.MemoryClaim),
		MemoryRetentionPolicy: api.WorkflowMemoryRetentionPolicy(in.MemoryRetentionPolicy),
	}
	if in.Emergent != nil {
		out.Emergent = &api.WorkflowEmergentSpec{
			Enabled: in.Emergent.Enabled,
			Budgets: api.WorkflowEmergentBudgets{
				MaxSteps:  in.Emergent.Budgets.MaxSteps,
				MaxTokens: in.Emergent.Budgets.MaxTokens,
				MaxCost:   in.Emergent.Budgets.MaxCost,
			},
		}
	}
	if len(in.Steps) > 0 {
		out.Steps = make([]api.WorkflowStep, len(in.Steps))
		for i, s := range in.Steps {
			out.Steps[i] = api.WorkflowStep{
				Name:            s.Name,
				Type:            api.WorkflowStepType(s.Type),
				AgentRef:        api.LocalObjectReference{Name: s.AgentRef.Name},
				ToolRef:         api.LocalObjectReference{Name: s.ToolRef.Name},
				Input:           s.Input,
				Timeout:         s.Timeout,
				Condition:       s.Condition,
				Retry:           workflowStepRetryFromK8s(s.Retry),
				ContinueOnError: s.ContinueOnError,
				MemoryClaim:     workflowMemoryClaimFromK8s(s.MemoryClaim),
				Collaboration:   workflowCollaborationFromK8s(s.Collaboration),
			}
		}
	}
	if len(in.Edges) > 0 {
		out.Edges = make([]api.WorkflowEdge, len(in.Edges))
		for i, e := range in.Edges {
			out.Edges[i] = api.WorkflowEdge{
				From:      e.From,
				To:        e.To,
				Condition: e.Condition,
			}
		}
	}
	if in.Budgets != nil {
		out.Budgets = &api.WorkflowBudget{
			MaxDuration:       in.Budgets.MaxDuration,
			MaxTokens:         in.Budgets.MaxTokens,
			MaxIterations:     in.Budgets.MaxIterations,
			MaxTokensPerCycle: in.Budgets.MaxTokensPerCycle,
		}
	}
	return out
}
