// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package driver

import (
	"context"
	"errors"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
)

var errBoom = errors.New("boom")

// faulty injects ledger failures around a MemoryLedger.
type faulty struct {
	*MemoryLedger
	loadErr     error
	createErr   error
	saveErr     error
	okSaves     int // saves allowed before saveErr applies
	conflicts   int // saves answered with ErrConflict before succeeding
	saveAttempt int
}

func (f *faulty) Load(ctx context.Context) (collaboration.State, Version, error) {
	if f.loadErr != nil {
		return collaboration.State{}, "", f.loadErr
	}
	return f.MemoryLedger.Load(ctx)
}

func (f *faulty) Create(ctx context.Context, st collaboration.State) (Version, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.MemoryLedger.Create(ctx, st)
}

func (f *faulty) Save(ctx context.Context, st collaboration.State, expect Version) (Version, error) {
	f.saveAttempt++
	if f.conflicts > 0 {
		f.conflicts--
		return "", ErrConflict
	}
	if f.saveErr != nil {
		if f.okSaves <= 0 {
			return "", f.saveErr
		}
		f.okSaves--
	}
	return f.MemoryLedger.Save(ctx, st, expect)
}

// gate wraps a Transport with failure hooks.
type gate struct {
	Transport
	admitErr   error
	observeErr error
}

func (g gate) Admit(ctx context.Context, m collaboration.Message) error {
	if g.admitErr != nil {
		return g.admitErr
	}
	return g.Transport.Admit(ctx, m)
}

func (g gate) Observe(ctx context.Context, m collaboration.Message) (Observation, error) {
	if g.observeErr != nil {
		return Observation{}, g.observeErr
	}
	return g.Transport.Observe(ctx, m)
}

