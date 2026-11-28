// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

const (
	defaultRuntimeApprovalClockSkew = 30 * time.Second
	maxRuntimeApprovalTTL           = 24 * time.Hour
)

func (s *StoreAdapter) runtimeApprovalNow() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *StoreAdapter) runtimeApprovalClockSkew() time.Duration {
	if s.maximumClockSkew >= defaultRuntimeApprovalClockSkew {
		return s.maximumClockSkew
	}
	return defaultRuntimeApprovalClockSkew
}

func (s *StoreAdapter) CreateOrGetRuntimeApproval(ctx context.Context, proposed api.RuntimeApproval, payload json.RawMessage, ttl time.Duration) (bool, api.RuntimeApproval, error) {
	if err := s.RuntimeApprovalClockReady(); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	keys := s.runtimeApprovalKeys()
	lease, err := s.acquireRuntimeApprovalBindingEpochWithRetry(ctx, keys)
	if err != nil {
		return false, api.RuntimeApproval{}, err
	}
	created, approval, operationErr := s.createOrGetRuntimeApproval(ctx, keys, proposed, payload, ttl)
	releaseErr := s.releaseRuntimeApprovalBindingEpoch(ctx, lease)
	if operationErr != nil {
		return false, api.RuntimeApproval{}, operationErr
	}
	if releaseErr != nil {
		return false, api.RuntimeApproval{}, releaseErr
	}
	return created, approval, nil
}

