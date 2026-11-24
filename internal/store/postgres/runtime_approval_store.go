// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const runtimeApprovalBindingEpochLock = "kognition.runtime-approval.binding-key-epoch"

type bindingKeyEpochMetadata struct {
	Version int64                            `json:"version"`
	Epoch   int64                            `json:"epoch"`
	Status  runtimeapproval.BindingKeyStatus `json:"status"`
	Digest  string                           `json:"digest"`
}

func (s *Store) CreateOrGetRuntimeApproval(ctx context.Context, proposed api.RuntimeApproval, payload json.RawMessage, ttl time.Duration) (bool, api.RuntimeApproval, error) {
	if err := validateRuntimeApprovalCreate(proposed, ttl); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	if err := runtimeapproval.NormalizeAndValidateReferences(&proposed); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	decisionIntent := proposed.Status.DecisionState
	proposed.Spec.InitialDecision = decisionIntent
	proposed.Status = runtimeapproval.InitialStatus(decisionIntent)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, api.RuntimeApproval{}, fmt.Errorf("begin RuntimeApproval create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	keys := s.runtimeApprovalKeys()
	if err := lockAndValidateRuntimeApprovalBindingEpoch(ctx, tx, keys); err != nil {
		return false, api.RuntimeApproval{}, err
	}
	logicalKey := proposed.Spec.Workspace + "\x1f" + proposed.Spec.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, logicalKey); err != nil {
		return false, api.RuntimeApproval{}, fmt.Errorf("lock RuntimeApproval: %w", err)
	}
	if existing, found, err := scanRuntimeApproval(ctx, tx, proposed.Spec.Workspace, proposed.Spec.InvocationID, true); err != nil {
		return false, api.RuntimeApproval{}, err
	} else if found {
		if err := runtimeapproval.AuthenticateRetainedBinding(ctx, keys, existing, payload); err != nil {
			return false, api.RuntimeApproval{}, fmt.Errorf("%w: %w", store.ErrConflict, err)
		}
		if !runtimeapproval.SameImmutableBinding(existing, proposed) {
			return false, api.RuntimeApproval{}, store.ErrConflict
		}
		return false, existing, nil
	}
	if tombstone, found, err := scanRuntimeApprovalTombstone(ctx, tx, proposed.Spec.Workspace, proposed.Spec.InvocationID); err != nil {
		return false, api.RuntimeApproval{}, err
	} else if found {
		if proposed.Spec.Binding != tombstone.Spec.Binding || proposed.Spec.ActorKind != tombstone.Spec.ActorKind || proposed.Spec.InitialDecision != tombstone.Spec.InitialDecision {
			return false, api.RuntimeApproval{}, store.ErrConflict
		}
		if err := runtimeapproval.AuthenticateRetainedBinding(ctx, keys, proposed, payload); err != nil {
			return false, api.RuntimeApproval{}, fmt.Errorf("%w: %w", store.ErrConflict, err)
		}
		proposed.TypeMeta = tombstone.TypeMeta
		proposed.ObjectMeta = tombstone.ObjectMeta
		proposed.Spec.CreatedAt = tombstone.Spec.CreatedAt
		proposed.Spec.ExpiresAt = tombstone.Spec.ExpiresAt
		proposed.Status = tombstone.Status
		return false, proposed, nil
	}
	if err := runtimeapproval.AuthenticateNewBinding(ctx, keys, proposed, payload); err != nil {
		return false, api.RuntimeApproval{}, err
	}

	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return false, api.RuntimeApproval{}, fmt.Errorf("read RuntimeApproval transaction time: %w", err)
	}
	uid := uuid.NewString()
	proposed.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApproval"}
	proposed.ObjectMeta.Name = runtimeapproval.CanonicalName(proposed.Spec.Workspace, proposed.Spec.InvocationID)
	proposed.ObjectMeta.Namespace = proposed.Spec.Workspace
	proposed.ObjectMeta.UID = uid
	proposed.ObjectMeta.ResourceVersion = 1
	proposed.ObjectMeta.CreationTimestamp = now.UTC()
	proposed.Spec.CreatedAt = now.UTC()
	proposed.Spec.ExpiresAt, err = runtimeapproval.RuntimeApprovalDeadline(proposed, now, ttl)
	if err != nil {
		return false, api.RuntimeApproval{}, err
	}
	runtimeapproval.FinalizeInitialStatus(&proposed, now)
	data, err := json.Marshal(proposed)
	if err != nil {
		return false, api.RuntimeApproval{}, fmt.Errorf("marshal RuntimeApproval: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO runtime_approvals
		(uid, name, workspace, invocation_id, binding_tag, binding_epoch, binding_key_version,
		 decision_state, execution_state, expires_at, data, resource_version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1, $12, $12)`,
		uid, proposed.Name, proposed.Spec.Workspace, proposed.Spec.InvocationID,
		proposed.Spec.Binding.Tag, proposed.Spec.Binding.Epoch, proposed.Spec.Binding.KeyVersion,
		proposed.Status.DecisionState, proposed.Status.ExecutionState,
		proposed.Spec.ExpiresAt, data, now)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, api.RuntimeApproval{}, store.ErrConflict
		}
		return false, api.RuntimeApproval{}, fmt.Errorf("insert RuntimeApproval: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, api.RuntimeApproval{}, fmt.Errorf("commit RuntimeApproval create: %w", err)
	}
	return true, proposed, nil
}

func (s *Store) ActivateRuntimeApprovalBindingKeys(ctx context.Context, keys runtimeapproval.BindingKeyStore) error {
	s.runtimeApprovalActivationMu.Lock()
	defer s.runtimeApprovalActivationMu.Unlock()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin RuntimeApproval binding-key activation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runtimeApprovalBindingEpochLock); err != nil {
		return fmt.Errorf("lock RuntimeApproval binding-key epoch: %w", err)
	}
	active, metadata, encoded, err := bindingKeyEpochSnapshot(ctx, keys)
	if err != nil {
		return err
	}
	var currentEpoch int64
	var currentVersion int64
	var currentMetadata []byte
	err = tx.QueryRow(ctx, `SELECT epoch, active_key_version, key_metadata FROM runtime_binding_key_state WHERE singleton = TRUE FOR UPDATE`).Scan(&currentEpoch, &currentVersion, &currentMetadata)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO runtime_binding_key_state (singleton, epoch, active_key_version, key_metadata) VALUES (TRUE, $1, $2, $3)`, active.Epoch, active.Version, encoded)
	} else if err == nil && active.Epoch == currentEpoch && active.Version == currentVersion && bindingKeyMetadataMatches(currentMetadata, metadata) {
		// Another replica already activated this exact epoch.
	} else if err == nil {
		if active.Epoch <= currentEpoch {
			return fmt.Errorf("runtime binding key epoch %d must advance beyond %d", active.Epoch, currentEpoch)
		}
		if err := validateBindingKeyRotation(currentVersion, currentMetadata, metadata); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE runtime_binding_key_state SET epoch = $1, active_key_version = $2, key_metadata = $3, updated_at = transaction_timestamp() WHERE singleton = TRUE`, active.Epoch, active.Version, encoded)
	}
	if err != nil {
		return fmt.Errorf("persist RuntimeApproval binding-key epoch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit RuntimeApproval binding-key activation: %w", err)
	}
	s.ConfigureRuntimeApprovalBindingKeys(keys)
	return nil
}

// RuntimeApprovalBindingKeysReady verifies local key material metadata against
// the durable activation witness used by every replica.
func (s *Store) RuntimeApprovalBindingKeysReady(ctx context.Context) error {
	active, metadata, _, err := bindingKeyEpochSnapshot(ctx, s.runtimeApprovalKeys())
	if err != nil {
		return err
	}
	var epoch int64
	var version int64
	var persisted []byte
	if err := s.pool.QueryRow(ctx, `SELECT epoch, active_key_version, key_metadata FROM runtime_binding_key_state WHERE singleton = TRUE`).Scan(&epoch, &version, &persisted); err != nil {
		return fmt.Errorf("read RuntimeApproval binding-key readiness: %w", err)
	}
	if epoch != active.Epoch || version != active.Version || !bindingKeyMetadataMatches(persisted, metadata) {
		return runtimeapproval.ErrBindingKeyUnavailable
	}
	return nil
}

func lockAndValidateRuntimeApprovalBindingEpoch(ctx context.Context, tx pgx.Tx, keys runtimeapproval.BindingKeyStore) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runtimeApprovalBindingEpochLock); err != nil {
		return fmt.Errorf("lock RuntimeApproval binding-key epoch: %w", err)
	}
	active, metadata, encoded, err := bindingKeyEpochSnapshot(ctx, keys)
	if err != nil {
		return err
	}
	var epoch int64
	var version int64
	var persisted []byte
	err = tx.QueryRow(ctx, `SELECT epoch, active_key_version, key_metadata FROM runtime_binding_key_state WHERE singleton = TRUE`).Scan(&epoch, &version, &persisted)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO runtime_binding_key_state (singleton, epoch, active_key_version, key_metadata) VALUES (TRUE, $1, $2, $3)`, active.Epoch, active.Version, encoded)
		if err == nil {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("read RuntimeApproval binding-key epoch: %w", err)
	}
	if epoch != active.Epoch || version != active.Version || !bindingKeyMetadataMatches(persisted, metadata) {
		return runtimeapproval.ErrBindingKeyUnavailable
	}
	return nil
}

