// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestToolValidator_RejectsMissingSecretRefName(t *testing.T) {
	v := NewToolValidator()
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "web-search",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolSpec{
			ClassName:  "process-networked",
			SecretRefs: []v1alpha1.ToolSecretRef{{Key: "API_KEY"}},
		},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "web-search", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for missing secretRef name")
	}
	if resp.Reason == "" {
		t.Fatal("expected denial reason")
	}
}

func TestToolValidator_AcceptsValidSecretRefs(t *testing.T) {
	v := NewToolValidator()
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "web-search",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolSpec{
			ClassName:  "process-networked",
			SecretRefs: []v1alpha1.ToolSecretRef{{Name: "web-search-api-key", Key: "API_KEY"}},
		},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "web-search", Object: data}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected valid tool to be accepted, got reason: %s", resp.Reason)
	}
}

func TestToolValidator_RejectsInvalidProtocol(t *testing.T) {
	v := NewToolValidator()
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-protocol",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolSpec{
			Protocol: "grpc",
			Endpoint: "https://tools.internal/bad-protocol",
		},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "bad-protocol", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid protocol")
	}
}

func TestToolValidator_RejectsInvalidDigestPin(t *testing.T) {
	v := NewToolValidator()
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-digest",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolMCP,
			Endpoint:  "https://tools.internal/bad-digest",
			DigestPin: "abc123",
		},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "bad-digest", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid digest pin")
	}
}

func TestToolValidator_RejectsInvalidSandboxTimeout(t *testing.T) {
	v := NewToolValidator()
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-sandbox-timeout",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/bad-timeout",
			Sandbox:  &v1alpha1.ToolSandboxSpec{Timeout: "soon"},
		},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "bad-sandbox-timeout", Object: data}

	resp := v.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected rejection for invalid sandbox timeout")
	}
}

func TestToolValidator_AcceptsTrustedToolWithSandboxLimits(t *testing.T) {
	v := NewToolValidator()
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Tool"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "trusted-tool",
			Namespace: "default",
		},
		Spec: v1alpha1.ToolSpec{
			Protocol: v1alpha1.ToolProtocolMCP,
			Endpoint: "https://tools.internal/trusted",
			Trusted:  true,
			Sandbox: &v1alpha1.ToolSandboxSpec{
				Timeout:  "2s",
				CPUMilli: 500,
				MemoryMB: 256,
			},
		},
	}
	data, _ := json.Marshal(tool)
	req := &Request{Operation: OperationCreate, Kind: "Tool", Namespace: "default", Name: "trusted-tool", Object: data}

	resp := v.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected trusted tool with valid sandbox limits to be accepted, got: %s", resp.Reason)
	}
}

func TestToolValidator_RejectsNonPositiveHealthProbeDurations(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval string
		timeout  string
		field    string
	}{
		{name: "zero interval", interval: "0s", timeout: "1s", field: "interval"},
		{name: "negative interval", interval: "-1s", timeout: "1s", field: "interval"},
		{name: "zero timeout", interval: "1s", timeout: "0s", field: "timeout"},
		{name: "negative timeout", interval: "1s", timeout: "-1s", field: "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := NewToolValidator()
			tool := v1alpha1.Tool{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "invalid-probe-duration", Namespace: "default"},
				Spec: v1alpha1.ToolSpec{
					Protocol: v1alpha1.ToolProtocolMCP,
					Endpoint: "https://tools.internal/health",
					HealthProbe: &v1alpha1.ToolHealthProbe{
						Type:     v1alpha1.ProbeTypeHTTPGet,
						Interval: tc.interval,
						Timeout:  tc.timeout,
					},
				},
			}
			data, _ := json.Marshal(tool)
			resp := v.Validate(context.Background(), &Request{
				Operation: OperationCreate,
				Kind:      "Tool",
				Namespace: "default",
				Name:      tool.Name,
				Object:    data,
			})
			if resp.Allowed || !strings.Contains(resp.Reason, tc.field+" must be greater than zero") {
				t.Fatalf("expected %s rejection, got allowed=%t reason=%q", tc.field, resp.Allowed, resp.Reason)
			}
		})
	}
}
