// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const (
	// SchemaVersion is the Kognition coordination payload version carried in a
	// typed DataPart under the Kognition A2A extension.
	SchemaVersion = "kognition.collaboration/v1"

	// MediaType is the DataPart media type of a coordination payload.
	MediaType = "application/vnd.kognition.collaboration.operations+json"

	// UsageMetadataKey names the result artifact metadata entry carrying a member's turn token usage.
	UsageMetadataKey = "kognition.dev/usage"

	// MaxOperations bounds the operations in one payload.
	MaxOperations = 16

	// MaxPayloadBytes bounds an encoded payload before parsing.
	MaxPayloadBytes = 64 << 10
)

// OperationKind is a coordination operation.
type OperationKind string

const (
	OpDelegate OperationKind = "delegate"
	OpHandoff  OperationKind = "handoff"
	OpComplete OperationKind = "complete"
	OpFinalize OperationKind = "finalize"
)

var operationIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Operation is one structured proposal. It carries no actor, epoch, task or
// run identity: those come from trusted execution state, and an unknown field
// such as an actor claim fails the strict decode.
type Operation struct {
	Kind        OperationKind `json:"kind"`
	OperationID string        `json:"operationId,omitempty"`
	To          string        `json:"to,omitempty"`
	Goal        string        `json:"goal,omitempty"`
	Context     string        `json:"context,omitempty"`
	Result      string        `json:"result,omitempty"`
	Required    *bool         `json:"required,omitempty"`
}

type payload struct {
	SchemaVersion string      `json:"schemaVersion"`
	Operations    []Operation `json:"operations"`
}

// ErrInvalidPayload rejects a malformed coordination payload.
var ErrInvalidPayload = errors.New("collaboration: invalid operation payload")

// EncodeOperations renders a payload for a member adapter to emit.
func EncodeOperations(ops []Operation) ([]byte, error) {
	if err := ValidateOperations(ops); err != nil {
		return nil, err
	}
	return json.Marshal(payload{SchemaVersion: SchemaVersion, Operations: ops})
}

// DecodeOperations strictly parses and validates a coordination payload.
func DecodeOperations(raw []byte) ([]Operation, error) {
	if len(raw) == 0 || len(raw) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: size", ErrInvalidPayload)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p payload
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPayload, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidPayload)
	}
	if p.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: unsupported schemaVersion %q", ErrInvalidPayload, p.SchemaVersion)
	}
	if err := ValidateOperations(p.Operations); err != nil {
		return nil, err
	}
	return p.Operations, nil
}

// ValidateOperations checks the structural rules that do not need ledger state.
func ValidateOperations(ops []Operation) error {
	if len(ops) == 0 || len(ops) > MaxOperations {
		return fmt.Errorf("%w: operation count", ErrInvalidPayload)
	}
	ids := map[string]struct{}{}
	for i, op := range ops {
		switch op.Kind {
		case OpDelegate:
			if op.To == "" || op.Goal == "" || op.Context != "" || op.Result != "" {
				return fmt.Errorf("%w: operations[%d] delegate fields", ErrInvalidPayload, i)
			}
		case OpHandoff:
			if op.To == "" || op.Goal != "" || op.Result != "" || op.Required != nil {
				return fmt.Errorf("%w: operations[%d] handoff fields", ErrInvalidPayload, i)
			}
		case OpComplete, OpFinalize:
			if op.Result == "" || op.To != "" || op.Goal != "" || op.Context != "" || op.Required != nil {
				return fmt.Errorf("%w: operations[%d] %s fields", ErrInvalidPayload, i, op.Kind)
			}
		default:
			return fmt.Errorf("%w: operations[%d] unknown kind %q", ErrInvalidPayload, i, op.Kind)
		}
		if op.Kind == OpDelegate || op.Kind == OpHandoff {
			if !operationIDPattern.MatchString(op.OperationID) {
				return fmt.Errorf("%w: operations[%d] operationId", ErrInvalidPayload, i)
			}
			if _, dup := ids[op.OperationID]; dup {
				return fmt.Errorf("%w: operations[%d] duplicate operationId", ErrInvalidPayload, i)
			}
			ids[op.OperationID] = struct{}{}
		} else if op.OperationID != "" {
			return fmt.Errorf("%w: operations[%d] operationId is not allowed", ErrInvalidPayload, i)
		}
		for _, f := range []struct{ name, v string }{{"goal", op.Goal}, {"context", op.Context}, {"result", op.Result}} {
			if err := contentErr(f.name, f.v); err != nil {
				return fmt.Errorf("%w: operations[%d] %w", ErrInvalidPayload, i, err)
			}
		}
	}
	return nil
}
