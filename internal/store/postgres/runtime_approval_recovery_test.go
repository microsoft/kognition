// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	testRecoveryWorkload = "tool-gateway/team-a/stable-a"
	testRecoveryNonce    = "process-nonce-current-0001"
)

func newClaimedApproval(t *testing.T, ctx context.Context, st *Store, invocationID, workloadIdentity, processStartNonce string) api.RuntimeApproval {
	t.Helper()
	approval := newExecutionReadyApproval(t, ctx, st, invocationID, api.RuntimeApprovalDecisionNotRequired)
	token := mustGeneratePreparationToken(t)
	prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", invocationID, approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("prepare %q: %v", invocationID, err)
	}
	claimant := newClaimant(0)
	claimant.WorkloadUID = workloadIdentity
	claimant.ProcessStartNonce = processStartNonce
	claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", invocationID, prepared.ResourceVersion, token, claimant)
	if err != nil {
		t.Fatalf("claim %q: %v", invocationID, err)
	}
	return claimed
}

func TestAcquireRuntimeApprovalRecoveryLeaseRequiresExplicitIdentity(t *testing.T) {
	st := newTestStore(t)

	for _, tc := range []struct {
		name     string
		identity string
		nonce    string
	}{
		{name: "MissingIdentity", nonce: testRecoveryNonce},
		{name: "MissingNonce", identity: testRecoveryWorkload},
		{name: "ShortNonce", identity: testRecoveryWorkload, nonce: "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease, err := st.AcquireRuntimeApprovalRecoveryLease(context.Background(), tc.identity, tc.nonce)
			if err == nil || lease != nil {
				t.Fatalf("AcquireRuntimeApprovalRecoveryLease(%q, %q) = (%v, %v), want validation error", tc.identity, tc.nonce, lease, err)
			}
		})
	}
}

func TestRuntimeApprovalRecoveryLeaseContendsAndReleasesByIdentity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, testRecoveryNonce)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	defer first.Close(ctx)

	contender, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, "process-nonce-contender-0002")
	if !errors.Is(err, store.ErrConflict) || contender != nil {
		t.Fatalf("contending lease = (%v, %v), want nil, ErrConflict", contender, err)
	}

	independent, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, "memory-gateway/team-a/stable-b", testRecoveryNonce)
	if err != nil {
		t.Fatalf("acquire independent identity: %v", err)
	}
	if err := independent.Close(ctx); err != nil {
		t.Fatalf("close independent lease: %v", err)
	}

	if err := first.Close(ctx); err != nil {
		t.Fatalf("close first lease: %v", err)
	}
	select {
	case <-first.Done():
	default:
		t.Fatal("closed lease did not signal Done")
	}
	if first.Err() != nil {
		t.Fatalf("normally closed lease error = %v, want nil", first.Err())
	}
	if _, err := first.RecoverRuntimeApprovalClaim(ctx, "team-a", "after-close", 1, 1); !errors.Is(err, errRuntimeApprovalRecoveryLeaseClosed) {
		t.Fatalf("recovery after close error = %v, want closed lease", err)
	}

	replacement, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, "process-nonce-replacement-0003")
	if err != nil {
		t.Fatalf("acquire replacement after release: %v", err)
	}
	if err := replacement.Close(ctx); err != nil {
		t.Fatalf("close replacement lease: %v", err)
	}

	cancelled, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, "process-nonce-cancelled-0004")
	if err != nil {
		t.Fatalf("acquire lease for cancelled close: %v", err)
	}
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := cancelled.Close(cancelledCtx); err == nil {
		t.Fatal("close with cancelled context succeeded, want unlock error")
	}
	afterCancelled, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, "process-nonce-after-cancel-0005")
	if err != nil {
		t.Fatalf("acquire after cancelled close discarded session: %v", err)
	}
	if err := afterCancelled.Close(ctx); err != nil {
		t.Fatalf("close lease after cancelled release: %v", err)
	}
}

