// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func workflowRunRequest(t *testing.T, op Operation, user string, spec, old *v1alpha1.WorkflowRunSpec) *Request {
	t.Helper()
	marshal := func(spec *v1alpha1.WorkflowRunSpec) json.RawMessage {
		raw, err := json.Marshal(v1alpha1.WorkflowRun{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "WorkflowRun"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "run", Namespace: "team-a"},
			Spec:       *spec,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	request := &Request{Operation: op, Kind: "WorkflowRun", Namespace: "team-a", Name: "run", Object: marshal(spec), UserInfo: UserInfo{Username: user, Groups: []string{"consumers"}}}
	if old != nil {
		request.OldObject = marshal(old)
	}
	return request
}

func runOf(workflow string) *v1alpha1.WorkflowRunSpec {
	return &v1alpha1.WorkflowRunSpec{WorkflowRef: v1alpha1.LocalObjectReference{Name: workflow}, Input: "go"}
}

func TestWorkflowRunAuthorizerRequiresUseOnTheNamedWorkflow(t *testing.T) {
	use := attrs("workflows", "use", "flow")
	tests := []struct {
		name   string
		allow  []AccessAttributes
		wantOK bool
	}{
		{name: "use on the workflow", allow: []AccessAttributes{use}, wantOK: true},
		{name: "create alone is not enough (consumer scoped to another workflow)"},
		{name: "use on another workflow", allow: []AccessAttributes{attrs("workflows", "use", "other")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reviewer := &recordingAccessReviewer{allow: map[AccessAttributes]bool{}}
			for _, allowed := range test.allow {
				reviewer.allow[allowed] = true
			}
			response := NewWorkflowRunAuthorizer(reviewer).Validate(context.Background(), workflowRunRequest(t, OperationCreate, "alice", runOf("flow"), nil))
			if response.Allowed != test.wantOK {
				t.Fatalf("allowed = %v (%s), want %v", response.Allowed, response.Reason, test.wantOK)
			}
			if len(reviewer.calls) != 1 || reviewer.calls[0] != use || reviewer.users[0].Username != "alice" {
				t.Fatalf("reviews = %v by %v, want exactly %+v by alice", reviewer.calls, reviewer.users, use)
			}
		})
	}
}

func TestWorkflowRunAuthorizerFailsClosed(t *testing.T) {
	allowAll := func() *recordingAccessReviewer {
		return &recordingAccessReviewer{allow: map[AccessAttributes]bool{attrs("workflows", "use", "flow"): true}}
	}
	tests := []struct {
		name    string
		hook    *WorkflowRunAuthorizer
		request *Request
	}{
		{name: "review error", hook: NewWorkflowRunAuthorizer(&recordingAccessReviewer{err: errors.New("unavailable")}), request: workflowRunRequest(t, OperationCreate, "alice", runOf("flow"), nil)},
		{name: "no reviewer", hook: NewWorkflowRunAuthorizer(nil), request: workflowRunRequest(t, OperationCreate, "alice", runOf("flow"), nil)},
		{name: "no user", hook: NewWorkflowRunAuthorizer(allowAll()), request: workflowRunRequest(t, OperationCreate, "", runOf("flow"), nil)},
		{name: "anonymous", hook: NewWorkflowRunAuthorizer(allowAll()), request: workflowRunRequest(t, OperationCreate, "system:anonymous", runOf("flow"), nil)},
		{name: "invalid object", hook: NewWorkflowRunAuthorizer(allowAll()), request: &Request{Operation: OperationCreate, Kind: "WorkflowRun", Object: json.RawMessage("{"), UserInfo: UserInfo{Username: "alice"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if response := test.hook.Validate(context.Background(), test.request); response.Allowed {
				t.Fatal("WorkflowRun accepted without a usable authorization decision")
			}
		})
	}
}

func TestWorkflowRunAuthorizerScope(t *testing.T) {
	hook := NewWorkflowRunAuthorizer(&recordingAccessReviewer{})
	if !hook.Handles("WorkflowRun", OperationCreate) || !hook.Handles("WorkflowRun", OperationUpdate) {
		t.Fatal("WorkflowRun create and update must be handled")
	}
	if hook.Handles("WorkflowRun", OperationDelete) || hook.Handles("Workflow", OperationCreate) {
		t.Fatal("only WorkflowRun create and update may be handled")
	}

	t.Run("a run naming no workflow has nothing to authorize", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		if response := NewWorkflowRunAuthorizer(reviewer).Validate(context.Background(), workflowRunRequest(t, OperationCreate, "alice", &v1alpha1.WorkflowRunSpec{Input: "x"}, nil)); !response.Allowed || len(reviewer.calls) != 0 {
			t.Fatalf("response = %+v, reviews = %v", response, reviewer.calls)
		}
	})

	t.Run("an unchanged update is not reviewed again", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		spec := runOf("flow")
		if response := NewWorkflowRunAuthorizer(reviewer).Validate(context.Background(), workflowRunRequest(t, OperationUpdate, "system:serviceaccount:kognition-system:kognition-controller", spec, spec)); !response.Allowed || len(reviewer.calls) != 0 {
			t.Fatalf("a finalizer-style update was reviewed: %+v %v", response, reviewer.calls)
		}
	})

	for _, change := range []struct {
		name string
		edit func(*v1alpha1.WorkflowRunSpec)
	}{
		{name: "workflow", edit: func(spec *v1alpha1.WorkflowRunSpec) { spec.WorkflowRef.Name = "other" }},
		{name: "input", edit: func(spec *v1alpha1.WorkflowRunSpec) { spec.Input = "something else" }},
		{name: "parameters", edit: func(spec *v1alpha1.WorkflowRunSpec) { spec.Parameters = map[string]string{"k": "v"} }},
	} {
		t.Run("a changed "+change.name+" is reviewed", func(t *testing.T) {
			reviewer := &recordingAccessReviewer{}
			before, after := runOf("flow"), runOf("flow")
			change.edit(after)
			if response := NewWorkflowRunAuthorizer(reviewer).Validate(context.Background(), workflowRunRequest(t, OperationUpdate, "alice", after, before)); response.Allowed {
				t.Fatal("a changed run was accepted without permission")
			}
			if len(reviewer.calls) == 0 {
				t.Fatal("a changed run was not reviewed")
			}
		})
	}
}

