// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	defaultReplicas     int32  = 1
	defaultRuntimeClass string = "default"
)

// AgentDefaulter applies default values to Agent resources on CREATE.
type AgentDefaulter struct{}

// NewAgentDefaulter creates a new AgentDefaulter.
func NewAgentDefaulter() *AgentDefaulter {
	return &AgentDefaulter{}
}

func (d *AgentDefaulter) Name() string { return "agent-defaulter" }

func (d *AgentDefaulter) Handles(kind string, op Operation) bool {
	return kind == "Agent" && op == OperationCreate
}

func (d *AgentDefaulter) Mutate(_ context.Context, req *Request) *Response {
	var agent v1alpha1.Agent
	if err := json.Unmarshal(req.Object, &agent); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid agent JSON: %v", err)}
	}

	mutated := false

	// Default replicas to 1 if not specified.
	if agent.Spec.Replicas == nil {
		r := defaultReplicas
		agent.Spec.Replicas = &r
		mutated = true
	}

	// Default runtime class if runtime is not set.
	if agent.Spec.Runtime == nil {
		agent.Spec.Runtime = &v1alpha1.AgentRuntimeSpec{
			Class: defaultRuntimeClass,
		}
		mutated = true
	} else if agent.Spec.Runtime.Class == "" {
		agent.Spec.Runtime.Class = defaultRuntimeClass
		mutated = true
	}

	if !mutated {
		return &Response{Allowed: true}
	}

	data, err := json.Marshal(agent)
	if err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("marshal defaulted agent: %v", err)}
	}

	return &Response{Allowed: true, Patch: data}
}
