// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const maxStdioInvocationLineBytes = 1 << 20

type stdioRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type stdioRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *stdioRPCError  `json:"error,omitempty"`
}

type stdioRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// InvokeMCPStdio performs one bounded MCP stdio session over a connected
// sandbox stream. The launch command is resolved from ToolClass; Tool input is
// sent only as JSON-RPC arguments and never becomes process argv.
func InvokeMCPStdio(ctx context.Context, stream io.ReadWriteCloser, tool *v1alpha1.Tool, input map[string]any) (map[string]any, error) {
	if stream == nil || tool == nil {
		return nil, fmt.Errorf("stdio stream and Tool are required")
	}
	defer func() { _ = stream.Close() }()
	encoder := json.NewEncoder(stream)
	if err := encoder.Encode(stdioRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "kognition", "version": "v1alpha1"},
	}}); err != nil {
		return nil, fmt.Errorf("write MCP initialize: %w", err)
	}
	reader := bufio.NewScanner(stream)
	reader.Buffer(make([]byte, 4096), maxStdioInvocationLineBytes)
	if _, err := readStdioRPCResponse(ctx, reader, 1); err != nil {
		return nil, fmt.Errorf("read MCP initialize: %w", err)
	}
	if err := encoder.Encode(stdioRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		return nil, fmt.Errorf("write MCP initialized notification: %w", err)
	}
	if err := encoder.Encode(stdioRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: map[string]any{"name": tool.ObjectMeta.Name, "arguments": input}}); err != nil {
		return nil, fmt.Errorf("write MCP tools/call: %w", err)
	}
	response, err := readStdioRPCResponse(ctx, reader, 2)
	if err != nil {
		return nil, fmt.Errorf("read MCP tools/call: %w", err)
	}
	var output map[string]any
	if err := json.Unmarshal(response.Result, &output); err != nil {
		return nil, fmt.Errorf("decode MCP tools/call result: %w", err)
	}
	if failed, _ := output["isError"].(bool); failed {
		return nil, fmt.Errorf("MCP tool returned isError=true")
	}
	return output, nil
}

func readStdioRPCResponse(ctx context.Context, reader *bufio.Scanner, expectedID int64) (stdioRPCResponse, error) {
	for {
		select {
		case <-ctx.Done():
			return stdioRPCResponse{}, ctx.Err()
		default:
		}
		if !reader.Scan() {
			if err := reader.Err(); err != nil {
				return stdioRPCResponse{}, err
			}
			return stdioRPCResponse{}, io.EOF
		}
		var response stdioRPCResponse
		if err := json.Unmarshal(reader.Bytes(), &response); err != nil {
			return stdioRPCResponse{}, fmt.Errorf("decode JSON-RPC response: %w", err)
		}
		if response.JSONRPC != "2.0" || response.ID != expectedID {
			continue
		}
		if response.Error != nil {
			return stdioRPCResponse{}, fmt.Errorf("MCP RPC error %d: %s", response.Error.Code, response.Error.Message)
		}
		if len(response.Result) == 0 {
			raw, _ := json.Marshal(response)
			return stdioRPCResponse{}, fmt.Errorf("MCP JSON-RPC response is missing result: %s", raw)
		}
		return response, nil
	}
}
