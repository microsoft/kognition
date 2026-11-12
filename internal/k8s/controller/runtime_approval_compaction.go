// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/hmac"
	"fmt"
	"reflect"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const DefaultRuntimeApprovalTombstoneRetention = 90 * 24 * time.Hour

func runtimeApprovalCompactable(approval *k8s.RuntimeApproval) bool {
	if approval == nil || !approval.Status.DecisionEvidenceDelivered {
		return false
	}
	switch approval.Status.DecisionState {
	case api.RuntimeApprovalDecisionRejected, api.RuntimeApprovalDecisionExpired:
		return approval.Status.ExecutionState == api.RuntimeApprovalExecutionNotStarted
	case api.RuntimeApprovalDecisionApproved, api.RuntimeApprovalDecisionNotRequired:
		return approval.Status.ExecutionState == api.RuntimeApprovalExecutionCompleted || approval.Status.ExecutionState == api.RuntimeApprovalExecutionFailed
	default:
		return false
	}
}

func runtimeApprovalHasFinalizer(object client.Object, finalizer string) bool {
	for _, existing := range object.GetFinalizers() {
		if existing == finalizer {
			return true
		}
	}
	return false
}

func addRuntimeApprovalFinalizer(object client.Object, finalizer string) bool {
	if runtimeApprovalHasFinalizer(object, finalizer) {
		return false
	}
	object.SetFinalizers(append(object.GetFinalizers(), finalizer))
	return true
}

func removeRuntimeApprovalFinalizer(object client.Object, finalizer string) bool {
	finalizers := object.GetFinalizers()
	filtered := finalizers[:0]
	for _, existing := range finalizers {
		if existing != finalizer {
			filtered = append(filtered, existing)
		}
	}
	if len(filtered) == len(finalizers) {
		return false
	}
	object.SetFinalizers(filtered)
	return true
}

func (s *StoreAdapter) getRuntimeApprovalTombstone(ctx context.Context, workspace, invocationID string) (*k8s.RuntimeApprovalTombstone, error) {
	object := &k8s.RuntimeApprovalTombstone{}
	key := client.ObjectKey{Namespace: workspace, Name: runtimeapproval.CanonicalName(workspace, invocationID)}
	if err := s.client.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("k8s get RuntimeApprovalTombstone: %w", err)
	}
	if object.Spec.Workspace != workspace || object.Spec.InvocationID != invocationID {
		return nil, store.ErrConflict
	}
	return object, nil
}

func (s *StoreAdapter) authenticateRuntimeApprovalTombstoneReplay(ctx context.Context, tombstone *k8s.RuntimeApprovalTombstone, proposed api.RuntimeApproval, payload []byte) (api.RuntimeApproval, error) {
	binding := runtimeActionBindingFromKubernetes(tombstone.Spec.Binding)
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if tombstone.Spec.Workspace != proposed.Spec.Workspace || tombstone.Spec.InvocationID != proposed.Spec.InvocationID ||
		tombstone.Spec.Plane != proposed.Spec.Plane || tombstone.Spec.Operation != proposed.Spec.Operation ||
		tombstone.Spec.InitialDecision != proposed.Spec.InitialDecision ||
		!sameRuntimeApprovalReference(tombstone.Spec.AgentRef, proposed.Spec.AgentRef) {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if err := runtimeapproval.Verify(ctx, s.runtimeApprovalKeys(), runtimeapproval.ActionEnvelopeFromApproval(proposed, payload), binding); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("%w: %w", store.ErrConflict, err)
	}
	result := proposed
	result.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApproval"}
	result.ObjectMeta.Name = tombstone.Name
	result.ObjectMeta.Namespace = tombstone.Namespace
	result.ObjectMeta.UID = tombstone.Spec.RuntimeApprovalUID
	if rv, err := strconv.ParseInt(tombstone.ResourceVersion, 10, 64); err == nil {
		result.ObjectMeta.ResourceVersion = rv
	}
	result.Spec.InitialDecision = tombstone.Spec.InitialDecision
	result.Spec.Binding = binding
	result.Spec.CreatedAt = tombstone.Spec.CreatedAt.Time.UTC()
	result.Spec.ExpiresAt = tombstone.Spec.ExpiresAt.Time.UTC()
	result.Status = api.RuntimeApprovalStatus{
		DecisionState:             tombstone.Spec.DecisionState,
		DecisionEvidenceDelivered: true,
		ExecutionState:            tombstone.Spec.ExecutionState,
		ExecutionDisposition:      tombstone.Spec.ExecutionDisposition,
	}
	return result, nil
}

