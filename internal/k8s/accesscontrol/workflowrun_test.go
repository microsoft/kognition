// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package accesscontrol

import (
	"context"
	"encoding/json"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/accesscatalog"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/k8s/webhook"
)

func workflowRunAdmission(t *testing.T, user, namespace, workflow string) *admission.Response {
	t.Helper()
	raw, err := json.Marshal(api.WorkflowRun{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "WorkflowRun"},
		ObjectMeta: api.ObjectMeta{Name: "run", Namespace: namespace},
		Spec:       api.WorkflowRunSpec{WorkflowRef: api.LocalObjectReference{Name: workflow}, Input: "go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hook := admission.NewWorkflowRunAuthorizer(webhook.NewSubjectAccessReviewer(h.kube.AuthorizationV1().SubjectAccessReviews()))
	return hook.Validate(context.Background(), &admission.Request{
		Operation: admission.OperationCreate, Kind: "WorkflowRun", Namespace: namespace, Name: "run", Object: raw,
		UserInfo: admission.UserInfo{Username: user},
	})
}

// `create` on workflowruns is namespace-wide, so the per-workflow `use` grant only
// bounds an ordinary run if admission checks it (T-0467, plan G7).
func TestOrdinaryWorkflowRunIsAuthorizedAgainstRealRBAC(t *testing.T) {
	ctx := context.Background()
	consumer, builder := userFor("kognition-consumer"), userFor("kognition-builder")

	t.Run("a consumer may run a Workflow it may use", func(t *testing.T) {
		if response := workflowRunAdmission(t, consumer, nsA, "flow"); !response.Allowed {
			t.Fatalf("denied: %s", response.Reason)
		}
	})
	t.Run("a consumer's binding does not reach another namespace", func(t *testing.T) {
		if response := workflowRunAdmission(t, consumer, nsB, "flow"); response.Allowed {
			t.Fatal("a binding in team-a reached team-b")
		}
	})
	t.Run("a builder, which holds no use, cannot start a run", func(t *testing.T) {
		if response := workflowRunAdmission(t, builder, nsA, "flow"); response.Allowed {
			t.Fatal("a principal without use started a Workflow")
		}
	})

	t.Run("create alone does not let a principal start any Workflow", func(t *testing.T) {
		const user = "user-run-scoped"
		if err := h.addUser(user); err != nil {
			t.Fatal(err)
		}
		role, err := h.kube.RbacV1().Roles(nsA).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "run-scoped"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"workflowruns"}, Verbs: []string{"create"}},
				{APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"workflows"}, Verbs: []string{"use"}, ResourceNames: []string{"approved"}},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "run-scoped"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		waitForAuthorization(t, user, authorizationv1.ResourceAttributes{Namespace: nsA, Group: accesscatalog.APIGroup, Resource: "workflows", Verb: "use", Name: "approved"}, true)

		if response := workflowRunAdmission(t, user, nsA, "approved"); !response.Allowed {
			t.Fatalf("the named workflow was denied: %s", response.Reason)
		}
		if response := workflowRunAdmission(t, user, nsA, "other"); response.Allowed {
			t.Fatal("an ordinary run started a Workflow outside the scoped use grant")
		}
	})
}
