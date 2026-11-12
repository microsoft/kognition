// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (operations *Kubernetes) verifyCompletedMigration(ctx context.Context, ref ObjectRef, object *unstructured.Unstructured) error {
	if operations.Plan.ReleaseRevision < 1 || operations.Plan.PreviousManifest == nil ||
		ref.APIVersion != "batch/v1" || ref.Kind != "Job" || ref.Namespace != operations.Manifest.Namespace {
		return errors.New("completed migration cleanup requires a previous release Job in the installation namespace")
	}
	prefix := strings.TrimSuffix(operations.Manifest.Deployment, "-k8s-controller")
	prefix = strings.TrimSuffix(prefix[:min(len(prefix), 35)], "-")
	if !regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "-migrations-[a-f0-9]{12}$").MatchString(ref.Name) {
		return errors.New("completed migration cleanup requires a content-addressed migration Job")
	}
	previous, err := ReadArtifact(*operations.Plan.PreviousManifest)
	if err != nil {
		return err
	}
	documents, err := Documents(previous)
	if err != nil {
		return err
	}
	found := false
	for _, old := range documents {
		if old.GetAPIVersion() == ref.APIVersion && old.GetKind() == ref.Kind && old.GetName() == ref.Name &&
			(old.GetNamespace() == ref.Namespace || old.GetNamespace() == "") &&
			old.GetLabels()["app.kubernetes.io/component"] == "migrations" {
			found = true
		}
	}
	if !found {
		return errors.New("completed migration Job is absent from the reviewed previous release")
	}
	next, err := ReadArtifact(operations.Plan.Manifest)
	if err != nil {
		return err
	}
	documents, err = Documents(next)
	if err != nil {
		return err
	}
	for _, object := range documents {
		if object.GetAPIVersion() == ref.APIVersion && object.GetKind() == ref.Kind && object.GetName() == ref.Name &&
			(object.GetNamespace() == ref.Namespace || object.GetNamespace() == "") {
			return errors.New("migration Job is still retained by the new release")
		}
	}
	if err := operations.releaseOwner(object); err != nil {
		return err
	}
	var job batchv1.Job
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &job); err != nil {
		return err
	}
	if job.Labels["app.kubernetes.io/component"] != "migrations" ||
		job.Spec.Template.Labels["app.kubernetes.io/component"] != "migrations" ||
		job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken ||
		(job.Spec.Completions != nil && *job.Spec.Completions != 1) ||
		(job.Spec.Parallelism != nil && *job.Spec.Parallelism != 1) ||
		job.Status.Active != 0 || job.Status.Succeeded != 1 || job.Status.CompletionTime == nil {
		return errors.New("migration Job must be a completed, inactive single execution with API token automount disabled")
	}
	complete := false
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return errors.New("failed migration Jobs cannot be retired")
		}
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			complete = true
		}
	}
	if !complete {
		return errors.New("migration Job has no successful completion condition")
	}
	_, err = operations.migrationPods(ctx, ref)
	return err
}

func (operations *Kubernetes) migrationPods(ctx context.Context, ref ObjectRef) (int, error) {
	var pods corev1.PodList
	if err := operations.Admin.List(ctx, &pods, client.InNamespace(ref.Namespace)); err != nil {
		return 0, err
	}
	count := 0
	for _, pod := range pods.Items {
		for _, owner := range pod.OwnerReferences {
			if owner.UID != types.UID(ref.UID) {
				continue
			}
			if owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.Name != ref.Name ||
				owner.Controller == nil || !*owner.Controller ||
				(pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) {
				return 0, fmt.Errorf("migration Job still has an unconfirmed or nonterminal Pod: %s/%s", pod.Namespace, pod.Name)
			}
			count++
		}
	}
	return count, nil
}

func (operations *Kubernetes) RemoveCompletedMigrations(ctx context.Context) error {
	if err := operations.remove(ctx, operations.Plan.CompletedMigrationJobs, func(ref ObjectRef, object *unstructured.Unstructured) error {
		return operations.verifyCompletedMigration(ctx, ref, object)
	}); err != nil {
		return err
	}
	for _, ref := range operations.Plan.CompletedMigrationJobs {
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			count, err := operations.migrationPods(ctx, ref)
			return count == 0, err
		}); err != nil {
			return fmt.Errorf("confirm retired migration Pods are absent: %w", err)
		}
	}
	return nil
}