func bindingKeyEpochSnapshot(ctx context.Context, keys runtimeapproval.BindingKeyStore) (runtimeapproval.BindingKey, []bindingKeyEpochMetadata, []byte, error) {
	active, all, err := runtimeapproval.ValidatedBindingKeys(ctx, keys)
	if err != nil {
		return runtimeapproval.BindingKey{}, nil, nil, err
	}
	metadata := make([]bindingKeyEpochMetadata, len(all))
	for index, key := range all {
		if key.Version <= 0 || key.Epoch <= 0 || key.Epoch > active.Epoch {
			return runtimeapproval.BindingKey{}, nil, nil, runtimeapproval.ErrBindingKeyUnavailable
		}
		digest := sha256.Sum256(key.Material)
		metadata[index] = bindingKeyEpochMetadata{Version: key.Version, Epoch: key.Epoch, Status: key.Status, Digest: base64.RawURLEncoding.EncodeToString(digest[:])}
	}
	encoded, err := json.Marshal(metadata)
	return active, metadata, encoded, err
}

func validateBindingKeyRotation(previousActive int64, previousJSON []byte, next []bindingKeyEpochMetadata) error {
	var previous []bindingKeyEpochMetadata
	if err := json.Unmarshal(previousJSON, &previous); err != nil {
		return fmt.Errorf("decode persisted runtime binding keys: %w", err)
	}
	nextByVersion := make(map[int64]bindingKeyEpochMetadata, len(next))
	for _, key := range next {
		nextByVersion[key.Version] = key
	}
	if nextByVersion[previousActive].Status != runtimeapproval.BindingKeyVerifyOnly {
		return fmt.Errorf("previous active runtime binding key %d must be retained verify-only", previousActive)
	}
	for _, key := range previous {
		retained, exists := nextByVersion[key.Version]
		if !exists {
			return fmt.Errorf("runtime binding key %d cannot be removed", key.Version)
		}
		if retained.Epoch != key.Epoch || retained.Digest != key.Digest {
			return fmt.Errorf("runtime binding key %d identity cannot change", key.Version)
		}
	}
	return nil
}

