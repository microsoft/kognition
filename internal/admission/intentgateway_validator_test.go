// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestIntentGatewayValidator(t *testing.T) {
	validator := NewIntentGatewayValidator()
	for _, test := range []struct {
		name    string
		spec    v1alpha1.IntentGatewaySpec
		allowed bool
		message string
	}{
		{name: "default HTTP", allowed: true},
		{name: "explicit default values", spec: v1alpha1.IntentGatewaySpec{Protocol: "", AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: ""}}, allowed: true},
		{name: "HTTP API key", spec: v1alpha1.IntentGatewaySpec{Protocol: "http", AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "gateway-key"}}}, allowed: true},
		{name: "unsupported protocol", spec: v1alpha1.IntentGatewaySpec{Protocol: "grpc"}, message: "unsupported"},
		{name: "blank route pattern", spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{Pattern: " \t", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}}}, message: "pattern is required"},
		{name: "invalid glob route pattern", spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{Pattern: "[", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}}}, message: "pattern is invalid"},
		{name: "missing API key reference", spec: v1alpha1.IntentGatewaySpec{AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey"}}, message: "apiKeySecretRef.name"},
		{name: "invalid mode", spec: v1alpha1.IntentGatewaySpec{AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "oauth"}}, message: "invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(v1alpha1.IntentGateway{Spec: test.spec})
			if err != nil {
				t.Fatal(err)
			}
			response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "IntentGateway", Object: data})
			if response.Allowed != test.allowed || (test.message != "" && !strings.Contains(response.Reason, test.message)) {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}
