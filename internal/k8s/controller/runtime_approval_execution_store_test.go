// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeapproval/conformancetest"
	"github.com/kognition-ai/kognition/internal/store"
)

type rejectRuntimeApprovalStatusUpdateClient struct {
	client.Client
}

type rejectRuntimeApprovalStatusUpdateWriter struct {
	client.SubResourceWriter
}

func (c *rejectRuntimeApprovalStatusUpdateClient) Status() client.SubResourceWriter {
	return &rejectRuntimeApprovalStatusUpdateWriter{SubResourceWriter: c.Client.Status()}
}

func (w *rejectRuntimeApprovalStatusUpdateWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("full RuntimeApproval status update is forbidden")
}

// newExecutionReadyKubernetesApproval walks the fixture through Create → Decide → Ack so that
// tests can start from a decision-evidence-delivered state. Time flows through the adapter's
// controlled clock (set by callers) so Prepare/Claim deadline arithmetic is deterministic.
func newExecutionReadyKubernetesApproval(t *testing.T, ctx context.Context, adapter *StoreAdapter, invocationID, decision string) api.RuntimeApproval {
	t.Helper()
	fixture := kubernetesRuntimeApprovalFixture(t, invocationID, decision)
	created, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, fixture, kubernetesRuntimeApprovalPayload, 15*time.Minute)
	if err != nil || !created {
		t.Fatalf("create %q: created=%v err=%v", invocationID, created, err)
	}
	if decision == api.RuntimeApprovalDecisionPending {
		approval, err = adapter.DecideRuntimeApproval(ctx, "team-a", invocationID, store.RuntimeApprovalDecision{
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
	approval, err = adapter.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", invocationID, approval.ResourceVersion, approval.Status.DecisionAudit.ID)
	if err != nil {
		t.Fatalf("ack %q: %v", invocationID, err)
	}
	if !approval.Status.DecisionEvidenceDelivered {
		t.Fatalf("evidence not delivered for %q", invocationID)
	}
	return approval
}

func mustGenerateKubernetesPreparationToken(t *testing.T) string {
	t.Helper()
	token, err := runtimeapproval.GeneratePreparationToken()
	if err != nil {
		t.Fatalf("GeneratePreparationToken: %v", err)
	}
	return token
}

func newKubernetesClaimant(claimedAt time.Time) api.RuntimeApprovalClaimant {
	return api.RuntimeApprovalClaimant{
		WorkloadUID:       "tool-gateway-pod-abc123",
		ProcessStartNonce: "nonce-0123456789abcdef",
		ClaimedAt:         claimedAt,
		LastHeartbeat:     claimedAt,
	}
}

// newExecutionTestAdapter builds an adapter with a frozen clock so tests can advance time
// deterministically for deadline-based transitions.
func newExecutionTestAdapter(t *testing.T, base *time.Time) *StoreAdapter {
	t.Helper()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	adapter.now = func() time.Time { return *base }
	return adapter
}

func TestKubernetesRuntimeApprovalExecutionConformance(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)
	conformancetest.RunPreDispatchDispositionContract(t, conformancetest.ExecutionDriver{
		Store: adapter,
		NewReady: func(t *testing.T, invocationID, decision string) api.RuntimeApproval {
			return newExecutionReadyKubernetesApproval(t, ctx, adapter, invocationID, decision)
		},
	})
}

func TestKubernetesPrepareRuntimeApprovalHappyPath(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

	for _, decision := range []string{api.RuntimeApprovalDecisionNotRequired, api.RuntimeApprovalDecisionPending} {
		name := decision
		if decision == api.RuntimeApprovalDecisionPending {
			name = "ApprovedViaDecide"
		}
		t.Run(name, func(t *testing.T) {
			approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-happy-"+name, decision)
			token := mustGenerateKubernetesPreparationToken(t)
			deadline := now.Add(2 * time.Minute)
			prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", approval.Spec.InvocationID, approval.ResourceVersion, token, deadline)
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if prepared.Status.PreparationToken != token || prepared.Status.PreparationDeadline == nil {
				t.Fatalf("prepared token/deadline = %q/%v", prepared.Status.PreparationToken, prepared.Status.PreparationDeadline)
			}
			if prepared.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
				t.Fatalf("execution state after prepare = %q", prepared.Status.ExecutionState)
			}
			if prepared.ResourceVersion <= approval.ResourceVersion {
				t.Fatalf("resource version did not advance after prepare: before=%d after=%d", approval.ResourceVersion, prepared.ResourceVersion)
			}
		})
	}
}

