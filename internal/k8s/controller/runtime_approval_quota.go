// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	defaultRuntimeApprovalWorkspaceRecords int64 = 10_000
	defaultRuntimeApprovalWorkspaceBytes   int64 = 64 << 20
	defaultRuntimeApprovalAgentRecords     int64 = 1_000
	defaultRuntimeApprovalAgentBytes       int64 = 8 << 20
)

// RuntimeApprovalQuotaLimits bounds retained logical approval records.
type RuntimeApprovalQuotaLimits struct {
	WorkspaceRecords int64
	WorkspaceBytes   int64
	AgentRecords     int64
	AgentBytes       int64
}

// DefaultRuntimeApprovalQuotaLimits returns the ADR-0013 limits.
func DefaultRuntimeApprovalQuotaLimits() RuntimeApprovalQuotaLimits {
	return RuntimeApprovalQuotaLimits{
		WorkspaceRecords: defaultRuntimeApprovalWorkspaceRecords,
		WorkspaceBytes:   defaultRuntimeApprovalWorkspaceBytes,
		AgentRecords:     defaultRuntimeApprovalAgentRecords,
		AgentBytes:       defaultRuntimeApprovalAgentBytes,
	}
}

// ValidateRuntimeApprovalQuotaLimits enforces the normative ADR-0013 bounds.
func ValidateRuntimeApprovalQuotaLimits(limits RuntimeApprovalQuotaLimits) error {
	defaults := DefaultRuntimeApprovalQuotaLimits()
	if limits.WorkspaceRecords <= 0 || limits.WorkspaceRecords > defaults.WorkspaceRecords ||
		limits.WorkspaceBytes <= 0 || limits.WorkspaceBytes > defaults.WorkspaceBytes ||
		limits.AgentRecords <= 0 || limits.AgentRecords > defaults.AgentRecords ||
		limits.AgentBytes <= 0 || limits.AgentBytes > defaults.AgentBytes {
		return fmt.Errorf("RuntimeApproval quota limits must be positive and no greater than ADR defaults")
	}
	return nil
}

// ConfigureRuntimeApprovalQuota accepts positive limits no greater than the
// defaults. Operators may lower RuntimeApproval quotas but cannot disable or
// raise them beyond the normative bounds.
func (s *StoreAdapter) ConfigureRuntimeApprovalQuota(limits RuntimeApprovalQuotaLimits) error {
	if err := ValidateRuntimeApprovalQuotaLimits(limits); err != nil {
		return err
	}
	s.runtimeApprovalQuota = limits
	return nil
}

func runtimeApprovalQuotaTicketFor(object *k8s.RuntimeApproval, chargedBytes int64) *k8s.RuntimeApprovalQuotaTicket {
	ticket := &k8s.RuntimeApprovalQuotaTicket{
		TypeMeta: metav1.TypeMeta{APIVersion: k8s.GroupVersion.String(), Kind: "RuntimeApprovalQuotaTicket"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: object.Namespace, Name: object.Name,
			Finalizers: []string{k8s.RuntimeApprovalQuotaTicketRetirementFinalizer},
		},
		Spec: k8s.RuntimeApprovalQuotaTicketSpec{
			ActorKind: object.Spec.ActorKind,
			Workspace: object.Spec.Workspace, InvocationID: object.Spec.InvocationID,
			AgentRef: object.Spec.AgentRef, Binding: object.Spec.Binding,
			Phase: k8s.RuntimeApprovalQuotaTicketPhaseLive, ChargedBytes: chargedBytes,
			CreatedAt: object.Spec.CreatedAt,
		},
	}
	if object.Spec.ExecutorAction != nil {
		executor := object.Spec.ExecutorAction.Executor
		ticket.Spec.ExecutorRef = &executor
	}
	return ticket
}

