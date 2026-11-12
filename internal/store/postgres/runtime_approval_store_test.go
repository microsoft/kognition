// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestRuntimeApprovalCreateOrGetExactBindingAndInitialDecision(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	pending := runtimeApprovalFixture(t, "invocation-pending", api.RuntimeApprovalDecisionPending)
	created, first, err := st.CreateOrGetRuntimeApproval(ctx, pending, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created {
		t.Fatalf("first CreateOrGetRuntimeApproval() = created %v, err %v", created, err)
	}
	if first.Status.DecisionState != api.RuntimeApprovalDecisionPending || first.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		t.Fatalf("first state = %s/%s", first.Status.DecisionState, first.Status.ExecutionState)
	}
	if first.Name != runtimeapproval.CanonicalName("team-a", "invocation-pending") || first.UID == "" || first.ResourceVersion != 1 {
		t.Fatalf("store identity not assigned: %#v", first.ObjectMeta)
	}

	created, replay, err := st.CreateOrGetRuntimeApproval(ctx, pending, runtimeApprovalPayload, time.Minute)
	if err != nil || created {
		t.Fatalf("identical replay = created %v, err %v", created, err)
	}
	if replay.UID != first.UID || !replay.Spec.CreatedAt.Equal(first.Spec.CreatedAt) || !replay.Spec.ExpiresAt.Equal(first.Spec.ExpiresAt) {
		t.Fatalf("identical replay did not preserve store fields: %#v", replay)
	}
	loosened := pending
	loosened.Status = runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionNotRequired)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, loosened, runtimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("policy-loosening replay error = %v, want conflict", err)
	}

	conflict := pending
	conflict.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, conflict, runtimeApprovalPayload, 15*time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed binding error = %v, want conflict", err)
	}

	notRequired := runtimeApprovalFixture(t, "invocation-not-required", api.RuntimeApprovalDecisionNotRequired)
	created, result, err := st.CreateOrGetRuntimeApproval(ctx, notRequired, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created || result.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired {
		t.Fatalf("NotRequired create = created %v, state %q, err %v", created, result.Status.DecisionState, err)
	}
	if result.Status.DecisionAudit == nil || result.Status.DecidedAt == nil || result.Status.DecisionAudit.Payload.Decision != api.RuntimeApprovalDecisionNotRequired {
		t.Fatalf("NotRequired decision evidence = %#v", result.Status)
	}
}

