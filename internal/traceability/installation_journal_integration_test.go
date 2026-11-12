// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestInstallationJournalAPI(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := client.NewWithWatch(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	authority := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "installation-authority"}}
	if err := admin.Create(ctx, authority); err != nil {
		test.Fatal(err)
	}
	root := &corev1.Namespace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, root); err != nil {
		test.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	test.Run("settled partial cleanup", func(test *testing.T) {
		control := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cleanup-control"}}
		tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cleanup-tenant"}}
		for _, namespace := range []*corev1.Namespace{control, tenant} {
			if err := admin.Create(ctx, namespace); err != nil {
				test.Fatal(err)
			}
		}
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "old-controller", Namespace: control.Name}, Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "old"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "old"}}, Spec: corev1.PodSpec{ServiceAccountName: "old-controller", Containers: []corev1.Container{{Name: "controller", Image: "example.invalid/controller:fixture"}}}},
		}}
		deployment.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
		deployment.Annotations = map[string]string{"meta.helm.sh/release-name": "cleanup", "meta.helm.sh/release-namespace": control.Name}
		if err := admin.Create(ctx, deployment); err != nil {
			test.Fatal(err)
		}
		grant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "old-grant", Namespace: control.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "old-role"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "old-controller", Namespace: control.Name}}}
		if err := admin.Create(ctx, grant); err != nil {
			test.Fatal(err)
		}
		labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/stdio-invocation": "invocation"}
		policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-orphan", Namespace: tenant.Name, Labels: labels}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}
		if err := admin.Create(ctx, policy); err != nil {
			test.Fatal(err)
		}
		unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: control.Name}, Data: map[string]string{"keep": "unchanged"}}
		if err := admin.Create(ctx, unrelated); err != nil {
			test.Fatal(err)
		}
		reference := func(version, kind string, object client.Object) installation.ObjectRef {
			live := &unstructured.Unstructured{}
			live.SetAPIVersion(version)
			live.SetKind(kind)
			if err := admin.Get(ctx, client.ObjectKeyFromObject(object), live); err != nil {
				test.Fatal(err)
			}
			hash, err := installation.ObjectDigest(live)
			if err != nil {
				test.Fatal(err)
			}
			return installation.ObjectRef{APIVersion: version, Kind: kind, Namespace: live.GetNamespace(), Name: live.GetName(), UID: string(live.GetUID()), SHA256: hash}
		}
		plan := installation.Plan{ClusterUID: string(root.UID), NamespaceUID: string(control.UID), Release: "cleanup", AuthorityNamespace: authority.Name, AuthorityNamespaceUID: string(authority.UID), WorkloadNamespaceUIDs: map[string]string{tenant.Name: string(tenant.UID)},
			Controllers: []installation.Controller{{Deployment: reference("apps/v1", "Deployment", deployment), ServiceAccount: "old-controller"}}, Grants: []installation.ObjectRef{reference("rbac.authorization.k8s.io/v1", "RoleBinding", grant)}}
		plan.LegacyPolicies = []installation.ObjectRef{reference("networking.k8s.io/v1", "NetworkPolicy", policy)}
		plan.CleanupAttestations = []installation.CleanupAttestation{
			{ResourceUID: string(grant.UID), ControllerUID: string(deployment.UID), Evidence: "reviewed fixture controller grant"},
			{ResourceUID: string(policy.UID), ControllerUID: string(deployment.UID), InvocationID: "invocation", Evidence: "reviewed fixture invocation audit"},
		}
		lost := errors.New("lost persisted cleanup response")
		losePatch, loseDelete := true, true
		uncertain := interceptor.NewClient(admin, interceptor.Funcs{
			Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if err := underlying.Patch(ctx, object, patch, options...); err != nil {
					return err
				}
				if losePatch && object.GetName() == deployment.Name && object.GetNamespace() == deployment.Namespace {
					losePatch = false
					return lost
				}
				return nil
			},
			Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
				if err := underlying.Delete(ctx, object, options...); err != nil {
					return err
				}
				if loseDelete && object.GetName() == grant.Name && object.GetNamespace() == grant.Namespace {
					loseDelete = false
					return lost
				}
				return nil
			},
		})
		journal, err := installation.Claim(ctx, uncertain, plan, digest)
		if err != nil {
			test.Fatal(err)
		}
		if err := journal.Record("stop-controllers:pending"); err != nil {
			test.Fatal(err)
		}
		discover, err := discovery.NewDiscoveryClientForConfig(config)
		if err != nil {
			test.Fatal(err)
		}
		operations := &installation.Kubernetes{Plan: plan, Admin: uncertain, Journal: journal, Discovery: discover, Manifest: activation.Manifest{Namespace: control.Name, Controller: "new-controller", Deployment: "new-controller", Namespaces: []string{tenant.Name}}}
		if err := operations.StopControllers(ctx); !errors.Is(err, lost) {
			test.Fatalf("scale response: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
			test.Fatal(err)
		}
		if *deployment.Spec.Replicas != 0 || deployment.Generation != 2 {
			test.Fatalf("unexpected persisted scale: %+v", deployment)
		}
		deployment.Status.ObservedGeneration = deployment.Generation
		if err := admin.Status().Update(ctx, deployment); err != nil {
			test.Fatal(err)
		}
		journal, err = installation.OpenJournal(ctx, uncertain, plan, digest, journal.UID())
		if err != nil {
			test.Fatal(err)
		}
		if err := operations.RecoverCleanup(ctx, journal); !errors.Is(err, lost) {
			test.Fatalf("delete response: %v", err)
		}
		journal, err = installation.OpenJournal(ctx, admin, plan, digest, journal.UID())
		if err != nil {
			test.Fatal(err)
		}
		operations.Admin = admin
		if err := operations.RecoverCleanup(ctx, journal); err != nil {
			test.Fatalf("settled cleanup: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(policy), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
			test.Fatalf("orphan policy remains: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(grant), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
			test.Fatalf("grant remains: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(unrelated), unrelated); err != nil || unrelated.Data["keep"] != "unchanged" {
			test.Fatalf("unrelated object changed: %v", err)
		}
		if err := operations.VerifyDrained(ctx); err != nil {
			test.Fatal(err)
		}
		if err := journal.Retire("drained-retry"); err != nil {
			test.Fatal(err)
		}
	})
	test.Run("settled Helm response loss", func(test *testing.T) {
		control := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "helm-control"}}
		tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "helm-tenant"}}
		for _, namespace := range []*corev1.Namespace{control, tenant} {
			if err := admin.Create(ctx, namespace); err != nil {
				test.Fatal(err)
			}
		}
		manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: unchanged, namespace: helm-control}\n"
		path := filepath.Join(test.TempDir(), "manifest.yaml")
		if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
			test.Fatal(err)
		}
		plan := installation.Plan{ClusterUID: string(root.UID), NamespaceUID: string(control.UID), Release: "settle", AuthorityNamespace: authority.Name, AuthorityNamespaceUID: string(authority.UID), WorkloadNamespaceUIDs: map[string]string{tenant.Name: string(tenant.UID)}, Manifest: installation.Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(manifest)))}}
		payload, err := json.Marshal(map[string]any{"name": plan.Release, "namespace": control.Name, "version": 1, "manifest": manifest, "info": map[string]string{"status": "pending-install"}, "config": map[string]bool{"activated": true}})
		if err != nil {
			test.Fatal(err)
		}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(payload); err != nil {
			test.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			test.Fatal(err)
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.settle.v1", Namespace: control.Name, Labels: map[string]string{"name": plan.Release, "owner": "helm", "version": "1", "status": "pending-install"}}, Type: "helm.sh/release.v1", Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(compressed.Bytes()))}}
		if err := admin.Create(ctx, secret); err != nil {
			test.Fatal(err)
		}
		live := &unstructured.Unstructured{}
		live.SetAPIVersion("v1")
		live.SetKind("Secret")
		if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), live); err != nil {
			test.Fatal(err)
		}
		recordDigest, err := installation.ObjectDigest(live)
		if err != nil {
			test.Fatal(err)
		}
		journal, err := installation.Claim(ctx, admin, plan, digest)
		if err != nil {
			test.Fatal(err)
		}
		if err := journal.Record("admission-bootstrap:pending"); err != nil {
			test.Fatal(err)
		}
		operations := &installation.Kubernetes{Plan: plan, Admin: admin, Manifest: activation.Manifest{Namespace: control.Name, Controller: "controller", Deployment: "controller", Namespaces: []string{tenant.Name}}}
		if err := operations.SettleHelm(ctx, string(secret.UID), recordDigest); err == nil {
			test.Fatal("settlement without journal succeeded")
		}
		lost := errors.New("lost Helm settlement response")
		uncertainIntent := interceptor.NewClient(admin, interceptor.Funcs{Update: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			return lost
		}})
		operations.Journal, err = installation.OpenJournal(ctx, uncertainIntent, plan, digest, journal.UID())
		if err != nil {
			test.Fatal(err)
		}
		if err := operations.SettleHelm(ctx, string(secret.UID), recordDigest); !errors.Is(err, lost) {
			test.Fatalf("uncertain intent: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || secret.Labels["status"] != "pending-install" {
			test.Fatalf("Secret changed without durable intent: %v", err)
		}
		uncertain := interceptor.NewClient(admin, interceptor.Funcs{Patch: func(ctx context.Context, underlying client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if err := underlying.Patch(ctx, object, patch, options...); err != nil {
				return err
			}
			if object.GetName() == secret.Name && object.GetNamespace() == secret.Namespace {
				return lost
			}
			return nil
		}})
		operations.Admin = uncertain
		operations.Journal = journal
		if err := operations.SettleHelm(ctx, string(secret.UID), recordDigest); !errors.Is(err, lost) {
			test.Fatalf("persisted settlement response: %v", err)
		}
		operations.Admin = admin
		operations.Journal, err = installation.OpenJournal(ctx, admin, plan, digest, journal.UID())
		if err != nil {
			test.Fatal(err)
		}
		if err := operations.SettleHelm(ctx, string(secret.UID), recordDigest); err != nil {
			test.Fatalf("exact settled retry: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil || secret.Labels["status"] != "failed" {
			test.Fatalf("settled record not failed: %v", err)
		}
		advanced := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.settle.v2", Namespace: control.Name}}
		if err := admin.Create(ctx, advanced); err != nil {
			test.Fatal(err)
		}
		if err := operations.SettleHelm(ctx, string(secret.UID), recordDigest); err == nil {
			test.Fatal("advanced history accepted")
		}
		if err := admin.Delete(ctx, advanced); err != nil {
			test.Fatal(err)
		}
		secret.Labels["drift"] = "changed"
		if err := admin.Update(ctx, secret); err != nil {
			test.Fatal(err)
		}
		if err := operations.SettleHelm(ctx, string(secret.UID), recordDigest); err == nil {
			test.Fatal("drift accepted through durable intent")
		}
	})
	plan := installation.Plan{ClusterUID: string(root.UID), NamespaceUID: "control", Release: "stale", AuthorityNamespace: authority.Name, AuthorityNamespaceUID: string(authority.UID)}
	stale, err := installation.Claim(ctx, admin, plan, digest)
	if err != nil {
		test.Fatal(err)
	}
	alternate := plan
	alternate.AuthorityNamespace = "another-authority"
	if _, err := installation.Claim(ctx, admin, alternate, digest); err == nil {
		test.Fatal("alternate authority bypassed target exclusivity")
	}
	current, err := installation.OpenJournal(ctx, admin, plan, digest, stale.UID())
	if err != nil {
		test.Fatal(err)
	}
	if err := current.Record("stop-controllers:pending"); err != nil {
		test.Fatal(err)
	}
	if err := stale.Retire("aborted-before-mutation"); err == nil {
		test.Fatal("stale recovery deleted a changed claim")
	}
	if err := current.Retire("drained-retry"); err != nil {
		test.Fatalf("stale recovery poisoned legitimate retirement: %v", err)
	}
	if err := admin.Get(ctx, installation.JournalKey(plan), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		test.Fatalf("retired claim remains: %v", err)
	}

	plan.Release = "lost-response"
	lost := errors.New("response lost after persistence")
	uncertain := interceptor.NewClient(admin, interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if err := underlying.Create(ctx, object, options...); err != nil {
			return err
		}
		return lost
	}})
	if _, err := installation.Claim(ctx, uncertain, plan, digest); !errors.Is(err, lost) {
		test.Fatalf("unknown create outcome was not returned: %v", err)
	}
	if _, err := installation.Claim(ctx, admin, plan, digest); err == nil {
		test.Fatal("retry acquired persisted uncertain claim")
	}
	record := &corev1.ConfigMap{}
	if err := admin.Get(ctx, installation.JournalKey(plan), record); err != nil {
		test.Fatal(err)
	}
	recovered, err := installation.OpenJournal(ctx, uncertain, plan, digest, string(record.UID))
	if err != nil {
		test.Fatal(err)
	}
	if err := recovered.Retire("aborted-before-mutation"); !errors.Is(err, lost) {
		test.Fatalf("lost receipt response: %v", err)
	}
	if err := admin.Get(ctx, installation.JournalKey(plan), record); err != nil {
		test.Fatalf("claim deleted before durable receipt confirmation: %v", err)
	}
	if err := recovered.Retire("aborted-before-mutation"); err != nil {
		test.Fatalf("matching durable receipt did not allow retry: %v", err)
	}

	replacement, err := installation.Claim(ctx, admin, plan, digest)
	if err != nil {
		test.Fatal(err)
	}
	if replacement.UID() == recovered.UID() {
		test.Fatal("replacement reused claim identity")
	}
	if err := recovered.Record("bootstrap:pending"); err == nil {
		test.Fatal("stale executor changed replacement claim")
	}
	if err := recovered.Retire("aborted-before-mutation"); err == nil {
		test.Fatal("stale recovery deleted replacement claim")
	}
	if err := admin.Get(ctx, installation.JournalKey(plan), record); err != nil || string(record.UID) != replacement.UID() {
		test.Fatalf("replacement claim lost: %v", err)
	}
	if err := replacement.Retire("aborted-before-mutation"); err != nil {
		test.Fatal(err)
	}
	plan.Release = "lost-delete"
	deleteUncertain := interceptor.NewClient(admin, interceptor.Funcs{Delete: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.DeleteOption) error {
		if err := underlying.Delete(ctx, object, options...); err != nil {
			return err
		}
		return lost
	}})
	deleting, err := installation.Claim(ctx, deleteUncertain, plan, digest)
	if err != nil {
		test.Fatal(err)
	}
	if err := deleting.Retire("aborted-before-mutation"); !errors.Is(err, lost) {
		test.Fatalf("lost delete outcome not preserved: %v", err)
	}
	if err := installation.ConfirmRetired(ctx, admin, plan, digest, deleting.UID()); err != nil {
		test.Fatalf("persisted deletion not recoverable: %v", err)
	}
	if err := installation.ConfirmRetired(ctx, admin, plan, "wrong", deleting.UID()); err == nil {
		test.Fatal("wrong review accepted")
	}
	if _, err := installation.Claim(ctx, admin, plan, digest); err != nil {
		test.Fatal(err)
	}
	if err := installation.ConfirmRetired(ctx, admin, plan, digest, deleting.UID()); err == nil {
		test.Fatal("replacement claim ignored")
	}
	receipts := &corev1.ConfigMapList{}
	if err := admin.List(ctx, receipts, client.InNamespace(plan.AuthorityNamespace)); err != nil {
		test.Fatal(err)
	}
	if len(receipts.Items) == 0 {
		test.Fatal("recovery audit missing")
	}
	for _, receipt := range receipts.Items {
		if !strings.HasPrefix(receipt.Name, "kognition-install-receipt-") {
			continue
		}
		before := receipt.DeepCopy()
		receipt.Data["outcome"] = "changed"
		if err := admin.Update(ctx, &receipt); !apierrors.IsInvalid(err) {
			test.Fatalf("receipt not immutable: %v", err)
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(before), &receipt); err != nil || receipt.Data["outcome"] != before.Data["outcome"] {
			test.Fatalf("receipt changed: %v", err)
		}
	}
}
