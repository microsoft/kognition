// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidState rejects a ledger that fails strict decoding or integrity checks.
var ErrInvalidState = errors.New("collaboration: invalid ledger")

// Encode renders the ledger for persistence and enforces the size bound.
func Encode(st State) ([]byte, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxStateBytes && !st.Phase.Terminal() {
		return nil, ErrStateTooLarge
	}
	return raw, nil
}

// Decode strictly parses a persisted ledger and checks its integrity.
func Decode(raw []byte) (State, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return State{}, fmt.Errorf("%w: size", ErrInvalidState)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var st State
	if err := dec.Decode(&st); err != nil {
		return State{}, fmt.Errorf("%w: %w", ErrInvalidState, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return State{}, fmt.Errorf("%w: trailing data", ErrInvalidState)
	}
	if err := st.validate(); err != nil {
		return State{}, fmt.Errorf("%w: %w", ErrInvalidState, err)
	}
	return st, nil
}

func (s *State) validate() error {
	if s.Version != StateVersion {
		return fmt.Errorf("unsupported version %d", s.Version)
	}
	switch s.Phase {
	case PhaseRunning, PhaseStopping, PhaseSucceeded, PhaseFailed, PhaseCancelled:
	default:
		return fmt.Errorf("unknown phase %q", s.Phase)
	}
	if (s.Phase == PhaseStopping || s.Phase.Terminal()) && s.Outcome == nil {
		return errors.New("missing outcome")
	}
	tasks := map[string]struct{}{}
	for _, t := range s.Tasks {
		if _, dup := tasks[t.ID]; dup || t.ID == "" {
			return fmt.Errorf("duplicate or empty task id %q", t.ID)
		}
		tasks[t.ID] = struct{}{}
		switch t.State {
		case TaskPending, TaskRunning, TaskWaiting, TaskUncertain, TaskSucceeded, TaskFailed, TaskCancelled:
		default:
			return fmt.Errorf("task %s has unknown state %q", t.ID, t.State)
		}
		if t.Turns < 0 || t.Depth < 0 {
			return fmt.Errorf("task %s has a negative counter", t.ID)
		}
	}
	seen := map[string]struct{}{}
	for _, d := range s.Dispatches {
		if _, dup := seen[d.ID]; dup || d.ID == "" {
			return fmt.Errorf("duplicate or empty dispatch id %q", d.ID)
		}
		seen[d.ID] = struct{}{}
		if _, ok := tasks[d.TaskID]; !ok {
			return fmt.Errorf("dispatch %s references unknown task %q", d.ID, d.TaskID)
		}
		switch d.Status {
		case DispatchIntended, DispatchCommitted, DispatchSent, DispatchUncertain, DispatchRefused, DispatchReported:
		default:
			return fmt.Errorf("dispatch %s has unknown status %q", d.ID, d.Status)
		}
	}
	c := s.Counters
	if c.Turns < 0 || c.Messages < 0 || c.Delegations < 0 || c.TokensUsed < 0 || c.TokensReserved < 0 {
		return errors.New("negative counter")
	}
	return nil
}
