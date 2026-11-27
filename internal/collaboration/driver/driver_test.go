// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package driver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
)

var bg = context.Background()

// scripted is a Transport whose members answer from a script.
type scripted struct {
	mu       sync.Mutex
	behave   func(collaboration.Message) Observation
	refuse   func(collaboration.Message) bool
	failSend func(collaboration.Message) bool
	hideAll  bool // Observe reports nothing settled
	sends    map[string]int
	sent     map[string]collaboration.Message
}

func newScripted(b func(collaboration.Message) Observation) *scripted {
	return &scripted{behave: b, sends: map[string]int{}, sent: map[string]collaboration.Message{}}
}

func (s *scripted) Admit(_ context.Context, m collaboration.Message) error {
	if s.refuse != nil && s.refuse(m) {
		return fmt.Errorf("%w: policy", ErrRefused)
	}
	return nil
}

func (s *scripted) Send(_ context.Context, m collaboration.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends[m.DispatchID]++
	if s.failSend != nil && s.failSend(m) {
		return errors.New("connection reset")
	}
	s.sent[m.DispatchID] = m
	return nil
}

func (s *scripted) Observe(_ context.Context, m collaboration.Message) (Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sent[m.DispatchID]; !ok || s.hideAll {
		return Observation{}, nil
	}
	return s.behave(m), nil
}

func done(m collaboration.Message, o ...collaboration.Operation) Observation {
	return Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportCompleted,
		Operations: o, TokensUsed: 10, TokensKnown: true}
}

func limits() api.CollaborationLimits {
	return api.CollaborationLimits{MaxTurns: 20, MaxMessages: 100, MaxDelegations: 10, MaxDelegationDepth: 3,
		MaxParallelTasks: 4, MaxTokens: 100000}
}

func collab(strategy api.CollaborationStrategy, members ...string) *api.WorkflowCollaboration {
	c := &api.WorkflowCollaboration{Strategy: strategy, Protocol: api.CollaborationProtocolA2A, Limits: limits()}
	for _, m := range members {
		c.Members = append(c.Members, api.CollaborationMember{Name: m, AgentRef: api.AgentReference{Name: m}})
	}
	return c
}

func allow(c *api.WorkflowCollaboration, pairs ...[2]string) {
	for _, p := range pairs {
		c.AllowedInteractions = append(c.AllowedInteractions, api.CollaborationInteraction{From: p[0], To: p[1]})
	}
}

func build(t *testing.T, c *api.WorkflowCollaboration, tr Transport) (*Driver, *MemoryLedger) {
	t.Helper()
	def, err := collaboration.NewDefinition(c)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := collaboration.New(def)
	if err != nil {
		t.Fatal(err)
	}
	led := NewMemoryLedger()
	d, err := New(eng, led, tr)
	if err != nil {
		t.Fatal(err)
	}
	return d, led
}

func finish(t *testing.T, d *Driver) collaboration.State {
	t.Helper()
	for i := 0; i < 50; i++ {
		st, finished, err := d.Step(bg)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if finished {
			return st
		}
	}
	t.Fatal("collaboration did not finish")
	return collaboration.State{}
}

func requireResult(t *testing.T, st collaboration.State, want string) {
	t.Helper()
	if st.Phase != collaboration.PhaseSucceeded {
		t.Fatalf("phase = %s outcome %+v", st.Phase, st.Outcome)
	}
	if r := st.Result(); r == nil || r.Text != want {
		t.Fatalf("result = %+v, want %q", r, want)
	}
}

func TestSupervisorRunsToCompletion(t *testing.T) {
	c := collab(api.CollaborationStrategySupervisor, "lead", "a", "b")
	c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
	allow(c, [2]string{"lead", "a"}, [2]string{"lead", "b"})
	tr := newScripted(func(m collaboration.Message) Observation {
		switch {
		case m.Recipient == "lead" && m.Kind == collaboration.DispatchStart:
			return done(m, collaboration.Operation{Kind: collaboration.OpDelegate, OperationID: "d1", To: "a", Goal: "ga"},
				collaboration.Operation{Kind: collaboration.OpDelegate, OperationID: "d2", To: "b", Goal: "gb"})
		case m.Recipient == "lead":
			var parts []string
			for _, r := range m.Results {
				parts = append(parts, r.Member+"="+r.Result.Text)
			}
			return done(m, collaboration.Operation{Kind: collaboration.OpFinalize, Result: strings.Join(parts, ",")})
		}
		return done(m, collaboration.Operation{Kind: collaboration.OpComplete, Result: "from-" + m.Recipient})
	})
	d, _ := build(t, c, tr)
	if err := d.Begin(bg, "investigate"); err != nil {
		t.Fatal(err)
	}
	st := finish(t, d)
	requireResult(t, st, "a=from-a,b=from-b")
	for id, n := range tr.sends {
		if n != 1 {
			t.Fatalf("dispatch %s sent %d times", id, n)
		}
	}
}

