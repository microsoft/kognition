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
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const cognitiveQuotaKind = "CognitiveQuota"

// Store is the PostgreSQL-backed resource store.
type Store struct {
	pool                        *pgxpool.Pool
	logger                      *zap.Logger
	runtimeApprovalBindingMu    sync.RWMutex
	runtimeApprovalActivationMu sync.Mutex
	runtimeApprovalBindingKeys  runtimeapproval.BindingKeyStore

	// subscribers tracks active watches by kind.
	mu          sync.RWMutex
	subscribers map[string][]watchSubscriber

	// listening is true while StartNotifyListener's goroutine is active. When
	// set, write-path notifications are delivered via pg_notify instead of
	// directly (see notifyLocal) to avoid double-delivery.
	listening atomic.Bool

	listenerMu     sync.Mutex
	listenerCancel context.CancelFunc
	listenerDone   chan struct{}
}

// ConfigureRuntimeApprovalBindingKeys configures the trusted key boundary used
// by dedicated RuntimeApproval operations. Calls fail closed until configured.
func (s *Store) ConfigureRuntimeApprovalBindingKeys(keys runtimeapproval.BindingKeyStore) {
	s.runtimeApprovalBindingMu.Lock()
	defer s.runtimeApprovalBindingMu.Unlock()
	s.runtimeApprovalBindingKeys = keys
}

func (s *Store) runtimeApprovalKeys() runtimeapproval.BindingKeyStore {
	s.runtimeApprovalBindingMu.RLock()
	defer s.runtimeApprovalBindingMu.RUnlock()
	return s.runtimeApprovalBindingKeys
}

type watchSubscriber struct {
	ch        chan store.WatchEvent
	namespace string
}

// New creates a new PostgreSQL store with the given connection pool.
func New(pool *pgxpool.Pool, logger *zap.Logger) *Store {
	return &Store{
		pool:        pool,
		logger:      logger,
		subscribers: make(map[string][]watchSubscriber),
	}
}

// Ping checks database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return err
	}
	if s.runtimeApprovalKeys() != nil {
		return s.RuntimeApprovalBindingKeysReady(ctx)
	}
	return nil
}

func (s *Store) ReserveToolGrantInvocation(ctx context.Context, reservation store.ToolGrantRateReservation) error {
	if reservation.Key == "" || reservation.Now.IsZero() {
		return errors.New("ToolGrant rate reservation key and time are required")
	}
	if reservation.CallsPerMinute < 0 || reservation.CallsPerHour < 0 || reservation.CallsPerDay < 0 {
		return errors.New("ToolGrant rate limits must be non-negative")
	}
	now := reservation.Now.UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin ToolGrant rate reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO tool_grant_rate_limits
			(rate_key, minute_reset, hour_reset, day_reset)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (rate_key) DO NOTHING`,
		reservation.Key, now.Add(time.Minute), now.Add(time.Hour), now.Add(24*time.Hour)); err != nil {
		return fmt.Errorf("initialize ToolGrant rate reservation: %w", err)
	}
	var minuteCount, hourCount, dayCount int64
	var minuteReset, hourReset, dayReset time.Time
	if err := tx.QueryRow(ctx, `
		SELECT minute_count, hour_count, day_count, minute_reset, hour_reset, day_reset
		FROM tool_grant_rate_limits
		WHERE rate_key = $1
		FOR UPDATE`, reservation.Key).Scan(
		&minuteCount, &hourCount, &dayCount, &minuteReset, &hourReset, &dayReset,
	); err != nil {
		return fmt.Errorf("lock ToolGrant rate reservation: %w", err)
	}
	if !now.Before(minuteReset) {
		minuteCount = 0
		minuteReset = now.Add(time.Minute)
	}
	if !now.Before(hourReset) {
		hourCount = 0
		hourReset = now.Add(time.Hour)
	}
	if !now.Before(dayReset) {
		dayCount = 0
		dayReset = now.Add(24 * time.Hour)
	}
	if reservation.CallsPerMinute > 0 && minuteCount >= reservation.CallsPerMinute ||
		reservation.CallsPerHour > 0 && hourCount >= reservation.CallsPerHour ||
		reservation.CallsPerDay > 0 && dayCount >= reservation.CallsPerDay {
		return store.ErrQuotaExceeded
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tool_grant_rate_limits
		SET minute_count = $2, hour_count = $3, day_count = $4,
			minute_reset = $5, hour_reset = $6, day_reset = $7,
			updated_at = transaction_timestamp()
		WHERE rate_key = $1`, reservation.Key, minuteCount+1, hourCount+1, dayCount+1, minuteReset, hourReset, dayReset); err != nil {
		return fmt.Errorf("update ToolGrant rate reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ToolGrant rate reservation: %w", err)
	}
	return nil
}

// ClaimCanaryOperation atomically creates the immutable replay fence for one
// reasoning canary. Exact duplicates return the stored operation.
func (s *Store) GetCanaryOperation(ctx context.Context, operationID string) (store.CanaryOperation, error) {
	if operationID == "" {
		return store.CanaryOperation{}, fmt.Errorf("canary operation ID is required")
	}
	operation, found, err := scanCanaryOperation(ctx, s.pool, operationID, false)
	if err != nil {
		return store.CanaryOperation{}, err
	}
	if !found {
		operation, found, err = scanCanaryOperationTombstone(ctx, s.pool, operationID)
		if err != nil {
			return store.CanaryOperation{}, err
		}
		if !found {
			return store.CanaryOperation{}, store.ErrNotFound
		}
	}
	return operation, nil
}

func (s *Store) ClaimCanaryOperation(ctx context.Context, operation store.CanaryOperation) (bool, store.CanaryOperation, error) {
	if operation.OperationID == "" || operation.RequestDigest == "" || operation.Phase != store.CanaryClaimed {
		return false, store.CanaryOperation{}, fmt.Errorf("canary claim requires an operation ID, request digest, and CanaryClaimed phase")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("begin canary operation claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operation.OperationID); err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("lock canary operation: %w", err)
	}
	existing, found, err := scanCanaryOperation(ctx, tx, operation.OperationID, false)
	if err != nil {
		return false, store.CanaryOperation{}, err
	}
	if found {
		if existing.RequestDigest != operation.RequestDigest {
			return false, store.CanaryOperation{}, store.ErrConflict
		}
		return false, existing, nil
	}
	existing, found, err = scanCanaryOperationTombstone(ctx, tx, operation.OperationID)
	if err != nil {
		return false, store.CanaryOperation{}, err
	}
	if found {
		if existing.RequestDigest != operation.RequestDigest {
			return false, store.CanaryOperation{}, store.ErrConflict
		}
		return false, existing, nil
	}
	now := operation.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO canary_operations
		(operation_id, request_digest, phase, result, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)`, operation.OperationID, operation.RequestDigest, operation.Phase, operation.Result, now); err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("insert canary operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("commit canary operation claim: %w", err)
	}
	operation.CreatedAt = now
	operation.UpdatedAt = now
	return true, operation, nil
}

// TransitionCanaryOperation conditionally advances an operation. Losing a
// race returns the current durable state without modifying it.
func (s *Store) TransitionCanaryOperation(ctx context.Context, operationID, requestDigest string, from, to store.CanaryOperationPhase, terminal *store.CanaryTerminalRecord) (bool, store.CanaryOperation, error) {
	if operationID == "" || requestDigest == "" || from == "" || to == "" {
		return false, store.CanaryOperation{}, fmt.Errorf("canary transition requires operation ID, request digest, and phases")
	}
	if !validCanaryTransition(from, to) {
		return false, store.CanaryOperation{}, fmt.Errorf("invalid canary transition %s -> %s", from, to)
	}
	if terminal != nil && to != store.CanaryProviderStarted && to != store.CanaryProviderCompleted && !(from == store.CanaryClaimed && to == store.CanaryFinalizing) {
		return false, store.CanaryOperation{}, fmt.Errorf("canary terminal record may only be recorded at ProviderStarted, ProviderCompleted, or pre-call Finalizing")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("begin canary operation transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operationID); err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("lock canary operation: %w", err)
	}
	existing, found, err := scanCanaryOperation(ctx, tx, operationID, true)
	if err != nil {
		return false, store.CanaryOperation{}, err
	}
	if !found {
		return false, store.CanaryOperation{}, store.ErrNotFound
	}
	if existing.RequestDigest != requestDigest {
		return false, store.CanaryOperation{}, store.ErrConflict
	}
	if existing.Phase != from {
		return false, existing, nil
	}
	if (to == store.CanaryProviderStarted || to == store.CanaryProviderCompleted) && (terminal == nil || len(terminal.Result) == 0 || len(terminal.UsageEvent) == 0 || len(terminal.AuditEvent) == 0) {
		return false, store.CanaryOperation{}, fmt.Errorf("%s requires result, usage, and audit outbox payloads", to)
	}
	if from == store.CanaryClaimed && to == store.CanaryFinalizing && (terminal == nil || len(terminal.Result) == 0 || len(terminal.UsageEvent) != 0 || len(terminal.AuditEvent) == 0) {
		return false, store.CanaryOperation{}, fmt.Errorf("pre-call finalization requires result and audit outbox payloads without usage")
	}
	var result, usageEvent, auditEvent json.RawMessage
	var reservationCreated bool
	if terminal != nil {
		result = terminal.Result
		usageEvent = terminal.UsageEvent
		auditEvent = terminal.AuditEvent
		reservationCreated = terminal.ReservationCreated
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE canary_operations
		SET phase = $2, result = COALESCE($3, result), usage_event = COALESCE($4, usage_event), audit_event = COALESCE($5, audit_event), reservation_created = $6, updated_at = $7 WHERE operation_id = $1`, operationID, to, result, usageEvent, auditEvent, reservationCreated, now); err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("transition canary operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, store.CanaryOperation{}, fmt.Errorf("commit canary operation transition: %w", err)
	}
	existing.Phase = to
	if len(result) > 0 {
		existing.Result = result
		existing.UsageEvent = usageEvent
		existing.AuditEvent = auditEvent
		existing.ReservationCreated = reservationCreated
	}
	existing.UpdatedAt = now
	return true, existing, nil
}

