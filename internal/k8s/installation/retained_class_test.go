// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestControlledDrainPreservesOnlyExactUnchangedReviewedDefaultMemoryClass(t *testing.T) {
	for _, scenario := range []string{"reviewed", "unreviewed", "replacement", "changed-live-spec", "changed-next-spec", "wrong-release", "finalizer", "deleting", "foreign-class", "missing-prior"} {
		t.Run(scenario, func(t *testing.T) {
			object := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kognition.dev/v1alpha1", "kind": "MemoryClass",
				"metadata": map[string]any{
					"name": "default", "uid": "default-class-uid",
					"labels":      map[string]any{"app.kubernetes.io/managed-by": "Helm"},
					"annotations": map[string]any{"meta.helm.sh/release-name": "kognition", "meta.helm.sh/release-namespace": "control"},
				},
				"spec": map[string]any{"backendType": "pgvector", "reclaimPolicy": "Retain"},
			}}
			artifact := func(name string, source *unstructured.Unstructured) Artifact {
				t.Helper()
				content, err := json.Marshal(source)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), name+".json")
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(content)
				return Artifact{Path: path, SHA256: hex.EncodeToString(digest[:])}
			}
			prior := artifact("prior", object)
			nextObject := object.DeepCopy()
			if scenario == "changed-next-spec" {
				if err := unstructured.SetNestedField(nextObject.Object, "redis", "spec", "backendType"); err != nil {
					t.Fatal(err)
				}
			}
			next := artifact("next", nextObject)
			reference := reviewedReference(t, object, schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "MemoryClass"})
			switch scenario {
			case "replacement":
				object.SetUID("replacement")
			case "changed-live-spec":
				if err := unstructured.SetNestedField(object.Object, "redis", "spec", "backendType"); err != nil {
					t.Fatal(err)
				}
			case "wrong-release":
				object.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "foreign", "meta.helm.sh/release-namespace": "control"})
			case "finalizer":
				object.SetFinalizers([]string{"cleanup"})
			case "deleting":
				stamp := metav1.Now()
				object.SetDeletionTimestamp(&stamp)
				object.SetFinalizers([]string{"cleanup"})
			case "foreign-class":
				object.SetName("foreign")
			}
			localScheme := runtime.NewScheme()
			if err := scheme.AddToScheme(localScheme); err != nil {
				t.Fatal(err)
			}
			localScheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "MemoryClassList"}, &unstructured.UnstructuredList{})
			admin := clientfake.NewClientBuilder().WithScheme(localScheme).WithObjects(object).Build()
			discovery := &preferredDiscovery{FakeDiscovery: &fake.FakeDiscovery{Fake: &k8stesting.Fake{}}, resources: []*metav1.APIResourceList{{
				GroupVersion: "kognition.dev/v1alpha1", APIResources: []metav1.APIResource{{Name: "memoryclasses", Kind: "MemoryClass", Verbs: metav1.Verbs{"list"}}},
			}}}
			plan := Plan{Release: "kognition", ReleaseRevision: 1, PreviousManifest: &prior, Manifest: next, RetainedObjects: []ObjectRef{reference}}
			if scenario == "unreviewed" {
				plan.RetainedObjects = nil
			}
			if scenario == "missing-prior" {
				plan.PreviousManifest = nil
			}
			operations := &Kubernetes{Admin: admin, Discovery: discovery, Plan: plan, Manifest: activation.Manifest{Namespace: "control", Namespaces: []string{"tenant"}}}
			err := operations.WorkloadsDrained(context.Background())
			if (err == nil) != (scenario == "reviewed") {
				t.Fatalf("scenario=%s error=%v", scenario, err)
			}
			actual := &unstructured.Unstructured{}
			actual.SetGroupVersionKind(object.GroupVersionKind())
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(object), actual); err != nil {
				t.Fatal("drain altered the retained class")
			}
		})
	}
}
