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
	"github.com/kognition-ai/kognition/internal/runtimeapproval/conformancetest"
	"github.com/kognition-ai/kognition/internal/store"
)

func newExecutionReadyApproval(t *testing.T, ctx context.Context, st *Store, invocationID, decision string) api.RuntimeApproval {
	t.Helper()
	fixture := runtimeApprovalFixture(t, invocationID, decision)
	created, approval, err := st.CreateOrGetRuntimeApproval(ctx, fixture, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created {
		t.Fatalf("create %q: created=%v err=%v", invocationID, created, err)
	}
	if decision == api.RuntimeApprovalDecisionPending {
		approval, err = st.DecideRuntimeApproval(ctx, "team-a", invocationID, store.RuntimeApprovalDecision{
			Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion,
			Issuer: "https://issuer.example", Subject: "operator-1",
		})
		if err != nil {
			t.Fatalf("decide %q: %v", invocationID, err)
		}
	}
	if approval.Status.DecisionAudit == nil {
		t.Fatalf("decision audit missing for %q", invocationID)
	}
	approval, err = st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", invocationID, approval.ResourceVersion, approval.Status.DecisionAudit.ID)
	if err != nil {
		t.Fatalf("ack %q: %v", invocationID, err)
	}
	if !approval.Status.DecisionEvidenceDelivered {
		t.Fatalf("evidence not delivered for %q", invocationID)
	}
	return approval
}

func mustGeneratePreparationToken(t *testing.T) string {
	t.Helper()
	token, err := runtimeapproval.GeneratePreparationToken()
	if err != nil {
		t.Fatalf("GeneratePreparationToken: %v", err)
	}
	return token
}

func newClaimant(nowOffset time.Duration) api.RuntimeApprovalClaimant {
	claimedAt := time.Now().UTC().Add(nowOffset)
	return api.RuntimeApprovalClaimant{
		WorkloadUID:       "tool-gateway-pod-abc123",
		ProcessStartNonce: "nonce-0123456789abcdef",
		ClaimedAt:         claimedAt,
		LastHeartbeat:     claimedAt,
	}
}

func TestRuntimeApprovalExecutionConformance(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	conformancetest.RunPreDispatchDispositionContract(t, conformancetest.ExecutionDriver{
		Store: st,
		NewReady: func(t *testing.T, invocationID, decision string) api.RuntimeApproval {
			return newExecutionReadyApproval(t, ctx, st, invocationID, decision)
		},
	})
}

func TestPrepareRuntimeApprovalHappyPath(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	for _, decision := range []string{api.RuntimeApprovalDecisionNotRequired, api.RuntimeApprovalDecisionPending} {
		name := decision
		if decision == api.RuntimeApprovalDecisionPending {
			name = "ApprovedViaDecide"
		}
		t.Run(name, func(t *testing.T) {
			approval := newExecutionReadyApproval(t, ctx, st, "prep-happy-"+name, decision)
			token := mustGeneratePreparationToken(t)
			deadline := time.Now().Add(2 * time.Minute)
			prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", approval.Spec.InvocationID, approval.ResourceVersion, token, deadline)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if prepared.Status.PreparationToken != token || prepared.Status.PreparationDeadline == nil {
				t.Fatalf("prepared token/deadline = %q/%v", prepared.Status.PreparationToken, prepared.Status.PreparationDeadline)
			}
			if prepared.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
				t.Fatalf("execution state after prepare = %q", prepared.Status.ExecutionState)
			}
			if prepared.ResourceVersion != approval.ResourceVersion+1 {
				t.Fatalf("resource version after prepare = %d, want %d", prepared.ResourceVersion, approval.ResourceVersion+1)
			}
		})
	}
}

