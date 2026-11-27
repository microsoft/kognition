// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/store"
)

// Operation represents the type of API operation being admitted.
type Operation string

const (
	OperationCreate Operation = "CREATE"
	OperationUpdate Operation = "UPDATE"
	OperationDelete Operation = "DELETE"
)

// Request carries information about the resource being admitted.
type Request struct {
	Operation Operation
	Kind      string
	Namespace string
	Name      string

	// Object is the resource body (after mutating hooks have run for validating hooks).
	Object json.RawMessage

	// OldObject is the existing resource body (only set for UPDATE operations).
	OldObject json.RawMessage

	// UserInfo carries the authenticated identity that made the request.
	// Populated by the K8s webhook handler from AdmissionRequest.UserInfo.
	// May be zero-valued when the request originates from the in-process apiserver
	// path (which does not currently perform authentication).
	UserInfo UserInfo
}

// UserInfo identifies the caller that originated the admission request.
type UserInfo struct {
	Username string
	Groups   []string
	Extra    map[string][]string
}

// Response carries the result of a hook execution.
type Response struct {
	// Allowed indicates whether the request is permitted.
	Allowed bool

	// Reason provides a human-readable denial reason when Allowed is false.
	Reason string

	// Patch holds the mutated object (only used by mutating hooks).
	Patch json.RawMessage
}

// ValidatingHook evaluates a resource request and approves or rejects it.
type ValidatingHook interface {
	// Name returns a unique identifier for this hook.
	Name() string

	// Handles reports whether this hook should be invoked for the given kind and operation.
	Handles(kind string, op Operation) bool

	// Validate evaluates the request. Return a non-nil error or a Response with Allowed=false to reject.
	Validate(ctx context.Context, req *Request) *Response
}

// MutatingHook can modify a resource before it is validated and persisted.
type MutatingHook interface {
	// Name returns a unique identifier for this hook.
	Name() string

	// Handles reports whether this hook should be invoked for the given kind and operation.
	Handles(kind string, op Operation) bool

	// Mutate may modify the object. Return the mutated object in Response.Patch.
	// If no mutation is needed, return Response with Allowed=true and nil Patch.
	Mutate(ctx context.Context, req *Request) *Response
}

// Chain runs an ordered sequence of mutating and then validating hooks.
type Chain struct {
	mutating   []MutatingHook
	validating []ValidatingHook
}

// NewChain creates an admission chain. Mutating hooks run first (in order),
// then validating hooks run on the final mutated object.
func NewChain(mutating []MutatingHook, validating []ValidatingHook) *Chain {
	return &Chain{
		mutating:   mutating,
		validating: validating,
	}
}

// ProductionHooks returns the admission policy shared by standalone and
// Kubernetes modes.
func ProductionHooks(st store.Store, policyEngine *opa.Engine, controllerUsernames ...string) ([]MutatingHook, []ValidatingHook) {
	return ProductionHooksWithDecisionAuthority(st, policyEngine, "", controllerUsernames...)
}

func ProductionHooksWithDecisionAuthority(st store.Store, policyEngine *opa.Engine, decisionAuthority string, controllerUsernames ...string) ([]MutatingHook, []ValidatingHook) {
	return ProductionHooksWithExecutionAuthority(st, policyEngine, decisionAuthority, nil, controllerUsernames...)
}

