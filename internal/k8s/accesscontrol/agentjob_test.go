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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/accesscatalog"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/k8s/webhook"
)

func agentJobAdmission(t *testing.T, user, namespace string, spec api.AgentJobSpec) *admission.Response {
	t.Helper()
	raw, err := json.Marshal(api.AgentJob{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentJob"},
		ObjectMeta: api.ObjectMeta{Name: "job", Namespace: namespace},
		Spec:       spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	hook := admission.NewAgentJobAuthorizer(webhook.NewSubjectAccessReviewer(h.kube.AuthorizationV1().SubjectAccessReviews()))
	return hook.Validate(context.Background(), &admission.Request{
		Operation: admission.OperationCreate, Kind: "AgentJob", Namespace: namespace, Name: "job", Object: raw,
		UserInfo: admission.UserInfo{Username: user},
	})
}

func TestAgentJobIsAuthorizedAgainstRealRBAC(t *testing.T) {
	ctx := context.Background()
	consumer, builder := userFor("kognition-consumer"), userFor("kognition-builder")
	byRef := api.AgentJobSpec{AgentRef: &api.LocalObjectReference{Name: "analyst"}, Input: "go"}
	inline := api.AgentJobSpec{AgentSpec: &api.AgentSpec{}, Input: "go"}

	t.Run("a consumer runs an Agent it may invoke", func(t *testing.T) {
		if response := agentJobAdmission(t, consumer, nsA, byRef); !response.Allowed {
			t.Fatalf("denied: %s", response.Reason)
		}
	})
	t.Run("a consumer cannot author an Agent through an inline spec", func(t *testing.T) {
		if response := agentJobAdmission(t, consumer, nsA, inline); response.Allowed {
			t.Fatal("a consumer authored an Agent through an AgentJob")
		}
	})
	t.Run("a consumer's binding does not reach another namespace", func(t *testing.T) {
		if response := agentJobAdmission(t, consumer, nsB, byRef); response.Allowed {
			t.Fatal("a binding in team-a reached team-b")
		}
	})
	t.Run("a builder, which may not invoke, cannot run an Agent through a job", func(t *testing.T) {
		if response := agentJobAdmission(t, builder, nsA, byRef); response.Allowed {
			t.Fatal("a builder ran an Agent it may not invoke")
		}
	})
	t.Run("a builder may define the inline Agent it submits", func(t *testing.T) {
		if response := agentJobAdmission(t, builder, nsA, inline); !response.Allowed {
			t.Fatalf("denied: %s", response.Reason)
		}
	})

	t.Run("a scoped invoke grant limits which Agent a job may run", func(t *testing.T) {
		const user = "user-job-scoped"
		if err := h.addUser(user); err != nil {
			t.Fatal(err)
		}
		role, err := h.kube.RbacV1().Roles(nsA).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "job-scoped"},
			Rules: []rbacv1.PolicyRule{
				{APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"agentjobs"}, Verbs: []string{"create"}},
				{APIGroups: []string{accesscatalog.APIGroup}, Resources: []string{"agents"}, Verbs: []string{"invoke"}, ResourceNames: []string{"approved"}},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "job-scoped"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: user}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		waitForAuthorization(t, user, authorizationv1.ResourceAttributes{Namespace: nsA, Group: accesscatalog.APIGroup, Resource: "agents", Verb: "invoke", Name: "approved"}, true)

		approved := api.AgentJobSpec{AgentRef: &api.LocalObjectReference{Name: "approved"}, Input: "go"}
		if response := agentJobAdmission(t, user, nsA, approved); !response.Allowed {
			t.Fatalf("the Agent the grant names was denied: %s", response.Reason)
		}
		if response := agentJobAdmission(t, user, nsA, byRef); response.Allowed {
			t.Fatal("an Agent outside the scoped invoke grant was run through a job")
		}
	})
}

// access-admin may revoke any RoleBinding in its namespace but may neither
// create nor edit its way into a binding that grants more than it may delegate.
// This characterizes the authority the plan documents (G10).
func TestAccessAdminRevocationAuthorityIsBoundedToRevocation(t *testing.T) {
	ctx := context.Background()
	admin := h.users[userFor("kognition-access-admin")]
	const privileged = "test-privileged-revocation"
	if _, err := h.kube.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: privileged},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding := func(name string) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsA},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: privileged},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "platform-component"}},
		}
	}

	t.Run("cannot create a binding for a role outside its allowlist", func(t *testing.T) {
		if _, err := admin.kube.RbacV1().RoleBindings(nsA).Create(ctx, binding("forged"), metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
			t.Fatalf("create err = %v, want Forbidden", err)
		}
	})

	if _, err := h.kube.RbacV1().RoleBindings(nsA).Create(ctx, binding("platform-owned"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Run("cannot add itself to an existing binding of such a role", func(t *testing.T) {
		patch := []byte(`{"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"` + userFor("kognition-access-admin") + `"}]}`)
		if _, err := admin.kube.RbacV1().RoleBindings(nsA).Patch(ctx, "platform-owned", types.StrategicMergePatchType, patch, metav1.PatchOptions{}); !apierrors.IsForbidden(err) {
			t.Fatalf("patch err = %v, want Forbidden", err)
		}
	})
	t.Run("can revoke it, which is the documented namespace-wide revocation authority", func(t *testing.T) {
		if err := admin.kube.RbacV1().RoleBindings(nsA).Delete(ctx, "platform-owned", metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete err = %v; the plan documents this authority, update G10 if it changed", err)
		}
	})
	t.Run("cannot restore what it revoked", func(t *testing.T) {
		if _, err := admin.kube.RbacV1().RoleBindings(nsA).Create(ctx, binding("platform-owned"), metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
			t.Fatalf("recreate err = %v, want Forbidden", err)
		}
	})
	t.Run("cannot delete in another namespace", func(t *testing.T) {
		other := binding("other-namespace")
		other.Namespace = nsB
		if _, err := h.kube.RbacV1().RoleBindings(nsB).Create(ctx, other, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := admin.kube.RbacV1().RoleBindings(nsB).Delete(ctx, "other-namespace", metav1.DeleteOptions{}); !apierrors.IsForbidden(err) {
			t.Fatalf("delete err = %v, want Forbidden", err)
		}
	})
}
