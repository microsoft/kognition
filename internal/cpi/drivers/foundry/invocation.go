// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

import (
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func (d *Driver) InvocationContract(name string) (modelinvocation.Contract, error) {
	return openai.InvocationContract(name)
}
