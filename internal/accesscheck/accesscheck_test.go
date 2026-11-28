// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package accesscheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestResolveKindAcceptsKindsAndPlurals(t *testing.T) {
	tests := []struct {
		input      string
		resource   string
		namespaced bool
	}{
		{input: "Workflow", resource: "workflows", namespaced: true},
		{input: "workflow", resource: "workflows", namespaced: true},
		{input: "workflows", resource: "workflows", namespaced: true},
		{input: "AgentJob", resource: "agentjobs", namespaced: true},
		{input: "ModelClass", resource: "modelclasses"},
		{input: "memoryclasses", resource: "memoryclasses"},
	}
	for _, test := range tests {
		resource, namespaced, err := ResolveResource(test.input)
		if err != nil || resource != test.resource || namespaced != test.namespaced {
			t.Errorf("ResolveResource(%q) = %q, %v, %v; want %q, %v", test.input, resource, namespaced, err, test.resource, test.namespaced)
		}
	}
}

func TestResolveKindRejectsWhatItCannotNameExactly(t *testing.T) {
	for _, input := range []string{"", "pods", "secret", "Work", "*", "workflows/status", "kognition.dev/workflows"} {
		if resource, _, err := ResolveResource(input); err == nil {
			t.Errorf("ResolveResource(%q) = %q, want an error rather than a guess", input, resource)
		}
	}
}

type reviewRecorder struct {
	created []authorizationv1.SelfSubjectAccessReview
}

func newClient(t *testing.T, allow func(authorizationv1.ResourceAttributes) (bool, bool, string, string), rules *authorizationv1.SubjectRulesReviewStatus, createErr error) (*fake.Clientset, *reviewRecorder) {
	t.Helper()
	client := fake.NewSimpleClientset()
	recorder := &reviewRecorder{}
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		recorder.created = append(recorder.created, *review.DeepCopy())
		if createErr != nil {
			return true, nil, createErr
		}
		allowed, denied, reason, evaluation := allow(*review.Spec.ResourceAttributes)
		review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, Denied: denied, Reason: reason, EvaluationError: evaluation}
		return true, review, nil
	})
	client.PrependReactor("create", "selfsubjectrulesreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
		if createErr != nil {
			return true, nil, createErr
		}
		if rules != nil {
			review.Status = *rules.DeepCopy()
		}
		return true, review, nil
	})
	return client, recorder
}

func TestCanIAsksTheClusterAboutTheCaller(t *testing.T) {
	client, recorder := newClient(t, func(attributes authorizationv1.ResourceAttributes) (bool, bool, string, string) {
		return attributes.Verb == "use" && attributes.Name == "flow", false, "RBAC: allowed by RoleBinding", ""
	}, nil, nil)

	decision, err := CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "use", Kind: "Workflow", Name: "flow", Namespace: "team-a"})
	if err != nil || !decision.Allowed {
		t.Fatalf("decision = %+v, err = %v", decision, err)
	}
	got := *recorder.created[0].Spec.ResourceAttributes
	want := authorizationv1.ResourceAttributes{Namespace: "team-a", Group: "kognition.dev", Resource: "workflows", Verb: "use", Name: "flow"}
	if got != want {
		t.Fatalf("review attributes = %+v, want %+v", got, want)
	}

	decision, err = CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "use", Kind: "Workflow", Name: "other", Namespace: "team-a"})
	if err != nil || decision.Allowed {
		t.Fatalf("a different workflow must not be allowed: %+v, %v", decision, err)
	}
}

func TestCanISendsTheSubresourceAndDropsTheNamespaceForClusterScopedKinds(t *testing.T) {
	client, recorder := newClient(t, func(authorizationv1.ResourceAttributes) (bool, bool, string, string) { return true, false, "", "" }, nil, nil)

	if _, err := CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "approve", Kind: "RuntimeApproval", Subresource: "decision", Name: "a", Namespace: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.created[0].Spec.ResourceAttributes; got.Subresource != "decision" || got.Resource != "runtimeapprovals" {
		t.Fatalf("attributes = %+v", got)
	}

	if _, err := CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "get", Kind: "ModelClass", Namespace: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if got := recorder.created[1].Spec.ResourceAttributes; got.Namespace != "" || got.Resource != "modelclasses" {
		t.Fatalf("a cluster-scoped kind must not carry a namespace: %+v", got)
	}
}

