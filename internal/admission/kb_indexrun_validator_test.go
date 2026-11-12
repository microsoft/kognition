// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func indexRunJSON(t *testing.T, mutate func(*v1alpha1.KnowledgeBaseIndexRun)) []byte {
	t.Helper()
	ttl := int32(3600)
	run := v1alpha1.KnowledgeBaseIndexRun{
		Spec: v1alpha1.KnowledgeBaseIndexRunSpec{
			KnowledgeBaseName:       "docs",
			TTLSecondsAfterFinished: &ttl,
		},
	}
	if mutate != nil {
		mutate(&run)
	}
	data, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal index run: %v", err)
	}
	return data
}

func TestKnowledgeBaseIndexRunValidatorContracts(t *testing.T) {
	validator := NewKnowledgeBaseIndexRunValidator()
	if validator.Name() != "knowledgebaseindexrun-validator" {
		t.Fatalf("Name() = %q", validator.Name())
	}
	if !validator.Handles("KnowledgeBaseIndexRun", OperationCreate) ||
		!validator.Handles("KnowledgeBaseIndexRun", OperationUpdate) ||
		validator.Handles("KnowledgeBaseIndexRun", Operation("DELETE")) ||
		validator.Handles("KnowledgeBase", OperationCreate) {
		t.Fatal("Handles() operation or kind routing is incorrect")
	}

	t.Run("allows valid create", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationCreate,
			Object:    indexRunJSON(t, nil),
		})
		if !response.Allowed {
			t.Fatalf("Validate() denied valid run: %s", response.Reason)
		}
	})

	t.Run("allows status-only update", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationUpdate,
			OldObject: indexRunJSON(t, nil),
			Object: indexRunJSON(t, func(run *v1alpha1.KnowledgeBaseIndexRun) {
				run.Status.Phase = v1alpha1.PhaseRunning
			}),
		})
		if !response.Allowed {
			t.Fatalf("Validate() denied status-only update: %s", response.Reason)
		}
	})

	t.Run("aggregates invalid create fields", func(t *testing.T) {
		negative := int32(-1)
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationCreate,
			Object: indexRunJSON(t, func(run *v1alpha1.KnowledgeBaseIndexRun) {
				run.Spec.KnowledgeBaseName = " "
				run.Spec.TTLSecondsAfterFinished = &negative
			}),
		})
		if response.Allowed {
			t.Fatal("Validate() allowed invalid run")
		}
		for _, want := range []string{"knowledgeBaseName is required", "ttlSecondsAfterFinished"} {
			if !strings.Contains(response.Reason, want) {
				t.Fatalf("denial reason %q does not contain %q", response.Reason, want)
			}
		}
	})

	t.Run("rejects malformed object", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationCreate,
			Object:    []byte("{"),
		})
		if response.Allowed || !strings.Contains(response.Reason, "invalid knowledgeBaseIndexRun JSON") {
			t.Fatalf("Validate() response = %#v, want malformed object denial", response)
		}
	})

	t.Run("spec is immutable", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationUpdate,
			OldObject: indexRunJSON(t, nil),
			Object: indexRunJSON(t, func(run *v1alpha1.KnowledgeBaseIndexRun) {
				ttl := int32(60)
				run.Spec.TTLSecondsAfterFinished = &ttl
			}),
		})
		if response.Allowed || !strings.Contains(response.Reason, "spec is immutable") {
			t.Fatalf("Validate() response = %#v, want immutable spec denial", response)
		}
	})

	t.Run("malformed old object fails closed", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationUpdate,
			OldObject: []byte("{"),
			Object:    indexRunJSON(t, nil),
		})
		if response.Allowed || !strings.Contains(response.Reason, "old") {
			t.Fatalf("Validate() response = %#v, want malformed old object denial", response)
		}
	})

	t.Run("missing old object fails closed", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationUpdate,
			Object:    indexRunJSON(t, nil),
		})
		if response.Allowed || !strings.Contains(response.Reason, "old") {
			t.Fatalf("Validate() response = %#v, want missing old object denial", response)
		}
	})
}