func sameRuntimeApprovalQuotaTicketIdentity(existing, candidate *k8s.RuntimeApprovalQuotaTicket) bool {
	return existing.Namespace == candidate.Namespace && existing.Name == candidate.Name &&
		existing.Spec.ActorKind == candidate.Spec.ActorKind && reflect.DeepEqual(existing.Spec.ExecutorRef, candidate.Spec.ExecutorRef) &&
		existing.Spec.Workspace == candidate.Spec.Workspace && existing.Spec.InvocationID == candidate.Spec.InvocationID &&
		reflect.DeepEqual(existing.Spec.AgentRef, candidate.Spec.AgentRef) &&
		reflect.DeepEqual(existing.Spec.Binding, candidate.Spec.Binding) &&
		hmac.Equal([]byte(existing.Spec.Binding.Tag), []byte(candidate.Spec.Binding.Tag)) &&
		existing.Spec.Phase == k8s.RuntimeApprovalQuotaTicketPhaseLive &&
		runtimeApprovalHasFinalizer(existing, k8s.RuntimeApprovalQuotaTicketRetirementFinalizer)
}

func (s *StoreAdapter) reserveRuntimeApprovalQuotaTicket(ctx context.Context, object *k8s.RuntimeApproval) (*k8s.RuntimeApprovalQuotaTicket, error) {
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("measure RuntimeApproval quota charge: %w", err)
	}
	candidate := runtimeApprovalQuotaTicketFor(object, int64(len(encoded)))
	existing := &k8s.RuntimeApprovalQuotaTicket{}
	key := client.ObjectKeyFromObject(candidate)
	if err := s.client.Get(ctx, key, existing); err == nil {
		if !sameRuntimeApprovalQuotaTicketIdentity(existing, candidate) {
			return nil, store.ErrConflict
		}
		return existing, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("read RuntimeApproval quota ticket: %w", err)
	}

	list := &k8s.RuntimeApprovalQuotaTicketList{}
	if err := s.client.List(ctx, list, client.InNamespace(object.Spec.Workspace)); err != nil {
		return nil, fmt.Errorf("list RuntimeApproval quota tickets: %w", err)
	}
	var workspaceRecords, workspaceBytes, agentRecords, agentBytes int64
	for i := range list.Items {
		item := &list.Items[i]
		workspaceRecords++
		workspaceBytes += item.Spec.ChargedBytes
		if item.Spec.ActorKind == candidate.Spec.ActorKind &&
			(candidate.Spec.ActorKind == api.RuntimeApprovalActorExecutor && reflect.DeepEqual(item.Spec.ExecutorRef, candidate.Spec.ExecutorRef) ||
				candidate.Spec.ActorKind == "" && item.Spec.AgentRef.UID == candidate.Spec.AgentRef.UID) {
			agentRecords++
			agentBytes += item.Spec.ChargedBytes
		}
	}
	limits := s.runtimeApprovalQuota
	if workspaceRecords+1 > limits.WorkspaceRecords || workspaceBytes+candidate.Spec.ChargedBytes > limits.WorkspaceBytes ||
		agentRecords+1 > limits.AgentRecords || agentBytes+candidate.Spec.ChargedBytes > limits.AgentBytes {
		return nil, fmt.Errorf("%w: RuntimeApproval records or bytes exceed workspace or Agent limit", store.ErrQuotaExceeded)
	}
	if err := s.client.Create(ctx, candidate); err == nil {
		return candidate, nil
	} else {
		createErr := err
		if err := s.client.Get(ctx, key, existing); err != nil {
			return nil, fmt.Errorf("create RuntimeApproval quota ticket: %w (authoritative reread failed: %w)", createErr, err)
		}
		if !sameRuntimeApprovalQuotaTicketIdentity(existing, candidate) {
			return nil, store.ErrConflict
		}
		return existing, nil
	}
}

func (s *StoreAdapter) bindRuntimeApprovalQuotaTicket(ctx context.Context, ticket *k8s.RuntimeApprovalQuotaTicket, approval *k8s.RuntimeApproval) error {
	current := &k8s.RuntimeApprovalQuotaTicket{}
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(ticket), current); err != nil {
		return fmt.Errorf("reread RuntimeApproval quota ticket for UID binding: %w", err)
	}
	uid := string(approval.UID)
	if current.Spec.RuntimeApprovalUID == uid {
		return nil
	}
	if current.Spec.RuntimeApprovalUID != "" {
		return store.ErrConflict
	}
	current.Spec.RuntimeApprovalUID = uid
	if err := s.client.Update(ctx, current); err != nil {
		return fmt.Errorf("bind RuntimeApproval quota ticket UID: %w", err)
	}
	return nil
}
