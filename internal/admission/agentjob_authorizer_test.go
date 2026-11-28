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

func agentJobRequest(t *testing.T, op Operation, user string, spec, old *v1alpha1.AgentJobSpec) *Request {
	t.Helper()
	marshal := func(spec *v1alpha1.AgentJobSpec) json.RawMessage {
		raw, err := json.Marshal(v1alpha1.AgentJob{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "job", Namespace: "team-a"},
			Spec:       *spec,
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	request := &Request{Operation: op, Kind: "AgentJob", Namespace: "team-a", Name: "job", Object: marshal(spec), UserInfo: UserInfo{Username: user, Groups: []string{"consumers"}}}
	if old != nil {
		request.OldObject = marshal(old)
	}
	return request
}

func jobByRef(name string) *v1alpha1.AgentJobSpec {
	return &v1alpha1.AgentJobSpec{AgentRef: &v1alpha1.LocalObjectReference{Name: name}, Input: "go"}
}

func jobInline() *v1alpha1.AgentJobSpec {
	return &v1alpha1.AgentJobSpec{AgentSpec: &v1alpha1.AgentSpec{}, Input: "go"}
}

func TestAgentJobAuthorizerChecksTheSubmitter(t *testing.T) {
	invoke := attrs("agents", "invoke", "analyst")
	author := attrs("agents", "create", "")
	tests := []struct {
		name     string
		spec     *v1alpha1.AgentJobSpec
		allow    []AccessAttributes
		wantOK   bool
		wantCall []AccessAttributes
	}{
		{name: "agentRef with invoke", spec: jobByRef("analyst"), allow: []AccessAttributes{invoke}, wantOK: true, wantCall: []AccessAttributes{invoke}},
		{name: "agentRef without invoke (consumer scoped to another agent)", spec: jobByRef("analyst"), wantCall: []AccessAttributes{invoke}},
		{name: "inline agentSpec with create agents", spec: jobInline(), allow: []AccessAttributes{author}, wantOK: true, wantCall: []AccessAttributes{author}},
		{name: "inline agentSpec without create agents (consumer only)", spec: jobInline(), allow: []AccessAttributes{invoke}, wantCall: []AccessAttributes{author}},
		{name: "both set needs both", spec: &v1alpha1.AgentJobSpec{AgentRef: &v1alpha1.LocalObjectReference{Name: "analyst"}, AgentSpec: &v1alpha1.AgentSpec{}}, allow: []AccessAttributes{invoke}, wantCall: []AccessAttributes{invoke, author}},
		{name: "both set and both allowed", spec: &v1alpha1.AgentJobSpec{AgentRef: &v1alpha1.LocalObjectReference{Name: "analyst"}, AgentSpec: &v1alpha1.AgentSpec{}}, allow: []AccessAttributes{invoke, author}, wantOK: true, wantCall: []AccessAttributes{invoke, author}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reviewer := &recordingAccessReviewer{allow: map[AccessAttributes]bool{}}
			for _, allowed := range test.allow {
				reviewer.allow[allowed] = true
			}
			response := NewAgentJobAuthorizer(reviewer).Validate(context.Background(), agentJobRequest(t, OperationCreate, "alice", test.spec, nil))
			if response.Allowed != test.wantOK {
				t.Fatalf("allowed = %v (%s), want %v", response.Allowed, response.Reason, test.wantOK)
			}
			if len(reviewer.calls) < 1 || reviewer.calls[0] != test.wantCall[0] || reviewer.users[0].Username != "alice" {
				t.Fatalf("reviews = %v by %v, want first %+v by alice", reviewer.calls, reviewer.users, test.wantCall[0])
			}
			if test.wantOK && len(reviewer.calls) != len(test.wantCall) {
				t.Fatalf("reviews = %v, want %v", reviewer.calls, test.wantCall)
			}
		})
	}
}

func TestAgentJobAuthorizerFailsClosed(t *testing.T) {
	allowAll := func() *recordingAccessReviewer {
		return &recordingAccessReviewer{allow: map[AccessAttributes]bool{attrs("agents", "invoke", "analyst"): true, attrs("agents", "create", ""): true}}
	}
	tests := []struct {
		name    string
		hook    *AgentJobAuthorizer
		request *Request
	}{
		{name: "review error", hook: NewAgentJobAuthorizer(&recordingAccessReviewer{err: errors.New("unavailable")}), request: agentJobRequest(t, OperationCreate, "alice", jobByRef("analyst"), nil)},
		{name: "no reviewer", hook: NewAgentJobAuthorizer(nil), request: agentJobRequest(t, OperationCreate, "alice", jobByRef("analyst"), nil)},
		{name: "no user", hook: NewAgentJobAuthorizer(allowAll()), request: agentJobRequest(t, OperationCreate, "", jobByRef("analyst"), nil)},
		{name: "anonymous", hook: NewAgentJobAuthorizer(allowAll()), request: agentJobRequest(t, OperationCreate, "system:anonymous", jobByRef("analyst"), nil)},
		{name: "invalid object", hook: NewAgentJobAuthorizer(allowAll()), request: &Request{Operation: OperationCreate, Kind: "AgentJob", Object: json.RawMessage("{"), UserInfo: UserInfo{Username: "alice"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if response := test.hook.Validate(context.Background(), test.request); response.Allowed {
				t.Fatal("AgentJob accepted without a usable authorization decision")
			}
		})
	}
}

func TestAgentJobAuthorizerScope(t *testing.T) {
	hook := NewAgentJobAuthorizer(&recordingAccessReviewer{})
	if !hook.Handles("AgentJob", OperationCreate) || !hook.Handles("AgentJob", OperationUpdate) {
		t.Fatal("AgentJob create and update must be handled")
	}
	if hook.Handles("AgentJob", OperationDelete) || hook.Handles("Solution", OperationCreate) {
		t.Fatal("only AgentJob create and update may be handled")
	}

	t.Run("neither agentRef nor agentSpec has nothing to authorize", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		if response := NewAgentJobAuthorizer(reviewer).Validate(context.Background(), agentJobRequest(t, OperationCreate, "alice", &v1alpha1.AgentJobSpec{Input: "x"}, nil)); !response.Allowed || len(reviewer.calls) != 0 {
			t.Fatalf("response = %+v, reviews = %v", response, reviewer.calls)
		}
	})

	t.Run("an unchanged update is not reviewed again", func(t *testing.T) {
		reviewer := &recordingAccessReviewer{}
		spec := jobByRef("analyst")
		if response := NewAgentJobAuthorizer(reviewer).Validate(context.Background(), agentJobRequest(t, OperationUpdate, "system:serviceaccount:kognition-system:kognition-controller", spec, spec)); !response.Allowed || len(reviewer.calls) != 0 {
			t.Fatalf("a finalizer-style update was reviewed: %+v %v", response, reviewer.calls)
		}
	})

	for _, change := range []struct {
		name string
		edit func(*v1alpha1.AgentJobSpec)
	}{
		{name: "agentRef", edit: func(spec *v1alpha1.AgentJobSpec) { spec.AgentRef.Name = "other" }},
		{name: "input", edit: func(spec *v1alpha1.AgentJobSpec) { spec.Input = "something else" }},
		{name: "revision", edit: func(spec *v1alpha1.AgentJobSpec) { revision := int64(2); spec.AgentRevision = &revision }},
		{name: "agentSpec added", edit: func(spec *v1alpha1.AgentJobSpec) { spec.AgentSpec = &v1alpha1.AgentSpec{} }},
	} {
		t.Run("a changed "+change.name+" is reviewed", func(t *testing.T) {
			reviewer := &recordingAccessReviewer{}
			before, after := jobByRef("analyst"), jobByRef("analyst")
			change.edit(after)
			if response := NewAgentJobAuthorizer(reviewer).Validate(context.Background(), agentJobRequest(t, OperationUpdate, "alice", after, before)); response.Allowed {
				t.Fatal("a changed job was accepted without permission")
			}
			if len(reviewer.calls) == 0 {
				t.Fatal("a changed job was not reviewed")
			}
		})
	}
}

func TestAgentJobAuthorizerExemptsOnlyConfiguredPlatformIdentities(t *testing.T) {
	const controller = "system:serviceaccount:kognition-system:kognition-controller"
	reviewer := &recordingAccessReviewer{}
	hook := NewAgentJobAuthorizer(reviewer, controller, " ")
	if response := hook.Validate(context.Background(), agentJobRequest(t, OperationCreate, controller, jobInline(), nil)); !response.Allowed || len(reviewer.calls) != 0 {
		t.Fatalf("the controller was reviewed or denied: %+v", response)
	}
	if response := hook.Validate(context.Background(), agentJobRequest(t, OperationCreate, "alice", jobInline(), nil)); response.Allowed {
		t.Fatal("a non-exempt user inherited the exemption")
	}
	if response := hook.Validate(context.Background(), agentJobRequest(t, OperationCreate, "", jobInline(), nil)); response.Allowed {
		t.Fatal("a blank exemption entry exempted the empty user")
	}
}