func TestPrepareRuntimeApprovalRejectedStates(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	t.Run("PendingDecision", func(t *testing.T) {
		_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "prep-pending", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-pending", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("prepare-pending err = %v, want ErrConflict", err)
		}
	})

	t.Run("EvidenceNotDelivered", func(t *testing.T) {
		_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "prep-no-evidence", api.RuntimeApprovalDecisionNotRequired), runtimeApprovalPayload, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		// Skip Ack — evidence not delivered.
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-no-evidence", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("prepare-no-evidence err = %v, want ErrConflict", err)
		}
	})

	t.Run("DoublePrepare", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "prep-double", api.RuntimeApprovalDecisionNotRequired)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-double", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatalf("first prepare: %v", err)
		}
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-double", prepared.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second prepare err = %v, want ErrConflict", err)
		}
	})

	t.Run("WrongResourceVersion", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "prep-wrong-rv", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-wrong-rv", approval.ResourceVersion+7, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("prepare wrong RV err = %v, want ErrConflict", err)
		}
	})

	t.Run("TokenTooShort", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "prep-short-tok", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-short-tok", approval.ResourceVersion, "short", time.Now().Add(2*time.Minute)); err == nil || strings.Contains(err.Error(), "ErrConflict") {
			t.Fatalf("prepare short token err = %v, want validation error", err)
		}
	})

	t.Run("DeadlineExceedsCap", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "prep-long-deadline", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-long-deadline", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(10*time.Minute)); err == nil {
			t.Fatalf("prepare over-cap deadline: err = nil, want cap error")
		}
	})

	t.Run("DeadlineInPast", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "prep-past-deadline", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.PrepareRuntimeApproval(ctx, "team-a", "prep-past-deadline", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(-time.Minute)); err == nil {
			t.Fatalf("prepare past deadline: err = nil, want validation error")
		}
	})
}

func TestAbortRuntimeApprovalBeforeDispatch(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	t.Run("WithPreparedToken", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "abort-with-tok", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "abort-with-tok", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		aborted, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-with-tok", prepared.ResourceVersion, token, runtimeapproval.ExecutionDispositionQuotaDenied)
		if err != nil {
			t.Fatalf("abort: %v", err)
		}
		if aborted.Status.ExecutionState != api.RuntimeApprovalExecutionFailed ||
			aborted.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionQuotaDenied ||
			aborted.Status.PreparationToken != "" || aborted.Status.PreparationDeadline != nil {
			t.Fatalf("abort state = %#v", aborted.Status)
		}
	})

	t.Run("WithoutPreparedToken", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "abort-no-tok", api.RuntimeApprovalDecisionNotRequired)
		aborted, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-no-tok", approval.ResourceVersion, "", runtimeapproval.ExecutionDispositionSessionChanged)
		if err != nil {
			t.Fatalf("abort-no-tok: %v", err)
		}
		if aborted.Status.ExecutionState != api.RuntimeApprovalExecutionFailed || aborted.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionSessionChanged {
			t.Fatalf("abort-no-tok state = %#v", aborted.Status)
		}
	})

	t.Run("TokenMismatch", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "abort-tok-mismatch", api.RuntimeApprovalDecisionNotRequired)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "abort-tok-mismatch", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-tok-mismatch", prepared.ResourceVersion, mustGeneratePreparationToken(t), runtimeapproval.ExecutionDispositionQuotaDenied); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("abort tok mismatch err = %v, want ErrConflict", err)
		}
	})

	t.Run("RejectsReservedDispositions", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "abort-reserved", api.RuntimeApprovalDecisionNotRequired)
		for _, disp := range []string{
			runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim,
			runtimeapproval.ExecutionDispositionAbandonedPreparation,
		} {
			if _, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-reserved", approval.ResourceVersion, "", disp); !errors.Is(err, runtimeapproval.ErrPreDispatchDispositionInvalid) {
				t.Fatalf("abort with reserved %q err = %v", disp, err)
			}
		}
	})

	t.Run("RejectsInvalidDisposition", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "abort-bad-disp", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-bad-disp", approval.ResourceVersion, "", "Bogus"); !errors.Is(err, runtimeapproval.ErrPreDispatchDispositionInvalid) {
			t.Fatalf("abort bogus disp err = %v", err)
		}
	})

	t.Run("RejectsAfterAbort", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "abort-terminal", api.RuntimeApprovalDecisionNotRequired)
		aborted, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-terminal", approval.ResourceVersion, "", runtimeapproval.ExecutionDispositionPolicyChanged)
		if err != nil {
			t.Fatalf("first abort: %v", err)
		}
		if _, err := st.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-terminal", aborted.ResourceVersion, "", runtimeapproval.ExecutionDispositionPolicyChanged); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second abort err = %v, want ErrConflict", err)
		}
	})
}

