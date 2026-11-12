// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	runtimeApprovalRecoveryLockDomain = "kognition.runtime-approval.standalone-recovery"
	runtimeApprovalRecoveryHeartbeat  = 5 * time.Second
)

var errRuntimeApprovalRecoveryLeaseClosed = errors.New("RuntimeApproval recovery lease is closed")

type runtimeApprovalRecoveryLease struct {
	conn              *pgxpool.Conn
	store             *Store
	workloadIdentity  string
	processStartNonce string
	lockKey           string

	mu       sync.Mutex
	closed   bool
	err      error
	done     chan struct{}
	doneOnce sync.Once
}

func (s *Store) AcquireRuntimeApprovalRecoveryLease(ctx context.Context, workloadIdentity, processStartNonce string) (store.RuntimeApprovalRecoveryLease, error) {
	if err := validateRuntimeApprovalRecoveryIdentity(workloadIdentity, processStartNonce); err != nil {
		return nil, err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire RuntimeApproval recovery connection: %w", err)
	}
	lockKey := runtimeApprovalRecoveryLockDomain + "\x1f" + workloadIdentity
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, lockKey).Scan(&acquired); err != nil {
		closeRuntimeApprovalRecoveryConnection(conn)
		return nil, fmt.Errorf("acquire RuntimeApproval recovery lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, fmt.Errorf("%w: RuntimeApproval recovery identity %q is already active", store.ErrConflict, workloadIdentity)
	}
	lease := &runtimeApprovalRecoveryLease{
		conn:              conn,
		store:             s,
		workloadIdentity:  workloadIdentity,
		processStartNonce: processStartNonce,
		lockKey:           lockKey,
		done:              make(chan struct{}),
	}
	go lease.monitor()
	return lease, nil
}

func validateRuntimeApprovalRecoveryIdentity(workloadIdentity, processStartNonce string) error {
	if workloadIdentity != strings.TrimSpace(workloadIdentity) {
		return fmt.Errorf("RuntimeApproval recovery workload identity must not have surrounding whitespace")
	}
	if length := len(workloadIdentity); length < 1 || length > 253 {
		return fmt.Errorf("RuntimeApproval recovery workload identity length %d out of bounds [1,253]", length)
	}
	if processStartNonce != strings.TrimSpace(processStartNonce) {
		return fmt.Errorf("RuntimeApproval recovery process-start nonce must not have surrounding whitespace")
	}
	if length := len(processStartNonce); length < 16 || length > 128 {
		return fmt.Errorf("RuntimeApproval recovery process-start nonce length %d out of bounds [16,128]", length)
	}
	return nil
}