func TestRuntimeApprovalCompactionPreservesReplayUntilRetention(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "compacted-replay", api.RuntimeApprovalDecisionNotRequired)
	created, approval, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created {
		t.Fatalf("create RuntimeApproval: created=%v err=%v", created, err)
	}
	approval.Status.DecisionEvidenceDelivered = true
	approval.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	approval.Status.ExecutionDisposition = "Succeeded"
	data, err := json.Marshal(approval)
	if err != nil {
		t.Fatalf("marshal terminal RuntimeApproval: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET decision_state = $3, execution_state = $4,
		execution_disposition = $5, data = $6, updated_at = transaction_timestamp()
		WHERE workspace = $1 AND invocation_id = $2`, "team-a", proposal.Spec.InvocationID,
		approval.Status.DecisionState, approval.Status.ExecutionState, approval.Status.ExecutionDisposition, data); err != nil {
		t.Fatalf("make RuntimeApproval compactable: %v", err)
	}

	compacted, err := st.CompactRuntimeApproval(ctx, "team-a", proposal.Spec.InvocationID, 90*24*time.Hour)
	if err != nil || !compacted {
		t.Fatalf("CompactRuntimeApproval() = (%v, %v), want (true, nil)", compacted, err)
	}
	created, replay, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || created {
		t.Fatalf("compacted replay = created %v, err %v", created, err)
	}
	if replay.UID != approval.UID || replay.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired || replay.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("compacted replay lost terminal identity/state: %#v", replay)
	}
	changed := proposal
	changed.Spec.Operation = "changed-invoke"
	binding, err := runtimeapproval.Bind(ctx, runtimeApprovalTestKeys(t), runtimeapproval.ActionEnvelopeFromApproval(changed, runtimeApprovalPayload))
	if err != nil {
		t.Fatalf("bind changed proposal: %v", err)
	}
	changed.Spec.Binding = binding
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, changed, runtimeApprovalPayload, 15*time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed compacted replay error = %v, want conflict", err)
	}

	pending := runtimeApprovalFixture(t, "not-compactable", api.RuntimeApprovalDecisionPending)
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, pending, runtimeApprovalPayload, 15*time.Minute); err != nil || !created {
		t.Fatalf("create pending RuntimeApproval: created=%v err=%v", created, err)
	}
	if compacted, err := st.CompactRuntimeApproval(ctx, "team-a", pending.Spec.InvocationID, 90*24*time.Hour); err != nil || compacted {
		t.Fatalf("compact pending RuntimeApproval = (%v, %v), want (false, nil)", compacted, err)
	}

	retired, err := st.RetireRuntimeApprovalTombstones(ctx, time.Now().UTC().Add(91*24*time.Hour))
	if err != nil || retired != 1 {
		t.Fatalf("RetireRuntimeApprovalTombstones() = (%d, %v), want (1, nil)", retired, err)
	}
	if _, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", proposal.Spec.InvocationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get retired RuntimeApproval error = %v, want not found", err)
	}
}

func TestRuntimeApprovalDecisionUsesDatabaseTimeAtExpiryBoundary(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	tests := []struct {
		name     string
		interval string
		want     string
	}{
		{name: "before", interval: "1 minute", want: api.RuntimeApprovalDecisionApproved},
		{name: "equal", interval: "0 seconds", want: api.RuntimeApprovalDecisionExpired},
		{name: "after", interval: "-1 second", want: api.RuntimeApprovalDecisionExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invocationID := "expiry-boundary-" + test.name
			_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, invocationID, api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `WITH boundary AS (SELECT clock_timestamp() + $1::interval AS expires_at) UPDATE runtime_approvals SET expires_at = boundary.expires_at, data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(boundary.expires_at)) FROM boundary WHERE workspace = 'team-a' AND invocation_id = $2`, test.interval, invocationID); err != nil {
				t.Fatal(err)
			}
			result, err := st.DecideRuntimeApproval(ctx, "team-a", invocationID, store.RuntimeApprovalDecision{
				Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion,
				Issuer: "https://issuer.example", Subject: "operator-1",
			})
			if err != nil || result.Status.DecisionState != test.want {
				t.Fatalf("decision = %q, err %v; want %q", result.Status.DecisionState, err, test.want)
			}
		})
	}
}

func TestRuntimeApprovalDecisionSamplesTimeAfterLockWait(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	invocationID := "expiry-after-lock-wait"
	_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, invocationID, api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	logicalKey := "team-a\x1f" + invocationID
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, logicalKey); err != nil {
		t.Fatal(err)
	}
	baseline := waitingAdvisoryLockCount(t, ctx)
	result := make(chan api.RuntimeApproval, 1)
	errResult := make(chan error, 1)
	go func() {
		decided, decideErr := st.DecideRuntimeApproval(ctx, "team-a", invocationID, store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: "operator-1"})
		result <- decided
		errResult <- decideErr
	}()
	waitForAdvisoryLockWaiters(t, ctx, baseline+1)
	if _, err := testPool.Exec(ctx, `WITH boundary AS (SELECT clock_timestamp() + interval '100 milliseconds' AS expires_at) UPDATE runtime_approvals SET expires_at = boundary.expires_at, data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(boundary.expires_at)) FROM boundary WHERE workspace = 'team-a' AND invocation_id = $1`, invocationID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `SELECT pg_sleep(0.2)`); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errResult; err != nil {
		t.Fatal(err)
	}
	if decided := <-result; decided.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
		t.Fatalf("decision after lock wait = %q, want Expired", decided.Status.DecisionState)
	}
}

func TestRuntimeApprovalCreateSerializesBeforeBindingKeyActivation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runtimeApprovalBindingEpochLock); err != nil {
		t.Fatal(err)
	}
	baseline := waitingAdvisoryLockCount(t, ctx)
	createResult := make(chan api.RuntimeApproval, 1)
	createErr := make(chan error, 1)
	go func() {
		_, created, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "create-before-activation", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
		createResult <- created
		createErr <- err
	}()
	waitForAdvisoryLockWaiters(t, ctx, baseline+1)
	activationErr := make(chan error, 1)
	go func() { activationErr <- st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)) }()
	waitForAdvisoryLockWaiters(t, ctx, baseline+2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-createErr; err != nil {
		t.Fatalf("queued create: %v", err)
	}
	if created := <-createResult; created.Spec.Binding.Epoch != 1 || created.Spec.Binding.KeyVersion != 1 {
		t.Fatalf("queued create binding = epoch %d version %d", created.Spec.Binding.Epoch, created.Spec.Binding.KeyVersion)
	}
	if err := <-activationErr; err != nil {
		t.Fatalf("queued activation: %v", err)
	}
	if err := st.RuntimeApprovalBindingKeysReady(ctx); err != nil {
		t.Fatalf("rotated key readiness: %v", err)
	}
}

func waitingAdvisoryLockCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func waitForAdvisoryLockWaiters(t *testing.T, ctx context.Context, minimum int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for waitingAdvisoryLockCount(t, ctx) < minimum {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d advisory lock waiters", minimum)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRuntimeApprovalDecisionCASAndExpiryAreFirstWriterWins(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "decision-race", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	decisions := []string{api.RuntimeApprovalDecisionApproved, api.RuntimeApprovalDecisionRejected}
	results := make(chan api.RuntimeApproval, len(decisions))
	errorsSeen := make(chan error, len(decisions))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, value := range decisions {
		value := value
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := st.DecideRuntimeApproval(ctx, "team-a", "decision-race", store.RuntimeApprovalDecision{
				Decision: value, ExpectedResourceVersion: approval.ResourceVersion,
				Issuer: "https://issuer.example", Subject: "operator-1",
			})
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent decision returned error: %v", err)
	}
	var winner string
	for result := range results {
		if winner == "" {
			winner = result.Status.DecisionState
		}
		if result.Status.DecisionState != winner {
			t.Fatalf("decision writers observed different terminal states: %q and %q", winner, result.Status.DecisionState)
		}
	}
	persisted, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", "decision-race")
	if err != nil || persisted.Status.DecisionState != winner || persisted.ResourceVersion != 2 {
		t.Fatalf("persisted winner = state %q rv %d, err %v", persisted.Status.DecisionState, persisted.ResourceVersion, err)
	}
	if persisted.Status.DecisionAudit == nil || persisted.Status.DecisionAudit.ID == "" || persisted.Status.DecisionAudit.Payload.Decision != winner {
		t.Fatalf("decision audit outbox not persisted atomically: %#v", persisted.Status.DecisionAudit)
	}
	if _, err := st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "decision-race", persisted.ResourceVersion, "wrong-audit-id"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong decision audit ID error = %v, want conflict", err)
	}
	acknowledged, err := st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "decision-race", persisted.ResourceVersion, persisted.Status.DecisionAudit.ID)
	if err != nil || !acknowledged.Status.DecisionEvidenceDelivered {
		t.Fatalf("acknowledge decision audit = %#v, %v", acknowledged.Status, err)
	}
	retried, err := st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "decision-race", persisted.ResourceVersion, persisted.Status.DecisionAudit.ID)
	if err != nil || retried.ResourceVersion != acknowledged.ResourceVersion {
		t.Fatalf("idempotent acknowledgement retry = rv %d, err %v", retried.ResourceVersion, err)
	}

	_, expired, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "expired-decision", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET expires_at = transaction_timestamp(), data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(transaction_timestamp())) WHERE workspace = 'team-a' AND invocation_id = 'expired-decision'`); err != nil {
		t.Fatal(err)
	}
	expiredResult, err := st.DecideRuntimeApproval(ctx, "team-a", "expired-decision", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: expired.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-1",
	})
	if err != nil || expiredResult.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
		t.Fatalf("decision at expiry = %q, err %v", expiredResult.Status.DecisionState, err)
	}
	if expiredResult.Status.DecidedByIssuer != "https://issuer.example" || expiredResult.Status.DecidedBySubject != "operator-1" {
		t.Fatalf("expired decision actor = %q/%q", expiredResult.Status.DecidedByIssuer, expiredResult.Status.DecidedBySubject)
	}
	retry, err := st.ExpireRuntimeApprovalDecision(ctx, "team-a", "expired-decision", expired.ResourceVersion)
	if err != nil || retry.Status.DecisionState != api.RuntimeApprovalDecisionExpired || retry.ResourceVersion != expiredResult.ResourceVersion {
		t.Fatalf("expiry retry changed winner: %#v, err %v", retry, err)
	}
}