func (s *StoreAdapter) createOrGetRuntimeApproval(ctx context.Context, keys runtimeapproval.BindingKeyStore, proposed api.RuntimeApproval, payload json.RawMessage, ttl time.Duration) (created bool, result api.RuntimeApproval, resultErr error) {
	if err := validateKubernetesRuntimeApprovalCreate(proposed, ttl); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	if err := runtimeapproval.NormalizeAndValidateReferences(&proposed); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	decisionIntent := proposed.Status.DecisionState
	proposed.Spec.InitialDecision = decisionIntent
	proposed.Status = runtimeapproval.InitialStatus(decisionIntent)
	name := runtimeapproval.CanonicalName(proposed.Spec.Workspace, proposed.Spec.InvocationID)
	key := client.ObjectKey{Namespace: proposed.Spec.Workspace, Name: name}
	existing := &k8s.RuntimeApproval{}
	if err := s.client.Get(ctx, key, existing); err == nil {
		approval := runtimeApprovalFromKubernetes(existing)
		if err := runtimeapproval.AuthenticateRetainedBinding(ctx, keys, approval, payload); err != nil {
			return false, api.RuntimeApproval{}, fmt.Errorf("%w: %w", store.ErrConflict, err)
		}
		if !runtimeapproval.SameImmutableBinding(approval, proposed) {
			return false, api.RuntimeApproval{}, store.ErrConflict
		}
		if approval.Status.DecisionState == "" {
			approval, err = s.initializeKubernetesRuntimeApprovalStatus(ctx, key, existing, "recover RuntimeApproval initial status")
			if err != nil {
				return false, api.RuntimeApproval{}, err
			}
		}
		return false, approval, nil
	} else if !apierrors.IsNotFound(err) {
		return false, api.RuntimeApproval{}, fmt.Errorf("k8s get RuntimeApproval: %w", err)
	}
	tombstone, tombstoneErr := s.getRuntimeApprovalTombstone(ctx, proposed.Spec.Workspace, proposed.Spec.InvocationID)
	if tombstoneErr == nil {
		approval, err := s.authenticateRuntimeApprovalTombstoneReplay(ctx, tombstone, proposed, payload)
		if err != nil {
			return false, api.RuntimeApproval{}, err
		}
		return false, approval, nil
	}
	if !errors.Is(tombstoneErr, store.ErrNotFound) {
		return false, api.RuntimeApproval{}, tombstoneErr
	}
	if err := runtimeapproval.AuthenticateNewBinding(ctx, keys, proposed, payload); err != nil {
		return false, api.RuntimeApproval{}, err
	}

	now := s.runtimeApprovalNow()
	proposed.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApproval"}
	proposed.ObjectMeta.Name = name
	proposed.ObjectMeta.Namespace = proposed.Spec.Workspace
	proposed.ObjectMeta.CreationTimestamp = now
	proposed.ObjectMeta.ResourceVersion = 0
	proposed.Spec.CreatedAt = now
	deadline, err := runtimeapproval.RuntimeApprovalDeadline(proposed, now, ttl)
	if err != nil {
		return false, api.RuntimeApproval{}, err
	}
	proposed.Spec.ExpiresAt = deadline
	object := convert.RuntimeApprovalToK8s(&proposed)
	object.ResourceVersion = ""
	object.CreationTimestamp = metav1.Time{}
	object.Finalizers = []string{k8s.RuntimeApprovalCompactionFinalizer}
	object.Status = k8s.RuntimeApprovalStatus{}
	workspaceLease, err := s.acquireRuntimeApprovalWorkspaceLeaseWithRetry(ctx, object.Spec.Workspace)
	if err != nil {
		return false, api.RuntimeApproval{}, err
	}
	defer func() {
		if err := s.releaseRuntimeApprovalWorkspaceLease(ctx, workspaceLease); resultErr == nil && err != nil {
			resultErr = err
		}
	}()
	ticket, err := s.reserveRuntimeApprovalQuotaTicket(ctx, object)
	if err != nil {
		return false, api.RuntimeApproval{}, err
	}
	if err := s.client.Create(ctx, object); err != nil {
		createErr := err
		if err := s.client.Get(ctx, key, existing); err != nil {
			if apierrors.IsNotFound(err) {
				tombstone, tombstoneErr := s.getRuntimeApprovalTombstone(ctx, proposed.Spec.Workspace, proposed.Spec.InvocationID)
				if tombstoneErr == nil {
					approval, replayErr := s.authenticateRuntimeApprovalTombstoneReplay(ctx, tombstone, proposed, payload)
					if replayErr != nil {
						return false, api.RuntimeApproval{}, replayErr
					}
					return false, approval, nil
				}
			}
			return false, api.RuntimeApproval{}, fmt.Errorf("k8s create RuntimeApproval: %w (authoritative reread failed: %w)", createErr, err)
		}
		approval := runtimeApprovalFromKubernetes(existing)
		if err := runtimeapproval.AuthenticateRetainedBinding(ctx, keys, approval, payload); err != nil {
			return false, api.RuntimeApproval{}, fmt.Errorf("%w: %w", store.ErrConflict, err)
		}
		if !runtimeapproval.SameImmutableBinding(approval, proposed) {
			return false, api.RuntimeApproval{}, store.ErrConflict
		}
		if approval.Status.DecisionState == "" {
			approval, err = s.initializeKubernetesRuntimeApprovalStatus(ctx, key, existing, "recover concurrent RuntimeApproval initial status")
			if err != nil {
				return false, api.RuntimeApproval{}, err
			}
		}
		if err := s.bindRuntimeApprovalQuotaTicket(ctx, ticket, existing); err != nil {
			return false, api.RuntimeApproval{}, err
		}
		return false, approval, nil
	}
	if err := s.bindRuntimeApprovalQuotaTicket(ctx, ticket, object); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	initialApproval, err := s.initializeKubernetesRuntimeApprovalStatus(ctx, key, object, "initialize RuntimeApproval status")
	if err != nil {
		return false, api.RuntimeApproval{}, err
	}
	return true, initialApproval, nil
}

func (s *StoreAdapter) initializeKubernetesRuntimeApprovalStatus(ctx context.Context, key client.ObjectKey, object *k8s.RuntimeApproval, operation string) (api.RuntimeApproval, error) {
	intended := runtimeApprovalFromKubernetes(object)
	intended.Status = runtimeapproval.InitialStatus(intended.Spec.InitialDecision)
	runtimeapproval.FinalizeInitialStatus(&intended, intended.Spec.CreatedAt)
	object.Status = convert.RuntimeApprovalToK8s(&intended).Status
	if err := s.client.Status().Update(ctx, object); err != nil {
		current := &k8s.RuntimeApproval{}
		if readErr := s.client.Get(ctx, key, current); readErr != nil {
			return api.RuntimeApproval{}, fmt.Errorf("k8s %s: %w (reread failed: %w)", operation, err, readErr)
		}
		persisted := runtimeApprovalFromKubernetes(current)
		if !reflect.DeepEqual(persisted.Status, intended.Status) {
			return api.RuntimeApproval{}, fmt.Errorf("k8s %s remained incomplete after ambiguous result: %w", operation, err)
		}
		return persisted, nil
	}
	return runtimeApprovalFromKubernetes(object), nil
}

