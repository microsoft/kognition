// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func indexScheduleJSON(t *testing.T, mutate func(*v1alpha1.KnowledgeBaseIndexSchedule)) []byte {
	t.Helper()
	schedule := v1alpha1.KnowledgeBaseIndexSchedule{
		Spec: v1alpha1.KnowledgeBaseIndexScheduleSpec{
			KnowledgeBaseName: "docs",
			Schedule:          "0 * * * *",
			ConcurrencyPolicy: v1alpha1.ConcurrencyPolicyForbid,
		},
	}
	if mutate != nil {
		mutate(&schedule)
	}
	data, err := json.Marshal(schedule)
	if err != nil {
		t.Fatalf("marshal schedule: %v", err)
	}
	return data
}

func TestKnowledgeBaseIndexScheduleValidatorContracts(t *testing.T) {
	validator := NewKnowledgeBaseIndexScheduleValidator(nil)
	if validator.Name() != "knowledgebaseindexschedule-validator" {
		t.Fatalf("Name() = %q", validator.Name())
	}
	if !validator.Handles("KnowledgeBaseIndexSchedule", OperationCreate) ||
		!validator.Handles("KnowledgeBaseIndexSchedule", OperationUpdate) ||
		validator.Handles("KnowledgeBaseIndexSchedule", Operation("DELETE")) ||
		validator.Handles("KnowledgeBase", OperationCreate) {
		t.Fatal("Handles() operation or kind routing is incorrect")
	}

	for _, expression := range []string{"0 * * * *", "@daily", "@every 15m"} {
		t.Run("valid "+expression, func(t *testing.T) {
			response := validator.Validate(context.Background(), &Request{
				Operation: OperationCreate,
				Object: indexScheduleJSON(t, func(schedule *v1alpha1.KnowledgeBaseIndexSchedule) {
					schedule.Spec.Schedule = expression
				}),
			})
			if !response.Allowed {
				t.Fatalf("Validate() denied valid schedule: %s", response.Reason)
			}
		})
	}

	t.Run("aggregates invalid fields", func(t *testing.T) {
		negative32 := int32(-1)
		negative64 := int64(-1)
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationCreate,
			Object: indexScheduleJSON(t, func(schedule *v1alpha1.KnowledgeBaseIndexSchedule) {
				schedule.Spec.KnowledgeBaseName = " "
				schedule.Spec.Schedule = "not a cron"
				schedule.Spec.ConcurrencyPolicy = "Parallel"
				schedule.Spec.SuccessfulRunsHistoryLimit = &negative32
				schedule.Spec.FailedRunsHistoryLimit = &negative32
				schedule.Spec.StartingDeadlineSeconds = &negative64
				schedule.Spec.RunTemplate.TTLSecondsAfterFinished = &negative32
			}),
		})
		if response.Allowed {
			t.Fatal("Validate() allowed invalid schedule")
		}
		for _, want := range []string{
			"knowledgeBaseName is required", "not a valid cron expression", "concurrencyPolicy",
			"successfulRunsHistoryLimit", "failedRunsHistoryLimit", "startingDeadlineSeconds",
			"ttlSecondsAfterFinished",
		} {
			if !strings.Contains(response.Reason, want) {
				t.Fatalf("denial reason %q does not contain %q", response.Reason, want)
			}
		}
	})

	t.Run("knowledge base is immutable", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationUpdate,
			OldObject: indexScheduleJSON(t, nil),
			Object: indexScheduleJSON(t, func(schedule *v1alpha1.KnowledgeBaseIndexSchedule) {
				schedule.Spec.KnowledgeBaseName = "other"
			}),
		})
		if response.Allowed || !strings.Contains(response.Reason, "immutable") {
			t.Fatalf("Validate() response = %#v, want immutable denial", response)
		}
	})

	t.Run("malformed old object fails closed", func(t *testing.T) {
		response := validator.Validate(context.Background(), &Request{
			Operation: OperationUpdate,
			OldObject: []byte("{"),
			Object:    indexScheduleJSON(t, nil),
		})
		if response.Allowed || !strings.Contains(response.Reason, "old") {
			t.Fatalf("Validate() response = %#v, want malformed old object denial", response)
		}
	})
}

func TestKnowledgeBaseIndexScheduleValidatorRequiresActiveKnowledgeBase(t *testing.T) {
	ctx := context.Background()
	memoryStore := newMemStore()
	validator := NewKnowledgeBaseIndexScheduleValidator(memoryStore)

	response := validator.Validate(ctx, &Request{Operation: OperationCreate, Namespace: "team-a", Object: indexScheduleJSON(t, nil)})
	if response.Allowed || !strings.Contains(response.Reason, "KnowledgeBase \"docs\" not found") {
		t.Fatalf("missing KnowledgeBase response = %#v", response)
	}

	knowledgeBase := v1alpha1.KnowledgeBase{ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "team-a"}}
	data, err := json.Marshal(knowledgeBase)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Create(ctx, "KnowledgeBase", "team-a", "docs", data); err != nil {
		t.Fatal(err)
	}
	response = validator.Validate(ctx, &Request{Operation: OperationCreate, Namespace: "team-a", Object: indexScheduleJSON(t, nil)})
	if !response.Allowed {
		t.Fatalf("active KnowledgeBase response = %#v", response)
	}
	response = validator.Validate(ctx, &Request{Operation: OperationCreate, Namespace: "team-a", Object: indexScheduleJSON(t, func(schedule *v1alpha1.KnowledgeBaseIndexSchedule) {
		schedule.Spec.KnowledgeBaseName = " docs "
	})})
	if response.Allowed || !strings.Contains(response.Reason, "must not contain leading or trailing whitespace") {
		t.Fatalf("whitespace-padded KnowledgeBase response = %#v", response)
	}

	now := time.Now().UTC()
	knowledgeBase.ObjectMeta.DeletionTimestamp = &now
	data, err = json.Marshal(knowledgeBase)
	if err != nil {
		t.Fatal(err)
	}
	if err := memoryStore.Update(ctx, "KnowledgeBase", "team-a", "docs", data, 0); err != nil {
		t.Fatal(err)
	}
	response = validator.Validate(ctx, &Request{Operation: OperationCreate, Namespace: "team-a", Object: indexScheduleJSON(t, nil)})
	if response.Allowed || !strings.Contains(response.Reason, "is deleting") {
		t.Fatalf("deleting KnowledgeBase response = %#v", response)
	}
}

func TestKnowledgeBaseIndexScheduleValidatorFailsClosedOnLookupError(t *testing.T) {
	validator := NewKnowledgeBaseIndexScheduleValidator(&stubStore{getErr: errors.New("store unavailable")})
	response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Namespace: "team-a", Object: indexScheduleJSON(t, nil)})
	if response.Allowed || !strings.Contains(response.Reason, "failed to get KnowledgeBase \"docs\": store unavailable") {
		t.Fatalf("lookup failure response = %#v", response)
	}
}