func TestKubernetesPrepareRuntimeApprovalPatchesStatus(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 2, 23, 29, 989695600, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)
	approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-patches-status", api.RuntimeApprovalDecisionPending)
	adapter.client = &rejectRuntimeApprovalStatusUpdateClient{Client: adapter.client}

	prepared, err := adapter.PrepareRuntimeApproval(
		ctx,
		"team-a",
		approval.Spec.InvocationID,
		approval.ResourceVersion,
		mustGenerateKubernetesPreparationToken(t),
		now.Add(2*time.Minute),
	)
	if err != nil {
		t.Fatalf("prepare with status update forbidden: %v", err)
	}
	if prepared.Status.DecisionAudit == nil || prepared.Status.DecisionAudit.ID != approval.Status.DecisionAudit.ID {
		t.Fatalf("decision audit changed during preparation: before=%+v after=%+v", approval.Status.DecisionAudit, prepared.Status.DecisionAudit)
	}
}

func TestKubernetesPrepareRuntimeApprovalRejectedStates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

	t.Run("PendingDecision", func(t *testing.T) {
		_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "prep-pending", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-pending", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("prepare-pending err = %v, want ErrConflict", err)
		}
	})

	t.Run("EvidenceNotDelivered", func(t *testing.T) {
		_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "prep-no-evidence", api.RuntimeApprovalDecisionNotRequired), kubernetesRuntimeApprovalPayload, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		// Skip Ack — evidence not delivered.
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-no-evidence", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("prepare-no-evidence err = %v, want ErrConflict", err)
		}
	})

	t.Run("DoublePrepare", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-double", api.RuntimeApprovalDecisionNotRequired)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-double", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("first prepare: %v", err)
		}
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-double", prepared.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second prepare err = %v, want ErrConflict", err)
		}
	})

	t.Run("WrongResourceVersion", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-wrong-rv", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-wrong-rv", approval.ResourceVersion+7, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("prepare wrong RV err = %v, want ErrConflict", err)
		}
	})

	t.Run("TokenTooShort", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-short-tok", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-short-tok", approval.ResourceVersion, "short", now.Add(2*time.Minute)); err == nil || strings.Contains(err.Error(), "ErrConflict") {
			t.Fatalf("prepare short token err = %v, want validation error", err)
		}
	})

	t.Run("DeadlineExceedsCap", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-long-deadline", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-long-deadline", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(10*time.Minute)); err == nil {
			t.Fatalf("prepare over-cap deadline: err = nil, want cap error")
		}
	})

	t.Run("DeadlineInPast", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "prep-past-deadline", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "prep-past-deadline", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(-time.Minute)); err == nil {
			t.Fatalf("prepare past deadline: err = nil, want validation error")
		}
	})
}

func TestKubernetesAbortRuntimeApprovalBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

	t.Run("WithPreparedToken", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "abort-with-tok", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "abort-with-tok", approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		aborted, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-with-tok", prepared.ResourceVersion, token, runtimeapproval.ExecutionDispositionQuotaDenied)
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
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "abort-no-tok", api.RuntimeApprovalDecisionNotRequired)
		aborted, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-no-tok", approval.ResourceVersion, "", runtimeapproval.ExecutionDispositionSessionChanged)
		if err != nil {
			t.Fatalf("abort-no-tok: %v", err)
		}
		if aborted.Status.ExecutionState != api.RuntimeApprovalExecutionFailed || aborted.Status.ExecutionDisposition != runtimeapproval.ExecutionDispositionSessionChanged {
			t.Fatalf("abort-no-tok state = %#v", aborted.Status)
		}
	})

	t.Run("TokenMismatch", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "abort-tok-mismatch", api.RuntimeApprovalDecisionNotRequired)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "abort-tok-mismatch", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-tok-mismatch", prepared.ResourceVersion, mustGenerateKubernetesPreparationToken(t), runtimeapproval.ExecutionDispositionQuotaDenied); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("abort tok mismatch err = %v, want ErrConflict", err)
		}
	})

	t.Run("RejectsReservedDispositions", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "abort-reserved", api.RuntimeApprovalDecisionNotRequired)
		for _, disp := range []string{
			runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim,
			runtimeapproval.ExecutionDispositionAbandonedPreparation,
		} {
			if _, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-reserved", approval.ResourceVersion, "", disp); !errors.Is(err, runtimeapproval.ErrPreDispatchDispositionInvalid) {
				t.Fatalf("abort with reserved %q err = %v", disp, err)
			}
		}
	})

	t.Run("RejectsInvalidDisposition", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "abort-bad-disp", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-bad-disp", approval.ResourceVersion, "", "Bogus"); !errors.Is(err, runtimeapproval.ErrPreDispatchDispositionInvalid) {
			t.Fatalf("abort bogus disp err = %v", err)
		}
	})

	t.Run("RejectsAfterAbort", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "abort-terminal", api.RuntimeApprovalDecisionNotRequired)
		aborted, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-terminal", approval.ResourceVersion, "", runtimeapproval.ExecutionDispositionPolicyChanged)
		if err != nil {
			t.Fatalf("first abort: %v", err)
		}
		if _, err := adapter.AbortRuntimeApprovalBeforeDispatch(ctx, "team-a", "abort-terminal", aborted.ResourceVersion, "", runtimeapproval.ExecutionDispositionPolicyChanged); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("second abort err = %v, want ErrConflict", err)
		}
	})
}

