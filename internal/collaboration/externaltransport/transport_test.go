// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/externalagent"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	testNS     = "team"
	testToken  = "s3cr3t-bearer-token"
	testDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
)

// fakeStore serves the resources the Gate reads.
type fakeStore struct {
	store.Store
	mu    sync.Mutex
	items map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{items: map[string][]byte{}} }

func storeKey(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }

func (s *fakeStore) put(t *testing.T, kind, name string, obj any) {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[storeKey(kind, testNS, name)] = raw
}

func (s *fakeStore) del(kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, storeKey(kind, testNS, name))
}

func (s *fakeStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.items[storeKey(kind, namespace, name)]
	if !ok {
		return nil, store.ErrNotFound
	}
	return raw, nil
}

func (s *fakeStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := kind + "/" + opts.Namespace + "/"
	var keys []string
	for key := range s.items {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	out := make([][]byte, 0, len(keys))
	for _, key := range keys {
		out = append(out, s.items[key])
	}
	return out, nil
}

// auditSpy records audit entries and can fail on demand.
type auditSpy struct {
	mu      sync.Mutex
	entries []observability.AuditEntry
	fail    error
}

func (a *auditSpy) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return nil, nil
}

func (a *auditSpy) Append(_ context.Context, e observability.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditSpy) AppendBatch(ctx context.Context, es []observability.AuditEntry) error {
	for _, e := range es {
		if err := a.Append(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (a *auditSpy) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

// decisions returns the audited trust decisions in order.
func (a *auditSpy) decisions(t *testing.T) []observability.TrustDecisionV1 {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]observability.TrustDecisionV1, 0, len(a.entries))
	for _, e := range a.entries {
		var d observability.TrustDecisionV1
		if err := json.Unmarshal(e.Details, &d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func (a *auditSpy) text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, _ := json.Marshal(a.entries)
	return string(raw)
}

type sentCall struct {
	call      Call
	messageID string
	text      string
}

// fakeClient is a scripted remote agent.
type fakeClient struct {
	mu         sync.Mutex
	sends      []sentCall
	cancels    []string
	taskID     string
	task       *a2a.Task
	sendErr    error
	getErr     error
	beforeSend func()
}

func (c *fakeClient) Send(_ context.Context, call Call, messageID, text string) (string, error) {
	c.mu.Lock()
	hook := c.beforeSend
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return "", c.sendErr
	}
	c.sends = append(c.sends, sentCall{call: call, messageID: messageID, text: text})
	return c.taskID, nil
}

func (c *fakeClient) GetTask(_ context.Context, _ Call, _ string) (*a2a.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getErr != nil {
		return nil, c.getErr
	}
	return c.task, nil
}

func (c *fakeClient) Cancel(_ context.Context, _ Call, taskID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancels = append(c.cancels, taskID)
	return nil
}

func (c *fakeClient) sent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sends)
}

type fakeCreds map[string]string

func (f fakeCreds) ResolveAPIKey(_ context.Context, _, name string) (string, error) {
	v, ok := f[name]
	if !ok {
		return "", errors.New("no such credential")
	}
	return v, nil
}

type stubResolver struct {
	mu    sync.Mutex
	addrs []netip.Addr
	err   error
}

func (s *stubResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addrs, s.err
}

// writeOnceHandles matches the production ledger: Put never rebinds a recorded
// handle to a different value, so state changes must go through Replace.
type writeOnceHandles struct{ *a2atransport.MemoryHandles }

func (h writeOnceHandles) Put(ctx context.Context, dispatch, value string) error {
	if cur, ok, _ := h.Get(ctx, dispatch); ok && cur != value {
		return errors.New("already has a different remote task")
	}
	return h.MemoryHandles.Put(ctx, dispatch, value)
}

