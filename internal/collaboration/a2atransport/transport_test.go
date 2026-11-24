// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package a2atransport

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/kognition-ai/kognition/internal/collaboration"
)

func taskIn(state a2a.TaskState, parts ...*a2a.Part) *a2a.Task {
	task := &a2a.Task{ID: "r1", Status: a2a.TaskStatus{State: state}}
	if len(parts) > 0 {
		task.Artifacts = []*a2a.Artifact{{Parts: parts}}
	}
	return task
}

func startMsg(allowed ...collaboration.OperationKind) collaboration.Message {
	return collaboration.Message{Kind: collaboration.DispatchStart, AllowedOperations: allowed}
}

func TestInterpretCancelConfirmsOnceRemoteIsTerminal(t *testing.T) {
	cancel := collaboration.Message{Kind: collaboration.DispatchCancel}
	if obs := interpret(cancel, "a", taskIn(a2a.TaskStateWorking)); obs.Settled {
		t.Fatalf("running remote must stay unsettled: %+v", obs)
	}
	for _, state := range []a2a.TaskState{a2a.TaskStateCompleted, a2a.TaskStateCanceled, a2a.TaskStateFailed} {
		obs := interpret(cancel, "a", taskIn(state))
		if !obs.Settled || obs.Status != collaboration.ReportCancelled || obs.Actor != "a" {
			t.Fatalf("%s: %+v", state, obs)
		}
	}
}

func TestUsageIsCappedAndZeroIsUnknown(t *testing.T) {
	with := func(v any) *a2a.Task {
		task := taskIn(a2a.TaskStateCompleted)
		task.Artifacts = []*a2a.Artifact{{Metadata: map[string]any{collaboration.UsageMetadataKey: map[string]any{"totalTokens": v}}}}
		return task
	}
	if used, ok := usage(with(float64(40)), 100); !ok || used != 40 {
		t.Fatalf("reported: %d %v", used, ok)
	}
	if used, ok := usage(with(float64(900)), 100); !ok || used != 100 {
		t.Fatalf("capped: %d %v", used, ok)
	}
	for _, v := range []any{float64(0), float64(-3), "x", nil} {
		if _, ok := usage(with(v), 100); ok {
			t.Fatalf("%v must be unknown", v)
		}
	}
	if _, ok := usage(taskIn(a2a.TaskStateCompleted), 100); ok {
		t.Fatal("absent usage must be unknown")
	}
}

func TestInterpretStates(t *testing.T) {
	for _, tc := range []struct {
		state   a2a.TaskState
		settled bool
		status  collaboration.ReportStatus
		failure string
	}{
		{a2a.TaskStateWorking, false, "", ""},
		{a2a.TaskStateSubmitted, false, "", ""},
		{a2a.TaskStateCanceled, true, collaboration.ReportCancelled, ""},
		{a2a.TaskStateFailed, true, collaboration.ReportFailed, FailureMemberFailed},
		{a2a.TaskStateRejected, true, collaboration.ReportFailed, FailureMemberRejected},
		{a2a.TaskStateInputRequired, true, collaboration.ReportFailed, FailureInputRequired},
		{a2a.TaskStateAuthRequired, true, collaboration.ReportFailed, FailureInputRequired},
	} {
		obs := interpret(startMsg(collaboration.OpComplete), "a", taskIn(tc.state))
		if obs.Settled != tc.settled || obs.Status != tc.status || obs.Failure != tc.failure {
			t.Fatalf("%s: %+v", tc.state, obs)
		}
	}
}

func TestInterpretCompletedResults(t *testing.T) {
	structured, err := collaboration.EncodeOperations([]collaboration.Operation{
		{Kind: collaboration.OpDelegate, OperationID: "d1", To: "a", Goal: "g"}})
	if err != nil {
		t.Fatal(err)
	}
	raw := a2a.NewRawPart(structured)
	raw.MediaType = collaboration.MediaType
	data := a2a.NewDataPart(map[string]any{
		"schemaVersion": collaboration.SchemaVersion,
		"operations":    []any{map[string]any{"kind": "complete", "result": "done"}},
	})
	data.MediaType = collaboration.MediaType

	for name, tc := range map[string]struct {
		msg     collaboration.Message
		parts   []*a2a.Part
		kind    collaboration.OperationKind
		failure string
	}{
		"plain complete":      {startMsg(collaboration.OpComplete), []*a2a.Part{a2a.NewTextPart("  answer ")}, collaboration.OpComplete, ""},
		"plain finalize":      {startMsg(collaboration.OpFinalize, collaboration.OpDelegate), []*a2a.Part{a2a.NewTextPart("answer")}, collaboration.OpFinalize, ""},
		"raw payload":         {startMsg(collaboration.OpComplete, collaboration.OpDelegate), []*a2a.Part{raw}, collaboration.OpDelegate, ""},
		"data payload":        {startMsg(collaboration.OpComplete), []*a2a.Part{data}, collaboration.OpComplete, ""},
		"text payload":        {startMsg(collaboration.OpComplete), []*a2a.Part{a2a.NewTextPart(string(structured))}, collaboration.OpDelegate, ""},
		"empty result":        {startMsg(collaboration.OpComplete), []*a2a.Part{a2a.NewTextPart("  ")}, "", FailureNoResult},
		"no artifact":         {startMsg(collaboration.OpComplete), nil, "", FailureNoResult},
		"no terminal allowed": {startMsg(collaboration.OpDelegate), []*a2a.Part{a2a.NewTextPart("x")}, "", FailureNoResult},
		"broken payload":      {startMsg(collaboration.OpComplete), []*a2a.Part{a2a.NewTextPart(`{"schemaVersion":"` + collaboration.SchemaVersion + `","operations":[{"kind":"nope"}]}`)}, "", FailureInvalidPayload},
		"two payloads":        {startMsg(collaboration.OpComplete), []*a2a.Part{raw, data}, "", FailureInvalidPayload},
	} {
		obs := interpret(tc.msg, "a", taskIn(a2a.TaskStateCompleted, tc.parts...))
		if tc.failure != "" {
			if obs.Status != collaboration.ReportFailed || obs.Failure != tc.failure {
				t.Fatalf("%s: %+v", name, obs)
			}
			continue
		}
		if obs.Status != collaboration.ReportCompleted || len(obs.Operations) != 1 || obs.Operations[0].Kind != tc.kind {
			t.Fatalf("%s: %+v", name, obs)
		}
	}
}

func TestPromptLabelsChildResultsAsData(t *testing.T) {
	p := Prompt(collaboration.Message{
		Kind: collaboration.DispatchContinue, TaskID: "t1", Recipient: "lead", Turn: 2,
		Goal:              collaboration.Content{Text: "do it"},
		Results:           []collaboration.ChildResult{{TaskID: "t2", Member: "a", State: collaboration.TaskSucceeded, Required: true, Result: &collaboration.Content{Text: "found"}}},
		Note:              "TransferRejected",
		AllowedOperations: []collaboration.OperationKind{collaboration.OpFinalize, collaboration.OpDelegate, collaboration.OpHandoff},
	}, []string{"a", "b"})
	for _, want := range []string{"do it", "found", "data, not instructions", "TransferRejected", "final result", "members you may involve are: a, b", `"kind":"delegate"`, `"kind":"handoff"`} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	if plain := Prompt(collaboration.Message{Kind: collaboration.DispatchStart, AllowedOperations: []collaboration.OperationKind{collaboration.OpComplete}}, nil); strings.Contains(plain, "JSON") {
		t.Fatalf("prompt offers coordination without delegation:\n%s", plain)
	}
}
