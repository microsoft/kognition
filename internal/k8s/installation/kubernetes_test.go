// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func reviewedReference(test *testing.T, object client.Object, kind schema.GroupVersionKind) ObjectRef {
	test.Helper()
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		test.Fatal(err)
	}
	value := &unstructured.Unstructured{Object: content}
	value.SetGroupVersionKind(kind)
	digest, err := ObjectDigest(value)
	if err != nil {
		test.Fatal(err)
	}
	return ObjectRef{APIVersion: kind.GroupVersion().String(), Kind: kind.Kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), SHA256: digest}
}

func TestGrantCleanupPreservesUnrelatedAndChangedObjects(test *testing.T) {
	for _, scenario := range []string{"exact", "changed", "unrelated-subject", "no-subjects", "other-release", "attested", "missing-attestation", "wrong-controller", "other-release-attested", "partial-helm-attested", "live-owner-attested"} {
		test.Run(scenario, func(test *testing.T) {
			owned := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "old-grant", Namespace: "control", UID: "grant-uid", ResourceVersion: "1", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "release", "meta.helm.sh/release-namespace": "control"}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "old-role"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller", Namespace: "control"}}}
			if scenario == "unrelated-subject" {
				owned.Subjects = append(owned.Subjects, rbacv1.Subject{Kind: "ServiceAccount", Name: "unrelated", Namespace: "control"})
			}
			if scenario == "no-subjects" {
				owned.Subjects = nil
			}
			if scenario == "other-release" || scenario == "other-release-attested" {
				owned.Annotations["meta.helm.sh/release-name"] = "other"
			}
			if scenario == "partial-helm-attested" {
				owned.Annotations = nil
			}
			if scenario == "live-owner-attested" {
				owned.Labels, owned.Annotations = nil, nil
				owned.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner"}}
			}
			if scenario == "attested" || scenario == "missing-attestation" || scenario == "wrong-controller" {
				owned.Labels, owned.Annotations = nil, nil
			}
			ref := reviewedReference(test, owned, rbacv1.SchemeGroupVersion.WithKind("RoleBinding"))
			unrelated := owned.DeepCopy()
			unrelated.Name = "unrelated"
			unrelated.UID = "other-uid"
			if scenario == "changed" {
				owned.RoleRef.Name = "changed-role"
			}
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(owned, unrelated).Build()
			plan := Plan{Release: "release", Grants: []ObjectRef{ref}, Controllers: []Controller{{Deployment: ObjectRef{UID: "controller-uid"}, ServiceAccount: "controller"}}}
			if scenario == "attested" || scenario == "wrong-controller" || strings.HasSuffix(scenario, "-attested") {
				plan.CleanupAttestations = []CleanupAttestation{{ResourceUID: ref.UID, ControllerUID: "controller-uid", Evidence: "reviewed controller installation audit"}}
				if scenario == "wrong-controller" {
					plan.CleanupAttestations[0].ControllerUID = "other"
				}
			}
			operations := &Kubernetes{Plan: plan, Manifest: activation.Manifest{Namespace: "control", Controller: "controller"}, Admin: admin}
			err := operations.RevokeGrants(context.Background())
			accepted := scenario == "exact" || scenario == "attested"
			if (err == nil) != accepted {
				test.Fatalf("unexpected cleanup: %v", err)
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(unrelated), &rbacv1.RoleBinding{}); err != nil {
				test.Fatal("unrelated grant deleted", err)
			}
			err = admin.Get(context.Background(), client.ObjectKeyFromObject(owned), &rbacv1.RoleBinding{})
			if !accepted && err != nil {
				test.Fatal("changed grant deleted")
			}
			if accepted && !apierrors.IsNotFound(err) {
				test.Fatalf("reviewed grant remains: %v", err)
			}
		})
	}
}