func sameRuntimeApprovalReference(left k8s.RuntimeApprovalReference, right api.RuntimeApprovalReference) bool {
	return left.Kind == right.Kind && left.Scope == right.Scope && left.Namespace == right.Namespace && left.Name == right.Name &&
		left.UID == right.UID && left.ResourceVersion == right.ResourceVersion
}

func runtimeActionBindingFromKubernetes(binding k8s.RuntimeActionBinding) api.RuntimeActionBinding {
	return api.RuntimeActionBinding{
		Format: binding.Format, Algorithm: binding.Algorithm, Domain: binding.Domain,
		Epoch: binding.Epoch, KeyVersion: binding.KeyVersion, Tag: binding.Tag,
	}
}

func runtimeApprovalTombstoneFor(object *k8s.RuntimeApproval, compactedAt time.Time, retention time.Duration) *k8s.RuntimeApprovalTombstone {
	return &k8s.RuntimeApprovalTombstone{
		TypeMeta: metav1.TypeMeta{APIVersion: k8s.GroupVersion.String(), Kind: "RuntimeApprovalTombstone"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  object.Namespace,
			Name:       object.Name,
			Finalizers: []string{k8s.RuntimeApprovalRetentionFinalizer},
		},
		Spec: k8s.RuntimeApprovalTombstoneSpec{
			Workspace: object.Spec.Workspace, InvocationID: object.Spec.InvocationID,
			Plane: object.Spec.Plane, Operation: object.Spec.Operation, AgentRef: object.Spec.AgentRef,
			RuntimeApprovalUID: string(object.UID), InitialDecision: object.Spec.InitialDecision,
			Binding: object.Spec.Binding, DecisionState: object.Status.DecisionState,
			ExecutionState: object.Status.ExecutionState, ExecutionDisposition: object.Status.ExecutionDisposition,
			CreatedAt: object.Spec.CreatedAt, ExpiresAt: object.Spec.ExpiresAt,
			CompactedAt: metav1.NewTime(compactedAt.UTC()), RetentionDeadline: metav1.NewTime(compactedAt.UTC().Add(retention)),
		},
	}
}

