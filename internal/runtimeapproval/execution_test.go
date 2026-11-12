// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type toolPreparationLifecycleStore struct {
	reapErr        error
	reservationErr error
	receiptErr     error
	calls          []string
}

type expiredToolPreparationLifecycleStore struct {
	toolPreparationLifecycleStore
	reservations   []store.ToolCallReservation
	approval       api.RuntimeApproval
	conflictWinner *api.RuntimeApproval
	listLimit      int
	getCalls       int
}

type memoryPreparationApprovalStore struct {
	approval       api.RuntimeApproval
	conflictWinner *api.RuntimeApproval
	reapErr        error
	getCalls       int
	reapCalls      int
	events         *[]string
}

type expiredMemoryPreparationReceiptStore struct {
	receipts     []store.MemoryMutationReceipt
	listLimit    int
	releaseCalls int
	releaseToken string
	events       *[]string
}

func (s *memoryPreparationApprovalStore) GetRuntimeApprovalByInvocation(context.Context, string, string) (api.RuntimeApproval, error) {
	s.getCalls++
	if s.getCalls > 1 && s.conflictWinner != nil {
		return *s.conflictWinner, nil
	}
	return s.approval, nil
}

func (s *memoryPreparationApprovalStore) ReapRuntimeApprovalPreparation(context.Context, string, string, int64, string) (api.RuntimeApproval, error) {
	s.reapCalls++
	if s.events != nil {
		*s.events = append(*s.events, "reap")
	}
	if s.reapErr != nil {
		return api.RuntimeApproval{}, s.reapErr
	}
	return api.RuntimeApproval{Status: api.RuntimeApprovalStatus{
		ExecutionState: api.RuntimeApprovalExecutionFailed, ExecutionDisposition: ExecutionDispositionAbandonedPreparation,
	}}, nil
}

func (s *expiredMemoryPreparationReceiptStore) ListExpiredMemoryMutations(_ context.Context, limit int) ([]store.MemoryMutationReceipt, error) {
	s.listLimit = limit
	return s.receipts, nil
}

func (s *expiredMemoryPreparationReceiptStore) ReleasePreparedMemoryMutation(_ context.Context, _, _, preparationToken string) (bool, error) {
	s.releaseCalls++
	s.releaseToken = preparationToken
	if s.events != nil {
		*s.events = append(*s.events, "release")
	}
	return true, nil
}

func (s *expiredToolPreparationLifecycleStore) ListExpiredToolCallReservations(_ context.Context, limit int) ([]store.ToolCallReservation, error) {
	s.listLimit = limit
	return s.reservations, nil
}

func (s *expiredToolPreparationLifecycleStore) GetRuntimeApprovalByInvocation(context.Context, string, string) (api.RuntimeApproval, error) {
	s.getCalls++
	if s.getCalls > 1 && s.conflictWinner != nil {
		return *s.conflictWinner, nil
	}
	return s.approval, nil
}

func (s *expiredToolPreparationLifecycleStore) ReapRuntimeApprovalPreparation(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string) (api.RuntimeApproval, error) {
	return s.toolPreparationLifecycleStore.ReapRuntimeApprovalPreparation(ctx, workspace, invocationID, expectedResourceVersion, preparationToken)
}

func (s *toolPreparationLifecycleStore) ReapRuntimeApprovalPreparation(_ context.Context, workspace, invocationID string, expectedResourceVersion int64, preparationToken string) (api.RuntimeApproval, error) {
	s.calls = append(s.calls, "reap")
	if s.reapErr != nil {
		return api.RuntimeApproval{}, s.reapErr
	}
	return api.RuntimeApproval{
		ObjectMeta: api.ObjectMeta{Namespace: workspace, ResourceVersion: expectedResourceVersion + 1},
		Spec:       api.RuntimeApprovalSpec{InvocationID: invocationID},
		Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionFailed, ExecutionDisposition: ExecutionDispositionAbandonedPreparation},
	}, nil
}

func (s *toolPreparationLifecycleStore) ReleaseToolCallReservation(context.Context, string, string, string) (bool, error) {
	s.calls = append(s.calls, "reservation")
	return s.reservationErr == nil, s.reservationErr
}

func (s *toolPreparationLifecycleStore) ReleasePreparedToolInvocation(context.Context, string, string, string) (bool, error) {
	s.calls = append(s.calls, "receipt")
	return s.receiptErr == nil, s.receiptErr
}

