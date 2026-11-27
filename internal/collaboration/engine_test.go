// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func limits() api.CollaborationLimits {
	return api.CollaborationLimits{
		MaxTurns: 20, MaxMessages: 100, MaxDelegations: 10, MaxDelegationDepth: 3,
		MaxParallelTasks: 4, MaxTokens: 100000,
	}
}

func baseCollab(strategy api.CollaborationStrategy, members ...string) *api.WorkflowCollaboration {
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

func TestDefinitionBoundsAuthorText(t *testing.T) {
	for name, mutate := range map[string]func(*api.WorkflowCollaboration){
		"long guidance": func(c *api.WorkflowCollaboration) {
			c.Guidance = strings.Repeat("g", api.CollaborationMaxGuidanceChars+1)
		},
		"guidance escape": func(c *api.WorkflowCollaboration) { c.Guidance = "x\x1b[31m" },
		"long description": func(c *api.WorkflowCollaboration) {
			c.Members[0].Description = strings.Repeat("d", api.CollaborationMaxMemberDescriptionChars+1)
		},
		"description newline": func(c *api.WorkflowCollaboration) { c.Members[0].Description = "a\nb" },
	} {
		c := baseCollab(api.CollaborationStrategySupervisor, "lead", "a")
		c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
		allow(c, [2]string{"lead", "a"})
		mutate(c)
		if _, err := NewDefinition(c); err == nil {
			t.Fatalf("%s: definition accepted", name)
		}
	}
	c := baseCollab(api.CollaborationStrategySupervisor, "lead", "a")
	c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
	allow(c, [2]string{"lead", "a"})
	c.Guidance, c.Members[0].Description = "Line one.\nLine two.", "drafts"
	if _, err := NewDefinition(c); err != nil {
		t.Fatalf("valid author text rejected: %v", err)
	}
}

func newEngine(t *testing.T, c *api.WorkflowCollaboration) *Engine {
	t.Helper()
	def, err := NewDefinition(c)
	if err != nil {
		t.Fatalf("NewDefinition: %v", err)
	}
	e, err := New(def)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func supervisor(t *testing.T) *Engine {
	c := baseCollab(api.CollaborationStrategySupervisor, "lead", "a", "b")
	c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
	allow(c, [2]string{"lead", "a"}, [2]string{"lead", "b"})
	return newEngine(t, c)
}

func peer(t *testing.T, mutate func(*api.WorkflowCollaboration)) *Engine {
	c := baseCollab(api.CollaborationStrategyPeer, "fin", "b", "c")
	c.Peer = &api.CollaborationPeer{Finalizer: "fin"}
	allow(c, [2]string{"fin", "b"}, [2]string{"b", "c"}, [2]string{"c", "fin"})
	if mutate != nil {
		mutate(c)
	}
	return newEngine(t, c)
}

func parallel(t *testing.T, synth string, optional ...string) *Engine {
	c := baseCollab(api.CollaborationStrategyParallel, "a", "b", "s")
	p := &api.CollaborationParallel{Synthesizer: synth}
	for _, m := range []string{"a", "b"} {
		p.Branches = append(p.Branches, api.CollaborationBranch{Member: m, Optional: contains(optional, m)})
	}
	c.Parallel = p
	return newEngine(t, c)
}

func handoffEngine(t *testing.T, max int32) *Engine {
	c := baseCollab(api.CollaborationStrategyHandoff, "a", "b", "c")
	c.Handoff = &api.CollaborationHandoff{EntryMember: "a", MaxHandoffs: max}
	allow(c, [2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"b", "a"})
	return newEngine(t, c)
}

func del(id, to, goal string) Operation {
	return Operation{Kind: OpDelegate, OperationID: id, To: to, Goal: goal}
}
func complete(text string) Operation { return Operation{Kind: OpComplete, Result: text} }
func finalize(text string) Operation { return Operation{Kind: OpFinalize, Result: text} }
func handoff(id, to, ctx string) Operation {
	return Operation{Kind: OpHandoff, OperationID: id, To: to, Context: ctx}
}

func report(m Message, ops ...Operation) TurnReport {
	return TurnReport{TaskID: m.TaskID, Turn: m.Turn, Actor: m.Recipient, Epoch: m.Epoch,
		Status: ReportCompleted, Operations: ops, TokensUsed: 10, TokensKnown: true}
}

func failed(m Message) TurnReport {
	r := report(m)
	r.Status, r.Failure = ReportFailed, "boom"
	return r
}

func must(t *testing.T, st State, err error) State {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return st
}

func apply(t *testing.T, e *Engine, st State, ev Event) State {
	t.Helper()
	next, err := e.Apply(st, ev)
	return must(t, next, err)
}

func start(t *testing.T, e *Engine) State {
	t.Helper()
	st, err := e.Start("investigate")
	return must(t, st, err)
}

// run performs every pending dispatch with the supplied member behavior.
func run(t *testing.T, e *Engine, st State, behave func(Message) Event) State {
	t.Helper()
	for i := 0; i < 200 && !st.Phase.Terminal(); i++ {
		pending := st.Pending()
		if len(pending) == 0 {
			t.Fatalf("stuck in phase %s outcome %+v tasks %+v", st.Phase, st.Outcome, st.Tasks)
		}
		d := pending[0]
		st = apply(t, e, st, CommitDispatch{d.ID})
		st = apply(t, e, st, AckDispatch{d.ID})
		if d.Kind == DispatchCancel {
			continue
		}
		msg, err := e.Message(st, d.ID)
		if err != nil {
			t.Fatalf("Message(%s): %v", d.ID, err)
		}
		st = apply(t, e, st, behave(msg))
	}
	if !st.Phase.Terminal() {
		t.Fatalf("did not terminate: %s", st.Phase)
	}
	return st
}

func wantOutcome(t *testing.T, st State, phase Phase, reasonPart string) {
	t.Helper()
	if st.Phase != phase {
		t.Fatalf("phase = %s (%+v), want %s", st.Phase, st.Outcome, phase)
	}
	if reasonPart != "" && (st.Outcome == nil || !strings.Contains(st.Outcome.Reason, reasonPart)) {
		t.Fatalf("reason = %+v, want contains %q", st.Outcome, reasonPart)
	}
}

func TestSupervisorDelegatesAndFinalizes(t *testing.T) {
	e := supervisor(t)
	st := run(t, e, start(t, e), func(m Message) Event {
		switch {
		case m.Recipient == "lead" && m.Kind == DispatchStart:
			return report(m, del("d1", "a", "do a"), del("d2", "b", "do b"))
		case m.Recipient == "lead":
			if len(m.Results) != 2 {
				t.Fatalf("lead got %d results", len(m.Results))
			}
			return report(m, finalize("done: "+m.Results[0].Result.Text+m.Results[1].Result.Text))
		default:
			return report(m, complete("r-"+m.Recipient))
		}
	})
	wantOutcome(t, st, PhaseSucceeded, "")
	res := st.Result()
	if res == nil || res.Text != "done: r-ar-b" {
		t.Fatalf("result = %+v", res)
	}
	if want := []string{"a", "b", "executor", "lead"}; !reflect.DeepEqual(res.Origins, want) {
		t.Fatalf("origins = %v, want %v", res.Origins, want)
	}
	if st.Counters.Delegations != 2 || st.Counters.Turns != 4 || st.Counters.TokensReserved != 0 {
		t.Fatalf("counters = %+v", st.Counters)
	}
}

func TestSupervisorOnlyCoordinatorMayDelegate(t *testing.T) {
	e := supervisor(t)
	st := run(t, e, start(t, e), func(m Message) Event {
		if m.Recipient == "lead" {
			return report(m, del("d1", "a", "x"))
		}
		return report(m, del("d9", "b", "sneaky"))
	})
	wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
	if !strings.Contains(st.Tasks[1].Reason, ReasonInvalidProposal) {
		t.Fatalf("child reason = %q", st.Tasks[1].Reason)
	}
}

func TestSupervisorRequiredFailureStopsAndCancels(t *testing.T) {
	e := supervisor(t)
	st := run(t, e, start(t, e), func(m Message) Event {
		if m.Recipient == "lead" {
			return report(m, del("d1", "a", "x"), del("d2", "b", "y"))
		}
		return failed(m)
	})
	wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
	for _, task := range st.Tasks {
		if !task.State.Terminal() {
			t.Fatalf("task %s not terminal: %s", task.ID, task.State)
		}
	}
}

func TestSupervisorOptionalFailureContinues(t *testing.T) {
	e := supervisor(t)
	optional := false
	st := run(t, e, start(t, e), func(m Message) Event {
		switch {
		case m.Recipient == "lead" && m.Kind == DispatchStart:
			opt := del("d2", "b", "y")
			opt.Required = &optional
			return report(m, del("d1", "a", "x"), opt)
		case m.Recipient == "lead":
			states := []TaskState{m.Results[0].State, m.Results[1].State}
			if states[0] != TaskSucceeded || states[1] != TaskFailed {
				t.Fatalf("states = %v", states)
			}
			return report(m, finalize("ok"))
		case m.Recipient == "b":
			return failed(m)
		}
		return report(m, complete("r"))
	})
	wantOutcome(t, st, PhaseSucceeded, "")
}

func TestFinalizeRejectedWhileRequiredWorkOutstanding(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st.Tasks = append(st.Tasks, Task{ID: "t9", Recipient: "a", Required: true, State: TaskWaiting, Children: []string{"t77"}, Epoch: 1})
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	m, _ := e.Message(st, "t1.1")
	st = apply(t, e, st, report(m, finalize("early")))
	if st.Phase != PhaseStopping || st.Outcome == nil || st.Outcome.Reason != ReasonRequiredTask+": t1 "+ReasonInvalidProposal+": required task t9 is unsettled" {
		t.Fatalf("phase=%s outcome=%+v", st.Phase, st.Outcome)
	}
}

func TestInvalidOperationCombinations(t *testing.T) {
	cases := map[string][]Operation{
		"complete on final":        {complete("x")},
		"delegate and finalize":    {del("d1", "a", "x"), finalize("y")},
		"delegate unknown member":  {del("d1", "ghost", "x")},
		"delegate disallowed edge": {del("d1", "lead", "x")},
		"duplicate operation id":   {del("d1", "a", "x"), del("d1", "b", "y")},
		"handoff outside handoff":  {handoff("h1", "a", "ctx")},
		"empty":                    nil,
	}
	for name, ops := range cases {
		t.Run(name, func(t *testing.T) {
			e := supervisor(t)
			st := run(t, e, start(t, e), func(m Message) Event { return report(m, ops...) })
			wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
			if !strings.Contains(st.Tasks[0].Reason, ReasonInvalidProposal) {
				t.Fatalf("reason = %q", st.Tasks[0].Reason)
			}
		})
	}
}

func TestParallelSynthesizerReceivesBranchResults(t *testing.T) {
	e := parallel(t, "s")
	st := run(t, e, start(t, e), func(m Message) Event {
		if m.Recipient == "s" {
			if len(m.Results) != 2 || m.Goal.Text != "" {
				t.Fatalf("synth message = %+v", m)
			}
			return report(m, finalize(m.Results[0].Result.Text+"+"+m.Results[1].Result.Text))
		}
		return report(m, complete("r-"+m.Recipient))
	})
	wantOutcome(t, st, PhaseSucceeded, "")
	if got := st.Result().Text; got != "r-a+r-b" {
		t.Fatalf("result = %q", got)
	}
	if len(st.Manifest) != 2 {
		t.Fatalf("manifest = %+v", st.Manifest)
	}
}

func TestParallelManifestWithoutSynthesizer(t *testing.T) {
	e := parallel(t, "", "b")
	st := run(t, e, start(t, e), func(m Message) Event {
		if m.Recipient == "b" {
			return failed(m)
		}
		return report(m, complete("r"))
	})
	wantOutcome(t, st, PhaseSucceeded, "")
	if !strings.Contains(st.Result().Text, `"state":"Failed"`) || !strings.Contains(st.Result().Text, `"optional":true`) {
		t.Fatalf("manifest = %s", st.Result().Text)
	}
}

func TestParallelFailureModes(t *testing.T) {
	t.Run("required branch fails", func(t *testing.T) {
		e := parallel(t, "s")
		st := run(t, e, start(t, e), func(m Message) Event {
			if m.Recipient == "a" {
				return failed(m)
			}
			return report(m, complete("r"))
		})
		wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
	})
	t.Run("no branch succeeds", func(t *testing.T) {
		e := parallel(t, "", "a", "b")
		st := run(t, e, start(t, e), func(m Message) Event { return failed(m) })
		wantOutcome(t, st, PhaseFailed, ReasonNoBranchSucceeded)
	})
	t.Run("branch cannot delegate", func(t *testing.T) {
		e := parallel(t, "")
		st := run(t, e, start(t, e), func(m Message) Event { return report(m, del("d1", "b", "x")) })
		wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
	})
}

func handoffBehavior(t *testing.T, ack func(Message) bool) func(Message) Event {
	return func(m Message) Event {
		switch {
		case m.Kind == DispatchPrepare:
			return TransferAck{TransferID: m.Transfer.ID, Actor: m.Recipient, Accepted: ack(m)}
		case m.Recipient == "a" && m.Note == "TransferRejected":
			return report(m, finalize("a kept it"))
		case m.Recipient == "a":
			return report(m, handoff("h1", "b", "state for b"))
		default:
			return report(m, finalize("b finished: "+m.Goal.Text))
		}
	}
}

func TestHandoffTransfersOwnership(t *testing.T) {
	e := handoffEngine(t, 2)
	st := run(t, e, start(t, e), handoffBehavior(t, func(Message) bool { return true }))
	wantOutcome(t, st, PhaseSucceeded, "")
	if st.Epoch != 2 || st.Owner != "b" || st.Result().Text != "b finished: state for b" {
		t.Fatalf("epoch=%d owner=%s result=%+v", st.Epoch, st.Owner, st.Result())
	}
	if st.Tasks[0].State != TaskSucceeded || st.Tasks[0].Reason != "HandedOff" {
		t.Fatalf("old owner task = %+v", st.Tasks[0])
	}
}

func TestHandoffRejectionReturnsToOwner(t *testing.T) {
	e := handoffEngine(t, 2)
	st := run(t, e, start(t, e), handoffBehavior(t, func(Message) bool { return false }))
	wantOutcome(t, st, PhaseSucceeded, "")
	if st.Epoch != 1 || st.Owner != "a" || st.Result().Text != "a kept it" {
		t.Fatalf("epoch=%d owner=%s result=%+v", st.Epoch, st.Owner, st.Result())
	}
}

func TestHandoffStaleOwnerDenied(t *testing.T) {
	e := handoffEngine(t, 2)
	st := start(t, e)
	var first Message
	st = run2(t, e, st, 2, func(m Message) Event {
		if m.Recipient == "a" && first.DispatchID == "" {
			first = m
		}
		return handoffBehavior(t, func(Message) bool { return true })(m)
	})
	if st.Epoch != 2 {
		t.Fatalf("epoch = %d", st.Epoch)
	}
	_, err := e.Apply(st, report(first, finalize("late old owner")))
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("err = %v, want stale epoch", err)
	}
	// A late ack from the new owner is also not accepted twice.
	if _, err := e.Apply(st, TransferAck{TransferID: "x1", Actor: "b", Accepted: true}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("duplicate ack err = %v", err)
	}
}

// run2 performs at most n dispatches, stopping as soon as the activation ends.
func run2(t *testing.T, e *Engine, st State, n int, behave func(Message) Event) State {
	t.Helper()
	for i := 0; i < n && !st.Phase.Terminal(); i++ {
		d := st.Pending()[0]
		st = apply(t, e, st, CommitDispatch{d.ID})
		st = apply(t, e, st, AckDispatch{d.ID})
		msg, err := e.Message(st, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		st = apply(t, e, st, behave(msg))
	}
	return st
}

func TestHandoffUncertainPrepareFreezesUntilResolved(t *testing.T) {
	e := handoffEngine(t, 2)
	st := start(t, e)
	st = run2(t, e, st, 1, handoffBehavior(t, nil))
	prep := st.Pending()[0]
	if prep.Kind != DispatchPrepare {
		t.Fatalf("pending = %+v", prep)
	}
	st = apply(t, e, st, CommitDispatch{prep.ID})
	st = apply(t, e, st, UncertainDispatch{prep.ID, "timeout"})
	if st.Transfer.Status != TransferUncertain || st.Phase != PhaseRunning || len(st.Pending()) != 0 {
		t.Fatalf("transfer=%+v phase=%s pending=%v", st.Transfer, st.Phase, st.Pending())
	}
	// Stop while uncertain: the run cannot finish until the transfer resolves.
	st = apply(t, e, st, Stop{Phase: PhaseCancelled, Reason: ReasonCancelled})
	if st.Phase != PhaseStopping {
		t.Fatalf("phase = %s, want Stopping", st.Phase)
	}
	// The recipient's late acceptance cannot commit ownership during a stop.
	st = apply(t, e, st, TransferAck{TransferID: st.Transfer.ID, Actor: "b", Accepted: true})
	for _, d := range st.Pending() {
		st = apply(t, e, st, CommitDispatch{d.ID})
		st = apply(t, e, st, AckDispatch{d.ID})
	}
	wantOutcome(t, st, PhaseCancelled, "")
	if st.Epoch != 1 || st.Owner != "a" {
		t.Fatalf("ownership changed during stop: epoch=%d owner=%s", st.Epoch, st.Owner)
	}
}

func TestHandoffPrepareAccountsForUnknownUsageAndRefusal(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "unknown usage"
		if refused {
			name = "refused before contact"
		}
		t.Run(name, func(t *testing.T) {
			e := handoffEngine(t, 2)
			st := run2(t, e, start(t, e), 1, handoffBehavior(t, nil))
			prep := st.Pending()[0]
			if prep.Allowance <= 0 || st.Counters.TokensReserved != prep.Allowance {
				t.Fatalf("prepare did not reserve tokens: dispatch=%+v counters=%+v", prep, st.Counters)
			}
			wantUsed := st.Counters.TokensUsed
			if refused {
				st = apply(t, e, st, RefuseDispatch{prep.ID, "policy denied"})
			} else {
				wantUsed += prep.Allowance
				st = apply(t, e, st, CommitDispatch{prep.ID})
				st = apply(t, e, st, AckDispatch{prep.ID})
				st = apply(t, e, st, TransferAck{TransferID: st.Transfer.ID, Actor: "b", Accepted: true})
			}
			if st.Counters.TokensUsed != wantUsed {
				t.Fatalf("tokens used=%d, want %d", st.Counters.TokensUsed, wantUsed)
			}
			next := st.Pending()
			if len(next) != 1 || st.Counters.TokensReserved != next[0].Allowance {
				t.Fatalf("only the next turn should hold a reservation: pending=%+v counters=%+v", next, st.Counters)
			}
		})
	}
}

func TestHandoffLimit(t *testing.T) {
	e := handoffEngine(t, 1)
	st := run(t, e, start(t, e), func(m Message) Event {
		switch {
		case m.Kind == DispatchPrepare:
			return TransferAck{TransferID: m.Transfer.ID, Actor: m.Recipient, Accepted: true}
		case m.Recipient == "a":
			return report(m, handoff("h1", "b", "ctx"))
		default:
			return report(m, handoff("h2", "a", "back"))
		}
	})
	wantOutcome(t, st, PhaseFailed, ReasonHandoffLimit)
}

func TestHandoffRejectsUnauthorizedRecipient(t *testing.T) {
	e := handoffEngine(t, 2)
	st := run(t, e, start(t, e), func(m Message) Event { return report(m, handoff("h1", "c", "ctx")) })
	wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
}

func TestPeerNestedDelegation(t *testing.T) {
	e := peer(t, nil)
	st := run(t, e, start(t, e), func(m Message) Event {
		switch {
		case m.Recipient == "fin" && m.Kind == DispatchStart:
			return report(m, del("d1", "b", "ask b"))
		case m.Recipient == "fin":
			return report(m, finalize("fin: "+m.Results[0].Result.Text))
		case m.Recipient == "b" && m.Kind == DispatchStart:
			return report(m, del("d1", "c", "ask c"))
		case m.Recipient == "b":
			return report(m, complete("b: "+m.Results[0].Result.Text))
		}
		return report(m, complete("c answer"))
	})
	wantOutcome(t, st, PhaseSucceeded, "")
	if got := st.Result().Text; got != "fin: b: c answer" {
		t.Fatalf("result = %q", got)
	}
}

func TestPeerDepthAndDelegationLimits(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		e := peer(t, func(c *api.WorkflowCollaboration) { c.Limits.MaxDelegationDepth = 1 })
		st := run(t, e, start(t, e), func(m Message) Event {
			switch m.Recipient {
			case "fin":
				return report(m, del("d1", "b", "x"))
			default:
				return report(m, del("d1", "c", "y"))
			}
		})
		wantOutcome(t, st, PhaseFailed, ReasonDepthLimit)
	})
	t.Run("delegations", func(t *testing.T) {
		e := peer(t, func(c *api.WorkflowCollaboration) { c.Limits.MaxDelegations = 1 })
		st := run(t, e, start(t, e), func(m Message) Event {
			if m.Recipient == "fin" {
				return report(m, del("d1", "b", "x"), del("d2", "b", "y"))
			}
			return report(m, complete("r"))
		})
		wantOutcome(t, st, PhaseFailed, ReasonDelegationLimit)
	})
}