func validCanaryTransition(from, to store.CanaryOperationPhase) bool {
	switch from {
	case store.CanaryClaimed:
		return to == store.CanaryProviderStarted || to == store.CanaryFinalizing
	case store.CanaryProviderStarted:
		return to == store.CanaryProviderCompleted
	case store.CanaryProviderCompleted:
		return to == store.CanaryFinalizing
	case store.CanaryFinalizing:
		return to == store.CanaryCompleted
	default:
		return false
	}
}

// AcknowledgeCanaryFinalization durably marks one completed idempotent effect.
func (s *Store) AcknowledgeCanaryFinalization(ctx context.Context, operationID, requestDigest string, step store.CanaryFinalizationStep) (store.CanaryOperation, error) {
	column := ""
	switch step {
	case store.CanaryReservationFinalized:
		column = "reservation_finalized"
	case store.CanaryUsageApplied:
		column = "usage_applied"
	case store.CanaryAuditDelivered:
		column = "audit_delivered"
	default:
		return store.CanaryOperation{}, fmt.Errorf("unknown canary finalization step %q", step)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.CanaryOperation{}, fmt.Errorf("begin canary finalization acknowledgement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operationID); err != nil {
		return store.CanaryOperation{}, fmt.Errorf("lock canary operation: %w", err)
	}
	operation, found, err := scanCanaryOperation(ctx, tx, operationID, true)
	if err != nil {
		return store.CanaryOperation{}, err
	}
	if !found {
		return store.CanaryOperation{}, store.ErrNotFound
	}
	if operation.RequestDigest != requestDigest {
		return store.CanaryOperation{}, store.ErrConflict
	}
	if operation.Phase != store.CanaryFinalizing && operation.Phase != store.CanaryCompleted {
		return store.CanaryOperation{}, fmt.Errorf("canary operation %q is not finalizing", operationID)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE canary_operations SET `+column+` = TRUE, updated_at = $2 WHERE operation_id = $1`, operationID, now); err != nil {
		return store.CanaryOperation{}, fmt.Errorf("acknowledge canary finalization step %q: %w", step, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CanaryOperation{}, fmt.Errorf("commit canary finalization acknowledgement: %w", err)
	}
	switch step {
	case store.CanaryReservationFinalized:
		operation.ReservationFinalized = true
	case store.CanaryUsageApplied:
		operation.UsageApplied = true
	case store.CanaryAuditDelivered:
		operation.AuditDelivered = true
	}
	operation.UpdatedAt = now
	return operation, nil
}

func scanCanaryOperation(ctx context.Context, query toolInvocationReceiptQuerier, operationID string, forUpdate bool) (store.CanaryOperation, bool, error) {
	statement := `SELECT operation_id, request_digest, phase, result, usage_event, audit_event, reservation_created, reservation_finalized, usage_applied, audit_delivered, created_at, updated_at
		FROM canary_operations WHERE operation_id = $1`
	if forUpdate {
		statement += ` FOR UPDATE`
	}
	var operation store.CanaryOperation
	err := query.QueryRow(ctx, statement, operationID).Scan(
		&operation.OperationID, &operation.RequestDigest, &operation.Phase, &operation.Result,
		&operation.UsageEvent, &operation.AuditEvent,
		&operation.ReservationCreated,
		&operation.ReservationFinalized, &operation.UsageApplied, &operation.AuditDelivered,
		&operation.CreatedAt, &operation.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CanaryOperation{}, false, nil
	}
	if err != nil {
		return store.CanaryOperation{}, false, fmt.Errorf("get canary operation: %w", err)
	}
	return operation, true, nil
}

func scanCanaryOperationTombstone(ctx context.Context, query toolInvocationReceiptQuerier, operationID string) (store.CanaryOperation, bool, error) {
	var operation store.CanaryOperation
	err := query.QueryRow(ctx, `SELECT operation_id, request_digest, result, created_at, completed_at
		FROM canary_operation_tombstones WHERE operation_id = $1`, operationID).Scan(
		&operation.OperationID, &operation.RequestDigest, &operation.Result, &operation.CreatedAt, &operation.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CanaryOperation{}, false, nil
	}
	if err != nil {
		return store.CanaryOperation{}, false, fmt.Errorf("get canary operation tombstone: %w", err)
	}
	operation.Phase = store.CanaryCompleted
	operation.ReservationFinalized = true
	operation.UsageApplied = true
	operation.AuditDelivered = true
	return operation, true, nil
}

// CompactCanaryOperationsBefore atomically replaces fully finalized completed
// operations with permanent metadata-only replay fences.
func (s *Store) CompactCanaryOperationsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT operation_id FROM canary_operations
		WHERE phase = $1 AND updated_at < $2
		AND reservation_finalized AND audit_delivered
		AND (usage_event IS NULL OR usage_applied)
		ORDER BY updated_at`, store.CanaryCompleted, cutoff)
	if err != nil {
		return 0, fmt.Errorf("list compactable canary operations: %w", err)
	}
	var operationIDs []string
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan compactable canary operation: %w", err)
		}
		operationIDs = append(operationIDs, operationID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("list compactable canary operations: %w", err)
	}
	rows.Close()

	var compacted int64
	for _, operationID := range operationIDs {
		changed, err := s.compactCanaryOperation(ctx, operationID, cutoff)
		if err != nil {
			return compacted, err
		}
		if changed {
			compacted++
		}
	}
	return compacted, nil
}

func (s *Store) compactCanaryOperation(ctx context.Context, operationID string, cutoff time.Time) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin canary operation compaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, operationID); err != nil {
		return false, fmt.Errorf("lock canary operation for compaction: %w", err)
	}
	operation, found, err := scanCanaryOperation(ctx, tx, operationID, true)
	if err != nil {
		return false, err
	}
	if !found || operation.Phase != store.CanaryCompleted || !operation.UpdatedAt.Before(cutoff) ||
		!operation.ReservationFinalized || !operation.AuditDelivered || (len(operation.UsageEvent) > 0 && !operation.UsageApplied) {
		return false, nil
	}
	resultDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(operation.Result))
	if _, err := tx.Exec(ctx, `INSERT INTO canary_operation_tombstones
		(operation_id, request_digest, result, result_digest, created_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, operation.OperationID, operation.RequestDigest,
		[]byte(operation.Result), resultDigest, operation.CreatedAt, operation.UpdatedAt); err != nil {
		return false, fmt.Errorf("insert canary operation tombstone: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM canary_operations WHERE operation_id = $1`, operationID); err != nil {
		return false, fmt.Errorf("delete compacted canary operation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit canary operation compaction: %w", err)
	}
	return true, nil
}

