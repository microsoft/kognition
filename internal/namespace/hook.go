// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package namespace

import (
	"context"
	"encoding/json"

	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/store"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
)

// ReferenceExtractor is a function that extracts cross-namespace references
// from a resource body. Callers register extractors per-kind so the admission
// hook knows how to find references in arbitrary resource types.
type ReferenceExtractor func(obj json.RawMessage) ([]v1alpha1.ResourceReference, error)

// CrossNamespaceHook is a validating admission hook that denies resources with
// cross-namespace references unless a matching CrossNamespaceGrant exists.
type CrossNamespaceHook struct {
	resolver   *Resolver
	extractors map[string]ReferenceExtractor
}

// NewCrossNamespaceHook creates a validating hook backed by the given resolver.
func NewCrossNamespaceHook(resolver *Resolver) *CrossNamespaceHook {
	return &CrossNamespaceHook{
		resolver:   resolver,
		extractors: make(map[string]ReferenceExtractor),
	}
}

// NewProductionCrossNamespaceHook creates the cross-namespace policy used by
// standalone and Kubernetes admission.
func NewProductionCrossNamespaceHook(st store.Store) *CrossNamespaceHook {
	hook := NewCrossNamespaceHook(NewResolver(st))
	hook.RegisterExtractor("Evaluation", extractEvaluationReferences)
	return hook
}

func extractEvaluationReferences(obj json.RawMessage) ([]v1alpha1.ResourceReference, error) {
	var evaluation v1alpha1.Evaluation
	if err := json.Unmarshal(obj, &evaluation); err != nil {
		return nil, err
	}
	return []v1alpha1.ResourceReference{
		{Kind: evaluation.Spec.TargetRef.Kind, Name: evaluation.Spec.TargetRef.Name, Namespace: evaluation.Spec.TargetRef.Namespace},
		{Kind: "EvaluationDataset", Name: evaluation.Spec.DatasetRef.Name, Namespace: evaluation.Spec.DatasetRef.Namespace},
	}, nil
}

// RegisterExtractor registers a reference extractor for a resource kind.
func (h *CrossNamespaceHook) RegisterExtractor(kind string, fn ReferenceExtractor) {
	h.extractors[kind] = fn
}

// Name implements admission.ValidatingHook.
func (h *CrossNamespaceHook) Name() string {
	return "cross-namespace-deny"
}

// Handles implements admission.ValidatingHook.
// Only handles kinds that have registered extractors.
func (h *CrossNamespaceHook) Handles(kind string, op admission.Operation) bool {
	_, ok := h.extractors[kind]
	return ok
}

// Validate implements admission.ValidatingHook.
func (h *CrossNamespaceHook) Validate(ctx context.Context, req *admission.Request) *admission.Response {
	extractor, ok := h.extractors[req.Kind]
	if !ok {
		return &admission.Response{Allowed: true}
	}

	refs, err := extractor(req.Object)
	if err != nil {
		return &admission.Response{
			Allowed: false,
			Reason:  "failed to extract resource references: " + err.Error(),
		}
	}

	if len(refs) == 0 {
		return &admission.Response{Allowed: true}
	}

	if err := h.resolver.CheckReferences(ctx, req.Namespace, refs); err != nil {
		return &admission.Response{
			Allowed: false,
			Reason:  err.Error(),
		}
	}

	return &admission.Response{Allowed: true}
}