func (s *StoreAdapter) GetRuntimeApprovalByInvocation(ctx context.Context, workspace, invocationID string) (api.RuntimeApproval, error) {
	object := &k8s.RuntimeApproval{}
	key := client.ObjectKey{Namespace: workspace, Name: runtimeapproval.CanonicalName(workspace, invocationID)}
	if err := s.client.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			tombstone, tombstoneErr := s.getRuntimeApprovalTombstone(ctx, workspace, invocationID)
			if tombstoneErr != nil {
				return api.RuntimeApproval{}, tombstoneErr
			}
			if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), runtimeActionBindingFromKubernetes(tombstone.Spec.Binding)); err != nil {
				return api.RuntimeApproval{}, err
			}
			return runtimeApprovalFromTombstoneForGet(tombstone), nil
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s get RuntimeApproval: %w", err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.Spec.Workspace != workspace || approval.Spec.InvocationID != invocationID {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if approval.Status.DecisionState == "" {
		return s.initializeKubernetesRuntimeApprovalStatus(ctx, key, object, "recover RuntimeApproval initial status on lookup")
	}
	return approval, nil
}

func (s *StoreAdapter) DecideRuntimeApproval(ctx context.Context, workspace, invocationID string, decision store.RuntimeApprovalDecision) (api.RuntimeApproval, error) {
	if decision.Decision != api.RuntimeApprovalDecisionApproved && decision.Decision != api.RuntimeApprovalDecisionRejected {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval decision must be Approved or Rejected")
	}
	if decision.ExpectedResourceVersion <= 0 || !runtimeapproval.ValidDecisionActor(decision.Issuer, decision.Subject) {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval decision requires expected resource version and canonical actor")
	}
	selfDecision := false
	approval, err := s.transitionKubernetesRuntimeApprovalDecision(ctx, workspace, invocationID, decision.ExpectedResourceVersion, func(approval *api.RuntimeApproval, effectiveNow time.Time) bool {
		if runtimeapproval.IsSelfDecision(*approval, decision.Issuer, decision.Subject) {
			selfDecision = true
			return false
		}
		approval.Status.DecidedByIssuer = decision.Issuer
		approval.Status.DecidedBySubject = decision.Subject
		if !effectiveNow.Before(approval.Spec.ExpiresAt) {
			approval.Status.DecisionState = api.RuntimeApprovalDecisionExpired
		} else {
			approval.Status.DecisionState = decision.Decision
		}
		now := s.runtimeApprovalNow()
		approval.Status.DecidedAt = &now
		runtimeapproval.PopulateDecisionAudit(approval)
		return true
	})
	if selfDecision {
		return api.RuntimeApproval{}, store.ErrSelfApproval
	}
	return approval, err
}

func (s *StoreAdapter) ExpireRuntimeApprovalDecision(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval expiry requires expected resource version")
	}
	return s.transitionKubernetesRuntimeApprovalDecision(ctx, workspace, invocationID, expectedResourceVersion, func(approval *api.RuntimeApproval, effectiveNow time.Time) bool {
		if effectiveNow.Before(approval.Spec.ExpiresAt) {
			return false
		}
		approval.Status.DecisionState = api.RuntimeApprovalDecisionExpired
		now := s.runtimeApprovalNow()
		approval.Status.DecidedAt = &now
		runtimeapproval.PopulateDecisionAudit(approval)
		return true
	})
}