// ClaimToolInvocation atomically creates a pending invocation receipt. An
// exact duplicate returns its stored receipt; a reused ID with a different
// binding is rejected before it can repeat a side effect.
func (s *Store) ClaimToolInvocation(ctx context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("tool invocation receipt requires namespace, invocation ID, and binding")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("begin tool invocation receipt claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := receipt.Namespace + "/" + receipt.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("lock tool invocation receipt: %w", err)
	}
	existing, found, err := scanToolInvocationReceipt(ctx, tx, receipt.Namespace, receipt.InvocationID, false)
	if err != nil {
		return false, store.ToolInvocationReceipt{}, err
	}
	if found {
		if existing.Binding != receipt.Binding {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		if existing.Status == store.ToolInvocationAwaitingApproval {
			if _, err := tx.Exec(ctx, `UPDATE tool_invocation_receipts
				SET status = $3, completed_at = NULL
				WHERE namespace = $1 AND invocation_id = $2`, receipt.Namespace, receipt.InvocationID, store.ToolInvocationPending); err != nil {
				return false, store.ToolInvocationReceipt{}, fmt.Errorf("reclaim tool invocation awaiting approval: %w", err)
			}
			existing.Status = store.ToolInvocationPending
			existing.CompletedAt = nil
			if err := tx.Commit(ctx); err != nil {
				return false, store.ToolInvocationReceipt{}, fmt.Errorf("commit tool invocation approval reclaim: %w", err)
			}
			return true, existing, nil
		}
		return false, existing, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tool_invocation_receipts
		(namespace, invocation_id, binding, status, created_at)
		VALUES ($1, $2, $3, $4, $5)`, receipt.Namespace, receipt.InvocationID, receipt.Binding, receipt.Status, receipt.CreatedAt); err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("insert tool invocation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("commit tool invocation receipt claim: %w", err)
	}
	return true, receipt, nil
}

// PrepareToolInvocation persists the shared preparation token before the
// RuntimeApproval execution claim. An identical preparation is resumable.
func (s *Store) PrepareToolInvocation(ctx context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" || receipt.Status != store.ToolInvocationPrepared || receipt.PreparationToken == "" || receipt.PreparationDeadline == nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("prepared tool invocation receipt requires namespace, invocation ID, binding, token, and deadline")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("begin tool invocation receipt preparation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := receipt.Namespace + "/" + receipt.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("lock tool invocation receipt: %w", err)
	}
	existing, found, err := scanToolInvocationReceipt(ctx, tx, receipt.Namespace, receipt.InvocationID, false)
	if err != nil {
		return false, store.ToolInvocationReceipt{}, err
	}
	if found {
		if existing.Binding != receipt.Binding || (existing.Status != store.ToolInvocationPending && existing.Status != store.ToolInvocationPrepared) {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		if existing.Status == store.ToolInvocationPending {
			if _, err := tx.Exec(ctx, `UPDATE tool_invocation_receipts
				SET status = $3, preparation_token = $4, preparation_deadline = $5
				WHERE namespace = $1 AND invocation_id = $2`, receipt.Namespace, receipt.InvocationID, receipt.Status, receipt.PreparationToken, receipt.PreparationDeadline); err != nil {
				return false, store.ToolInvocationReceipt{}, fmt.Errorf("update pending tool invocation preparation: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, store.ToolInvocationReceipt{}, fmt.Errorf("commit pending tool invocation preparation: %w", err)
			}
			return true, receipt, nil
		}
		if existing.PreparationToken != receipt.PreparationToken || existing.PreparationDeadline == nil || !existing.PreparationDeadline.Equal(*receipt.PreparationDeadline) {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		return true, existing, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tool_invocation_receipts
		(namespace, invocation_id, binding, status, preparation_token, preparation_deadline, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, receipt.Namespace, receipt.InvocationID, receipt.Binding, receipt.Status, receipt.PreparationToken, receipt.PreparationDeadline, receipt.CreatedAt); err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("insert prepared tool invocation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("commit tool invocation receipt preparation: %w", err)
	}
	return true, receipt, nil
}

// ReleasePreparedToolInvocation removes only the exact preparation owned by
// the caller. A claimed or terminal receipt is never eligible for cleanup.
func (s *Store) ReleasePreparedToolInvocation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	if namespace == "" || invocationID == "" || preparationToken == "" {
		return false, fmt.Errorf("prepared tool invocation release requires namespace, invocation ID, and preparation token")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin prepared tool invocation release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := namespace + "/" + invocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return false, fmt.Errorf("lock prepared tool invocation release: %w", err)
	}
	existing, found, err := scanToolInvocationReceipt(ctx, tx, namespace, invocationID, true)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if existing.Status != store.ToolInvocationPrepared || existing.PreparationToken != preparationToken {
		return false, store.ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tool_invocation_receipts WHERE namespace = $1 AND invocation_id = $2`, namespace, invocationID); err != nil {
		return false, fmt.Errorf("delete prepared tool invocation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit prepared tool invocation release: %w", err)
	}
	return true, nil
}

// CompleteToolInvocation parks an invocation awaiting approval or stores a
// terminal Tool result. Repeated writes preserve the first durable outcome.
func (s *Store) CompleteToolInvocation(ctx context.Context, receipt store.ToolInvocationReceipt) error {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" || receipt.Status == store.ToolInvocationPending {
		return fmt.Errorf("tool invocation completion requires a terminal receipt")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tool invocation receipt completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := receipt.Namespace + "/" + receipt.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return fmt.Errorf("lock tool invocation receipt: %w", err)
	}
	existing, found, err := scanToolInvocationReceipt(ctx, tx, receipt.Namespace, receipt.InvocationID, true)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrNotFound
	}
	if existing.Binding != receipt.Binding {
		return store.ErrConflict
	}
	if existing.Status != store.ToolInvocationPending && existing.Status != store.ToolInvocationPrepared {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE tool_invocation_receipts
		SET status = $3, output = $4, execution_mode = $5, duration_ms = $6, error_message = $7,
			preparation_token = '', preparation_deadline = NULL, completed_at = $8
		WHERE namespace = $1 AND invocation_id = $2`, receipt.Namespace, receipt.InvocationID, receipt.Status, receipt.Output, receipt.ExecutionMode, receipt.DurationMS, receipt.ErrorMessage, receipt.CompletedAt); err != nil {
		return fmt.Errorf("complete tool invocation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tool invocation receipt completion: %w", err)
	}
	return nil
}

type toolInvocationReceiptQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func scanToolInvocationReceipt(ctx context.Context, query toolInvocationReceiptQuerier, namespace, invocationID string, forUpdate bool) (store.ToolInvocationReceipt, bool, error) {
	statement := `SELECT namespace, invocation_id, binding, status, output, execution_mode, duration_ms, error_message, preparation_token, preparation_deadline, created_at, completed_at
		FROM tool_invocation_receipts WHERE namespace = $1 AND invocation_id = $2`
	if forUpdate {
		statement += ` FOR UPDATE`
	}
	var receipt store.ToolInvocationReceipt
	err := query.QueryRow(ctx, statement, namespace, invocationID).Scan(
		&receipt.Namespace, &receipt.InvocationID, &receipt.Binding, &receipt.Status, &receipt.Output,
		&receipt.ExecutionMode, &receipt.DurationMS, &receipt.ErrorMessage, &receipt.PreparationToken, &receipt.PreparationDeadline, &receipt.CreatedAt, &receipt.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ToolInvocationReceipt{}, false, nil
	}
	if err != nil {
		return store.ToolInvocationReceipt{}, false, fmt.Errorf("get tool invocation receipt: %w", err)
	}
	return receipt, true, nil
}

func (s *Store) GetMemoryMutation(ctx context.Context, namespace, invocationID string) (store.MemoryMutationReceipt, bool, error) {
	return scanMemoryMutationReceipt(ctx, s.pool, namespace, invocationID, false)
}

// PrepareMemoryMutation persists the exact preparation fence before the
// RuntimeApproval execution claim. An identical preparation is resumable.
func (s *Store) PrepareMemoryMutation(ctx context.Context, receipt store.MemoryMutationReceipt) (bool, store.MemoryMutationReceipt, error) {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" || (receipt.Operation != "store" && receipt.Operation != "delete") || receipt.Status != store.MemoryMutationPrepared || receipt.PreparationToken == "" || receipt.PreparationDeadline == nil {
		return false, store.MemoryMutationReceipt{}, fmt.Errorf("prepared Memory mutation receipt requires namespace, invocation ID, binding, operation, token, and deadline")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, store.MemoryMutationReceipt{}, fmt.Errorf("begin Memory mutation receipt preparation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := receipt.Namespace + "/" + receipt.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return false, store.MemoryMutationReceipt{}, fmt.Errorf("lock Memory mutation receipt: %w", err)
	}
	existing, found, err := scanMemoryMutationReceipt(ctx, tx, receipt.Namespace, receipt.InvocationID, false)
	if err != nil {
		return false, store.MemoryMutationReceipt{}, err
	}
	if found {
		if existing.Binding != receipt.Binding || existing.Operation != receipt.Operation || existing.Status != store.MemoryMutationPrepared || existing.PreparationToken != receipt.PreparationToken || existing.PreparationDeadline == nil || !existing.PreparationDeadline.Equal(*receipt.PreparationDeadline) {
			return false, store.MemoryMutationReceipt{}, store.ErrConflict
		}
		return true, existing, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memory_mutation_receipts
		(namespace, invocation_id, binding, operation, status, preparation_token, preparation_deadline, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, receipt.Namespace, receipt.InvocationID, receipt.Binding, receipt.Operation, receipt.Status, receipt.PreparationToken, receipt.PreparationDeadline, receipt.CreatedAt); err != nil {
		return false, store.MemoryMutationReceipt{}, fmt.Errorf("insert prepared Memory mutation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, store.MemoryMutationReceipt{}, fmt.Errorf("commit Memory mutation receipt preparation: %w", err)
	}
	return true, receipt, nil
}

// ReleasePreparedMemoryMutation removes only the exact preparation owned by
// the caller. A terminal receipt is never eligible for cleanup.
func (s *Store) ReleasePreparedMemoryMutation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	if namespace == "" || invocationID == "" || preparationToken == "" {
		return false, fmt.Errorf("prepared Memory mutation release requires namespace, invocation ID, and preparation token")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin prepared Memory mutation release: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := namespace + "/" + invocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return false, fmt.Errorf("lock prepared Memory mutation release: %w", err)
	}
	existing, found, err := scanMemoryMutationReceipt(ctx, tx, namespace, invocationID, true)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if existing.Status != store.MemoryMutationPrepared || existing.PreparationToken != preparationToken {
		return false, store.ErrConflict
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_mutation_receipts WHERE namespace = $1 AND invocation_id = $2`, namespace, invocationID); err != nil {
		return false, fmt.Errorf("delete prepared Memory mutation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit prepared Memory mutation release: %w", err)
	}
	return true, nil
}