func TestReleaseObjectsRequireExactOwnership(test *testing.T) {
	for _, scenario := range []string{"exact", "replaced", "owner-changed", "missing-review", "collision", "normalized-duplicate", "removed-unreviewed"} {
		test.Run(scenario, func(test *testing.T) {
			object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "control", UID: "reviewed", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "release", "meta.helm.sh/release-namespace": "control"}}, Data: map[string]string{"key": "value"}}
			ref := reviewedReference(test, object, corev1.SchemeGroupVersion.WithKind("ConfigMap"))
			if scenario == "replaced" {
				object.UID = "replacement"
			}
			if scenario == "owner-changed" {
				object.Annotations["meta.helm.sh/release-name"] = "other"
				ref = reviewedReference(test, object, corev1.SchemeGroupVersion.WithKind("ConfigMap"))
			}
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
			mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithRESTMapper(mapper).WithObjects(object).Build()
			directory := test.TempDir()
			artifact := func(name, content string) Artifact {
				path := filepath.Join(directory, name)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					test.Fatal(err)
				}
				return Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
			}
			previous := artifact("previous.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n")
			next := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n  namespace: control\n"
			if scenario == "normalized-duplicate" {
				next += "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n"
			}
			if scenario == "removed-unreviewed" {
				next = ""
			}
			plan := Plan{Release: "release", PreviousManifest: &previous, Manifest: artifact("next.yaml", next), RetainedObjects: []ObjectRef{ref}}
			if scenario == "missing-review" {
				plan.RetainedObjects = nil
			}
			if scenario == "collision" {
				plan.PreviousManifest = nil
				plan.RetainedObjects = nil
			}
			operations := &Kubernetes{Plan: plan, Admin: admin, Manifest: activation.Manifest{Namespace: "control"}}
			err := operations.VerifyReleaseObjects(context.Background())
			if (err == nil) != (scenario == "exact") {
				test.Fatalf("ownership check: %v", err)
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(object), &corev1.ConfigMap{}); err != nil {
				test.Fatal("validation mutated object", err)
			}
		})
	}
}

func TestFailedReleaseInventory(test *testing.T) {
	for _, scenario := range []string{"absent", "deployed-only", "unreviewed-missing", "unexpected-live", "absent-outside-history", "deployed-unreviewed", "replaced-record", "pending-record", "wrong-record", "live-and-absent"} {
		test.Run(scenario, func(test *testing.T) {
			artifact := func(name, content string) Artifact {
				path := filepath.Join(test.TempDir(), name)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					test.Fatal(err)
				}
				return Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
			}
			manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: control}\n"
			previous := artifact("previous.yaml", manifest)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.release.v2", Namespace: "control", UID: "record", Labels: map[string]string{"owner": "helm", "name": "release", "version": "2", "status": "failed"}}, Type: "helm.sh/release.v1"}
			ref := reviewedReference(test, secret, corev1.SchemeGroupVersion.WithKind("Secret"))
			plan := Plan{Release: "release", ReleaseRevision: 2, FailedRelease: &ref, PreviousManifest: &previous, Manifest: artifact("next.yaml", manifest), AbsentObjects: []ObjectIdentity{{APIVersion: "v1", Kind: "ConfigMap", Namespace: "control", Name: "settings"}}}
			object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "settings", Namespace: "control", UID: "settings", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "release", "meta.helm.sh/release-namespace": "control"}}}
			objects := []client.Object{secret}
			switch scenario {
			case "deployed-only", "deployed-unreviewed":
				plan.PreviousDeployedManifest = &previous
				empty := artifact("failed.yaml", "")
				plan.PreviousManifest = &empty
				plan.PreviousDeployedRevision = 1
				plan.AbsentObjects = nil
				objects = append(objects, object)
				if scenario == "deployed-only" {
					plan.RetainedObjects = []ObjectRef{reviewedReference(test, object, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}
				}
			case "unreviewed-missing":
				plan.AbsentObjects = nil
			case "unexpected-live":
				objects = append(objects, object)
			case "absent-outside-history":
				plan.AbsentObjects[0].Name = "other"
			case "replaced-record":
				secret.UID = "replacement"
			case "pending-record":
				secret.Labels["status"] = "pending-upgrade"
				ref = reviewedReference(test, secret, corev1.SchemeGroupVersion.WithKind("Secret"))
			case "wrong-record":
				secret.Name = "sh.helm.release.v1.release.v1"
				ref = reviewedReference(test, secret, corev1.SchemeGroupVersion.WithKind("Secret"))
			case "live-and-absent":
				plan.RetainedObjects = []ObjectRef{reviewedReference(test, object, corev1.SchemeGroupVersion.WithKind("ConfigMap"))}
			}
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
			mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
			mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithRESTMapper(mapper).WithObjects(objects...).Build()
			operations := &Kubernetes{Plan: plan, Admin: admin, Manifest: activation.Manifest{Namespace: "control"}}
			err := operations.VerifyReleaseObjects(context.Background())
			if (err == nil) != (scenario == "absent" || scenario == "deployed-only") {
				test.Fatalf("failed-release inventory: %v", err)
			}
		})
	}
}