func TestReapToolPreparationCleansExactPreparationAfterTerminalTransition(t *testing.T) {
	lifecycle := &toolPreparationLifecycleStore{}
	reaped, err := ReapToolPreparation(context.Background(), lifecycle, lifecycle, "team-a", "invocation-1", 7, "preparation-token-00000000000001")
	if err != nil {
		t.Fatalf("ReapToolPreparation() error = %v", err)
	}
	if reaped.Status.ExecutionDisposition != ExecutionDispositionAbandonedPreparation {
		t.Fatalf("ReapToolPreparation() disposition = %q, want %q", reaped.Status.ExecutionDisposition, ExecutionDispositionAbandonedPreparation)
	}
	wantCalls := []string{"reap", "reservation", "receipt"}
	if len(lifecycle.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
	}
	for index := range wantCalls {
		if lifecycle.calls[index] != wantCalls[index] {
			t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
		}
	}
}

func TestReapToolPreparationDoesNotCleanWhenTerminalTransitionLoses(t *testing.T) {
	lifecycle := &toolPreparationLifecycleStore{reapErr: store.ErrConflict}
	if _, err := ReapToolPreparation(context.Background(), lifecycle, lifecycle, "team-a", "invocation-1", 7, "preparation-token-00000000000001"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReapToolPreparation() error = %v, want ErrConflict", err)
	}
	if len(lifecycle.calls) != 1 || lifecycle.calls[0] != "reap" {
		t.Fatalf("calls = %v, want [reap]", lifecycle.calls)
	}
}

func TestReapToolPreparationRetainsReceiptWhenReservationCleanupIsAmbiguous(t *testing.T) {
	lifecycle := &toolPreparationLifecycleStore{reservationErr: errors.New("quota store unavailable")}
	if _, err := ReapToolPreparation(context.Background(), lifecycle, lifecycle, "team-a", "invocation-1", 7, "preparation-token-00000000000001"); err == nil {
		t.Fatal("ReapToolPreparation() error = nil, want cleanup error")
	}
	wantCalls := []string{"reap", "reservation"}
	if len(lifecycle.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
	}
	for index := range wantCalls {
		if lifecycle.calls[index] != wantCalls[index] {
			t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
		}
	}
}

func TestCleanupReapedToolPreparationCanResumeAfterTerminalTransition(t *testing.T) {
	lifecycle := &toolPreparationLifecycleStore{}
	if err := CleanupReapedToolPreparation(context.Background(), lifecycle, "team-a", "invocation-1", "preparation-token-00000000000001"); err != nil {
		t.Fatalf("CleanupReapedToolPreparation() error = %v", err)
	}
	wantCalls := []string{"reservation", "receipt"}
	if len(lifecycle.calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
	}
	for index := range wantCalls {
		if lifecycle.calls[index] != wantCalls[index] {
			t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
		}
	}
}

func TestReapExpiredToolPreparationsReapsBoundedCandidate(t *testing.T) {
	token := "preparation-token-00000000000001"
	lifecycle := &expiredToolPreparationLifecycleStore{
		reservations: []store.ToolCallReservation{{Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: token}},
		approval: api.RuntimeApproval{
			ObjectMeta: api.ObjectMeta{ResourceVersion: 7},
			Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionNotStarted, PreparationToken: token},
		},
	}
	reaped, err := ReapExpiredToolPreparations(context.Background(), lifecycle, 25)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredToolPreparations() = (%d, %v), want (1, nil)", reaped, err)
	}
	if lifecycle.listLimit != 25 {
		t.Fatalf("list limit = %d, want 25", lifecycle.listLimit)
	}
	wantCalls := []string{"reap", "reservation", "receipt"}
	if !equalStrings(lifecycle.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
	}
}

func TestReapExpiredToolPreparationsResumesCleanup(t *testing.T) {
	token := "preparation-token-00000000000001"
	lifecycle := &expiredToolPreparationLifecycleStore{
		reservations: []store.ToolCallReservation{{Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: token}},
		approval: api.RuntimeApproval{Status: api.RuntimeApprovalStatus{
			ExecutionState: api.RuntimeApprovalExecutionFailed, ExecutionDisposition: ExecutionDispositionAbandonedPreparation,
		}},
	}
	reaped, err := ReapExpiredToolPreparations(context.Background(), lifecycle, 10)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredToolPreparations() = (%d, %v), want (1, nil)", reaped, err)
	}
	if wantCalls := []string{"reservation", "receipt"}; !equalStrings(lifecycle.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
	}
}