type fixture struct {
	t        *testing.T
	st       *fakeStore
	audit    *auditSpy
	client   *fakeClient
	handles  writeOnceHandles
	creds    fakeCreds
	resolver *stubResolver
	limiter  *Limiter
	allow    string
	now      time.Time
	owner    string
	runName  string
	limits   *api.ExternalAgentLimits
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t: t, st: newFakeStore(), audit: &auditSpy{}, client: &fakeClient{taskID: "remote-1"},
		handles: writeOnceHandles{&a2atransport.MemoryHandles{}}, creds: fakeCreds{"agent-cred": testToken},
		resolver: &stubResolver{addrs: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
		limiter:  NewLimiter(), allow: "agent.example.com:443",
		now:   time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC),
		owner: "Workflow", runName: "wf",
	}
	f.putAgent()
	f.putWorkflow(map[string]string{"team": "blue"})
	f.putGrant("g1", api.ExternalAgentGrantSpec{
		ExternalAgentRef: api.LocalObjectReference{Name: "ext"},
		WorkflowRef:      api.LocalObjectReference{Name: "wf"},
		Verbs:            []api.ExternalAgentGrantVerb{api.ExternalAgentGrantVerbInvoke},
	})
	return f
}

func (f *fixture) putAgent() {
	f.st.put(f.t, kindExternalAgent, "ext", api.ExternalAgent{
		ObjectMeta: api.ObjectMeta{Name: "ext", Namespace: testNS},
		Spec: api.ExternalAgentSpec{
			Protocol: api.ExternalAgentProtocolA2A, Endpoint: "agent.example.com:443",
			Card:          api.ExternalAgentCard{Digest: testDigest},
			CredentialRef: api.LocalObjectReference{Name: "agent-cred"},
			Limits:        f.limits,
		},
	})
}

func (f *fixture) putWorkflow(labels map[string]string) {
	f.st.put(f.t, kindWorkflow, "wf", api.Workflow{ObjectMeta: api.ObjectMeta{Name: "wf", Namespace: testNS, Labels: labels}})
}

func (f *fixture) putGrant(name string, spec api.ExternalAgentGrantSpec) {
	f.st.put(f.t, kindExternalAgentGrant, name, api.ExternalAgentGrant{
		ObjectMeta: api.ObjectMeta{Name: name, Namespace: testNS}, Spec: spec,
	})
}

func (f *fixture) transport() *Transport {
	f.t.Helper()
	allow, err := externalagent.ParseAllowList(f.allow)
	if err != nil {
		f.t.Fatal(err)
	}
	tr, err := New(Config{
		Store: f.st,
		Run: controller.WorkflowCollaborationRun{
			Namespace: testNS, OwnerKind: f.owner, RunName: f.runName, Step: "s",
			Spec: &api.WorkflowCollaboration{},
		},
		Members: map[string]string{"remote": "ext"}, Allow: allow, Resolver: f.resolver,
		Credentials: f.creds, Client: f.client, Handles: f.handles,
		Audit:   &Auditor{Log: observability.NewCognitiveAuditLog(f.audit)},
		Limiter: f.limiter, Now: func() time.Time { return f.now },
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return tr
}

func startMessage(id string) collaboration.Message {
	return collaboration.Message{
		DispatchID: id, Kind: collaboration.DispatchStart, TaskID: "t1", Recipient: "remote", Turn: 1,
		Goal: collaboration.Content{Text: "summarize the incident"}, Allowance: 1000,
		AllowedOperations: []collaboration.OperationKind{collaboration.OpComplete},
	}
}

func cancelMessage(id string, target collaboration.Message) collaboration.Message {
	return collaboration.Message{DispatchID: id, Kind: collaboration.DispatchCancel, TaskID: "t1", Recipient: "remote", Target: &target}
}

func completedTask(text string) *a2a.Task {
	return &a2a.Task{
		ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{Parts: []*a2a.Part{a2a.NewTextPart(text)}}},
	}
}

func isRefusal(err error) bool { return errors.Is(err, driver.ErrRefused) }

func TestAuthorizedSendReachesRemoteWithCredentialAndAuditFirst(t *testing.T) {
	f := newFixture(t)
	var auditBeforeSend int
	f.client.beforeSend = func() { auditBeforeSend = f.audit.count() }
	tr := f.transport()
	msg := startMessage("d1")

	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if auditBeforeSend != 1 {
		t.Fatalf("audit records at the time of contact = %d, want 1 (write-ahead)", auditBeforeSend)
	}
	if f.client.sent() != 1 {
		t.Fatalf("sends = %d, want 1", f.client.sent())
	}
	got := f.client.sends[0]
	if got.call.Token.Reveal() != testToken || got.call.Host != "agent.example.com" || got.call.Port != 443 {
		t.Fatalf("call = %+v", got.call)
	}
	if got.call.CardDigest != testDigest || len(got.call.Addrs) != 1 {
		t.Fatalf("call must carry the pinned digest and resolved addresses: %+v", got.call)
	}
	if got.messageID != "d1" || !strings.Contains(got.text, "summarize the incident") {
		t.Fatalf("send = %+v", got)
	}
	decisions := f.audit.decisions(t)
	if len(decisions) != 1 || decisions[0].Plane != "communication" || decisions[0].Outcome != "allowed" {
		t.Fatalf("decisions = %+v", decisions)
	}
	if decisions[0].Authorization.GrantRef == nil || decisions[0].Authorization.GrantRef.Name != "g1" {
		t.Fatalf("decision must name the grant: %+v", decisions[0].Authorization)
	}
}

func TestCredentialNeverLeavesTheCallAndIsScrubbedFromErrors(t *testing.T) {
	f := newFixture(t)
	f.client.sendErr = fmt.Errorf("dial failed: Authorization: Bearer %s rejected", testToken)
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	err := tr.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected the client error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the credential: %v", err)
	}
	raw, _, _ := f.handles.Get(context.Background(), "d1")
	if strings.Contains(raw, testToken) || strings.Contains(f.audit.text(), testToken) {
		t.Fatal("credential reached the handle or the audit log")
	}
	secret := Secret(testToken)
	for _, rendered := range []string{secret.String(), fmt.Sprintf("%v %+v %#v", secret, secret, secret)} {
		if strings.Contains(rendered, testToken) {
			t.Fatalf("Secret printed its value: %s", rendered)
		}
	}
	if raw, _ := json.Marshal(struct{ T Secret }{secret}); strings.Contains(string(raw), testToken) {
		t.Fatalf("Secret marshaled its value: %s", raw)
	}
}