func TestRuntimeApprovalRecoveryCancelledCloseReleasesBeforeReturning(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	for attempt := 0; attempt < 32; attempt++ {
		lease, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, testRecoveryNonce)
		if err != nil {
			t.Fatalf("acquire attempt %d: %v", attempt, err)
		}
		if err := lease.Close(cancelledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled close attempt %d hid caller cancellation: %v", attempt, err)
		}
		if !errors.Is(lease.Err(), context.Canceled) {
			t.Fatalf("cancelled lease attempt %d lost its terminal error: %v", attempt, lease.Err())
		}
		select {
		case <-lease.Done():
		default:
			t.Fatalf("cancelled close attempt %d returned before signalling completion", attempt)
		}
		replacement, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, "process-nonce-replacement-0003")
		if err != nil {
			t.Fatalf("cancelled close attempt %d returned with the old identity still active: %v", attempt, err)
		}
		if err := replacement.Close(ctx); err != nil {
			t.Fatalf("close replacement attempt %d: %v", attempt, err)
		}
	}
}

func TestRuntimeApprovalRecoveryLeaseConcurrentContendersHaveOneWinner(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	start := make(chan struct{})
	type result struct {
		lease store.RuntimeApprovalRecoveryLease
		err   error
	}
	results := make(chan result, 2)
	for _, nonce := range []string{"process-nonce-concurrent-0001", "process-nonce-concurrent-0002"} {
		go func(processStartNonce string) {
			<-start
			lease, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, processStartNonce)
			results <- result{lease: lease, err: err}
		}(nonce)
	}
	close(start)

	var winner store.RuntimeApprovalRecoveryLease
	conflicts := 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			if winner != nil {
				t.Fatal("both concurrent contenders acquired the same identity")
			}
			winner = result.lease
		case errors.Is(result.err, store.ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent contender error = %v", result.err)
		}
	}
	if winner == nil || conflicts != 1 {
		t.Fatalf("concurrent results winner=%v conflicts=%d, want one each", winner, conflicts)
	}
	if err := winner.Close(ctx); err != nil {
		t.Fatalf("close concurrent winner: %v", err)
	}
}

func TestRuntimeApprovalRecoveryLeaseSignalsSessionLoss(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	leaseContract, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, testRecoveryNonce)
	if err != nil {
		t.Fatalf("acquire recovery lease: %v", err)
	}
	lease := leaseContract.(*runtimeApprovalRecoveryLease)
	backendPID := lease.conn.Conn().PgConn().PID()
	if _, err := testPool.Exec(ctx, `SELECT pg_terminate_backend($1)`, backendPID); err != nil {
		t.Fatalf("terminate recovery backend: %v", err)
	}
	select {
	case <-lease.Done():
	case <-time.After(2 * runtimeApprovalRecoveryHeartbeat):
		t.Fatal("lease did not signal session loss")
	}
	if lease.Err() == nil {
		t.Fatal("lost lease error = nil")
	}
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", "after-loss", 1, 1); !errors.Is(err, errRuntimeApprovalRecoveryLeaseClosed) {
		t.Fatalf("recovery after session loss error = %v, want closed lease", err)
	}
	replacement, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, "process-nonce-after-loss-0002")
	if err != nil {
		t.Fatalf("acquire replacement after session loss: %v", err)
	}
	if err := replacement.Close(ctx); err != nil {
		t.Fatalf("close replacement after session loss: %v", err)
	}
}

