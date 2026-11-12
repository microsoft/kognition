// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TC-0305: Tool and Memory trust decisions share a stable, redacted versioned schema.
func TestTrustDecisionV1SchemaAcceptsRedactedDecision(t *testing.T) {
	schema := loadTrustDecisionSchema(t)
	decision := map[string]any{
		"schemaVersion": 1,
		"id":            "tool-allow-001",
		"timestamp":     time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"plane":         "tool",
		"outcome":       "allowed",
		"reasonCode":    "authorized",
		"workspace":     "demo-team",
		"runId":         "hack-demo-001",
		"identity": map[string]any{
			"agentIdentity":    "spiffe://kognition.dev/workspace/demo-team/agent/planner",
			"agentIdentityRef": map[string]any{"kind": "AgentIdentity", "namespace": "demo-team", "name": "agent-identity-planner", "resourceVersion": "42"},
		},
		"session": map[string]any{"sessionId": "session-001", "capabilityId": "capability-001"},
		"request": map[string]any{
			"operation":    "invoke",
			"invocationId": "tool-call-001",
			"targetRef":    map[string]any{"kind": "Tool", "namespace": "demo-team", "name": "weather", "resourceVersion": "9"},
		},
		"authorization": map[string]any{
			"identity": "allowed", "role": "allowed", "grant": "allowed", "policy": "allowed",
			"grantRef": map[string]any{"kind": "ToolGrant", "namespace": "demo-team", "name": "weather-grant"},
		},
		"approval":  map[string]any{"status": "not_required"},
		"execution": map[string]any{"status": "completed", "targetReached": true},
		"trace":     map[string]any{"traceId": "0123456789abcdef0123456789abcdef", "spanId": "0123456789abcdef"},
	}
	if err := schema.Validate(decision); err != nil {
		t.Fatalf("valid trust decision was rejected: %v", err)
	}
}

func TestTrustDecisionV1SchemaRejectsSensitiveAndInvalidFields(t *testing.T) {
	schema := loadTrustDecisionSchema(t)
	base := map[string]any{
		"schemaVersion": 1,
		"id":            "memory-deny-001",
		"timestamp":     "2026-08-27T12:00:00Z",
		"plane":         "memory",
		"outcome":       "denied",
		"reasonCode":    "grant_revoked",
		"workspace":     "demo-team",
		"request":       map[string]any{"operation": "store", "targetRef": map[string]any{"kind": "MemoryVolume", "name": "working-memory"}},
		"authorization": map[string]any{"identity": "allowed", "role": "allowed", "grant": "revoked", "policy": "not_evaluated"},
		"execution":     map[string]any{"status": "blocked", "targetReached": false},
	}
	for name, mutate := range map[string]func(map[string]any){
		"credential":     func(decision map[string]any) { decision["credential"] = "secret" },
		"raw prompt":     func(decision map[string]any) { decision["rawPrompt"] = "user content" },
		"model response": func(decision map[string]any) { decision["modelResponse"] = "provider response" },
		"unrestricted arguments": func(decision map[string]any) {
			decision["request"].(map[string]any)["arguments"] = map[string]any{"city": "Seattle"}
		},
		"unknown outcome": func(decision map[string]any) { decision["outcome"] = "maybe" },
		"unknown plane":   func(decision map[string]any) { decision["plane"] = "database" },
	} {
		t.Run(name, func(t *testing.T) {
			decision := cloneTrustDecision(t, base)
			mutate(decision)
			if err := schema.Validate(decision); err == nil {
				t.Fatal("invalid trust decision was accepted")
			}
		})
	}
}

func loadTrustDecisionSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schemaPath := filepath.Join("..", "..", "api", "schemas", "trust-decision-v1.schema.json")
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatalf("compile trust decision schema: %v", err)
	}
	return schema
}

func cloneTrustDecision(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}