// CompleteMemoryMutation persists the first terminal mutation outcome.
func (s *Store) CompleteMemoryMutation(ctx context.Context, receipt store.MemoryMutationReceipt) error {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" || (receipt.Operation != "store" && receipt.Operation != "delete") || (receipt.Status != store.MemoryMutationSucceeded && receipt.Status != store.MemoryMutationFailed) || receipt.CompletedAt == nil {
		return fmt.Errorf("Memory mutation completion requires namespace, invocation ID, binding, operation, terminal status, and completion time")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Memory mutation receipt completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockID := receipt.Namespace + "/" + receipt.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockID); err != nil {
		return fmt.Errorf("lock Memory mutation receipt: %w", err)
	}
	existing, found, err := scanMemoryMutationReceipt(ctx, tx, receipt.Namespace, receipt.InvocationID, true)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrNotFound
	}
	if existing.Binding != receipt.Binding || existing.Operation != receipt.Operation {
		return store.ErrConflict
	}
	if existing.Status != store.MemoryMutationPrepared {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE memory_mutation_receipts
		SET status = $3, result = $4, error_message = $5, preparation_token = '', preparation_deadline = NULL, completed_at = $6
		WHERE namespace = $1 AND invocation_id = $2`, receipt.Namespace, receipt.InvocationID, receipt.Status, receipt.Result, receipt.ErrorMessage, receipt.CompletedAt); err != nil {
		return fmt.Errorf("complete Memory mutation receipt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Memory mutation receipt completion: %w", err)
	}
	return nil
}

// ListExpiredMemoryMutations returns a bounded deadline-ordered snapshot for
// reconciliation. Exact-token release remains authoritative.
func (s *Store) ListExpiredMemoryMutations(ctx context.Context, limit int) ([]store.MemoryMutationReceipt, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT namespace, invocation_id, binding, operation, status, result, error_message, preparation_token, preparation_deadline, created_at, completed_at
		 FROM memory_mutation_receipts
		 WHERE status = 'Prepared' AND preparation_deadline <= clock_timestamp()
		 ORDER BY preparation_deadline, namespace, invocation_id
		 LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired Memory mutations: %w", err)
	}
	defer rows.Close()

	receipts := make([]store.MemoryMutationReceipt, 0)
	for rows.Next() {
		var receipt store.MemoryMutationReceipt
		if err := rows.Scan(
			&receipt.Namespace, &receipt.InvocationID, &receipt.Binding, &receipt.Operation, &receipt.Status,
			&receipt.Result, &receipt.ErrorMessage, &receipt.PreparationToken, &receipt.PreparationDeadline, &receipt.CreatedAt, &receipt.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan expired Memory mutation: %w", err)
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired Memory mutations: %w", err)
	}
	return receipts, nil
}

func scanMemoryMutationReceipt(ctx context.Context, query toolInvocationReceiptQuerier, namespace, invocationID string, forUpdate bool) (store.MemoryMutationReceipt, bool, error) {
	statement := `SELECT namespace, invocation_id, binding, operation, status, result, error_message, preparation_token, preparation_deadline, created_at, completed_at
		FROM memory_mutation_receipts WHERE namespace = $1 AND invocation_id = $2`
	if forUpdate {
		statement += ` FOR UPDATE`
	}
	var receipt store.MemoryMutationReceipt
	err := query.QueryRow(ctx, statement, namespace, invocationID).Scan(
		&receipt.Namespace, &receipt.InvocationID, &receipt.Binding, &receipt.Operation, &receipt.Status,
		&receipt.Result, &receipt.ErrorMessage, &receipt.PreparationToken, &receipt.PreparationDeadline, &receipt.CreatedAt, &receipt.CompletedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MemoryMutationReceipt{}, false, nil
	}
	if err != nil {
		return store.MemoryMutationReceipt{}, false, fmt.Errorf("get Memory mutation receipt: %w", err)
	}
	return receipt, true, nil
}

// PrepareToolCallReservation atomically charges one Tool call and persists the
// exact preparation fence so retries cannot charge the invocation twice.
func (s *Store) PrepareToolCallReservation(ctx context.Context, reservation store.ToolCallReservation) (bool, error) {
	if reservation.Namespace == "" || reservation.InvocationID == "" || reservation.Binding == "" || reservation.PreparationToken == "" {
		return false, fmt.Errorf("Tool call reservation namespace, invocation ID, binding, and preparation token are required")
	}
	if reservation.PreparationDeadline.IsZero() {
		return false, fmt.Errorf("Tool call reservation preparation deadline is required")
	}
	if !time.Now().UTC().Before(reservation.PreparationDeadline) {
		return false, fmt.Errorf("Tool call reservation preparation deadline must be in the future")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin Tool call reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reservationKey := reservation.Namespace + "/" + reservation.InvocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, reservationKey); err != nil {
		return false, fmt.Errorf("lock Tool call reservation: %w", err)
	}

	var existing store.ToolCallReservation
	err = tx.QueryRow(ctx,
		`SELECT namespace, invocation_id, binding, preparation_token, preparation_deadline
		 FROM tool_call_reservations WHERE namespace = $1 AND invocation_id = $2`,
		reservation.Namespace, reservation.InvocationID,
	).Scan(&existing.Namespace, &existing.InvocationID, &existing.Binding, &existing.PreparationToken, &existing.PreparationDeadline)
	if err == nil {
		if existing.Namespace == reservation.Namespace && existing.InvocationID == reservation.InvocationID &&
			existing.Binding == reservation.Binding && existing.PreparationToken == reservation.PreparationToken &&
			existing.PreparationDeadline.Equal(reservation.PreparationDeadline) {
			return false, nil
		}
		return false, store.ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("get Tool call reservation: %w", err)
	}

	quotas, err := lockCostQuotas(ctx, tx, reservation.Namespace)
	if err != nil {
		return false, err
	}
	for index := range quotas {
		if err := reserveQuotaToolCall(&quotas[index].quota); err != nil {
			return false, fmt.Errorf("%w: %w", store.ErrQuotaExceeded, err)
		}
	}
	quotaPeriods := make(map[string]quotaPeriod, len(quotas))
	for index := range quotas {
		quotaPeriods[quotas[index].name] = quotaPeriod{
			ResourceID: quotas[index].resourceID,
			ResetTime:  quotas[index].quota.Status.LastResetTime,
		}
	}
	periodData, err := json.Marshal(quotaPeriods)
	if err != nil {
		return false, fmt.Errorf("marshal Tool call reservation quota periods: %w", err)
	}
	if err := persistLockedTokenQuotas(ctx, tx, quotas); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tool_call_reservations
		 (namespace, invocation_id, binding, preparation_token, preparation_deadline, quota_periods)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		reservation.Namespace, reservation.InvocationID, reservation.Binding,
		reservation.PreparationToken, reservation.PreparationDeadline, periodData,
	); err != nil {
		return false, fmt.Errorf("insert Tool call reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit Tool call reservation: %w", err)
	}
	return true, nil
}

// SettleToolCallReservation removes a consumed preparation while preserving
// the Tool-call charge. The preparation token fences ownership of the action.
func (s *Store) SettleToolCallReservation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	return s.finalizeToolCallReservation(ctx, namespace, invocationID, preparationToken, false)
}

// ReleaseToolCallReservation removes an unused preparation and refunds the
// Tool-call charge only in quota periods charged by that preparation.
func (s *Store) ReleaseToolCallReservation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	return s.finalizeToolCallReservation(ctx, namespace, invocationID, preparationToken, true)
}

// ListExpiredToolCallReservations returns a bounded deadline-ordered snapshot
// for reconciliation. Exact-token finalization remains authoritative.
func (s *Store) ListExpiredToolCallReservations(ctx context.Context, limit int) ([]store.ToolCallReservation, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT namespace, invocation_id, binding, preparation_token, preparation_deadline
		 FROM tool_call_reservations
		 WHERE preparation_deadline <= clock_timestamp()
		 ORDER BY preparation_deadline, namespace, invocation_id
		 LIMIT $1`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired Tool call reservations: %w", err)
	}
	defer rows.Close()

	reservations := make([]store.ToolCallReservation, 0)
	for rows.Next() {
		var reservation store.ToolCallReservation
		if err := rows.Scan(&reservation.Namespace, &reservation.InvocationID, &reservation.Binding, &reservation.PreparationToken, &reservation.PreparationDeadline); err != nil {
			return nil, fmt.Errorf("scan expired Tool call reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired Tool call reservations: %w", err)
	}
	return reservations, nil
}

func (s *Store) finalizeToolCallReservation(ctx context.Context, namespace, invocationID, preparationToken string, refund bool) (bool, error) {
	if namespace == "" || invocationID == "" || preparationToken == "" {
		return false, fmt.Errorf("Tool call reservation namespace, invocation ID, and preparation token are required")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin Tool call reservation finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	reservationKey := namespace + "/" + invocationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, reservationKey); err != nil {
		return false, fmt.Errorf("lock Tool call reservation: %w", err)
	}
	var existingToken string
	var periodData []byte
	err = tx.QueryRow(ctx,
		`SELECT preparation_token, quota_periods FROM tool_call_reservations
		 WHERE namespace = $1 AND invocation_id = $2 FOR UPDATE`, namespace, invocationID,
	).Scan(&existingToken, &periodData)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get Tool call reservation for finalization: %w", err)
	}
	if existingToken != preparationToken {
		return false, store.ErrConflict
	}
	if refund {
		var quotaPeriods map[string]quotaPeriod
		if err := json.Unmarshal(periodData, &quotaPeriods); err != nil {
			return false, fmt.Errorf("unmarshal Tool call reservation quota periods: %w", err)
		}
		quotas, err := lockCostQuotas(ctx, tx, namespace)
		if err != nil {
			return false, err
		}
		for index := range quotas {
			if !reservationChargedQuotaPeriod(quotaPeriods, &quotas[index]) {
				continue
			}
			if err := adjustQuotaToolCalls(&quotas[index].quota, -1); err != nil {
				return false, fmt.Errorf("refund Tool call reservation: %w", err)
			}
		}
		if err := persistLockedTokenQuotas(ctx, tx, quotas); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tool_call_reservations WHERE namespace = $1 AND invocation_id = $2`, namespace, invocationID); err != nil {
		return false, fmt.Errorf("delete Tool call reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit Tool call reservation finalization: %w", err)
	}
	return true, nil
}

// CreateTokenReservation atomically checks CognitiveQuota budgets, charges the
// reservation ceiling, and persists the reservation for later settlement.
func (s *Store) CreateTokenReservation(ctx context.Context, reservation store.TokenReservation) (bool, error) {
	return s.createTokenReservation(ctx, reservation, false)
}

// CreateCanaryReservation charges quota while marking the reservation as
// ineligible for generic expiry reclamation.
func (s *Store) CreateCanaryReservation(ctx context.Context, reservation store.TokenReservation) (bool, error) {
	return s.createTokenReservation(ctx, reservation, true)
}

func (s *Store) createTokenReservation(ctx context.Context, reservation store.TokenReservation, canary bool) (bool, error) {
	if reservation.ID == "" || reservation.Namespace == "" {
		return false, fmt.Errorf("token reservation ID and namespace are required")
	}
	if reservation.ReservedInput < 0 || reservation.ReservedOutput < 0 {
		return false, fmt.Errorf("token reservation ceilings must be non-negative")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin token reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, reservation.ID); err != nil {
		return false, fmt.Errorf("lock token reservation: %w", err)
	}
	var existing store.TokenReservation
	var existingCanary bool
	err = tx.QueryRow(ctx,
		`SELECT reservation_id, namespace, reserved_input, reserved_output, canary
		 FROM token_reservations WHERE reservation_id = $1`, reservation.ID,
	).Scan(&existing.ID, &existing.Namespace, &existing.ReservedInput, &existing.ReservedOutput, &existingCanary)
	if err == nil {
		if existing == reservation && existingCanary == canary {
			return false, nil
		}
		return false, store.ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("get token reservation: %w", err)
	}
	quotas, err := lockTokenQuotas(ctx, tx, reservation.Namespace)
	if err != nil {
		return false, err
	}
	for index := range quotas {
		if err := reserveQuotaTokens(&quotas[index].quota, reservation.ReservedInput, reservation.ReservedOutput); err != nil {
			return false, fmt.Errorf("%w: %w", store.ErrQuotaExceeded, err)
		}
	}
	quotaPeriods := make(map[string]quotaPeriod, len(quotas))
	for index := range quotas {
		quotaPeriods[quotas[index].name] = quotaPeriod{
			ResourceID: quotas[index].resourceID,
			ResetTime:  quotas[index].quota.Status.LastResetTime,
		}
	}
	periodData, err := json.Marshal(quotaPeriods)
	if err != nil {
		return false, fmt.Errorf("marshal token reservation quota periods: %w", err)
	}
	if err := persistLockedTokenQuotas(ctx, tx, quotas); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO token_reservations (reservation_id, namespace, reserved_input, reserved_output, quota_periods, canary)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		reservation.ID, reservation.Namespace, reservation.ReservedInput, reservation.ReservedOutput, periodData, canary,
	); err != nil {
		return false, fmt.Errorf("insert token reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit token reservation: %w", err)
	}
	return true, nil
}

// SettleCanaryReservation applies reported usage and permanently records the
// exact settlement. Changed retries conflict.
func (s *Store) SettleCanaryReservation(ctx context.Context, reservationID string, actualInput, actualOutput int64) (store.CanaryReservationDisposition, error) {
	return s.finalizeCanaryReservation(ctx, reservationID, store.CanaryReservationSettled, actualInput, actualOutput)
}

// ReleaseCanaryReservation refunds a pre-provider reservation and permanently
// records that no usage was charged.
func (s *Store) ReleaseCanaryReservation(ctx context.Context, reservationID string) (store.CanaryReservationDisposition, error) {
	return s.finalizeCanaryReservation(ctx, reservationID, store.CanaryReservationReleased, 0, 0)
}

func (s *Store) finalizeCanaryReservation(ctx context.Context, reservationID, state string, actualInput, actualOutput int64) (store.CanaryReservationDisposition, error) {
	want := store.CanaryReservationDisposition{ReservationID: reservationID, State: state, InputTokens: actualInput, OutputTokens: actualOutput}
	if reservationID == "" {
		return store.CanaryReservationDisposition{}, fmt.Errorf("canary reservation ID is required")
	}
	if actualInput < 0 || actualOutput < 0 {
		return store.CanaryReservationDisposition{}, fmt.Errorf("actual token usage must be non-negative")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("begin canary reservation finalization: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, reservationID); err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("lock canary reservation: %w", err)
	}
	var existing store.CanaryReservationDisposition
	err = tx.QueryRow(ctx,
		`SELECT reservation_id, state, input_tokens, output_tokens
		 FROM canary_reservation_dispositions WHERE reservation_id = $1`, reservationID,
	).Scan(&existing.ReservationID, &existing.State, &existing.InputTokens, &existing.OutputTokens)
	if err == nil {
		if existing != want {
			return store.CanaryReservationDisposition{}, store.ErrConflict
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.CanaryReservationDisposition{}, fmt.Errorf("get canary reservation disposition: %w", err)
	}
	var reservation store.TokenReservation
	var periodData []byte
	var canary bool
	err = tx.QueryRow(ctx,
		`SELECT reservation_id, namespace, reserved_input, reserved_output, quota_periods, canary
		 FROM token_reservations WHERE reservation_id = $1 FOR UPDATE`, reservationID,
	).Scan(&reservation.ID, &reservation.Namespace, &reservation.ReservedInput, &reservation.ReservedOutput, &periodData, &canary)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CanaryReservationDisposition{}, store.ErrNotFound
	}
	if err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("get canary reservation for finalization: %w", err)
	}
	if !canary {
		return store.CanaryReservationDisposition{}, store.ErrConflict
	}
	var quotaPeriods map[string]quotaPeriod
	if err := json.Unmarshal(periodData, &quotaPeriods); err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("unmarshal canary reservation quota periods: %w", err)
	}
	quotas, err := lockTokenQuotas(ctx, tx, reservation.Namespace)
	if err != nil {
		return store.CanaryReservationDisposition{}, err
	}
	actualTotal, err := checkedTokenDelta(actualInput, actualOutput)
	if err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("finalize canary token usage: %w", err)
	}
	reservedTotal, err := checkedTokenDelta(reservation.ReservedInput, reservation.ReservedOutput)
	if err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("finalize canary token usage: %w", err)
	}
	for index := range quotas {
		inputDelta, outputDelta, totalDelta := actualInput, actualOutput, actualTotal
		if reservationChargedQuotaPeriod(quotaPeriods, &quotas[index]) {
			inputDelta -= reservation.ReservedInput
			outputDelta -= reservation.ReservedOutput
			totalDelta -= reservedTotal
		}
		if err := adjustQuotaTokens(&quotas[index].quota, inputDelta, outputDelta, totalDelta); err != nil {
			return store.CanaryReservationDisposition{}, fmt.Errorf("finalize canary token usage: %w", err)
		}
	}
	if err := persistLockedTokenQuotas(ctx, tx, quotas); err != nil {
		return store.CanaryReservationDisposition{}, err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO canary_reservation_dispositions (reservation_id, state, input_tokens, output_tokens)
		 VALUES ($1, $2, $3, $4)`, reservationID, state, actualInput, actualOutput,
	); err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("insert canary reservation disposition: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_reservations WHERE reservation_id = $1`, reservationID); err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("delete canary reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.CanaryReservationDisposition{}, fmt.Errorf("commit canary reservation finalization: %w", err)
	}
	return want, nil
}