func (s *StoreAdapter) transitionKubernetesRuntimeApprovalDecision(
	ctx context.Context,
	workspace, invocationID string,
	expectedResourceVersion int64,
	transition func(*api.RuntimeApproval, time.Time) bool,
) (api.RuntimeApproval, error) {
	if err := s.RuntimeApprovalClockReady(); err != nil {
		return api.RuntimeApproval{}, err
	}
	key := client.ObjectKey{Namespace: workspace, Name: runtimeapproval.CanonicalName(workspace, invocationID)}
	object := &k8s.RuntimeApproval{}
	if err := s.client.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return api.RuntimeApproval{}, store.ErrNotFound
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s get RuntimeApproval decision: %w", err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.Spec.Workspace != workspace || approval.Spec.InvocationID != invocationID {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	recoveredInitialStatus := false
	if approval.Status.DecisionState == "" {
		if approval.ResourceVersion != expectedResourceVersion {
			return api.RuntimeApproval{}, store.ErrConflict
		}
		recovered, recoveryErr := s.initializeKubernetesRuntimeApprovalStatus(ctx, key, object, "recover RuntimeApproval initial status before decision")
		if recoveryErr != nil {
			return api.RuntimeApproval{}, recoveryErr
		}
		approval = recovered
		recoveredInitialStatus = true
	}
	if approval.Status.DecisionState != api.RuntimeApprovalDecisionPending {
		return approval, nil
	}
	if !recoveredInitialStatus && approval.ResourceVersion != expectedResourceVersion {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	effectiveNow := s.runtimeApprovalNow().Add(s.runtimeApprovalClockSkew())
	if !transition(&approval, effectiveNow) {
		return approval, nil
	}
	updated := convert.RuntimeApprovalToK8s(&approval)
	updated.ResourceVersion = object.ResourceVersion
	if err := s.client.Status().Update(ctx, updated); err != nil {
		current := &k8s.RuntimeApproval{}
		if readErr := s.client.Get(ctx, key, current); readErr != nil {
			return api.RuntimeApproval{}, fmt.Errorf("k8s update RuntimeApproval decision: %w (reread failed: %w)", err, readErr)
		}
		winner := runtimeApprovalFromKubernetes(current)
		if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), winner.Spec.Binding); err != nil {
			return api.RuntimeApproval{}, err
		}
		if winner.Spec.Workspace != workspace || winner.Spec.InvocationID != invocationID {
			return api.RuntimeApproval{}, store.ErrConflict
		}
		if reflect.DeepEqual(winner.Status, approval.Status) {
			return winner, nil
		}
		if winner.Status.DecisionState != api.RuntimeApprovalDecisionPending {
			return winner, nil
		}
		if apierrors.IsConflict(err) {
			return api.RuntimeApproval{}, store.ErrConflict
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s update RuntimeApproval decision remained pending after ambiguous result: %w", err)
	}
	return runtimeApprovalFromKubernetes(updated), nil
}

func (s *StoreAdapter) AcknowledgeRuntimeApprovalDecisionAudit(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, expectedID string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 || expectedID == "" {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval audit acknowledgement requires expected resource version and outbox ID")
	}
	key := client.ObjectKey{Namespace: workspace, Name: runtimeapproval.CanonicalName(workspace, invocationID)}
	object := &k8s.RuntimeApproval{}
	if err := s.client.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return api.RuntimeApproval{}, store.ErrNotFound
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s get RuntimeApproval audit acknowledgement: %w", err)
	}
	approval := runtimeApprovalFromKubernetes(object)
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.Spec.Workspace != workspace || approval.Spec.InvocationID != invocationID {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if approval.Status.DecisionAudit == nil || approval.Status.DecisionAudit.ID != expectedID {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if approval.Status.DecisionEvidenceDelivered {
		return approval, nil
	}
	if approval.ResourceVersion != expectedResourceVersion {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	base := object.DeepCopy()
	object.Status.DecisionEvidenceDelivered = true
	if err := s.client.Status().Patch(ctx, object, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			current := &k8s.RuntimeApproval{}
			if readErr := s.client.Get(ctx, key, current); readErr != nil {
				return api.RuntimeApproval{}, fmt.Errorf("k8s reread RuntimeApproval audit acknowledgement conflict: %w", readErr)
			}
			winner := runtimeApprovalFromKubernetes(current)
			if winner.Status.DecisionEvidenceDelivered && winner.Status.DecisionAudit != nil && winner.Status.DecisionAudit.ID == expectedID {
				return winner, nil
			}
			return api.RuntimeApproval{}, store.ErrConflict
		}
		return api.RuntimeApproval{}, fmt.Errorf("k8s update RuntimeApproval audit acknowledgement: %w", err)
	}
	return runtimeApprovalFromKubernetes(object), nil
}

func runtimeApprovalFromKubernetes(object *k8s.RuntimeApproval) api.RuntimeApproval {
	approval := *convert.RuntimeApprovalFromK8s(object)
	runtimeapproval.NormalizeLegacyStoredApproval(&approval)
	return approval
}

func validateKubernetesRuntimeApprovalCreate(approval api.RuntimeApproval, ttl time.Duration) error {
	return runtimeapproval.ValidateCreate(approval, ttl)
}

var _ store.RuntimeApprovalStore = (*StoreAdapter)(nil)