func TestKubernetesClaimRuntimeApprovalExecution(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

	t.Run("HappyPath", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "claim-happy", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "claim-happy", approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-happy", prepared.ResourceVersion, token, newKubernetesClaimant(now))
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
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "claim-tok-mismatch", api.RuntimeApprovalDecisionNotRequired)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "claim-tok-mismatch", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-tok-mismatch", prepared.ResourceVersion, mustGenerateKubernetesPreparationToken(t), newKubernetesClaimant(now)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("claim tok mismatch err = %v", err)
		}
	})

	t.Run("ClaimantBoundsRejected", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "claim-bad-claimant", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "claim-bad-claimant", approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		bad := newKubernetesClaimant(now)
		bad.ProcessStartNonce = "short" // < 16
		if _, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-bad-claimant", prepared.ResourceVersion, token, bad); err == nil {
			t.Fatalf("bad claimant accepted")
		}
	})

	t.Run("PreparationDeadlineExpired", func(t *testing.T) {
		// Isolate this subtest with its own frozen-time adapter so advancing time does not
		// leak into sibling subtests that share the parent adapter.
		subNow := now
		subAdapter := newExecutionTestAdapter(t, &subNow)
		approval := newExecutionReadyKubernetesApproval(t, ctx, subAdapter, "claim-prep-expired", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := subAdapter.PrepareRuntimeApproval(ctx, "team-a", "claim-prep-expired", approval.ResourceVersion, token, subNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		// Advance clock past the preparation deadline.
		subNow = subNow.Add(3 * time.Minute)
		if _, err := subAdapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-prep-expired", prepared.ResourceVersion, token, newKubernetesClaimant(subNow)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expired prep err = %v", err)
		}
	})

	t.Run("ApprovedButExpired", func(t *testing.T) {
		// Isolate this subtest to control time independently.
		subNow := now
		subAdapter := newExecutionTestAdapter(t, &subNow)
		approval := newExecutionReadyKubernetesApproval(t, ctx, subAdapter, "claim-approved-expired", api.RuntimeApprovalDecisionPending)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := subAdapter.PrepareRuntimeApproval(ctx, "team-a", "claim-approved-expired", approval.ResourceVersion, token, subNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		// Advance clock past decision ExpiresAt (15m TTL from Create) but still within the
		// preparation deadline arithmetic — we need to hit the Approved-but-past-expiry branch
		// before the deadline-expired branch, so we set a longer prep deadline first, then
		// advance time past ExpiresAt while still holding a valid deadline. However the code
		// checks approval-expiry BEFORE deadline, so we can advance past ExpiresAt regardless.
		subNow = subNow.Add(20 * time.Minute)
		result, err := subAdapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "claim-approved-expired", prepared.ResourceVersion, token, newKubernetesClaimant(subNow))
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

func TestKubernetesMarkRuntimeApprovalClaimUncertain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)
	approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "uncertain-happy", api.RuntimeApprovalDecisionNotRequired)
	token := mustGenerateKubernetesPreparationToken(t)
	prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "uncertain-happy", approval.ResourceVersion, token, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "uncertain-happy", prepared.ResourceVersion, token, newKubernetesClaimant(now))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("WrongFencing", func(t *testing.T) {
		if _, err := adapter.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", "uncertain-happy", claimed.ResourceVersion, claimed.Status.FencingToken+99, "proof://x"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("wrong fencing err = %v", err)
		}
	})

	t.Run("EmptyProof", func(t *testing.T) {
		if _, err := adapter.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", "uncertain-happy", claimed.ResourceVersion, claimed.Status.FencingToken, ""); err == nil {
			t.Fatalf("empty proof accepted")
		}
	})

	t.Run("HappyPath", func(t *testing.T) {
		uncertain, err := adapter.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", "uncertain-happy", claimed.ResourceVersion, claimed.Status.FencingToken, "proof://x")
		if err != nil {
			t.Fatalf("uncertain: %v", err)
		}
		if uncertain.Status.ExecutionState != api.RuntimeApprovalExecutionUncertain || uncertain.Status.TerminationProofRef != "proof://x" {
			t.Fatalf("uncertain state = %#v", uncertain.Status)
		}
	})
}