func TestRuntimeApprovalRecoveryLeaseRequiresDeadIncarnationProofAndCAS(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	lease, err := st.AcquireRuntimeApprovalRecoveryLease(ctx, testRecoveryWorkload, testRecoveryNonce)
	if err != nil {
		t.Fatalf("acquire recovery lease: %v", err)
	}
	defer lease.Close(ctx)

	sameNonce := newClaimedApproval(t, ctx, st, "recover-same-nonce", testRecoveryWorkload, testRecoveryNonce)
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", sameNonce.Spec.InvocationID, sameNonce.ResourceVersion, sameNonce.Status.FencingToken); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same-nonce recovery error = %v, want ErrConflict", err)
	}

	otherIdentity := newClaimedApproval(t, ctx, st, "recover-other-identity", "tool-gateway/team-a/stable-other", "process-nonce-dead-0001")
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", otherIdentity.Spec.InvocationID, otherIdentity.ResourceVersion, otherIdentity.Status.FencingToken); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("identity-mismatch recovery error = %v, want ErrConflict", err)
	}

	notClaimed := newExecutionReadyApproval(t, ctx, st, "recover-not-claimed", api.RuntimeApprovalDecisionNotRequired)
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", notClaimed.Spec.InvocationID, notClaimed.ResourceVersion, runtimeapproval.MinFencingToken); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("not-claimed recovery error = %v, want ErrConflict", err)
	}

	missingProof := newClaimedApproval(t, ctx, st, "recover-missing-proof", testRecoveryWorkload, "process-nonce-dead-0004")
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals
		SET claim_workload_uid = NULL,
		    claim_process_start_nonce = NULL,
		    claim_fencing_token = NULL,
		    claim_claimed_at = NULL,
		    claim_last_heartbeat = NULL,
		    claim_lease_deadline = NULL
		WHERE workspace = 'team-a' AND invocation_id = 'recover-missing-proof'`); err != nil {
		t.Fatalf("remove indexed claimant proof: %v", err)
	}
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", missingProof.Spec.InvocationID, missingProof.ResourceVersion, missingProof.Status.FencingToken); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("missing-proof recovery error = %v, want ErrConflict", err)
	}

	staleCAS := newClaimedApproval(t, ctx, st, "recover-stale-cas", testRecoveryWorkload, "process-nonce-dead-0002")
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", staleCAS.Spec.InvocationID, staleCAS.ResourceVersion+1, staleCAS.Status.FencingToken); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale resource-version recovery error = %v, want ErrConflict", err)
	}
	if _, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", staleCAS.Spec.InvocationID, staleCAS.ResourceVersion, staleCAS.Status.FencingToken+1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale fencing-token recovery error = %v, want ErrConflict", err)
	}

	recoverable := newClaimedApproval(t, ctx, st, "recover-different-nonce", testRecoveryWorkload, "process-nonce-dead-0003")
	recovered, err := lease.RecoverRuntimeApprovalClaim(ctx, "team-a", recoverable.Spec.InvocationID, recoverable.ResourceVersion, recoverable.Status.FencingToken)
	if err != nil {
		t.Fatalf("recover different nonce: %v", err)
	}
	if recovered.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain {
		t.Fatalf("recovered execution state = %q, want %q", recovered.Status.ExecutionState, api.RuntimeApprovalExecutionUncertain)
	}
	if recovered.Status.TerminationProofRef == "" {
		t.Fatal("recovered approval is missing termination proof")
	}
	if len(recovered.Status.TerminationProofRef) > maxTerminationProofRef {
		t.Fatalf("termination proof length = %d, want <= %d", len(recovered.Status.TerminationProofRef), maxTerminationProofRef)
	}
	for _, secret := range []string{testRecoveryWorkload, testRecoveryNonce, recoverable.Status.Claimant.ProcessStartNonce} {
		if strings.Contains(recovered.Status.TerminationProofRef, secret) {
			t.Fatalf("termination proof exposes identity material %q", secret)
		}
	}
	if recovered.ResourceVersion != recoverable.ResourceVersion+1 {
		t.Fatalf("recovered resource version = %d, want %d", recovered.ResourceVersion, recoverable.ResourceVersion+1)
	}
	if recovered.Status.ExecutionDisposition != "" || recovered.Status.FencingToken != runtimeapproval.MinFencingToken {
		t.Fatalf("recovered status changed unrelated claim fields: %#v", recovered.Status)
	}

	persisted, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", recoverable.Spec.InvocationID)
	if err != nil {
		t.Fatalf("read recovered approval: %v", err)
	}
	if persisted.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain || persisted.Status.TerminationProofRef != recovered.Status.TerminationProofRef {
		t.Fatalf("persisted recovery = %#v, want proof/state from result", persisted.Status)
	}
	var auditCount int
	var canonicalPayload []byte
	if err := testPool.QueryRow(ctx, `SELECT canonical_payload, count(*) OVER ()
		FROM cognitive_audit_log
		WHERE event_type = 'runtime_approval_claim_uncertain'
		  AND workspace = 'team-a'
		  AND subject_ref = $1
		ORDER BY id
		LIMIT 1`, recovered.Name).Scan(&canonicalPayload, &auditCount); err != nil {
		t.Fatalf("read recovery audit: %v", err)
	}
	if auditCount != 1 || len(canonicalPayload) == 0 {
		t.Fatalf("recovery audit count/payload = %d/%q, want one durable event", auditCount, canonicalPayload)
	}
	for _, secret := range []string{testRecoveryNonce, recoverable.Status.Claimant.ProcessStartNonce} {
		if strings.Contains(string(canonicalPayload), secret) {
			t.Fatalf("recovery audit exposes process nonce %q", secret)
		}
	}
}