func faultyDriver(t *testing.T, tr Transport) (*Driver, *faulty) {
	t.Helper()
	c := collab(api.CollaborationStrategySupervisor, "lead", "a")
	c.Supervisor = &api.CollaborationSupervisor{Coordinator: "lead"}
	allow(c, [2]string{"lead", "a"})
	def, err := collaboration.NewDefinition(c)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := collaboration.New(def)
	if err != nil {
		t.Fatal(err)
	}
	led := &faulty{MemoryLedger: NewMemoryLedger()}
	d, err := New(eng, led, tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Begin(bg, "go"); err != nil {
		t.Fatal(err)
	}
	return d, led
}

func TestBeginReportsInvalidInputAndLedgerFailure(t *testing.T) {
	d, _ := faultyDriver(t, newScripted(finalizeOnly))
	if err := d.Begin(bg, "  "); !errors.Is(err, collaboration.ErrInvalidEvent) {
		t.Fatalf("empty input: %v", err)
	}
	fresh := &faulty{MemoryLedger: NewMemoryLedger(), createErr: errBoom}
	d.ledger = fresh
	if err := d.Begin(bg, "go"); !errors.Is(err, errBoom) {
		t.Fatalf("create failure: %v", err)
	}
}

func TestStepSurfacesLedgerFailures(t *testing.T) {
	d, led := faultyDriver(t, newScripted(finalizeOnly))
	led.loadErr = errBoom
	if _, _, err := d.Step(bg); !errors.Is(err, errBoom) {
		t.Fatalf("load failure: %v", err)
	}
	for okSaves := 0; okSaves < 3; okSaves++ {
		d, led := faultyDriver(t, newScripted(finalizeOnly))
		led.saveErr, led.okSaves = errBoom, okSaves
		if _, _, err := d.Step(bg); !errors.Is(err, errBoom) {
			t.Fatalf("save failure after %d saves: %v", okSaves, err)
		}
	}
}

func TestRecoverSaveFailureIsSurfaced(t *testing.T) {
	tr := newScripted(finalizeOnly)
	d, led := faultyDriver(t, tr)
	st, ver, _ := led.Load(bg)
	next, err := d.engine.Apply(st, collaboration.CommitDispatch{ID: "t1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := led.Save(bg, next, ver); err != nil {
		t.Fatal(err)
	}
	led.saveErr = errBoom
	if _, _, err := d.Step(bg); !errors.Is(err, errBoom) {
		t.Fatalf("recover save failure: %v", err)
	}
	if len(tr.sends) != 0 {
		t.Fatalf("recovery resent: %v", tr.sends)
	}
}

func TestUndecidedAdmissionLeavesDispatchUncontacted(t *testing.T) {
	tr := newScripted(finalizeOnly)
	d, led := faultyDriver(t, gate{Transport: tr, admitErr: errors.New("policy backend unavailable")})
	if _, _, err := d.Step(bg); err == nil {
		t.Fatal("an undecided admission must surface")
	}
	st, _, _ := led.Load(bg)
	if x := st.Dispatches[0]; x.Status != collaboration.DispatchIntended || len(tr.sends) != 0 {
		t.Fatalf("dispatch %s, sends %v", x.Status, tr.sends)
	}
}

func TestObservationErrorLeavesDispatchPending(t *testing.T) {
	tr := newScripted(finalizeOnly)
	d, _ := faultyDriver(t, gate{Transport: tr, observeErr: errBoom})
	for i := 0; i < 3; i++ {
		if _, done, err := d.Step(bg); err != nil || done {
			t.Fatalf("step %d: done=%v err=%v", i, done, err)
		}
	}
}

func TestMutateRetriesLostRaces(t *testing.T) {
	d, led := faultyDriver(t, newScripted(finalizeOnly))
	led.conflicts = maxConflictRetries - 1
	if st, err := d.Stop(bg, collaboration.PhaseCancelled, "user"); err != nil || st.Phase != collaboration.PhaseStopping && !st.Phase.Terminal() {
		t.Fatalf("phase %s err %v", st.Phase, err)
	}
	led.conflicts = maxConflictRetries
	if _, err := d.Stop(bg, collaboration.PhaseCancelled, "again"); err != nil {
		// the ledger is already stopping, so Stop is a no-op that still saves; exhausted races surface
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestMutateSurfacesFailures(t *testing.T) {
	d, led := faultyDriver(t, newScripted(finalizeOnly))
	led.saveErr = errBoom
	if _, err := d.Stop(bg, collaboration.PhaseCancelled, "x"); !errors.Is(err, errBoom) {
		t.Fatalf("save failure: %v", err)
	}
	led.saveErr = nil
	if _, err := d.Abandon(bg, "t1", "not uncertain"); err == nil {
		t.Fatal("abandoning a running task must fail")
	}
	led.loadErr = errBoom
	if _, err := d.Stop(bg, collaboration.PhaseCancelled, "x"); !errors.Is(err, errBoom) {
		t.Fatalf("load failure: %v", err)
	}
}

func TestAbandonOnTerminalLedgerIsNoop(t *testing.T) {
	d, _ := faultyDriver(t, newScripted(finalizeOnly))
	if st := finish(t, d); !st.Phase.Terminal() {
		t.Fatalf("phase %s", st.Phase)
	}
	if st, err := d.Abandon(bg, "t1", "late"); err != nil || !st.Phase.Terminal() {
		t.Fatalf("abandon on terminal: %v %s", err, st.Phase)
	}
}

func TestEventMapping(t *testing.T) {
	if _, ok := event(collaboration.Message{Kind: collaboration.DispatchCancel}, Observation{Status: collaboration.ReportCompleted}); ok {
		t.Fatal("a cancel is only confirmed by a cancelled observation")
	}
	ev, ok := event(collaboration.Message{Kind: collaboration.DispatchCancel, DispatchID: "t1.c"}, Observation{Status: collaboration.ReportCancelled})
	if _, isAck := ev.(collaboration.AckDispatch); !ok || !isAck {
		t.Fatalf("cancel confirmation: %v %v", ev, ok)
	}
}

func handoffDriver(t *testing.T, tr Transport) *Driver {
	t.Helper()
	c := collab(api.CollaborationStrategyHandoff, "a", "b")
	c.Handoff = &api.CollaborationHandoff{EntryMember: "a", MaxHandoffs: 2}
	allow(c, [2]string{"a", "b"})
	d, _ := build(t, c, tr)
	if err := d.Begin(bg, "go"); err != nil {
		t.Fatal(err)
	}
	return d
}

func handoffThenFinish(m collaboration.Message) Observation {
	if m.Recipient == "a" && m.Kind == collaboration.DispatchStart {
		return done(m, collaboration.Operation{Kind: collaboration.OpHandoff, OperationID: "h1", To: "b", Context: "ctx"})
	}
	return done(m, collaboration.Operation{Kind: collaboration.OpFinalize, Result: "a finished"})
}

func TestRejectedHandoffKeepsOwnership(t *testing.T) {
	tr := newScripted(func(m collaboration.Message) Observation {
		if m.Kind == collaboration.DispatchPrepare {
			return Observation{Settled: true, Actor: m.Recipient, Accepted: false}
		}
		return handoffThenFinish(m)
	})
	st := finish(t, handoffDriver(t, tr))
	requireResult(t, st, "a finished")
	if st.Owner != "" && st.Owner != "a" {
		t.Fatalf("owner = %q", st.Owner)
	}
}

func TestRefusedHandoffPrepareKeepsOwnership(t *testing.T) {
	tr := newScripted(handoffThenFinish)
	tr.refuse = func(m collaboration.Message) bool { return m.Kind == collaboration.DispatchPrepare }
	st := finish(t, handoffDriver(t, tr))
	requireResult(t, st, "a finished")
	if tr.sends["x1.p"] != 0 {
		t.Fatalf("a refused prepare made contact: %v", tr.sends)
	}
}

func TestMemoryLedgerRequiresCreateBeforeSave(t *testing.T) {
	led := NewMemoryLedger()
	if _, err := led.Save(bg, collaboration.State{}, "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := led.Load(bg); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
