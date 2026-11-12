// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package modelinvocation resolves portable intent against adapter-owned contracts.
package modelinvocation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	strictjson "sigs.k8s.io/json"
)

// Contract is supplied by a trusted CPI adapter, never by an inference caller.
type Contract struct {
	Name                          string
	Operation                     string
	Protocol                      string
	CompletionLimitParameter      string
	Temperature                   bool
	MaximumTemperature            float64
	ToolCalling                   bool
	StructuredOutput              bool
	DisabledReasoning             bool
	DisabledReasoningValue        string
	RequiredReasoningValue        string
	ToolsRequireReasoningDisabled bool
	Dimensions                    bool
	APIVersion                    bool
}

type Resolved struct {
	Binding         api.ModelInvocationProfileBinding
	Spec            api.ModelInvocationProfileSpec
	ResourceVersion int64
}

func Digest(spec api.ModelInvocationProfileSpec) (string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode invocation profile: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return "", err
	}
	for name, value := range fields {
		if string(value) == "{}" {
			delete(fields, name)
		}
	}
	encoded, err = json.Marshal(fields)
	if err != nil {
		return "", err
	}
	encoded, err = runtimeapproval.CanonicalizeJSON(encoded)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("kognition.model-invocation/v1\n"), encoded...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func ValidateModel(spec api.ModelSpec) error {
	if spec.InvocationProfileRef == nil {
		if spec.InvocationContract != "" {
			return fmt.Errorf("invocationContract requires invocationProfileRef")
		}
		return nil
	}
	if spec.InvocationProfileRef.Name == "" || spec.InvocationContract == "" || len(spec.InvocationContract) > 128 {
		return fmt.Errorf("invocationProfileRef requires a name and versioned invocationContract")
	}
	if !ValidDigest(spec.InvocationProfileRef.SpecDigest) {
		return fmt.Errorf("invocationProfileRef requires the reviewed profile specDigest")
	}
	for name := range spec.Parameters {
		if name != "apiVersion" && name != "api-version" {
			return fmt.Errorf("profile-backed Model parameter %q must be declared in the invocation profile", name)
		}
	}
	if spec.Parameters["apiVersion"] != "" && spec.Parameters["api-version"] != "" {
		return fmt.Errorf("specify only one API version parameter")
	}
	return nil
}

func Load(ctx context.Context, storage store.Store, model *api.Model) (*Resolved, error) {
	if model == nil {
		return nil, fmt.Errorf("model is required to resolve an invocation profile")
	}
	if err := ValidateModel(model.Spec); err != nil {
		return nil, err
	}
	if model.Spec.InvocationProfileRef == nil {
		return nil, nil
	}
	if storage == nil || model.Namespace == "" {
		return nil, fmt.Errorf("invocation profiles require a namespace-local store")
	}
	name := model.Spec.InvocationProfileRef.Name
	raw, err := storage.Get(ctx, "ModelInvocationProfile", model.Namespace, name)
	if err != nil {
		return nil, fmt.Errorf("load invocation profile %s/%s: %w", model.Namespace, name, err)
	}
	var profile api.ModelInvocationProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return nil, fmt.Errorf("decode invocation profile: %w", err)
	}
	if profile.Name != name || profile.Namespace != model.Namespace || profile.UID == "" || profile.DeletionTimestamp != nil {
		return nil, fmt.Errorf("invocation profile identity is unavailable or not namespace-local")
	}
	if err := profile.Spec.Validate(); err != nil {
		return nil, err
	}
	digest, err := Digest(profile.Spec)
	if err != nil {
		return nil, err
	}
	if digest != model.Spec.InvocationProfileRef.SpecDigest {
		return nil, fmt.Errorf("invocation profile spec digest differs from the reviewed Model reference")
	}
	return &Resolved{
		Binding:         api.ModelInvocationProfileBinding{Name: name, UID: profile.UID, SpecDigest: digest, Contract: model.Spec.InvocationContract},
		Spec:            profile.Spec,
		ResourceVersion: profile.ResourceVersion,
	}, nil
}

func EqualBinding(left, right *api.ModelInvocationProfileBinding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *Resolved) ValidateContract(contract Contract) error {
	if r == nil {
		return nil
	}
	if contract.Name != r.Binding.Contract || contract.Operation != r.Spec.Operation {
		return fmt.Errorf("invocation contract %q does not support operation %q", r.Binding.Contract, r.Spec.Operation)
	}
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	if r.Spec.Requirements.ToolCalling == "required" && !contract.ToolCalling {
		return fmt.Errorf("invocation contract does not support required tool calling")
	}
	if r.Spec.Requirements.StructuredOutput == "required" && !contract.StructuredOutput {
		return fmt.Errorf("invocation contract does not support required structured output")
	}
	_, err := r.Prepare(contract, nil, r.Spec.Requirements.ToolCalling == "required", r.Spec.Requirements.StructuredOutput == "required", 0)
	return err
}

