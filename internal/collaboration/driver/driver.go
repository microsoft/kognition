// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package driver runs a collaboration ledger to completion. It persists every
// engine transition before the effect it authorizes, performs dispatch contact
// only through an injected Transport, and feeds authenticated observations back
// to the deterministic engine. It never resends an ambiguous dispatch.
package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/kognition-ai/kognition/internal/collaboration"
)

var (
	// ErrNotFound reports that no ledger exists yet.
	ErrNotFound = errors.New("driver: ledger not found")
	// ErrExists reports that a ledger already exists.
	ErrExists = errors.New("driver: ledger already exists")
	// ErrConflict reports a lost compare-and-swap; the caller reloads and retries.
	ErrConflict = errors.New("driver: ledger version conflict")
	// ErrRefused is returned by Transport.Admit when the dispatch was denied
	// before any contact. It must wrap a reason that is safe to record.
	ErrRefused = errors.New("driver: dispatch refused")
)

// maxConflictRetries bounds re-application of one event after lost CAS races.
const maxConflictRetries = 5

// Version is an opaque ledger revision used for compare-and-swap.
type Version string

// Ledger durably stores one activation's collaboration state. Implementations
// must make Save conditional on the expected Version.
type Ledger interface {
	Load(ctx context.Context) (collaboration.State, Version, error)
	Create(ctx context.Context, st collaboration.State) (Version, error)
	Save(ctx context.Context, st collaboration.State, expect Version) (Version, error)
}

// Observation is the authenticated remote outcome of one dispatch.
type Observation struct {
	// Settled is false while the remote task is still working or unknown.
	Settled bool
	// Actor is the authenticated responding principal's member alias.
	Actor string
	// Accepted answers a handoff Prepare dispatch.
	Accepted bool

	Status      collaboration.ReportStatus
	Operations  []collaboration.Operation
	Failure     string
	TokensUsed  int64
	TokensKnown bool
}

// Transport performs governed contact with members. Admit is the final
// dispatch-admission fence; Send is made only after the commit is durable.
type Transport interface {
	// Admit returns an error wrapping ErrRefused when the dispatch may not
	// contact the member. Any other error leaves the dispatch undecided.
	Admit(ctx context.Context, msg collaboration.Message) error
	// Send contacts the member once. Any error is recorded as uncertain.
	Send(ctx context.Context, msg collaboration.Message) error
	// Observe reads the remote outcome of a previously contacted dispatch.
	Observe(ctx context.Context, msg collaboration.Message) (Observation, error)
}

// Driver advances one activation.
type Driver struct {
	engine    *collaboration.Engine
	ledger    Ledger
	transport Transport
}

// New returns a Driver. All dependencies are required.
func New(engine *collaboration.Engine, ledger Ledger, transport Transport) (*Driver, error) {
	if engine == nil || ledger == nil || transport == nil {
		return nil, errors.New("driver: engine, ledger and transport are required")
	}
	return &Driver{engine: engine, ledger: ledger, transport: transport}, nil
}

// Begin creates the ledger for the activation input. It is idempotent: an
// existing ledger is left untouched.
func (d *Driver) Begin(ctx context.Context, input string) error {
	st, err := d.engine.Start(input)
	if err != nil {
		return err
	}
	if _, err := d.ledger.Create(ctx, st); err != nil && !errors.Is(err, ErrExists) {
		return err
	}
	return nil
}

// Step makes the progress that is currently possible and returns the resulting
// ledger. A false done means the caller should call Step again later.
func (d *Driver) Step(ctx context.Context) (st collaboration.State, done bool, err error) {
	st, ver, err := d.ledger.Load(ctx)
	if err != nil {
		return collaboration.State{}, false, err
	}
	if st.Phase.Terminal() {
		return st, true, nil
	}
	if st, ver, err = d.recover(ctx, st, ver); err != nil {
		return st, false, err
	}
	for pass := 0; pass < maxPasses; pass++ {
		var moved bool
		if st, ver, moved, err = d.dispatchPending(ctx, st, ver); err != nil {
			return st, st.Phase.Terminal(), err
		}
		var observed bool
		if st, ver, observed, err = d.observe(ctx, st, ver); err != nil {
			return st, st.Phase.Terminal(), err
		}
		if st.Phase.Terminal() {
			return st, true, nil
		}
		if !moved && !observed {
			break
		}
	}
	return st, st.Phase.Terminal(), nil
}

