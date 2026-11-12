// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPortingFixtureRejectsFailedOrSyntheticResults(t *testing.T) {
	for _, input := range []struct{ id, content string }{
		{"port_generate", ""}, {"port_generate", "Unavailable"}, {"port_discovery", "{}"},
		{"port_resources", `{"kind":"PodList","items":[]}`},
		{"port_query", `{"status":"success","data":{"result":[]}}`},
		{"port_create", `{"uid":"foreign","status":"success"}`},
		{"port_get", `{"dashboard":{"uid":"porting-owned-acceptance"},"meta":{"folderUid":"foreign"}}`},
		{"port_delete", `{"title":"foreign"}`},
	} {
		t.Run(input.id, func(t *testing.T) {
			if validateResult(input.id, input.content) == nil {
				t.Fatal("failed result accepted")
			}
		})
	}
}

func TestPortingFixtureChildAndChangedParentContracts(t *testing.T) {
	server := &fixture{contract: contract{SystemMessage: "exact parent", PromQLPrompt: "exact child"}}
	child := httptest.NewRecorder()
	server.handler().ServeHTTP(child, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"system","content":"exact child"}]}`)))
	if child.Code != http.StatusOK || server.childCalls.Load() != 1 || !strings.Contains(child.Body.String(), `"content":"up"`) {
		t.Fatalf("child completion: %d %s", child.Code, child.Body.String())
	}
	changed := httptest.NewRecorder()
	server.handler().ServeHTTP(changed, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"system","content":"changed parent"}]}`)))
	if changed.Code != http.StatusUnprocessableEntity || server.parentCalls.Load() != 0 {
		t.Fatal("changed source prompt reached acceptance")
	}
	if server.requests.Load() != 2 || server.rejected.Load() != 1 {
		t.Fatal("rejected provider contact was not independently accounted")
	}
}

func TestPortingFixtureAcceptsPinnedKubectlListAndRejectsForeignItems(t *testing.T) {
	for _, scenario := range []struct {
		kind, namespace, itemKind string
		accepted                  bool
	}{
		{"List", "recovery-runtime", "Pod", true},
		{"PodList", "recovery-runtime", "Pod", true},
		{"List", "other", "Pod", false},
		{"List", "recovery-runtime", "Secret", false},
	} {
		t.Run(scenario.kind+scenario.namespace+scenario.itemKind, func(t *testing.T) {
			server := &fixture{contract: contract{SystemMessage: "exact parent"}}
			result, _ := json.Marshal(map[string]any{"kind": scenario.kind, "items": []any{map[string]any{"kind": scenario.itemKind, "metadata": map[string]any{"namespace": scenario.namespace}}}})
			input, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "system", "content": "exact parent"}, map[string]any{"role": "tool", "tool_call_id": "port_resources", "content": string(result)}}})
			response := httptest.NewRecorder()
			server.handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(input))))
			if scenario.accepted {
				if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "get_available_api_resources") {
					t.Fatal("valid pinned kubectl list was rejected")
				}
			} else if response.Code != http.StatusUnprocessableEntity || server.rejected.Load() != 1 {
				t.Fatal("foreign Kubernetes items reached the next Tool call")
			}
		})
	}
}