// CreateOrVerifyRuntimeApprovalTombstone persists the replay fence before the
// live RuntimeApproval can lose its compaction finalizer.
func (s *StoreAdapter) CreateOrVerifyRuntimeApprovalTombstone(ctx context.Context, object *k8s.RuntimeApproval, retention time.Duration) (result *k8s.RuntimeApprovalTombstone, resultErr error) {
	if object == nil || object.Spec.Workspace == "" || object.Spec.InvocationID == "" || object.Namespace != object.Spec.Workspace ||
		object.Name != runtimeapproval.CanonicalName(object.Spec.Workspace, object.Spec.InvocationID) {
		return nil, fmt.Errorf("RuntimeApproval identity is invalid for compaction")
	}
	if retention <= 0 {
		return nil, fmt.Errorf("RuntimeApproval tombstone retention must be positive")
	}
	if !runtimeApprovalCompactable(object) {
		return nil, fmt.Errorf("RuntimeApproval is not terminal and compactable")
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), runtimeActionBindingFromKubernetes(object.Spec.Binding)); err != nil {
		return nil, err
	}
	lease, err := s.acquireRuntimeApprovalWorkspaceLease(ctx, object.Spec.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := s.releaseRuntimeApprovalWorkspaceLease(ctx, lease); resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	candidate := runtimeApprovalTombstoneFor(object, s.runtimeApprovalNow(), retention)
	createErr := s.client.Create(ctx, candidate)
	if createErr == nil {
		if err := s.transitionRuntimeApprovalQuotaTicketToCompacted(ctx, object, candidate); err != nil {
			return nil, err
		}
		return candidate, nil
	}
	existing := &k8s.RuntimeApprovalTombstone{}
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(candidate), existing); err != nil {
		return nil, fmt.Errorf("create RuntimeApprovalTombstone: %w (authoritative reread failed: %w)", createErr, err)
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), runtimeActionBindingFromKubernetes(existing.Spec.Binding)); err != nil {
		return nil, err
	}
	candidate.Spec.CompactedAt = existing.Spec.CompactedAt
	candidate.Spec.RetentionDeadline = existing.Spec.RetentionDeadline
	candidate.ResourceVersion = existing.ResourceVersion
	candidate.UID = existing.UID
	candidate.CreationTimestamp = existing.CreationTimestamp
	if !reflect.DeepEqual(existing.Spec, candidate.Spec) || !hmac.Equal([]byte(existing.Spec.Binding.Tag), []byte(candidate.Spec.Binding.Tag)) {
		return nil, store.ErrConflict
	}
	if !runtimeApprovalHasFinalizer(existing, k8s.RuntimeApprovalRetentionFinalizer) {
		return nil, fmt.Errorf("existing RuntimeApprovalTombstone lacks retention finalizer")
	}
	if err := s.transitionRuntimeApprovalQuotaTicketToCompacted(ctx, object, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

func (s *StoreAdapter) transitionRuntimeApprovalQuotaTicketToCompacted(ctx context.Context, approval *k8s.RuntimeApproval, tombstone *k8s.RuntimeApprovalTombstone) error {
	ticket := &k8s.RuntimeApprovalQuotaTicket{}
	key := client.ObjectKeyFromObject(approval)
	if err := s.client.Get(ctx, key, ticket); err != nil {
		return fmt.Errorf("read RuntimeApproval quota ticket for compaction: %w", err)
	}
	if ticket.Spec.Workspace != approval.Spec.Workspace || ticket.Spec.InvocationID != approval.Spec.InvocationID ||
		ticket.Spec.RuntimeApprovalUID != string(approval.UID) || !reflect.DeepEqual(ticket.Spec.Binding, approval.Spec.Binding) ||
		!hmac.Equal([]byte(ticket.Spec.Binding.Tag), []byte(approval.Spec.Binding.Tag)) {
		return store.ErrConflict
	}
	if ticket.Spec.Phase == k8s.RuntimeApprovalQuotaTicketPhaseCompacted {
		if ticket.Spec.CompactedAt == nil || ticket.Spec.RetentionDeadline == nil ||
			!ticket.Spec.CompactedAt.Equal(&tombstone.Spec.CompactedAt) || !ticket.Spec.RetentionDeadline.Equal(&tombstone.Spec.RetentionDeadline) {
			return store.ErrConflict
		}
		return nil
	}
	if ticket.Spec.Phase != k8s.RuntimeApprovalQuotaTicketPhaseLive {
		return store.ErrConflict
	}
	ticket.Spec.Phase = k8s.RuntimeApprovalQuotaTicketPhaseCompacted
	ticket.Spec.CompactedAt = tombstone.Spec.CompactedAt.DeepCopy()
	ticket.Spec.RetentionDeadline = tombstone.Spec.RetentionDeadline.DeepCopy()
	if err := s.client.Update(ctx, ticket); err != nil {
		current := &k8s.RuntimeApprovalQuotaTicket{}
		if readErr := s.client.Get(ctx, key, current); readErr != nil || current.Spec.Phase != k8s.RuntimeApprovalQuotaTicketPhaseCompacted ||
			current.Spec.CompactedAt == nil || current.Spec.RetentionDeadline == nil ||
			!current.Spec.CompactedAt.Equal(&tombstone.Spec.CompactedAt) || !current.Spec.RetentionDeadline.Equal(&tombstone.Spec.RetentionDeadline) {
			return fmt.Errorf("transition RuntimeApproval quota ticket to compacted: %w", err)
		}
	}
	return nil
}

func runtimeApprovalFromTombstoneForGet(tombstone *k8s.RuntimeApprovalTombstone) api.RuntimeApproval {
	result := api.RuntimeApproval{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApproval"},
		ObjectMeta: api.ObjectMeta{Name: tombstone.Name, Namespace: tombstone.Namespace, UID: tombstone.Spec.RuntimeApprovalUID},
		Spec: api.RuntimeApprovalSpec{
			Workspace: tombstone.Spec.Workspace, InvocationID: tombstone.Spec.InvocationID,
			InitialDecision: tombstone.Spec.InitialDecision, Plane: tombstone.Spec.Plane, Operation: tombstone.Spec.Operation,
			AgentRef:  convert.RuntimeApprovalFromK8s(&k8s.RuntimeApproval{Spec: k8s.RuntimeApprovalSpec{AgentRef: tombstone.Spec.AgentRef}}).Spec.AgentRef,
			Binding:   runtimeActionBindingFromKubernetes(tombstone.Spec.Binding),
			CreatedAt: tombstone.Spec.CreatedAt.Time.UTC(), ExpiresAt: tombstone.Spec.ExpiresAt.Time.UTC(),
		},
		Status: api.RuntimeApprovalStatus{
			DecisionState: tombstone.Spec.DecisionState, DecisionEvidenceDelivered: true,
			ExecutionState: tombstone.Spec.ExecutionState, ExecutionDisposition: tombstone.Spec.ExecutionDisposition,
		},
	}
	if rv, err := strconv.ParseInt(tombstone.ResourceVersion, 10, 64); err == nil {
		result.ResourceVersion = rv
	}
	return result
}
