// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"encoding/json"
	"fmt"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/store"
)

// InvocationContractProvider is implemented only by adapters with explicit contracts.
type InvocationContractProvider interface {
	InvocationContract(string) (modelinvocation.Contract, error)
}

func BindCompletion(ctx context.Context, storage store.Store, driver Driver, model *api.Model, request *CompletionRequest) (*CompletionRequest, error) {
	resolved, err := ResolveInvocation(ctx, storage, driver, model)
	if err != nil || resolved == nil {
		return request, err
	}
	result := *request
	result.Invocation = resolved
	provider, ok := driver.(InvocationContractProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q has no invocation contract support", driver.Name())
	}
	return PrepareCompletion(&result, provider)
}

func ResolveInvocation(ctx context.Context, storage store.Store, driver Driver, model *api.Model) (*modelinvocation.Resolved, error) {
	resolved, err := modelinvocation.Load(ctx, storage, model)
	if err != nil || resolved == nil {
		return resolved, err
	}
	provider, ok := driver.(InvocationContractProvider)
	if !ok {
		return nil, fmt.Errorf("provider %q has no invocation contract support", driver.Name())
	}
	contract, err := provider.InvocationContract(model.Spec.InvocationContract)
	if err != nil {
		return nil, err
	}
	if err := resolved.ValidateContract(contract); err != nil {
		return nil, err
	}
	if _, err := resolved.Prepare(contract, model.Spec.Parameters, false, false, 0); err != nil {
		return nil, err
	}
	return resolved, nil
}

func PrepareCompletion(request *CompletionRequest, driver InvocationContractProvider) (*CompletionRequest, error) {
	if request.Invocation == nil {
		return request, nil
	}
	for _, message := range request.Messages {
		switch message.Role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("unsupported canonical message role %q", message.Role)
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Name == "" || !json.Valid([]byte(call.Arguments)) {
				return nil, fmt.Errorf("invalid canonical function tool call")
			}
		}
	}
	contract, err := driver.InvocationContract(request.Invocation.Binding.Contract)
	if err != nil {
		return nil, err
	}
	if contract.Operation != "chat" {
		return nil, fmt.Errorf("chat requires a chat invocation contract")
	}
	parameters, err := request.Invocation.Prepare(contract, request.Parameters, len(request.Tools) > 0, false, request.MaxCompletionTokens)
	if err != nil {
		return nil, err
	}
	result := *request
	result.Parameters = parameters
	return &result, nil
}

func PrepareEmbedding(request *EmbedRequest, driver InvocationContractProvider) (*EmbedRequest, error) {
	if request.Invocation == nil {
		return request, nil
	}
	contract, err := driver.InvocationContract(request.Invocation.Binding.Contract)
	if err != nil {
		return nil, err
	}
	if contract.Operation != "embedding" {
		return nil, fmt.Errorf("embedding requires an embedding invocation contract")
	}
	parameters, err := request.Invocation.Prepare(contract, request.Parameters, false, false, 0)
	if err != nil {
		return nil, err
	}
	result := *request
	result.Parameters = parameters
	return &result, nil
}
