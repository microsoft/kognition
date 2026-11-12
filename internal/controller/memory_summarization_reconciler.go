// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// memoryKind is the resource kind string for Memory objects.
const memoryKind = "Memory"

// MemorySummarizationReconciler reconciles Memory resources and triggers
// background summarization of MemoryRecord entries when the active-record
// count exceeds the threshold configured in the associated MemoryClass.
//
// On each reconciliation it:
//  1. Loads the Memory resource.
//  2. Counts the active (non-summarized) MemoryRecords that reference it.
//  3. Resolves the summarization threshold via Memory → MemoryClaim → MemoryClass.
//  4. When the count exceeds the threshold, merges all active records into a
//     new summary MemoryRecord and transitions the source records to PhaseSummarized.
//  5. Updates Memory.Status.RecordCount and sets ConditionReady accordingly.
type MemorySummarizationReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewMemorySummarizationReconciler creates a MemorySummarizationReconciler
// backed by the provided store.
func NewMemorySummarizationReconciler(s store.Store, logger *zap.Logger) *MemorySummarizationReconciler {
	return &MemorySummarizationReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "memory-summarization")),
	}
}

// Reconcile implements controller.Reconciler for Memory resources.
func (r *MemorySummarizationReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	ns := req.Namespace
	memoryName := req.Name

	if req.Kind == memoryRecordKind {
		resolvedMemoryName, skip, err := r.resolveMemoryNameFromRecord(ctx, req.Namespace, req.Name)
		if err != nil {
			return Result{}, fmt.Errorf("resolve Memory from MemoryRecord %s/%s: %w", req.Namespace, req.Name, err)
		}
		if skip {
			return Result{}, nil
		}
		memoryName = resolvedMemoryName
	}

	log := r.logger.With(zap.String("namespace", ns), zap.String("name", memoryName), requestid.ZapField(ctx))

	memory, err := r.getMemory(ctx, ns, memoryName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("Memory not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get Memory %s/%s: %w", ns, memoryName, err)
	}

	// Collect all active (non-summarized) records for this Memory.
	activeRecords, err := ListActiveMemoryRecords(ctx, r.store, ns, memoryName)
	if err != nil {
		return Result{}, fmt.Errorf("list active MemoryRecords for %s/%s: %w", ns, memoryName, err)
	}

	// Resolve summarization threshold from MemoryClass (via MemoryClaim).
	threshold, err := r.getSummarizationThreshold(ctx, ns, memory)
	if err != nil {
		log.Warn("could not resolve summarization threshold, skipping summarization", zap.Error(err))
		// Still update the record count so status stays consistent.
		return Result{}, r.updateMemoryStatus(ctx, memory, int64(len(activeRecords)))
	}

	if threshold <= 0 {
		log.Debug("summarization disabled or threshold not configured, skipping")
		return Result{}, r.updateMemoryStatus(ctx, memory, int64(len(activeRecords)))
	}

	if int64(len(activeRecords)) <= threshold {
		log.Debug("active record count within threshold",
			zap.Int("count", len(activeRecords)),
			zap.Int64("threshold", threshold),
		)
		return Result{}, r.updateMemoryStatus(ctx, memory, int64(len(activeRecords)))
	}

	// Threshold exceeded — run background summarization.
	log.Info("summarization threshold exceeded, summarizing memory",
		zap.Int("activeRecords", len(activeRecords)),
		zap.Int64("threshold", threshold),
	)
	if err := r.summarize(ctx, ns, memoryName, activeRecords, log); err != nil {
		return Result{}, fmt.Errorf("summarize Memory %s/%s: %w", ns, memoryName, err)
	}

	// After summarization: exactly 1 summary record is active.
	return Result{}, r.updateMemoryStatus(ctx, memory, 1)
}

// resolveMemoryNameFromRecord maps a MemoryRecord reconciliation request to
// the owning Memory name via MemoryRecord.Spec.MemoryRef.Name.
//
// Returns skip=true when the triggering MemoryRecord no longer exists (delete
// event) or does not reference a Memory.
func (r *MemorySummarizationReconciler) resolveMemoryNameFromRecord(ctx context.Context, ns, recordName string) (memoryName string, skip bool, err error) {
	raw, err := r.store.Get(ctx, memoryRecordKind, ns, recordName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", true, nil
		}
		return "", false, err
	}

	var rec v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", false, fmt.Errorf("unmarshal MemoryRecord: %w", err)
	}

	if rec.Spec.MemoryRef.Name == "" {
		return "", true, nil
	}

	return rec.Spec.MemoryRef.Name, false, nil
}

