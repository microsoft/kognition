// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestActivationSnapshotHelmPolicyOwnership(test *testing.T) {
	server := httptest.NewTLSServer(nil)
	certificate := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	server.Close()
	digest := strings.Repeat("a", 64)
	documents := activationDocuments(test,
		"--set", "networkPolicy.enabled=true",
		"--set", "k8sController.image.digest=sha256:"+digest,
		"--set", "webhook.certManager.enabled=false",
		"--set", "webhook.caBundle="+certificate,
		"--set", "admissionGuard.tls.existingSecret=reviewed-guard",
		"--set", "admissionGuard.tls.caBundle="+certificate,
		"--set", "memoryPolicyGateway.secret.create=false",
		"--set", "toolGateway.secret.create=false",
	)
	policyName := ""
	for _, object := range documents {
		if object.GetKind() == "NetworkPolicy" && object.GetLabels()["app.kubernetes.io/component"] == "resource-webhooks" {
			object.SetAnnotations(map[string]string{"reviewed": "true"})
			policyName = object.GetName()
		}
	}
	if policyName == "" {
		test.Fatal("resource admission NetworkPolicy is required")
	}
	content := []byte(encodeActivationDocuments(test, documents))
	candidate, err := activation.ParseCandidate(content, "security-test")
	if err != nil {
		test.Fatal(err)
	}
	prepare := func(admin client.Client) (activation.ActivationTarget, error) {
		return candidate.Snapshot(context.Background(), admin, "https://127.0.0.1:6443", "v1.31.1", digest, "fixture-tool", "authority", 1)
	}
	for _, scenario := range []string{
		"gitops", "helm", "wrong-release", "wrong-namespace", "missing-release", "missing-namespace",
		"empty-release", "extra-annotation", "changed-annotation", "missing-annotation",
		"changed-label", "owner-reference", "changed-policy",
	} {
		test.Run(scenario, func(test *testing.T) {
			admin := activationHelmSnapshotClient(test, documents, policyName, scenario)
			target, err := prepare(admin)
			if scenario != "helm" && scenario != "gitops" {
				if err == nil || !strings.Contains(err.Error(), policyName) {
					test.Fatalf("unreviewed policy ownership or content accepted: %v", err)
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			if target.ClusterUID != "namespace-kube-system" {
				test.Fatalf("snapshot did not bind the target cluster: %s", target.ClusterUID)
			}
			if scenario == "helm" {
				withoutHelm, err := prepare(activationHelmSnapshotClient(test, documents, policyName, "gitops"))
				if err != nil {
					test.Fatalf("preparation mutated the reviewed candidate: %v", err)
				}
				var withHash, withoutHash string
				for _, item := range target.Objects {
					if item.Kind == "NetworkPolicy" && item.Name == policyName {
						withHash = item.SHA256
					}
				}
				for _, item := range withoutHelm.Objects {
					if item.Kind == "NetworkPolicy" && item.Name == policyName {
						withoutHash = item.SHA256
					}
				}
				if withHash == "" || withoutHash == "" || withHash == withoutHash {
					test.Fatal("live Helm ownership must remain bound into the exact activation target")
				}
			}
		})
	}
}

func activationHelmSnapshotClient(test *testing.T, documents []*unstructured.Unstructured, policyName, scenario string) client.Client {
	test.Helper()
	var stored []client.Object
	for _, expected := range documents {
		object := expected.DeepCopy()
		object.SetUID(types.UID(object.GetKind() + "-" + object.GetNamespace() + "-" + object.GetName()))
		object.SetGeneration(1)
		if object.GetKind() == "Deployment" {
			replicas, _, _ := unstructured.NestedInt64(object.Object, "spec", "replicas")
			object.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "replicas": replicas, "updatedReplicas": replicas, "availableReplicas": replicas}
		}
		if object.GetKind() == "ValidatingAdmissionPolicy" {
			object.Object["status"] = map[string]interface{}{"observedGeneration": int64(1), "typeChecking": map[string]interface{}{}}
		}
		if object.GetKind() == "NetworkPolicy" && object.GetName() == policyName {
			annotations := map[string]string{
				"reviewed": "true", "meta.helm.sh/release-name": "security-test", "meta.helm.sh/release-namespace": "control",
			}
			switch scenario {
			case "gitops":
				delete(annotations, "meta.helm.sh/release-name")
				delete(annotations, "meta.helm.sh/release-namespace")
			case "wrong-release":
				annotations["meta.helm.sh/release-name"] = "foreign"
			case "wrong-namespace":
				annotations["meta.helm.sh/release-namespace"] = "foreign"
			case "missing-release":
				delete(annotations, "meta.helm.sh/release-name")
			case "missing-namespace":
				delete(annotations, "meta.helm.sh/release-namespace")
			case "empty-release":
				annotations["meta.helm.sh/release-name"] = ""
			case "extra-annotation":
				annotations["unreviewed"] = "true"
			case "changed-annotation":
				annotations["reviewed"] = "changed"
			case "missing-annotation":
				delete(annotations, "reviewed")
			case "changed-label":
				labels := object.GetLabels()
				labels["unreviewed"] = "true"
				object.SetLabels(labels)
			case "owner-reference":
				object.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign"}})
			case "changed-policy":
				if err := unstructured.SetNestedSlice(object.Object, []interface{}{map[string]interface{}{}}, "spec", "ingress"); err != nil {
					test.Fatal(err)
				}
			}
			object.SetAnnotations(annotations)
		}
		stored = append(stored, object)
	}
	for _, namespace := range []string{"kube-system", "control", "authority", "tenant", "second"} {
		stored = append(stored, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID("namespace-" + namespace)}})
	}
	return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(stored...).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, underlying client.WithWatch, key client.ObjectKey, target client.Object, options ...client.GetOption) error {
			if object, ok := target.(*unstructured.Unstructured); ok {
				for _, item := range stored {
					if original, ok := item.(*unstructured.Unstructured); ok && original.GroupVersionKind() == object.GroupVersionKind() && client.ObjectKeyFromObject(original) == key {
						object.Object = original.DeepCopy().Object
						return nil
					}
				}
			}
			return underlying.Get(ctx, key, target, options...)
		},
		Update: func(_ context.Context, _ client.WithWatch, _ client.Object, options ...client.UpdateOption) error {
			settings := (&client.UpdateOptions{}).ApplyOptions(options)
			if !reflect.DeepEqual(settings.DryRun, []string{"All"}) {
				test.Fatal("snapshot attempted a persistent update")
			}
			return nil
		},
	}).Build()
}