func TestRuntimeApprovalDecisionActorBounds(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	_, approval, err := st.CreateOrGetRuntimeApproval(context.Background(), runtimeApprovalFixture(t, "actor-bounds", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DecideRuntimeApproval(context.Background(), "team-a", "actor-bounds", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: string(make([]byte, 513))})
	if err == nil {
		t.Fatal("oversized decision actor was accepted")
	}
}

func TestRuntimeApprovalCreateRejectsCallerStatusAndUntrustedKeys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "status-forgery", api.RuntimeApprovalDecisionPending)
	proposal.Status.DecidedBySubject = "forged"
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("caller-owned decision status was accepted")
	}
	proposal.Status = runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionPending)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("missing key store error = %v", err)
	}
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	proposal.Spec.Binding.KeyVersion = 2
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("stale key version was accepted for a new write")
	}
	forged := runtimeApprovalFixture(t, "forged-binding", api.RuntimeApprovalDecisionPending)
	forged.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, forged, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("forged binding tag was accepted for a new write")
	}
}

func TestRuntimeApprovalPersistsLargePositiveKeyIdentifier(t *testing.T) {
	const version = int64(^uint64(0) >> 1)
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version: version, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	if err := st.ActivateRuntimeApprovalBindingKeys(context.Background(), keys); err != nil {
		t.Fatal(err)
	}
	proposal := runtimeApprovalFixture(t, "large-key-identifier", api.RuntimeApprovalDecisionPending)
	binding, err := runtimeapproval.Bind(context.Background(), keys, runtimeapproval.ActionEnvelopeFromApproval(proposal, runtimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	proposal.Spec.Binding = binding
	created, approval, err := st.CreateOrGetRuntimeApproval(context.Background(), proposal, runtimeApprovalPayload, time.Minute)
	if err != nil || !created || approval.Spec.Binding.KeyVersion != version {
		t.Fatalf("large key identifier create = created %v, version %d, error %v", created, approval.Spec.Binding.KeyVersion, err)
	}
}

func TestRuntimeApprovalReplayAuthenticatesRetainedVerifyOnlyKey(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "rotated-replay", api.RuntimeApprovalDecisionPending)
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("create before rotation = %v, %v", created, err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || created {
		t.Fatalf("replay after rotation = %v, %v", created, err)
	}
	fresh := runtimeApprovalFixture(t, "verify-only-new-write", api.RuntimeApprovalDecisionPending)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, fresh, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("verify-only key accepted for a new write")
	}
}

func TestRuntimeApprovalBindingKeyRotationRejectsRetainedEpochMutationAndSupportsThreeEpochs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	proposal := runtimeApprovalFixture(t, "three-epoch-replay", api.RuntimeApprovalDecisionPending)
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("create before rotations = %v, %v", created, err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	mutated, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")},
		{Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, mutated); err == nil {
		t.Fatal("retained key epoch mutation was accepted")
	}
	third, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")},
		{Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, third); err != nil {
		t.Fatalf("activate third binding-key epoch: %v", err)
	}
	if created, approval, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || created || approval.Spec.Binding.Epoch != 1 {
		t.Fatalf("epoch-one replay after third activation = created %v, epoch %d, error %v", created, approval.Spec.Binding.Epoch, err)
	}
}

func TestRuntimeApprovalBindingKeyReadinessRejectsMixedVersionsAndMaterial(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.RuntimeApprovalBindingKeysReady(ctx); err != nil {
		t.Fatalf("matching initial keys were not ready: %v", err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}

	oldReplica := New(testPool, st.logger)
	oldReplica.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	if err := oldReplica.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("old-version readiness error = %v", err)
	}
	materialMismatch, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("different-material-abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatchedReplica := New(testPool, st.logger)
	mismatchedReplica.ConfigureRuntimeApprovalBindingKeys(materialMismatch)
	if err := mismatchedReplica.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("material-mismatch readiness error = %v", err)
	}
}

func TestRuntimeApprovalDecideAndExpireRaceConvergesOnExpired(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "decide-expire-race", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET expires_at = transaction_timestamp(), data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(transaction_timestamp())) WHERE workspace = 'team-a' AND invocation_id = 'decide-expire-race'`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan api.RuntimeApproval, 2)
	errorsSeen := make(chan error, 2)
	go func() {
		<-start
		result, err := st.DecideRuntimeApproval(ctx, "team-a", "decide-expire-race", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: "operator-1"})
		results <- result
		errorsSeen <- err
	}()
	go func() {
		<-start
		result, err := st.ExpireRuntimeApprovalDecision(ctx, "team-a", "decide-expire-race", approval.ResourceVersion)
		results <- result
		errorsSeen <- err
	}()
	close(start)
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("Decide/Expire race error: %v", err)
		}
		if result := <-results; result.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
			t.Fatalf("Decide/Expire race state = %q", result.Status.DecisionState)
		}
	}
}

func TestRuntimeApprovalReadRejectsIndexedColumnDrift(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "column-drift", api.RuntimeApprovalDecisionPending)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET binding_tag = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' WHERE workspace = 'team-a' AND invocation_id = 'column-drift'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", "column-drift"); err == nil {
		t.Fatal("RuntimeApproval with mismatched indexed columns was returned")
	}
}

func TestPostgresStableUIDProjectionUniquenessAndAgentRecreation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM resources WHERE namespace = 'team-a' AND ((kind = 'Agent' AND name = 'agent-a') OR (kind = 'TrustProfile' AND name IN ('agent-a-profile', 'duplicate-profile')))`); err != nil {
		t.Fatal(err)
	}
	agent := api.Agent{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"}, ObjectMeta: api.ObjectMeta{Name: "agent-a", Namespace: "team-a", UID: "forged"}}
	agentData, _ := json.Marshal(agent)
	createdData, err := st.CreateAndGet(ctx, "Agent", "team-a", "agent-a", agentData)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(createdData, &agent); err != nil || agent.UID == "" || agent.UID == "forged" {
		t.Fatalf("standalone Agent UID was not row-backed: %#v, err %v", agent.ObjectMeta, err)
	}
	firstUID := agent.UID
	profile := api.TrustProfile{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "TrustProfile"},
		ObjectMeta: api.ObjectMeta{Name: "agent-a-profile", Namespace: "team-a"},
		Spec:       api.TrustProfileSpec{AgentRef: api.LocalObjectReference{Name: "agent-a"}, AgentIncarnationUID: firstUID, InitialScore: 50},
	}
	profileData, _ := json.Marshal(profile)
	if _, err := st.CreateAndGet(ctx, "TrustProfile", "team-a", profile.Name, profileData); err != nil {
		t.Fatal(err)
	}
	duplicate := profile
	duplicate.Name = "duplicate-profile"
	duplicateData, _ := json.Marshal(duplicate)
	if _, err := st.CreateAndGet(ctx, "TrustProfile", "team-a", duplicate.Name, duplicateData); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate TrustProfile error = %v, want already exists", err)
	}
	if err := st.Delete(ctx, "Agent", "team-a", "agent-a"); err != nil {
		t.Fatal(err)
	}
	agent.UID = ""
	agentData, _ = json.Marshal(agent)
	createdData, err = st.CreateAndGet(ctx, "Agent", "team-a", "agent-a", agentData)
	if err != nil {
		t.Fatalf("recreate Agent: %v", err)
	}
	var recreated api.Agent
	_ = json.Unmarshal(createdData, &recreated)
	if recreated.UID == firstUID {
		t.Fatalf("recreated Agent inherited UID %q", firstUID)
	}
	if _, err := runtimeapproval.NewResolver(st).Resolve(ctx, "team-a", "agent-a"); !errors.Is(err, runtimeapproval.ErrPolicyUnavailable) {
		t.Fatalf("resolver error = %v, want stale-profile rejection", err)
	}
}

