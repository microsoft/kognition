// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// RedactionLevel controls how aggressively PII is redacted.
type RedactionLevel string

const (
	// RedactionNone disables redaction (use only in development).
	RedactionNone RedactionLevel = "none"
	// RedactionStandard applies default PII patterns.
	RedactionStandard RedactionLevel = "standard"
	// RedactionStrict redacts all free-form text fields entirely.
	RedactionStrict RedactionLevel = "strict"
)

// RedactableField identifies span/event fields that may contain PII.
type RedactableField string

const (
	FieldToolArgs         RedactableField = "tool.arguments"
	FieldToolResult       RedactableField = "tool.result"
	FieldMemoryValue      RedactableField = "memory.value"
	FieldReasoningContent RedactableField = "reasoning.content"
)

// RedactionRule defines a named pattern that matches PII.
type RedactionRule struct {
	Name        string
	Pattern     *regexp.Regexp
	Replacement string
}

// DefaultRules returns the standard set of PII detection rules.
func DefaultRules() []RedactionRule {
	return []RedactionRule{
		{
			Name:        "email",
			Pattern:     regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`),
			Replacement: "[REDACTED:email]",
		},
		{
			Name:        "phone_us",
			Pattern:     regexp.MustCompile(`\b(\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}\b`),
			Replacement: "[REDACTED:phone]",
		},
		{
			Name:        "ssn",
			Pattern:     regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),
			Replacement: "[REDACTED:ssn]",
		},
		{
			Name:        "credit_card",
			Pattern:     regexp.MustCompile(`\b\d{4}[-\s]?\d{4}[-\s]?\d{4}[-\s]?\d{4}\b`),
			Replacement: "[REDACTED:credit_card]",
		},
		{
			Name:        "ip_address",
			Pattern:     regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
			Replacement: "[REDACTED:ip]",
		},
		{
			Name:        "jwt_token",
			Pattern:     regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
			Replacement: "[REDACTED:token]",
		},
		{
			Name:        "bearer_token",
			Pattern:     regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9._~+/\-]+=*`),
			Replacement: "[REDACTED:bearer]",
		},
		{
			Name:        "api_key_header",
			Pattern:     regexp.MustCompile(`(?i)(api[_-]?key|authorization|secret|password|token)\s*[:=]\s*\S+`),
			Replacement: "[REDACTED:credential]",
		},
	}
}

// RedactionConfig holds workspace-level redaction settings.
type RedactionConfig struct {
	Level          RedactionLevel
	Rules          []RedactionRule
	FieldOverrides map[RedactableField]RedactionLevel
}

// DefaultRedactionConfig returns a config with standard rules.
func DefaultRedactionConfig() *RedactionConfig {
	return &RedactionConfig{
		Level:          RedactionStandard,
		Rules:          DefaultRules(),
		FieldOverrides: make(map[RedactableField]RedactionLevel),
	}
}

// Redactor applies PII redaction to span/event attribute values.
type Redactor struct {
	config *RedactionConfig
}

// NewRedactor creates a redactor with the given configuration.
func NewRedactor(config *RedactionConfig) *Redactor {
	if config == nil {
		config = DefaultRedactionConfig()
	}
	return &Redactor{config: config}
}

// Redact applies redaction rules to a value based on the field type.
func (r *Redactor) Redact(field RedactableField, value string) string {
	level := r.effectiveLevel(field)
	switch level {
	case RedactionNone:
		return value
	case RedactionStrict:
		return "[REDACTED]"
	default:
		return r.applyRules(value)
	}
}

// RedactSpanAttributes applies redaction to all redactable attributes in a span.
func (r *Redactor) RedactSpanAttributes(attrs map[string]interface{}) map[string]interface{} {
	if r.config.Level == RedactionNone {
		return attrs
	}

	result := make(map[string]interface{}, len(attrs))
	for k, v := range attrs {
		field := attrKeyToField(k)
		if field == "" {
			result[k] = v
			continue
		}

		result[k] = r.Redact(field, valueToRedactableString(v))
	}
	return result
}

func valueToRedactableString(value interface{}) string {
	if str, ok := value.(string); ok {
		return str
	}
	encoded, err := json.Marshal(value)
	if err == nil {
		return string(encoded)
	}
	return fmt.Sprint(value)
}

// effectiveLevel returns the redaction level for a specific field,
// considering field-level overrides.
func (r *Redactor) effectiveLevel(field RedactableField) RedactionLevel {
	if override, ok := r.config.FieldOverrides[field]; ok {
		return override
	}
	return r.config.Level
}

// applyRules runs all redaction patterns on the input.
func (r *Redactor) applyRules(value string) string {
	result := value
	for _, rule := range r.config.Rules {
		result = rule.Pattern.ReplaceAllString(result, rule.Replacement)
	}
	return result
}

// attrKeyToField maps span attribute keys to redactable field identifiers.
func attrKeyToField(key string) RedactableField {
	switch {
	case strings.HasSuffix(key, ".arguments") || key == AttrToolArgs:
		return FieldToolArgs
	case strings.HasSuffix(key, ".result") || key == AttrToolResult:
		return FieldToolResult
	case key == AttrMemoryValue:
		return FieldMemoryValue
	case key == AttrReasoningContent:
		return FieldReasoningContent
	default:
		return ""
	}
}
