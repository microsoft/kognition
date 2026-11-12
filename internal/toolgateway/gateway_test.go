// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type gatewayStore struct {
	data                  map[string][]byte
	events                []store.InstanceEvent
	receipts              map[string]store.ToolInvocationReceipt
	preparedReceipts      []store.ToolInvocationReceipt
	preparedReservations  []store.ToolCallReservation
	settledReservations   []store.ToolCallReservation
	releasedReservations  []store.ToolCallReservation
	cleanupCalls          []string
	prepareReceiptErr     error
	prepareReservationErr error
	releaseReceiptErr     error
	releaseReservationErr error
	settleReservationErr  error
	settleReservationOK   *bool
	completeReceiptErr    error
	onCreate              func(string, string, string, []byte) error
}

func (s *gatewayStore) Ping(context.Context) error { return nil }
func (s *gatewayStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	data, found := s.data[kind+"/"+namespace+"/"+name]
	if !found {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), data...), nil
}
func (*gatewayStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return nil, nil
}
func (s *gatewayStore) Create(_ context.Context, kind, namespace, name string, data []byte) error {
	key := kind + "/" + namespace + "/" + name
	if _, exists := s.data[key]; exists {
		return store.ErrAlreadyExists
	}
	s.data[key] = append([]byte(nil), data...)
	if s.onCreate != nil {
		return s.onCreate(kind, namespace, name, data)
	}
	return nil
}
func (*gatewayStore) Update(context.Context, string, string, string, []byte, int64) error {
	return nil
}
func (*gatewayStore) Delete(context.Context, string, string, string) error { return nil }
func (*gatewayStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (*gatewayStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (*gatewayStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (*gatewayStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (s *gatewayStore) AppendEvent(_ context.Context, _, _, _ string, event store.InstanceEvent) error {
	s.events = append(s.events, event)
	return nil
}
func (*gatewayStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}
func (s *gatewayStore) ClaimToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if s.receipts == nil {
		s.receipts = make(map[string]store.ToolInvocationReceipt)
	}
	key := receipt.Namespace + "/" + receipt.InvocationID
	if existing, found := s.receipts[key]; found {
		if existing.Binding != receipt.Binding {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		if existing.Status == store.ToolInvocationAwaitingApproval {
			existing.Status = store.ToolInvocationPending
			existing.CompletedAt = nil
			s.receipts[key] = existing
			return true, existing, nil
		}
		return false, existing, nil
	}
	s.receipts[key] = receipt
	return true, receipt, nil
}
func (s *gatewayStore) PrepareToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	s.preparedReceipts = append(s.preparedReceipts, receipt)
	if s.prepareReceiptErr != nil {
		return false, store.ToolInvocationReceipt{}, s.prepareReceiptErr
	}
	if s.receipts == nil {
		s.receipts = make(map[string]store.ToolInvocationReceipt)
	}
	key := receipt.Namespace + "/" + receipt.InvocationID
	if existing, found := s.receipts[key]; found {
		if existing.Binding != receipt.Binding || (existing.Status != store.ToolInvocationPending && existing.Status != store.ToolInvocationPrepared) {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		if existing.Status == store.ToolInvocationPrepared && existing.PreparationToken != receipt.PreparationToken {
			return false, store.ToolInvocationReceipt{}, store.ErrConflict
		}
		s.receipts[key] = receipt
		return true, existing, nil
	}
	s.receipts[key] = receipt
	return true, receipt, nil
}
func (s *gatewayStore) ReleasePreparedToolInvocation(_ context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	s.cleanupCalls = append(s.cleanupCalls, "receipt")
	if s.releaseReceiptErr != nil {
		return false, s.releaseReceiptErr
	}
	key := namespace + "/" + invocationID
	existing, found := s.receipts[key]
	if !found {
		return false, nil
	}
	if existing.Status != store.ToolInvocationPrepared || existing.PreparationToken != preparationToken {
		return false, store.ErrConflict
	}
	delete(s.receipts, key)
	return true, nil
}
func (s *gatewayStore) PrepareToolCallReservation(_ context.Context, reservation store.ToolCallReservation) (bool, error) {
	s.preparedReservations = append(s.preparedReservations, reservation)
	if s.prepareReservationErr != nil {
		return false, s.prepareReservationErr
	}
	return true, nil
}
func (s *gatewayStore) SettleToolCallReservation(_ context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	s.settledReservations = append(s.settledReservations, store.ToolCallReservation{Namespace: namespace, InvocationID: invocationID, PreparationToken: preparationToken})
	if s.settleReservationErr != nil {
		return false, s.settleReservationErr
	}
	if s.settleReservationOK != nil {
		return *s.settleReservationOK, nil
	}
	return true, nil
}
func (s *gatewayStore) ReleaseToolCallReservation(_ context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	s.releasedReservations = append(s.releasedReservations, store.ToolCallReservation{Namespace: namespace, InvocationID: invocationID, PreparationToken: preparationToken})
	s.cleanupCalls = append(s.cleanupCalls, "reservation")
	if s.releaseReservationErr != nil {
		return false, s.releaseReservationErr
	}
	return true, nil
}
func (s *gatewayStore) CompleteToolInvocation(_ context.Context, receipt store.ToolInvocationReceipt) error {
	if s.completeReceiptErr != nil {
		return s.completeReceiptErr
	}
	key := receipt.Namespace + "/" + receipt.InvocationID
	existing, found := s.receipts[key]
	if !found {
		return store.ErrNotFound
	}
	if existing.Binding != receipt.Binding {
		return store.ErrConflict
	}
	s.receipts[key] = receipt
	return nil
}

type gatewayAuthorizer struct {
	decision            AuthorizationDecision
	err                 error
	calls               int
	authorizeInvocation func(map[string]any) (AuthorizationDecision, error)
}

type legacyGatewayAuthorizer struct{}

func (*legacyGatewayAuthorizer) AuthorizeTool(context.Context, string, string, string, v1alpha1.ToolGrantVerb) (AuthorizationDecision, error) {
	return AuthorizationDecision{}, nil
}

func (a *gatewayAuthorizer) AuthorizeTool(context.Context, string, string, string, v1alpha1.ToolGrantVerb) (AuthorizationDecision, error) {
	a.calls++
	return a.decision, a.err
}

func (a *gatewayAuthorizer) AuthorizeToolInvocation(_ context.Context, _, _, _ string, input map[string]any) (AuthorizationDecision, error) {
	a.calls++
	if a.authorizeInvocation != nil {
		return a.authorizeInvocation(input)
	}
	return a.decision, a.err
}

type invocationGatewayAuthorizer struct {
	gatewayAuthorizer
	input map[string]any
}

func (a *invocationGatewayAuthorizer) AuthorizeToolInvocation(_ context.Context, _, _, _ string, input map[string]any) (AuthorizationDecision, error) {
	a.calls++
	a.input = input
	return a.decision, a.err
}

type gatewayQuota struct {
	err   error
	calls int
}

func (q *gatewayQuota) ReserveToolCall(context.Context, string) error {
	q.calls++
	return q.err
}

type gatewayExecutor struct {
	calls  int
	output map[string]any
	err    error
	before func()
}

type sequenceGatewayExecutor struct {
	calls int
	errs  []error
}

type blockingGatewayExecutor struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

func (e *blockingGatewayExecutor) Execute(_ context.Context, _ runtime.ToolExecutionRequest) (runtime.ToolExecutionResult, error) {
	e.calls++
	e.started <- struct{}{}
	<-e.release
	return runtime.ToolExecutionResult{Output: map[string]any{"updated": true}, ExecutionMode: "sandbox"}, nil
}

func (e *sequenceGatewayExecutor) Execute(_ context.Context, _ runtime.ToolExecutionRequest) (runtime.ToolExecutionResult, error) {
	e.calls++
	if len(e.errs) > 0 {
		err := e.errs[0]
		e.errs = e.errs[1:]
		if err != nil {
			return runtime.ToolExecutionResult{}, err
		}
	}
	return runtime.ToolExecutionResult{Output: map[string]any{"updated": true}, ExecutionMode: "sandbox"}, nil
}

func (e *gatewayExecutor) Execute(_ context.Context, _ runtime.ToolExecutionRequest) (runtime.ToolExecutionResult, error) {
	e.calls++
	if e.before != nil {
		e.before()
	}
	return runtime.ToolExecutionResult{Output: e.output, ExecutionMode: "sandbox", Duration: time.Millisecond}, e.err
}

type gatewayLedger struct {
	mu            sync.Mutex
	starts        []LedgerEntry
	completions   []LedgerEntry
	decisions     []LedgerEntry
	startErr      error
	completionErr error
	decisionErr   error
}

type auditGatewayStore struct {
	*gatewayStore
	entries []observability.AuditEntry
	err     error
}

func (s *auditGatewayStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *auditGatewayStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *auditGatewayStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

func (l *gatewayLedger) RecordStart(_ context.Context, entry LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts = append(l.starts, entry)
	return l.startErr
}

func (l *gatewayLedger) RecordCompletion(_ context.Context, entry LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.completions = append(l.completions, entry)
	return l.completionErr
}

func (l *gatewayLedger) RecordDecision(_ context.Context, entry LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.decisions = append(l.decisions, entry)
	return l.decisionErr
}

func testGateway(t *testing.T, authorizer Authorizer, quota *gatewayQuota, executor *gatewayExecutor, ledger *gatewayLedger, toolVersion int64) *Gateway {
	t.Helper()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: toolVersion}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := New(&gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}}, authorizer, quota, executor, zaptest.NewLogger(t), WithLedger(ledger), WithInvocationTracer(invocation.NewRecorder()))
	if err != nil {
		t.Fatal(err)
	}
	gateway.newID = func() string { return "invocation-1" }
	return gateway
}

func testRequest() Request {
	return Request{Namespace: "team-a", Principal: "planner", SubjectKind: "AgentInstance", SubjectName: "planner-r1", ToolName: "crm-update", ToolResourceVersion: 7, Input: map[string]any{"ticket": "42"}}
}

func TestGatewayInvokesOnlyAfterDurableAuthorizationRecord(t *testing.T) {
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/planner-crm", RoleRefs: []string{"team-a/tool-writer"}, PolicyRefs: []string{"team-a/agent-policy@7"}}}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}, before: func() {
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if len(ledger.starts) != 1 {
			t.Fatalf("executor ran before durable start record")
		}
	}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)

	response, err := gateway.Invoke(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if response.InvocationID != "invocation-1" || !response.Output["updated"].(bool) || executor.calls != 1 || quota.calls != 1 || authorizer.calls != 1 {
		t.Fatalf("response=%#v executor=%d quota=%d authorizer=%d", response, executor.calls, quota.calls, authorizer.calls)
	}
	if len(ledger.starts) != 1 || len(ledger.completions) != 1 {
		t.Fatalf("ledger starts=%#v completions=%#v", ledger.starts, ledger.completions)
	}
	if ledger.starts[0].GrantRef != "team-a/planner-crm" || ledger.starts[0].RoleRefs[0] != "team-a/tool-writer" || ledger.starts[0].PolicyRefs[0] != "team-a/agent-policy@7" || ledger.completions[0].Status != string(invocation.ResultSuccess) {
		t.Fatalf("ledger entries = %#v %#v", ledger.starts[0], ledger.completions[0])
	}
}

func TestGatewayPassesInputToInvocationAuthorizer(t *testing.T) {
	authorizer := &invocationGatewayAuthorizer{}
	gateway := testGateway(t, authorizer, &gatewayQuota{}, &gatewayExecutor{output: map[string]any{}}, &gatewayLedger{}, 7)
	request := testRequest()

	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if authorizer.calls != 1 || authorizer.input["ticket"] != "42" {
		t.Fatalf("invocation authorization calls=%d input=%#v", authorizer.calls, authorizer.input)
	}
}

// TC-0306: Tool decisions are durable before execution and denied operations never execute.
func TestStoreLedgerWritesCognitiveAuditBeforeToolExecution(t *testing.T) {
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	resourceStore := &gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}}
	auditStore := &auditGatewayStore{gatewayStore: &gatewayStore{data: map[string][]byte{}}}
	ledger := toolgatewayLedgerAdapter{StoreLedger: NewStoreLedgerWithAudit(resourceStore, observability.NewCognitiveAuditLog(auditStore))}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}, before: func() {
		if len(auditStore.entries) != 1 || auditStore.entries[0].EventType != observability.AuditToolCall {
			t.Fatalf("tool executor ran before CognitiveAuditLog entry: %#v", auditStore.entries)
		}
	}}
	gateway, err := New(resourceStore, &gatewayAuthorizer{}, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(&ledger))
	if err != nil {
		t.Fatal(err)
	}
	gateway.newID = func() string { return "audit-invocation" }
	if _, err := gateway.Invoke(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	if len(auditStore.entries) != 2 || auditStore.entries[1].Details == nil {
		t.Fatalf("audit entries = %#v, want start and completion", auditStore.entries)
	}
}