func TestClaimRuntimeApprovalExecution(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	t.Run("HappyPath", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "claim-happy", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "claim-happy", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-happy", prepared.ResourceVersion, token, newClaimant(0))
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed {
			t.Fatalf("state after claim = %q", claimed.Status.ExecutionState)
		}
		if claimed.Status.FencingToken != runtimeapproval.MinFencingToken {
			t.Fatalf("fencing token = %d", claimed.Status.FencingToken)
		}
		if claimed.Status.Claimant == nil || claimed.Status.Claimant.WorkloadUID == "" {
			t.Fatalf("claimant not persisted: %#v", claimed.Status.Claimant)
		}
		if claimed.Status.PreparationToken != "" || claimed.Status.PreparationDeadline != nil {
			t.Fatalf("preparation state not cleared: %#v", claimed.Status)
		}
	})

	t.Run("TokenMismatch", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "claim-tok-mismatch", api.RuntimeApprovalDecisionNotRequired)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "claim-tok-mismatch", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-tok-mismatch", prepared.ResourceVersion, mustGeneratePreparationToken(t), newClaimant(0)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("claim tok mismatch err = %v", err)
		}
	})

	t.Run("ClaimantBoundsRejected", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "claim-bad-claimant", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "claim-bad-claimant", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		bad := newClaimant(0)
		bad.ProcessStartNonce = "short" // < 16
		if _, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-bad-claimant", prepared.ResourceVersion, token, bad); err == nil {
			t.Fatalf("bad claimant accepted")
		}
	})

	t.Run("PreparationDeadlineExpired", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "claim-prep-expired", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "claim-prep-expired", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		// Push preparation_deadline into the past.
		if _, err := testPool.Exec(ctx, `WITH b AS (SELECT clock_timestamp() - interval '10 seconds' AS d)
			UPDATE runtime_approvals SET preparation_deadline = b.d,
			data = jsonb_set(data, '{status,preparationDeadline}', to_jsonb(b.d))
			FROM b WHERE workspace = 'team-a' AND invocation_id = 'claim-prep-expired'`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-prep-expired", prepared.ResourceVersion, token, newClaimant(0)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expired prep err = %v", err)
		}
	})

	t.Run("ApprovedButExpired", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "claim-approved-expired", api.RuntimeApprovalDecisionPending)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "claim-approved-expired", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		// Push decision expiry into the past.
		if _, err := testPool.Exec(ctx, `WITH b AS (SELECT clock_timestamp() - interval '10 seconds' AS d)
			UPDATE runtime_approvals SET expires_at = b.d,
			data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(b.d))
			FROM b WHERE workspace = 'team-a' AND invocation_id = 'claim-approved-expired'`); err != nil {
			t.Fatal(err)
		}
		result, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-approved-expired", prepared.ResourceVersion, token, newClaimant(0))
		if err != nil {
			t.Fatalf("claim expired-approval: %v", err)
		}
		if result.Status.ExecutionState != api.RuntimeApprovalExecutionFailed ||
			result.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim {
			t.Fatalf("expired-approval terminal state = %#v", result.Status)
		}
		if result.Status.Claimant != nil {
			t.Fatalf("expired-approval must not persist claimant: %#v", result.Status.Claimant)
		}
	})
}

func TestMarkRuntimeApprovalClaimUncertain(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	approval := newExecutionReadyApproval(t, ctx, st, "uncertain-happy", api.RuntimeApprovalDecisionNotRequired)
	token := mustGeneratePreparationToken(t)
	prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "uncertain-happy", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "uncertain-happy", prepared.ResourceVersion, token, newClaimant(0))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("WrongFencing", func(t *testing.T) {
		if _, err := st.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", "uncertain-happy", claimed.ResourceVersion, claimed.Status.FencingToken+99, "proof://x"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("wrong fencing err = %v", err)
		}
	})

	t.Run("EmptyProof", func(t *testing.T) {
		if _, err := st.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", "uncertain-happy", claimed.ResourceVersion, claimed.Status.FencingToken, ""); err == nil {
			t.Fatalf("empty proof accepted")
		}
	})

	t.Run("HappyPath", func(t *testing.T) {
		uncertain, err := st.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", "uncertain-happy", claimed.ResourceVersion, claimed.Status.FencingToken, "proof://x")
		if err != nil {
			t.Fatalf("uncertain: %v", err)
		}
		if uncertain.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain || uncertain.Status.TerminationProofRef != "proof://x" {
			t.Fatalf("uncertain state = %#v", uncertain.Status)
		}
	})
}