func TestPeerQuiescenceFailsClosed(t *testing.T) {
	e := peer(t, nil)
	st := start(t, e)
	st.Tasks[0].State = TaskSucceeded
	next, err := e.Recover(st)
	must(t, next, err)
	if next.Phase != PhaseFailed || next.Outcome.Reason != ReasonPeerQuiescence {
		t.Fatalf("phase=%s outcome=%+v", next.Phase, next.Outcome)
	}
}

func TestLimitExhaustionNeverSucceeds(t *testing.T) {
	cases := map[string]struct {
		mutate func(*api.WorkflowCollaboration)
		reason string
	}{
		"turns":    {func(c *api.WorkflowCollaboration) { c.Limits.MaxTurns = 2 }, ReasonTurnLimit},
		"messages": {func(c *api.WorkflowCollaboration) { c.Limits.MaxMessages = 3 }, ReasonMessageLimit},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := baseCollab(api.CollaborationStrategySupervisor, "lead", "a", "b")
			c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
			allow(c, [2]string{"lead", "a"}, [2]string{"lead", "b"})
			tc.mutate(c)
			e := newEngine(t, c)
			st := run(t, e, start(t, e), func(m Message) Event {
				if m.Recipient == "lead" && m.Kind == DispatchStart {
					return report(m, del("d1", "a", "x"))
				}
				if m.Recipient == "lead" {
					return report(m, finalize("never"))
				}
				return report(m, complete("r"))
			})
			wantOutcome(t, st, PhaseFailed, tc.reason)
		})
	}
}

