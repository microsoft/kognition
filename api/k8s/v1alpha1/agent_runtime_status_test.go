// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestAgentRuntimeStatusPreservesZeroCountsAndDeepCopies(t *testing.T) {
	agent := &Agent{Status: AgentStatus{Runtime: &AgentRuntimeStatus{Ready: "0/2", DesiredReplicas: 2}}}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Status struct {
			Runtime map[string]any `json:"runtime"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"replicas", "readyReplicas", "updatedReplicas", "availableReplicas"} {
		if value, present := output.Status.Runtime[field]; !present || value != float64(0) {
			t.Fatalf("zero %s was omitted or changed: %s", field, data)
		}
	}
	copied := agent.DeepCopy()
	copied.Status.Runtime.Ready, copied.Status.Runtime.ReadyReplicas = "2/2", 2
	if agent.Status.Runtime.Ready != "0/2" || agent.Status.Runtime.ReadyReplicas != 0 {
		t.Fatal("Agent.DeepCopy shares runtime status")
	}
}