func ProductionHooksWithExecutionAuthority(st store.Store, policyEngine *opa.Engine, decisionAuthority string, executionWriters map[string]string, controllerUsernames ...string) ([]MutatingHook, []ValidatingHook) {
	approvalValidator := NewRuntimeApprovalValidator(controllerUsernames...).WithDecisionAuthority(decisionAuthority).WithExecutionWriters(executionWriters)
	if claimantReader, ok := st.(store.RuntimeApprovalClaimantReader); ok {
		approvalValidator.WithClaimantReader(claimantReader)
	}
	if terminationReader, ok := st.(store.RuntimeApprovalTerminationReader); ok {
		approvalValidator.WithTerminationReader(terminationReader)
	}
	controllerUsername := ""
	if len(controllerUsernames) > 0 {
		controllerUsername = controllerUsernames[0]
	}
	return []MutatingHook{
			NewLimitRangeDefaulter(st),
			NewAgentDefaulter(),
		}, []ValidatingHook{
			NewSecretRefValidator(),
			NewModelInvocationValidator(),
			NewToolValidator(),
			NewToolClassValidator(),
			NewToolGrantValidator(st),
			NewSignatureValidator(st),
			NewCognitiveAdmissionPolicyHook(st, policyEngine),
			NewQuotaEnforcer(st),
			NewAutonomyClassValidator(st),
			NewTrustProfileValidator(st),
			approvalValidator,
			NewAgentValidator(),
			NewAgentServiceValidator(),
			NewSolutionValidator(),
			NewWorkflowValidator(),
			NewKnowledgeBaseClassValidator(),
			NewKnowledgeBaseValidator(),
			NewKnowledgeBaseClaimValidator(),
			NewKnowledgeBaseIndexRunValidator(),
			NewKnowledgeBaseIndexScheduleValidator(st),
			NewApprovalDecisionValidator(),
			NewAutonomyReviewValidator(),
			NewIntentGatewayValidator(),
			NewAgentIdentityValidator(controllerUsername),
			NewExternalAgentValidator(),
			NewExternalAgentGrantValidator(),
		}
}

// AppendChain appends another chain's hooks to this chain, preserving order.
func (c *Chain) AppendChain(other *Chain) {
	if c == nil || other == nil {
		return
	}
	c.mutating = append(c.mutating, other.mutating...)
	c.validating = append(c.validating, other.validating...)
}

// Admit runs the full admission pipeline on the given request.
// It returns the (possibly mutated) object or an error if admission is denied.
func (c *Chain) Admit(ctx context.Context, req *Request) (json.RawMessage, error) {
	obj, err := c.Mutate(ctx, req)
	if err != nil {
		return nil, err
	}
	validateReq := *req
	validateReq.Object = obj
	if err := c.Validate(ctx, &validateReq); err != nil {
		return nil, err
	}
	return obj, nil
}

// Mutate runs the ordered mutating hooks and returns the resulting object.
func (c *Chain) Mutate(ctx context.Context, req *Request) (json.RawMessage, error) {
	obj := req.Object
	for _, hook := range c.mutating {
		if !hook.Handles(req.Kind, req.Operation) {
			continue
		}
		// Build per-hook request with the current state of the object.
		hookReq := &Request{
			Operation: req.Operation,
			Kind:      req.Kind,
			Namespace: req.Namespace,
			Name:      req.Name,
			Object:    obj,
			OldObject: req.OldObject,
			UserInfo:  req.UserInfo,
		}
		resp := hook.Mutate(ctx, hookReq)
		if resp == nil {
			continue
		}
		if !resp.Allowed {
			return nil, &DeniedError{Hook: hook.Name(), Reason: resp.Reason}
		}
		if resp.Patch != nil {
			obj = resp.Patch
		}
	}
	return obj, nil
}

// Validate runs the ordered validating hooks against the request object.
func (c *Chain) Validate(ctx context.Context, req *Request) error {
	for _, hook := range c.validating {
		if !hook.Handles(req.Kind, req.Operation) {
			continue
		}
		hookReq := &Request{
			Operation: req.Operation,
			Kind:      req.Kind,
			Namespace: req.Namespace,
			Name:      req.Name,
			Object:    req.Object,
			OldObject: req.OldObject,
			UserInfo:  req.UserInfo,
		}
		resp := hook.Validate(ctx, hookReq)
		if resp == nil {
			continue
		}
		if !resp.Allowed {
			return &DeniedError{Hook: hook.Name(), Reason: resp.Reason}
		}
	}

	return nil
}

// RegisterMutating appends a mutating hook to the chain.
func (c *Chain) RegisterMutating(hook MutatingHook) {
	c.mutating = append(c.mutating, hook)
}

// RegisterValidating appends a validating hook to the chain.
func (c *Chain) RegisterValidating(hook ValidatingHook) {
	c.validating = append(c.validating, hook)
}

// DeniedError is returned when admission is denied by a hook.
type DeniedError struct {
	Hook   string
	Reason string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("admission denied by %s: %s", e.Hook, e.Reason)
}