func TestWorkflowRunAuthorizerExemptsOnlyConfiguredPlatformIdentities(t *testing.T) {
	const controller = "system:serviceaccount:kognition-system:kognition-controller"
	reviewer := &recordingAccessReviewer{}
	hook := NewWorkflowRunAuthorizer(reviewer, controller, " ")
	if response := hook.Validate(context.Background(), workflowRunRequest(t, OperationCreate, controller, runOf("flow"), nil)); !response.Allowed || len(reviewer.calls) != 0 {
		t.Fatalf("the controller was reviewed or denied: %+v", response)
	}
	if response := hook.Validate(context.Background(), workflowRunRequest(t, OperationCreate, "alice", runOf("flow"), nil)); response.Allowed {
		t.Fatal("a non-exempt user inherited the exemption")
	}
	if response := hook.Validate(context.Background(), workflowRunRequest(t, OperationCreate, "", runOf("flow"), nil)); response.Allowed {
		t.Fatal("a blank exemption entry exempted the empty user")
	}
}

func TestSameRunExecutionIgnoresNilAndEmptyParameters(t *testing.T) {
	nilParameters := v1alpha1.WorkflowRunSpec{WorkflowRef: v1alpha1.LocalObjectReference{Name: "f"}}
	emptyParameters := v1alpha1.WorkflowRunSpec{WorkflowRef: v1alpha1.LocalObjectReference{Name: "f"}, Parameters: map[string]string{}}
	if !sameRunExecution(nilParameters, emptyParameters) {
		t.Fatal("nil and empty parameters were treated as a change")
	}
}
