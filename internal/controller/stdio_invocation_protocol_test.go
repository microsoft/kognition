// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestInvokeMCPStdio_InitializesAndCallsPinnedTool(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	go func() {
		reader := bufio.NewScanner(server)
		encoder := json.NewEncoder(server)
		var initialize stdioRPCRequest
		if !reader.Scan() || json.Unmarshal(reader.Bytes(), &initialize) != nil || initialize.Method != "initialize" || initialize.ID != 1 {
			return
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"protocolVersion": "2025-03-26"}})
		if !reader.Scan() {
			return
		}
		var notification stdioRPCRequest
		if json.Unmarshal(reader.Bytes(), &notification) != nil || notification.Method != "notifications/initialized" {
			return
		}
		if !reader.Scan() {
			return
		}
		var call stdioRPCRequest
		if json.Unmarshal(reader.Bytes(), &call) != nil || call.Method != "tools/call" || call.ID != 2 {
			return
		}
		params, _ := call.Params.(map[string]any)
		if params["name"] != "shell" {
			return
		}
		arguments, _ := params["arguments"].(map[string]any)
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"content": []any{}, "command": arguments["command"]}})
	}()
	output, err := InvokeMCPStdio(context.Background(), client, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell"}}, map[string]any{"command": "status"})
	if err != nil || output["command"] != "status" {
		t.Fatalf("InvokeMCPStdio output=%#v err=%v", output, err)
	}
}

func TestInvokeMCPStdio_RejectsToolFailureAndMalformedOutput(t *testing.T) {
	for _, test := range []struct{ name, response, want string }{
		{name: "tool error", response: `{"jsonrpc":"2.0","id":1,"result":{}}\n{"jsonrpc":"2.0","id":2,"result":{"isError":true}}`, want: "isError=true"},
		{name: "malformed", response: `not-json`, want: "decode JSON-RPC response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			go func() {
				reader := bufio.NewScanner(server)
				if !reader.Scan() {
					return
				}
				_, _ = server.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
				if !reader.Scan() {
					return
				}
				if !reader.Scan() {
					return
				}
				response := strings.ReplaceAll(test.response, `\n`, "\n")
				if test.name == "tool error" {
					response = strings.Split(response, "\n")[1]
				}
				_, _ = server.Write([]byte(response + "\n"))
			}()
			_, err := InvokeMCPStdio(context.Background(), client, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell"}}, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("InvokeMCPStdio error=%v, want %q", err, test.want)
			}
		})
	}
}