func TestReapExpiredToolPreparationsPreservesClaimWinner(t *testing.T) {
	token := "preparation-token-00000000000001"
	winner := api.RuntimeApproval{Status: api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionClaimed}}
	lifecycle := &expiredToolPreparationLifecycleStore{
		toolPreparationLifecycleStore: toolPreparationLifecycleStore{reapErr: store.ErrConflict},
		reservations:                  []store.ToolCallReservation{{Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: token}},
		approval: api.RuntimeApproval{
			ObjectMeta: api.ObjectMeta{ResourceVersion: 7},
			Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionNotStarted, PreparationToken: token},
		},
		conflictWinner: &winner,
	}
	reaped, err := ReapExpiredToolPreparations(context.Background(), lifecycle, 10)
	if err != nil || reaped != 0 {
		t.Fatalf("ReapExpiredToolPreparations() = (%d, %v), want (0, nil)", reaped, err)
	}
	if wantCalls := []string{"reap"}; !equalStrings(lifecycle.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", lifecycle.calls, wantCalls)
	}
}

func TestReapExpiredToolPreparationsReturnsUnresolvedConflict(t *testing.T) {
	token := "preparation-token-00000000000001"
	lifecycle := &expiredToolPreparationLifecycleStore{
		toolPreparationLifecycleStore: toolPreparationLifecycleStore{reapErr: store.ErrConflict},
		reservations:                  []store.ToolCallReservation{{Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: token}},
		approval: api.RuntimeApproval{
			ObjectMeta: api.ObjectMeta{ResourceVersion: 7},
			Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionNotStarted, PreparationToken: token},
		},
	}
	if reaped, err := ReapExpiredToolPreparations(context.Background(), lifecycle, 10); reaped != 0 || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReapExpiredToolPreparations() = (%d, %v), want (0, ErrConflict)", reaped, err)
	}
}

