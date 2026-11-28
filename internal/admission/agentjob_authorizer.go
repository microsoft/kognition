// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// AgentJobAuthorizer authorizes the submitter of an AgentJob. The AgentJob
// reconciler turns spec.agentSpec into a new Agent (authoring) and spec.agentRef
// into an AgentInstance (invocation) without asking who submitted the job, so
// `create` on agentjobs alone would let a consumer author an Agent or run an Agent
// outside a scoped `invoke` grant. The submitter must therefore hold the rights
// that doing either directly would need: `invoke` on the named Agent, and `create`
// on agents for an inline definition.
//
// Platform identities that create jobs after their own checks (the controller, for
// Solutions, Workflows and Evaluations) are exempt by explicit configuration.
type AgentJobAuthorizer struct {
	reviewer AccessReviewer
	exempt   map[string]struct{}
}

// NewAgentJobAuthorizer creates the hook.
func NewAgentJobAuthorizer(reviewer AccessReviewer, exemptUsernames ...string) *AgentJobAuthorizer {
	exempt := make(map[string]struct{}, len(exemptUsernames))
	for _, username := range exemptUsernames {
		if username = strings.TrimSpace(username); username != "" {
			exempt[username] = struct{}{}
		}
	}
	return &AgentJobAuthorizer{reviewer: reviewer, exempt: exempt}
}

func (a *AgentJobAuthorizer) Name() string { return "agentjob-authorizer" }

func (a *AgentJobAuthorizer) Handles(kind string, op Operation) bool {
	return kind == "AgentJob" && (op == OperationCreate || op == OperationUpdate)
}

func (a *AgentJobAuthorizer) Validate(ctx context.Context, req *Request) *Response {
	var job v1alpha1.AgentJob
	if err := json.Unmarshal(req.Object, &job); err != nil {
		return denyAgentJob("invalid AgentJob JSON: %v", err)
	}
	refName := ""
	if job.Spec.AgentRef != nil {
		refName = job.Spec.AgentRef.Name
	}
	if refName == "" && job.Spec.AgentSpec == nil {
		return &Response{Allowed: true}
	}
	if req.Operation == OperationUpdate && len(req.OldObject) > 0 {
		var previous v1alpha1.AgentJob
		if err := json.Unmarshal(req.OldObject, &previous); err != nil {
			return denyAgentJob("invalid previous AgentJob JSON: %v", err)
		}
		if sameJobExecution(previous.Spec, job.Spec) {
			return &Response{Allowed: true}
		}
	}
	if _, exempt := a.exempt[req.UserInfo.Username]; exempt {
		return &Response{Allowed: true}
	}
	if req.UserInfo.Username == "" || req.UserInfo.Username == "system:anonymous" {
		return denyAgentJob("an AgentJob requires an authenticated user")
	}
	if a.reviewer == nil {
		return denyAgentJob("AgentJob authorization is unavailable")
	}

	var required []AccessAttributes
	if refName != "" {
		required = append(required, AccessAttributes{Namespace: req.Namespace, Group: kognitionAPIGroup, Resource: "agents", Verb: "invoke", Name: refName})
	}
	if job.Spec.AgentSpec != nil {
		required = append(required, AccessAttributes{Namespace: req.Namespace, Group: kognitionAPIGroup, Resource: "agents", Verb: "create"})
	}
	for _, attributes := range required {
		allowed, err := a.reviewer.Allowed(ctx, req.UserInfo, attributes)
		if err != nil {
			return denyAgentJob("AgentJob authorization is unavailable")
		}
		if !allowed {
			return denyAgentJob("%s may not %s agents%s, which this AgentJob does on its behalf",
				req.UserInfo.Username, attributes.Verb, namedSuffix(attributes.Name))
		}
	}
	return &Response{Allowed: true}
}

// sameJobExecution reports whether two specs run the same agent with the same input.
func sameJobExecution(previous, current v1alpha1.AgentJobSpec) bool {
	previousRef, currentRef := "", ""
	if previous.AgentRef != nil {
		previousRef = previous.AgentRef.Name
	}
	if current.AgentRef != nil {
		currentRef = current.AgentRef.Name
	}
	return previousRef == currentRef && previous.Input == current.Input &&
		reflect.DeepEqual(previous.AgentRevision, current.AgentRevision) &&
		reflect.DeepEqual(previous.AgentSpec, current.AgentSpec)
}

func denyAgentJob(format string, args ...any) *Response {
	return &Response{Allowed: false, Reason: fmt.Sprintf(format, args...)}
}