func TestLegacyPolicyCleanupRequiresReviewedLineage(test *testing.T) {
	for _, scenario := range []string{"exact", "missing-attestation", "wrong-controller", "duplicate-attestation", "empty-evidence", "wrong-invocation", "allow-egress", "live-owner", "outside-enrollment", "baseline"} {
		test.Run(scenario, func(test *testing.T) {
			labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/stdio-invocation": "invocation"}
			policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-old", Namespace: "tenant", UID: "policy", Labels: labels}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}
			attestation := CleanupAttestation{ResourceUID: "policy", ControllerUID: "controller", InvocationID: "invocation", Evidence: "administrator reviewed archived invocation and controller audit records"}
			if scenario == "wrong-controller" {
				attestation.ControllerUID = "other"
			}
			if scenario == "empty-evidence" {
				attestation.Evidence = " "
			}
			if scenario == "wrong-invocation" {
				attestation.InvocationID = "other"
			}
			if scenario == "allow-egress" {
				policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			}
			if scenario == "live-owner" {
				policy.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "job", UID: "job"}}
			}
			plan := Plan{Controllers: []Controller{{Deployment: ObjectRef{UID: "controller"}}}, LegacyPolicies: []ObjectRef{reviewedReference(test, policy, networkingv1.SchemeGroupVersion.WithKind("NetworkPolicy"))}, CleanupAttestations: []CleanupAttestation{attestation}}
			if scenario == "missing-attestation" {
				plan.CleanupAttestations = nil
			}
			if scenario == "duplicate-attestation" {
				plan.CleanupAttestations = append(plan.CleanupAttestations, attestation)
			}
			manifest := activation.Manifest{Namespace: "control", Namespaces: []string{"tenant"}}
			if scenario == "outside-enrollment" {
				manifest.Namespaces = []string{"other"}
			}
			if scenario == "baseline" {
				manifest.Objects = []*unstructured.Unstructured{{Object: map[string]any{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": map[string]any{"namespace": policy.Namespace, "name": policy.Name}}}}
			}
			unrelated := policy.DeepCopy()
			unrelated.Name, unrelated.UID = "unrelated", "unrelated"
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(policy, unrelated).Build()
			operations := &Kubernetes{Plan: plan, Manifest: manifest, Admin: admin}
			err := operations.RemoveLegacyPolicies(context.Background())
			if (err == nil) != (scenario == "exact") {
				test.Fatalf("cleanup outcome: %v", err)
			}
			err = admin.Get(context.Background(), client.ObjectKeyFromObject(policy), &networkingv1.NetworkPolicy{})
			if scenario == "exact" && !apierrors.IsNotFound(err) {
				test.Fatalf("reviewed policy remains: %v", err)
			}
			if scenario != "exact" && err != nil {
				test.Fatalf("unapproved policy deleted: %v", err)
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(unrelated), &networkingv1.NetworkPolicy{}); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestCleanupRecoveryRequiresExactDurableIntent(test *testing.T) {
	for _, mutation := range []string{"unchanged", "replaced", "modified"} {
		test.Run(mutation, func(test *testing.T) {
			ctx := context.Background()
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "control", UID: "controller", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}}
			deployment.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
			deployment.Annotations = map[string]string{"meta.helm.sh/release-name": "release", "meta.helm.sh/release-namespace": "control"}
			deployment.Spec.Template.Spec.ServiceAccountName = "controller"
			grant := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "grant", UID: "grant"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "controller", Namespace: "control"}}}
			plan := Plan{Controllers: []Controller{{Deployment: reviewedReference(test, deployment, appsv1.SchemeGroupVersion.WithKind("Deployment")), ServiceAccount: "controller"}}, Grants: []ObjectRef{reviewedReference(test, grant, rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"))}, CleanupAttestations: []CleanupAttestation{{ResourceUID: "grant", ControllerUID: "controller", Evidence: "reviewed old controller grant"}}}
			plan.Release = "release"
			directory := test.TempDir()
			artifact := func(name, content string) Artifact {
				path := filepath.Join(directory, name)
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					test.Fatal(err)
				}
				return Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
			}
			next := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: controller, namespace: control}\n"
			previousContent := next + "---\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: grant}\n"
			previous := artifact("previous.yaml", previousContent)
			plan.PreviousManifest, plan.Manifest = &previous, artifact("next.yaml", next)
			mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{appsv1.SchemeGroupVersion, rbacv1.SchemeGroupVersion})
			mapper.Add(appsv1.SchemeGroupVersion.WithKind("Deployment"), meta.RESTScopeNamespace)
			mapper.Add(rbacv1.SchemeGroupVersion.WithKind("ClusterRoleBinding"), meta.RESTScopeRoot)
			mapper.Add(schema.GroupVersionKind{Group: rbacv1.GroupName, Version: "v1beta1", Kind: "ClusterRoleBinding"}, meta.RESTScopeRoot)
			record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "journal", Namespace: "authority", UID: "claim", ResourceVersion: "1"}, Data: map[string]string{"events": "claimed\nstop-controllers:pending\n", "planSHA256": strings.Repeat("a", 64)}}
			lost := errors.New("lost mutation response")
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithRESTMapper(mapper).WithObjects(deployment, grant, record).WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if err := underlying.Patch(ctx, object, patch, options...); err != nil {
						return err
					}
					object.SetGeneration(object.GetGeneration() + 1)
					if err := underlying.Update(ctx, object); err != nil {
						return err
					}
					return lost
				},
				Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					if err := underlying.Delete(ctx, object, options...); err != nil {
						return err
					}
					return lost
				},
			}).Build()
			journal := &Journal{admin: admin, ctx: ctx, record: record}
			operations := &Kubernetes{Plan: plan, Admin: admin, Journal: journal, Manifest: activation.Manifest{Namespace: "control", Controller: "controller"}}
			if err := operations.StopControllers(ctx); !errors.Is(err, lost) {
				test.Fatal(err)
			}
			if err := operations.RevokeGrants(ctx); !errors.Is(err, lost) {
				test.Fatal(err)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
				test.Fatal(err)
			}
			if mutation == "replaced" {
				deployment.UID = "replacement"
			}
			if mutation == "modified" {
				deployment.Spec.Template.Spec.ServiceAccountName = "other"
			}
			if mutation != "unchanged" {
				if err := admin.Update(ctx, deployment); err != nil {
					test.Fatal(err)
				}
				grant.UID, grant.ResourceVersion = "replacement", ""
				if err := admin.Create(ctx, grant); err != nil {
					test.Fatal(err)
				}
			}
			operations.recovering = true
			if err := operations.StopControllers(ctx); (err == nil) != (mutation == "unchanged") {
				test.Fatalf("controller recovery: %v", err)
			}
			if err := operations.RevokeGrants(ctx); (err == nil) != (mutation == "unchanged") {
				test.Fatalf("grant recovery: %v", err)
			}
			if err := operations.VerifyReleaseObjects(ctx); (err == nil) != (mutation == "unchanged") {
				test.Fatalf("release inventory recovery: %v", err)
			}
			operations.Journal = nil
			if err := operations.VerifyReleaseObjects(ctx); err == nil {
				test.Fatal("changed release inventory accepted without durable mutation intent")
			}
			operations.Journal = journal
			if mutation == "unchanged" {
				previous = artifact("previous.yaml", strings.ReplaceAll(previousContent, "rbac.authorization.k8s.io/v1", "rbac.authorization.k8s.io/v1beta1"))
				if err := operations.VerifyReleaseObjects(ctx); err == nil {
					test.Fatal("deleted grant intent bypassed previous manifest API version check")
				}
			}
			if mutation != "unchanged" {
				if err := admin.Get(ctx, client.ObjectKeyFromObject(grant), grant); err != nil {
					test.Fatal("replacement deleted", err)
				}
			}
		})
	}
}

