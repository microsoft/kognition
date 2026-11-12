// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/json"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	intadmission "github.com/kognition-ai/kognition/internal/admission"
)

func TestKnowledgeClaimWebhookValidatesInlineAndGovernedRetrieval(t *testing.T) {
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		for _, scenario := range []struct {
			name      string
			retrieval map[string]any
			allowed   bool
		}{
			{name: "inline claim without retrieval override", allowed: true},
			{name: "governed dense claim", retrieval: map[string]any{"mode": "dense", "topK": 5, "minScore": "0.5"}, allowed: true},
			{name: "unsupported mode", retrieval: map[string]any{"mode": "inline"}, allowed: false},
			{name: "excessive topK", retrieval: map[string]any{"mode": "dense", "topK": 101}, allowed: false},
			{name: "nonfinite score", retrieval: map[string]any{"minScore": "NaN"}, allowed: false},
		} {
			t.Run(string(operation)+"/"+scenario.name, func(t *testing.T) {
				spec := map[string]any{"knowledgeBaseName": "documents", "accessMode": "ReadOnly"}
				if scenario.retrieval != nil {
					spec["retrieval"] = scenario.retrieval
				}
				raw, err := json.Marshal(map[string]any{
					"apiVersion": "kognition.dev/v1alpha1", "kind": "KnowledgeBaseClaim",
					"metadata": map[string]any{"name": "reader", "namespace": "team-a"}, "spec": spec,
				})
				if err != nil {
					t.Fatal(err)
				}
				chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewKnowledgeBaseClaimValidator()})
				handler := newHandlerForMode(chain, testScheme(), testLogger(), modeValidate)
				request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
					Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "KnowledgeBaseClaim"},
					Namespace: "team-a", Name: "reader", Operation: operation, Object: runtime.RawExtension{Raw: raw},
				}}
				if operation == admissionv1.Update {
					request.OldObject = runtime.RawExtension{Raw: raw}
				}
				response := handler.Handle(context.Background(), request)
				if response.Allowed != scenario.allowed {
					t.Fatalf("allowed=%v, want %v: %v", response.Allowed, scenario.allowed, response.Result)
				}
				if response.Result != nil && response.Result.Code >= 500 {
					t.Fatalf("claim conversion failed before declared validation: %v", response.Result)
				}
			})
		}
	}
}
