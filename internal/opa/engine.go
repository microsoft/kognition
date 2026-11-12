// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package opa

import (
	"context"
	"fmt"
	"sync"
)

// EvalResult represents the outcome of a policy evaluation.
type EvalResult struct {
	Allowed  bool
	Reason   string
	Warnings []string
}

// Engine evaluates OPA/Rego policies against input data.
type Engine struct {
	mu       sync.RWMutex
	policies map[string]*compiledPolicy
}

type compiledPolicy struct {
	name   string
	source string
}

// NewEngine creates a new OPA policy engine.
func NewEngine() *Engine {
	return &Engine{
		policies: make(map[string]*compiledPolicy),
	}
}

// LoadPolicy compiles and registers a Rego policy with the given name.
func (e *Engine) LoadPolicy(name, regoSource string) error {
	if regoSource == "" {
		return fmt.Errorf("empty rego source for policy %q", name)
	}
	if err := validateRegoStructure(regoSource); err != nil {
		return fmt.Errorf("invalid rego source for policy %q: %w", name, err)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	e.policies[name] = &compiledPolicy{
		name:   name,
		source: regoSource,
	}
	return nil
}

func validateRegoStructure(source string) error {
	matching := map[rune]rune{'}': '{', ']': '[', ')': '('}
	stack := make([]rune, 0)
	var quote rune
	escaped := false
	inComment := false

	for _, current := range source {
		if inComment {
			if current == '\n' {
				inComment = false
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if current == '\\' && quote == '"' {
				escaped = true
				continue
			}
			if current == quote {
				quote = 0
			}
			continue
		}

		switch current {
		case '#':
			inComment = true
		case '"', '`':
			quote = current
		case '{', '[', '(':
			stack = append(stack, current)
		case '}', ']', ')':
			if len(stack) == 0 || stack[len(stack)-1] != matching[current] {
				return fmt.Errorf("unexpected %q", current)
			}
			stack = stack[:len(stack)-1]
		}
	}

	if quote != 0 {
		return fmt.Errorf("unterminated string literal")
	}
	if len(stack) > 0 {
		return fmt.Errorf("unclosed %q", stack[len(stack)-1])
	}
	return nil
}

// RemovePolicy removes a previously loaded policy.
func (e *Engine) RemovePolicy(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.policies, name)
}

// Evaluate runs the named policy against the given input.
// Input is a map representing the admission request data.
func (e *Engine) Evaluate(_ context.Context, policyName string, input map[string]interface{}) (*EvalResult, error) {
	e.mu.RLock()
	p, ok := e.policies[policyName]
	e.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("policy %q not loaded", policyName)
	}

	return e.evaluate(p, input)
}

// EvaluateAll runs all loaded policies that match the given resource kind.
func (e *Engine) EvaluateAll(_ context.Context, input map[string]interface{}) (*EvalResult, error) {
	e.mu.RLock()
	policies := make([]*compiledPolicy, 0, len(e.policies))
	for _, p := range e.policies {
		policies = append(policies, p)
	}
	e.mu.RUnlock()

	for _, p := range policies {
		result, err := e.evaluate(p, input)
		if err != nil {
			return nil, fmt.Errorf("evaluate policy %q: %w", p.name, err)
		}
		if !result.Allowed {
			return result, nil
		}
	}

	return &EvalResult{Allowed: true}, nil
}

// evaluate interprets the Rego source against input.
// This is a simplified evaluator that looks for "deny" rules.
func (e *Engine) evaluate(p *compiledPolicy, input map[string]interface{}) (*EvalResult, error) {
	// The simplified Rego evaluator supports a basic pattern:
	// - Looks for "deny[msg]" patterns in the source
	// - Evaluates simple field comparisons against input
	//
	// For production use, this would integrate with github.com/open-policy-agent/opa/rego.
	// For now, we implement a rule-matching engine that covers the MVP patterns.

	result, err := interpretRego(p.source, input)
	if err != nil {
		return nil, fmt.Errorf("interpret policy %q: %w", p.name, err)
	}

	return result, nil
}

// PolicyCount returns the number of loaded policies.
func (e *Engine) PolicyCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.policies)
}