func TestMissingCredentialIsRefusedBeforeContact(t *testing.T) {
	f := newFixture(t)
	f.creds = fakeCreds{}
	tr := f.transport()
	err := tr.Admit(context.Background(), startMessage("d1"))
	if !isRefusal(err) {
		t.Fatalf("Admit = %v, want a refusal", err)
	}
	if f.client.sent() != 0 {
		t.Fatal("contacted the agent without a credential")
	}
}

func TestAmbiguousSendIsNeverResentAndSettlesAsUnknown(t *testing.T) {
	f := newFixture(t)
	f.client.sendErr = errors.New("connection reset")
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(context.Background(), msg); err == nil {
		t.Fatal("expected send error")
	}
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || obs.Settled {
		t.Fatalf("before the deadline the dispatch stays unsettled: %+v %v", obs, err)
	}
	f.now = f.now.Add(api.ExternalAgentDefaultTaskTimeout + time.Second)
	obs, err = tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Status != collaboration.ReportFailed || obs.Failure != FailureSendUnknown {
		t.Fatalf("after the deadline: %+v %v", obs, err)
	}
}

func TestGrantDecisions(t *testing.T) {
	invoke := []api.ExternalAgentGrantVerb{api.ExternalAgentGrantVerbInvoke}
	future := "2031-01-01T00:00:00Z"
	past := "2029-01-01T00:00:00Z"
	for name, tc := range map[string]struct {
		grants map[string]api.ExternalAgentGrantSpec
		reason string
	}{
		"no grant at all": {grants: nil, reason: reasonNoGrant},
		"grant for another agent": {
			grants: map[string]api.ExternalAgentGrantSpec{"g": {ExternalAgentRef: api.LocalObjectReference{Name: "other"}, WorkflowRef: api.LocalObjectReference{Name: "wf"}, Verbs: invoke}},
			reason: reasonNoGrant,
		},
		"grant for another workflow": {
			grants: map[string]api.ExternalAgentGrantSpec{"g": {ExternalAgentRef: api.LocalObjectReference{Name: "ext"}, WorkflowRef: api.LocalObjectReference{Name: "other"}, Verbs: invoke}},
			reason: reasonNoGrant,
		},
		"selector matches": {
			grants: map[string]api.ExternalAgentGrantSpec{"g": {ExternalAgentRef: api.LocalObjectReference{Name: "ext"}, WorkflowSelector: map[string]string{"team": "blue"}, Verbs: invoke}},
		},
		"selector mismatch": {
			grants: map[string]api.ExternalAgentGrantSpec{"g": {ExternalAgentRef: api.LocalObjectReference{Name: "ext"}, WorkflowSelector: map[string]string{"team": "red"}, Verbs: invoke}},
			reason: reasonNoGrant,
		},
		"window open": {
			grants: map[string]api.ExternalAgentGrantSpec{"g": {ExternalAgentRef: api.LocalObjectReference{Name: "ext"}, WorkflowRef: api.LocalObjectReference{Name: "wf"}, Verbs: invoke, Conditions: &api.ExternalAgentGrantConditions{ValidFrom: past, ValidUntil: future}}},
		},
		"window expired": {
			grants: map[string]api.ExternalAgentGrantSpec{"g": {ExternalAgentRef: api.LocalObjectReference{Name: "ext"}, WorkflowRef: api.LocalObjectReference{Name: "wf"}, Verbs: invoke, Conditions: &api.ExternalAgentGrantConditions{ValidUntil: past}}},
			reason: reasonNoGrant,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.st.del(kindExternalAgentGrant, "g1")
			for grantName, spec := range tc.grants {
				f.putGrant(grantName, spec)
			}
			err := f.transport().Admit(context.Background(), startMessage("d1"))
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("Admit = %v, want nil", err)
				}
				return
			}
			if !isRefusal(err) {
				t.Fatalf("Admit = %v, want a refusal", err)
			}
			decisions := f.audit.decisions(t)
			if len(decisions) != 1 || decisions[0].ReasonCode != tc.reason || decisions[0].Outcome != "denied" {
				t.Fatalf("decisions = %+v", decisions)
			}
			if f.client.sent() != 0 {
				t.Fatal("contacted the agent without a grant")
			}
		})
	}
}

