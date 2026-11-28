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

// WorkflowRunAuthorizer authorizes the submitter of a WorkflowRun with verb `use`
// on the Workflow it names. `create` on workflowruns is namespace-wide because
// RBAC cannot scope a top-level create to a name, so without this check the `use`
// grant, which can be limited with resourceNames, bounds only the protected
// submission path and not an ordinary run.
//
// The protected submission path (ADR-0029) performs its own, stricter check on
// the exact Workflow revision; this hook runs for every submission and does not
// replace it. Platform identities that create runs after their own checks (the
// controller, for Solution targets) are exempt by explicit configuration.
type WorkflowRunAuthorizer struct {
	reviewer AccessReviewer
	exempt   map[string]struct{}
}

// NewWorkflowRunAuthorizer creates the hook.
func NewWorkflowRunAuthorizer(reviewer AccessReviewer, exemptUsernames ...string) *WorkflowRunAuthorizer {
	exempt := make(map[string]struct{}, len(exemptUsernames))
	for _, username := range exemptUsernames {
		if username = strings.TrimSpace(username); username != "" {
			exempt[username] = struct{}{}
		}
	}
	return &WorkflowRunAuthorizer{reviewer: reviewer, exempt: exempt}
}

func (a *WorkflowRunAuthorizer) Name() string { return "workflowrun-authorizer" }

func (a *WorkflowRunAuthorizer) Handles(kind string, op Operation) bool {
	return kind == "WorkflowRun" && (op == OperationCreate || op == OperationUpdate)
}

func (a *WorkflowRunAuthorizer) Validate(ctx context.Context, req *Request) *Response {
	var run v1alpha1.WorkflowRun
	if err := json.Unmarshal(req.Object, &run); err != nil {
		return denyWorkflowRun("invalid WorkflowRun JSON: %v", err)
	}
	workflow := run.Spec.WorkflowRef.Name
	if workflow == "" {
		return &Response{Allowed: true}
	}
	if req.Operation == OperationUpdate && len(req.OldObject) > 0 {
		var previous v1alpha1.WorkflowRun
		if err := json.Unmarshal(req.OldObject, &previous); err != nil {
			return denyWorkflowRun("invalid previous WorkflowRun JSON: %v", err)
		}
		if sameRunExecution(previous.Spec, run.Spec) {
			return &Response{Allowed: true}
		}
	}
	if _, exempt := a.exempt[req.UserInfo.Username]; exempt {
		return &Response{Allowed: true}
	}
	if req.UserInfo.Username == "" || req.UserInfo.Username == "system:anonymous" {
		return denyWorkflowRun("a WorkflowRun requires an authenticated user")
	}
	if a.reviewer == nil {
		return denyWorkflowRun("WorkflowRun authorization is unavailable")
	}
	attributes := AccessAttributes{Namespace: req.Namespace, Group: kognitionAPIGroup, Resource: "workflows", Verb: "use", Name: workflow}
	allowed, err := a.reviewer.Allowed(ctx, req.UserInfo, attributes)
	if err != nil {
		return denyWorkflowRun("WorkflowRun authorization is unavailable")
	}
	if !allowed {
		return denyWorkflowRun("%s may not use workflow %s, which this WorkflowRun starts", req.UserInfo.Username, workflow)
	}
	return &Response{Allowed: true}
}

// sameRunExecution reports whether two specs start the same workflow with the same
// inputs; nil and empty parameters are the same.
func sameRunExecution(previous, current v1alpha1.WorkflowRunSpec) bool {
	if previous.WorkflowRef.Name != current.WorkflowRef.Name || previous.Input != current.Input {
		return false
	}
	if len(previous.Parameters) == 0 && len(current.Parameters) == 0 {
		return true
	}
	return reflect.DeepEqual(previous.Parameters, current.Parameters)
}

func denyWorkflowRun(format string, args ...any) *Response {
	return &Response{Allowed: false, Reason: fmt.Sprintf(format, args...)}
}