// SettleTokenReservation atomically adjusts quota usage to actual tokens and
// removes the durable reservation. A missing ID is an idempotent no-op.
func (s *Store) SettleTokenReservation(ctx context.Context, reservationID string, actualInput, actualOutput int64) (bool, error) {
	if reservationID == "" {
		return false, fmt.Errorf("token reservation ID is required")
	}
	if actualInput < 0 || actualOutput < 0 {
		return false, fmt.Errorf("actual token usage must be non-negative")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin token settlement: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, reservationID); err != nil {
		return false, fmt.Errorf("lock token reservation: %w", err)
	}
	var reservation store.TokenReservation
	var periodData []byte
	err = tx.QueryRow(ctx,
		`SELECT reservation_id, namespace, reserved_input, reserved_output, quota_periods
		 FROM token_reservations WHERE reservation_id = $1 FOR UPDATE`, reservationID,
	).Scan(&reservation.ID, &reservation.Namespace, &reservation.ReservedInput, &reservation.ReservedOutput, &periodData)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get token reservation for settlement: %w", err)
	}
	var quotaPeriods map[string]quotaPeriod
	if err := json.Unmarshal(periodData, &quotaPeriods); err != nil {
		return false, fmt.Errorf("unmarshal token reservation quota periods: %w", err)
	}
	quotas, err := lockTokenQuotas(ctx, tx, reservation.Namespace)
	if err != nil {
		return false, err
	}
	actualTotal, err := checkedTokenDelta(actualInput, actualOutput)
	if err != nil {
		return false, fmt.Errorf("settle token usage: %w", err)
	}
	reservedTotal, err := checkedTokenDelta(reservation.ReservedInput, reservation.ReservedOutput)
	if err != nil {
		return false, fmt.Errorf("settle token usage: %w", err)
	}
	for index := range quotas {
		inputDelta := actualInput
		outputDelta := actualOutput
		totalDelta := actualTotal
		if reservationChargedQuotaPeriod(quotaPeriods, &quotas[index]) {
			inputDelta -= reservation.ReservedInput
			outputDelta -= reservation.ReservedOutput
			totalDelta -= reservedTotal
		}
		if err := adjustQuotaTokens(
			&quotas[index].quota,
			inputDelta,
			outputDelta,
			totalDelta,
		); err != nil {
			return false, fmt.Errorf("settle token usage: %w", err)
		}
	}
	if err := persistLockedTokenQuotas(ctx, tx, quotas); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_reservations WHERE reservation_id = $1`, reservationID); err != nil {
		return false, fmt.Errorf("delete token reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit token settlement: %w", err)
	}
	return true, nil
}

// ReapExpiredTokenReservations refunds and removes a bounded set of abandoned
// reservations. Rows are revalidated under their reservation lock before use.
func (s *Store) ReapExpiredTokenReservations(ctx context.Context, expiredBefore time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT reservation_id FROM token_reservations
		 WHERE created_at < $1 AND NOT canary ORDER BY created_at LIMIT $2`, expiredBefore, limit,
	)
	if err != nil {
		return 0, fmt.Errorf("list expired token reservations: %w", err)
	}
	var reservationIDs []string
	for rows.Next() {
		var reservationID string
		if err := rows.Scan(&reservationID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired token reservation: %w", err)
		}
		reservationIDs = append(reservationIDs, reservationID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("list expired token reservations: %w", err)
	}
	rows.Close()

	reaped := 0
	for _, reservationID := range reservationIDs {
		found, err := s.reapExpiredTokenReservation(ctx, reservationID, expiredBefore)
		if err != nil {
			return reaped, err
		}
		if found {
			reaped++
		}
	}
	return reaped, nil
}