func TestTokenBudget(t *testing.T) {
	e := supervisor(t)
	st := run(t, e, start(t, e), func(m Message) Event {
		r := report(m, complete("r"))
		if m.Recipient == "lead" {
			r = report(m, del("d1", "a", "x"))
		}
		r.TokensUsed = 1 << 30
		return r
	})
	wantOutcome(t, st, PhaseFailed, ReasonTokenBudget)
}

func TestUnknownTokenUsageChargesAllowance(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	m, _ := e.Message(st, "t1.1")
	allowance := m.Allowance
	r := report(m, del("d1", "a", "x"))
	r.TokensKnown = false
	st = apply(t, e, st, r)
	var reserved int64
	for _, d := range st.Pending() {
		reserved += d.Allowance
	}
	if st.Counters.TokensUsed != allowance || st.Counters.TokensReserved != reserved {
		t.Fatalf("counters = %+v, allowance %d, reserved by pending %d", st.Counters, allowance, reserved)
	}
}

func TestFanOutFinalTurnLeavesBudgetForItsWork(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	m, _ := e.Message(st, "t1.1")
	if m.Allowance != limits().MaxTokens-limits().MaxTokens/2 {
		t.Fatalf("fan-out final allowance = %d", m.Allowance)
	}
	r := report(m, del("d1", "a", "x"), del("d2", "b", "y"))
	r.TokensKnown = false
	st = apply(t, e, st, r)
	pending := st.Pending()
	if st.Phase != PhaseRunning || len(pending) != 2 {
		t.Fatalf("phase=%s pending=%v", st.Phase, pending)
	}
	for _, d := range pending {
		if d.Allowance <= 0 {
			t.Fatalf("child %s starved: %+v", d.ID, d)
		}
	}
}