func metadataFromJSON(encoded []byte) []bindingKeyEpochMetadata {
	var metadata []bindingKeyEpochMetadata
	_ = json.Unmarshal(encoded, &metadata)
	return metadata
}

func bindingKeyMetadataMatches(encoded []byte, expected []bindingKeyEpochMetadata) bool {
	actual := metadataFromJSON(encoded)
	if len(actual) != len(expected) {
		return false
	}
	for index := range actual {
		if actual[index] != expected[index] {
			return false
		}
	}
	return true
}

func (s *Store) GetRuntimeApprovalByInvocation(ctx context.Context, workspace, invocationID string) (api.RuntimeApproval, error) {
	approval, found, err := scanRuntimeApproval(ctx, s.pool, workspace, invocationID, false)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if !found {
		approval, found, err = scanRuntimeApprovalTombstone(ctx, s.pool, workspace, invocationID)
		if err != nil {
			return api.RuntimeApproval{}, err
		}
		if !found {
			return api.RuntimeApproval{}, store.ErrNotFound
		}
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	return approval, nil
}

func (s *Store) CompactRuntimeApproval(ctx context.Context, workspace, invocationID string, retention time.Duration) (bool, error) {
	var compacted bool
	if err := s.pool.QueryRow(ctx, `SELECT compact_runtime_approval($1, $2, $3::interval)`,
		workspace, invocationID, retention.String()).Scan(&compacted); err != nil {
		return false, fmt.Errorf("compact RuntimeApproval: %w", err)
	}
	return compacted, nil
}

func (s *Store) RetireRuntimeApprovalTombstones(ctx context.Context, cutoff time.Time) (int64, error) {
	var retired int64
	if err := s.pool.QueryRow(ctx, `SELECT retire_runtime_approval_tombstones($1)`, cutoff).Scan(&retired); err != nil {
		return 0, fmt.Errorf("retire RuntimeApproval tombstones: %w", err)
	}
	return retired, nil
}

func (s *Store) DecideRuntimeApproval(ctx context.Context, workspace, invocationID string, decision store.RuntimeApprovalDecision) (api.RuntimeApproval, error) {
	if decision.Decision != api.RuntimeApprovalDecisionApproved && decision.Decision != api.RuntimeApprovalDecisionRejected {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval decision must be Approved or Rejected")
	}
	if decision.ExpectedResourceVersion <= 0 || !runtimeapproval.ValidDecisionActor(decision.Issuer, decision.Subject) {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval decision requires expected resource version and canonical actor")
	}
	return s.transitionRuntimeApprovalDecision(ctx, workspace, invocationID, decision.ExpectedResourceVersion, func(approval *api.RuntimeApproval, now time.Time) bool {
		approval.Status.DecidedByIssuer = decision.Issuer
		approval.Status.DecidedBySubject = decision.Subject
		if !now.Before(approval.Spec.ExpiresAt) {
			approval.Status.DecisionState = api.RuntimeApprovalDecisionExpired
		} else {
			approval.Status.DecisionState = decision.Decision
		}
		approval.Status.DecidedAt = timePointer(now.UTC())
		runtimeapproval.PopulateDecisionAudit(approval)
		return true
	})
}

func (s *Store) ExpireRuntimeApprovalDecision(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval expiry requires expected resource version")
	}
	return s.transitionRuntimeApprovalDecision(ctx, workspace, invocationID, expectedResourceVersion, func(approval *api.RuntimeApproval, now time.Time) bool {
		if now.Before(approval.Spec.ExpiresAt) {
			return false
		}
		approval.Status.DecisionState = api.RuntimeApprovalDecisionExpired
		approval.Status.DecidedAt = timePointer(now.UTC())
		runtimeapproval.PopulateDecisionAudit(approval)
		return true
	})
}