// Stop records a stop request so remaining work drains deterministically.
func (d *Driver) Stop(ctx context.Context, phase collaboration.Phase, reason string) (collaboration.State, error) {
	return d.mutate(ctx, func(st collaboration.State) (collaboration.State, error) {
		if st.Phase.Terminal() || st.Phase == collaboration.PhaseStopping {
			return st, nil
		}
		return d.engine.Apply(st, collaboration.Stop{Phase: phase, Reason: reason})
	})
}

// Abandon closes an Uncertain task that can no longer settle.
func (d *Driver) Abandon(ctx context.Context, taskID, reason string) (collaboration.State, error) {
	return d.mutate(ctx, func(st collaboration.State) (collaboration.State, error) {
		if st.Phase.Terminal() {
			return st, nil
		}
		return d.engine.Apply(st, collaboration.TaskAbandoned{TaskID: taskID, Reason: reason})
	})
}

// maxPasses bounds the work of one Step so a reconciler yields regularly.
const maxPasses = 64

func (d *Driver) recover(ctx context.Context, st collaboration.State, ver Version) (collaboration.State, Version, error) {
	need := false
	for _, x := range st.Dispatches {
		if x.Status == collaboration.DispatchCommitted && x.Kind != collaboration.DispatchCancel {
			need = true
			break
		}
	}
	if !need {
		return st, ver, nil
	}
	next, err := d.engine.Recover(st)
	if err != nil {
		return st, ver, err
	}
	nv, err := d.ledger.Save(ctx, next, ver)
	if err != nil {
		return st, ver, err
	}
	return next, nv, nil
}

// dispatchPending persists and performs every Intended dispatch.
func (d *Driver) dispatchPending(ctx context.Context, st collaboration.State, ver Version) (collaboration.State, Version, bool, error) {
	moved := false
	for _, p := range st.Pending() {
		msg, err := d.engine.Message(st, p.ID)
		if err != nil {
			return st, ver, moved, err
		}
		if err := d.transport.Admit(ctx, msg); err != nil {
			if !errors.Is(err, ErrRefused) {
				return st, ver, moved, err
			}
			if st, ver, err = d.apply(ctx, st, ver, collaboration.RefuseDispatch{ID: p.ID, Reason: err.Error()}); err != nil {
				return st, ver, moved, err
			}
			moved = true
			continue
		}
		if st, ver, err = d.apply(ctx, st, ver, collaboration.CommitDispatch{ID: p.ID}); err != nil {
			return st, ver, moved, err
		}
		moved = true
		sendErr := d.transport.Send(ctx, msg)
		var ev collaboration.Event
		switch {
		case sendErr != nil:
			ev = collaboration.UncertainDispatch{ID: p.ID, Reason: "SendOutcomeUnknown"}
		case p.Kind == collaboration.DispatchCancel:
			continue // the remote cancel is confirmed by observation
		default:
			ev = collaboration.AckDispatch{ID: p.ID}
		}
		if st, ver, err = d.apply(ctx, st, ver, ev); err != nil {
			return st, ver, moved, err
		}
		if st.Phase.Terminal() {
			return st, ver, moved, nil
		}
	}
	return st, ver, moved, nil
}