func TestFinalTurnWithoutFanOutTakesTheWholeAllowance(t *testing.T) {
	e := parallel(t, "s")
	var synth Message
	st := run(t, e, start(t, e), func(m Message) Event {
		if m.Recipient == "s" {
			synth = m
			return report(m, finalize("done"))
		}
		return report(m, complete("part"))
	})
	wantOutcome(t, st, PhaseSucceeded, "")
	if want := limits().MaxTokens - 20; synth.Allowance != want {
		t.Fatalf("allowance = %d, want %d", synth.Allowance, want)
	}
}

func TestUncertainDispatchIsNeverResent(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, UncertainDispatch{"t1.1", "deadline"})
	if st.Tasks[0].State != TaskUncertain || len(st.Pending()) != 0 {
		t.Fatalf("task=%s pending=%v", st.Tasks[0].State, st.Pending())
	}
	again, err := e.Recover(st)
	if err != nil || len(again.Pending()) != 0 || again.Dispatches[0].Status != DispatchUncertain {
		t.Fatalf("recover: %v %+v", err, again.Dispatches)
	}
	// The remote turn may still report; it settles normally.
	m := Message{TaskID: "t1", Turn: 1, Recipient: "lead", Epoch: 1}
	st = apply(t, e, st, report(m, del("d1", "a", "x")))
	if st.Tasks[0].State != TaskWaiting {
		t.Fatalf("state = %s", st.Tasks[0].State)
	}
}