func TestStoreLedgerEmitsRedactedTrustDecisionsForAllowDenyAndReplay(t *testing.T) {
	for _, mode := range []string{"standalone", "kubernetes"} {
		t.Run(mode, func(t *testing.T) { testStoreLedgerTrustEvidence(t, mode) })
	}
}

func testStoreLedgerTrustEvidence(t *testing.T, mode string) {
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	storeWithAudit := &auditGatewayStore{gatewayStore: &gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}}}
	ledger := toolgatewayLedgerAdapter{StoreLedger: NewStoreLedger(storeWithAudit)}
	if mode == "kubernetes" {
		ledger.StoreLedger = NewStoreLedgerWithAudit(storeWithAudit, observability.NewCognitiveAuditLog(storeWithAudit))
	}
	authorizer := &gatewayAuthorizer{decision: AuthorizationDecision{GrantRef: "team-a/crm-grant", PolicyRefs: []string{"team-a/allow-policy@7"}}}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway, err := New(storeWithAudit, authorizer, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(&ledger))
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.InvocationID = "trust-allow"
	request.AgentIdentity = "spiffe://kognition.dev/workspace/team-a/agent/planner"
	request.SessionID = "session-1"
	request.Solution = "support"
	request.SolutionRev = 3
	request.RunID = "demo-run"
	if mode == "kubernetes" {
		request.CapabilityIssued = true
		request.CapabilityID = "verified-capability-id"
	}
	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	authorizer.err = errors.New("no matching ToolGrant")
	denied := request
	denied.InvocationID = "trust-denied"
	denied.CapabilityIssued = true
	if _, err := gateway.Invoke(context.Background(), denied); err == nil {
		t.Fatal("authorization denial was accepted")
	}
	authorizer.err = nil
	authorizer.authorizeInvocation = func(map[string]any) (AuthorizationDecision, error) {
		return AuthorizationDecision{GrantRef: "team-a/constrained-crm@9"}, ErrToolArgumentConstraintsDenied
	}
	argumentDenied := request
	argumentDenied.InvocationID = "trust-argument-denied"
	argumentDenied.Input = map[string]any{"ticket": "sensitive-ticket-value"}
	if _, err := gateway.Invoke(context.Background(), argumentDenied); err == nil {
		t.Fatal("argument constraint denial was accepted")
	}
	if executor.calls != 1 {
		t.Fatalf("executor calls = %d, want one allowed execution", executor.calls)
	}

	schema := loadToolTrustDecisionSchema(t)
	wantOutcomes := map[string]bool{"allowed": false, "completed": false, "replayed": false, "revoked": false}
	foundArgumentDenial := false
	for _, entry := range storeWithAudit.entries {
		if entry.EventType != observability.AuditToolCall {
			continue
		}
		var decision map[string]any
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			t.Fatalf("decode decision details: %v", err)
		}
		if err := schema.Validate(decision); err != nil {
			t.Fatalf("schema rejected Tool trust decision %s: %v", entry.ID, err)
		}
		if _, found := decision["input"]; found {
			t.Fatalf("Tool trust decision leaked input: %s", entry.Details)
		}
		identity, _ := decision["identity"].(map[string]any)
		session, _ := decision["session"].(map[string]any)
		capabilityID, _ := session["capabilityId"].(string)
		if capabilityID != request.CapabilityID {
			t.Fatalf("capability evidence=%q, want %q", capabilityID, request.CapabilityID)
		}
		if decision["runId"] != request.RunID || identity["agentIdentity"] != request.AgentIdentity || session["sessionId"] != request.SessionID || entry.AgentIdentity != request.AgentIdentity || entry.SessionID != request.SessionID || entry.Solution != request.Solution || entry.SolutionRev != request.SolutionRev {
			t.Fatalf("Tool trust decision lost indexed identity/session/solution evidence: entry=%#v decision=%s", entry, entry.Details)
		}
		outcome, _ := decision["outcome"].(string)
		if outcome == "allowed" || outcome == "completed" {
			approval, _ := decision["approval"].(map[string]any)
			if approval["status"] != "not_required" {
				t.Fatalf("incorrect approval evidence: %s", entry.Details)
			}
		}
		if _, found := wantOutcomes[outcome]; found {
			wantOutcomes[outcome] = true
		}
		if outcome == "revoked" && decision["reasonCode"] != "grant_revoked" {
			t.Fatalf("revoked Tool decision reason = %q", decision["reasonCode"])
		}
		if decision["reasonCode"] == "argument_constraints_denied" {
			foundArgumentDenial = true
			authorization, _ := decision["authorization"].(map[string]any)
			if authorization["grant"] != "denied" || authorization["policy"] != "not_evaluated" || strings.Contains(string(entry.Details), "sensitive-ticket-value") {
				t.Fatalf("argument denial evidence = %s", entry.Details)
			}
		}
	}
	for outcome, found := range wantOutcomes {
		if !found {
			t.Fatalf("missing %s trust decision: %#v", outcome, storeWithAudit.entries)
		}
	}
	if !foundArgumentDenial {
		t.Fatalf("missing argument constraint trust decision: %#v", storeWithAudit.entries)
	}
}

