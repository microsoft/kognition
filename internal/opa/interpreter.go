// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package opa

import (
	"fmt"
	"strings"
)

// interpretRego provides a simplified Rego interpreter that handles
// the common patterns used in cognitive admission policies:
//
//   - deny[msg] { condition }
//   - Simple field access: input.metadata.namespace, input.spec.field
//   - Equality and inequality comparisons
//   - String matching
//
// For production, this would be replaced by the full OPA/Rego engine.
func interpretRego(source string, input map[string]interface{}) (*EvalResult, error) {
	lines := strings.Split(source, "\n")
	var denials []string

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])

		// Look for deny rules: deny[msg] { ... } or deny[msg] := ... { ... }
		if !strings.HasPrefix(line, "deny[") {
			continue
		}

		// Extract the rule body (between { and })
		body := extractBody(lines, i)
		if body == "" {
			continue
		}

		// Evaluate conditions in the body.
		denied, msg := evaluateBody(body, input)
		if denied {
			denials = append(denials, msg)
		}
	}

	if len(denials) > 0 {
		return &EvalResult{
			Allowed: false,
			Reason:  strings.Join(denials, "; "),
		}, nil
	}

	return &EvalResult{Allowed: true}, nil
}

// extractBody extracts the rule body between { and }, handling multi-line rules.
func extractBody(lines []string, startIdx int) string {
	combined := ""
	braceCount := 0
	inBody := false

	for i := startIdx; i < len(lines); i++ {
		line := lines[i]
		for _, ch := range line {
			if ch == '{' {
				braceCount++
				if !inBody {
					inBody = true
					continue
				}
			}
			if ch == '}' {
				braceCount--
				if braceCount == 0 {
					return strings.TrimSpace(combined)
				}
			}
			if inBody {
				combined += string(ch)
			}
		}
		if inBody {
			combined += "\n"
		}
	}
	return strings.TrimSpace(combined)
}

// evaluateBody processes the conditions inside a deny rule body.
func evaluateBody(body string, input map[string]interface{}) (bool, string) {
	conditions := splitConditions(body)
	msg := ""

	for _, cond := range conditions {
		cond = strings.TrimSpace(cond)
		if cond == "" {
			continue
		}

		// msg assignment: msg := "..." or msg = "..."
		if strings.HasPrefix(cond, "msg") {
			if idx := strings.Index(cond, ":="); idx >= 0 {
				msg = extractString(cond[idx+2:])
			} else if idx := strings.Index(cond, "="); idx >= 0 {
				msg = extractString(cond[idx+1:])
			}
			continue
		}

		// Evaluate the condition.
		if !evaluateCondition(cond, input) {
			return false, ""
		}
	}

	if msg == "" {
		msg = "policy violation"
	}

	return true, msg
}

// splitConditions splits a rule body into individual conditions.
// Conditions are separated by newlines or semicolons.
func splitConditions(body string) []string {
	// First split by newlines, then by semicolons.
	var conditions []string
	for _, line := range strings.Split(body, "\n") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part != "" {
				conditions = append(conditions, part)
			}
		}
	}
	return conditions
}

// evaluateCondition evaluates a single condition against the input.
func evaluateCondition(cond string, input map[string]interface{}) bool {
	if strings.HasPrefix(cond, "contains(") {
		return evaluateContains(cond, input)
	}

	// Not-equal: input.field != "value"
	if parts := strings.SplitN(cond, "!=", 2); len(parts) == 2 {
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		leftVal := resolveValue(left, input)
		rightVal := resolveValue(right, input)
		return fmt.Sprintf("%v", leftVal) != fmt.Sprintf("%v", rightVal)
	}

	// Equality: input.field == "value" or input.field = "value"
	if parts := strings.SplitN(cond, "==", 2); len(parts) == 2 {
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		leftVal := resolveValue(left, input)
		rightVal := resolveValue(right, input)
		return fmt.Sprintf("%v", leftVal) == fmt.Sprintf("%v", rightVal)
	}

	// not operator: not input.field
	if strings.HasPrefix(cond, "not ") {
		inner := strings.TrimPrefix(cond, "not ")
		val := resolveValue(strings.TrimSpace(inner), input)
		return val == nil || val == false || val == "" || val == 0
	}

	// Simple field existence check: input.metadata.annotations["key"]
	if strings.HasPrefix(cond, "input.") {
		val := resolveValue(cond, input)
		return val != nil && val != "" && val != false
	}

	return false
}