func (s *Store) reapExpiredTokenReservation(ctx context.Context, reservationID string, expiredBefore time.Time) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin expired token reservation reclaim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, reservationID); err != nil {
		return false, fmt.Errorf("lock expired token reservation: %w", err)
	}
	var reservation store.TokenReservation
	var periodData []byte
	err = tx.QueryRow(ctx,
		`SELECT reservation_id, namespace, reserved_input, reserved_output, quota_periods
		 FROM token_reservations WHERE reservation_id = $1 AND created_at < $2 FOR UPDATE`,
		reservationID, expiredBefore,
	).Scan(&reservation.ID, &reservation.Namespace, &reservation.ReservedInput, &reservation.ReservedOutput, &periodData)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get expired token reservation: %w", err)
	}
	var quotaPeriods map[string]quotaPeriod
	if err := json.Unmarshal(periodData, &quotaPeriods); err != nil {
		return false, fmt.Errorf("unmarshal expired token reservation quota periods: %w", err)
	}
	quotas, err := lockTokenQuotas(ctx, tx, reservation.Namespace)
	if err != nil {
		return false, err
	}
	reservedTotal, err := checkedTokenDelta(reservation.ReservedInput, reservation.ReservedOutput)
	if err != nil {
		return false, fmt.Errorf("reclaim token usage: %w", err)
	}
	for index := range quotas {
		if !reservationChargedQuotaPeriod(quotaPeriods, &quotas[index]) {
			continue
		}
		if err := adjustQuotaTokens(
			&quotas[index].quota,
			-reservation.ReservedInput,
			-reservation.ReservedOutput,
			-reservedTotal,
		); err != nil {
			return false, fmt.Errorf("reclaim token usage: %w", err)
		}
	}
	if err := persistLockedTokenQuotas(ctx, tx, quotas); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_reservations WHERE reservation_id = $1`, reservationID); err != nil {
		return false, fmt.Errorf("delete expired token reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit expired token reservation reclaim: %w", err)
	}
	return true, nil
}

func reservationChargedQuotaPeriod(periods map[string]quotaPeriod, quota *lockedTokenQuota) bool {
	period, ok := periods[quota.name]
	return ok && period.ResourceID == quota.resourceID && period.ResetTime == quota.quota.Status.LastResetTime
}

type lockedTokenQuota struct {
	name            string
	resourceID      string
	resourceVersion int64
	quota           v1alpha1.CognitiveQuota
}

type quotaPeriod struct {
	ResourceID string `json:"resourceId"`
	ResetTime  string `json:"resetTime"`
}

func reserveQuotaToolCall(quota *v1alpha1.CognitiveQuota) error {
	if quota.Status.CostUsage == nil {
		quota.Status.CostUsage = &v1alpha1.QuotaCostUsage{}
	}
	requested, err := checkedTokenDelta(quota.Status.CostUsage.ToolCalls, 1)
	if err != nil {
		return err
	}
	if limit := quota.Spec.CostBudget.MaxToolCallsPerPeriod; limit > 0 && requested > limit {
		return fmt.Errorf("Tool call limit exceeded")
	}
	quota.Status.CostUsage.ToolCalls = requested
	return nil
}

func adjustQuotaToolCalls(quota *v1alpha1.CognitiveQuota, delta int64) error {
	if quota.Status.CostUsage == nil {
		quota.Status.CostUsage = &v1alpha1.QuotaCostUsage{}
	}
	toolCalls, err := checkedTokenDelta(quota.Status.CostUsage.ToolCalls, delta)
	if err != nil {
		return err
	}
	quota.Status.CostUsage.ToolCalls = toolCalls
	return nil
}

func lockTokenQuotas(ctx context.Context, tx pgx.Tx, namespace string) ([]lockedTokenQuota, error) {
	rows, err := tx.Query(ctx,
		`SELECT name, id::text, data, resource_version FROM resources
		 WHERE kind = $1 AND namespace = $2 AND deleted_at IS NULL
		 ORDER BY name FOR UPDATE`, cognitiveQuotaKind, namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("lock CognitiveQuotas: %w", err)
	}
	defer rows.Close()
	var quotas []lockedTokenQuota
	for rows.Next() {
		var item lockedTokenQuota
		var data []byte
		if err := rows.Scan(&item.name, &item.resourceID, &data, &item.resourceVersion); err != nil {
			return nil, fmt.Errorf("scan CognitiveQuota: %w", err)
		}
		if err := json.Unmarshal(data, &item.quota); err != nil {
			return nil, fmt.Errorf("unmarshal CognitiveQuota %q: %w", item.name, err)
		}
		if item.quota.Spec.TokenBudget != nil {
			quotas = append(quotas, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list locked CognitiveQuotas: %w", err)
	}
	return quotas, nil
}

func persistLockedTokenQuotas(ctx context.Context, tx pgx.Tx, quotas []lockedTokenQuota) error {
	for index := range quotas {
		quotas[index].quota.ObjectMeta.ResourceVersion = quotas[index].resourceVersion + 1
		data, err := json.Marshal(quotas[index].quota)
		if err != nil {
			return fmt.Errorf("marshal CognitiveQuota %q: %w", quotas[index].name, err)
		}
		command, err := tx.Exec(ctx,
			`UPDATE resources SET data = $1, resource_version = resource_version + 1, updated_at = NOW()
			 WHERE kind = $2 AND namespace = $3 AND name = $4 AND resource_version = $5 AND deleted_at IS NULL`,
			data, cognitiveQuotaKind, quotas[index].quota.ObjectMeta.Namespace, quotas[index].name, quotas[index].resourceVersion,
		)
		if err != nil {
			return fmt.Errorf("update CognitiveQuota %q: %w", quotas[index].name, err)
		}
		if command.RowsAffected() != 1 {
			return store.ErrConflict
		}
	}
	return nil
}

func reserveQuotaTokens(quota *v1alpha1.CognitiveQuota, input, output int64) error {
	if quota.Status.TokenUsage == nil {
		quota.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{}
	}
	usage := quota.Status.TokenUsage
	budget := quota.Spec.TokenBudget
	requestedInput, err := checkedTokenDelta(usage.InputTokens, input)
	if err != nil {
		return err
	}
	requestedOutput, err := checkedTokenDelta(usage.OutputTokens, output)
	if err != nil {
		return err
	}
	reservationTotal, err := checkedTokenDelta(input, output)
	if err != nil {
		return err
	}
	requestedTotal, err := checkedTokenDelta(usage.TotalTokens, reservationTotal)
	if err != nil {
		return err
	}
	if budget.MaxInputTokens > 0 && requestedInput > budget.MaxInputTokens {
		return fmt.Errorf("input token limit exceeded")
	}
	if budget.MaxOutputTokens > 0 && requestedOutput > budget.MaxOutputTokens {
		return fmt.Errorf("output token limit exceeded")
	}
	if budget.MaxTotalTokens > 0 && requestedTotal > budget.MaxTotalTokens {
		return fmt.Errorf("total token limit exceeded")
	}
	usage.InputTokens = requestedInput
	usage.OutputTokens = requestedOutput
	usage.TotalTokens = requestedTotal
	return nil
}

func adjustQuotaTokens(quota *v1alpha1.CognitiveQuota, inputDelta, outputDelta, totalDelta int64) error {
	if quota.Status.TokenUsage == nil {
		quota.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{}
	}
	usage := quota.Status.TokenUsage
	var err error
	usage.InputTokens, err = checkedTokenDelta(usage.InputTokens, inputDelta)
	if err != nil {
		return err
	}
	usage.OutputTokens, err = checkedTokenDelta(usage.OutputTokens, outputDelta)
	if err != nil {
		return err
	}
	usage.TotalTokens, err = checkedTokenDelta(usage.TotalTokens, totalDelta)
	return err
}

func checkedTokenDelta(current, delta int64) (int64, error) {
	if delta > 0 && current > int64(^uint64(0)>>1)-delta {
		return 0, fmt.Errorf("token usage overflow")
	}
	result := current + delta
	if result < 0 {
		return 0, fmt.Errorf("token usage cannot be negative")
	}
	return result, nil
}

// Get retrieves a resource by kind, namespace, and name.
// The returned JSON has metadata.resourceVersion injected from the DB column.
func (s *Store) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	var data []byte
	var resourceVersion int64
	var uid string
	err := s.pool.QueryRow(ctx,
		`SELECT data, resource_version, id::text FROM resources WHERE kind = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL`,
		kind, namespace, name,
	).Scan(&data, &resourceVersion, &uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get %s/%s/%s: %w", kind, namespace, name, err)
	}
	data = injectResourceIdentity(data, uid, resourceVersion)
	return data, nil
}

// injectResourceVersion sets metadata.resourceVersion in a JSON-encoded resource.
func injectResourceVersion(data []byte, rv int64) []byte {
	return injectResourceIdentity(data, "", rv)
}

func injectResourceIdentity(data []byte, uid string, rv int64) []byte {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["resourceVersion"] = rv
	if uid != "" {
		meta["uid"] = uid
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return out
}

// List returns all resources matching the query.
func (s *Store) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	query := `SELECT data, resource_version, id::text FROM resources WHERE kind = $1 AND deleted_at IS NULL`
	args := []any{kind}

	if opts.Namespace != "" {
		query += fmt.Sprintf(` AND namespace = $%d`, len(args)+1)
		args = append(args, opts.Namespace)
	}
	query += ` ORDER BY created_at`

	if opts.Limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d`, len(args)+1)
		args = append(args, opts.Limit+1) // fetch one extra to detect more pages
	}
	if opts.Continue != "" {
		offset, err := decodeContinue(opts.Continue)
		if err == nil && offset > 0 {
			query += fmt.Sprintf(` OFFSET $%d`, len(args)+1)
			args = append(args, offset)
		}
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	defer rows.Close()

	var results [][]byte
	for rows.Next() {
		var data []byte
		var rv int64
		var uid string
		if err := rows.Scan(&data, &rv, &uid); err != nil {
			return nil, fmt.Errorf("scan %s: %w", kind, err)
		}
		results = append(results, injectResourceIdentity(data, uid, rv))
	}
	return results, rows.Err()
}

// decodeContinue decodes a base64-encoded pagination offset.
func decodeContinue(token string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(b))
}

// EncodeContinue encodes an offset into a base64 pagination token.
func EncodeContinue(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// Create persists a new resource.
func (s *Store) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	_, err := s.CreateAndGet(ctx, kind, namespace, name, data)
	return err
}

// CreateAndGet persists a resource using the PostgreSQL row UUID as its
// standalone incarnation and returns exactly the representation stored.
func (s *Store) CreateAndGet(ctx context.Context, kind, namespace, name string, data []byte) ([]byte, error) {
	uid := uuid.NewString()
	stored := injectResourceIdentity(data, uid, 1)
	_, err := s.pool.Exec(ctx,
		`INSERT INTO resources (id, kind, namespace, name, data, resource_version) VALUES ($1, $2, $3, $4, $5, 1)`,
		uid, kind, namespace, name, stored,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
			return nil, store.ErrAlreadyExists
		}
		return nil, fmt.Errorf("create %s/%s/%s: %w", kind, namespace, name, err)
	}

	s.notifyLocal(store.WatchEvent{
		Type:            store.EventCreated,
		Kind:            kind,
		Namespace:       namespace,
		Name:            name,
		ResourceVersion: 1,
		Object:          stored,
	})
	return stored, nil
}

// CreateControlPlaneRoleBinding locks the referenced role until the binding is
// committed, preventing a concurrent role deletion from passing its reference check.
func (s *Store) CreateControlPlaneRoleBinding(ctx context.Context, namespace, name, roleName string, data []byte) ([]byte, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin ControlPlaneRoleBinding create: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockControlPlaneRole(ctx, tx, namespace, roleName); err != nil {
		return nil, err
	}
	uid := uuid.NewString()
	stored := injectResourceIdentity(data, uid, 1)
	if _, err := tx.Exec(ctx,
		`INSERT INTO resources (id, kind, namespace, name, data, resource_version) VALUES ($1, 'ControlPlaneRoleBinding', $2, $3, $4, 1)`,
		uid, namespace, name, stored,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, store.ErrAlreadyExists
		}
		return nil, fmt.Errorf("create ControlPlaneRoleBinding/%s/%s: %w", namespace, name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit ControlPlaneRoleBinding create: %w", err)
	}
	s.notifyLocal(store.WatchEvent{Type: store.EventCreated, Kind: "ControlPlaneRoleBinding", Namespace: namespace, Name: name, ResourceVersion: 1, Object: stored})
	return stored, nil
}

func lockControlPlaneRole(ctx context.Context, tx pgx.Tx, namespace, name string) error {
	var exists bool
	err := tx.QueryRow(ctx,
		`SELECT true FROM resources
		 WHERE kind = 'ControlPlaneRole' AND namespace = $1 AND name = $2 AND deleted_at IS NULL
		 FOR KEY SHARE`,
		namespace, name,
	).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock ControlPlaneRole/%s/%s: %w", namespace, name, err)
	}
	return nil
}