// TC-0085: A policy denial is durable before the failed receipt is completed.
func TestStoreLedgerDenialAuditFailureLeavesReceiptPending(t *testing.T) {
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	storeWithAudit := &auditGatewayStore{
		gatewayStore: &gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}},
		err:          errors.New("audit unavailable"),
	}
	ledger := toolgatewayLedgerAdapter{StoreLedger: NewStoreLedger(storeWithAudit)}
	executor := &gatewayExecutor{}
	gateway, err := New(storeWithAudit, &gatewayAuthorizer{err: errors.New("AgentPolicy denied tool")}, nil, executor, zaptest.NewLogger(t), WithLedger(&ledger))
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.InvocationID = "denial-audit-failure"
	if _, err := gateway.Invoke(context.Background(), request); err == nil || !strings.Contains(err.Error(), "record tool trust decision") {
		t.Fatalf("Invoke() error = %v, want durable decision failure", err)
	}
	receipt := storeWithAudit.receipts["team-a/denial-audit-failure"]
	if receipt.Status != store.ToolInvocationPending || executor.calls != 0 {
		t.Fatalf("receipt status=%q executor calls=%d, want pending/0", receipt.Status, executor.calls)
	}
}

func loadToolTrustDecisionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(filepath.Join("..", "..", "api", "schemas", "trust-decision-v1.schema.json"))
	if err != nil {
		t.Fatalf("compile trust decision schema: %v", err)
	}
	return schema
}