func TestMissingExternalAgentOrGrantIsRefusedWithoutContact(t *testing.T) {
	for name, tc := range map[string]struct {
		remove func(f *fixture)
		want   string
	}{
		"missing ExternalAgent": {func(f *fixture) { f.st.del(kindExternalAgent, "ext") }, `ExternalAgent "ext" not found`},
		"missing grant":         {func(f *fixture) { f.st.del(kindExternalAgentGrant, "g1") }, `no ExternalAgentGrant lets Workflow "wf" invoke ExternalAgent "ext"`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			tc.remove(f)
			tr := f.transport()
			msg := startMessage("d1")
			err := tr.Admit(context.Background(), msg)
			if !isRefusal(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Admit = %v, want a refusal containing %q", err, tc.want)
			}
			if err := tr.Send(context.Background(), msg); err == nil {
				t.Fatal("Send succeeded without an authorized target")
			}
			if f.client.sent() != 0 {
				t.Fatal("contacted the agent")
			}
			for _, d := range f.audit.decisions(t) {
				if d.Outcome == "allowed" {
					t.Fatalf("an allowed decision was recorded: %+v", d)
				}
			}
		})
	}
}

func TestWorkflowRunIsGovernedByItsWorkflowTemplate(t *testing.T) {
	f := newFixture(t)
	f.owner, f.runName = "WorkflowRun", "wf-run-1"
	f.st.put(t, kindWorkflowRun, "wf-run-1", api.WorkflowRun{
		ObjectMeta: api.ObjectMeta{Name: "wf-run-1", Namespace: testNS},
		Spec:       api.WorkflowRunSpec{WorkflowRef: api.LocalObjectReference{Name: "wf"}},
	})
	if err := f.transport().Admit(context.Background(), startMessage("d1")); err != nil {
		t.Fatalf("Admit = %v, want the template's grant to apply", err)
	}
	f.st.del(kindWorkflowRun, "wf-run-1")
	if err := f.transport().Admit(context.Background(), startMessage("d2")); !isRefusal(err) {
		t.Fatalf("Admit without the run = %v, want a refusal", err)
	}
}