func TestReapExpiredMemoryPreparationsTransitionsBeforeCleanup(t *testing.T) {
	token := "preparation-token-00000000000001"
	events := make([]string, 0, 2)
	approvals := &memoryPreparationApprovalStore{approval: api.RuntimeApproval{
		ObjectMeta: api.ObjectMeta{ResourceVersion: 7},
		Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionNotStarted, PreparationToken: token},
	}, events: &events}
	receipts := &expiredMemoryPreparationReceiptStore{receipts: []store.MemoryMutationReceipt{{
		Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: token,
	}}, events: &events}

	reaped, err := ReapExpiredMemoryPreparations(context.Background(), approvals, receipts, 25)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredMemoryPreparations() = (%d, %v), want (1, nil)", reaped, err)
	}
	if receipts.listLimit != 25 || approvals.reapCalls != 1 || receipts.releaseCalls != 1 || receipts.releaseToken != token {
		t.Fatalf("calls = (limit %d, reap %d, release %d, token %q), want (25, 1, 1, %q)", receipts.listLimit, approvals.reapCalls, receipts.releaseCalls, receipts.releaseToken, token)
	}
	if want := []string{"reap", "release"}; !equalStrings(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestReapExpiredMemoryPreparationsResumesCleanup(t *testing.T) {
	approvals := &memoryPreparationApprovalStore{approval: api.RuntimeApproval{Status: api.RuntimeApprovalStatus{
		ExecutionState: api.RuntimeApprovalExecutionFailed, ExecutionDisposition: ExecutionDispositionAbandonedPreparation,
	}}}
	receipts := &expiredMemoryPreparationReceiptStore{receipts: []store.MemoryMutationReceipt{{
		Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: "preparation-token-00000000000001",
	}}}

	reaped, err := ReapExpiredMemoryPreparations(context.Background(), approvals, receipts, 10)
	if err != nil || reaped != 1 || approvals.reapCalls != 0 || receipts.releaseCalls != 1 {
		t.Fatalf("result = (%d, %v), calls = (reap %d, release %d), want (1, nil, 0, 1)", reaped, err, approvals.reapCalls, receipts.releaseCalls)
	}
}

func TestReapExpiredMemoryPreparationsPreservesClaimWinner(t *testing.T) {
	token := "preparation-token-00000000000001"
	winner := api.RuntimeApproval{Status: api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionClaimed}}
	approvals := &memoryPreparationApprovalStore{
		approval: api.RuntimeApproval{
			ObjectMeta: api.ObjectMeta{ResourceVersion: 7},
			Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionNotStarted, PreparationToken: token},
		},
		conflictWinner: &winner,
		reapErr:        store.ErrConflict,
	}
	receipts := &expiredMemoryPreparationReceiptStore{receipts: []store.MemoryMutationReceipt{{
		Namespace: "team-a", InvocationID: "invocation-1", PreparationToken: token,
	}}}

	reaped, err := ReapExpiredMemoryPreparations(context.Background(), approvals, receipts, 10)
	if err != nil || reaped != 0 || receipts.releaseCalls != 0 {
		t.Fatalf("result = (%d, %v), release calls = %d, want (0, nil, 0)", reaped, err, receipts.releaseCalls)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestIsTerminalDecision(t *testing.T) {
	terminal := []string{
		api.RuntimeApprovalDecisionApproved,
		api.RuntimeApprovalDecisionRejected,
		api.RuntimeApprovalDecisionExpired,
		api.RuntimeApprovalDecisionNotRequired,
	}
	for _, state := range terminal {
		if !IsTerminalDecision(state) {
			t.Errorf("IsTerminalDecision(%q) = false, want true", state)
		}
	}
	nonTerminal := []string{api.RuntimeApprovalDecisionPending, "", "Bogus"}
	for _, state := range nonTerminal {
		if IsTerminalDecision(state) {
			t.Errorf("IsTerminalDecision(%q) = true, want false", state)
		}
	}
}

func TestIsTerminalExecution(t *testing.T) {
	if !IsTerminalExecution(api.RuntimeApprovalExecutionCompleted) {
		t.Errorf("Completed must be terminal")
	}
	if !IsTerminalExecution(api.RuntimeApprovalExecutionFailed) {
		t.Errorf("Failed must be terminal")
	}
	for _, state := range []string{
		api.RuntimeApprovalExecutionNotStarted,
		api.RuntimeApprovalExecutionClaimed,
		api.RuntimeApprovalExecutionUncertain,
		"",
		"Bogus",
	} {
		if IsTerminalExecution(state) {
			t.Errorf("IsTerminalExecution(%q) = true, want false", state)
		}
	}
}

func TestPermitsExecution(t *testing.T) {
	if !PermitsExecution(api.RuntimeApprovalDecisionApproved) {
		t.Errorf("Approved must permit execution")
	}
	if !PermitsExecution(api.RuntimeApprovalDecisionNotRequired) {
		t.Errorf("NotRequired must permit execution")
	}
	for _, state := range []string{
		api.RuntimeApprovalDecisionPending,
		api.RuntimeApprovalDecisionRejected,
		api.RuntimeApprovalDecisionExpired,
		"",
	} {
		if PermitsExecution(state) {
			t.Errorf("PermitsExecution(%q) = true, want false", state)
		}
	}
}

func TestGeneratePreparationToken_UniqueAndBounded(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 64; i++ {
		token, err := GeneratePreparationToken()
		if err != nil {
			t.Fatalf("GeneratePreparationToken() error = %v", err)
		}
		if err := ValidatePreparationToken(token); err != nil {
			t.Fatalf("ValidatePreparationToken(%q) = %v", token, err)
		}
		if _, exists := seen[token]; exists {
			t.Fatalf("GeneratePreparationToken returned duplicate token %q", token)
		}
		seen[token] = struct{}{}
	}
}

func TestValidatePreparationToken_BoundEnforcement(t *testing.T) {
	if err := ValidatePreparationToken(""); err == nil {
		t.Errorf("empty preparation token must be rejected")
	}
	if err := ValidatePreparationToken("shortish"); err == nil {
		t.Errorf("short preparation token must be rejected")
	}
	oversized := make([]byte, 129)
	for i := range oversized {
		oversized[i] = 'a'
	}
	if err := ValidatePreparationToken(string(oversized)); err == nil {
		t.Errorf("oversized preparation token must be rejected")
	}
}

func TestValidatePreparationDeadline(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		deadline time.Time
		wantErr  bool
	}{
		{"zero", time.Time{}, true},
		{"before now", now.Add(-time.Second), true},
		{"equal to now", now, true},
		{"within cap", now.Add(2 * time.Minute), false},
		{"exactly cap", now.Add(MaxPreparationDeadline), false},
		{"beyond cap", now.Add(MaxPreparationDeadline + time.Second), true},
	}
	for _, tt := range tests {
		err := ValidatePreparationDeadline(now, tt.deadline)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestValidatePreDispatchDisposition(t *testing.T) {
	allowed := []string{
		ExecutionDispositionQuotaDenied,
		ExecutionDispositionReceiptConflict,
		ExecutionDispositionPolicyChanged,
		ExecutionDispositionTargetChanged,
		ExecutionDispositionSessionChanged,
		ExecutionDispositionPreparationRejected,
		ExecutionDispositionApprovalExpiredBeforeClaim,
		ExecutionDispositionAbandonedPreparation,
	}
	for _, d := range allowed {
		if err := ValidatePreDispatchDisposition(d); err != nil {
			t.Errorf("ValidatePreDispatchDisposition(%q) = %v, want nil", d, err)
		}
	}
	forbidden := []string{
		"",
		"Succeeded",
		"Failed",
		ExecutionDispositionReconciledCompleted,
		ExecutionDispositionReconciledNotExecuted,
		"Bogus",
	}
	for _, d := range forbidden {
		err := ValidatePreDispatchDisposition(d)
		if err == nil {
			t.Errorf("ValidatePreDispatchDisposition(%q) accepted invalid value", d)
			continue
		}
		if !errors.Is(err, ErrPreDispatchDispositionInvalid) {
			t.Errorf("ValidatePreDispatchDisposition(%q) = %v, want ErrPreDispatchDispositionInvalid", d, err)
		}
	}
}

func TestValidateReconciliationDisposition(t *testing.T) {
	if err := ValidateReconciliationDisposition(ExecutionDispositionReconciledCompleted); err != nil {
		t.Errorf("ReconciledCompleted rejected: %v", err)
	}
	if err := ValidateReconciliationDisposition(ExecutionDispositionReconciledNotExecuted); err != nil {
		t.Errorf("ReconciledNotExecuted rejected: %v", err)
	}
	for _, d := range []string{"", ExecutionDispositionSucceeded, ExecutionDispositionQuotaDenied} {
		if err := ValidateReconciliationDisposition(d); !errors.Is(err, ErrReconciliationDispositionInvalid) {
			t.Errorf("ValidateReconciliationDisposition(%q) = %v, want ErrReconciliationDispositionInvalid", d, err)
		}
	}
}

func TestValidateTerminalDisposition(t *testing.T) {
	if err := ValidateTerminalDisposition(ExecutionDispositionSucceeded); err != nil {
		t.Errorf("Succeeded rejected: %v", err)
	}
	if err := ValidateTerminalDisposition(ExecutionDispositionFailed); err != nil {
		t.Errorf("Failed rejected: %v", err)
	}
	for _, d := range []string{"", ExecutionDispositionReconciledCompleted, ExecutionDispositionQuotaDenied} {
		if err := ValidateTerminalDisposition(d); !errors.Is(err, ErrTerminalDispositionInvalid) {
			t.Errorf("ValidateTerminalDisposition(%q) = %v, want ErrTerminalDispositionInvalid", d, err)
		}
	}
}

func TestValidateClaimant(t *testing.T) {
	valid := api.RuntimeApprovalClaimant{
		WorkloadUID:       "gateway-tool-1",
		ProcessStartNonce: "0123456789abcdef",
		ClaimedAt:         time.Now().UTC(),
		LastHeartbeat:     time.Now().UTC(),
	}
	if err := ValidateClaimant(valid); err != nil {
		t.Fatalf("ValidateClaimant(valid) = %v", err)
	}

	// Every mutated field must be rejected.
	tests := []struct {
		name  string
		patch func(*api.RuntimeApprovalClaimant)
	}{
		{"empty workload", func(c *api.RuntimeApprovalClaimant) { c.WorkloadUID = "" }},
		{"short nonce", func(c *api.RuntimeApprovalClaimant) { c.ProcessStartNonce = "short" }},
		{"zero claimedAt", func(c *api.RuntimeApprovalClaimant) { c.ClaimedAt = time.Time{} }},
		{"zero heartbeat", func(c *api.RuntimeApprovalClaimant) { c.LastHeartbeat = time.Time{} }},
	}
	for _, tt := range tests {
		bad := valid
		tt.patch(&bad)
		if err := ValidateClaimant(bad); err == nil {
			t.Errorf("%s: ValidateClaimant accepted invalid value", tt.name)
		}
	}

	// LeaseDeadline before claimedAt must be rejected.
	past := valid.ClaimedAt.Add(-time.Second)
	bad := valid
	bad.LeaseDeadline = &past
	if err := ValidateClaimant(bad); err == nil {
		t.Errorf("lease deadline before claimedAt accepted")
	}
}
