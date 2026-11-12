// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mcp

import (
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// Compile produces an MCPToolDefinition from a Tool's declared protocol and spec.
// This implements the protocol compilation flow where all tool protocols are
// normalized to MCP at registration time.
func Compile(tool *v1alpha1.Tool) (*v1alpha1.MCPToolDefinition, error) {
	protocol := tool.Spec.EffectiveProtocol()
	switch protocol {
	case v1alpha1.ToolProtocolMCP:
		return compileMCP(tool)
	case v1alpha1.ToolProtocolOpenAPI:
		return compileOpenAPI(tool)
	case v1alpha1.ToolProtocolBrowser:
		return compileBrowser(tool)
	case v1alpha1.ToolProtocolShell:
		return compileShell(tool)
	default:
		return nil, fmt.Errorf("unsupported protocol %q for MCP compilation", protocol)
	}
}

// compileMCP validates and stores an MCP tool definition as-is.
func compileMCP(tool *v1alpha1.Tool) (*v1alpha1.MCPToolDefinition, error) {
	transport := resolveTransport(tool)
	return &v1alpha1.MCPToolDefinition{
		Name:        tool.ObjectMeta.Name,
		Description: tool.Spec.Description,
		InputSchema: tool.Spec.EffectiveSchema(),
		Transport:   transport,
		Endpoint:    tool.Spec.Endpoint,
	}, nil
}

// compileOpenAPI fetches/parses the OpenAPI schema and compiles operations to MCP tool definitions.
func compileOpenAPI(tool *v1alpha1.Tool) (*v1alpha1.MCPToolDefinition, error) {
	schema := tool.Spec.EffectiveSchema()
	if len(schema) == 0 {
		return nil, fmt.Errorf("OpenAPI tools require spec.schema for MCP compilation")
	}
	// Adapt the OpenAPI operation to an MCP tool definition.
	// The inputSchema from the OpenAPI spec becomes the MCP input schema.
	return &v1alpha1.MCPToolDefinition{
		Name:        tool.ObjectMeta.Name,
		Description: tool.Spec.Description,
		InputSchema: schema,
		Transport:   v1alpha1.TransportHTTP,
		Endpoint:    tool.Spec.Endpoint,
	}, nil
}

// compileBrowser wraps a browser-automation tool with a predefined schema.
func compileBrowser(tool *v1alpha1.Tool) (*v1alpha1.MCPToolDefinition, error) {
	inputSchema := tool.Spec.EffectiveSchema()
	if len(inputSchema) == 0 {
		inputSchema = browserDefaultSchema()
	}
	return &v1alpha1.MCPToolDefinition{
		Name:        tool.ObjectMeta.Name,
		Description: tool.Spec.Description,
		InputSchema: inputSchema,
		Transport:   v1alpha1.TransportHTTP,
		Endpoint:    tool.Spec.Endpoint,
	}, nil
}

// compileShell wraps a shell/exec tool with a predefined schema.
func compileShell(tool *v1alpha1.Tool) (*v1alpha1.MCPToolDefinition, error) {
	inputSchema := tool.Spec.EffectiveSchema()
	if len(inputSchema) == 0 {
		inputSchema = shellDefaultSchema()
	}
	return &v1alpha1.MCPToolDefinition{
		Name:        tool.ObjectMeta.Name,
		Description: tool.Spec.Description,
		InputSchema: inputSchema,
		Transport:   v1alpha1.TransportStdio,
		Endpoint:    tool.Spec.Endpoint,
	}, nil
}

// resolveTransport determines the appropriate MCP transport for a tool.
func resolveTransport(tool *v1alpha1.Tool) v1alpha1.Transport {
	// If the tool has an endpoint URL, default to HTTP streamable.
	if tool.Spec.Endpoint != "" {
		return v1alpha1.TransportHTTP
	}
	return v1alpha1.TransportStdio
}

// browserDefaultSchema returns the predefined input schema for browser tools.
func browserDefaultSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "Target URL for browser automation",
			},
			"action": map[string]any{
				"type":        "string",
				"description": "Browser action to perform",
				"enum":        []any{"navigate", "click", "type", "screenshot"},
			},
			"selector": map[string]any{
				"type":        "string",
				"description": "CSS selector for target element",
			},
		},
		"required": []any{"url", "action"},
	}
}

// shellDefaultSchema returns the predefined input schema for shell tools.
func shellDefaultSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to execute",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Working directory for command execution",
			},
			"timeout": map[string]any{
				"type":        "string",
				"description": "Execution timeout (Go duration, e.g. 30s)",
			},
		},
		"required": []any{"command"},
	}
}