type toolgatewayLedgerAdapter struct {
	*StoreLedger
}

func TestGatewayFailsClosedBeforeExecution(t *testing.T) {
	tests := []struct {
		name       string
		authorizer *gatewayAuthorizer
		quota      *gatewayQuota
		ledger     *gatewayLedger
		request    Request
		want       error
	}{
		{name: "authorization denied", authorizer: &gatewayAuthorizer{err: errors.New("grant denied")}, quota: &gatewayQuota{}, ledger: &gatewayLedger{}, request: testRequest()},
		{name: "quota denied", authorizer: &gatewayAuthorizer{}, quota: &gatewayQuota{err: errors.New("quota exhausted")}, ledger: &gatewayLedger{}, request: testRequest()},
		{name: "version mismatch", authorizer: &gatewayAuthorizer{}, quota: &gatewayQuota{}, ledger: &gatewayLedger{}, request: Request{Namespace: "team-a", Principal: "planner", ToolName: "crm-update", ToolResourceVersion: 8}},
		{name: "write ahead audit unavailable", authorizer: &gatewayAuthorizer{}, quota: &gatewayQuota{}, ledger: &gatewayLedger{startErr: errors.New("audit unavailable")}, request: testRequest()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &gatewayExecutor{output: map[string]any{"updated": true}}
			gateway := testGateway(t, test.authorizer, test.quota, executor, test.ledger, 7)
			_, err := gateway.Invoke(context.Background(), test.request)
			if err == nil || executor.calls != 0 {
				t.Fatalf("error=%v executor calls=%d, want denial before execution", err, executor.calls)
			}
		})
	}
}

