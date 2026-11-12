// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPrompt_JSONRoundtrip_ExpandedSchema(t *testing.T) {
	prompt := Prompt{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: "Prompt"},
		ObjectMeta: ObjectMeta{
			Name:      "assistant-core",
			Namespace: "default",
		},
		Spec: PromptSpec{
			Content: "You are a helpful assistant for {{domain}}.",
			Parameters: []PromptParameter{
				{Name: "domain", Description: "Problem area", Required: true},
				{Name: "tone", Default: "concise"},
			},
			Metadata: map[string]string{"owner": "platform", "tier": "gold"},
		},
		Status: PromptStatus{Hash: "sha256:abc123", Version: 3},
	}

	data, err := json.Marshal(prompt)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}

	var got Prompt
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}

	if got.Spec.Content != prompt.Spec.Content {
		t.Fatalf("content = %q, want %q", got.Spec.Content, prompt.Spec.Content)
	}
	if len(got.Spec.Parameters) != 2 {
		t.Fatalf("parameters len = %d, want 2", len(got.Spec.Parameters))
	}
	if got.Spec.Parameters[0].Name != "domain" || !got.Spec.Parameters[0].Required {
		t.Fatalf("first parameter = %+v, want required domain", got.Spec.Parameters[0])
	}
	if got.Spec.Metadata["owner"] != "platform" {
		t.Fatalf("metadata.owner = %q, want %q", got.Spec.Metadata["owner"], "platform")
	}
	if got.Status.Hash != "sha256:abc123" || got.Status.Version != 3 {
		t.Fatalf("status = %+v, want hash/version set", got.Status)
	}
}

func TestPromptSpec_JSONRoundtrip_LegacyInstructionsAlias(t *testing.T) {
	spec := PromptSpec{Instructions: "Legacy prompt text"}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal prompt spec: %v", err)
	}

	var got PromptSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal prompt spec: %v", err)
	}

	if got.Instructions != "Legacy prompt text" {
		t.Fatalf("instructions = %q, want %q", got.Instructions, "Legacy prompt text")
	}
	if got.Content != "" {
		t.Fatalf("content = %q, want empty for legacy-only payload", got.Content)
	}
	if got.ResolvedContent() != "Legacy prompt text" {
		t.Fatalf("resolved content = %q, want %q", got.ResolvedContent(), "Legacy prompt text")
	}
}

func TestPromptSpec_ResolvedContent_PrefersCanonicalContent(t *testing.T) {
	spec := PromptSpec{
		Content:      "Canonical content",
		Instructions: "Legacy instructions",
	}

	if got := spec.ResolvedContent(); got != "Canonical content" {
		t.Fatalf("resolved content = %q, want %q", got, "Canonical content")
	}
}

func TestPromptSpec_JSONRoundtrip_BothFieldsDeterministicPrecedence(t *testing.T) {
	spec := PromptSpec{
		Content:      "Canonical content",
		Instructions: "Legacy instructions",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal prompt spec: %v", err)
	}

	var got PromptSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal prompt spec: %v", err)
	}

	if got.Content != "Canonical content" {
		t.Fatalf("content = %q, want %q", got.Content, "Canonical content")
	}
	if got.Instructions != "Legacy instructions" {
		t.Fatalf("instructions = %q, want %q", got.Instructions, "Legacy instructions")
	}
	if got.ResolvedContent() != "Canonical content" {
		t.Fatalf("resolved content = %q, want %q", got.ResolvedContent(), "Canonical content")
	}
}

func TestSecretRef_JSONRoundtrip_ExpandedSchema(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	secret := SecretRef{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: "SecretRef"},
		ObjectMeta: ObjectMeta{
			Name:      "openai-key",
			Namespace: "default",
		},
		Spec: SecretRefSpec{
			BackendType: "internal",
			BackendRef:  "secret/openai",
			Key:         "api-key",
		},
		Status: SecretRefStatus{Synced: true, LastSyncTime: &now},
	}

	data, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("marshal secretRef: %v", err)
	}

	var got SecretRef
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal secretRef: %v", err)
	}

	if got.Spec.BackendType != "internal" || got.Spec.BackendRef != "secret/openai" || got.Spec.Key != "api-key" {
		t.Fatalf("secretRef spec = %+v, want backendType/backendRef/key preserved", got.Spec)
	}
	if !got.Status.Synced || got.Status.LastSyncTime == nil || !got.Status.LastSyncTime.Equal(now) {
		t.Fatalf("secretRef status = %+v, want synced with lastSyncTime", got.Status)
	}
	if got.Spec.ResolvedBackendType() != "internal" || got.Spec.ResolvedBackendRef() != "secret/openai" || got.Spec.ResolvedKey() != "api-key" {
		t.Fatalf("resolved fields = type:%q ref:%q key:%q, want canonical values", got.Spec.ResolvedBackendType(), got.Spec.ResolvedBackendRef(), got.Spec.ResolvedKey())
	}
}