func (s *Store) AcknowledgeRuntimeApprovalDecisionAudit(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, expectedID string) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 || expectedID == "" {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval audit acknowledgement requires expected resource version and outbox ID")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("begin RuntimeApproval audit acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	approval, found, err := scanRuntimeApproval(ctx, tx, workspace, invocationID, true)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if !found {
		return api.RuntimeApproval{}, store.ErrNotFound
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
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
	approval.Status.DecisionEvidenceDelivered = true
	approval.ResourceVersion++
	data, err := json.Marshal(approval)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("marshal RuntimeApproval audit acknowledgement: %w", err)
	}
	command, err := tx.Exec(ctx, `UPDATE runtime_approvals SET data = $3, resource_version = $4, updated_at = transaction_timestamp()
		WHERE workspace = $1 AND invocation_id = $2 AND resource_version = $5`, workspace, invocationID, data, approval.ResourceVersion, expectedResourceVersion)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("update RuntimeApproval audit acknowledgement: %w", err)
	}
	if command.RowsAffected() != 1 {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("commit RuntimeApproval audit acknowledgement: %w", err)
	}
	return approval, nil
}

func (s *Store) transitionRuntimeApprovalDecision(
	ctx context.Context,
	workspace, invocationID string,
	expectedResourceVersion int64,
	transition func(*api.RuntimeApproval, time.Time) bool,
) (api.RuntimeApproval, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("begin RuntimeApproval decision: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	logicalKey := workspace + "\x1f" + invocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, logicalKey); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("lock RuntimeApproval decision: %w", err)
	}
	approval, found, err := scanRuntimeApproval(ctx, tx, workspace, invocationID, true)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if !found {
		return api.RuntimeApproval{}, store.ErrNotFound
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, s.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.Status.DecisionState != api.RuntimeApprovalDecisionPending {
		return approval, nil
	}
	if approval.ResourceVersion != expectedResourceVersion {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("read RuntimeApproval decision time: %w", err)
	}
	if !transition(&approval, now) {
		return approval, nil
	}
	approval.ResourceVersion++
	data, err := json.Marshal(approval)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("marshal RuntimeApproval decision: %w", err)
	}
	command, err := tx.Exec(ctx, `UPDATE runtime_approvals
		SET decision_state = $3, data = $4, resource_version = $5, updated_at = $6
		WHERE workspace = $1 AND invocation_id = $2 AND resource_version = $7 AND decision_state = $8`,
		workspace, invocationID, approval.Status.DecisionState, data,
		approval.ResourceVersion, now, expectedResourceVersion, api.RuntimeApprovalDecisionPending)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("update RuntimeApproval decision: %w", err)
	}
	if command.RowsAffected() != 1 {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("commit RuntimeApproval decision: %w", err)
	}
	return approval, nil
}

type runtimeApprovalQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanRuntimeApproval(ctx context.Context, query runtimeApprovalQuerier, workspace, invocationID string, forUpdate bool) (api.RuntimeApproval, bool, error) {
	statement := `SELECT data, resource_version, uid::text, name, workspace, invocation_id,
		binding_tag, binding_epoch, binding_key_version, decision_state, execution_state, created_at, expires_at
		FROM runtime_approvals WHERE workspace = $1 AND invocation_id = $2`
	if forUpdate {
		statement += ` FOR UPDATE`
	}
	var data []byte
	var resourceVersion int64
	var uid, name, persistedWorkspace, persistedInvocationID, bindingTag, decisionState, executionState string
	var bindingEpoch int64
	var bindingKeyVersion int64
	var createdAt, expiresAt time.Time
	err := query.QueryRow(ctx, statement, workspace, invocationID).Scan(
		&data, &resourceVersion, &uid, &name, &persistedWorkspace, &persistedInvocationID,
		&bindingTag, &bindingEpoch, &bindingKeyVersion, &decisionState, &executionState, &createdAt, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.RuntimeApproval{}, false, nil
	}
	if err != nil {
		return api.RuntimeApproval{}, false, fmt.Errorf("get RuntimeApproval: %w", err)
	}
	var approval api.RuntimeApproval
	if err := json.Unmarshal(data, &approval); err != nil {
		return api.RuntimeApproval{}, false, fmt.Errorf("unmarshal RuntimeApproval: %w", err)
	}
	runtimeapproval.NormalizeLegacyStoredApproval(&approval)
	if approval.Name != name || approval.Namespace != persistedWorkspace || approval.Spec.Workspace != persistedWorkspace || approval.Spec.InvocationID != persistedInvocationID ||
		approval.Spec.Binding.Tag != bindingTag || approval.Spec.Binding.Epoch != bindingEpoch || approval.Spec.Binding.KeyVersion != bindingKeyVersion ||
		approval.Status.DecisionState != decisionState || approval.Status.ExecutionState != executionState || !approval.Spec.CreatedAt.Equal(createdAt) || !approval.Spec.ExpiresAt.Equal(expiresAt) {
		return api.RuntimeApproval{}, false, fmt.Errorf("RuntimeApproval indexed columns do not match persisted document")
	}
	approval.UID = uid
	approval.ResourceVersion = resourceVersion
	approval.CreationTimestamp = createdAt.UTC()
	approval.Spec.CreatedAt = createdAt.UTC()
	approval.Spec.ExpiresAt = expiresAt.UTC()
	return approval, true, nil
}