func TestGatewayRecordsExecutorFailureCompletion(t *testing.T) {
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{err: errors.New("endpoint unavailable")}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, executor, ledger, 7)
	_, err := gateway.Invoke(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected executor error")
	}
	if len(ledger.starts) != 1 || len(ledger.completions) != 1 || ledger.completions[0].Status != string(invocation.ResultError) {
		t.Fatalf("ledger starts=%#v completions=%#v", ledger.starts, ledger.completions)
	}
}

func TestGatewayReturnsStoredResponseForDuplicateInvocation(t *testing.T) {
	authorizer := &gatewayAuthorizer{}
	quota := &gatewayQuota{}
	ledger := &gatewayLedger{}
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway := testGateway(t, authorizer, quota, executor, ledger, 7)
	request := testRequest()
	request.InvocationID = "duplicate-1"
	first, err := gateway.Invoke(context.Background(), request)
	if err != nil || first.Output["updated"] != true {
		t.Fatalf("first invocation = %#v, %v", first, err)
	}
	second, err := gateway.Invoke(context.Background(), request)
	if err != nil || second.Output["updated"] != true {
		t.Fatalf("duplicate invocation = %#v, %v", second, err)
	}
	if executor.calls != 1 || quota.calls != 1 || authorizer.calls != 1 || len(ledger.starts) != 1 || len(ledger.completions) != 1 {
		t.Fatalf("duplicate executed: executor=%d quota=%d authorizer=%d starts=%d completions=%d", executor.calls, quota.calls, authorizer.calls, len(ledger.starts), len(ledger.completions))
	}
	if len(ledger.decisions) != 1 || ledger.decisions[0].Approval == nil || ledger.decisions[0].Approval.Status != "not_required" {
		t.Fatalf("disabled gate replay approval = %#v", ledger.decisions)
	}
}

