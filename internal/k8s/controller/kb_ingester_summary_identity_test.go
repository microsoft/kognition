// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestKBIngesterSummaryRequiresJobIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		mutate        func(*corev1.Pod)
		wantDocuments int64
	}{
		{name: "exact owner and container", wantDocuments: 7},
		{name: "previous Job", mutate: func(pod *corev1.Pod) { pod.OwnerReferences[0].UID = "previous-job" }},
		{name: "unowned Pod", mutate: func(pod *corev1.Pod) { pod.OwnerReferences = nil }},
		{name: "noncontroller owner", mutate: func(pod *corev1.Pod) { pod.OwnerReferences[0].Controller = nil }},
		{name: "other container", mutate: func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].Name = "other" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{batchv1.AddToScheme, corev1.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}
			kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "work", UID: "kb-uid"}}
			job := (&KBIngesterLauncher{image: "ingester:test"}).buildJob(kb, ingestJobName(kb.Name, 1), "input", nil)
			job.UID = "job-uid"
			job.Status.Succeeded = 1
			controller := true
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "completed", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}},
				Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
					Name:  job.Spec.Template.Spec.Containers[0].Name,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"documents":7,"chunks":9}`}},
				}}},
			}
			if scenario.mutate != nil {
				scenario.mutate(pod)
			}
			storage := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, pod).Build()
			launcher := NewKBIngesterLauncher(storage, nil, zap.NewNop(), "ingester:test", "", "control")
			_, stats, err := launcher.Ensure(context.Background(), kb, &api.KnowledgeBaseClass{})
			if err != nil || stats.Documents != scenario.wantDocuments {
				t.Fatalf("Ensure summary documents=%d error=%v, want %d", stats.Documents, err, scenario.wantDocuments)
			}
		})
	}
}
