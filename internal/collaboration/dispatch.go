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

const (
	// DispatchSchemaVersion is the version of the signed dispatch payload.
	DispatchSchemaVersion = "kognition.collaboration.dispatch/v1"

	// MaxDispatchPayloadBytes bounds one encoded dispatch payload.
	MaxDispatchPayloadBytes = 512 << 10
)

// ErrNotDispatch reports that a JSON document is not a collaboration dispatch payload.
var ErrNotDispatch = errors.New("collaboration: not a dispatch payload")

// ErrInvalidDispatch rejects a malformed dispatch payload.
var ErrInvalidDispatch = errors.New("collaboration: invalid dispatch payload")

// DispatchPayload is the exact document the executor signs for one governed
// dispatch. The prompt is the only text a member is asked to act on; the other
// fields restate trusted execution state so the payload is self-describing.
type DispatchPayload struct {
	SchemaVersion     string          `json:"schemaVersion"`
	DispatchID        string          `json:"dispatchId"`
	Kind              DispatchKind    `json:"kind"`
	TaskID            string          `json:"taskId"`
	Turn              int32           `json:"turn"`
	Epoch             uint64          `json:"epoch"`
	Recipient         string          `json:"recipient"`
	Requester         string          `json:"requester,omitempty"`
	Allowance         int64           `json:"allowance,omitempty"`
	AllowedOperations []OperationKind `json:"allowedOperations,omitempty"`
	Prompt            string          `json:"prompt"`
}

// EncodeDispatch renders the deterministic payload of one dispatch.
func EncodeDispatch(msg Message, prompt string) ([]byte, error) {
	payload := DispatchPayload{
		SchemaVersion: DispatchSchemaVersion, DispatchID: msg.DispatchID, Kind: msg.Kind, TaskID: msg.TaskID, Turn: msg.Turn,
		Epoch: msg.Epoch, Recipient: msg.Recipient, Requester: msg.Requester, Allowance: msg.Allowance,
		AllowedOperations: msg.AllowedOperations, Prompt: prompt,
	}
	if err := validateDispatch(payload); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxDispatchPayloadBytes {
		return nil, fmt.Errorf("%w: size", ErrInvalidDispatch)
	}
	return raw, nil
}

// DecodeDispatch strictly parses a dispatch payload. A JSON document that does
// not declare the dispatch schema yields ErrNotDispatch so callers can treat it as plain input.
func DecodeDispatch(raw []byte) (DispatchPayload, error) {
	var probe struct {
		SchemaVersion string `json:"schemaVersion"`
	}
	if len(raw) > MaxDispatchPayloadBytes || json.NewDecoder(bytes.NewReader(raw)).Decode(&probe) != nil || probe.SchemaVersion != DispatchSchemaVersion {
		return DispatchPayload{}, ErrNotDispatch
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var payload DispatchPayload
	if err := dec.Decode(&payload); err != nil {
		return DispatchPayload{}, fmt.Errorf("%w: %w", ErrInvalidDispatch, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return DispatchPayload{}, fmt.Errorf("%w: trailing data", ErrInvalidDispatch)
	}
	if err := validateDispatch(payload); err != nil {
		return DispatchPayload{}, err
	}
	return payload, nil
}

func validateDispatch(p DispatchPayload) error {
	switch {
	case p.SchemaVersion != DispatchSchemaVersion:
		return fmt.Errorf("%w: schemaVersion", ErrInvalidDispatch)
	case p.DispatchID == "" || p.TaskID == "" || p.Recipient == "":
		return fmt.Errorf("%w: identity", ErrInvalidDispatch)
	case p.Kind != DispatchStart && p.Kind != DispatchContinue && p.Kind != DispatchPrepare:
		return fmt.Errorf("%w: kind %q", ErrInvalidDispatch, p.Kind)
	case p.Prompt == "":
		return fmt.Errorf("%w: prompt", ErrInvalidDispatch)
	case p.Allowance < 0:
		return fmt.Errorf("%w: allowance", ErrInvalidDispatch)
	}
	for _, kind := range p.AllowedOperations {
		switch kind {
		case OpDelegate, OpHandoff, OpComplete, OpFinalize:
		default:
			return fmt.Errorf("%w: operation %q", ErrInvalidDispatch, kind)
		}
	}
	return nil
}