// Update modifies an existing resource with optimistic concurrency.
func (s *Store) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	_, err := s.UpdateAndGet(ctx, kind, namespace, name, data, resourceVersion)
	return err
}

// UpdateAndGet modifies a resource and returns the representation written by
// the same SQL statement, avoiding a race with a subsequent reader.
func (s *Store) UpdateAndGet(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) ([]byte, error) {
	var newVersion int64
	var updatedData []byte
	var uid string
	err := s.pool.QueryRow(ctx,
		`UPDATE resources
		 SET data = jsonb_set(jsonb_set($4::jsonb, '{metadata,uid}', to_jsonb(id::text), true), '{metadata,resourceVersion}', to_jsonb(resource_version + 1), true),
		     resource_version = resource_version + 1, updated_at = NOW()
		 WHERE kind = $1 AND namespace = $2 AND name = $3
		   AND resource_version = $5 AND deleted_at IS NULL
		 RETURNING resource_version, data, id::text`,
		kind, namespace, name, data, resourceVersion,
	).Scan(&newVersion, &updatedData, &uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish between not found and version conflict.
			exists, _ := s.exists(ctx, kind, namespace, name)
			if exists {
				return nil, store.ErrConflict
			}
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("update %s/%s/%s: %w", kind, namespace, name, err)
	}
	updatedData = injectResourceIdentity(updatedData, uid, newVersion)

	s.notifyLocal(store.WatchEvent{
		Type:            store.EventUpdated,
		Kind:            kind,
		Namespace:       namespace,
		Name:            name,
		ResourceVersion: newVersion,
		Object:          updatedData,
	})
	return updatedData, nil
}

// UpdateControlPlaneRoleBinding locks the target role through the binding update.
func (s *Store) UpdateControlPlaneRoleBinding(ctx context.Context, namespace, name, roleName string, data []byte, resourceVersion int64) ([]byte, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin ControlPlaneRoleBinding update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockControlPlaneRole(ctx, tx, namespace, roleName); err != nil {
		return nil, err
	}
	var updatedData []byte
	var newVersion int64
	var uid string
	err = tx.QueryRow(ctx,
		`UPDATE resources
		 SET data = jsonb_set(jsonb_set($4::jsonb, '{metadata,uid}', to_jsonb(id::text), true), '{metadata,resourceVersion}', to_jsonb(resource_version + 1), true),
		     resource_version = resource_version + 1, updated_at = NOW()
		 WHERE kind = 'ControlPlaneRoleBinding' AND namespace = $1 AND name = $2
		   AND resource_version = $3 AND deleted_at IS NULL
		 RETURNING data, resource_version, id::text`,
		namespace, name, resourceVersion, data,
	).Scan(&updatedData, &newVersion, &uid)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		lookupErr := tx.QueryRow(ctx,
			`SELECT true FROM resources WHERE kind = 'ControlPlaneRoleBinding' AND namespace = $1 AND name = $2 AND deleted_at IS NULL`,
			namespace, name,
		).Scan(&exists)
		if lookupErr == nil {
			return nil, store.ErrConflict
		}
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("lookup ControlPlaneRoleBinding/%s/%s: %w", namespace, name, lookupErr)
	}
	if err != nil {
		return nil, fmt.Errorf("update ControlPlaneRoleBinding/%s/%s: %w", namespace, name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit ControlPlaneRoleBinding update: %w", err)
	}
	updatedData = injectResourceIdentity(updatedData, uid, newVersion)
	s.notifyLocal(store.WatchEvent{Type: store.EventUpdated, Kind: "ControlPlaneRoleBinding", Namespace: namespace, Name: name, ResourceVersion: newVersion, Object: updatedData})
	return updatedData, nil
}

// Delete soft-deletes a resource.
func (s *Store) Delete(ctx context.Context, kind, namespace, name string) error {
	var data []byte
	var resourceVersion int64
	var uid string
	err := s.pool.QueryRow(ctx,
		`UPDATE resources SET deleted_at = NOW()
		 WHERE kind = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL
		 RETURNING data, resource_version, id::text`,
		kind, namespace, name,
	).Scan(&data, &resourceVersion, &uid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return fmt.Errorf("delete %s/%s/%s: %w", kind, namespace, name, err)
	}
	data = injectResourceIdentity(data, uid, resourceVersion)

	s.notifyLocal(store.WatchEvent{
		Type:            store.EventDeleted,
		Kind:            kind,
		Namespace:       namespace,
		Name:            name,
		ResourceVersion: resourceVersion,
		Object:          data,
	})
	return nil
}