func TestParallelRunsBranchesThenSynthesizer(t *testing.T) {
	c := collab(api.CollaborationStrategyParallel, "a", "b", "s")
	c.Parallel = &api.CollaborationParallel{Synthesizer: "s", Branches: []api.CollaborationBranch{{Member: "a"}, {Member: "b"}}}
	tr := newScripted(func(m collaboration.Message) Observation {
		if m.Recipient == "s" {
			return done(m, collaboration.Operation{Kind: collaboration.OpFinalize, Result: fmt.Sprintf("synth(%d)", len(m.Results))})
		}
		return done(m, collaboration.Operation{Kind: collaboration.OpComplete, Result: m.Recipient})
	})
	d, _ := build(t, c, tr)
	if err := d.Begin(bg, "go"); err != nil {
		t.Fatal(err)
	}
	requireResult(t, finish(t, d), "synth(2)")
}

func TestHandoffTransfersOwnership(t *testing.T) {
	c := collab(api.CollaborationStrategyHandoff, "a", "b", "c")
	c.Handoff = &api.CollaborationHandoff{EntryMember: "a", MaxHandoffs: 2}
	allow(c, [2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"b", "a"})
	tr := newScripted(func(m collaboration.Message) Observation {
		switch {
		case m.Kind == collaboration.DispatchPrepare:
			return Observation{Settled: true, Actor: m.Recipient, Accepted: true}
		case m.Recipient == "a":
			return done(m, collaboration.Operation{Kind: collaboration.OpHandoff, OperationID: "h1", To: "b", Context: "ctx"})
		}
		return done(m, collaboration.Operation{Kind: collaboration.OpFinalize, Result: "b finished"})
	})
	d, _ := build(t, c, tr)
	if err := d.Begin(bg, "go"); err != nil {
		t.Fatal(err)
	}
	st := finish(t, d)
	requireResult(t, st, "b finished")
	if st.Owner != "b" {
		t.Fatalf("owner = %q", st.Owner)
	}
}

func TestPeerNestedDelegation(t *testing.T) {
	c := collab(api.CollaborationStrategyPeer, "fin", "b", "c")
	c.Peer = &api.CollaborationPeer{Finalizer: "fin"}
	allow(c, [2]string{"fin", "b"}, [2]string{"b", "c"}, [2]string{"c", "fin"})
	tr := newScripted(func(m collaboration.Message) Observation {
		start := m.Kind == collaboration.DispatchStart
		switch {
		case m.Recipient == "fin" && start:
			return done(m, collaboration.Operation{Kind: collaboration.OpDelegate, OperationID: "x", To: "b", Goal: "g"})
		case m.Recipient == "fin":
			return done(m, collaboration.Operation{Kind: collaboration.OpFinalize, Result: "final:" + m.Results[0].Result.Text})
		case m.Recipient == "b" && start:
			return done(m, collaboration.Operation{Kind: collaboration.OpDelegate, OperationID: "y", To: "c", Goal: "g2"})
		case m.Recipient == "b":
			return done(m, collaboration.Operation{Kind: collaboration.OpComplete, Result: "b+" + m.Results[0].Result.Text})
		}
		return done(m, collaboration.Operation{Kind: collaboration.OpComplete, Result: "c"})
	})
	d, _ := build(t, c, tr)
	if err := d.Begin(bg, "go"); err != nil {
		t.Fatal(err)
	}
	requireResult(t, finish(t, d), "final:b+c")
}

func supervisorDriver(t *testing.T, tr Transport) *Driver {
	c := collab(api.CollaborationStrategySupervisor, "lead", "a")
	c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
	allow(c, [2]string{"lead", "a"})
	d, _ := build(t, c, tr)
	if err := d.Begin(bg, "go"); err != nil {
		t.Fatal(err)
	}
	return d
}

func finalizeOnly(m collaboration.Message) Observation {
	return done(m, collaboration.Operation{Kind: collaboration.OpFinalize, Result: "ok"})
}