// summarize merges all activeRecords into one summary MemoryRecord, then
// marks each source record as PhaseSummarized.
func (r *MemorySummarizationReconciler) summarize(
	ctx context.Context,
	ns, memoryName string,
	activeRecords []v1alpha1.MemoryRecord,
	log *zap.Logger,
) error {
	// Sort oldest-first so the summary reads chronologically.
	sort.Slice(activeRecords, func(i, j int) bool {
		return summarizationTimestamp(activeRecords[i]).Before(summarizationTimestamp(activeRecords[j]))
	})

	summaryContent := buildSummary(activeRecords)
	summaryName := fmt.Sprintf("summary-%s-%d", memoryName, time.Now().UTC().UnixNano())
	sourceNames := make([]string, 0, len(activeRecords))
	for _, rec := range activeRecords {
		sourceNames = append(sourceNames, rec.Name)
	}

	summaryRecord := v1alpha1.MemoryRecord{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       memoryRecordKind,
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      summaryName,
			Namespace: ns,
		},
		Spec: v1alpha1.MemoryRecordSpec{
			MemoryRef: v1alpha1.LocalObjectReference{Name: memoryName},
			Content:   summaryContent,
			Metadata: map[string]string{
				"summary":            "true",
				"recordCount":        fmt.Sprintf("%d", len(activeRecords)),
				"sourceRecords":      strings.Join(sourceNames, ","),
				"derivedFromRecords": strings.Join(sourceNames, ","),
			},
			Provenance: &v1alpha1.MemoryProvenance{
				SourceAgentIdentity:    "system/summarization-controller",
				SessionID:              "system-summarization",
				ReasoningStepID:        "background-summarization",
				Timestamp:              time.Now().UTC(),
				DerivedFromRecordNames: sourceNames,
			},
		},
	}

	data, err := json.Marshal(summaryRecord)
	if err != nil {
		return fmt.Errorf("marshal summary record: %w", err)
	}
	if err := r.store.Create(ctx, memoryRecordKind, ns, summaryName, data); err != nil {
		return fmt.Errorf("create summary record: %w", err)
	}
	log.Info("created summary MemoryRecord",
		zap.String("name", summaryName),
		zap.Int("sourceRecords", len(activeRecords)),
	)

	// Transition source records to PhaseSummarized.
	for i := range activeRecords {
		if err := r.markRecordSummarized(ctx, &activeRecords[i]); err != nil {
			return fmt.Errorf("mark record %q summarized: %w", activeRecords[i].Name, err)
		}
	}
	return nil
}

// buildSummary concatenates record content in chronological order, prefixed
// by a header that identifies how many source records were merged.
func buildSummary(records []v1alpha1.MemoryRecord) string {
	parts := make([]string, 0, len(records))
	for _, rec := range records {
		parts = append(parts, rec.Spec.Content)
	}
	return fmt.Sprintf("[Summary of %d records] %s", len(records), strings.Join(parts, " | "))
}

// summarizationTimestamp returns the Provenance.Timestamp of a record, or
// the zero time when provenance is absent (zero-time records sort first).
func summarizationTimestamp(rec v1alpha1.MemoryRecord) time.Time {
	if rec.Spec.Provenance != nil {
		return rec.Spec.Provenance.Timestamp
	}
	return time.Time{}
}

// markRecordSummarized updates a MemoryRecord's phase to PhaseSummarized.
func (r *MemorySummarizationReconciler) markRecordSummarized(ctx context.Context, rec *v1alpha1.MemoryRecord) error {
	rec.Status.Phase = v1alpha1.PhaseSummarized
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(rec.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"Summarized",
		"Record has been incorporated into a summary",
	)
	rec.Status.Conditions = lifecycleToAPIConditions(lc)
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	return r.store.Update(ctx, memoryRecordKind, rec.Namespace, rec.Name, data, rec.ResourceVersion)
}

// updateMemoryStatus persists updated RecordCount and sets ConditionReady=True.
func (r *MemorySummarizationReconciler) updateMemoryStatus(ctx context.Context, memory *v1alpha1.Memory, recordCount int64) error {
	const (
		reconciledReason  = "Reconciled"
		reconciledMessage = "Memory is within summarization bounds"
	)

	// Avoid no-op writes that can create unnecessary controller churn.
	currentReady := lifecycle.GetCondition(apiToLifecycleConditions(memory.Status.Conditions), lifecycle.ConditionReady)
	if memory.Status.RecordCount == recordCount &&
		currentReady != nil &&
		currentReady.Status == string(lifecycle.ConditionTrue) &&
		currentReady.Reason == reconciledReason &&
		currentReady.Message == reconciledMessage {
		return nil
	}

	memory.Status.RecordCount = recordCount
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(memory.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		reconciledReason,
		reconciledMessage,
	)
	memory.Status.Conditions = lifecycleToAPIConditions(lc)
	return r.updateMemory(ctx, memory)
}

