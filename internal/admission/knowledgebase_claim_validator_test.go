// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission_test

import (
	"context"
	"encoding/json"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
)

func TestKnowledgeClaimRetrievalIntentRejectsUnencodableValues(t *testing.T) {
	for _, test := range []struct {
		mode, score string
		top         int32
		allowed     bool
	}{
		{mode: "dense", score: "0.4", top: 3, allowed: true},
		{mode: "hybrid", top: 100, allowed: true},
		{mode: "keyword", top: 1, allowed: true},
		{mode: "automatic", top: 3},
		{mode: "dense", score: "NaN", top: 3},
		{mode: "dense", score: "+Inf", top: 3},
		{mode: "dense", top: 101},
	} {
		claim := api.KnowledgeBaseClaim{Spec: api.KnowledgeBaseClaimSpec{Retrieval: &api.KBRetrievalOverride{Mode: test.mode, MinScore: test.score, TopK: &test.top}}}
		body, err := json.Marshal(claim)
		if err != nil {
			t.Fatal(err)
		}
		response := admission.NewKnowledgeBaseClaimValidator().Validate(context.Background(), &admission.Request{Object: body})
		if response.Allowed != test.allowed {
			t.Fatalf("mode=%s score=%s top=%d response=%+v", test.mode, test.score, test.top, response)
		}
	}
}
