// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mcp

import (
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// TC-0170: OpenAPI spec → protocol compilation → valid MCP tool definition.
func TestCompile_OpenAPI_ProducesValidMCPDefinition(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "petstore-list", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolOpenAPI,
			ClassName: "openapi-adapter",
			Endpoint:  "https://petstore.example.com/v1",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{"type": "integer"},
				},
			},
		},
	}

	def, err := Compile(tool)
	if err != nil {
		t.Fatalf("Compile() error: %v", err)
	}
	if def.Name != "petstore-list" {
		t.Errorf("Name = %q, want %q", def.Name, "petstore-list")
	}
	if def.Transport != v1alpha1.TransportHTTP {
		t.Errorf("Transport = %q, want %q", def.Transport, v1alpha1.TransportHTTP)
	}
	if def.Endpoint != "https://petstore.example.com/v1" {
		t.Errorf("Endpoint = %q, want %q", def.Endpoint, "https://petstore.example.com/v1")
	}
	if def.InputSchema == nil {
		t.Fatal("InputSchema is nil, expected OpenAPI schema")
	}
	props, ok := def.InputSchema["properties"]
	if !ok {
		t.Fatal("InputSchema missing properties")
	}
	if _, hasLimit := props.(map[string]any)["limit"]; !hasLimit {
		t.Fatal("InputSchema.properties missing 'limit'")
	}
}

func TestCompile_MCP_WithEndpoint(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "code-exec", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolMCP,
			ClassName: "process-networked",
			Endpoint:  "http://localhost:8080/mcp",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string"},
				},
			},
		},
	}

	def, err := Compile(tool)
	if err != nil {
		t.Fatalf("Compile() error: %v", err)
	}
	if def.Transport != v1alpha1.TransportHTTP {
		t.Errorf("Transport = %q, want %q", def.Transport, v1alpha1.TransportHTTP)
	}
}

func TestCompile_MCP_WithoutEndpoint(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "local-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolMCP,
			ClassName: "process-local",
		},
	}

	def, err := Compile(tool)
	if err != nil {
		t.Fatalf("Compile() error: %v", err)
	}
	if def.Transport != v1alpha1.TransportStdio {
		t.Errorf("Transport = %q, want %q", def.Transport, v1alpha1.TransportStdio)
	}
}

func TestCompile_Browser_DefaultSchema(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "browser-tool", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolBrowser,
			ClassName: "browser-auto",
			Endpoint:  "http://localhost:9222",
		},
	}

	def, err := Compile(tool)
	if err != nil {
		t.Fatalf("Compile() error: %v", err)
	}
	if def.Transport != v1alpha1.TransportHTTP {
		t.Errorf("Transport = %q, want %q", def.Transport, v1alpha1.TransportHTTP)
	}
	if def.InputSchema == nil {
		t.Fatal("expected default browser schema, got nil")
	}
	required, ok := def.InputSchema["required"].([]any)
	if !ok || len(required) == 0 {
		t.Fatal("expected required fields in default browser schema")
	}
}

func TestCompile_Shell_DefaultSchema(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "shell-exec", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolShell,
			ClassName: "exec-local",
		},
	}

	def, err := Compile(tool)
	if err != nil {
		t.Fatalf("Compile() error: %v", err)
	}
	if def.Transport != v1alpha1.TransportStdio {
		t.Errorf("Transport = %q, want %q", def.Transport, v1alpha1.TransportStdio)
	}
	if def.InputSchema == nil {
		t.Fatal("expected default shell schema, got nil")
	}
	props, ok := def.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in default shell schema")
	}
	if _, hasCmd := props["command"]; !hasCmd {
		t.Fatal("expected 'command' in shell schema properties")
	}
}

func TestCompile_OpenAPI_MissingSchema_ReturnsError(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "no-schema", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:  v1alpha1.ToolProtocolOpenAPI,
			ClassName: "openapi-adapter",
			Endpoint:  "https://api.example.com",
		},
	}

	_, err := Compile(tool)
	if err == nil {
		t.Fatal("expected error for OpenAPI tool without schema")
	}
}

func TestCompile_UnsupportedProtocol_ReturnsError(t *testing.T) {
	tool := &v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "unknown", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol: "grpc",
		},
	}

	_, err := Compile(tool)
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}