func (l *runtimeApprovalRecoveryLease) RecoverRuntimeApprovalClaim(ctx context.Context, workspace, invocationID string, expectedResourceVersion, fencingToken int64) (api.RuntimeApproval, error) {
	if expectedResourceVersion <= 0 {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval recovery requires expected resource version")
	}
	if fencingToken < runtimeapproval.MinFencingToken {
		return api.RuntimeApproval{}, fmt.Errorf("RuntimeApproval recovery requires positive fencing token, got %d", fencingToken)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return api.RuntimeApproval{}, errRuntimeApprovalRecoveryLeaseClosed
	}

	tx, err := l.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("begin RuntimeApproval recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	approval, found, err := scanRuntimeApproval(ctx, tx, workspace, invocationID, true)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if !found {
		return api.RuntimeApproval{}, store.ErrNotFound
	}
	if err := runtimeapproval.ValidateRetainedBindingKey(ctx, l.store.runtimeApprovalKeys(), approval.Spec.Binding); err != nil {
		return api.RuntimeApproval{}, err
	}
	if approval.ResourceVersion != expectedResourceVersion ||
		approval.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed ||
		approval.Status.FencingToken != fencingToken ||
		approval.Status.Claimant == nil ||
		approval.Status.Claimant.WorkloadUID != l.workloadIdentity ||
		approval.Status.Claimant.ProcessStartNonce == "" ||
		approval.Status.Claimant.ProcessStartNonce == l.processStartNonce {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	deadNonce := approval.Status.Claimant.ProcessStartNonce
	proofRef := runtimeApprovalRecoveryProof(l.workloadIdentity, deadNonce, l.processStartNonce)
	approval.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
	approval.Status.TerminationProofRef = proofRef
	approval.ResourceVersion++
	data, err := json.Marshal(approval)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("marshal RuntimeApproval recovery: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("read RuntimeApproval recovery time: %w", err)
	}
	command, err := tx.Exec(ctx, `UPDATE runtime_approvals
		SET execution_state = $3,
		    termination_proof_ref = $4,
		    data = $5,
		    resource_version = $6,
		    updated_at = $7
		WHERE workspace = $1
		  AND invocation_id = $2
		  AND resource_version = $8
		  AND execution_state = $9
		  AND claim_workload_uid = $10
		  AND claim_process_start_nonce = $11
		  AND claim_process_start_nonce <> $12
		  AND claim_fencing_token = $13`,
		workspace, invocationID,
		approval.Status.ExecutionState, proofRef, data, approval.ResourceVersion, now,
		expectedResourceVersion, api.RuntimeApprovalExecutionClaimed,
		l.workloadIdentity, deadNonce, l.processStartNonce, fencingToken)
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("update RuntimeApproval recovery: %w", err)
	}
	if command.RowsAffected() != 1 {
		return api.RuntimeApproval{}, store.ErrConflict
	}
	auditID, auditPayload, err := runtimeApprovalRecoveryAudit(approval, l.workloadIdentity, deadNonce, l.processStartNonce, now)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cognitive_audit_log
		(id, created_at, agent_identity, workspace, event_type, subject_kind, subject_ref, verb, details, canonical_payload)
		VALUES ($1, $2, $3, $4, 'runtime_approval_claim_uncertain', 'RuntimeApproval', $5, 'recover', $6, $7)`,
		auditID, now, l.workloadIdentity, workspace, approval.Name, auditPayload, auditPayload); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("append RuntimeApproval recovery audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("commit RuntimeApproval recovery: %w", err)
	}
	return approval, nil
}

func runtimeApprovalRecoveryProof(workloadIdentity, deadNonce, currentNonce string) string {
	digest := sha256.Sum256([]byte(runtimeApprovalRecoveryLockDomain + "\x00" + workloadIdentity + "\x00" + deadNonce + "\x00" + currentNonce))
	return "postgres-session-lock:sha256:" + hex.EncodeToString(digest[:])
}

func runtimeApprovalRecoveryAudit(approval api.RuntimeApproval, workloadIdentity, deadNonce, currentNonce string, recoveredAt time.Time) (string, []byte, error) {
	idDigest := sha256.Sum256([]byte(runtimeApprovalRecoveryLockDomain + "\x00audit\x00" + approval.UID + "\x00" + fmt.Sprint(approval.ResourceVersion)))
	payload, err := json.Marshal(struct {
		Workspace          string    `json:"workspace"`
		InvocationID       string    `json:"invocationID"`
		ApprovalUID        string    `json:"approvalUID"`
		WorkloadIdentity   string    `json:"workloadIdentity"`
		DeadNonceDigest    string    `json:"deadNonceDigest"`
		CurrentNonceDigest string    `json:"currentNonceDigest"`
		TerminationProof   string    `json:"terminationProof"`
		RecoveredAt        time.Time `json:"recoveredAt"`
	}{
		Workspace:          approval.Spec.Workspace,
		InvocationID:       approval.Spec.InvocationID,
		ApprovalUID:        approval.UID,
		WorkloadIdentity:   workloadIdentity,
		DeadNonceDigest:    runtimeApprovalRecoveryNonceDigest(deadNonce),
		CurrentNonceDigest: runtimeApprovalRecoveryNonceDigest(currentNonce),
		TerminationProof:   approval.Status.TerminationProofRef,
		RecoveredAt:        recoveredAt.UTC(),
	})
	if err != nil {
		return "", nil, fmt.Errorf("marshal RuntimeApproval recovery audit: %w", err)
	}
	return "runtime-approval-recovery-" + hex.EncodeToString(idDigest[:]), payload, nil
}

func runtimeApprovalRecoveryNonceDigest(nonce string) string {
	digest := sha256.Sum256([]byte(runtimeApprovalRecoveryLockDomain + "\x00nonce\x00" + nonce))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (l *runtimeApprovalRecoveryLease) Done() <-chan struct{} {
	return l.done
}

func (l *runtimeApprovalRecoveryLease) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *runtimeApprovalRecoveryLease) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	// Lock release must not depend on an asynchronous backend disconnect after cancellation.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeApprovalRecoveryHeartbeat)
	defer cancel()
	var unlocked bool
	err := l.conn.QueryRow(cleanupCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, l.lockKey).Scan(&unlocked)
	if err == nil && !unlocked {
		err = fmt.Errorf("RuntimeApproval recovery lock was not held")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		err = errors.Join(contextErr, err)
	}
	l.finishLocked(err, err != nil)
	return err
}

func (l *runtimeApprovalRecoveryLease) monitor() {
	ticker := time.NewTicker(runtimeApprovalRecoveryHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.mu.Lock()
			if l.closed {
				l.mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), runtimeApprovalRecoveryHeartbeat)
			err := l.conn.Ping(ctx)
			cancel()
			if err != nil {
				l.finishLocked(fmt.Errorf("RuntimeApproval recovery lock connection lost: %w", err), true)
			}
			l.mu.Unlock()
		}
	}
}

func (l *runtimeApprovalRecoveryLease) finishLocked(err error, discard bool) {
	l.closed = true
	l.err = err
	if discard {
		closeRuntimeApprovalRecoveryConnection(l.conn)
	} else {
		l.conn.Release()
	}
	l.doneOnce.Do(func() { close(l.done) })
}

func closeRuntimeApprovalRecoveryConnection(conn *pgxpool.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), runtimeApprovalRecoveryHeartbeat)
	defer cancel()
	_ = conn.Hijack().Close(closeCtx)
}

var _ store.RuntimeApprovalRecoveryStore = (*Store)(nil)
var _ store.RuntimeApprovalRecoveryLease = (*runtimeApprovalRecoveryLease)(nil)
