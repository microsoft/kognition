// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/observability"
)

func TestControllerRBACAuthorization(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	documents := renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant", "--set", "k8sController.agentNamespace=runtime",
		"--set", "runtimeAuth.existingSecret=source-runtime", "--set", "knowledgeGateway.enabled=true",
		"--set", "knowledgeGateway.tls.secretName=source-provider", "--set", "knowledgeGateway.runtimeTLS.secretName=source-gateway",
		"--set", "knowledgeGateway.provider.address=provider:9443", "--set", "knowledgeGateway.provider.serverName=provider",
		"--set", "knowledgeGateway.provider.peerURI=spiffe://kognition/provider", "--set", "knowledgeGateway.runtimeTLS.serverName=gateway",
		"--set", "knowledgeGateway.provider.kubernetesApiEgressCIDRs[0]=10.96.0.1/32")
	environment := &envtest.Environment{}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Errorf("stop envtest: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, eventsv1.AddToScheme, rbacv1.AddToScheme, authorizationv1.AddToScheme} {
		if err := register(scheme); err != nil {
			test.Fatal(err)
		}
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			test.Fatal(err)
		}
	}
	controllerUser := "system:serviceaccount:control:security-test-kognition-controller"
	controllerRoles := 0
	for _, document := range documents {
		var metadata metav1.TypeMeta
		if err := json.Unmarshal(document.Raw, &metadata); err != nil {
			test.Fatal(err)
		}
		var object client.Object
		switch metadata.Kind {
		case "ClusterRole":
			object = &rbacv1.ClusterRole{}
		case "ClusterRoleBinding":
			object = &rbacv1.ClusterRoleBinding{}
		case "Role":
			object = &rbacv1.Role{}
		case "RoleBinding":
			object = &rbacv1.RoleBinding{}
		default:
			continue
		}
		if err := json.Unmarshal(document.Raw, object); err != nil {
			test.Fatal(err)
		}
		if err := admin.Create(ctx, object); err != nil {
			test.Fatal(err)
		}
		if role, ok := object.(*rbacv1.Role); ok && role.Labels["app.kubernetes.io/component"] == "k8s-controller" {
			binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name + "-fixture", Namespace: role.Namespace},
				RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "security-test-kognition-controller", Namespace: "control"}}}
			if err := admin.Create(ctx, binding); err != nil {
				test.Fatal(err)
			}
			controllerRoles++
		}
	}
	if controllerRoles != 4 {
		test.Fatalf("expected installation, quota, tenant and runtime Roles, got %d", controllerRoles)
	}
	for _, user := range []string{controllerUser, "system:serviceaccount:control:security-test-kognition"} {
		for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
			for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
				review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User:               user,
					Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
					ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: "events.k8s.io", Resource: "events", Verb: verb},
				}}
				if err := admin.Create(ctx, review); err != nil {
					test.Fatal(err)
				}
				allowed := user == controllerUser && (namespace == "control" || namespace == "tenant" || namespace == "runtime") && (verb == "create" || verb == "patch")
				if review.Status.Allowed != allowed {
					test.Errorf("Event %s by %s in %q: allowed=%t, want %t", verb, user, namespace, review.Status.Allowed, allowed)
				}
			}
		}
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
		for _, permission := range []struct{ group, resource, verb string }{
			{"", "secrets", "get"}, {"", "pods", "create"},
			{"apps", "deployments", "create"}, {"batch", "jobs", "create"},
			{"", "services", "create"}, {"networking.k8s.io", "networkpolicies", "create"},
		} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               "system:serviceaccount:control:security-test-kognition",
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: permission.group, Resource: permission.resource, Verb: permission.verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			if review.Status.Allowed {
				test.Errorf("shared platform ServiceAccount inherits controller permission: %s %s/%s in %q", permission.verb, permission.group, permission.resource, namespace)
			}
		}
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
		for _, verb := range []string{"create", "get", "list", "update", "patch", "delete", "watch"} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               "system:serviceaccount:control:security-test-kognition",
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Resource: "configmaps", Verb: verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			allowed := namespace == "control" && (verb == "create" || verb == "get" || verb == "list")
			if review.Status.Allowed != allowed {
				test.Errorf("memory audit %s in %q: allowed=%t, want %t", verb, namespace, review.Status.Allowed, allowed)
			}
		}
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
		for _, permission := range []struct{ group, resource, verb string }{
			{"", "secrets", "get"}, {"", "secrets", "create"}, {"", "secrets", "update"}, {"", "secrets", "patch"}, {"", "secrets", "delete"},
			{"", "secrets", "list"}, {"", "secrets", "watch"},
			{"", "pods", "create"}, {"", "pods", "update"}, {"", "pods", "patch"}, {"", "pods", "delete"},
			{"", "services", "create"}, {"", "services", "update"}, {"", "services", "patch"}, {"", "services", "delete"},
			{"apps", "deployments", "create"}, {"apps", "deployments", "update"}, {"apps", "deployments", "patch"}, {"apps", "deployments", "delete"},
			{"batch", "jobs", "create"}, {"batch", "jobs", "update"}, {"batch", "jobs", "patch"}, {"batch", "jobs", "delete"},
			{"networking.k8s.io", "networkpolicies", "create"}, {"networking.k8s.io", "networkpolicies", "update"}, {"networking.k8s.io", "networkpolicies", "patch"}, {"networking.k8s.io", "networkpolicies", "delete"},
			{"rbac.authorization.k8s.io", "roles", "escalate"}, {"rbac.authorization.k8s.io", "rolebindings", "create"},
		} {
			allowed := (namespace == "tenant" || namespace == "runtime") && permission.group != "rbac.authorization.k8s.io" && !(permission.resource == "secrets" && (permission.verb == "list" || permission.verb == "watch"))
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               controllerUser,
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: permission.group, Resource: permission.resource, Verb: permission.verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			if review.Status.Allowed != allowed {
				test.Errorf("%s %s/%s in %q: allowed=%t, want %t (%s)", permission.verb, permission.group, permission.resource, namespace, review.Status.Allowed, allowed, review.Status.Reason)
			}
		}
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
		for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               controllerUser,
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Resource: "configmaps", Verb: verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			allowed := (namespace == "control" || namespace == "tenant" || namespace == "runtime") && verb != "patch" && (verb != "update" || namespace == "control")
			if review.Status.Allowed != allowed {
				test.Errorf("ConfigMap %s in %q: allowed=%t, want %t", verb, namespace, review.Status.Allowed, allowed)
			}
		}
	}
	for _, name := range []string{"source-runtime", "source-provider", "source-gateway", "security-test-kognition-runtime-auth", "unrelated"} {
		for _, verb := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               controllerUser,
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "control", Resource: "secrets", Name: name, Verb: verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			allowed := verb == "get" && (name == "source-runtime" || name == "source-provider" || name == "source-gateway")
			if review.Status.Allowed != allowed {
				test.Errorf("source Secret %s %s: allowed=%t, want %t", verb, name, review.Status.Allowed, allowed)
			}
		}
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
		for _, verb := range []string{"get", "create", "update", "list", "watch", "patch", "delete"} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               controllerUser,
				Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: "coordination.k8s.io", Resource: "leases", Verb: verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			allowed := (namespace == "control" || namespace == "tenant" || namespace == "runtime") && (verb == "get" || verb == "create" || verb == "update")
			if review.Status.Allowed != allowed {
				test.Errorf("Lease %s in %q: allowed=%t, want %t", verb, namespace, review.Status.Allowed, allowed)
			}
		}
	}
	for _, namespace := range []string{"control", "tenant", "runtime", "unrelated", ""} {
		review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               controllerUser,
			Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Resource: "pods", Subresource: "attach", Verb: "create"},
		}}
		if err := admin.Create(ctx, review); err != nil {
			test.Fatal(err)
		}
		if allowed := namespace == "tenant" || namespace == "runtime"; review.Status.Allowed != allowed {
			test.Errorf("Pod attach in %q: allowed=%t, want %t", namespace, review.Status.Allowed, allowed)
		}
	}
	platformConfig := rest.CopyConfig(config)
	platformConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:control:security-test-kognition", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"}}
	platform, err := client.New(platformConfig, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	auditStore, err := k8saudit.NewConfigMapStore(platform, "control")
	if err != nil {
		test.Fatal(err)
	}
	entry := observability.AuditEntry{ID: "identity-isolation", Workspace: "tenant", EventType: observability.AuditMemoryOp, Verb: "store"}
	if err := auditStore.Append(ctx, entry); err != nil {
		test.Fatalf("append memory audit: %v", err)
	}
	if err := auditStore.Append(ctx, entry); err != nil {
		test.Fatalf("retry memory audit: %v", err)
	}
	entries, err := auditStore.Query(ctx, observability.AuditFilter{Workspace: "tenant", Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID {
		test.Fatalf("query memory audit: entries=%v error=%v", entries, err)
	}
	controllerConfig := rest.CopyConfig(config)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: controllerUser, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"}}
	controller, err := client.New(controllerConfig, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	event := &eventsv1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: "controller-reconcile", Namespace: "tenant"},
		EventTime:           metav1.NewMicroTime(time.Now()),
		ReportingController: "kognition.dev/test-controller",
		ReportingInstance:   "rbac-test",
		Action:              "Reconcile",
		Reason:              "Reconciled",
		Note:                "model is ready",
		Type:                corev1.EventTypeNormal,
		Regarding:           corev1.ObjectReference{APIVersion: "kognition.dev/v1alpha1", Kind: "Model", Namespace: "tenant", Name: "model-1"},
	}
	if err := controller.Create(ctx, event); err != nil {
		test.Fatalf("create controller Event: %v", err)
	}
	previousEvent := event.DeepCopy()
	event.Series = &eventsv1.EventSeries{Count: 2, LastObservedTime: metav1.NewMicroTime(time.Now())}
	if err := controller.Patch(ctx, event, client.MergeFrom(previousEvent)); err != nil {
		test.Fatalf("patch controller Event: %v", err)
	}
	coreEvent := &corev1.Event{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(event), coreEvent); err != nil {
		test.Fatalf("read controller Event through core API: %v", err)
	}
	if coreEvent.Reason != event.Reason || coreEvent.Message != event.Note || coreEvent.Action != event.Action || coreEvent.Series == nil || coreEvent.Series.Count != 2 {
		test.Fatalf("core Event lost reconciliation details: %#v", coreEvent)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-token", Namespace: "runtime"}, Data: map[string][]byte{"token": []byte("initial")}}
	if err := controller.Create(ctx, secret); err != nil {
		test.Fatal(err)
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := controller.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
			return err
		}
		secret.Data["token"] = []byte("rotated")
		return controller.Update(ctx, secret)
	}); err != nil {
		test.Fatal(err)
	}
	if err := controller.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || string(secret.Data["token"]) != "rotated" {
		test.Fatalf("read rotated Secret: %v", err)
	}
	if err := controller.Delete(ctx, secret); err != nil {
		test.Fatal(err)
	}
	if err := controller.Get(ctx, client.ObjectKey{Namespace: "unrelated", Name: "credential"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
		test.Fatalf("unrelated Secret read: %v, want Forbidden", err)
	}
	scopedCache, err := cache.New(controllerConfig, cache.Options{Scheme: scheme, DefaultNamespaces: map[string]cache.Config{"control": {}, "tenant": {}, "runtime": {}}})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := scopedCache.GetInformer(ctx, &corev1.Pod{}); err != nil {
		test.Fatal(err)
	}
	cacheErrors := make(chan error, 1)
	go func() { cacheErrors <- scopedCache.Start(ctx) }()
	if !scopedCache.WaitForCacheSync(ctx) {
		test.Fatal("scoped Pod cache failed to synchronize under rendered RBAC")
	}
	if err := scopedCache.List(ctx, &corev1.PodList{}, client.InNamespace("unrelated")); err == nil {
		test.Fatal("cache accepted an unmanaged namespace")
	}
	cancel()
	select {
	case err := <-cacheErrors:
		if err != nil {
			test.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("cache did not stop")
	}
}