func TestKubernetesFinalizeRuntimeApprovalExecution(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

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
			approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, inv, api.RuntimeApprovalDecisionNotRequired)
			token := mustGenerateKubernetesPreparationToken(t)
			prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", inv, approval.ResourceVersion, token, now.Add(2*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", inv, prepared.ResourceVersion, token, newKubernetesClaimant(now))
			if err != nil {
				t.Fatal(err)
			}
			final, err := adapter.FinalizeRuntimeApprovalExecution(ctx, "team-a", inv, claimed.ResourceVersion, claimed.Status.FencingToken, tc.disposition)
			if err != nil {
				t.Fatalf("finalize: %v", err)
			}
			if final.Status.ExecutionState != tc.want || final.Status.ExecutionDisposition != tc.disposition {
				t.Fatalf("final state = %#v", final.Status)
			}
		})
	}

	t.Run("WrongFencing", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "finalize-wrong-fencing", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "finalize-wrong-fencing", approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "finalize-wrong-fencing", prepared.ResourceVersion, token, newKubernetesClaimant(now))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.FinalizeRuntimeApprovalExecution(ctx, "team-a", "finalize-wrong-fencing", claimed.ResourceVersion, claimed.Status.FencingToken+42, runtimeapproval.ExecutionDispositionSucceeded); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("wrong fencing err = %v", err)
		}
	})

	t.Run("BadDisposition", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "finalize-bad-disp", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.FinalizeRuntimeApprovalExecution(ctx, "team-a", "finalize-bad-disp", approval.ResourceVersion, 1, "Bogus"); !errors.Is(err, runtimeapproval.ErrTerminalDispositionInvalid) {
			t.Fatalf("bad disposition err = %v", err)
		}
	})

	t.Run("RejectedFromNotStarted", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "finalize-not-claimed", api.RuntimeApprovalDecisionNotRequired)
		if _, err := adapter.FinalizeRuntimeApprovalExecution(ctx, "team-a", "finalize-not-claimed", approval.ResourceVersion, 1, runtimeapproval.ExecutionDispositionSucceeded); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("not-claimed err = %v", err)
		}
	})
}

