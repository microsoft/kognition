// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"strings"
	"testing"
)

func TestToolClassSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    ToolClassSpec
		wantErr string
	}{
		{name: "minimal", spec: ToolClassSpec{Provisioner: "mcp"}},
		{
			name: "all options",
			spec: ToolClassSpec{
				Provisioner:   "mcp",
				ExecutionTier: ExecutionTierPod,
				SandboxImage:  "registry.example/tool-probe:1.0",
				Transport:     TransportHTTP,
				ResourceLimits: &ToolClassResourceLimits{
					MaxCPUMilli: 1000,
					MaxMemoryMB: 2048,
					MaxTimeout:  "30s",
				},
			},
		},
		{name: "sandbox image requires pod tier", spec: ToolClassSpec{Provisioner: "mcp", ExecutionTier: ExecutionTierSidecar, SandboxImage: "registry.example/tool-probe:1.0"}, wantErr: "sandboxImage requires executionTier pod"},
		{name: "stdio command requires pod tier", spec: ToolClassSpec{Provisioner: "mcp", ExecutionTier: ExecutionTierSidecar, StdioCommand: []string{"/server"}}, wantErr: "stdioCommand requires executionTier pod"},
		{name: "stdio command requires image", spec: ToolClassSpec{Provisioner: "mcp", ExecutionTier: ExecutionTierPod, StdioCommand: []string{"/server"}}, wantErr: "stdioCommand requires sandboxImage"},
		{name: "stdio command rejects blank argument", spec: ToolClassSpec{Provisioner: "mcp", ExecutionTier: ExecutionTierPod, SandboxImage: "registry.example/stdio:1.0", StdioCommand: []string{"/server", " "}}, wantErr: "stdioCommand[1]"},
		{name: "blank provisioner", spec: ToolClassSpec{Provisioner: "  "}, wantErr: "provisioner is required"},
		{name: "invalid execution tier", spec: ToolClassSpec{Provisioner: "mcp", ExecutionTier: "container"}, wantErr: "executionTier"},
		{name: "invalid transport", spec: ToolClassSpec{Provisioner: "mcp", Transport: "grpc"}, wantErr: "transport"},
		{name: "invalid sandbox default timeout", spec: ToolClassSpec{Provisioner: "mcp", SandboxDefaults: &ToolSandboxSpec{Timeout: "later"}}, wantErr: "sandboxDefaults.timeout"},
		{name: "negative sandbox default cpu", spec: ToolClassSpec{Provisioner: "mcp", SandboxDefaults: &ToolSandboxSpec{CPUMilli: -1}}, wantErr: "sandboxDefaults.cpuMilli"},
		{name: "negative sandbox default memory", spec: ToolClassSpec{Provisioner: "mcp", SandboxDefaults: &ToolSandboxSpec{MemoryMB: -1}}, wantErr: "sandboxDefaults.memoryMB"},
		{
			name:    "negative cpu limit",
			spec:    ToolClassSpec{Provisioner: "mcp", ResourceLimits: &ToolClassResourceLimits{MaxCPUMilli: -1}},
			wantErr: "maxCpuMilli",
		},
		{
			name:    "negative memory limit",
			spec:    ToolClassSpec{Provisioner: "mcp", ResourceLimits: &ToolClassResourceLimits{MaxMemoryMB: -1}},
			wantErr: "maxMemoryMB",
		},
		{
			name:    "invalid timeout",
			spec:    ToolClassSpec{Provisioner: "mcp", ResourceLimits: &ToolClassResourceLimits{MaxTimeout: "later"}},
			wantErr: "maxTimeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.spec.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestToolSpecValidate(t *testing.T) {
	tests := []struct {
		name    string
		spec    ToolSpec
		wantErr string
	}{
		{name: "minimal", spec: ToolSpec{}},
		{name: "explicit protocol", spec: ToolSpec{Protocol: ToolProtocolBrowser}},
		{name: "invalid protocol", spec: ToolSpec{Protocol: "grpc"}, wantErr: "protocol"},
		{name: "secret name required", spec: ToolSpec{SecretRefs: []ToolSecretRef{{Key: "TOKEN"}}}, wantErr: "secretRefs[0].name"},
		{name: "secret key required", spec: ToolSpec{SecretRefs: []ToolSecretRef{{Name: "credential"}}}, wantErr: "secretRefs[0].key"},
		{name: "valid digest pin", spec: ToolSpec{DigestPin: "sha256:abc123"}},
		{name: "invalid digest pin", spec: ToolSpec{DigestPin: "abc123"}, wantErr: "digestPin"},
		{name: "empty digest", spec: ToolSpec{DigestPin: "sha256:"}, wantErr: "digestPin"},
		{name: "openapi schema required", spec: ToolSpec{Protocol: ToolProtocolOpenAPI}, wantErr: "schema is required"},
		{name: "openapi legacy schema", spec: ToolSpec{Protocol: ToolProtocolOpenAPI, InputSchema: map[string]any{"type": "object"}}},
		{name: "invalid sandbox timeout", spec: ToolSpec{Sandbox: &ToolSandboxSpec{Timeout: "later"}}, wantErr: "sandbox.timeout"},
		{name: "negative sandbox cpu", spec: ToolSpec{Sandbox: &ToolSandboxSpec{CPUMilli: -1}}, wantErr: "sandbox.cpuMilli"},
		{name: "negative sandbox memory", spec: ToolSpec{Sandbox: &ToolSandboxSpec{MemoryMB: -1}}, wantErr: "sandbox.memoryMB"},
		{name: "valid sandbox", spec: ToolSpec{Sandbox: &ToolSandboxSpec{Timeout: "2s", CPUMilli: 250, MemoryMB: 128}}},
		{name: "invalid probe type", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: "grpc"}}, wantErr: "healthProbe.type"},
		{name: "invalid probe interval", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeMCPPing, Interval: "later"}}, wantErr: "healthProbe.interval"},
		{name: "zero probe interval", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeMCPPing, Interval: "0s"}}, wantErr: "interval must be greater than zero"},
		{name: "invalid probe timeout", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeTCP, Timeout: "later"}}, wantErr: "healthProbe.timeout"},
		{name: "zero probe timeout", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeTCP, Timeout: "0s"}}, wantErr: "timeout must be greater than zero"},
		{name: "negative failure threshold", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeHTTPGet, FailureThreshold: -1}}, wantErr: "failureThreshold"},
		{name: "negative success threshold", spec: ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeHTTPGet, SuccessThreshold: -1}}, wantErr: "successThreshold"},
		{
			name: "valid probe",
			spec: ToolSpec{HealthProbe: &ToolHealthProbe{
				Type:             ProbeTypeHTTPGet,
				Interval:         "30s",
				Timeout:          "5s",
				FailureThreshold: 3,
				SuccessThreshold: 1,
			}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.spec.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestToolSpecValidateExecutionPolicy(t *testing.T) {
	tests := []struct {
		name   string
		policy *ToolExecutionPolicy
		valid  bool
	}{
		{name: "idempotent retry", policy: &ToolExecutionPolicy{Idempotent: true, RetryMaxAttempts: 2, Timeout: "2s", MaxConcurrent: 3, CircuitBreaker: &ToolCircuitBreaker{FailureThreshold: 2, Cooldown: "30s"}}, valid: true},
		{name: "retry non idempotent", policy: &ToolExecutionPolicy{RetryMaxAttempts: 2}, valid: false},
		{name: "invalid cooldown", policy: &ToolExecutionPolicy{CircuitBreaker: &ToolCircuitBreaker{FailureThreshold: 1, Cooldown: "invalid"}}, valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (ToolSpec{Protocol: ToolProtocolMCP, ExecutionPolicy: test.policy}).Validate()
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v, want valid=%t", err, test.valid)
			}
		})
	}
}

