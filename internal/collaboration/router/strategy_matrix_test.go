// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/collaboration/externaltransport"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/externalagent"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

const matrixNS = "team"

type matrixStore struct {
	store.Store
	items map[string][]byte
}

func (s *matrixStore) put(t *testing.T, kind, name string, obj any) {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	s.items[kind+"/"+matrixNS+"/"+name] = raw
}

func (s *matrixStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	raw, ok := s.items[kind+"/"+namespace+"/"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return raw, nil
}

func (s *matrixStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	var keys []string
	for key := range s.items {
		if strings.HasPrefix(key, kind+"/"+opts.Namespace+"/") {
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

type matrixAudit struct {
	mu sync.Mutex
	n  int
}

func (a *matrixAudit) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return nil, nil
}

func (a *matrixAudit) Append(context.Context, observability.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return nil
}

func (a *matrixAudit) AppendBatch(ctx context.Context, es []observability.AuditEntry) error {
	for _, e := range es {
		if err := a.Append(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

type matrixCreds struct{}

func (matrixCreds) ResolveAPIKey(context.Context, string, string) (string, error) {
	return "token", nil
}

type matrixResolver struct{}

func (matrixResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

var memberInPrompt = regexp.MustCompile(`member "([^"]+)"`)

// matrixRemote is a remote A2A agent: it accepts handoffs and answers every
// other message with plain text naming the member it was asked as.
type matrixRemote struct {
	mu      sync.Mutex
	prompts map[string]string // task -> prompt
	ids     map[string]int    // message ID -> sends
	usage   map[string]int64  // member -> reported tokens
	reject  map[string]bool   // member -> declines handoffs
}

func (r *matrixRemote) Send(_ context.Context, _ externaltransport.Call, messageID, text string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids[messageID]++
	task := "remote-" + messageID
	r.prompts[task] = text
	return task, nil
}

func (r *matrixRemote) GetTask(_ context.Context, _ externaltransport.Call, taskID string) (*a2a.Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prompt := r.prompts[taskID]
	member := memberInPrompt.FindStringSubmatch(prompt)[1]
	reply := "ext:" + member
	if strings.Contains(prompt, "ACCEPT") {
		reply = "ACCEPT"
		if r.reject[member] {
			reply = "REJECT"
		}
	}
	artifact := &a2a.Artifact{Parts: []*a2a.Part{a2a.NewTextPart(reply)}}
	if used := r.usage[member]; used > 0 {
		artifact.Metadata = map[string]any{collaboration.UsageMetadataKey: map[string]any{"totalTokens": float64(used)}}
	}
	return &a2a.Task{ID: a2a.TaskID(taskID), Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, Artifacts: []*a2a.Artifact{artifact}}, nil
}

func (r *matrixRemote) Cancel(context.Context, externaltransport.Call, string) error { return nil }

func (r *matrixRemote) sends() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ids)
}

// localTeam answers for Agent members and fails the test if it is asked to
// serve an external one.
type localTeam struct {
	t        *testing.T
	external map[string]bool
	behave   func(collaboration.Message) driver.Observation
	mu       sync.Mutex
	sent     map[string]collaboration.Message
}

func (l *localTeam) Admit(context.Context, collaboration.Message) error { return nil }

func (l *localTeam) Send(_ context.Context, m collaboration.Message) error {
	if l.external[m.Recipient] {
		l.t.Errorf("external member %q was dispatched through the internal transport", m.Recipient)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent[m.DispatchID] = m
	return nil
}

func (l *localTeam) Observe(_ context.Context, m collaboration.Message) (driver.Observation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.sent[m.DispatchID]; !ok {
		return driver.Observation{}, nil
	}
	return l.behave(m), nil
}

func done(m collaboration.Message, o ...collaboration.Operation) driver.Observation {
	return driver.Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportCompleted,
		Operations: o, TokensUsed: 10, TokensKnown: true}
}

func joined(m collaboration.Message) string {
	parts := make([]string, 0, len(m.Results))
	for _, r := range m.Results {
		parts = append(parts, r.Member+"="+r.Result.Text)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func delegate(id, to string) collaboration.Operation {
	return collaboration.Operation{Kind: collaboration.OpDelegate, OperationID: id, To: to, Goal: "goal-" + to}
}

func complete(text string) collaboration.Operation {
	return collaboration.Operation{Kind: collaboration.OpComplete, Result: text}
}

func finalize(text string) collaboration.Operation {
	return collaboration.Operation{Kind: collaboration.OpFinalize, Result: text}
}

type matrixCase struct {
	name        string
	strategy    api.CollaborationStrategy
	local       []string
	external    []string
	configure   func(*api.WorkflowCollaboration)
	edges       [][2]string
	behave      func(collaboration.Message) driver.Observation
	want        string
	delegations int32
	owner       string
}

func matrixCases() []matrixCase {
	supervisor := func(targets ...string) func(collaboration.Message) driver.Observation {
		return func(m collaboration.Message) driver.Observation {
			switch {
			case m.Recipient == "lead" && m.Kind == collaboration.DispatchStart:
				ops := make([]collaboration.Operation, 0, len(targets))
				for i, to := range targets {
					ops = append(ops, delegate(fmt.Sprintf("d%d", i), to))
				}
				return done(m, ops...)
			case m.Recipient == "lead":
				return done(m, finalize(joined(m)))
			}
			return done(m, complete("local:"+m.Recipient))
		}
	}
	parallel := func(m collaboration.Message) driver.Observation {
		if m.Recipient == "s" {
			return done(m, finalize(fmt.Sprintf("%d:%s", len(m.Results), joined(m))))
		}
		return done(m, complete("local:"+m.Recipient))
	}
	setSupervisor := func(c *api.WorkflowCollaboration) { c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"} }
	setPeer := func(c *api.WorkflowCollaboration) { c.Peer = &api.CollaborationPeer{Finalizer: "fin"} }

	return []matrixCase{
		{
			name: "supervisor/one external", strategy: api.CollaborationStrategySupervisor,
			local: []string{"lead", "a"}, external: []string{"x"}, configure: setSupervisor,
			edges:  [][2]string{{"lead", "a"}, {"lead", "x"}},
			behave: supervisor("a", "x"),
			want:   "a=local:a,x=ext:x", delegations: 2,
		},
		{
			name: "supervisor/mixed team", strategy: api.CollaborationStrategySupervisor,
			local: []string{"lead", "a"}, external: []string{"x", "y"}, configure: setSupervisor,
			edges:  [][2]string{{"lead", "a"}, {"lead", "x"}, {"lead", "y"}},
			behave: supervisor("a", "x", "y"),
			want:   "a=local:a,x=ext:x,y=ext:y", delegations: 3,
		},
		{
			name: "parallel/one external", strategy: api.CollaborationStrategyParallel,
			local: []string{"a", "s"}, external: []string{"x"},
			configure: func(c *api.WorkflowCollaboration) {
				c.Parallel = &api.CollaborationParallel{Synthesizer: "s", Branches: []api.CollaborationBranch{{Member: "a"}, {Member: "x"}}}
			},
			behave: parallel,
			want:   "2:a=local:a,x=ext:x",
		},
		{
			name: "parallel/mixed team", strategy: api.CollaborationStrategyParallel,
			local: []string{"a", "s"}, external: []string{"x", "y"},
			configure: func(c *api.WorkflowCollaboration) {
				c.Parallel = &api.CollaborationParallel{Synthesizer: "s", Branches: []api.CollaborationBranch{{Member: "a"}, {Member: "x"}, {Member: "y"}}}
			},
			behave: parallel,
			want:   "3:a=local:a,x=ext:x,y=ext:y",
		},
		{
			name: "handoff/external becomes owner", strategy: api.CollaborationStrategyHandoff,
			local: []string{"a"}, external: []string{"x"},
			configure: func(c *api.WorkflowCollaboration) {
				c.Handoff = &api.CollaborationHandoff{EntryMember: "a", MaxHandoffs: 2}
			},
			edges: [][2]string{{"a", "x"}},
			behave: func(m collaboration.Message) driver.Observation {
				if m.Kind == collaboration.DispatchPrepare {
					return driver.Observation{Settled: true, Actor: m.Recipient, Accepted: true}
				}
				return done(m, collaboration.Operation{Kind: collaboration.OpHandoff, OperationID: "h1", To: "x", Context: "ctx"})
			},
			want: "ext:x", owner: "x", delegations: 1,
		},
		{
			name: "handoff/mixed team", strategy: api.CollaborationStrategyHandoff,
			local: []string{"a", "b"}, external: []string{"x", "y"},
			configure: func(c *api.WorkflowCollaboration) {
				c.Handoff = &api.CollaborationHandoff{EntryMember: "a", MaxHandoffs: 2}
			},
			edges: [][2]string{{"a", "b"}, {"b", "x"}, {"b", "y"}},
			behave: func(m collaboration.Message) driver.Observation {
				switch {
				case m.Kind == collaboration.DispatchPrepare:
					return driver.Observation{Settled: true, Actor: m.Recipient, Accepted: true}
				case m.Recipient == "a":
					return done(m, collaboration.Operation{Kind: collaboration.OpHandoff, OperationID: "h1", To: "b", Context: "ctx"})
				}
				return done(m, collaboration.Operation{Kind: collaboration.OpHandoff, OperationID: "h2", To: "x", Context: "ctx2"})
			},
			want: "ext:x", owner: "x", delegations: 2,
		},
		{
			name: "peer/one external", strategy: api.CollaborationStrategyPeer,
			local: []string{"fin", "b"}, external: []string{"x"}, configure: setPeer,
			edges: [][2]string{{"fin", "b"}, {"b", "x"}},
			behave: func(m collaboration.Message) driver.Observation {
				start := m.Kind == collaboration.DispatchStart
				switch {
				case m.Recipient == "fin" && start:
					return done(m, delegate("d1", "b"))
				case m.Recipient == "fin":
					return done(m, finalize("final:"+m.Results[0].Result.Text))
				case start:
					return done(m, delegate("d2", "x"))
				}
				return done(m, complete("b+"+m.Results[0].Result.Text))
			},
			want: "final:b+ext:x", delegations: 2,
		},
		{
			name: "peer/mixed team", strategy: api.CollaborationStrategyPeer,
			local: []string{"fin", "b"}, external: []string{"x", "y"}, configure: setPeer,
			edges: [][2]string{{"fin", "b"}, {"fin", "y"}, {"b", "x"}},
			behave: func(m collaboration.Message) driver.Observation {
				start := m.Kind == collaboration.DispatchStart
				switch {
				case m.Recipient == "fin" && start:
					return done(m, delegate("d1", "b"), delegate("d3", "y"))
				case m.Recipient == "fin":
					return done(m, finalize("final:"+joined(m)))
				case start:
					return done(m, delegate("d2", "x"))
				}
				return done(m, complete("b+"+m.Results[0].Result.Text))
			},
			want: "final:b=b+ext:x,y=ext:y", delegations: 3,
		},
	}
}

type matrixRun struct {
	state   collaboration.State
	remote  *matrixRemote
	local   *localTeam
	audit   *matrixAudit
	externs int
}

func runMatrix(t *testing.T, tc matrixCase, usage map[string]int64, reject map[string]bool) matrixRun {
	t.Helper()
	c := &api.WorkflowCollaboration{Strategy: tc.strategy, Protocol: api.CollaborationProtocolA2A,
		Limits: api.CollaborationLimits{MaxTurns: 20, MaxMessages: 100, MaxDelegations: 10, MaxDelegationDepth: 3, MaxParallelTasks: 4, MaxTokens: 100000}}
	st := &matrixStore{items: map[string][]byte{}}
	st.put(t, "Workflow", "wf", api.Workflow{ObjectMeta: api.ObjectMeta{Name: "wf", Namespace: matrixNS}})
	external := map[string]bool{}
	for _, alias := range tc.local {
		c.Members = append(c.Members, agent(alias))
	}
	for _, alias := range tc.external {
		external[alias] = true
		c.Members = append(c.Members, remote(alias))
		name := alias + "-ext"
		st.put(t, "ExternalAgent", name, api.ExternalAgent{
			ObjectMeta: api.ObjectMeta{Name: name, Namespace: matrixNS},
			Spec: api.ExternalAgentSpec{
				Protocol: api.ExternalAgentProtocolA2A, Endpoint: "agent.example.com:443",
				Card:          api.ExternalAgentCard{Digest: "sha256:" + strings.Repeat("0", 64)},
				CredentialRef: api.LocalObjectReference{Name: "cred"},
			},
		})
		st.put(t, "ExternalAgentGrant", "g-"+alias, api.ExternalAgentGrant{
			ObjectMeta: api.ObjectMeta{Name: "g-" + alias, Namespace: matrixNS},
			Spec: api.ExternalAgentGrantSpec{
				ExternalAgentRef: api.LocalObjectReference{Name: name},
				WorkflowRef:      api.LocalObjectReference{Name: "wf"},
				Verbs:            []api.ExternalAgentGrantVerb{api.ExternalAgentGrantVerbInvoke},
			},
		})
	}
	for _, e := range tc.edges {
		c.AllowedInteractions = append(c.AllowedInteractions, api.CollaborationInteraction{From: e[0], To: e[1]})
	}
	tc.configure(c)

	allow, err := externalagent.ParseAllowList("agent.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	remoteAgent := &matrixRemote{prompts: map[string]string{}, ids: map[string]int{}, usage: usage, reject: reject}
	audit := &matrixAudit{}
	local := &localTeam{t: t, external: external, behave: tc.behave, sent: map[string]collaboration.Message{}}
	f := Factory{
		Internal: factoryFunc(func() driver.Transport { return local }),
		External: externaltransport.Factory{
			Store: st, Allow: allow, Resolver: matrixResolver{}, Credentials: matrixCreds{}, Client: remoteAgent,
			Audit: &externaltransport.Auditor{Log: observability.NewCognitiveAuditLog(audit)},
		},
	}
	run := controller.WorkflowCollaborationRun{Namespace: matrixNS, OwnerKind: "Workflow", RunName: "wf", Step: "s", Spec: c}
	tr, err := f.Transport(context.Background(), run, &a2atransport.MemoryHandles{})
	if err != nil {
		t.Fatal(err)
	}
	def, err := collaboration.NewDefinition(c)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := collaboration.New(def)
	if err != nil {
		t.Fatal(err)
	}
	d, err := driver.New(eng, driver.NewMemoryLedger(), tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Begin(context.Background(), "investigate"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		state, finished, err := d.Step(context.Background())
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if finished {
			return matrixRun{state: state, remote: remoteAgent, local: local, audit: audit, externs: len(tc.external)}
		}
	}
	t.Fatal("collaboration did not finish")
	return matrixRun{}
}

func TestStrategyMatrixWithExternalMembers(t *testing.T) {
	for _, tc := range matrixCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := runMatrix(t, tc, nil, nil)
			st := got.state
			if st.Phase != collaboration.PhaseSucceeded {
				t.Fatalf("phase = %s outcome %+v", st.Phase, st.Outcome)
			}
			if r := st.Result(); r == nil || r.Text != tc.want {
				t.Fatalf("result = %+v, want %q", r, tc.want)
			}
			if st.Counters.Delegations != tc.delegations {
				t.Fatalf("delegations = %d, want %d", st.Counters.Delegations, tc.delegations)
			}
			if tc.owner != "" && st.Owner != tc.owner {
				t.Fatalf("owner = %q, want %q", st.Owner, tc.owner)
			}

			external := map[string]bool{}
			for _, alias := range tc.external {
				external[alias] = true
			}
			wantSends := 0
			for _, d := range st.Dispatches {
				if external[d.Recipient] {
					wantSends++
				}
			}
			if wantSends == 0 || got.remote.sends() != wantSends {
				t.Fatalf("remote tasks = %d, want %d external dispatches", got.remote.sends(), wantSends)
			}
			for id, n := range got.remote.ids {
				if n != 1 {
					t.Fatalf("message %s sent %d times", id, n)
				}
			}
			for id, m := range got.local.sent {
				if external[m.Recipient] {
					t.Fatalf("dispatch %s to external %q reached the internal transport", id, m.Recipient)
				}
			}
			if got.audit.n < wantSends {
				t.Fatalf("audit entries = %d, want at least one per external dispatch (%d)", got.audit.n, wantSends)
			}
		})
	}
}

func caseNamed(t *testing.T, name string) matrixCase {
	t.Helper()
	for _, c := range matrixCases() {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no case %q", name)
	return matrixCase{}
}

func TestExternalTokenUseIsCappedAndUnknownIsChargedInFull(t *testing.T) {
	limit := api.ExternalAgentDefaultMaxTokensPerTask
	cases := []struct {
		name  string
		usage map[string]int64
		// charge is what an external dispatch with the given allowance costs.
		charge func(member string, allowance int64) int64
	}{
		{"reported", map[string]int64{"x": 25, "y": 1 << 40}, func(member string, allowance int64) int64 {
			if member == "x" {
				return 25
			}
			return min(allowance, limit)
		}},
		{"unreported", nil, func(_ string, allowance int64) int64 { return allowance }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runMatrix(t, caseNamed(t, "parallel/mixed team"), c.usage, nil)
			var want int64
			for _, d := range got.state.Dispatches {
				if d.Recipient == "x" || d.Recipient == "y" {
					want += c.charge(d.Recipient, d.Allowance)
				} else {
					want += 10
				}
			}
			if got.state.Counters.TokensUsed != want {
				t.Fatalf("tokens used = %d, want %d", got.state.Counters.TokensUsed, want)
			}
		})
	}
}

func TestRejectedHandoffToAnExternalMemberKeepsOwnership(t *testing.T) {
	tc := caseNamed(t, "handoff/external becomes owner")
	tc.behave = func(m collaboration.Message) driver.Observation {
		if m.Kind == collaboration.DispatchPrepare {
			return driver.Observation{Settled: true, Actor: m.Recipient, Accepted: true}
		}
		if m.Kind == collaboration.DispatchStart {
			return done(m, collaboration.Operation{Kind: collaboration.OpHandoff, OperationID: "h1", To: "x", Context: "ctx"})
		}
		return done(m, finalize("a kept the task"))
	}
	got := runMatrix(t, tc, nil, map[string]bool{"x": true})
	if got.state.Phase != collaboration.PhaseSucceeded || got.state.Result().Text != "a kept the task" || got.state.Owner != "a" {
		t.Fatalf("phase=%s owner=%q result=%+v outcome=%+v", got.state.Phase, got.state.Owner, got.state.Result(), got.state.Outcome)
	}
}
