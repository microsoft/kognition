// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// scheduleParser accepts either standard 5-field cron expressions or the
// @every/@hourly/@daily/@weekly/@monthly/@yearly descriptors.
var scheduleParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// KnowledgeBaseIndexScheduleValidator validates KnowledgeBaseIndexSchedule
// CREATE and UPDATE operations.
type KnowledgeBaseIndexScheduleValidator struct {
	store store.Store
}

// NewKnowledgeBaseIndexScheduleValidator returns a new validator.
func NewKnowledgeBaseIndexScheduleValidator(s store.Store) *KnowledgeBaseIndexScheduleValidator {
	return &KnowledgeBaseIndexScheduleValidator{store: s}
}

func (v *KnowledgeBaseIndexScheduleValidator) Name() string {
	return "knowledgebaseindexschedule-validator"
}

func (v *KnowledgeBaseIndexScheduleValidator) Handles(kind string, op Operation) bool {
	return kind == "KnowledgeBaseIndexSchedule" && (op == OperationCreate || op == OperationUpdate)
}

var validConcurrencyPolicies = map[string]struct{}{
	"":                                {},
	v1alpha1.ConcurrencyPolicyAllow:   {},
	v1alpha1.ConcurrencyPolicyForbid:  {},
	v1alpha1.ConcurrencyPolicyReplace: {},
}

func (v *KnowledgeBaseIndexScheduleValidator) Validate(ctx context.Context, req *Request) *Response {
	var sched v1alpha1.KnowledgeBaseIndexSchedule
	if err := json.Unmarshal(req.Object, &sched); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid knowledgeBaseIndexSchedule JSON: %v", err)}
	}

	var errs []string

	knowledgeBaseName := strings.TrimSpace(sched.Spec.KnowledgeBaseName)
	if knowledgeBaseName == "" {
		errs = append(errs, "spec.knowledgeBaseName is required")
	} else if knowledgeBaseName != sched.Spec.KnowledgeBaseName {
		errs = append(errs, "spec.knowledgeBaseName must not contain leading or trailing whitespace")
	} else if v.store != nil {
		raw, err := v.store.Get(ctx, "KnowledgeBase", req.Namespace, knowledgeBaseName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				errs = append(errs, fmt.Sprintf("KnowledgeBase %q not found", knowledgeBaseName))
			} else {
				errs = append(errs, fmt.Sprintf("failed to get KnowledgeBase %q: %v", knowledgeBaseName, err))
			}
		} else {
			var knowledgeBase v1alpha1.KnowledgeBase
			if err := json.Unmarshal(raw, &knowledgeBase); err != nil {
				errs = append(errs, fmt.Sprintf("invalid KnowledgeBase %q: %v", knowledgeBaseName, err))
			} else if knowledgeBase.ObjectMeta.DeletionTimestamp != nil {
				errs = append(errs, fmt.Sprintf("KnowledgeBase %q is deleting", knowledgeBaseName))
			}
		}
	}

	trimmed := strings.TrimSpace(sched.Spec.Schedule)
	if trimmed == "" {
		errs = append(errs, "spec.schedule is required")
	} else if _, err := scheduleParser.Parse(trimmed); err != nil {
		errs = append(errs, fmt.Sprintf("spec.schedule is not a valid cron expression: %v", err))
	}

	if _, ok := validConcurrencyPolicies[sched.Spec.ConcurrencyPolicy]; !ok {
		errs = append(errs, fmt.Sprintf("spec.concurrencyPolicy %q is invalid (must be Allow, Forbid, or Replace)", sched.Spec.ConcurrencyPolicy))
	}

	if sched.Spec.SuccessfulRunsHistoryLimit != nil && *sched.Spec.SuccessfulRunsHistoryLimit < 0 {
		errs = append(errs, "spec.successfulRunsHistoryLimit must be >= 0")
	}
	if sched.Spec.FailedRunsHistoryLimit != nil && *sched.Spec.FailedRunsHistoryLimit < 0 {
		errs = append(errs, "spec.failedRunsHistoryLimit must be >= 0")
	}
	if sched.Spec.StartingDeadlineSeconds != nil && *sched.Spec.StartingDeadlineSeconds < 0 {
		errs = append(errs, "spec.startingDeadlineSeconds must be >= 0")
	}
	if sched.Spec.RunTemplate.TTLSecondsAfterFinished != nil && *sched.Spec.RunTemplate.TTLSecondsAfterFinished < 0 {
		errs = append(errs, "spec.runTemplate.ttlSecondsAfterFinished must be >= 0")
	}

	if req.Operation == OperationUpdate && len(req.OldObject) > 0 {
		var old v1alpha1.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(req.OldObject, &old); err != nil {
			errs = append(errs, fmt.Sprintf("invalid old knowledgeBaseIndexSchedule JSON: %v", err))
		} else {
			if old.Spec.KnowledgeBaseName != "" && old.Spec.KnowledgeBaseName != sched.Spec.KnowledgeBaseName {
				errs = append(errs, "spec.knowledgeBaseName is immutable")
			}
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}
	return &Response{Allowed: true}
}