// getSummarizationThreshold resolves the active threshold by walking
// Memory.Spec.ClaimRef → MemoryClaim → MemoryClass.SummarizationConfig.
// Returns 0 when summarization is not configured or is disabled.
func (r *MemorySummarizationReconciler) getSummarizationThreshold(ctx context.Context, ns string, memory *v1alpha1.Memory) (int64, error) {
	claimName := memory.Spec.ClaimRef.Name
	if claimName == "" {
		return 0, nil
	}

	// MemoryClaim is namespace-scoped.
	rawClaim, err := r.store.Get(ctx, memoryClaimKind, ns, claimName)
	if err != nil {
		return 0, fmt.Errorf("get MemoryClaim %s/%s: %w", ns, claimName, err)
	}
	var claim v1alpha1.MemoryClaim
	if err := json.Unmarshal(rawClaim, &claim); err != nil {
		return 0, fmt.Errorf("unmarshal MemoryClaim: %w", err)
	}

	if claim.Spec.MemoryClassName == "" {
		return 0, nil
	}

	// MemoryClass is cluster-scoped (empty namespace).
	rawClass, err := r.store.Get(ctx, memoryClassKind, "", claim.Spec.MemoryClassName)
	if err != nil {
		return 0, fmt.Errorf("get MemoryClass %q: %w", claim.Spec.MemoryClassName, err)
	}
	var mc v1alpha1.MemoryClass
	if err := json.Unmarshal(rawClass, &mc); err != nil {
		return 0, fmt.Errorf("unmarshal MemoryClass: %w", err)
	}

	if mc.Spec.SummarizationConfig == nil || !mc.Spec.SummarizationConfig.Enabled {
		return 0, nil
	}
	return int64(mc.Spec.SummarizationConfig.ThresholdRecords), nil
}

// getMemory loads a Memory resource from the store.
func (r *MemorySummarizationReconciler) getMemory(ctx context.Context, ns, name string) (*v1alpha1.Memory, error) {
	raw, err := r.store.Get(ctx, memoryKind, ns, name)
	if err != nil {
		return nil, err
	}
	var memory v1alpha1.Memory
	if err := json.Unmarshal(raw, &memory); err != nil {
		return nil, fmt.Errorf("unmarshal Memory: %w", err)
	}
	return &memory, nil
}

// updateMemory persists the Memory resource to the store.
func (r *MemorySummarizationReconciler) updateMemory(ctx context.Context, memory *v1alpha1.Memory) error {
	data, err := json.Marshal(memory)
	if err != nil {
		return fmt.Errorf("marshal Memory: %w", err)
	}
	return r.store.Update(ctx, memoryKind, memory.Namespace, memory.Name, data, memory.ResourceVersion)
}

// ---------------------------------------------------------------------------
// T-0057 — Pre/post summarization state visibility query API
// ---------------------------------------------------------------------------

// ListActiveMemoryRecords returns all MemoryRecord resources in ns whose
// MemoryRef references memoryName and that are neither summarized nor deleted.
// Consumers see only the live working set — either original records (before
// summarization) or the merged summary record (after).
func ListActiveMemoryRecords(ctx context.Context, s store.Store, ns, memoryName string) ([]v1alpha1.MemoryRecord, error) {
	return listRecordsByPredicate(ctx, s, ns, func(rec v1alpha1.MemoryRecord) bool {
		return rec.Spec.MemoryRef.Name == memoryName &&
			rec.Status.Phase != v1alpha1.PhaseSummarized &&
			rec.Status.Phase != v1alpha1.PhaseDeleted
	})
}

// ListAllMemoryRecords returns every MemoryRecord in ns whose MemoryRef
// references memoryName, including those with PhaseSummarized. This provides
// access to the full raw history even after background summarization (TC-0044).
func ListAllMemoryRecords(ctx context.Context, s store.Store, ns, memoryName string) ([]v1alpha1.MemoryRecord, error) {
	return listRecordsByPredicate(ctx, s, ns, func(rec v1alpha1.MemoryRecord) bool {
		return rec.Spec.MemoryRef.Name == memoryName
	})
}
