// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestToolClassValidator_AcceptsValidToolClass(t *testing.T) {
	v := NewToolClassValidator()
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "mcp-sidecar", Namespace: "default"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierSidecar,
			Transport:     v1alpha1.TransportSSE,
		},
	}
	data, _ := json.Marshal(tc)
	req := &Request{Operation: OperationCreate, Kind: "ToolClass", Namespace: "default", Name: "mcp-sidecar", Object: data}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected valid ToolClass to be accepted, got reason: %s", resp.Reason)
	}
}

func TestToolClassValidator_RejectsMissingProvisioner(t *testing.T) {
	v := NewToolClassValidator()
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "missing-provisioner"},
		Spec:       v1alpha1.ToolClassSpec{Provisioner: " \t"},
	}
	data, _ := json.Marshal(tc)
	req := &Request{Operation: OperationCreate, Kind: "ToolClass", Name: "missing-provisioner", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for missing provisioner")
	}
	if resp.Reason != "toolClass.spec.provisioner is required" {
		t.Fatalf("reason = %q, want missing provisioner validation error", resp.Reason)
	}
}

func TestToolClassValidator_RejectsInvalidExecutionTier(t *testing.T) {
	v := NewToolClassValidator()
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bad-tier", Namespace: "default"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: "invalid-tier",
			Transport:     v1alpha1.TransportSSE,
		},
	}
	data, _ := json.Marshal(tc)
	req := &Request{Operation: OperationCreate, Kind: "ToolClass", Namespace: "default", Name: "bad-tier", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid executionTier")
	}
}

func TestToolClassValidator_RejectsSandboxImageOutsidePodTier(t *testing.T) {
	v := NewToolClassValidator()
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "sidecar-image"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierSidecar,
			SandboxImage:  "registry.example/probe:1.0",
		},
	}
	data, _ := json.Marshal(tc)
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "ToolClass", Name: tc.Name, Object: data})
	if resp.Allowed || resp.Reason != "toolClass.spec.sandboxImage requires executionTier pod" {
		t.Fatalf("ToolClass validation = allowed:%t reason:%q", resp.Allowed, resp.Reason)
	}
}

func TestToolClassValidator_RejectsInvalidTransport(t *testing.T) {
	v := NewToolClassValidator()
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bad-transport", Namespace: "default"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierProcess,
			Transport:     "grpc",
		},
	}
	data, _ := json.Marshal(tc)
	req := &Request{Operation: OperationCreate, Kind: "ToolClass", Namespace: "default", Name: "bad-transport", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid transport")
	}
}

func TestToolClassValidator_RejectsNegativeResourceLimits(t *testing.T) {
	v := NewToolClassValidator()
	tc := v1alpha1.ToolClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ToolClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bad-limits", Namespace: "default"},
		Spec: v1alpha1.ToolClassSpec{
			Provisioner:   "mcp",
			ExecutionTier: v1alpha1.ExecutionTierPod,
			Transport:     v1alpha1.TransportHTTP,
			ResourceLimits: &v1alpha1.ToolClassResourceLimits{
				MaxCPUMilli: -1,
				MaxMemoryMB: 512,
				MaxTimeout:  "60s",
			},
		},
	}
	data, _ := json.Marshal(tc)
	req := &Request{Operation: OperationCreate, Kind: "ToolClass", Namespace: "default", Name: "bad-limits", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for negative resource limits")
	}
}

func TestToolClassValidator_HandlesOnlyToolClass(t *testing.T) {
	v := NewToolClassValidator()
	if v.Handles("Tool", OperationCreate) {
		t.Fatal("should not handle Tool kind")
	}
	if !v.Handles("ToolClass", OperationCreate) {
		t.Fatal("should handle ToolClass CREATE")
	}
	if !v.Handles("ToolClass", OperationUpdate) {
		t.Fatal("should handle ToolClass UPDATE")
	}
	if v.Handles("ToolClass", Operation("DELETE")) {
		t.Fatal("should not handle ToolClass DELETE")
	}
}

func TestToolClassValidator_RejectsInvalidJSON(t *testing.T) {
	v := NewToolClassValidator()
	req := &Request{Operation: OperationCreate, Kind: "ToolClass", Namespace: "default", Name: "bad", Object: []byte(`{invalid`)}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid JSON")
	}
}