func TestGrantRevokedAfterAdmitStopsTheSend(t *testing.T) {
	f := newFixture(t)
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	f.st.del(kindExternalAgentGrant, "g1")
	err := tr.Send(context.Background(), msg)
	var denial *Denial
	if !errors.As(err, &denial) || denial.Outcome != outcomeRevoked || denial.Code != reasonRevoked {
		t.Fatalf("Send = %v, want a revoked denial", err)
	}
	if f.client.sent() != 0 {
		t.Fatal("contacted the agent after the grant was revoked")
	}
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Failure != FailureNotSent {
		t.Fatalf("Observe = %+v %v, want settled NotSent", obs, err)
	}
	last := f.audit.decisions(t)
	if got := last[len(last)-1]; got.Outcome != "failed" && got.Outcome != "revoked" {
		t.Fatalf("unexpected final decision %+v", got)
	}
}

func TestGrantRevokedMidRunSettlesAsRevoked(t *testing.T) {
	f := newFixture(t)
	f.client.task = &a2a.Task{ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	f.st.del(kindExternalAgentGrant, "g1")
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Failure != FailureAccessRevoked {
		t.Fatalf("Observe = %+v %v, want AccessRevoked", obs, err)
	}
	decisions := f.audit.decisions(t)
	if got := decisions[len(decisions)-1]; got.Outcome != outcomeRevoked || got.ReasonCode != reasonRevoked {
		t.Fatalf("final decision = %+v", got)
	}
}

func TestRequestSizeCap(t *testing.T) {
	f := newFixture(t)
	f.limits = &api.ExternalAgentLimits{MaxRequestBytes: 64}
	f.putAgent()
	if err := f.transport().Admit(context.Background(), startMessage("d1")); !isRefusal(err) {
		t.Fatalf("Admit = %v, want a refusal", err)
	}
}

func TestEgressAllowList(t *testing.T) {
	for name, allow := range map[string]string{
		"empty list permits nothing": "",
		"other host":                 "other.example.com:443",
		"other port":                 "agent.example.com:8443",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.allow = allow
			err := f.transport().Admit(context.Background(), startMessage("d1"))
			if !isRefusal(err) {
				t.Fatalf("Admit = %v, want a refusal", err)
			}
			if f.client.sent() != 0 {
				t.Fatal("contacted a host outside the allow-list")
			}
		})
	}
	f := newFixture(t)
	f.allow = "*.example.com:443"
	if err := f.transport().Admit(context.Background(), startMessage("d1")); err != nil {
		t.Fatalf("wildcard entry: %v", err)
	}
}

func TestForbiddenResolvedAddressIsRefused(t *testing.T) {
	for name, addrs := range map[string][]netip.Addr{
		"private":      {netip.MustParseAddr("10.0.0.5")},
		"metadata":     {netip.MustParseAddr("169.254.169.254")},
		"loopback v6":  {netip.MustParseAddr("::1")},
		"mixed answer": {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("192.168.1.10")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.resolver.addrs = addrs
			tr := f.transport()
			if err := tr.Admit(context.Background(), startMessage("d1")); !isRefusal(err) {
				t.Fatalf("Admit = %v, want a refusal", err)
			}
			if err := tr.Send(context.Background(), startMessage("d1")); err == nil {
				t.Fatal("Send must refuse a forbidden address")
			}
			if f.client.sent() != 0 {
				t.Fatal("contacted a forbidden address")
			}
		})
	}
}

func TestResolutionFailureLeavesTheDispatchUndecided(t *testing.T) {
	f := newFixture(t)
	f.resolver.addrs, f.resolver.err = nil, errors.New("temporary DNS failure")
	err := f.transport().Admit(context.Background(), startMessage("d1"))
	if err == nil || isRefusal(err) {
		t.Fatalf("Admit = %v, want a non-refusal error", err)
	}
}

func TestAuditFailureStopsContact(t *testing.T) {
	f := newFixture(t)
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	f.audit.fail = errors.New("audit store unavailable")
	if err := tr.Send(context.Background(), msg); err == nil {
		t.Fatal("Send must fail when the audit record cannot be written")
	}
	if f.client.sent() != 0 {
		t.Fatal("contacted the agent without a durable audit record")
	}
}

func TestMissingAuditLogFailsClosed(t *testing.T) {
	f := newFixture(t)
	_, err := New(Config{
		Store: f.st, Run: controller.WorkflowCollaborationRun{Namespace: testNS, OwnerKind: "Workflow", RunName: "wf", Spec: &api.WorkflowCollaboration{}},
		Resolver: f.resolver, Credentials: f.creds, Client: f.client, Handles: f.handles,
	})
	if err == nil {
		t.Fatal("New must require an audit log")
	}
}

