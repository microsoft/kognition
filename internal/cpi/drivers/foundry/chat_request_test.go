// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

import (
	"encoding/json"
	"testing"
)

func TestPrepareChatRequest(t *testing.T) {
	for _, model := range []string{"gpt-5", "gpt-5.6-luna", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			payload, err := PrepareChatRequest([]byte(`{"max_tokens":128,"temperature":0.7,"reasoning_effort":"none","tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[{"role":"user","content":"hello"}]}`), model)
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]json.RawMessage
			if err := json.Unmarshal(payload, &request); err != nil {
				t.Fatal(err)
			}
			if string(request["max_completion_tokens"]) != "128" || request["max_tokens"] != nil || request["temperature"] != nil {
				t.Fatalf("unsupported reasoning parameters: %s", payload)
			}
			if string(request["reasoning_effort"]) != `"none"` || request["tools"] == nil || request["messages"] == nil {
				t.Fatalf("request fields were lost: %s", payload)
			}
		})
	}
	legacy := []byte(`{"max_tokens":128,"temperature":0.25}`)
	got, err := PrepareChatRequest(legacy, "gpt-4.1")
	if err != nil || string(got) != string(legacy) {
		t.Fatalf("legacy request changed: %s, %v", got, err)
	}
	modern := []byte(`{"max_completion_tokens":128}`)
	if prepared, err := PrepareChatRequest(modern, "gpt-6-luna"); err != nil {
		t.Fatalf("modern completion limit rejected: %v", err)
	} else {
		var request map[string]json.RawMessage
		if err := json.Unmarshal(prepared, &request); err != nil {
			t.Fatal(err)
		}
		if string(request["reasoning_effort"]) != `"none"` {
			t.Fatalf("GPT-6 reasoning default = %s, want none", request["reasoning_effort"])
		}
	}
	for _, invalid := range []string{`null`, `{`, `{"max_tokens":128,"max_completion_tokens":256}`} {
		if _, err := PrepareChatRequest([]byte(invalid), "gpt-6-luna"); err == nil {
			t.Fatalf("invalid reasoning request accepted: %s", invalid)
		}
	}
}
