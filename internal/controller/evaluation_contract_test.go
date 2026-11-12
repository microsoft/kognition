// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestEvaluationObjectContract(t *testing.T) {
	obj, err := v1alpha1.NewObject("Evaluation")
	if err != nil {
		t.Fatal(err)
	}
	evaluation, ok := obj.(*v1alpha1.Evaluation)
	if !ok {
		t.Fatalf("NewObject(Evaluation) returned %T", obj)
	}
	evaluation.TypeMeta = v1alpha1.TypeMeta{Kind: "Evaluation", APIVersion: v1alpha1.APIVersion}
	evaluation.ObjectMeta = v1alpha1.ObjectMeta{Name: "quality-gate", Namespace: "default"}
	evaluation.Spec.TargetRef = v1alpha1.EvaluationTargetRef{Kind: "Solution", Name: "assistant"}
	evaluation.Spec.DatasetRef = v1alpha1.EvaluationDatasetRef{Name: "assistant-dataset"}

	if got := evaluation.GetTypeMeta().Kind; got != "Evaluation" {
		t.Fatalf("kind = %q, want Evaluation", got)
	}
	if got := evaluation.GetObjectMeta().Name; got != "quality-gate" {
		t.Fatalf("name = %q, want quality-gate", got)
	}

	data, err := json.Marshal(evaluation)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	if err := store.Create(context.Background(), "Evaluation", "default", "quality-gate", data); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), "Evaluation", "default", "quality-gate")
	if err != nil {
		t.Fatal(err)
	}
	var out v1alpha1.Evaluation
	if err := json.Unmarshal(stored, &out); err != nil {
		t.Fatal(err)
	}
	if out.Spec.TargetRef.Kind != "Solution" || out.Spec.DatasetRef.Name != "assistant-dataset" {
		t.Fatalf("stored evaluation = %#v", out.Spec)
	}
}
