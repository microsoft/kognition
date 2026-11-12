// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadStatusRegistryRejectsInvalidOperatorBindings(test *testing.T) {
	source := testWorkloadSource()
	valid := WorkloadIdentityStatusRegistration{Namespace: "tenant", AgentName: "another", AgentUID: "70000000-0000-4000-8000-000000000002", Endpoint: "https://127.0.0.1:1"}
	for _, registrations := range [][]WorkloadIdentityStatusRegistration{
		nil, {valid, valid}, {{Namespace: "tenant", AgentName: "another", AgentUID: "", Endpoint: valid.Endpoint}},
		{{Namespace: "tenant", AgentName: "another", AgentUID: valid.AgentUID, Endpoint: "http://127.0.0.1:1"}},
	} {
		if _, err := NewWorkloadIdentityStatusRegistry(source, "kognition.dev", registrations); err == nil {
			test.Fatal("invalid registry accepted")
		}
	}
	registry, err := NewWorkloadIdentityStatusRegistry(source, "kognition.dev", []WorkloadIdentityStatusRegistration{valid})
	if err != nil {
		test.Fatal(err)
	}
	defer registry.Close()
	for _, target := range []WorkloadIdentityStatusRegistration{{Namespace: "another", AgentName: valid.AgentName, AgentUID: valid.AgentUID}, {Namespace: valid.Namespace, AgentName: valid.AgentName, AgentUID: "70000000-0000-4000-8000-000000000003"}} {
		if status, err := registry.AgentStatus(target.Namespace, target.AgentName, target.AgentUID); !errors.Is(err, ErrWorkloadIdentityUnavailable) || status != (WorkloadIdentityStatus{}) {
			test.Fatal("unregistered incarnation received status")
		}
	}
}

func TestWorkloadStatusRegistryLoaderDefaultsAndBounds(test *testing.T) {
	if registry, close, err := LoadWorkloadIdentityStatusRegistry(context.Background(), func(string) string { return "" }); err != nil || registry != nil || close == nil {
		test.Fatal("legacy controller default changed")
	}
	for _, body := range []string{`[]`, `[{}] {}`, `[{"namespace":"tenant","unknown":true}]`, strings.Repeat(" ", 65537),
		`[{"namespace":"tenant","namespace":"another","agentName":"worker","agentUID":"70000000-0000-4000-8000-000000000001","endpoint":"https://127.0.0.1:1"}]`,
		`[{"namespace":"tenant","agentName":"worker","agentUID":"70000000-0000-4000-8000-000000000001","endpoint":null}]`,
		`[{"namespace":"tenant","agentName":"worker","agentUID":"","endpoint":"https://127.0.0.1:1"}]`,
		`[{"namespace":"tenant","agentName":"worker","agentUID":"70000000-0000-4000-8000-000000000001","endpoint":"http://127.0.0.1:1"}]`,
	} {
		path := filepath.Join(test.TempDir(), "registry.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			test.Fatal(err)
		}
		values := map[string]string{"KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE": path, "KOGNITION_SPIFFE_ID": "spiffe://kognition.dev/controller/identity", "KOGNITION_SPIFFE_TRUST_DOMAIN": "kognition.dev"}
		calls := 0
		_, _, err := loadWorkloadIdentityStatusRegistry(context.Background(), func(key string) string { return values[key] }, func(context.Context, WorkloadIdentityConfig) (*WorkloadIdentity, error) {
			calls++
			return nil, ErrWorkloadIdentityUnavailable
		})
		if err == nil || calls != 0 {
			test.Fatal("malformed registry contacted the issuer")
		}
	}
}
