// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"fmt"
	"math"
	"strconv"
)

const (
	// ModelInputCostMicrousdPerMillionTokensParameter configures an input-token
	// price in micro-USD per million tokens on ModelClass.parameters.
	ModelInputCostMicrousdPerMillionTokensParameter = "cost.inputMicrousdPerMillionTokens"
	// ModelOutputCostMicrousdPerMillionTokensParameter configures an output-token
	// price in micro-USD per million tokens on ModelClass.parameters.
	ModelOutputCostMicrousdPerMillionTokensParameter = "cost.outputMicrousdPerMillionTokens"
	modelCostTokenUnit                               = int64(1_000_000)
)

// ModelPricingSnapshot is the immutable pricing configuration used to price
// one model usage event.
type ModelPricingSnapshot struct {
	InputMicrousdPerMillionTokens  int64
	OutputMicrousdPerMillionTokens int64
	Configured                     bool
}

// ResolveModelPricing validates and snapshots ModelClass token pricing.
func ResolveModelPricing(parameters map[string]string) (ModelPricingSnapshot, error) {
	if len(parameters) == 0 {
		return ModelPricingSnapshot{}, nil
	}
	inputText, hasInput := parameters[ModelInputCostMicrousdPerMillionTokensParameter]
	outputText, hasOutput := parameters[ModelOutputCostMicrousdPerMillionTokensParameter]
	if !hasInput && !hasOutput {
		return ModelPricingSnapshot{}, nil
	}
	if !hasInput || !hasOutput {
		return ModelPricingSnapshot{Configured: true}, fmt.Errorf("ModelClass pricing requires both %s and %s", ModelInputCostMicrousdPerMillionTokensParameter, ModelOutputCostMicrousdPerMillionTokensParameter)
	}
	inputRate, err := parseModelCostRate(ModelInputCostMicrousdPerMillionTokensParameter, inputText)
	if err != nil {
		return ModelPricingSnapshot{Configured: true}, err
	}
	outputRate, err := parseModelCostRate(ModelOutputCostMicrousdPerMillionTokensParameter, outputText)
	if err != nil {
		return ModelPricingSnapshot{Configured: true}, err
	}
	return ModelPricingSnapshot{InputMicrousdPerMillionTokens: inputRate, OutputMicrousdPerMillionTokens: outputRate, Configured: true}, nil
}

// ModelCostUnits calculates normalized micro-USD cost from ModelClass pricing
// parameters and provider-reported token usage. configured is false when the
// ModelClass does not configure either price.
func ModelCostUnits(parameters map[string]string, promptTokens, completionTokens int64) (cost CostUnit, configured bool, err error) {
	pricing, err := ResolveModelPricing(parameters)
	if err != nil || !pricing.Configured {
		return 0, pricing.Configured, err
	}
	if promptTokens < 0 || completionTokens < 0 {
		return 0, true, fmt.Errorf("model token usage cannot be negative")
	}
	inputNumerator, err := modelCostNumerator(promptTokens, pricing.InputMicrousdPerMillionTokens)
	if err != nil {
		return 0, true, err
	}
	outputNumerator, err := modelCostNumerator(completionTokens, pricing.OutputMicrousdPerMillionTokens)
	if err != nil {
		return 0, true, err
	}
	if inputNumerator > math.MaxInt64-outputNumerator {
		return 0, true, fmt.Errorf("model cost exceeds supported range")
	}
	numerator := inputNumerator + outputNumerator
	if numerator == 0 {
		return 0, true, nil
	}
	return CostUnit((numerator-1)/modelCostTokenUnit + 1), true, nil
}

func parseModelCostRate(name, value string) (int64, error) {
	rate, err := strconv.ParseInt(value, 10, 64)
	if err != nil || rate < 0 {
		return 0, fmt.Errorf("ModelClass pricing %s must be a non-negative integer micro-USD rate", name)
	}
	return rate, nil
}

func modelCostNumerator(tokens, rate int64) (int64, error) {
	if tokens == 0 || rate == 0 {
		return 0, nil
	}
	if tokens > math.MaxInt64/rate {
		return 0, fmt.Errorf("model cost exceeds supported range")
	}
	return tokens * rate, nil
}