func TestBeginIsIdempotent(t *testing.T) {
	d := supervisorDriver(t, newScripted(finalizeOnly))
	st1, _, err := d.ledger.Load(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Begin(bg, "different input"); err != nil {
		t.Fatal(err)
	}
	st2, _, _ := d.ledger.Load(bg)
	if st1.Input.Text != st2.Input.Text {
		t.Fatal("Begin replaced an existing ledger")
	}
}

func TestRefusedDispatchFailsTaskWithoutContact(t *testing.T) {
	tr := newScripted(finalizeOnly)
	tr.refuse = func(collaboration.Message) bool { return true }
	d := supervisorDriver(t, tr)
	st := finish(t, d)
	if st.Phase != collaboration.PhaseFailed {
		t.Fatalf("phase = %s", st.Phase)
	}
	if len(tr.sends) != 0 {
		t.Fatalf("a refused dispatch made contact: %v", tr.sends)
	}
}

func TestSendFailureIsUncertainAndNeverResent(t *testing.T) {
	tr := newScripted(finalizeOnly)
	tr.failSend = func(collaboration.Message) bool { return true }
	d := supervisorDriver(t, tr)
	for i := 0; i < 5; i++ {
		if _, _, err := d.Step(bg); err != nil {
			t.Fatal(err)
		}
	}
	st, _, _ := d.ledger.Load(bg)
	if st.Phase.Terminal() {
		t.Fatalf("uncertain dispatch ended the step: %s", st.Phase)
	}
	if got := tr.sends["t1.1"]; got != 1 {
		t.Fatalf("ambiguous dispatch sent %d times, want 1", got)
	}
	root, _ := st.Task("t1")
	if root.State != collaboration.TaskUncertain {
		t.Fatalf("task state = %s", root.State)
	}
}

func TestUncertainTaskSettlesFromLateObservation(t *testing.T) {
	tr := newScripted(finalizeOnly)
	fail := true
	tr.failSend = func(collaboration.Message) bool { return fail }
	d := supervisorDriver(t, tr)
	if _, _, err := d.Step(bg); err != nil {
		t.Fatal(err)
	}
	// The remote task exists after all; observation (not resend) settles it.
	tr.mu.Lock()
	tr.sent["t1.1"] = collaboration.Message{DispatchID: "t1.1"}
	tr.mu.Unlock()
	fail = false
	requireResult(t, finish(t, d), "ok")
	if tr.sends["t1.1"] != 1 {
		t.Fatalf("dispatch resent: %d", tr.sends["t1.1"])
	}
}

func TestCommittedAfterCrashBecomesUncertainWithoutResend(t *testing.T) {
	tr := newScripted(finalizeOnly)
	d := supervisorDriver(t, tr)
	st, ver, _ := d.ledger.Load(bg)
	next, err := d.engine.Apply(st, collaboration.CommitDispatch{ID: "t1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ledger.Save(bg, next, ver); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Step(bg); err != nil {
		t.Fatal(err)
	}
	if len(tr.sends) != 0 {
		t.Fatalf("a committed dispatch was resent after recovery: %v", tr.sends)
	}
	got, _, _ := d.ledger.Load(bg)
	if x := got.Dispatches[0]; x.Status != collaboration.DispatchUncertain {
		t.Fatalf("dispatch status = %s", x.Status)
	}
}

func TestStopDrainsActivation(t *testing.T) {
	tr := newScripted(func(m collaboration.Message) Observation {
		if m.Kind == collaboration.DispatchCancel {
			return Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportCancelled}
		}
		return finalizeOnly(m)
	})
	tr.hideAll = true
	d := supervisorDriver(t, tr)
	if _, _, err := d.Step(bg); err != nil {
		t.Fatal(err)
	}
	tr.hideAll = false
	if _, err := d.Stop(bg, collaboration.PhaseCancelled, "user"); err != nil {
		t.Fatal(err)
	}
	st := finish(t, d)
	if st.Phase != collaboration.PhaseCancelled && st.Phase != collaboration.PhaseFailed {
		t.Fatalf("phase = %s", st.Phase)
	}
	if st.Phase.Terminal() {
		if _, err := d.Stop(bg, collaboration.PhaseCancelled, "again"); err != nil {
			t.Fatalf("Stop on terminal ledger: %v", err)
		}
	}
}

func TestAbandonClosesUncertainTask(t *testing.T) {
	tr := newScripted(finalizeOnly)
	tr.failSend = func(collaboration.Message) bool { return true }
	d := supervisorDriver(t, tr)
	if _, _, err := d.Step(bg); err != nil {
		t.Fatal(err)
	}
	st, err := d.Abandon(bg, "t1", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != collaboration.PhaseFailed {
		t.Fatalf("phase = %s", st.Phase)
	}
}

func TestStepWithoutLedgerFails(t *testing.T) {
	c := collab(api.CollaborationStrategySupervisor, "lead", "a")
	c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
	d, _ := build(t, c, newScripted(finalizeOnly))
	if _, _, err := d.Step(bg); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestLedgerConflictAbortsStep(t *testing.T) {
	tr := newScripted(finalizeOnly)
	d := supervisorDriver(t, tr)
	st, ver, _ := d.ledger.Load(bg)
	if _, err := d.ledger.Save(bg, st, ver); err != nil { // another writer advances the revision
		t.Fatal(err)
	}
	if _, err := d.ledger.Save(bg, st, ver); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestMemberFailureFailsRequiredStep(t *testing.T) {
	tr := newScripted(func(m collaboration.Message) Observation {
		return Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportFailed, Failure: "boom"}
	})
	d := supervisorDriver(t, tr)
	if st := finish(t, d); st.Phase != collaboration.PhaseFailed {
		t.Fatalf("phase = %s", st.Phase)
	}
}

func TestWrongActorObservationIsRejected(t *testing.T) {
	tr := newScripted(func(m collaboration.Message) Observation {
		o := finalizeOnly(m)
		o.Actor = "a"
		return o
	})
	d := supervisorDriver(t, tr)
	if _, _, err := d.Step(bg); !errors.Is(err, collaboration.ErrActorMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestConstructorRequiresDependencies(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Fatal("expected error")
	}
}