func TestCanIFailsClosedAndNeverReportsUncertaintyAsAnAllow(t *testing.T) {
	allowAll := func(authorizationv1.ResourceAttributes) (bool, bool, string, string) { return true, false, "", "" }
	t.Run("evaluation error even when allowed", func(t *testing.T) {
		client, _ := newClient(t, func(authorizationv1.ResourceAttributes) (bool, bool, string, string) {
			return true, false, "", "authorizer down"
		}, nil, nil)
		decision, err := CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "get", Kind: "Agent", Namespace: "n"})
		if err == nil || decision.Allowed {
			t.Fatalf("decision = %+v, err = %v; want an error and no allow", decision, err)
		}
	})
	t.Run("explicit denial overrides allow", func(t *testing.T) {
		client, _ := newClient(t, func(authorizationv1.ResourceAttributes) (bool, bool, string, string) { return true, true, "", "" }, nil, nil)
		decision, err := CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "get", Kind: "Agent", Namespace: "n"})
		if err != nil || decision.Allowed {
			t.Fatalf("decision = %+v, err = %v", decision, err)
		}
	})
	t.Run("API failure", func(t *testing.T) {
		client, _ := newClient(t, allowAll, nil, errors.New("connection refused"))
		if decision, err := CanI(context.Background(), client.AuthorizationV1(), Request{Verb: "get", Kind: "Agent", Namespace: "n"}); err == nil || decision.Allowed {
			t.Fatalf("decision = %+v, err = %v", decision, err)
		}
	})
	t.Run("invalid request", func(t *testing.T) {
		client, recorder := newClient(t, allowAll, nil, nil)
		for _, request := range []Request{{Kind: "Agent"}, {Verb: "get"}, {Verb: "get", Kind: "nope"}, {Verb: "get verbs", Kind: "Agent"}, {Verb: "*", Kind: "Agent"}} {
			if _, err := CanI(context.Background(), client.AuthorizationV1(), request); err == nil {
				t.Errorf("request %+v was accepted", request)
			}
		}
		if len(recorder.created) != 0 {
			t.Fatal("an invalid request reached the API server")
		}
	})
}

func TestRulesAreFilteredToKognitionAndSummarized(t *testing.T) {
	status := &authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: []authorizationv1.ResourceRule{
			{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflows"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflows"}, Verbs: []string{"use"}, ResourceNames: []string{"approved"}},
			{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflows", "agents"}, Verbs: []string{"watch", "get"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"*"}},
			{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
		},
		Incomplete:      true,
		EvaluationError: "webhook authorizer unavailable",
	}
	client, _ := newClient(t, nil, status, nil)

	set, err := Rules(context.Background(), client.AuthorizationV1(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	if !set.Incomplete || set.EvaluationError == "" || set.Namespace != "team-a" {
		t.Fatalf("an incomplete review must be reported as such: %+v", set)
	}
	for _, row := range set.Rows {
		if row.Resource == "secrets" || row.Resource == "deployments" {
			t.Fatalf("a non-Kognition rule leaked into the output: %+v", row)
		}
	}
	rows := map[string]Row{}
	for _, row := range set.Rows {
		rows[row.Resource+"|"+strings.Join(row.ResourceNames, ",")] = row
	}
	if got := strings.Join(rows["workflows|"].Verbs, ","); got != "get,list,watch" {
		t.Fatalf("workflows verbs = %q, want the sorted union get,list,watch", got)
	}
	if got := strings.Join(rows["workflows|approved"].Verbs, ","); got != "use" {
		t.Fatalf("named workflow verbs = %q, want use (a named rule must not merge into the unnamed one)", got)
	}
	if got := strings.Join(rows["agents|"].Verbs, ","); got != "get,watch" {
		t.Fatalf("agents verbs = %q", got)
	}
	if got := strings.Join(rows["*|"].Verbs, ","); got != "*" || !rows["*|"].Wildcard {
		t.Fatalf("a wildcard rule must be shown and flagged: %+v", rows["*|"])
	}
	for index := 1; index < len(set.Rows); index++ {
		if set.Rows[index-1].Resource > set.Rows[index].Resource {
			t.Fatalf("rows are not sorted: %+v", set.Rows)
		}
	}
}

func TestRulesReportAnAPIFailure(t *testing.T) {
	client, _ := newClient(t, nil, nil, errors.New("forbidden"))
	if _, err := Rules(context.Background(), client.AuthorizationV1(), "team-a"); err == nil {
		t.Fatal("an API failure was swallowed")
	}
	if _, err := Rules(context.Background(), client.AuthorizationV1(), ""); err == nil {
		t.Fatal("an empty namespace was accepted")
	}
}
