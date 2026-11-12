// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"math"
	"strconv"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

type KnowledgeBaseClaimValidator struct{}

func NewKnowledgeBaseClaimValidator() *KnowledgeBaseClaimValidator {
	return &KnowledgeBaseClaimValidator{}
}

func (*KnowledgeBaseClaimValidator) Name() string { return "knowledgebaseclaim-validator" }

func (*KnowledgeBaseClaimValidator) Handles(kind string, operation Operation) bool {
	return kind == "KnowledgeBaseClaim" && (operation == OperationCreate || operation == OperationUpdate)
}

func (*KnowledgeBaseClaimValidator) Validate(_ context.Context, request *Request) *Response {
	var claim api.KnowledgeBaseClaim
	if json.Unmarshal(request.Object, &claim) != nil {
		return &Response{Allowed: false, Reason: "invalid KnowledgeBaseClaim JSON"}
	}
	if retrieval := claim.Spec.Retrieval; retrieval != nil {
		if retrieval.Mode != "" && retrieval.Mode != "keyword" && retrieval.Mode != "dense" && retrieval.Mode != "hybrid" {
			return &Response{Allowed: false, Reason: "retrieval.mode must be keyword, dense or hybrid"}
		}
		if retrieval.TopK != nil && (*retrieval.TopK < 1 || *retrieval.TopK > 100) {
			return &Response{Allowed: false, Reason: "retrieval.topK must be between 1 and 100"}
		}
		if retrieval.MinScore != "" {
			value, err := strconv.ParseFloat(retrieval.MinScore, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return &Response{Allowed: false, Reason: "retrieval.minScore must be a finite decimal"}
			}
		}
	}
	return &Response{Allowed: true}
}
