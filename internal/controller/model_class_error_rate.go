// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// defaultErrorRatePercent is the error rate that degrades a ModelClass whose
	// healthProbe leaves errorRateThreshold unset.
	defaultErrorRatePercent = 50
	// minCallsForErrorRate is the evidence needed before a rate means anything, so
	// one failed call out of one is not a 100% error rate.
	minCallsForErrorRate = 5
	// errorRateReason is the condition reason of a ModelClass degraded by its
	// error rate.
	errorRateReason = "HighErrorRate"
)

// SetCallHealth enables the error-rate-threshold probe from a store shared with
// the processes that make model calls.
func (r *ModelClassReconciler) SetCallHealth(health store.ModelCallHealthStore) {
	r.callHealth = health
}

// errorRateThreshold returns the configured error rate, in percent.
func errorRateThreshold(probe *v1alpha1.HealthProbeSpec) int32 {
	if probe.ErrorRateThreshold != nil {
		return *probe.ErrorRateThreshold
	}
	return defaultErrorRatePercent
}

// errorRateExceeded reports whether the real model calls served by the class fail
// at or above the probe's error rate. It uses the provider failures recorded for
// the targets of the class's Models in the last few minutes, and only counts as
// exceeded while the latest call to some target failed, so a recovered provider
// stops degrading the class without waiting for old failures to age out.
//
// A class without evidence, or without a shared health store, never exceeds it.
// When the store cannot be read, a degraded class stays degraded and any other
// class stays as it is.
func (r *ModelClassReconciler) errorRateExceeded(ctx context.Context, mc *v1alpha1.ModelClass) (exceeded bool, reason, message string) {
	if r.callHealth == nil {
		return false, "", ""
	}
	targets, err := r.classTargets(ctx, mc)
	if err != nil {
		return r.healthEvidenceUnavailable(mc, err)
	}
	if len(targets) == 0 {
		return false, "", ""
	}
	records, err := r.callHealth.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter)
	if err != nil {
		return r.healthEvidenceUnavailable(mc, err)
	}
	var matching []store.ModelCallHealth
	for _, record := range records {
		if _, ok := targets[record.Target]; ok {
			matching = append(matching, record)
		}
	}
	var calls, faults, running int
	for _, aggregate := range modelhealth.MergeByTarget(matching) {
		calls += aggregate.Calls
		faults += aggregate.Faults
		running += aggregate.ConsecutiveFaults
	}
	threshold := int(errorRateThreshold(mc.Spec.HealthProbe))
	if calls < minCallsForErrorRate || running == 0 || faults*100 < threshold*calls {
		return false, "", ""
	}
	return true, errorRateReason, fmt.Sprintf("error-rate-threshold: %d of %d recent model calls failed (%d%%, threshold %d%%)", faults, calls, faults*100/calls, threshold)
}

// healthEvidenceUnavailable keeps the class as it is when passive health cannot
// be read, so a monitoring outage neither degrades nor heals it.
func (r *ModelClassReconciler) healthEvidenceUnavailable(mc *v1alpha1.ModelClass, err error) (bool, string, string) {
	r.logger.Warn("read passive health for model class", zap.String("modelClass", mc.ObjectMeta.Name), zap.Error(err))
	if mc.Status.Phase == v1alpha1.PhaseDegraded {
		return true, "ErrorRateUnavailable", "error-rate-threshold: recent model call health could not be read"
	}
	return false, "", ""
}

// classTargets returns the health targets of the Models that use the class.
func (r *ModelClassReconciler) classTargets(ctx context.Context, mc *v1alpha1.ModelClass) (map[string]struct{}, error) {
	raws, err := r.store.List(ctx, modelKind, store.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list models of class: %w", err)
	}
	targets := make(map[string]struct{})
	for _, raw := range raws {
		var model v1alpha1.Model
		if json.Unmarshal(raw, &model) != nil || model.Spec.ClassName != mc.ObjectMeta.Name || model.Status.Phase == v1alpha1.PhaseTerminated {
			continue
		}
		targets[modelhealth.TargetKey(mc.Spec.Provisioner, model.Spec.Endpoint, model.Spec.ModelID)] = struct{}{}
	}
	return targets, nil
}