func TestGatewayRejectsConflictingInvocationID(t *testing.T) {
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, &gatewayExecutor{output: map[string]any{"updated": true}}, &gatewayLedger{}, 7)
	request := testRequest()
	request.InvocationID = "conflict-1"
	if _, err := gateway.Invoke(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Input = map[string]any{"ticket": "43"}
	if _, err := gateway.Invoke(context.Background(), request); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting invocation error = %v, want store.ErrConflict", err)
	}
}

func TestGatewayRejectsDuplicateInvocationWhilePending(t *testing.T) {
	fixtureStore := &gatewayStore{data: map[string][]byte{}}
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	fixtureStore.data["Tool/team-a/crm-update"] = data
	executor := &gatewayExecutor{output: map[string]any{"updated": true}}
	gateway, err := New(fixtureStore, &gatewayAuthorizer{}, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(&gatewayLedger{}))
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.InvocationID = "pending-1"
	receipt := gateway.newReceipt(request, &tool)
	fixtureStore.receipts = map[string]store.ToolInvocationReceipt{receipt.Namespace + "/" + receipt.InvocationID: receipt}
	if _, err := gateway.Invoke(context.Background(), request); !errors.Is(err, ErrToolInvocationInProgress) {
		t.Fatalf("pending duplicate error = %v, want ErrToolInvocationInProgress", err)
	}
	if executor.calls != 0 {
		t.Fatalf("pending duplicate executor calls = %d, want 0", executor.calls)
	}
}

// TC-0295: The Kubernetes E2E extends this core policy check with live target
// failures, timeout receipts, circuit recovery, and Prometheus metrics.
func TestGatewayRetriesOnlyIdempotentTransientFailures(t *testing.T) {
	transient := &runtime.StructuredToolError{Code: "ToolTransportError", Message: "temporary", Retryable: true}
	executor := &sequenceGatewayExecutor{errs: []error{transient, nil}}
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{ExecutionPolicy: &v1alpha1.ToolExecutionPolicy{Idempotent: true, RetryMaxAttempts: 2}}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	st := &gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}}
	gateway, err := New(st, &gatewayAuthorizer{}, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(&gatewayLedger{}))
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.InvocationID = "retry-1"
	if _, err := gateway.Invoke(context.Background(), request); err != nil || executor.calls != 2 {
		t.Fatalf("Invoke() error=%v calls=%d, want retried success", err, executor.calls)
	}
}