func scanRuntimeApprovalTombstone(ctx context.Context, query runtimeApprovalQuerier, workspace, invocationID string) (api.RuntimeApproval, bool, error) {
	var approval api.RuntimeApproval
	var agentRef []byte
	var actorKind string
	var executorAction, workflowContext []byte
	var createdAt, expiresAt time.Time
	err := query.QueryRow(ctx, `SELECT runtime_approval_uid::text, name, plane, operation, agent_ref,
		actor_kind, executor_action, workflow_context,
		initial_decision, binding_format, binding_algorithm, binding_domain, binding_epoch,
		binding_key_version, binding_tag, decision_state, execution_state,
		COALESCE(execution_disposition, ''), created_at, expires_at
		FROM runtime_approval_tombstones WHERE workspace = $1 AND invocation_id = $2`, workspace, invocationID).Scan(
		&approval.UID, &approval.Name, &approval.Spec.Plane, &approval.Spec.Operation, &agentRef,
		&actorKind, &executorAction, &workflowContext,
		&approval.Spec.InitialDecision, &approval.Spec.Binding.Format, &approval.Spec.Binding.Algorithm,
		&approval.Spec.Binding.Domain, &approval.Spec.Binding.Epoch, &approval.Spec.Binding.KeyVersion,
		&approval.Spec.Binding.Tag, &approval.Status.DecisionState, &approval.Status.ExecutionState,
		&approval.Status.ExecutionDisposition, &createdAt, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return api.RuntimeApproval{}, false, nil
	}
	if err != nil {
		return api.RuntimeApproval{}, false, fmt.Errorf("get RuntimeApproval tombstone: %w", err)
	}
	if len(agentRef) > 0 {
		if err := json.Unmarshal(agentRef, &approval.Spec.AgentRef); err != nil {
			return api.RuntimeApproval{}, false, fmt.Errorf("unmarshal RuntimeApproval tombstone Agent reference: %w", err)
		}
	}
	if actorKind == api.RuntimeApprovalActorExecutor {
		approval.Spec.ActorKind = actorKind
		if err := json.Unmarshal(executorAction, &approval.Spec.ExecutorAction); err != nil || approval.Spec.ExecutorAction == nil {
			return api.RuntimeApproval{}, false, fmt.Errorf("invalid executor tombstone action: %v", err)
		}
		approval.Spec.TargetRef = approval.Spec.ExecutorAction.Service
	} else if actorKind != "Agent" {
		return api.RuntimeApproval{}, false, store.ErrConflict
	}
	if len(workflowContext) > 0 {
		if err := json.Unmarshal(workflowContext, &approval.Spec.WorkflowContext); err != nil {
			return api.RuntimeApproval{}, false, err
		}
	}
	approval.TypeMeta = api.TypeMeta{APIVersion: api.APIVersion, Kind: "RuntimeApproval"}
	approval.Namespace = workspace
	approval.Spec.Workspace = workspace
	approval.Spec.InvocationID = invocationID
	approval.Spec.CreatedAt = createdAt.UTC()
	approval.Spec.ExpiresAt = expiresAt.UTC()
	approval.CreationTimestamp = createdAt.UTC()
	approval.Status.DecisionEvidenceDelivered = true
	return approval, true, nil
}

func validateRuntimeApprovalCreate(approval api.RuntimeApproval, ttl time.Duration) error {
	return runtimeapproval.ValidateCreate(approval, ttl)
}

func timePointer(value time.Time) *time.Time { return &value }

var _ store.RuntimeApprovalStore = (*Store)(nil)
var _ store.RuntimeApprovalCompactionStore = (*Store)(nil)