// DeleteControlPlaneRole compares the expected version and reference set while
// holding a lock that binding mutations must also acquire.
func (s *Store) DeleteControlPlaneRole(ctx context.Context, namespace, name string, resourceVersion int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin ControlPlaneRole delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var data []byte
	var currentVersion int64
	var uid string
	err = tx.QueryRow(ctx,
		`SELECT data, resource_version, id::text FROM resources
		 WHERE kind = 'ControlPlaneRole' AND namespace = $1 AND name = $2 AND deleted_at IS NULL
		 FOR UPDATE`,
		namespace, name,
	).Scan(&data, &currentVersion, &uid)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock ControlPlaneRole/%s/%s for delete: %w", namespace, name, err)
	}
	if currentVersion != resourceVersion {
		return store.ErrConflict
	}
	var referenced bool
	err = tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM resources
			 WHERE kind = 'ControlPlaneRoleBinding' AND namespace = $1 AND deleted_at IS NULL
			   AND data #>> '{spec,roleRef,name}' = $2
		)`,
		namespace, name,
	).Scan(&referenced)
	if err != nil {
		return fmt.Errorf("check ControlPlaneRole/%s/%s references: %w", namespace, name, err)
	}
	if referenced {
		return store.ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`UPDATE resources SET deleted_at = NOW()
		 WHERE kind = 'ControlPlaneRole' AND namespace = $1 AND name = $2 AND resource_version = $3 AND deleted_at IS NULL`,
		namespace, name, resourceVersion,
	); err != nil {
		return fmt.Errorf("delete ControlPlaneRole/%s/%s: %w", namespace, name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit ControlPlaneRole delete: %w", err)
	}
	data = injectResourceIdentity(data, uid, currentVersion)
	s.notifyLocal(store.WatchEvent{Type: store.EventDeleted, Kind: "ControlPlaneRole", Namespace: namespace, Name: name, ResourceVersion: currentVersion, Object: data})
	return nil
}

// DeleteControlPlaneRoleBinding deletes only the version observed by the caller.
func (s *Store) DeleteControlPlaneRoleBinding(ctx context.Context, namespace, name string, resourceVersion int64) error {
	var data []byte
	var uid string
	err := s.pool.QueryRow(ctx,
		`UPDATE resources SET deleted_at = NOW()
		 WHERE kind = 'ControlPlaneRoleBinding' AND namespace = $1 AND name = $2
		   AND resource_version = $3 AND deleted_at IS NULL
		 RETURNING data, id::text`,
		namespace, name, resourceVersion,
	).Scan(&data, &uid)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		lookupErr := s.pool.QueryRow(ctx,
			`SELECT true FROM resources WHERE kind = 'ControlPlaneRoleBinding' AND namespace = $1 AND name = $2 AND deleted_at IS NULL`,
			namespace, name,
		).Scan(&exists)
		if lookupErr == nil {
			return store.ErrConflict
		}
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		return fmt.Errorf("lookup ControlPlaneRoleBinding/%s/%s: %w", namespace, name, lookupErr)
	}
	if err != nil {
		return fmt.Errorf("delete ControlPlaneRoleBinding/%s/%s: %w", namespace, name, err)
	}
	data = injectResourceIdentity(data, uid, resourceVersion)
	s.notifyLocal(store.WatchEvent{Type: store.EventDeleted, Kind: "ControlPlaneRoleBinding", Namespace: namespace, Name: name, ResourceVersion: resourceVersion, Object: data})
	return nil
}

// Watch returns a channel of events for a resource kind.
func (s *Store) Watch(ctx context.Context, kind string, opts store.ListOptions) (<-chan store.WatchEvent, error) {
	ch := make(chan store.WatchEvent, 64)

	s.mu.Lock()
	s.subscribers[kind] = append(s.subscribers[kind], watchSubscriber{
		ch:        ch,
		namespace: opts.Namespace,
	})
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.removeSub(kind, ch)
		// Do not close(ch) — there is a race between notify() reading the
		// subscribers slice and this goroutine closing the channel, which
		// would panic on send-to-closed-channel.  The channel will be GC'd
		// once no references remain.
	}()

	return ch, nil
}

// Backoff bounds for the pg_notify listener's reconnection loop.
const (
	notifyInitialBackoff = 250 * time.Millisecond
	notifyMaxBackoff     = 30 * time.Second
)

// StartNotifyListener subscribes to pg_notify('resource_changes') and dispatches
// events to watch subscribers. It establishes the initial LISTEN synchronously
// (so startup errors surface to the caller) and then services notifications in a
// background goroutine that automatically reconnects with exponential backoff if
// the underlying connection drops. Call once at startup.
//
// While the listener is active it becomes the single source of watch delivery:
// write-path notifications are suppressed (see notifyLocal) so that every process
// — including this one and any peer replica — receives exactly one copy of each
// event via the shared database channel.
func (s *Store) StartNotifyListener(ctx context.Context) error {
	if !s.listening.CompareAndSwap(false, true) {
		return nil
	}
	listenerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.listenerMu.Lock()
	s.listenerCancel = cancel
	s.listenerDone = done
	s.listenerMu.Unlock()
	conn, err := s.acquireAndListen(listenerCtx)
	if err != nil {
		cancel()
		s.listening.Store(false)
		close(done)
		s.clearNotifyListener(done)
		return err
	}
	go func() {
		s.runListener(listenerCtx, conn)
		close(done)
		s.clearNotifyListener(done)
	}()
	return nil
}

func (s *Store) clearNotifyListener(done chan struct{}) {
	s.listenerMu.Lock()
	if s.listenerDone == done {
		s.listenerCancel = nil
		s.listenerDone = nil
	}
	s.listenerMu.Unlock()
}

// StopNotifyListener cancels the pg_notify listener and waits for it to release
// its pooled connection. Call this before closing the pool.
func (s *Store) StopNotifyListener(ctx context.Context) error {
	s.listenerMu.Lock()
	cancel := s.listenerCancel
	done := s.listenerDone
	s.listenerMu.Unlock()
	if cancel == nil || done == nil {
		return nil
	}

	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop pg notify listener: %w", ctx.Err())
	}
}

// acquireAndListen checks out a pooled connection and issues LISTEN on it.
func (s *Store) acquireAndListen(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire conn for LISTEN: %w", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN resource_changes"); err != nil {
		conn.Release()
		return nil, fmt.Errorf("LISTEN: %w", err)
	}
	return conn, nil
}

// runListener services notifications on conn, reconnecting with exponential
// backoff (with jitter) whenever the connection drops, until ctx is cancelled.
func (s *Store) runListener(ctx context.Context, conn *pgxpool.Conn) {
	defer s.listening.Store(false)
	backoff := notifyInitialBackoff
	for {
		err := s.consumeNotifications(ctx, conn) // releases conn before returning
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("pg notify connection lost; reconnecting", zap.Error(err))

		// Reconnect loop with exponential backoff until success or shutdown.
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(withJitter(backoff)):
			}
			newConn, lerr := s.acquireAndListen(ctx)
			if lerr != nil {
				s.logger.Warn("pg notify reconnect failed; retrying",
					zap.Error(lerr), zap.Duration("backoff", backoff))
				backoff = nextBackoff(backoff)
				continue
			}
			conn = newConn
			backoff = notifyInitialBackoff
			s.logger.Info("pg notify reconnected")
			break
		}
	}
}

// consumeNotifications blocks reading notifications from conn until an error
// occurs or ctx is cancelled. It always releases conn before returning, first
// clearing the LISTEN registration so a pooled connection is not reused while
// still subscribed (which would leak buffered notifications to later callers).
func (s *Store) consumeNotifications(ctx context.Context, conn *pgxpool.Conn) error {
	defer func() {
		// Best-effort UNLISTEN on a fresh context; if the connection already
		// broke this is a no-op and the pool discards it on Release.
		unlistenCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = conn.Exec(unlistenCtx, "UNLISTEN resource_changes")
		cancel()
		conn.Release()
	}()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		s.handleNotification(n.Payload)
	}
}

// nextBackoff doubles d, capped at notifyMaxBackoff.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > notifyMaxBackoff {
		return notifyMaxBackoff
	}
	return d
}

// withJitter returns a duration uniformly in [d/2, d] to de-synchronize
// reconnection attempts across replicas ("full jitter").
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}

// handleNotification parses a pg_notify payload and dispatches to subscribers.
func (s *Store) handleNotification(payload string) {
	var evt struct {
		Operation       string `json:"operation"`
		Kind            string `json:"kind"`
		Namespace       string `json:"namespace"`
		Name            string `json:"name"`
		ResourceVersion int64  `json:"resource_version"`
	}
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		s.logger.Warn("failed to parse notification", zap.Error(err))
		return
	}

	// Fetch current data for the event (except DELETE — data already captured in trigger).
	var eventType store.EventType
	switch evt.Operation {
	case "INSERT":
		eventType = store.EventCreated
	case "UPDATE":
		eventType = store.EventUpdated
	case "DELETE":
		eventType = store.EventDeleted
	default:
		return
	}

	s.logger.Debug("pg notification",
		zap.String("op", evt.Operation),
		zap.String("kind", evt.Kind),
		zap.String("ns", evt.Namespace),
		zap.String("name", evt.Name),
	)

	// Fetch the object payload so watchers receive the full resource. For
	// DELETE the row is soft-deleted (s.Get filters it out), so read the
	// last-known state including the tombstone.
	var data []byte
	if eventType == store.EventDeleted {
		data = s.getIncludingDeleted(context.Background(), evt.Kind, evt.Namespace, evt.Name)
	} else {
		data, _ = s.Get(context.Background(), evt.Kind, evt.Namespace, evt.Name)
	}
	s.notify(store.WatchEvent{
		Type:            eventType,
		Kind:            evt.Kind,
		Namespace:       evt.Namespace,
		Name:            evt.Name,
		ResourceVersion: evt.ResourceVersion,
		Object:          data,
	})
}

// notifyLocal delivers an event produced by a write in THIS process. When the
// pg_notify listener is active it returns without dispatching: delivery is
// handled uniformly via the LISTEN path so this process and every peer replica
// receive exactly one copy (avoiding double-delivery to local subscribers).
// Without an active listener (unit tests, or a single process with no database
// trigger) it dispatches directly.
func (s *Store) notifyLocal(evt store.WatchEvent) {
	if s.listening.Load() {
		return
	}
	s.notify(evt)
}

// getIncludingDeleted returns the most recent row for kind/namespace/name,
// including a soft-deleted tombstone, with resourceVersion injected. Used to
// populate the object payload on DELETE watch events. Returns nil if absent.
func (s *Store) getIncludingDeleted(ctx context.Context, kind, namespace, name string) []byte {
	var data []byte
	var rv int64
	var uid string
	err := s.pool.QueryRow(ctx,
		`SELECT data, resource_version, id::text FROM resources
		 WHERE kind = $1 AND namespace = $2 AND name = $3
		 ORDER BY deleted_at DESC NULLS LAST LIMIT 1`,
		kind, namespace, name,
	).Scan(&data, &rv, &uid)
	if err != nil {
		return nil
	}
	return injectResourceIdentity(data, uid, rv)
}

// notify dispatches a watch event to every subscriber for that kind.
//
// A subscriber that originated the write (e.g. a controller reconciling in the
// same process) receives its own event back, producing one redundant reconcile
// enqueue. This is intentional and safe: the controller WorkQueue coalesces
// duplicates. Filtering by event source would trim that work under very high
// write rates but is deliberately omitted to keep delivery source-agnostic.
func (s *Store) notify(evt store.WatchEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	subscribers := s.subscribers[evt.Kind]
	active := subscribers[:0]
	for _, subscriber := range subscribers {
		if subscriber.namespace != "" && subscriber.namespace != evt.Namespace {
			active = append(active, subscriber)
			continue
		}
		select {
		case subscriber.ch <- evt:
			active = append(active, subscriber)
		default:
			for len(subscriber.ch) > 0 {
				<-subscriber.ch
			}
			subscriber.ch <- store.WatchEvent{Type: store.EventResyncRequired, Kind: evt.Kind}
			close(subscriber.ch)
			s.logger.Warn("watch subscriber fell behind; resync required",
				zap.String("kind", evt.Kind),
				zap.String("name", evt.Name),
			)
		}
	}
	s.subscribers[evt.Kind] = active
}

// removeSub removes a subscriber channel.
func (s *Store) removeSub(kind string, target chan store.WatchEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.subscribers[kind]
	for i, subscriber := range subs {
		if subscriber.ch == target {
			s.subscribers[kind] = append(subs[:i], subs[i+1:]...)
			return
		}
	}
}

// exists checks if a resource exists (including soft-deleted for conflict detection).
func (s *Store) exists(ctx context.Context, kind, namespace, name string) (bool, error) {
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM resources WHERE kind = $1 AND namespace = $2 AND name = $3 AND deleted_at IS NULL`,
		kind, namespace, name,
	).Scan(&count)
	return count > 0, err
}

// CreateRevision stores an immutable revision snapshot for a resource.
func (s *Store) CreateRevision(ctx context.Context, kind, namespace, name string, revision int64, spec, data []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO resource_revisions (kind, namespace, name, revision, spec, data)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		kind, namespace, name, revision, spec, data,
	)
	return err
}

// ListRevisions returns all revisions for a resource, ordered by revision number.
func (s *Store) ListRevisions(ctx context.Context, kind, namespace, name string) ([]store.Revision, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT revision, spec, data, created_at, created_by
		 FROM resource_revisions
		 WHERE kind = $1 AND namespace = $2 AND name = $3
		 ORDER BY revision ASC`,
		kind, namespace, name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var revisions []store.Revision
	for rows.Next() {
		var rev store.Revision
		var createdAt time.Time
		if err := rows.Scan(&rev.Revision, &rev.Spec, &rev.Data, &createdAt, &rev.CreatedBy); err != nil {
			return nil, err
		}
		rev.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		revisions = append(revisions, rev)
	}
	return revisions, rows.Err()
}

// GetRevision retrieves a specific revision of a resource.
func (s *Store) GetRevision(ctx context.Context, kind, namespace, name string, revision int64) (*store.Revision, error) {
	var rev store.Revision
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT revision, spec, data, created_at, created_by
		 FROM resource_revisions
		 WHERE kind = $1 AND namespace = $2 AND name = $3 AND revision = $4`,
		kind, namespace, name, revision,
	).Scan(&rev.Revision, &rev.Spec, &rev.Data, &createdAt, &rev.CreatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	rev.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	return &rev, nil
}

// AppendEvent appends an execution event for a resource instance.
func (s *Store) AppendEvent(ctx context.Context, kind, namespace, name string, event store.InstanceEvent) error {
	details, err := json.Marshal(event.Details)
	if err != nil {
		return fmt.Errorf("marshal event details: %w", err)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO instance_events (kind, namespace, name, event_type, reason, message, details, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		kind, namespace, name, event.Type, event.Reason, event.Message, details, event.Timestamp,
	)
	return err
}

// ListEvents returns all events for a resource instance, ordered by timestamp ascending.
func (s *Store) ListEvents(ctx context.Context, kind, namespace, name string) ([]store.InstanceEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT event_type, reason, message, details, created_at
		 FROM instance_events
		 WHERE kind = $1 AND namespace = $2 AND name = $3
		 ORDER BY created_at ASC`,
		kind, namespace, name,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []store.InstanceEvent
	for rows.Next() {
		var evt store.InstanceEvent
		var details []byte
		var createdAt time.Time
		if err := rows.Scan(&evt.Type, &evt.Reason, &evt.Message, &details, &createdAt); err != nil {
			return nil, err
		}
		evt.Timestamp = createdAt.UTC().Format(time.RFC3339)
		if len(details) > 0 && string(details) != "null" {
			evt.Details = details
		}
		events = append(events, evt)
	}
	return events, rows.Err()
}
