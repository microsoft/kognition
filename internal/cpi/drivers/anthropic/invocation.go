// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package anthropic

import (
	"fmt"

	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func invocationAPIVersion(parameters map[string]string) string {
	if version := parameters["apiVersion"]; version != "" {
		return version
	}
	if version := parameters["api-version"]; version != "" {
		return version
	}
	return apiVersion
}

func (d *Driver) InvocationContract(name string) (modelinvocation.Contract, error) {
	if name != "anthropic-messages-v1" {
		return modelinvocation.Contract{}, fmt.Errorf("unsupported Anthropic invocation contract %q", name)
	}

	return modelinvocation.Contract{
		Name: name, Operation: "chat", Protocol: "anthropic-messages",
		CompletionLimitParameter: "max_tokens", Temperature: true, MaximumTemperature: 1,
		ToolCalling: true, DisabledReasoning: true,
		APIVersion: true,
	}, nil
}
