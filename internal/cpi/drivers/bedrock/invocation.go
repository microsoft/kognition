// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bedrock

import (
	"fmt"

	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func (d *Driver) InvocationContract(name string) (modelinvocation.Contract, error) {
	if name != "bedrock-converse-v1" {
		return modelinvocation.Contract{}, fmt.Errorf("unsupported Bedrock invocation contract %q", name)
	}
	return modelinvocation.Contract{
		Name: name, Operation: "chat", Protocol: "bedrock-converse",
		CompletionLimitParameter: "max_tokens", Temperature: true, MaximumTemperature: 1,
		ToolCalling: true,
	}, nil
}