func evaluateContains(cond string, input map[string]interface{}) bool {
	if !strings.HasSuffix(cond, ")") {
		return false
	}
	args := splitFunctionArgs(strings.TrimSuffix(strings.TrimPrefix(cond, "contains("), ")"))
	if len(args) != 2 {
		return false
	}
	haystack, ok := resolveStringExpression(args[0], input)
	if !ok {
		return false
	}
	needle, ok := resolveStringExpression(args[1], input)
	if !ok {
		return false
	}
	return strings.Contains(haystack, needle)
}

func splitFunctionArgs(args string) []string {
	var out []string
	depth := 0
	quote := rune(0)
	start := 0
	for i, r := range args {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '`':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			if depth > 0 {
				depth--
			}
		case r == ',' && depth == 0:
			out = append(out, strings.TrimSpace(args[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(args[start:]))
	return out
}

func resolveStringExpression(expr string, input map[string]interface{}) (string, bool) {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "lower(") && strings.HasSuffix(expr, ")") {
		value, ok := resolveStringExpression(strings.TrimSuffix(strings.TrimPrefix(expr, "lower("), ")"), input)
		if !ok {
			return "", false
		}
		return strings.ToLower(value), true
	}
	value := resolveValue(expr, input)
	if value == nil {
		return "", false
	}
	return fmt.Sprintf("%v", value), true
}

// resolveValue resolves a value reference against the input map.
func resolveValue(ref string, input map[string]interface{}) interface{} {
	ref = strings.TrimSpace(ref)

	// String literal: "value" or `value`
	if (strings.HasPrefix(ref, "\"") && strings.HasSuffix(ref, "\"")) ||
		(strings.HasPrefix(ref, "`") && strings.HasSuffix(ref, "`")) {
		return ref[1 : len(ref)-1]
	}

	// Numeric literal.
	if ref == "true" {
		return true
	}
	if ref == "false" {
		return false
	}

	// Input field path: input.metadata.namespace, input.spec.field
	if strings.HasPrefix(ref, "input.") {
		path := strings.TrimPrefix(ref, "input.")
		return resolvePath(path, input)
	}

	return ref
}

// resolvePath traverses a dot-separated path in a map.
func resolvePath(path string, data map[string]interface{}) interface{} {
	parts := splitPath(path)
	var current interface{} = data

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]interface{}:
			current = v[part]
		default:
			return nil
		}
	}
	return current
}

// splitPath splits a dotted path, handling bracket notation: metadata.annotations["key"]
func splitPath(path string) []string {
	var parts []string
	current := ""

	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '.':
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
		case '[':
			if current != "" {
				parts = append(parts, current)
				current = ""
			}
			// Extract the key inside brackets.
			i++ // skip '['
			key := ""
			for i < len(path) && path[i] != ']' {
				if path[i] != '"' && path[i] != '\'' {
					key += string(path[i])
				}
				i++
			}
			if key != "" {
				parts = append(parts, key)
			}
		default:
			current += string(path[i])
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// extractString extracts a string value from a Rego expression.
func extractString(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "\"") && strings.HasSuffix(s, "\"") {
		return s[1 : len(s)-1]
	}
	if strings.HasPrefix(s, "`") && strings.HasSuffix(s, "`") {
		return s[1 : len(s)-1]
	}
	// Handle sprintf or concat patterns: just return the raw string
	if idx := strings.Index(s, "\""); idx >= 0 {
		end := strings.LastIndex(s, "\"")
		if end > idx {
			return s[idx+1 : end]
		}
	}
	return s
}
