// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCompletedMigrationTransitionRequiresExactReview(t *testing.T) {
	previous := []byte("apiVersion: batch/v1\nkind: Job\nmetadata: {name: release-migrations-0123456789ab, namespace: control}\n")
	if err := ValidateTransition(previous, nil, Plan{}); err == nil {
		t.Fatal("unreviewed migration Job deletion accepted")
	}
	var plan Plan
	if err := json.Unmarshal([]byte(`{"completedMigrationJobs":[{"apiVersion":"batch/v1","kind":"Job","namespace":"control","name":"release-migrations-0123456789ab"}]}`), &plan); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransition(previous, nil, plan); err != nil {
		t.Fatalf("explicitly reviewed completed migration Job cannot be replaced: %v", err)
	}
}

func TestCompletedMigrationCleanupIsBoundedAndFenced(t *testing.T) {
	for _, scenario := range []string{
		"completed", "active", "failed", "no-completion-condition", "unconfirmed-completion",
		"wrong-release", "wrong-namespace", "not-a-migration", "token-mounted",
		"unreviewed-previous-release", "still-retained", "changed-uid", "changed-content", "running-pod", "delete-conflict",
	} {
		t.Run(scenario, func(t *testing.T) {
			completed := metav1.Now()
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name: "release-migrations-0123456789ab", Namespace: "control", UID: "migration-uid", ResourceVersion: "1",
					Labels:      map[string]string{"app.kubernetes.io/managed-by": "Helm", "app.kubernetes.io/component": "migrations"},
					Annotations: map[string]string{"meta.helm.sh/release-name": "release", "meta.helm.sh/release-namespace": "control"},
				},
				Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/component": "migrations"}},
					Spec:       corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false)},
				}},
				Status: batchv1.JobStatus{
					Succeeded: 1, CompletionTime: &completed,
					Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}},
				},
			}
			switch scenario {
			case "active":
				job.Status.Active = 1
			case "failed":
				job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue})
			case "no-completion-condition":
				job.Status.Conditions = nil
			case "unconfirmed-completion":
				job.Status.CompletionTime = nil
			case "wrong-release":
				job.Annotations["meta.helm.sh/release-name"] = "other"
			case "wrong-namespace":
				job.Namespace = "other"
			case "not-a-migration":
				job.Labels["app.kubernetes.io/component"] = "workload"
			case "token-mounted":
				job.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true)
			}
			ref := reviewedReference(t, job, batchv1.SchemeGroupVersion.WithKind("Job"))
			if scenario == "changed-uid" {
				job.UID = "replacement"
			}
			if scenario == "changed-content" {
				job.Labels["changed"] = "after-review"
			}
			previous := []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: release-migrations-0123456789ab\n  namespace: control\n  labels:\n    app.kubernetes.io/component: migrations\n")
			if scenario == "unreviewed-previous-release" {
				previous = nil
			}
			path := filepath.Join(t.TempDir(), "previous.yaml")
			if err := os.WriteFile(path, previous, 0600); err != nil {
				t.Fatal(err)
			}
			artifact := Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(previous))}
			next := []byte{}
			if scenario == "still-retained" {
				next = previous
			}
			nextPath := filepath.Join(t.TempDir(), "next.yaml")
			if err := os.WriteFile(nextPath, next, 0600); err != nil {
				t.Fatal(err)
			}
			nextArtifact := Artifact{Path: nextPath, SHA256: fmt.Sprintf("%x", sha256.Sum256(next))}
			unrelated := job.DeepCopy()
			unrelated.Name, unrelated.UID = "unrelated-job", "unrelated-uid"
			objects := []client.Object{job, unrelated}
			if scenario == "running-pod" {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "migration-pod", Namespace: "control", OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: ptr.To(true),
					}}},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				})
			}
			deletions := 0
			admin := clientfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, base client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					deletions++
					configured := &client.DeleteOptions{}
					configured.ApplyOptions(options)
					if configured.Preconditions == nil || configured.Preconditions.UID == nil ||
						*configured.Preconditions.UID != job.UID || configured.Preconditions.ResourceVersion == nil ||
						*configured.Preconditions.ResourceVersion != job.ResourceVersion {
						t.Fatal("migration cleanup lacks exact UID/resource-version preconditions")
					}
					if scenario == "delete-conflict" {
						return apierrors.NewConflict(schema.GroupResource{Group: "batch", Resource: "jobs"}, object.GetName(), errors.New("replacement raced deletion"))
					}
					return base.Delete(ctx, object, options...)
				}}).Build()
			operations := &Kubernetes{
				Plan:     Plan{Release: "release", ReleaseRevision: 1, Manifest: nextArtifact, PreviousManifest: &artifact, CompletedMigrationJobs: []ObjectRef{ref}},
				Manifest: activation.Manifest{Namespace: "control", Deployment: "release-k8s-controller"},
				Admin:    admin,
			}
			err := operations.RemoveCompletedMigrations(context.Background())
			accepted := scenario == "completed"
			if (err == nil) != accepted {
				t.Fatalf("cleanup result = %v, accepted = %v", err, accepted)
			}
			if !accepted && scenario != "delete-conflict" && deletions != 0 {
				t.Fatal("unconfirmed migration Job reached deletion")
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(unrelated), &batchv1.Job{}); err != nil {
				t.Fatal("unrelated Job changed", err)
			}
			err = admin.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{})
			if accepted && !apierrors.IsNotFound(err) {
				t.Fatalf("reviewed completed migration Job remains: %v", err)
			}
			if !accepted && err != nil {
				t.Fatal("rejected migration cleanup changed the Job", err)
			}
		})
	}
}
