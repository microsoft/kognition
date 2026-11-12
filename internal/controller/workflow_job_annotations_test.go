// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

func TestStepJobAnnotationsPreserveWorkflowDefinition(t *testing.T) {
	for _, ownerKind := range []string{"Workflow", "WorkflowRun"} {
		t.Run(ownerKind, func(t *testing.T) {
			workflow := &v1alpha1.Workflow{}
			workflow.ObjectMeta.Name = "research-run-123"
			workflow.ObjectMeta.Annotations = map[string]string{
				observability.AnnotationTraceID:      "0123456789abcdef0123456789abcdef",
				observability.AnnotationRootSpanID:   "0123456789abcdef",
				observability.AnnotationWorkflowName: "research",
			}
			reconciler := &WorkflowReconciler{ownerKind: ownerKind}
			annotations := reconciler.buildStepJobAnnotations(context.Background(), workflow, v1alpha1.WorkflowStep{Name: "answer"}, 0)
			wantWorkflow := workflow.ObjectMeta.Name
			if ownerKind == "WorkflowRun" {
				wantWorkflow = "research"
				if got := annotations[observability.AnnotationWorkflowRun]; got != workflow.ObjectMeta.Name {
					t.Fatalf("workflow run = %q, want %q", got, workflow.ObjectMeta.Name)
				}
			}
			if got := annotations[observability.AnnotationWorkflowName]; got != wantWorkflow {
				t.Fatalf("workflow name = %q, want %q", got, wantWorkflow)
			}
		})
	}
}
