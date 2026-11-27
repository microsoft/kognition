// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

// externalAgentResolveTimeout bounds the admission-time DNS check.
const externalAgentResolveTimeout = 3 * time.Second

// ExternalAgentValidatorOption configures an ExternalAgentValidator.
type ExternalAgentValidatorOption func(*ExternalAgentValidator)

// WithExternalAgentEgress enables the operator egress policy. When enabled, an
// endpoint must be permitted by the allow-list; an empty allow-list permits
// nothing. When not enabled, only the structural and address checks run.
func WithExternalAgentEgress(allowList *externalagent.AllowList) ExternalAgentValidatorOption {
	return func(v *ExternalAgentValidator) {
		v.egressEnforced = true
		v.allowList = allowList
	}
}

// WithExternalAgentResolver replaces the DNS resolver. It exists for tests and
// for deployments that must resolve through a specific resolver.
func WithExternalAgentResolver(resolver externalagent.Resolver) ExternalAgentValidatorOption {
	return func(v *ExternalAgentValidator) { v.resolver = resolver }
}

// ExternalAgentValidator validates ExternalAgent resources on CREATE and UPDATE.
//
// The spec is immutable: a change to the endpoint, pinned card, credential or
// limits is a different trust decision and needs a new resource. The validator
// rejects an endpoint that resolves to a forbidden address. It does not reject
// a name that does not resolve yet or a credential that does not exist yet;
// the status reconciler reports those so a resource can be applied before its
// dependencies.
type ExternalAgentValidator struct {
	egressEnforced bool
	allowList      *externalagent.AllowList
	resolver       externalagent.Resolver
}

// NewExternalAgentValidator creates an ExternalAgentValidator.
func NewExternalAgentValidator(opts ...ExternalAgentValidatorOption) *ExternalAgentValidator {
	v := &ExternalAgentValidator{}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (v *ExternalAgentValidator) Name() string { return "externalagent-validator" }

func (v *ExternalAgentValidator) Handles(kind string, op Operation) bool {
	return kind == "ExternalAgent" && (op == OperationCreate || op == OperationUpdate)
}

func (v *ExternalAgentValidator) Validate(ctx context.Context, req *Request) *Response {
	var agent v1alpha1.ExternalAgent
	if err := json.Unmarshal(req.Object, &agent); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid externalAgent JSON: %v", err)}
	}
	if err := agent.Spec.Validate(); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	if req.Operation == OperationUpdate && len(req.OldObject) > 0 {
		var previous v1alpha1.ExternalAgent
		if err := json.Unmarshal(req.OldObject, &previous); err != nil {
			return &Response{Allowed: false, Reason: fmt.Sprintf("invalid previous externalAgent JSON: %v", err)}
		}
		if !reflect.DeepEqual(previous.Spec, agent.Spec) {
			return &Response{Allowed: false, Reason: "externalAgent.spec is immutable; create a new ExternalAgent to change the endpoint, card, credential or limits"}
		}
		return &Response{Allowed: true}
	}
	host, port, err := v1alpha1.ParseExternalAgentEndpoint(agent.Spec.Endpoint)
	if err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("externalAgent.spec.endpoint %v", err)}
	}
	if v.egressEnforced && !v.allowList.Permits(host, port) {
		return &Response{Allowed: false, Reason: fmt.Sprintf("externalAgent.spec.endpoint %s:%d is not in the operator egress allow-list", host, port)}
	}
	resolver := v.resolver
	if resolver == nil {
		resolver = externalagent.DefaultResolver()
	}
	lookupContext, cancel := context.WithTimeout(ctx, externalAgentResolveTimeout)
	defer cancel()
	if _, err := externalagent.ResolvePermitted(lookupContext, resolver, host); err != nil && !errors.Is(err, externalagent.ErrUnresolvable) {
		return &Response{Allowed: false, Reason: fmt.Sprintf("externalAgent.spec.endpoint %q: %v", agent.Spec.Endpoint, err)}
	}
	return &Response{Allowed: true}
}

// ExternalAgentGrantValidator validates ExternalAgentGrant resources.
type ExternalAgentGrantValidator struct{}

// NewExternalAgentGrantValidator creates an ExternalAgentGrantValidator.
func NewExternalAgentGrantValidator() *ExternalAgentGrantValidator {
	return &ExternalAgentGrantValidator{}
}

func (v *ExternalAgentGrantValidator) Name() string { return "externalagentgrant-validator" }

func (v *ExternalAgentGrantValidator) Handles(kind string, op Operation) bool {
	return kind == "ExternalAgentGrant" && (op == OperationCreate || op == OperationUpdate)
}

func (v *ExternalAgentGrantValidator) Validate(_ context.Context, req *Request) *Response {
	var grant v1alpha1.ExternalAgentGrant
	if err := json.Unmarshal(req.Object, &grant); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid externalAgentGrant JSON: %v", err)}
	}
	if err := grant.Spec.Validate(); err != nil {
		return &Response{Allowed: false, Reason: err.Error()}
	}
	return &Response{Allowed: true}
}