func TestRecoverTurnsCommittedIntoUncertain(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st, err := e.Recover(st)
	must(t, st, err)
	if st.Dispatches[0].Status != DispatchUncertain || st.Tasks[0].State != TaskUncertain || len(st.Pending()) != 0 {
		t.Fatalf("dispatch=%+v task=%s", st.Dispatches[0], st.Tasks[0].State)
	}
	st = apply(t, e, st, AckDispatch{"t1.1"})
	if st.Tasks[0].State != TaskRunning {
		t.Fatalf("resolved state = %s", st.Tasks[0].State)
	}
}

func TestAbandonUncertainTask(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, UncertainDispatch{"t1.1", "x"})
	st = apply(t, e, st, TaskAbandoned{TaskID: "t1", Reason: "operator"})
	wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
	if _, err := e.Apply(start(t, e), TaskAbandoned{TaskID: "t1"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("abandon of running task err = %v", err)
	}
}

func TestReportValidation(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	m := Message{TaskID: "t1", Turn: 1, Recipient: "lead", Epoch: 1}
	good := report(m, del("d1", "a", "x"))
	mut := func(f func(*TurnReport)) TurnReport { r := good; f(&r); return r }
	cases := map[string]struct {
		ev  TurnReport
		err error
	}{
		"spoofed actor": {mut(func(r *TurnReport) { r.Actor = "a" }), ErrActorMismatch},
		"unknown task":  {mut(func(r *TurnReport) { r.TaskID = "t77" }), ErrUnknownTask},
		"stale turn":    {mut(func(r *TurnReport) { r.Turn = 2 }), ErrStaleTurn},
		"stale epoch":   {mut(func(r *TurnReport) { r.Epoch = 0 }), ErrStaleEpoch},
		"bad status":    {mut(func(r *TurnReport) { r.Status = "Bogus" }), ErrInvalidEvent},
		"negative use":  {mut(func(r *TurnReport) { r.TokensUsed = -1 }), ErrInvalidEvent},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := e.Apply(st, tc.ev)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if !reflect.DeepEqual(got, st) {
				t.Fatalf("ledger changed by a rejected event")
			}
		})
	}
	t.Run("uncontacted dispatch", func(t *testing.T) {
		fresh := start(t, e)
		if _, err := e.Apply(fresh, good); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("settled task", func(t *testing.T) {
		done := apply(t, e, st, mut(func(r *TurnReport) { r.Status = ReportFailed }))
		if _, err := e.Apply(done, good); !errors.Is(err, ErrTerminal) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dispatch lifecycle", func(t *testing.T) {
		if _, err := e.Apply(st, CommitDispatch{"t1.1"}); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("double commit err = %v", err)
		}
		if _, err := e.Apply(st, RefuseDispatch{"t1.1", "late"}); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("refuse after commit err = %v", err)
		}
		if _, err := e.Apply(st, AckDispatch{"nope"}); !errors.Is(err, ErrUnknownTask) {
			t.Fatalf("unknown dispatch err = %v", err)
		}
	})
}

