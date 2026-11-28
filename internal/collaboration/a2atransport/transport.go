// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package a2atransport maps A2A tasks to driver observations and renders member
// prompts. Member output is read as untrusted proposals.
package a2atransport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
)

// Failure reasons recorded when a member's remote task does not yield a result.
const (
	FailureMemberFailed   = "MemberFailed"
	FailureMemberRejected = "MemberRejected"
	FailureInputRequired  = "MemberRequestedInput"
	FailureInvalidPayload = "InvalidPayload"
	FailureNoResult       = "NoResult"
)

// Handles remembers the remote A2A task created for a dispatch.
type Handles interface {
	Put(ctx context.Context, dispatch, remoteTask string) error
	Get(ctx context.Context, dispatch string) (remoteTask string, ok bool, err error)
}

// HandleReplacer is a Handles that can also move a recorded handle from one
// value to the next. Put never replaces a different value, so a transport whose
// handle changes state (an intent before contact, a task after it) needs it.
type HandleReplacer interface {
	Handles
	// Replace sets next only while the recorded value is expected. It is a
	// no-op when next is already recorded and returns ErrHandleChanged otherwise.
	Replace(ctx context.Context, dispatch, expected, next string) error
}

// ErrHandleChanged reports that a handle no longer holds the value a Replace expected.
var ErrHandleChanged = errors.New("a2atransport: the recorded handle changed")

// MemoryHandles keeps handles in process memory; a restart leaves previously
// contacted dispatches unobservable, so they settle only through Abandon.
type MemoryHandles struct {
	mu sync.Mutex
	m  map[string]string
}

// Put implements Handles.
func (h *MemoryHandles) Put(_ context.Context, dispatch, remoteTask string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]string{}
	}
	h.m[dispatch] = remoteTask
	return nil
}

// Replace implements HandleReplacer.
func (h *MemoryHandles) Replace(_ context.Context, dispatch, expected, next string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, ok := h.m[dispatch]
	switch {
	case ok && cur == next:
		return nil
	case ok && cur == expected:
		h.m[dispatch] = next
		return nil
	default:
		return ErrHandleChanged
	}
}

// Get implements Handles.
func (h *MemoryHandles) Get(_ context.Context, dispatch string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.m[dispatch]
	return v, ok, nil
}

// Interpret reads a polled remote task as the authenticated outcome of msg. For a
// Cancel it reports Cancelled once the task is terminal; the task must be the one
// of the dispatch being cancelled. Member output is untrusted and only proposed.
func Interpret(msg collaboration.Message, actor string, task *a2a.Task) driver.Observation {
	return interpret(msg, actor, task)
}

func interpret(msg collaboration.Message, actor string, task *a2a.Task) driver.Observation {
	state := task.Status.State
	if msg.Kind == collaboration.DispatchCancel {
		// A cancel is confirmed once the remote task is no longer running.
		if !state.Terminal() {
			return driver.Observation{}
		}
		return driver.Observation{Settled: true, Actor: actor, Status: collaboration.ReportCancelled}
	}
	if !state.Terminal() && state != a2a.TaskStateInputRequired && state != a2a.TaskStateAuthRequired {
		return driver.Observation{}
	}
	obs := driver.Observation{Settled: true, Actor: actor}
	switch {
	case state == a2a.TaskStateCanceled:
		obs.Status = collaboration.ReportCancelled
	case state == a2a.TaskStateCompleted:
		return completed(msg, obs, task)
	case state == a2a.TaskStateRejected:
		obs.Status, obs.Failure = collaboration.ReportFailed, FailureMemberRejected
	case state == a2a.TaskStateFailed:
		obs.Status, obs.Failure = collaboration.ReportFailed, FailureMemberFailed
	default:
		obs.Status, obs.Failure = collaboration.ReportFailed, FailureInputRequired
	}
	return obs
}

func completed(msg collaboration.Message, obs driver.Observation, task *a2a.Task) driver.Observation {
	obs.TokensUsed, obs.TokensKnown = usage(task, msg.Allowance)
	text, ops, err := artifacts(task, msg.MaxContentBytes)
	if msg.Kind == collaboration.DispatchPrepare {
		obs.Status = collaboration.ReportCompleted
		obs.Accepted = err == nil && ops == nil && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(text)), "ACCEPT")
		return obs
	}
	if err != nil {
		obs.Status, obs.Failure = collaboration.ReportFailed, FailureInvalidPayload
		return obs
	}
	if ops == nil {
		ops, err = plain(msg, text)
		if err != nil {
			obs.Status, obs.Failure = collaboration.ReportFailed, FailureNoResult
			if errors.Is(err, collaboration.ErrInvalidPayload) {
				obs.Failure = FailureInvalidPayload + ": " + err.Error()
			}
			return obs
		}
	}
	obs.Status, obs.Operations = collaboration.ReportCompleted, ops
	return obs
}

// usage reads the member-reported token count, capped at the dispatch allowance.
// An absent or zero report is unknown, which the engine charges at the full allowance.
func usage(task *a2a.Task, allowance int64) (int64, bool) {
	for _, artifact := range task.Artifacts {
		reported, ok := artifact.Metadata[collaboration.UsageMetadataKey].(map[string]any)
		if !ok {
			continue
		}
		total, ok := reported["totalTokens"].(float64)
		if !ok || total <= 0 {
			continue
		}
		used := int64(total)
		if allowance > 0 && used > allowance {
			used = allowance
		}
		return used, true
	}
	return 0, false
}

// artifacts returns the concatenated text and any structured coordination payload.
func artifacts(task *a2a.Task, maxContentBytes int64) (string, []collaboration.Operation, error) {
	var text strings.Builder
	var ops []collaboration.Operation
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			switch content := part.Content.(type) {
			case a2a.Text:
				text.WriteString(string(content))
			case a2a.Raw:
				if part.MediaType != collaboration.MediaType {
					continue
				}
				decoded, err := collaboration.DecodeOperations(content, maxContentBytes)
				if err != nil || ops != nil {
					return "", nil, errors.New("invalid payload")
				}
				ops = decoded
			case a2a.Data:
				if part.MediaType != collaboration.MediaType {
					continue
				}
				raw, err := json.Marshal(content.Value)
				if err != nil {
					return "", nil, err
				}
				decoded, err := collaboration.DecodeOperations(raw, maxContentBytes)
				if err != nil || ops != nil {
					return "", nil, errors.New("invalid payload")
				}
				ops = decoded
			}
		}
	}
	result := text.String()
	if ops == nil && strings.Contains(result, collaboration.SchemaVersion) {
		decoded, err := collaboration.DecodeOperations([]byte(strings.TrimSpace(result)), maxContentBytes)
		if err != nil {
			return "", nil, err
		}
		ops = decoded
	}
	return result, ops, nil
}

// plain turns a text reply into the one operation the engine allows for the turn.
func plain(msg collaboration.Message, text string) ([]collaboration.Operation, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("empty result")
	}
	for _, kind := range msg.AllowedOperations {
		if kind == collaboration.OpFinalize || kind == collaboration.OpComplete {
			ops := []collaboration.Operation{{Kind: kind, Result: text}}
			if err := collaboration.ValidateOperations(ops, msg.MaxContentBytes); err != nil {
				return nil, err
			}
			return ops, nil
		}
	}
	return nil, errors.New("no terminal operation allowed")
}
