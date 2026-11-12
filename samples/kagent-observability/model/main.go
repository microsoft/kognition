// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"
)

type contract struct {
	SystemMessage string `json:"systemMessage"`
	PromQLPrompt  string `json:"promqlPrompt"`
	Tools         []struct {
		SDKSchema map[string]any `json:"sdkSchema"`
	} `json:"tools"`
}

type fixture struct {
	contract    contract
	parentCalls atomic.Int64
	childCalls  atomic.Int64
	requests    atomic.Int64
	rejected    atomic.Int64
}

type action struct{ id, name, arguments string }

var actions = []action{
	{"port_resources", "get_resources", `{"name":null,"resource_type":"pod","all_namespaces":false,"ns":"recovery-runtime","output":"json"}`},
	{"port_discovery", "get_available_api_resources", `{}`},
	{"port_generate", "GeneratePromQLTool", `{"query_description":"Return the Prometheus query up, without explanation."}`},
	{"port_query", "QueryTool", `{"query":"up","time":null,"timeout":null}`},
	{"port_create", "DashboardManagementTool", `{"action":"create","dashboard":{"uid":"porting-owned-acceptance","title":"T-0150 Acceptance","schemaVersion":41,"version":0,"panels":[]},"folder_uid":"porting-owned","overwrite":false}`},
	{"port_get", "DashboardManagementTool", `{"action":"get","uid":"porting-owned-acceptance"}`},
	{"port_delete", "DashboardManagementTool", `{"action":"delete","uid":"porting-owned-acceptance"}`},
}

func main() {
	data, err := os.ReadFile(os.Getenv("KOGNITION_PORTING_REFERENCE_CONTRACT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "reviewed reference contract unavailable")
		os.Exit(1)
	}
	var reviewed contract
	if json.Unmarshal(data, &reviewed) != nil || reviewed.SystemMessage == "" || reviewed.PromQLPrompt == "" || len(reviewed.Tools) != 5 {
		fmt.Fprintln(os.Stderr, "incomplete reference contract")
		os.Exit(1)
	}
	server := &http.Server{Addr: ":443", Handler: (&fixture{contract: reviewed}).handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (fixture *fixture) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/models", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]any{"object": "list", "data": []any{map[string]any{"id": "gpt-4o", "object": "model"}}})
	})
	mux.HandleFunc("GET /stats", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, map[string]int64{"requests": fixture.requests.Load(), "rejected": fixture.rejected.Load(), "parentCalls": fixture.parentCalls.Load(), "childCalls": fixture.childCalls.Load()})
	})
	mux.HandleFunc("POST /v1/chat/completions", fixture.complete)
	return mux
}

func (fixture *fixture) complete(writer http.ResponseWriter, request *http.Request) {
	fixture.requests.Add(1)
	accepted := false
	defer func() {
		if !accepted {
			fixture.rejected.Add(1)
		}
	}()
	var input struct {
		Messages []map[string]any `json:"messages"`
		Tools    []map[string]any `json:"tools"`
	}
	if json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&input) != nil || len(input.Messages) == 0 {
		http.Error(writer, "invalid completion request", http.StatusBadRequest)
		return
	}
	system, _ := input.Messages[0]["content"].(string)
	if input.Messages[0]["role"] != "system" {
		http.Error(writer, "missing exact system prompt", http.StatusUnprocessableEntity)
		return
	}
	if system == fixture.contract.PromQLPrompt && len(input.Tools) == 0 {
		fixture.childCalls.Add(1)
		accepted = true
		completion(writer, "up", nil)
		return
	}
	if system != fixture.contract.SystemMessage || len(input.Tools) != len(fixture.contract.Tools) {
		http.Error(writer, "parent prompt or tool count changed", http.StatusUnprocessableEntity)
		return
	}
	for index, expected := range fixture.contract.Tools {
		if input.Tools[index]["type"] != "function" || !reflect.DeepEqual(input.Tools[index]["function"], expected.SDKSchema) {
			http.Error(writer, "reviewed SDK tool contract changed", http.StatusUnprocessableEntity)
			return
		}
	}
	results := map[string]string{}
	for _, message := range input.Messages {
		if message["role"] != "tool" {
			continue
		}
		id, _ := message["tool_call_id"].(string)
		content, _ := message["content"].(string)
		if _, duplicate := results[id]; duplicate {
			http.Error(writer, "duplicate tool result", http.StatusUnprocessableEntity)
			return
		}
		if err := validateResult(id, content); err != nil {
			http.Error(writer, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		results[id] = content
	}
	fixture.parentCalls.Add(1)
	accepted = true
	for _, step := range actions {
		if _, done := results[step.id]; done {
			continue
		}
		completion(writer, nil, []any{map[string]any{"id": step.id, "type": "function", "function": map[string]any{"name": step.name, "arguments": step.arguments}}})
		return
	}
	completion(writer, "KAGENT_REAL_OBSERVABILITY_OK", nil)
}

func validateResult(id, content string) error {
	failed := errors.New("real governed tool result did not satisfy acceptance")
	if id == "port_generate" {
		if strings.TrimSpace(content) == "up" {
			return nil
		}
		return failed
	}
	if id == "port_discovery" {
		if strings.Contains(content, "pods") && strings.Contains(content, "deployments") {
			return nil
		}
		return failed
	}
	var result map[string]any
	if json.Unmarshal([]byte(content), &result) != nil {
		return failed
	}
	switch id {
	case "port_resources":
		items, ok := result["items"].([]any)
		if (result["kind"] == "PodList" || result["kind"] == "List") && ok && len(items) > 0 {
			for _, item := range items {
				pod, _ := item.(map[string]any)
				metadata, _ := pod["metadata"].(map[string]any)
				if pod["kind"] != "Pod" || metadata["namespace"] != "recovery-runtime" {
					return failed
				}
			}
			return nil
		}
	case "port_query":
		data, _ := result["data"].(map[string]any)
		items, _ := data["result"].([]any)
		if result["status"] == "success" && len(items) > 0 {
			return nil
		}
	case "port_create":
		if result["uid"] == "porting-owned-acceptance" && result["status"] == "success" {
			return nil
		}
	case "port_get":
		dashboard, _ := result["dashboard"].(map[string]any)
		meta, _ := result["meta"].(map[string]any)
		if dashboard["uid"] == "porting-owned-acceptance" && meta["folderUid"] == "porting-owned" {
			return nil
		}
	case "port_delete":
		if result["title"] == "T-0150 Acceptance" {
			return nil
		}
	}
	return failed
}

func completion(writer http.ResponseWriter, content any, calls []any) {
	message := map[string]any{"role": "assistant", "content": content}
	reason := "stop"
	if calls != nil {
		message["tool_calls"] = calls
		reason = "tool_calls"
	}
	writeJSON(writer, map[string]any{"id": "kagent-porting-fixture", "object": "chat.completion", "created": 1, "model": "gpt-4o", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": reason}}, "usage": map[string]int{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}})
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