func TestGatewayRejectsConcurrencyAndOpensCircuit(t *testing.T) {
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{ExecutionPolicy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1, CircuitBreaker: &v1alpha1.ToolCircuitBreaker{FailureThreshold: 1, Cooldown: "1m"}}}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	st := &gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}}
	executor := &sequenceGatewayExecutor{errs: []error{&runtime.StructuredToolError{Code: "ToolTransportError", Message: "temporary", Retryable: true}}}
	gateway, err := New(st, &gatewayAuthorizer{}, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(&gatewayLedger{}))
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.InvocationID = "circuit-1"
	if _, err := gateway.Invoke(context.Background(), request); err == nil {
		t.Fatal("expected transient failure")
	}
	request.InvocationID = "circuit-2"
	if _, err := gateway.Invoke(context.Background(), request); err == nil || !strings.Contains(err.Error(), ErrToolCircuitOpen.Error()) {
		t.Fatalf("circuit invocation error = %v, want open circuit", err)
	}
}

func TestGatewayRejectsConcurrentInvocationOverToolLimit(t *testing.T) {
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "crm-update", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{ExecutionPolicy: &v1alpha1.ToolExecutionPolicy{MaxConcurrent: 1}}}
	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	st := &gatewayStore{data: map[string][]byte{"Tool/team-a/crm-update": data}}
	executor := &blockingGatewayExecutor{started: make(chan struct{}, 1), release: make(chan struct{})}
	gateway, err := New(st, &gatewayAuthorizer{}, &gatewayQuota{}, executor, zaptest.NewLogger(t), WithLedger(&gatewayLedger{}))
	if err != nil {
		t.Fatal(err)
	}
	first := testRequest()
	first.InvocationID = "concurrent-1"
	done := make(chan error, 1)
	go func() { _, err := gateway.Invoke(context.Background(), first); done <- err }()
	<-executor.started
	second := testRequest()
	second.InvocationID = "concurrent-2"
	if _, err := gateway.Invoke(context.Background(), second); err == nil || !strings.Contains(err.Error(), ErrToolConcurrencyLimit.Error()) {
		t.Fatalf("concurrent invocation error = %v, want concurrency limit", err)
	}
	close(executor.release)
	if err := <-done; err != nil {
		t.Fatalf("first invocation error = %v", err)
	}
}

func TestGatewayReturnsNonRetryableErrorWhenCompletionAuditFails(t *testing.T) {
	ledger := &gatewayLedger{completionErr: errors.New("audit write unavailable")}
	gateway := testGateway(t, &gatewayAuthorizer{}, &gatewayQuota{}, &gatewayExecutor{output: map[string]any{"updated": true}}, ledger, 7)
	response, err := gateway.Invoke(context.Background(), testRequest())
	if err == nil || response.Output["updated"] != true {
		t.Fatalf("response=%#v error=%v, want visible completion audit failure after execution", response, err)
	}
	var structured *runtime.StructuredToolError
	if !errors.As(err, &structured) || structured.Code != "ToolAuditCompletionFailed" || structured.Retryable {
		t.Fatalf("completion audit error = %#v", err)
	}
}

func TestStoreLedgerPersistsLifecycleEvents(t *testing.T) {
	store := &gatewayStore{data: map[string][]byte{}}
	ledger := NewStoreLedger(store)
	entry := LedgerEntry{InvocationID: "invocation-1", Namespace: "team-a", SubjectKind: "AgentInstance", SubjectName: "planner-r1"}
	if err := ledger.RecordStart(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	entry.Status = "success"
	if err := ledger.RecordCompletion(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 2 || store.events[0].Reason != "ToolInvocationAuthorized" || store.events[1].Reason != "ToolInvocationCompleted" {
		t.Fatalf("store events = %#v", store.events)
	}
}