func TestConcurrencyCapIsNotARefusalAndSlotsAreReleased(t *testing.T) {
	f := newFixture(t)
	f.limits = &api.ExternalAgentLimits{MaxConcurrentTasks: 1}
	f.putAgent()
	f.client.task = completedTask("done")
	tr := f.transport()
	first, second := startMessage("d1"), startMessage("d2")
	if err := tr.Admit(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	err := tr.Admit(context.Background(), second)
	if err == nil || isRefusal(err) {
		t.Fatalf("Admit over the cap = %v, want a retryable non-refusal", err)
	}
	if obs, err := tr.Observe(context.Background(), first); err != nil || !obs.Settled {
		t.Fatalf("Observe = %+v %v", obs, err)
	}
	if err := tr.Admit(context.Background(), second); err != nil {
		t.Fatalf("Admit after the slot was released: %v", err)
	}
}

func TestObserveInterpretsReplies(t *testing.T) {
	f := newFixture(t)
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Admit(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	f.client.task = &a2a.Task{ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	if obs, err := tr.Observe(context.Background(), msg); err != nil || obs.Settled {
		t.Fatalf("working task must stay unsettled: %+v %v", obs, err)
	}

	f.client.task = completedTask("the incident was caused by a bad deploy")
	f.client.task.Artifacts[0].Metadata = map[string]any{collaboration.UsageMetadataKey: map[string]any{"totalTokens": float64(900000)}}
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Status != collaboration.ReportCompleted || len(obs.Operations) != 1 {
		t.Fatalf("completed reply = %+v %v", obs, err)
	}
	if obs.Operations[0].Result != "the incident was caused by a bad deploy" {
		t.Fatalf("result = %q", obs.Operations[0].Result)
	}
	if !obs.TokensKnown || obs.TokensUsed > 1000 {
		t.Fatalf("usage must be capped at the allowance: %+v", obs)
	}
	last := f.audit.decisions(t)
	if got := last[len(last)-1]; got.Outcome != "completed" || got.Execution.Status != "completed" {
		t.Fatalf("final decision = %+v", got)
	}
}

func TestObserveRejectsUnsafeReplies(t *testing.T) {
	big := strings.Repeat("x", int(api.ExternalAgentDefaultMaxResponseBytes)+1)
	for name, task := range map[string]*a2a.Task{
		"binary part": {ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Artifacts: []*a2a.Artifact{{Parts: []*a2a.Part{a2a.NewRawPart([]byte("\x00\x01"))}}}},
		"oversized": completedTask(big),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.client.task = task
			tr := f.transport()
			msg := startMessage("d1")
			if err := tr.Admit(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			if err := tr.Send(context.Background(), msg); err != nil {
				t.Fatal(err)
			}
			obs, err := tr.Observe(context.Background(), msg)
			if err != nil || !obs.Settled || obs.Status != collaboration.ReportFailed || obs.Failure != a2atransport.FailureInvalidPayload {
				t.Fatalf("Observe = %+v %v", obs, err)
			}
		})
	}
}

func TestInputRequiredFailsTheMember(t *testing.T) {
	f := newFixture(t)
	f.client.task = &a2a.Task{ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	tr := f.transport()
	msg := startMessage("d1")
	_ = tr.Admit(context.Background(), msg)
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Failure != a2atransport.FailureInputRequired {
		t.Fatalf("Observe = %+v %v", obs, err)
	}
}

func TestTaskTimeoutCancelsTheRemoteTask(t *testing.T) {
	f := newFixture(t)
	f.client.task = &a2a.Task{ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	tr := f.transport()
	msg := startMessage("d1")
	_ = tr.Admit(context.Background(), msg)
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(api.ExternalAgentDefaultTaskTimeout + time.Second)
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Failure != FailureTimedOut {
		t.Fatalf("Observe = %+v %v", obs, err)
	}
	if len(f.client.cancels) != 1 || f.client.cancels[0] != "remote-1" {
		t.Fatalf("cancels = %v", f.client.cancels)
	}
}

func TestTransientObserveErrorsLeaveTheDispatchPolled(t *testing.T) {
	f := newFixture(t)
	f.client.getErr = errors.New("503")
	tr := f.transport()
	msg := startMessage("d1")
	_ = tr.Admit(context.Background(), msg)
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if obs, err := tr.Observe(context.Background(), msg); err == nil || obs.Settled {
		t.Fatalf("Observe = %+v %v, want an unsettled error", obs, err)
	}
	f.now = f.now.Add(api.ExternalAgentDefaultTaskTimeout + time.Second)
	obs, err := tr.Observe(context.Background(), msg)
	if err != nil || !obs.Settled || obs.Failure != FailureTimedOut {
		t.Fatalf("after the deadline: %+v %v", obs, err)
	}
}

func TestCancelAsksTheRemoteAgentEvenAfterRevocation(t *testing.T) {
	f := newFixture(t)
	f.client.task = &a2a.Task{ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	tr := f.transport()
	msg := startMessage("d1")
	_ = tr.Admit(context.Background(), msg)
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	f.st.del(kindExternalAgentGrant, "g1")
	cancel := cancelMessage("c1", msg)
	if err := tr.Admit(context.Background(), cancel); err != nil {
		t.Fatalf("Admit cancel: %v", err)
	}
	if err := tr.Send(context.Background(), cancel); err != nil {
		t.Fatalf("Send cancel: %v", err)
	}
	if len(f.client.cancels) != 1 {
		t.Fatalf("cancels = %v", f.client.cancels)
	}
	if obs, err := tr.Observe(context.Background(), cancel); err != nil || obs.Settled {
		t.Fatalf("cancel is unconfirmed while the task runs: %+v %v", obs, err)
	}
	f.client.task = &a2a.Task{ID: "remote-1", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}
	obs, err := tr.Observe(context.Background(), cancel)
	if err != nil || !obs.Settled || obs.Status != collaboration.ReportCancelled {
		t.Fatalf("cancel = %+v %v", obs, err)
	}
}

func TestCancelWithoutARemoteTaskIsANoOp(t *testing.T) {
	f := newFixture(t)
	tr := f.transport()
	msg := startMessage("d1")
	if err := tr.Send(context.Background(), cancelMessage("c1", msg)); err != nil {
		t.Fatalf("Send cancel: %v", err)
	}
	if len(f.client.cancels) != 0 {
		t.Fatalf("cancels = %v", f.client.cancels)
	}
}

func TestUnknownMemberIsRefused(t *testing.T) {
	f := newFixture(t)
	msg := startMessage("d1")
	msg.Recipient = "stranger"
	if err := f.transport().Admit(context.Background(), msg); !isRefusal(err) {
		t.Fatalf("Admit = %v, want a refusal", err)
	}
}

func TestConcurrentActivationsDoNotShareReservation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.limits = &api.ExternalAgentLimits{MaxConcurrentTasks: 1}
	f.putAgent()
	first, second := f.transport(), f.transport()
	second.cfg.Run.Step = "other"
	msg := startMessage("t1.1")
	if err := first.Admit(ctx, msg); err != nil {
		t.Fatalf("admit first activation: %v", err)
	}
	if err := first.Send(ctx, msg); err != nil {
		t.Fatalf("send first activation: %v", err)
	}
	if err := second.Admit(ctx, msg); !errors.Is(err, driver.ErrDeferred) {
		t.Fatalf("second activation should wait for the first, got %v", err)
	}
	f.client.task = completedTask("done")
	if obs, err := first.Observe(ctx, msg); err != nil || !obs.Settled {
		t.Fatalf("settle first activation: %+v, %v", obs, err)
	}
	if err := second.Admit(ctx, msg); err != nil {
		t.Fatalf("admit second activation after release: %v", err)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	now := time.Unix(1000, 0)
	first := l.Reserve("k", "a", 1, now, time.Minute)
	again := l.Reserve("k", "a", 1, now, time.Minute)
	if !first || !again {
		t.Fatal("reserving the same dispatch again must be idempotent")
	}
	if l.Reserve("k", "b", 1, now, time.Minute) {
		t.Fatal("cap exceeded")
	}
	if !l.Reserve("k", "b", 1, now.Add(2*time.Minute), time.Minute) {
		t.Fatal("an expired reservation must free its slot")
	}
	l.Release("k", "b")
	if !l.Reserve("k", "c", 1, now.Add(2*time.Minute), time.Minute) {
		t.Fatal("a released slot must be reusable")
	}
}