func TestStopBeforeContactMakesNoContact(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, Stop{Phase: PhaseCancelled, Reason: ReasonCancelled})
	wantOutcome(t, st, PhaseCancelled, ReasonCancelled)
	if len(st.Pending()) != 0 || st.Dispatches[0].Status != DispatchRefused || st.Counters.TokensReserved != 0 {
		t.Fatalf("dispatches=%+v counters=%+v", st.Dispatches, st.Counters)
	}
	if _, err := e.Apply(st, CommitDispatch{"t1.1"}); !errors.Is(err, ErrTerminal) {
		t.Fatalf("commit after stop err = %v", err)
	}
}

func TestStopAfterContactRequestsCancel(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	st = apply(t, e, st, Stop{Phase: PhaseFailed, Reason: ReasonDeadlineExceeded})
	if st.Phase != PhaseStopping || len(st.Pending()) != 1 || st.Pending()[0].Kind != DispatchCancel {
		t.Fatalf("phase=%s pending=%+v", st.Phase, st.Pending())
	}
	st = apply(t, e, st, CommitDispatch{"t1.c"})
	st = apply(t, e, st, AckDispatch{"t1.c"})
	wantOutcome(t, st, PhaseFailed, ReasonDeadlineExceeded)
	if st.Counters.TokensReserved != 0 || st.Tasks[0].State != TaskCancelled {
		t.Fatalf("task=%s counters=%+v", st.Tasks[0].State, st.Counters)
	}
}

func TestStopLateResultIsKeptButCannotChangeOutcome(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	st = apply(t, e, st, Stop{Phase: PhaseCancelled, Reason: ReasonCancelled})
	m := Message{TaskID: "t1", Turn: 1, Recipient: "lead", Epoch: 1}
	st = apply(t, e, st, report(m, finalize("too late")))
	wantOutcome(t, st, PhaseCancelled, ReasonCancelled)
	if st.Result() != nil {
		t.Fatalf("late finalize published a result: %+v", st.Result())
	}
}

