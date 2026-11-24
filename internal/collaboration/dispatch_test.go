// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func sampleDispatchMessage() Message {
	return Message{
		DispatchID: "t1.1", Kind: DispatchStart, TaskID: "t1", Recipient: "lead", Requester: "boss", Turn: 1, Epoch: 2, Allowance: 500,
		AllowedOperations: []OperationKind{OpComplete, OpDelegate},
	}
}

func TestDispatchPayloadRoundTripIsDeterministic(t *testing.T) {
	first, err := EncodeDispatch(sampleDispatchMessage(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeDispatch(sampleDispatchMessage(), "do it")
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("encoding is not deterministic: %v", err)
	}
	decoded, err := DecodeDispatch(first)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.DispatchID != "t1.1" || decoded.Recipient != "lead" || decoded.Requester != "boss" || decoded.Epoch != 2 || decoded.Prompt != "do it" ||
		len(decoded.AllowedOperations) != 2 || decoded.Allowance != 500 {
		t.Fatalf("%+v", decoded)
	}
}

func TestEncodeDispatchRejectsInvalidMessages(t *testing.T) {
	cases := map[string]func(*Message) string{
		"no dispatch id":  func(m *Message) string { m.DispatchID = ""; return "x" },
		"no task":         func(m *Message) string { m.TaskID = ""; return "x" },
		"no recipient":    func(m *Message) string { m.Recipient = ""; return "x" },
		"cancel":          func(m *Message) string { m.Kind = DispatchCancel; return "x" },
		"no prompt":       func(m *Message) string { return "" },
		"negative budget": func(m *Message) string { m.Allowance = -1; return "x" },
		"unknown op":      func(m *Message) string { m.AllowedOperations = []OperationKind{"root"}; return "x" },
		"oversized":       func(m *Message) string { return strings.Repeat("a", MaxDispatchPayloadBytes) },
	}
	for name, mutate := range cases {
		msg := sampleDispatchMessage()
		prompt := mutate(&msg)
		if _, err := EncodeDispatch(msg, prompt); !errors.Is(err, ErrInvalidDispatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestDecodeDispatchClassifiesDocuments(t *testing.T) {
	valid, err := EncodeDispatch(sampleDispatchMessage(), "do it")
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"plain text":      "hello",
		"other schema":    `{"schemaVersion":"other/v1"}`,
		"no schema":       `{"prompt":"x"}`,
		"array":           `[1]`,
		"oversize":        `{"schemaVersion":"` + DispatchSchemaVersion + `"}` + strings.Repeat(" ", MaxDispatchPayloadBytes),
		"empty":           ``,
		"declared string": `"` + DispatchSchemaVersion + `"`,
	} {
		if _, err := DecodeDispatch([]byte(raw)); !errors.Is(err, ErrNotDispatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(string(valid), `"prompt"`, `"actor":"x","prompt"`, 1),
		"trailing":      string(valid) + ` {}`,
		"bad kind":      strings.Replace(string(valid), `"Start"`, `"Cancel"`, 1),
		"wrong type":    strings.Replace(string(valid), `"turn":1`, `"turn":"1"`, 1),
	} {
		if _, err := DecodeDispatch([]byte(raw)); !errors.Is(err, ErrInvalidDispatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
