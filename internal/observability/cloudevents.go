// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// CloudEvents v1.0 specification version.
const CloudEventsSpecVersion = "1.0"

// Event type URIs for kognition internal events.
const (
	EventTypeReasoningStep = "kognition.reasoning.step"
	EventTypeToolCall      = "kognition.tool.call"
	EventTypeMemoryOp      = "kognition.memory.op"
	EventTypeCostRecord    = "kognition.cost.record"
	EventTypeAuditLog      = "kognition.audit.log"
	EventTypeSpecChange    = "kognition.spec.change"
	EventTypeGrantChange   = "kognition.grant.change"
)

// CloudEvent is the Go representation of a CNCF CloudEvents v1.0 envelope.
// All internal kognition events are wrapped in this format for transport and storage.
type CloudEvent struct {
	// Required attributes.
	SpecVersion string    `json:"specversion"`
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`
	Time        time.Time `json:"time"`

	// Optional attributes.
	DataContentType string `json:"datacontenttype,omitempty"`
	DataSchema      string `json:"dataschema,omitempty"`
	Subject         string `json:"subject,omitempty"`

	// Protobuf-encoded data (stored as JSON for non-proto consumers).
	Data json.RawMessage `json:"data,omitempty"`

	// Extension attributes (kognition-specific metadata).
	Extensions map[string]string `json:"extensions,omitempty"`
}

// NewCloudEvent creates a new CloudEvent with required fields populated.
func NewCloudEvent(eventType, source string, data interface{}) (*CloudEvent, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	return &CloudEvent{
		SpecVersion:     CloudEventsSpecVersion,
		ID:              uuid.New().String(),
		Type:            eventType,
		Source:          source,
		Time:            time.Now().UTC(),
		DataContentType: "application/json",
		Data:            encoded,
	}, nil
}

// WithSubject sets the event subject.
func (e *CloudEvent) WithSubject(subject string) *CloudEvent {
	e.Subject = subject
	return e
}

// WithExtension adds an extension attribute.
func (e *CloudEvent) WithExtension(key, value string) *CloudEvent {
	if e.Extensions == nil {
		e.Extensions = make(map[string]string)
	}
	e.Extensions[key] = value
	return e
}

// Validate checks that the CloudEvent conforms to the v1.0 specification.
func (e *CloudEvent) Validate() error {
	if e.SpecVersion != CloudEventsSpecVersion {
		return &ValidationError{Field: "specversion", Message: "must be 1.0"}
	}
	if e.ID == "" {
		return &ValidationError{Field: "id", Message: "required"}
	}
	if e.Type == "" {
		return &ValidationError{Field: "type", Message: "required"}
	}
	if e.Source == "" {
		return &ValidationError{Field: "source", Message: "required"}
	}
	if e.Time.IsZero() {
		return &ValidationError{Field: "time", Message: "required"}
	}
	return nil
}

// ValidationError represents a CloudEvent validation failure.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return "cloudevent validation: " + e.Field + " " + e.Message
}

// EventSource builds the canonical source URI for a workspace/agent.
func EventSource(workspace, agentNamespace, agentName string) string {
	return "/kognition/workspaces/" + workspace + "/namespaces/" + agentNamespace + "/agents/" + agentName
}