// observe polls every contacted dispatch that still awaits a remote outcome.
func (d *Driver) observe(ctx context.Context, st collaboration.State, ver Version) (collaboration.State, Version, bool, error) {
	moved := false
	for _, x := range awaiting(st) {
		msg, err := d.engine.Message(st, x.ID)
		if err != nil {
			return st, ver, moved, err
		}
		obs, err := d.transport.Observe(ctx, msg)
		if err != nil || !obs.Settled {
			continue
		}
		ev, ok := event(msg, obs)
		if !ok {
			continue
		}
		next, nv, err := d.apply(ctx, st, ver, ev)
		if err != nil {
			return st, ver, moved, err
		}
		st, ver, moved = next, nv, true
		if st.Phase.Terminal() {
			return st, ver, moved, nil
		}
	}
	return st, ver, moved, nil
}

func event(msg collaboration.Message, obs Observation) (collaboration.Event, bool) {
	switch msg.Kind {
	case collaboration.DispatchPrepare:
		return collaboration.TransferAck{TransferID: msg.Transfer.ID, Actor: obs.Actor, Accepted: obs.Accepted}, true
	case collaboration.DispatchCancel:
		if obs.Status != collaboration.ReportCancelled {
			return nil, false
		}
		return collaboration.AckDispatch{ID: msg.DispatchID}, true
	}
	return collaboration.TurnReport{
		TaskID: msg.TaskID, Turn: msg.Turn, Actor: obs.Actor, Epoch: msg.Epoch,
		Status: obs.Status, Operations: obs.Operations, Failure: obs.Failure,
		TokensUsed: obs.TokensUsed, TokensKnown: obs.TokensKnown,
	}, true
}

// awaiting lists contacted dispatches whose remote outcome is still needed.
func awaiting(st collaboration.State) []collaboration.Dispatch {
	var out []collaboration.Dispatch
	for _, x := range st.Dispatches {
		if x.Status != collaboration.DispatchSent && x.Status != collaboration.DispatchUncertain && x.Status != collaboration.DispatchCommitted {
			continue
		}
		switch x.Kind {
		case collaboration.DispatchStart, collaboration.DispatchContinue:
			t, ok := st.Task(x.TaskID)
			if !ok || x.Status == collaboration.DispatchCommitted ||
				(t.State != collaboration.TaskRunning && t.State != collaboration.TaskUncertain) ||
				x.ID != fmt.Sprintf("%s.%d", t.ID, t.Turns) {
				continue
			}
		case collaboration.DispatchPrepare:
			tr := st.Transfer
			if x.Status == collaboration.DispatchCommitted || tr == nil || tr.ID != x.TransferID ||
				(tr.Status != collaboration.TransferProposed && tr.Status != collaboration.TransferUncertain) {
				continue
			}
		case collaboration.DispatchCancel:
			if x.Status == collaboration.DispatchSent {
				continue
			}
		}
		out = append(out, x)
	}
	return out
}

// apply persists one event; a lost race aborts the Step and the next Step reloads.
func (d *Driver) apply(ctx context.Context, st collaboration.State, ver Version, ev collaboration.Event) (collaboration.State, Version, error) {
	next, err := d.engine.Apply(st, ev)
	if err != nil {
		return st, ver, err
	}
	nv, err := d.ledger.Save(ctx, next, ver)
	if err != nil {
		return st, ver, err
	}
	return next, nv, nil
}

// mutate applies f to the latest ledger and saves it, retrying lost races.
func (d *Driver) mutate(ctx context.Context, f func(collaboration.State) (collaboration.State, error)) (collaboration.State, error) {
	var lastErr error
	for i := 0; i < maxConflictRetries; i++ {
		st, ver, err := d.ledger.Load(ctx)
		if err != nil {
			return collaboration.State{}, err
		}
		next, err := f(st)
		if err != nil {
			return st, err
		}
		if _, err = d.ledger.Save(ctx, next, ver); err == nil {
			return next, nil
		}
		if !errors.Is(err, ErrConflict) {
			return st, err
		}
		lastErr = err
	}
	return collaboration.State{}, lastErr
}
