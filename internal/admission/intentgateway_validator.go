// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// IntentGatewayValidator validates the declarative HTTP and authentication
// contract shared by standalone and Kubernetes modes.
type IntentGatewayValidator struct{}

func NewIntentGatewayValidator() *IntentGatewayValidator { return &IntentGatewayValidator{} }

func (v *IntentGatewayValidator) Name() string { return "intentgateway-validator" }

func (v *IntentGatewayValidator) Handles(kind string, op Operation) bool {
	return kind == "IntentGateway" && (op == OperationCreate || op == OperationUpdate)
}

func (v *IntentGatewayValidator) Validate(_ context.Context, req *Request) *Response {
	var gateway v1alpha1.IntentGateway
	if err := json.Unmarshal(req.Object, &gateway); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid intentGateway JSON: %v", err)}
	}
	if protocol := gateway.Spec.Protocol; protocol != "" && protocol != "http" {
		return &Response{Allowed: false, Reason: fmt.Sprintf("intentGateway.spec.protocol %q is unsupported; only http is available", protocol)}
	}
	for index, route := range gateway.Spec.Routes {
		pattern := strings.TrimSpace(route.Pattern)
		if pattern == "" {
			return &Response{Allowed: false, Reason: fmt.Sprintf("intentGateway.spec.routes[%d].pattern is required", index)}
		}
		if strings.ContainsAny(pattern, "*?[") {
			if _, err := path.Match(pattern, ""); err != nil {
				return &Response{Allowed: false, Reason: fmt.Sprintf("intentGateway.spec.routes[%d].pattern is invalid: %v", index, err)}
			}
		}
	}
	if gateway.Spec.AuthConfig == nil || gateway.Spec.AuthConfig.Mode == "" || gateway.Spec.AuthConfig.Mode == "none" {
		return &Response{Allowed: true}
	}
	auth := gateway.Spec.AuthConfig
	if auth.Mode != "apiKey" {
		return &Response{Allowed: false, Reason: fmt.Sprintf("intentGateway.spec.authConfig.mode %q is invalid (must be none or apiKey)", auth.Mode)}
	}
	if auth.APIKeySecretRef == nil || strings.TrimSpace(auth.APIKeySecretRef.Name) == "" {
		return &Response{Allowed: false, Reason: "intentGateway.spec.authConfig.apiKeySecretRef.name is required when mode=apiKey"}
	}
	if strings.ContainsAny(auth.Header, "\r\n") {
		return &Response{Allowed: false, Reason: "intentGateway.spec.authConfig.header must not contain a newline"}
	}
	return &Response{Allowed: true}
}