func TestFinalizeRuntimeApprovalExecution(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		disposition string
		want        string
	}{
		{"Succeeded", runtimeapproval.ExecutionDispositionSucceeded, api.RuntimeApprovalExecutionCompleted},
		{"Failed", runtimeapproval.ExecutionDispositionFailed, api.RuntimeApprovalExecutionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := "finalize-" + tc.name
			approval := newExecutionReadyApproval(t, ctx, st, inv, api.RuntimeApprovalDecisionNotRequired)
			token := mustGeneratePreparationToken(t)
			prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", inv, approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", inv, prepared.ResourceVersion, token, newClaimant(0))
			if err != nil {
				t.Fatal(err)
			}
			final, err := st.FinalizeRuntimeApprovalExecution(ctx, "team-a", inv, claimed.ResourceVersion, claimed.Status.FencingToken, tc.disposition)
			if err != nil {
				t.Fatalf("finalize: %v", err)
			}
			if final.Status.ExecutionState != tc.want || final.Status.ExecutionDisposition != tc.disposition {
				t.Fatalf("final state = %#v", final.Status)
			}
		})
	}

	t.Run("WrongFencing", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "finalize-wrong-fencing", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "finalize-wrong-fencing", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "finalize-wrong-fencing", prepared.ResourceVersion, token, newClaimant(0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.FinalizeRuntimeApprovalExecution(ctx, "team-a", "finalize-wrong-fencing", claimed.ResourceVersion, claimed.Status.FencingToken+42, runtimeapproval.ExecutionDispositionSucceeded); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("wrong fencing err = %v", err)
		}
	})

	t.Run("BadDisposition", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "finalize-bad-disp", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.FinalizeRuntimeApprovalExecution(ctx, "team-a", "finalize-bad-disp", approval.ResourceVersion, 1, "Bogus"); !errors.Is(err, runtimeapproval.ErrTerminalDispositionInvalid) {
			t.Fatalf("bad disposition err = %v", err)
		}
	})

	t.Run("RejectedFromNotStarted", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "finalize-not-claimed", api.RuntimeApprovalDecisionNotRequired)
		if _, err := st.FinalizeRuntimeApprovalExecution(ctx, "team-a", "finalize-not-claimed", approval.ResourceVersion, 1, runtimeapproval.ExecutionDispositionSucceeded); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("not-claimed err = %v", err)
		}
	})
}

func TestReconcileRuntimeApprovalExecution(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	newUncertain := func(t *testing.T, inv string) api.RuntimeApproval {
		t.Helper()
		approval := newExecutionReadyApproval(t, ctx, st, inv, api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", inv, approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", inv, prepared.ResourceVersion, token, newClaimant(0))
		if err != nil {
			t.Fatal(err)
		}
		uncertain, err := st.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", inv, claimed.ResourceVersion, claimed.Status.FencingToken, "proof://x")
		if err != nil {
			t.Fatal(err)
		}
		return uncertain
	}

	for _, tc := range []struct {
		name        string
		disposition string
		want        string
	}{
		{"ReconciledCompleted", runtimeapproval.ExecutionDispositionReconciledCompleted, api.RuntimeApprovalExecutionCompleted},
		{"ReconciledNotExecuted", runtimeapproval.ExecutionDispositionReconciledNotExecuted, api.RuntimeApprovalExecutionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := "reconcile-" + tc.name
			uncertain := newUncertain(t, inv)
			final, err := st.ReconcileRuntimeApprovalExecution(ctx, "team-a", inv, uncertain.ResourceVersion, tc.disposition, "evidence://y", "kubernetes.io/tokenreview", "operator-a")
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if final.Status.ExecutionState != tc.want || final.Status.ExecutionDisposition != tc.disposition || final.Status.ReconciliationEvidenceRef != "evidence://y" || final.Status.ReconciledByIssuer != "kubernetes.io/tokenreview" || final.Status.ReconciledBySubject != "operator-a" {
				t.Fatalf("reconcile state = %#v", final.Status)
			}
		})
	}

	t.Run("BadDisposition", func(t *testing.T) {
		uncertain := newUncertain(t, "reconcile-bad-disp")
		if _, err := st.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-bad-disp", uncertain.ResourceVersion, "Bogus", "evidence://x", "issuer", "subject"); !errors.Is(err, runtimeapproval.ErrReconciliationDispositionInvalid) {
			t.Fatalf("bad reconcile disp err = %v", err)
		}
	})

	t.Run("EmptyEvidence", func(t *testing.T) {
		uncertain := newUncertain(t, "reconcile-empty-evidence")
		if _, err := st.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-empty-evidence", uncertain.ResourceVersion, runtimeapproval.ExecutionDispositionReconciledCompleted, "", "issuer", "subject"); err == nil {
			t.Fatalf("empty evidence accepted")
		}
	})

	for _, tc := range []struct {
		name    string
		issuer  string
		subject string
	}{
		{name: "EmptyIssuer", subject: "operator-a"},
		{name: "EmptySubject", issuer: "kubernetes.io/tokenreview"},
		{name: "OversizedIssuer", issuer: strings.Repeat("i", 513), subject: "operator-a"},
		{name: "OversizedSubject", issuer: "kubernetes.io/tokenreview", subject: strings.Repeat("s", 513)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-invalid-actor", 1, runtimeapproval.ExecutionDispositionReconciledCompleted, "evidence://x", tc.issuer, tc.subject); err == nil {
				t.Fatalf("invalid reconciliation actor accepted")
			}
		})
	}

	t.Run("RejectedFromClaimed", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "reconcile-from-claimed", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, _ := st.PrepareRuntimeApproval(ctx, "team-a", "reconcile-from-claimed", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		claimed, _ := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "reconcile-from-claimed", prepared.ResourceVersion, token, newClaimant(0))
		if _, err := st.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-from-claimed", claimed.ResourceVersion, runtimeapproval.ExecutionDispositionReconciledCompleted, "evidence://x", "issuer", "subject"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reconcile-from-claimed err = %v", err)
		}
	})
}