func TestToolSpecEffectiveValues(t *testing.T) {
	legacySchema := map[string]any{"source": "legacy"}
	currentSchema := map[string]any{"source": "current"}

	if got := (ToolSpec{}).EffectiveProtocol(); got != ToolProtocolMCP {
		t.Fatalf("EffectiveProtocol() = %q, want %q", got, ToolProtocolMCP)
	}
	if got := (ToolSpec{Protocol: ToolProtocolShell}).EffectiveProtocol(); got != ToolProtocolShell {
		t.Fatalf("explicit EffectiveProtocol() = %q, want %q", got, ToolProtocolShell)
	}
	if got := (ToolSpec{InputSchema: legacySchema}).EffectiveSchema(); got["source"] != "legacy" {
		t.Fatalf("legacy EffectiveSchema() = %+v", got)
	}
	if got := (ToolSpec{Schema: currentSchema, InputSchema: legacySchema}).EffectiveSchema(); got["source"] != "current" {
		t.Fatalf("current EffectiveSchema() = %+v", got)
	}
}

func TestToolGrantSpecValidate(t *testing.T) {
	valid := ToolGrantSpec{
		ToolRef: LocalObjectReference{Name: "search"},
		Verbs:   []ToolGrantVerb{ToolGrantVerbInvoke, ToolGrantVerbDiscover, ToolGrantVerbInspect},
	}
	tests := []struct {
		name    string
		spec    ToolGrantSpec
		wantErr string
	}{
		{name: "valid", spec: valid},
		{name: "tool required", spec: ToolGrantSpec{Verbs: []ToolGrantVerb{ToolGrantVerbInvoke}}, wantErr: "toolRef.name"},
		{name: "verbs required", spec: ToolGrantSpec{ToolRef: valid.ToolRef}, wantErr: "verbs must be non-empty"},
		{name: "invalid verb", spec: ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: []ToolGrantVerb{"admin"}}, wantErr: "verbs[0]"},
		{name: "duplicate verb", spec: ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: []ToolGrantVerb{ToolGrantVerbInvoke, ToolGrantVerbInvoke}}, wantErr: "duplicate verb"},
		{
			name: "agent reference and selector are mutually exclusive",
			spec: ToolGrantSpec{
				AgentRef:      LocalObjectReference{Name: "researcher"},
				AgentSelector: map[string]string{"team": "research"},
				ToolRef:       valid.ToolRef,
				Verbs:         valid.Verbs,
			},
			wantErr: "agentRef and agentSelector are mutually exclusive",
		},
		{
			name:    "negative per-minute limit",
			spec:    ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: valid.Verbs, RateLimits: &ToolGrantRateLimits{CallsPerMinute: -1}},
			wantErr: "callsPerMinute",
		},
		{
			name:    "negative hourly limit",
			spec:    ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: valid.Verbs, RateLimits: &ToolGrantRateLimits{CallsPerHour: -1}},
			wantErr: "callsPerHour",
		},
		{
			name:    "negative daily limit",
			spec:    ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: valid.Verbs, RateLimits: &ToolGrantRateLimits{CallsPerDay: -1}},
			wantErr: "callsPerDay",
		},
		{
			name: "valid rate limits",
			spec: ToolGrantSpec{
				ToolRef:    valid.ToolRef,
				Verbs:      valid.Verbs,
				RateLimits: &ToolGrantRateLimits{CallsPerMinute: 10, CallsPerHour: 100, CallsPerDay: 1000},
			},
		},
		{
			name:    "invalid valid-from timestamp",
			spec:    ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: valid.Verbs, Conditions: &ToolGrantConditions{ValidFrom: "tomorrow"}},
			wantErr: "conditions.validFrom",
		},
		{
			name:    "valid-until before valid-from",
			spec:    ToolGrantSpec{ToolRef: valid.ToolRef, Verbs: valid.Verbs, Conditions: &ToolGrantConditions{ValidFrom: "2026-02-01T00:00:00Z", ValidUntil: "2026-01-01T00:00:00Z"}},
			wantErr: "validUntil must be after validFrom",
		},
		{
			name: "invalid argument constraints schema",
			spec: ToolGrantSpec{
				ToolRef:             valid.ToolRef,
				Verbs:               valid.Verbs,
				ArgumentConstraints: map[string]any{"type": "not-a-json-schema-type"},
			},
			wantErr: "argumentConstraints",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.spec.Validate()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestToolGrantSpecHasVerb(t *testing.T) {
	spec := ToolGrantSpec{Verbs: []ToolGrantVerb{ToolGrantVerbInvoke, ToolGrantVerbInspect}}
	if !spec.HasVerb(ToolGrantVerbInspect) {
		t.Fatal("HasVerb(inspect) = false, want true")
	}
	if spec.HasVerb(ToolGrantVerbDiscover) {
		t.Fatal("HasVerb(discover) = true, want false")
	}
}
