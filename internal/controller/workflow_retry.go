// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"math"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// computeBackoff returns the delay before the next retry attempt for a step,
// using exponential backoff optionally capped by retry.MaxBackoff. attempts is
// the number of attempts already made (>= 1). When MaxBackoff is unset, the
// delay grows unbounded by the multiplier; otherwise it is clamped to that cap.
func computeBackoff(retry *v1alpha1.WorkflowStepRetry, attempts int32) time.Duration {
	if retry == nil {
		return 0
	}
	base, err := time.ParseDuration(retry.Backoff)
	if err != nil || base <= 0 {
		return 0
	}
	mult := retry.BackoffMultiplier
	if mult <= 0 {
		mult = 2.0
	}
	var maxDelay time.Duration = math.MaxInt64
	if retry.MaxBackoff != "" {
		if mb, err := time.ParseDuration(retry.MaxBackoff); err == nil && mb > 0 {
			maxDelay = mb
		}
	}
	exp := math.Pow(mult, float64(attempts-1))
	scaled := float64(base) * exp
	// Guard against overflow / NaN producing undefined Duration values.
	if math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled >= float64(maxDelay) {
		return maxDelay
	}
	delay := time.Duration(scaled)
	if delay <= 0 || delay > maxDelay {
		return maxDelay
	}
	return delay
}

// retryEligible reports whether a failed step should be retried.
// MaxAttempts is the total number of attempts allowed (initial + retries);
// a step that has just completed `attempts` runs is eligible for another
// run iff attempts < MaxAttempts.
func retryEligible(retry *v1alpha1.WorkflowStepRetry, attempts int32) bool {
	if retry == nil {
		return false
	}
	return attempts < retry.MaxAttempts
}

// findStepStatus returns a pointer to the named step status in wf, or nil
// if absent. The returned pointer is a live reference into wf.Status.
func findStepStatus(wf *v1alpha1.Workflow, name string) *v1alpha1.WorkflowStepStatus {
	for i := range wf.Status.StepStatuses {
		if wf.Status.StepStatuses[i].Name == name {
			return &wf.Status.StepStatuses[i]
		}
	}
	return nil
}