func TestReapRuntimeApprovalPreparation(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	t.Run("HappyPath", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "reap-happy", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "reap-happy", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `WITH b AS (SELECT clock_timestamp() - interval '10 seconds' AS d)
			UPDATE runtime_approvals SET preparation_deadline = b.d,
			data = jsonb_set(data, '{status,preparationDeadline}', to_jsonb(b.d))
			FROM b WHERE workspace = 'team-a' AND invocation_id = 'reap-happy'`); err != nil {
			t.Fatal(err)
		}
		reaped, err := st.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-happy", prepared.ResourceVersion, token)
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if reaped.Status.ExecutionState != api.RuntimeApprovalExecutionFailed ||
			reaped.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionAbandonedPreparation {
			t.Fatalf("reap state = %#v", reaped.Status)
		}
		if reaped.Status.PreparationToken != "" || reaped.Status.PreparationDeadline != nil {
			t.Fatalf("reap must clear preparation fields: %#v", reaped.Status)
		}
	})

	t.Run("BeforeDeadline", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "reap-early", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "reap-early", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-early", prepared.ResourceVersion, token); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reap-early err = %v", err)
		}
	})

	t.Run("TokenMismatch", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "reap-tok-mismatch", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "reap-tok-mismatch", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(ctx, `WITH b AS (SELECT clock_timestamp() - interval '10 seconds' AS d)
			UPDATE runtime_approvals SET preparation_deadline = b.d,
			data = jsonb_set(data, '{status,preparationDeadline}', to_jsonb(b.d))
			FROM b WHERE workspace = 'team-a' AND invocation_id = 'reap-tok-mismatch'`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-tok-mismatch", prepared.ResourceVersion, mustGeneratePreparationToken(t)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reap tok mismatch err = %v", err)
		}
	})

	t.Run("WrongState", func(t *testing.T) {
		approval := newExecutionReadyApproval(t, ctx, st, "reap-wrong-state", api.RuntimeApprovalDecisionNotRequired)
		token := mustGeneratePreparationToken(t)
		prepared, err := st.PrepareRuntimeApproval(ctx, "team-a", "reap-wrong-state", approval.ResourceVersion, token, time.Now().Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := st.ClaimRuntimeApprovalExecution(ctx, "team-a", "reap-wrong-state", prepared.ResourceVersion, token, newClaimant(0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-wrong-state", claimed.ResourceVersion, token); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reap-wrong-state err = %v", err)
		}
	})
}

func TestRuntimeApprovalExecutionSerializesWithAdvisoryLock(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	approval := newExecutionReadyApproval(t, ctx, st, "exec-serialize", api.RuntimeApprovalDecisionNotRequired)

	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	logicalKey := "team-a\x1fexec-serialize"
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, logicalKey); err != nil {
		t.Fatal(err)
	}
	baseline := waitingAdvisoryLockCount(t, ctx)
	done := make(chan error, 1)
	go func() {
		_, err := st.PrepareRuntimeApproval(ctx, "team-a", "exec-serialize", approval.ResourceVersion, mustGeneratePreparationToken(t), time.Now().Add(2*time.Minute))
		done <- err
	}()
	waitForAdvisoryLockWaiters(t, ctx, baseline+1)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("queued prepare: %v", err)
	}
}