func TestRefusedDispatchFailsRequiredTask(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, RefuseDispatch{"t1.1", "policy"})
	wantOutcome(t, st, PhaseFailed, ReasonRequiredTask)
	if st.Counters.TokensReserved != 0 || !strings.Contains(st.Tasks[0].Reason, ReasonDispatchRefused) {
		t.Fatalf("counters=%+v task=%+v", st.Counters, st.Tasks[0])
	}
}

func TestTerminalLedgerRejectsEvents(t *testing.T) {
	e := supervisor(t)
	st := apply(t, e, start(t, e), Stop{Phase: PhaseFailed, Reason: "x"})
	for _, ev := range []Event{Stop{Phase: PhaseFailed}, CommitDispatch{"t1.1"}} {
		if _, err := e.Apply(st, ev); !errors.Is(err, ErrTerminal) {
			t.Fatalf("err = %v", err)
		}
	}
	if _, err := e.Apply(start(t, e), Stop{Phase: PhaseSucceeded}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("stop(Succeeded) err = %v", err)
	}
}

func TestMessageExposesOnlyRoleValidOperations(t *testing.T) {
	cases := []struct {
		name string
		e    *Engine
		want []OperationKind
	}{
		{"supervisor coordinator", supervisor(t), []OperationKind{OpFinalize, OpDelegate}},
		{"handoff owner", handoffEngine(t, 1), []OperationKind{OpFinalize, OpHandoff}},
		{"parallel branch", parallel(t, ""), []OperationKind{OpComplete}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := start(t, tc.e)
			m, err := tc.e.Message(st, st.Pending()[0].ID)
			if err != nil || !reflect.DeepEqual(m.AllowedOperations, tc.want) {
				t.Fatalf("ops = %v (%v), want %v", m.AllowedOperations, err, tc.want)
			}
		})
	}
}

func TestMessageCarriesTheRequesterOfADelegatedTask(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	root, err := e.Message(st, "t1.1")
	if err != nil || root.Requester != "" {
		t.Fatalf("root requester = %q (%v)", root.Requester, err)
	}
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	st = apply(t, e, st, report(root, del("d1", "a", "x")))
	child, err := e.Message(st, st.Pending()[0].ID)
	if err != nil || child.Requester != "lead" || child.Recipient != "a" {
		t.Fatalf("child = %+v (%v)", child, err)
	}
}

func TestMessageFreezesTheAllowedOperationsAtIntent(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	first, err := e.Message(st, "t1.1")
	if err != nil || !reflect.DeepEqual(first.AllowedOperations, []OperationKind{OpFinalize, OpDelegate}) {
		t.Fatalf("ops = %v (%v)", first.AllowedOperations, err)
	}
	st.Counters.Delegations = e.def.Limits.MaxDelegations
	again, err := e.Message(st, "t1.1")
	if err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("frozen message changed: %+v vs %+v (%v)", again, first, err)
	}
	dispatches := append([]Dispatch(nil), st.Dispatches...)
	dispatches[0].Allowed = nil
	st.Dispatches = dispatches
	computed, err := e.Message(st, "t1.1")
	if err != nil || !reflect.DeepEqual(computed.AllowedOperations, []OperationKind{OpFinalize}) {
		t.Fatalf("fallback ops = %v (%v)", computed.AllowedOperations, err)
	}
}

func TestMessageForCancelCarriesItsTarget(t *testing.T) {
	e := supervisor(t)
	st := start(t, e)
	st = apply(t, e, st, CommitDispatch{"t1.1"})
	st = apply(t, e, st, AckDispatch{"t1.1"})
	st = apply(t, e, st, Stop{Phase: PhaseFailed, Reason: "x"})
	m, err := e.Message(st, "t1.c")
	if err != nil || m.Kind != DispatchCancel || m.Target == nil || m.Target.DispatchID != "t1.1" || m.Target.Kind != DispatchStart {
		t.Fatalf("cancel = %+v (%v)", m, err)
	}
	broken := st
	broken.Dispatches = nil
	for _, d := range st.Dispatches {
		if d.ID == "t1.c" {
			broken.Dispatches = append(broken.Dispatches, d)
		}
	}
	if _, err := e.Message(broken, "t1.c"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("cancel without its target err = %v", err)
	}
}

