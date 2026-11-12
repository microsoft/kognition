// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestAgentServiceValidator(t *testing.T) {
	validator := NewAgentServiceValidator()
	for _, test := range []struct {
		name    string
		service v1alpha1.AgentService
		allowed bool
	}{
		{name: "valid unpinned", service: v1alpha1.AgentService{Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}}, allowed: true},
		{name: "missing agent", service: v1alpha1.AgentService{}, allowed: false},
		{name: "zero revision", service: v1alpha1.AgentService{Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, RevisionPin: int64Ptr(0)}}, allowed: false},
		{name: "negative revision", service: v1alpha1.AgentService{Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, RevisionPin: int64Ptr(-1)}}, allowed: false},
		{name: "positive revision", service: v1alpha1.AgentService{Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}, RevisionPin: int64Ptr(1)}}, allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.service)
			if err != nil {
				t.Fatal(err)
			}
			response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "AgentService", Object: data})
			if response.Allowed != test.allowed {
				t.Fatalf("Allowed = %t, want %t; reason = %s", response.Allowed, test.allowed, response.Reason)
			}
		})
	}
}

func TestProductionHooksIncludeAgentServiceValidator(t *testing.T) {
	_, validators := ProductionHooks(nil, nil)
	for _, validator := range validators {
		if validator.Name() == "agent-service-validator" {
			return
		}
	}
	t.Fatal("ProductionHooks does not include agent-service-validator")
}

func int64Ptr(value int64) *int64 { return &value }