func TestSecretRefSpec_JSONRoundtrip_LegacyAliases(t *testing.T) {
	spec := SecretRefSpec{
		Provider:  "vault",
		Path:      "kv/platform/openai",
		LegacyKey: "api-key",
	}

	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal secretRef spec: %v", err)
	}

	var got SecretRefSpec
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal secretRef spec: %v", err)
	}

	if got.Provider != "vault" || got.Path != "kv/platform/openai" || got.LegacyKey != "api-key" {
		t.Fatalf("legacy fields = %+v, want preserved", got)
	}
	if got.ResolvedBackendType() != "vault" || got.ResolvedBackendRef() != "kv/platform/openai" || got.ResolvedKey() != "api-key" {
		t.Fatalf("resolved legacy fields = type:%q ref:%q key:%q, want fallback values", got.ResolvedBackendType(), got.ResolvedBackendRef(), got.ResolvedKey())
	}
}

func TestSecretRefSpec_ResolvedFields_PreferCanonicalWhenBothSet(t *testing.T) {
	spec := SecretRefSpec{
		BackendType: "internal",
		BackendRef:  "secret/canonical",
		Key:         "canonical-key",
		Provider:    "vault",
		Path:        "kv/legacy/path",
		LegacyKey:   "legacy-key",
	}

	if got := spec.ResolvedBackendType(); got != "internal" {
		t.Fatalf("resolved backend type = %q, want %q", got, "internal")
	}
	if got := spec.ResolvedBackendRef(); got != "secret/canonical" {
		t.Fatalf("resolved backend ref = %q, want %q", got, "secret/canonical")
	}
	if got := spec.ResolvedKey(); got != "canonical-key" {
		t.Fatalf("resolved key = %q, want %q", got, "canonical-key")
	}
}

func TestSecretRefSpec_Validate_RequiresResolvedFields(t *testing.T) {
	tests := []struct {
		name    string
		spec    SecretRefSpec
		wantErr bool
	}{
		{
			name: "valid canonical fields",
			spec: SecretRefSpec{BackendType: "internal", BackendRef: "secret/app", Key: "token"},
		},
		{
			name: "valid legacy fallback fields",
			spec: SecretRefSpec{Provider: "env", Path: "OPENAI_API_KEY", LegacyKey: "token"},
		},
		{
			name:    "missing backend type",
			spec:    SecretRefSpec{BackendRef: "secret/app", Key: "token"},
			wantErr: true,
		},
		{
			name:    "missing backend ref",
			spec:    SecretRefSpec{BackendType: "internal", Key: "token"},
			wantErr: true,
		},
		{
			name:    "missing key",
			spec:    SecretRefSpec{BackendType: "internal", BackendRef: "secret/app"},
			wantErr: true,
		},
		{
			name:    "unsupported backend type",
			spec:    SecretRefSpec{BackendType: "vault", BackendRef: "kv/app", Key: "token"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no validation error, got %v", err)
			}
		})
	}
}

func TestTool_JSONRoundtrip_WithSecretRefs(t *testing.T) {
	tool := Tool{
		TypeMeta: TypeMeta{APIVersion: APIVersion, Kind: "Tool"},
		ObjectMeta: ObjectMeta{
			Name:      "web-search",
			Namespace: "default",
		},
		Spec: ToolSpec{
			ClassName:   "process-networked",
			Endpoint:    "https://tools.internal/web-search",
			SecretRefs:  []ToolSecretRef{{Name: "web-search-api-key", Key: "API_KEY"}},
			InputSchema: map[string]any{"type": "object"},
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}

	var got Tool
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal tool: %v", err)
	}

	if len(got.Spec.SecretRefs) != 1 {
		t.Fatalf("secretRefs len = %d, want 1", len(got.Spec.SecretRefs))
	}
	if got.Spec.SecretRefs[0].Name != "web-search-api-key" || got.Spec.SecretRefs[0].Key != "API_KEY" {
		t.Fatalf("secretRefs[0] = %+v, want name/key preserved", got.Spec.SecretRefs[0])
	}
}

func TestToolSpec_Validate_SecretRefs(t *testing.T) {
	tests := []struct {
		name    string
		spec    ToolSpec
		wantErr string
	}{
		{
			name: "valid secret ref",
			spec: ToolSpec{SecretRefs: []ToolSecretRef{{Name: "web-search-api-key", Key: "API_KEY"}}},
		},
		{
			name:    "missing secretRef name",
			spec:    ToolSpec{SecretRefs: []ToolSecretRef{{Key: "API_KEY"}}},
			wantErr: "tool.spec.secretRefs[0].name is required",
		},
		{
			name:    "missing secretRef key",
			spec:    ToolSpec{SecretRefs: []ToolSecretRef{{Name: "web-search-api-key"}}},
			wantErr: "tool.spec.secretRefs[0].key is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("expected no validation error, got %v", err)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected validation error %q, got nil", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("want error %q, got %q", tt.wantErr, err.Error())
				}
			}
		})
	}
}
