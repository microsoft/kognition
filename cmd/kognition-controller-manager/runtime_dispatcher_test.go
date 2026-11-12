// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestStandaloneRuntimeDispatcherPreservesCPIDefault(t *testing.T) {
	compatibility := controller.NewCPIDispatcher(nil, nil, zap.NewNop())
	for _, mode := range []string{"", "cpi"} {
		t.Setenv("KOGNITION_RUNTIME_DISPATCH_MODE", mode)
		t.Setenv("KOGNITION_RUNTIME_ENDPOINTS", "invalid")
		actual, err := standaloneRuntimeDispatcher(nil, zap.NewNop(), compatibility)
		if err != nil || actual != compatibility {
			t.Fatal("CPI default changed")
		}
	}
	t.Setenv("KOGNITION_RUNTIME_DISPATCH_MODE", "typo")
	if _, err := standaloneRuntimeDispatcher(nil, zap.NewNop(), compatibility); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func configureHTTPRuntimeTest(t *testing.T) {
	t.Helper()
	t.Setenv("KOGNITION_RUNTIME_DISPATCH_MODE", "http")
	t.Setenv("KOGNITION_RUNTIME_ENDPOINTS", `{"tenant/agent":"http://127.0.0.1:8080/invoke"}`)
	t.Setenv("KOGNITION_RUNTIME_AUTH_TOKEN", "test-runtime-token")
	for _, variable := range []string{"MEMORY_POLICY", "TOOL", "MODEL", "AGENT_COMMUNICATION"} {
		t.Setenv("KOGNITION_"+variable+"_GATEWAY_URL", "http://gateway.test:8091")
	}
	for _, plane := range []string{"MEMORY", "TOOL", "MODEL", "AGENT_DELEGATION"} {
		prefix := "KOGNITION_" + plane + "_CAPABILITY_HMAC_"
		t.Setenv(prefix+"KEY_ID", "test-v1")
		t.Setenv(prefix+"KEY", "0123456789abcdef0123456789abcdef")
		t.Setenv(prefix+"PREVIOUS_KEY_ID", "")
		t.Setenv(prefix+"PREVIOUS_KEY", "")
	}
}

func TestStandaloneRuntimeDispatcherHTTPConfiguration(t *testing.T) {
	configureHTTPRuntimeTest(t)
	storage := &struct{ store.Store }{}
	actual, err := standaloneRuntimeDispatcher(storage, zap.NewNop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := actual.(*dispatcher.K8sDispatcher); !ok {
		t.Fatalf("unexpected dispatcher %T", actual)
	}
	for _, variable := range []string{"KOGNITION_RUNTIME_ENDPOINTS", "KOGNITION_RUNTIME_AUTH_TOKEN", "KOGNITION_MODEL_GATEWAY_URL", "KOGNITION_TOOL_GATEWAY_URL", "KOGNITION_MEMORY_POLICY_GATEWAY_URL", "KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL", "KOGNITION_MODEL_CAPABILITY_HMAC_KEY", "KOGNITION_TOOL_CAPABILITY_HMAC_KEY", "KOGNITION_MEMORY_CAPABILITY_HMAC_KEY", "KOGNITION_AGENT_DELEGATION_CAPABILITY_HMAC_KEY"} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv(variable, "")
			if _, err := standaloneRuntimeDispatcher(storage, zap.NewNop(), nil); err == nil {
				t.Fatal("incomplete HTTP configuration accepted")
			}
		})
	}
}