var runtimeApprovalPayload = json.RawMessage(`{"argument":"sensitive"}`)

func runtimeApprovalFixture(t *testing.T, invocationID, decision string) api.RuntimeApproval {
	t.Helper()
	ref := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	approval := api.RuntimeApproval{
		Spec: api.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: invocationID, Plane: api.RuntimeApprovalPlaneTool, Operation: "invoke",
			AgentRef:         ref("Agent", "agent-a", "agent-uid"),
			AgentIdentityRef: ref("AgentIdentity", "agent-a", "identity-uid"),
			AgentInstanceRef: ref("AgentInstance", "agent-a-1", "instance-uid"),
			SessionRef:       "session-a", TargetRef: ref("Tool", "tool-a", "tool-uid"),
			AutonomyClassRef: api.RuntimeApprovalReference{Kind: "AutonomyClass", Scope: "Cluster", Name: "supervised", UID: "class-uid", ResourceVersion: 1},
		},
		Status: api.RuntimeApprovalStatus{DecisionState: decision, ExecutionState: api.RuntimeApprovalExecutionNotStarted},
	}
	binding, err := runtimeapproval.Bind(context.Background(), runtimeApprovalTestKeys(t), runtimeapproval.ActionEnvelopeFromApproval(approval, runtimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	approval.Spec.Binding = binding
	return approval
}

func runtimeApprovalTestKeys(t *testing.T) runtimeapproval.BindingKeyStore {
	t.Helper()
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func runtimeApprovalRotatedTestKeys(t *testing.T) runtimeapproval.BindingKeyStore {
	t.Helper()
	keys, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