func TestControllerShutdownRequiresReleaseOwnership(test *testing.T) {
	for _, scenario := range []string{"exact", "other-release", "live-owner", "wrong-account", "default-account", "outside-installation"} {
		test.Run(scenario, func(test *testing.T) {
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "control", UID: "controller", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "release", "meta.helm.sh/release-namespace": "control"}}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "controller"}}}}
			if scenario == "other-release" {
				deployment.Annotations["meta.helm.sh/release-name"] = "other"
			}
			if scenario == "live-owner" {
				deployment.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner"}}
			}
			if scenario == "wrong-account" {
				deployment.Spec.Template.Spec.ServiceAccountName = "other"
			}
			if scenario == "default-account" {
				deployment.Spec.Template.Spec.ServiceAccountName = "default"
			}
			if scenario == "outside-installation" {
				deployment.Namespace = "other"
			}
			controller := Controller{Deployment: reviewedReference(test, deployment, appsv1.SchemeGroupVersion.WithKind("Deployment")), ServiceAccount: "controller"}
			if scenario == "default-account" {
				controller.ServiceAccount = "default"
			}
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(deployment).Build()
			operations := &Kubernetes{Plan: Plan{Release: "release", Controllers: []Controller{controller}}, Manifest: activation.Manifest{Namespace: "control"}, Admin: admin}
			err := operations.StopControllers(context.Background())
			if (err == nil) != (scenario == "exact") {
				test.Fatalf("shutdown outcome: %v", err)
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(deployment), deployment); err != nil {
				test.Fatal(err)
			}
			want := int32(1)
			if scenario == "exact" {
				want = 0
			}
			if *deployment.Spec.Replicas != want {
				test.Fatalf("replicas = %d, want %d", *deployment.Spec.Replicas, want)
			}
		})
	}
}

