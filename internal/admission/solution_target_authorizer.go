// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const kognitionAPIGroup = "kognition.dev"

// AccessAttributes names one Kubernetes authorization question.
type AccessAttributes struct {
	Namespace string
	Group     string
	Resource  string
	Verb      string
	Name      string
}

// AccessReviewer answers whether a user may perform an action. Implementations
// must report an inability to decide as an error, never as an allow.
type AccessReviewer interface {
	Allowed(ctx context.Context, user UserInfo, attributes AccessAttributes) (bool, error)
}

// SolutionTargetAuthorizer authorizes the author of a Solution that selects an
// execution target. The Solution controller starts the target's run on the
// author's behalf, so the author must hold the permissions that starting it
// directly would need: create on the run kind plus invoke on the Agent or use on
// the Workflow. Without this check a role that may write Solutions but may not
// submit work could start any target in its namespace.
//
// Operator-bound identities (for example the intent gateway) are exempt by
// explicit configuration; they authenticate their own callers.
type SolutionTargetAuthorizer struct {
	reviewer AccessReviewer
	store    store.Store
	exempt   map[string]struct{}
}

// NewSolutionTargetAuthorizer creates the hook. The store resolves an
// AgentService target to its Agent and may be nil when no such target is used.
func NewSolutionTargetAuthorizer(reviewer AccessReviewer, st store.Store, exemptUsernames ...string) *SolutionTargetAuthorizer {
	exempt := make(map[string]struct{}, len(exemptUsernames))
	for _, username := range exemptUsernames {
		if username = strings.TrimSpace(username); username != "" {
			exempt[username] = struct{}{}
		}
	}
	return &SolutionTargetAuthorizer{reviewer: reviewer, store: st, exempt: exempt}
}

func (a *SolutionTargetAuthorizer) Name() string { return "solution-target-authorizer" }

func (a *SolutionTargetAuthorizer) Handles(kind string, op Operation) bool {
	return kind == "Solution" && (op == OperationCreate || op == OperationUpdate)
}

func (a *SolutionTargetAuthorizer) Validate(ctx context.Context, req *Request) *Response {
	var solution v1alpha1.Solution
	if err := json.Unmarshal(req.Object, &solution); err != nil {
		return solutionDenied("invalid solution JSON: %v", err)
	}
	if solution.Spec.Target == nil {
		return &Response{Allowed: true}
	}
	if req.Operation == OperationUpdate && len(req.OldObject) > 0 {
		var previous v1alpha1.Solution
		if err := json.Unmarshal(req.OldObject, &previous); err != nil {
			return solutionDenied("invalid previous solution JSON: %v", err)
		}
		if sameExecution(previous.Spec, solution.Spec) {
			return &Response{Allowed: true}
		}
	}
	if _, exempt := a.exempt[req.UserInfo.Username]; exempt {
		return &Response{Allowed: true}
	}
	if req.UserInfo.Username == "" || req.UserInfo.Username == "system:anonymous" {
		return solutionDenied("spec.target requires an authenticated user")
	}
	if a.reviewer == nil {
		return solutionDenied("spec.target authorization is unavailable")
	}

	namespace := req.Namespace
	target := solution.Spec.Target
	var required []AccessAttributes
	switch target.Kind {
	case "Agent":
		required = []AccessAttributes{
			{Namespace: namespace, Group: kognitionAPIGroup, Resource: "agentjobs", Verb: "create"},
			{Namespace: namespace, Group: kognitionAPIGroup, Resource: "agents", Verb: "invoke", Name: target.Ref.Name},
		}
	case "AgentService":
		agent, err := a.agentBehindService(ctx, namespace, target.Ref.Name)
		if err != nil {
			return solutionDenied("spec.target AgentService %q cannot be resolved: %v", target.Ref.Name, err)
		}
		required = []AccessAttributes{
			{Namespace: namespace, Group: kognitionAPIGroup, Resource: "agentjobs", Verb: "create"},
			{Namespace: namespace, Group: kognitionAPIGroup, Resource: "agents", Verb: "invoke", Name: agent},
		}
	case "Workflow":
		required = []AccessAttributes{
			{Namespace: namespace, Group: kognitionAPIGroup, Resource: "workflowruns", Verb: "create"},
			{Namespace: namespace, Group: kognitionAPIGroup, Resource: "workflows", Verb: "use", Name: target.Ref.Name},
		}
	default:
		return solutionDenied("spec.target.kind %q is not supported", target.Kind)
	}

	for _, attributes := range required {
		allowed, err := a.reviewer.Allowed(ctx, req.UserInfo, attributes)
		if err != nil {
			return solutionDenied("spec.target authorization is unavailable")
		}
		if !allowed {
			return solutionDenied("%s may not %s %s%s, which this Solution target starts on its behalf",
				req.UserInfo.Username, attributes.Verb, attributes.Resource, namedSuffix(attributes.Name))
		}
	}
	return &Response{Allowed: true}
}

func (a *SolutionTargetAuthorizer) agentBehindService(ctx context.Context, namespace, name string) (string, error) {
	if a.store == nil {
		return "", errors.New("no store is configured")
	}
	raw, err := a.store.Get(ctx, "AgentService", namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", errors.New("it must exist before a Solution can target it")
		}
		return "", err
	}
	var service v1alpha1.AgentService
	if err := json.Unmarshal(raw, &service); err != nil {
		return "", err
	}
	if service.Spec.AgentRef.Name == "" {
		return "", errors.New("it names no Agent")
	}
	return service.Spec.AgentRef.Name, nil
}

// sameExecution reports whether two specs start the same work with the same input.
func sameExecution(previous, current v1alpha1.SolutionSpec) bool {
	if previous.Goal != current.Goal {
		return false
	}
	if previous.Target == nil || current.Target == nil {
		return previous.Target == current.Target
	}
	return previous.Target.Kind == current.Target.Kind && previous.Target.Ref.Name == current.Target.Ref.Name
}

func namedSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " " + name
}

func solutionDenied(format string, args ...any) *Response {
	return &Response{Allowed: false, Reason: fmt.Sprintf(format, args...)}
}