func TestKubernetesReconcileRuntimeApprovalExecution(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

	newUncertain := func(t *testing.T, inv string) api.RuntimeApproval {
		t.Helper()
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, inv, api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", inv, approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", inv, prepared.ResourceVersion, token, newKubernetesClaimant(now))
		if err != nil {
			t.Fatal(err)
		}
		uncertain, err := adapter.MarkRuntimeApprovalClaimUncertain(ctx, "team-a", inv, claimed.ResourceVersion, claimed.Status.FencingToken, "proof://x")
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
			final, err := adapter.ReconcileRuntimeApprovalExecution(ctx, "team-a", inv, uncertain.ResourceVersion, tc.disposition, "evidence://y", "kubernetes.io/tokenreview", "operator-a")
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
		if _, err := adapter.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-bad-disp", uncertain.ResourceVersion, "Bogus", "evidence://x", "issuer", "subject"); !errors.Is(err, runtimeapproval.ErrReconciliationDispositionInvalid) {
			t.Fatalf("bad reconcile disp err = %v", err)
		}
	})

	t.Run("EmptyEvidence", func(t *testing.T) {
		uncertain := newUncertain(t, "reconcile-empty-evidence")
		if _, err := adapter.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-empty-evidence", uncertain.ResourceVersion, runtimeapproval.ExecutionDispositionReconciledCompleted, "", "issuer", "subject"); err == nil {
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
			if _, err := adapter.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-invalid-actor", 1, runtimeapproval.ExecutionDispositionReconciledCompleted, "evidence://x", tc.issuer, tc.subject); err == nil {
				t.Fatalf("invalid reconciliation actor accepted")
			}
		})
	}

	t.Run("RejectedFromClaimed", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "reconcile-from-claimed", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, _ := adapter.PrepareRuntimeApproval(ctx, "team-a", "reconcile-from-claimed", approval.ResourceVersion, token, now.Add(2*time.Minute))
		claimed, _ := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "reconcile-from-claimed", prepared.ResourceVersion, token, newKubernetesClaimant(now))
		if _, err := adapter.ReconcileRuntimeApprovalExecution(ctx, "team-a", "reconcile-from-claimed", claimed.ResourceVersion, runtimeapproval.ExecutionDispositionReconciledCompleted, "evidence://x", "issuer", "subject"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reconcile-from-claimed err = %v", err)
		}
	})
}

func TestKubernetesReapRuntimeApprovalPreparation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)

	t.Run("HappyPath", func(t *testing.T) {
		subNow := now
		subAdapter := newExecutionTestAdapter(t, &subNow)
		approval := newExecutionReadyKubernetesApproval(t, ctx, subAdapter, "reap-happy", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := subAdapter.PrepareRuntimeApproval(ctx, "team-a", "reap-happy", approval.ResourceVersion, token, subNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		// Advance clock past the preparation deadline.
		subNow = subNow.Add(3 * time.Minute)
		reaped, err := subAdapter.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-happy", prepared.ResourceVersion, token)
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
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "reap-early", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "reap-early", approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-early", prepared.ResourceVersion, token); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reap-early err = %v", err)
		}
	})

	t.Run("TokenMismatch", func(t *testing.T) {
		subNow := now
		subAdapter := newExecutionTestAdapter(t, &subNow)
		approval := newExecutionReadyKubernetesApproval(t, ctx, subAdapter, "reap-tok-mismatch", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := subAdapter.PrepareRuntimeApproval(ctx, "team-a", "reap-tok-mismatch", approval.ResourceVersion, token, subNow.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		subNow = subNow.Add(3 * time.Minute)
		if _, err := subAdapter.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-tok-mismatch", prepared.ResourceVersion, mustGenerateKubernetesPreparationToken(t)); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reap tok mismatch err = %v", err)
		}
	})

	t.Run("WrongState", func(t *testing.T) {
		approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "reap-wrong-state", api.RuntimeApprovalDecisionNotRequired)
		token := mustGenerateKubernetesPreparationToken(t)
		prepared, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "reap-wrong-state", approval.ResourceVersion, token, now.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := adapter.ClaimRuntimeApprovalExecution(ctx, "team-a", "reap-wrong-state", prepared.ResourceVersion, token, newKubernetesClaimant(now))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.ReapRuntimeApprovalPreparation(ctx, "team-a", "reap-wrong-state", claimed.ResourceVersion, token); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("reap-wrong-state err = %v", err)
		}
	})
}

// TestKubernetesRuntimeApprovalExecutionRVConflict validates the K8s optimistic-concurrency
// invariant: two callers observing the same ResourceVersion see the first writer win and the
// second receive ErrConflict. This is the K8s equivalent of the postgres advisory-lock
// serialization test.
func TestKubernetesRuntimeApprovalExecutionRVConflict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter := newExecutionTestAdapter(t, &now)
	approval := newExecutionReadyKubernetesApproval(t, ctx, adapter, "exec-rv-conflict", api.RuntimeApprovalDecisionNotRequired)

	if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "exec-rv-conflict", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute)); err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Second Prepare with the pre-Prepare RV must fail with ErrConflict because the current
	// object has advanced past that version.
	if _, err := adapter.PrepareRuntimeApproval(ctx, "team-a", "exec-rv-conflict", approval.ResourceVersion, mustGenerateKubernetesPreparationToken(t), now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale RV prepare err = %v, want ErrConflict", err)
	}
}