func TestControlledDrainPreservesInvocationsAndControllerPrivileges(test *testing.T) {
	for _, finalizers := range [][]string{nil, {"kognition.dev/cleanup"}} {
		invocation := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kognition.dev/v1alpha1", "kind": "Invocation", "metadata": map[string]any{"name": "existing", "namespace": "tenant", "uid": "invocation-uid"}}}
		invocation.SetFinalizers(finalizers)
		localScheme := runtime.NewScheme()
		if err := scheme.AddToScheme(localScheme); err != nil {
			test.Fatal(err)
		}
		localScheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "InvocationList"}, &unstructured.UnstructuredList{})
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "control", UID: "controller-uid"}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}}
		grant := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "old", UID: "grant-uid"}}
		admin := clientfake.NewClientBuilder().WithScheme(localScheme).WithObjects(invocation, deployment, grant).Build()
		discover := &fake.FakeDiscovery{Fake: &k8stesting.Fake{}}
		operations := &Kubernetes{Admin: admin, Discovery: &preferredDiscovery{FakeDiscovery: discover, resources: []*metav1.APIResourceList{{GroupVersion: "kognition.dev/v1alpha1", APIResources: []metav1.APIResource{{Name: "invocations", Kind: "Invocation", Namespaced: true, Verbs: metav1.Verbs{"list"}}}}}}, Manifest: activation.Manifest{Namespace: "control", Namespaces: []string{"tenant"}}}
		if err := operations.WorkloadsDrained(context.Background()); err == nil {
			test.Fatal("existing invocation accepted")
		}
		if err := admin.Get(context.Background(), client.ObjectKeyFromObject(deployment), deployment); err != nil || *deployment.Spec.Replicas != 1 {
			test.Fatal("controller stopped before finalization")
		}
		if err := admin.Get(context.Background(), client.ObjectKeyFromObject(grant), grant); err != nil {
			test.Fatal("finalization privileges revoked")
		}
		if err := admin.Get(context.Background(), client.ObjectKeyFromObject(invocation), invocation); err != nil || len(invocation.GetFinalizers()) != len(finalizers) {
			test.Fatal("invocation or finalizers altered")
		}
	}
}

type preferredDiscovery struct {
	*fake.FakeDiscovery
	resources []*metav1.APIResourceList
}

func TestControlledDrainRejectsControllerIdentityProducers(test *testing.T) {
	for _, owned := range []bool{false, true} {
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: "control", UID: "controller-uid"}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "controller"}}}}
		replicas := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "replicas", Namespace: "control", UID: "replicas-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: ptr.To(true)}}}, Spec: appsv1.ReplicaSetSpec{Template: deployment.Spec.Template}}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "producer", Namespace: "control", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "controller"}}
		if owned {
			pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicas.Name, UID: replicas.UID, Controller: ptr.To(true)}}
		}
		admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(deployment, replicas, pod).Build()
		operations := &Kubernetes{Admin: admin, Discovery: &preferredDiscovery{}, Manifest: activation.Manifest{Namespace: "control", Controller: "controller"}, Plan: Plan{Controllers: []Controller{{Deployment: reviewedReference(test, deployment, appsv1.SchemeGroupVersion.WithKind("Deployment")), ServiceAccount: "controller"}}}}
		if err := operations.WorkloadsDrained(context.Background()); (err == nil) != owned {
			test.Fatalf("owned=%v: %v", owned, err)
		}
		if err := admin.Get(context.Background(), client.ObjectKeyFromObject(deployment), deployment); err != nil || *deployment.Spec.Replicas != 1 {
			test.Fatal("drain changed controller")
		}
	}
}