func TestLedgerRoundTripIsDeterministic(t *testing.T) {
	e := supervisor(t)
	behave := func(m Message) Event {
		switch {
		case m.Recipient == "lead" && m.Kind == DispatchStart:
			return report(m, del("d1", "a", "x"), del("d2", "b", "y"))
		case m.Recipient == "lead":
			return report(m, finalize("done"))
		}
		return report(m, complete("r"))
	}
	direct := run(t, e, start(t, e), behave)

	// A fresh engine over a decoded ledger must take the identical path.
	fresh := supervisor(t)
	st := start(t, fresh)
	for i := 0; i < 200 && !st.Phase.Terminal(); i++ {
		raw, err := Encode(st)
		if err != nil {
			t.Fatal(err)
		}
		st = must(t, mustDecode(t, raw), nil)
		fresh = supervisor(t)
		d := st.Pending()[0]
		st = apply(t, fresh, st, CommitDispatch{d.ID})
		st = apply(t, fresh, st, AckDispatch{d.ID})
		m, err := fresh.Message(st, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		st = apply(t, fresh, st, behave(m))
	}
	if !reflect.DeepEqual(direct, st) {
		t.Fatalf("round-tripped ledger diverged:\n%+v\n%+v", direct, st)
	}
}

func mustDecode(t *testing.T, raw []byte) State {
	t.Helper()
	st, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDecodeRejectsMalformedLedgers(t *testing.T) {
	e := supervisor(t)
	raw, _ := Encode(start(t, e))
	cases := map[string]string{
		"empty":            "",
		"unknown field":    strings.Replace(string(raw), `"phase"`, `"admin":true,"phase"`, 1),
		"bad version":      strings.Replace(string(raw), `"version":1`, `"version":2`, 1),
		"unknown phase":    strings.Replace(string(raw), `"Running"`, `"Teleporting"`, 1),
		"trailing data":    string(raw) + `{}`,
		"unknown dispatch": strings.Replace(string(raw), `"taskId":"t1"`, `"taskId":"t9"`, 1),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(in)); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	big := start(t, e)
	big.Input.Text = strings.Repeat("x", MaxStateBytes+1)
	if _, err := Encode(big); !errors.Is(err, ErrStateTooLarge) {
		t.Fatalf("oversize err = %v", err)
	}
}

func TestStartRejectsBadInput(t *testing.T) {
	e := supervisor(t)
	for _, in := range []string{"", "  ", strings.Repeat("x", MaxContentBytes+1)} {
		if _, err := e.Start(in); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("Start(%d bytes) err = %v", len(in), err)
		}
	}
}

func TestNewDefinitionRejectsInvalidShapes(t *testing.T) {
	good := func() *api.WorkflowCollaboration {
		c := baseCollab(api.CollaborationStrategySupervisor, "lead", "a")
		c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
		allow(c, [2]string{"lead", "a"})
		return c
	}
	if _, err := NewDefinition(good()); err != nil {
		t.Fatalf("good: %v", err)
	}
	cases := map[string]func(*api.WorkflowCollaboration){
		"nil protocol":     func(c *api.WorkflowCollaboration) { c.Protocol = "MCP" },
		"one member":       func(c *api.WorkflowCollaboration) { c.Members = c.Members[:1] },
		"duplicate alias":  func(c *api.WorkflowCollaboration) { c.Members[1].Name = "lead" },
		"unknown coord":    func(c *api.WorkflowCollaboration) { c.Supervisor.Coordinator = "ghost" },
		"self interaction": func(c *api.WorkflowCollaboration) { allow(c, [2]string{"a", "a"}) },
		"zero turns":       func(c *api.WorkflowCollaboration) { c.Limits.MaxTurns = 0 },
		"turns above max":  func(c *api.WorkflowCollaboration) { c.Limits.MaxTurns = api.CollaborationMaxTurns + 1 },
		"no tokens":        func(c *api.WorkflowCollaboration) { c.Limits.MaxTokens = 0 },
		"extra block":      func(c *api.WorkflowCollaboration) { c.Peer = &api.CollaborationPeer{Finalizer: "a"} },
		"unknown strategy": func(c *api.WorkflowCollaboration) { c.Strategy = "Mesh" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := good()
			mutate(c)
			if _, err := NewDefinition(c); !errors.Is(err, ErrInvalidDefinition) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := NewDefinition(nil); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("nil err = %v", err)
	}
	if _, err := New(Definition{}); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("zero definition err = %v", err)
	}
}

func TestOperationPayloadIsStrict(t *testing.T) {
	ok := `{"schemaVersion":"kognition.collaboration/v1","operations":[{"kind":"complete","result":"x"}]}`
	if ops, err := DecodeOperations([]byte(ok)); err != nil || len(ops) != 1 {
		t.Fatalf("valid payload: %v %v", ops, err)
	}
	cases := map[string]string{
		"actor claim":     `{"schemaVersion":"kognition.collaboration/v1","operations":[{"kind":"complete","result":"x","actor":"lead"}]}`,
		"epoch claim":     `{"schemaVersion":"kognition.collaboration/v1","epoch":9,"operations":[{"kind":"complete","result":"x"}]}`,
		"schema version":  `{"schemaVersion":"kognition.collaboration/v2","operations":[{"kind":"complete","result":"x"}]}`,
		"missing version": `{"operations":[{"kind":"complete","result":"x"}]}`,
		"unknown kind":    `{"schemaVersion":"kognition.collaboration/v1","operations":[{"kind":"escalate","result":"x"}]}`,
		"delegate shape":  `{"schemaVersion":"kognition.collaboration/v1","operations":[{"kind":"delegate","operationId":"d1","to":"a"}]}`,
		"bad operationId": `{"schemaVersion":"kognition.collaboration/v1","operations":[{"kind":"delegate","operationId":"Bad ID","to":"a","goal":"g"}]}`,
		"trailing":        ok + ` {}`,
		"no operations":   `{"schemaVersion":"kognition.collaboration/v1","operations":[]}`,
		"oversize":        `{"schemaVersion":"kognition.collaboration/v1","operations":[{"kind":"complete","result":"` + strings.Repeat("x", MaxContentBytes+1) + `"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeOperations([]byte(in)); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	raw, err := EncodeOperations([]Operation{del("d1", "a", "g")})
	if err != nil {
		t.Fatal(err)
	}
	if ops, err := DecodeOperations(raw); err != nil || ops[0].To != "a" {
		t.Fatalf("round trip: %v %v", ops, err)
	}
}
