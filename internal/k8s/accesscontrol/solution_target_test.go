// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package accesscontrol

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/accesscatalog"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/k8s/webhook"
)

// solutionAdmission runs the Solution target hook with the real SubjectAccessReview
// path, so RBAC on the live API server decides.
func solutionAdmission(t *testing.T, user, namespace, kind, name string) *admission.Response {
	t.Helper()
	raw, err := json.Marshal(api.Solution{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Solution"},
		ObjectMeta: api.ObjectMeta{Name: "s", Namespace: namespace},
		Spec:       api.SolutionSpec{Goal: "run it", Target: &api.SolutionTarget{Kind: kind, Ref: api.LocalObjectReference{Name: name}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook := admission.NewSolutionTargetAuthorizer(webhook.NewSubjectAccessReviewer(h.kube.AuthorizationV1().SubjectAccessReviews()), nil)
	return hook.Validate(context.Background(), &admission.Request{
		Operation: admission.OperationCreate, Kind: "Solution", Namespace: namespace, Name: "s", Object: raw,
		UserInfo: admission.UserInfo{Username: user},
	})
}

func waitForAuthorization(t *testing.T, user string, attrs authorizationv1.ResourceAttributes, want bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := h.review(context.Background(), user, attrs)
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s %s = %v, want %v", user, attrs.Verb, attrs.Resource, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSolutionTargetIsAuthorizedAgainstRealRBAC(t *testing.T) {
	builder, consumer := userFor("kognition-builder"), userFor("kognition-consumer")

	// The shipped builder may write Solutions but may not submit work, so it must
	// not start a target through one. The consumer holds exactly the run rights.
	for _, target := range []struct{ kind, name string }{{"Agent", "analyst"}, {"Workflow", "flow"}} {
		t.Run("builder cannot start a "+target.kind+" target", func(t *testing.T) {
			if response := solutionAdmission(t, builder, nsA, target.kind, target.name); response.Allowed {
				t.Fatal("a builder started work through a Solution target")
			}
		})
		t.Run("consumer may start a "+target.kind+" target in its namespace", func(t *testing.T) {
			if response := solutionAdmission(t, consumer, nsA, target.kind, target.name); !response.Allowed {
				t.Fatalf("denied: %s", response.Reason)
			}
		})
		t.Run("consumer may not start a "+target.kind+" target in another namespace", func(t *testing.T) {
			if response := solutionAdmission(t, consumer, nsB, target.kind, target.name); response.Allowed {
				t.Fatal("a binding in team-a reached team-b")
			}
		})
	}

	t.Run("resourceNames scopes the target to the workflow the user may use", func(t *testing.T) {
		ctx := context.Background()
		const user = "user-solution-scoped"
		if err := h.addUser(user); err != nil {
			t.Fatal(err)
		}
		role, err := h.kube.RbacV1().Roles(nsA).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "solution-scoped"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"workflowruns"}, Verbs: []string{"create"}},
				{APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"workflows"}, Verbs: []string{"use"}, ResourceNames: []string{"approved"}},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "solution-scoped"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		waitForAuthorization(t, user, authorizationv1.ResourceAttributes{Namespace: nsA, Group: accesscatalog.APIGroup, Resource: "workflows", Verb: "use", Name: "approved"}, true)

		if response := solutionAdmission(t, user, nsA, "Workflow", "approved"); !response.Allowed {
			t.Fatalf("the named workflow was denied: %s", response.Reason)
		}
		if response := solutionAdmission(t, user, nsA, "Workflow", "other"); response.Allowed {
			t.Fatal("a workflow the user may not use was started through a Solution")
		}
		if response := solutionAdmission(t, user, nsA, "Agent", "analyst"); response.Allowed {
			t.Fatal("an Agent target was accepted without create on agentjobs or invoke")
		}
	})

	t.Run("an authorizer that cannot decide never allows", func(t *testing.T) {
		raw, err := json.Marshal(api.Solution{Spec: api.SolutionSpec{Goal: "g", Target: &api.SolutionTarget{Kind: "Workflow", Ref: api.LocalObjectReference{Name: "flow"}}}})
		if err != nil {
			t.Fatal(err)
		}
		hook := admission.NewSolutionTargetAuthorizer(webhook.NewSubjectAccessReviewer(nil), nil)
		if response := hook.Validate(context.Background(), &admission.Request{Operation: admission.OperationCreate, Kind: "Solution", Namespace: nsA, Object: raw, UserInfo: admission.UserInfo{Username: consumer}}); response.Allowed {
			t.Fatal("a missing SubjectAccessReview client allowed a Solution target")
		}
	})
}