func TestBootstrapReadinessRetriesOnlyWhileDrained(test *testing.T) {
	delay := errors.New("certificate pending")
	unsafe := errors.New("controller running")
	for _, changed := range []bool{false, true} {
		attempts := 0
		err := waitForBootstrap(context.Background(), time.Millisecond, time.Second, func(context.Context) error {
			if changed && attempts > 0 {
				return unsafe
			}
			return nil
		}, func(context.Context) error {
			attempts++
			if attempts < 2 {
				return delay
			}
			return nil
		})
		if changed && (!errors.Is(err, unsafe) || attempts != 1) {
			test.Fatalf("unsafe readiness retry: %d, %v", attempts, err)
		}
		if !changed && (err != nil || attempts != 2) {
			test.Fatalf("readiness not retried: %d, %v", attempts, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForBootstrap(ctx, time.Millisecond, time.Second, func(context.Context) error { return nil }, func(context.Context) error { return delay }); !errors.Is(err, context.Canceled) {
		test.Fatalf("cancellation lost: %v", err)
	}
}

func TestBootstrapReadinessPreservesTerminationCause(test *testing.T) {
	for _, scenario := range []string{"timeout", "canceled"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pending := errors.New("admission certificate pending")
			attempts := 0
			err := waitForBootstrap(ctx, time.Millisecond, 20*time.Millisecond, func(context.Context) error {
				return nil
			}, func(ctx context.Context) error {
				attempts++
				if scenario == "canceled" {
					cancel()
				}
				<-ctx.Done()
				return pending
			})
			cause := context.DeadlineExceeded
			if scenario == "canceled" {
				cause = context.Canceled
			}
			if !errors.Is(err, cause) || !errors.Is(err, pending) || attempts != 1 {
				test.Fatalf("readiness termination lost diagnostic or retried after termination: attempts=%d, error=%v", attempts, err)
			}
		})
	}
}

func (discover *preferredDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return discover.resources, nil
}

func TestTargetRejectsNamespaceReplacement(test *testing.T) {
	admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control", UID: "control"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant", UID: types.UID("replacement")}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "authority", UID: "authority-uid"}},
	).Build()
	operations := &Kubernetes{Admin: admin, Manifest: activation.Manifest{Namespace: "control", Controller: "controller", Namespaces: []string{"tenant"}}, Plan: Plan{ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid", WorkloadNamespaceUIDs: map[string]string{"tenant": "original"}}}
	if err := operations.VerifyTarget(context.Background()); err == nil {
		test.Fatal("namespace replacement accepted")
	}
}

func TestTargetRejectsControllerAuthorityAccess(test *testing.T) {
	for _, status := range []authv1.SubjectAccessReviewStatus{{Denied: true}, {Allowed: true}, {EvaluationError: "unavailable"}} {
		admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control", UID: "control"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "authority", UID: "authority"}},
		).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if review, ok := object.(*authv1.SubjectAccessReview); ok {
				review.Status = status
				return nil
			}
			return underlying.Create(ctx, object, options...)
		}}).Build()
		operations := &Kubernetes{Admin: admin, Manifest: activation.Manifest{Namespace: "control", Controller: "controller"}, Plan: Plan{ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority"}}
		err := operations.VerifyTarget(context.Background())
		if (err != nil) != !status.Denied {
			test.Fatalf("status %+v: %v", status, err)
		}
	}
}

func TestTargetRejectsNamedAuthorityAccess(test *testing.T) {
	for _, target := range []struct{ resource, name string }{{"namespaces", "kube-system"}, {"configmaps", "specific-recovery-receipt"}} {
		test.Run(target.resource, func(test *testing.T) {
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control", UID: "control"}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "authority", UID: "authority"}},
				&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "named", Namespace: "authority"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"specific-recovery-receipt"}, Verbs: []string{"patch"}}}},
			).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
				if review, ok := object.(*authv1.SubjectAccessReview); ok {
					attributes := review.Spec.ResourceAttributes
					review.Status.Allowed = attributes.Resource == target.resource && attributes.Name == target.name && attributes.Verb == "patch"
					review.Status.Denied = !review.Status.Allowed
					return nil
				}
				return underlying.Create(ctx, object, options...)
			}}).Build()
			operations := &Kubernetes{Admin: admin, Manifest: activation.Manifest{Namespace: "control", Controller: "controller"}, Plan: Plan{ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority"}}
			if err := operations.VerifyTarget(context.Background()); err == nil {
				test.Fatal("named authority mutation accepted")
			}
		})
	}
}
