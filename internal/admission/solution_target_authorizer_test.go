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

type recordingAccessReviewer struct {
	allow map[AccessAttributes]bool
	err   error
	calls []AccessAttributes
	users []UserInfo
}

func (r *recordingAccessReviewer) Allowed(_ context.Context, user UserInfo, attributes AccessAttributes) (bool, error) {
	r.calls = append(r.calls, attributes)
	r.users = append(r.users, user)
	if r.err != nil {
		return false, r.err
	}
	return r.allow[attributes], nil
}

func solutionRequest(t *testing.T, op Operation, user string, spec, old *v1alpha1.SolutionSpec) *Request {
	t.Helper()
	marshal := func(spec *v1alpha1.SolutionSpec) json.RawMessage {
		raw, err := json.Marshal(v1alpha1.Solution{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "s", Namespace: "team-a"},
			Spec:       *spec,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	request := &Request{Operation: op, Kind: "Solution", Namespace: "team-a", Name: "s", Object: marshal(spec), UserInfo: UserInfo{Username: user, Groups: []string{"builders"}}}
	if old != nil {
		request.OldObject = marshal(old)
	}
	return request
}

func targetSpec(kind, name string) *v1alpha1.SolutionSpec {
	return &v1alpha1.SolutionSpec{Goal: "do it", Target: &v1alpha1.SolutionTarget{Kind: kind, Ref: v1alpha1.LocalObjectReference{Name: name}}}
}

func attrs(resource, verb, name string) AccessAttributes {
	return AccessAttributes{Namespace: "team-a", Group: "kognition.dev", Resource: resource, Verb: verb, Name: name}
}

func TestSolutionTargetAuthorizerChecksTheAuthorAgainstTheTarget(t *testing.T) {
	service, err := json.Marshal(v1alpha1.AgentService{Spec: v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "analyst"}}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		target   *v1alpha1.SolutionSpec
		required []AccessAttributes
	}{
		{name: "agent", target: targetSpec("Agent", "analyst"), required: []AccessAttributes{attrs("agentjobs", "create", ""), attrs("agents", "invoke", "analyst")}},
		{name: "agent service", target: targetSpec("AgentService", "svc"), required: []AccessAttributes{attrs("agentjobs", "create", ""), attrs("agents", "invoke", "analyst")}},
		{name: "workflow", target: targetSpec("Workflow", "flow"), required: []AccessAttributes{attrs("workflowruns", "create", ""), attrs("workflows", "use", "flow")}},
	}
	for _, test := range tests {
		memory := newMemStore()
		if err := memory.Create(context.Background(), "AgentService", "team-a", "svc", service); err != nil {
			t.Fatal(err)
		}
		t.Run(test.name+" allowed with every permission", func(t *testing.T) {
			reviewer := &recordingAccessReviewer{allow: map[AccessAttributes]bool{}}
			for _, required := range test.required {
				reviewer.allow[required] = true
			}
			response := NewSolutionTargetAuthorizer(reviewer, memory).Validate(context.Background(), solutionRequest(t, OperationCreate, "alice", test.target, nil))
			if !response.Allowed {
				t.Fatalf("denied: %s", response.Reason)
			}
			if len(reviewer.calls) != len(test.required) {
				t.Fatalf("reviews = %v, want exactly %v", reviewer.calls, test.required)
			}
			for index, want := range test.required {
				if reviewer.calls[index] != want {
					t.Fatalf("review %d = %+v, want %+v", index, reviewer.calls[index], want)
				}
			}
			if reviewer.users[0].Username != "alice" || len(reviewer.users[0].Groups) != 1 {
				t.Fatalf("reviewed %+v, want the requesting user with groups", reviewer.users[0])
			}
		})
		for missing := range test.required {
			t.Run(test.name+" denied without "+test.required[missing].Verb+" "+test.required[missing].Resource, func(t *testing.T) {
				reviewer := &recordingAccessReviewer{allow: map[AccessAttributes]bool{}}
				for index, required := range test.required {
					reviewer.allow[required] = index != missing
				}
				response := NewSolutionTargetAuthorizer(reviewer, memory).Validate(context.Background(), solutionRequest(t, OperationCreate, "alice", test.target, nil))
				if response.Allowed {
					t.Fatal("a builder without the permission started work through a Solution")
				}
			})
		}
	}
}

func TestSolutionTargetAuthorizerFailsClosed(t *testing.T) {
	spec := targetSpec("Workflow", "flow")
	allowAll := func() *recordingAccessReviewer {
		return &recordingAccessReviewer{allow: map[AccessAttributes]bool{attrs("workflowruns", "create", ""): true, attrs("workflows", "use", "flow"): true}}
	}
	tests := []struct {
		name    string
		hook    *SolutionTargetAuthorizer
		request *Request
	}{
		{name: "review error", hook: NewSolutionTargetAuthorizer(&recordingAccessReviewer{err: errors.New("unavailable")}, nil), request: solutionRequest(t, OperationCreate, "alice", spec, nil)},
		{name: "no reviewer configured", hook: NewSolutionTargetAuthorizer(nil, nil), request: solutionRequest(t, OperationCreate, "alice", spec, nil)},
		{name: "no authenticated user", hook: NewSolutionTargetAuthorizer(allowAll(), nil), request: solutionRequest(t, OperationCreate, "", spec, nil)},
		{name: "anonymous user", hook: NewSolutionTargetAuthorizer(allowAll(), nil), request: solutionRequest(t, OperationCreate, "system:anonymous", spec, nil)},
		{name: "unresolvable AgentService", hook: NewSolutionTargetAuthorizer(allowAll(), newMemStore()), request: solutionRequest(t, OperationCreate, "alice", targetSpec("AgentService", "missing"), nil)},
		{name: "AgentService without a store", hook: NewSolutionTargetAuthorizer(allowAll(), nil), request: solutionRequest(t, OperationCreate, "alice", targetSpec("AgentService", "svc"), nil)},
		{name: "unknown target kind", hook: NewSolutionTargetAuthorizer(allowAll(), nil), request: solutionRequest(t, OperationCreate, "alice", targetSpec("Tool", "x"), nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if response := test.hook.Validate(context.Background(), test.request); response.Allowed {
				t.Fatal("Solution target accepted without a usable authorization decision")
			}
		})
	}
}

func TestSolutionTargetAuthorizerScope(t *testing.T) {
	hook := NewSolutionTargetAuthorizer(&recordingAccessReviewer{}, nil)
	for _, op := range []Operation{OperationCreate, OperationUpdate} {
		if !hook.Handles("Solution", op) {
			t.Fatalf("Solution %s is not handled", op)
		}
	}
	if hook.Handles("Solution", OperationDelete) || hook.Handles("Agent", OperationCreate) {
		t.Fatal("only Solution create and update may be handled")
	}

	t.Run("no target needs no review", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		if response := NewSolutionTargetAuthorizer(reviewer, nil).Validate(context.Background(), solutionRequest(t, OperationCreate, "alice", &v1alpha1.SolutionSpec{Goal: "plan"}, nil)); !response.Allowed || len(reviewer.calls) != 0 {
			t.Fatalf("response = %+v, reviews = %v", response, reviewer.calls)
		}
	})

	t.Run("an unchanged target and goal is not reviewed again", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		spec := targetSpec("Workflow", "flow")
		if response := NewSolutionTargetAuthorizer(reviewer, nil).Validate(context.Background(), solutionRequest(t, OperationUpdate, "system:serviceaccount:kognition-system:kognition-controller", spec, spec)); !response.Allowed || len(reviewer.calls) != 0 {
			t.Fatalf("a metadata-only update was reviewed: %+v %v", response, reviewer.calls)
		}
	})

	for _, change := range []struct {
		name string
		edit func(*v1alpha1.SolutionSpec)
	}{
		{name: "target name", edit: func(spec *v1alpha1.SolutionSpec) { spec.Target.Ref.Name = "other" }},
		{name: "target kind", edit: func(spec *v1alpha1.SolutionSpec) { spec.Target.Kind = "Agent" }},
		{name: "goal", edit: func(spec *v1alpha1.SolutionSpec) { spec.Goal = "something else" }},
	} {
		t.Run("a changed "+change.name+" is reviewed", func(t *testing.T) {
			reviewer := &recordingAccessReviewer{}
			before, after := targetSpec("Workflow", "flow"), targetSpec("Workflow", "flow")
			change.edit(after)
			if response := NewSolutionTargetAuthorizer(reviewer, nil).Validate(context.Background(), solutionRequest(t, OperationUpdate, "alice", after, before)); response.Allowed {
				t.Fatal("a changed target was accepted without permission")
			}
			if len(reviewer.calls) == 0 {
				t.Fatal("a changed target was not reviewed")
			}
		})
	}

	t.Run("adding a target to a plain solution is reviewed", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		if response := NewSolutionTargetAuthorizer(reviewer, nil).Validate(context.Background(), solutionRequest(t, OperationUpdate, "alice", targetSpec("Workflow", "flow"), &v1alpha1.SolutionSpec{Goal: "do it"})); response.Allowed {
			t.Fatal("a newly added target was accepted without permission")
		}
	})
}

func TestSolutionTargetAuthorizerExemptsOnlyConfiguredOperatorIdentities(t *testing.T) {
	reviewer := &recordingAccessReviewer{}
	const gateway = "system:serviceaccount:kognition-system:kognition-intent-gateway"
	hook := NewSolutionTargetAuthorizer(reviewer, nil, gateway, " ")
	if response := hook.Validate(context.Background(), solutionRequest(t, OperationCreate, gateway, targetSpec("Workflow", "flow"), nil)); !response.Allowed || len(reviewer.calls) != 0 {
		t.Fatalf("the operator-bound intent gateway was reviewed or denied: %+v", response)
	}
	if response := hook.Validate(context.Background(), solutionRequest(t, OperationCreate, "alice", targetSpec("Workflow", "flow"), nil)); response.Allowed {
		t.Fatal("a non-exempt user inherited the exemption")
	}
	if response := hook.Validate(context.Background(), solutionRequest(t, OperationCreate, "", targetSpec("Workflow", "flow"), nil)); response.Allowed {
		t.Fatal("a blank exemption entry exempted the empty user")
	}
}