// Prepare rejects incompatible explicit intent and supplies defaults only for omissions.
func (r *Resolved) Prepare(contract Contract, supplied map[string]string, tools, structured bool, callerLimit int64) (map[string]string, error) {
	if r == nil || contract.Name != r.Binding.Contract || contract.Operation != r.Spec.Operation {
		return nil, fmt.Errorf("matching resolved invocation profile and adapter contract are required")
	}
	if tools && (!contract.ToolCalling || r.Spec.Requirements.ToolCalling == "forbidden") {
		return nil, fmt.Errorf("tool calling is forbidden by the invocation contract or profile")
	}
	if structured && (!contract.StructuredOutput || r.Spec.Requirements.StructuredOutput == "forbidden") {
		return nil, fmt.Errorf("structured output is forbidden by the invocation contract or profile")
	}
	parameters := make(map[string]string)
	var limit *int64
	var temperature *float64
	var dimensions *int64
	reasoning := ""
	for name, value := range supplied {
		switch name {
		case "apiVersion", "api-version":
			if !contract.APIVersion {
				return nil, fmt.Errorf("transport API-version overrides are unsupported by invocation contract %q", contract.Name)
			}
			parameters[name] = value
		case "max_tokens", "max_completion_tokens", "maxCompletionTokens":
			if limit != nil {
				return nil, fmt.Errorf("conflicting completion-token limits")
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 1 || parsed > api.MaxInvocationCompletionTokens {
				return nil, fmt.Errorf("%s must be a positive integer", name)
			}
			limit = &parsed
		case "temperature":
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > 2 {
				return nil, fmt.Errorf("temperature must be finite and between 0 and 2")
			}
			temperature = &parsed
		case "reasoning", "reasoning_effort":
			if reasoning != "" {
				return nil, fmt.Errorf("conflicting reasoning parameters")
			}
			switch {
			case name == "reasoning" && (value == "providerDefault" || value == "disabled" || value == "required"):
				reasoning = value
			case name == "reasoning_effort" && value == "none" && contract.DisabledReasoningValue == "none":
				reasoning = "disabled"
			case name == "reasoning_effort" && value != "" && value == contract.RequiredReasoningValue:
				reasoning = "required"
			default:
				return nil, fmt.Errorf("reasoning setting %q is unsupported by invocation contract %q", value, contract.Name)
			}
		case "dimensions":
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed < 1 || parsed > 65536 {
				return nil, fmt.Errorf("embedding dimensions must be between 1 and 65536")
			}
			dimensions = &parsed
		default:
			return nil, fmt.Errorf("unknown invocation parameter %q", name)
		}
	}
	if r.Spec.Operation == "embedding" {
		if limit != nil || temperature != nil || reasoning != "" || tools || structured {
			return nil, fmt.Errorf("embedding invocation cannot contain chat settings")
		}
		if dimensions == nil {
			dimensions = r.Spec.Defaults.Dimensions
		}
		if dimensions != nil {
			if !contract.Dimensions {
				return nil, fmt.Errorf("embedding dimensions are unsupported by this invocation contract")
			}
			parameters["dimensions"] = strconv.FormatInt(*dimensions, 10)
		}
		return parameters, nil
	}
	if dimensions != nil {
		return nil, fmt.Errorf("chat invocation cannot contain embedding dimensions")
	}
	if limit == nil {
		if callerLimit > 0 {
			limit = &callerLimit
		} else {
			limit = r.Spec.Defaults.MaxCompletionTokens
		}
	}
	if limit == nil || *limit < 1 || *limit > api.MaxInvocationCompletionTokens || contract.CompletionLimitParameter == "" {
		return nil, fmt.Errorf("positive completion-token limit and contract encoding are required")
	}
	if r.Spec.Limits.MaxCompletionTokens != nil && *limit > *r.Spec.Limits.MaxCompletionTokens {
		return nil, fmt.Errorf("completion-token limit exceeds the invocation profile hard limit")
	}
	if callerLimit > 0 && *limit > callerLimit {
		return nil, fmt.Errorf("completion-token limit exceeds the Agent invocation limit")
	}
	parameters[contract.CompletionLimitParameter] = strconv.FormatInt(*limit, 10)
	if temperature == nil {
		temperature = r.Spec.Defaults.Temperature
	}
	if temperature != nil {
		if !contract.Temperature {
			return nil, fmt.Errorf("temperature is unsupported by invocation contract %q; omit it to use provider defaults", contract.Name)
		}
		if contract.MaximumTemperature > 0 && *temperature > contract.MaximumTemperature {
			return nil, fmt.Errorf("temperature exceeds the invocation contract limit")
		}
		parameters["temperature"] = strconv.FormatFloat(*temperature, 'g', -1, 64)
	}
	if requirement := r.Spec.Requirements.Reasoning; requirement != "" {
		if reasoning != "" && reasoning != "providerDefault" && reasoning != requirement {
			return nil, fmt.Errorf("explicit reasoning conflicts with the invocation profile requirement")
		}
		if reasoning == "providerDefault" {
			return nil, fmt.Errorf("provider-default reasoning cannot satisfy an explicit reasoning requirement")
		}
		reasoning = requirement
	}
	if reasoning == "" {
		reasoning = r.Spec.Defaults.Reasoning
	}
	switch reasoning {
	case "", "providerDefault":
	case "disabled":
		if !contract.DisabledReasoning {
			return nil, fmt.Errorf("disabling reasoning is unsupported by invocation contract %q", contract.Name)
		}
		if contract.DisabledReasoningValue != "" {
			parameters["reasoning_effort"] = contract.DisabledReasoningValue
		}
	case "required":
		if contract.RequiredReasoningValue == "" {
			return nil, fmt.Errorf("required reasoning is unsupported by invocation contract %q", contract.Name)
		}
		parameters["reasoning_effort"] = contract.RequiredReasoningValue
	default:
		return nil, fmt.Errorf("unsupported reasoning mode")
	}
	if tools && contract.ToolsRequireReasoningDisabled && reasoning != "disabled" {
		return nil, fmt.Errorf("this invocation contract requires explicitly disabled reasoning with tools")
	}
	return parameters, nil
}

// PrepareChatJSON preserves the supported chat envelope while validating generation settings.
func (r *Resolved) PrepareChatJSON(contract Contract, payload []byte, callerLimit int64) ([]byte, error) {
	if contract.Operation != "chat" || r.Spec.Operation != "chat" {
		return nil, fmt.Errorf("chat endpoint requires a chat invocation profile and contract")
	}
	var request map[string]json.RawMessage
	strictErrors, err := strictjson.UnmarshalStrict(payload, &request)
	if err != nil || len(strictErrors) > 0 || request == nil {
		return nil, fmt.Errorf("invocation request must be a JSON object")
	}
	supplied := map[string]string{}
	for key, value := range request {
		switch key {
		case "model", "messages", "tools", "tool_choice", "parallel_tool_calls", "response_format", "user":
		case "stream":
			if string(value) != "false" {
				return nil, fmt.Errorf("streaming is unsupported by this invocation endpoint")
			}
		default:
			switch key {
			case "max_tokens", "max_completion_tokens", "temperature":
				if len(value) == 0 || value[0] == '"' || string(value) == "null" {
					return nil, fmt.Errorf("%s must be a JSON number", key)
				}
				supplied[key] = string(value)
			case "reasoning", "reasoning_effort":
				var text string
				if err := json.Unmarshal(value, &text); err != nil || text == "" {
					return nil, fmt.Errorf("%s must be a nonempty string", key)
				}
				supplied[key] = text
			default:
				return nil, fmt.Errorf("unknown invocation parameter %q", key)
			}
			delete(request, key)
		}
	}
	var tools []json.RawMessage
	if raw, exists := request["tools"]; exists {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("tools must be an array")
		}
	}
	structured := false
	if raw, exists := request["response_format"]; exists {
		var format struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &format); err != nil || format.Type == "" {
			return nil, fmt.Errorf("response_format must declare a type")
		}
		structured = format.Type != "text"
	}
	parameters, err := r.Prepare(contract, supplied, len(tools) > 0, structured, callerLimit)
	if err != nil {
		return nil, err
	}
	for key, value := range parameters {
		if key == "apiVersion" || key == "api-version" {
			continue
		}
		if key == "reasoning_effort" {
			request[key], _ = json.Marshal(value)
		} else {
			request[key] = json.RawMessage(value)
		}
	}
	return json.Marshal(request)
}

func ValidBinding(binding *api.ModelInvocationProfileBinding) bool {
	if binding == nil {
		return true
	}
	if binding.Name == "" || binding.UID == "" || binding.Contract == "" ||
		!strings.HasPrefix(binding.SpecDigest, "sha256:") || len(binding.SpecDigest) != 71 {
		return false
	}
	return ValidDigest(binding.SpecDigest)
}

func ValidDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return false
	}
	encoded := strings.TrimPrefix(digest, "sha256:")
	_, err := hex.DecodeString(encoded)
	return err == nil && encoded == strings.ToLower(encoded)
}
