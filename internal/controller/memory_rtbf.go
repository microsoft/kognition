// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/store"
)

const rtbfTombstoneMessage = "Record tombstoned by right-to-be-forgotten"

// RightToBeForgotten tombstones the source MemoryRecord and every derived
// MemoryRecord in the same Memory by traversing provenance metadata links.
//
// Derived relationships are discovered via metadata keys:
// - derivedFromRecord: <recordName>
// - derivedFromRecords: <recordA,recordB,...>
// - sourceRecords: <recordA,recordB,...>
//
// Returns the list of tombstoned record names (including source).
func RightToBeForgotten(ctx context.Context, s store.Store, ns, sourceRecordName string) ([]string, error) {
	rawSource, err := s.Get(ctx, memoryRecordKind, ns, sourceRecordName)
	if err != nil {
		return nil, fmt.Errorf("get source MemoryRecord %s/%s: %w", ns, sourceRecordName, err)
	}

	var source v1alpha1.MemoryRecord
	if err := json.Unmarshal(rawSource, &source); err != nil {
		return nil, fmt.Errorf("unmarshal source MemoryRecord: %w", err)
	}

	allRecords, err := ListAllMemoryRecords(ctx, s, ns, source.Spec.MemoryRef.Name)
	if err != nil {
		return nil, fmt.Errorf("list MemoryRecords for RTBF: %w", err)
	}

	toVisit := []string{sourceRecordName}
	visited := map[string]bool{}
	tombstoned := make([]string, 0)

	for len(toVisit) > 0 {
		cur := toVisit[0]
		toVisit = toVisit[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true

		rec, ok := findRecordByName(allRecords, cur)
		if !ok {
			continue
		}

		if err := tombstoneRecord(ctx, s, &rec, sourceRecordName); err != nil {
			return nil, fmt.Errorf("tombstone record %q: %w", rec.Name, err)
		}
		tombstoned = append(tombstoned, rec.Name)

		for _, candidate := range allRecords {
			if candidate.Name == rec.Name {
				continue
			}
			if referencesRecord(candidate, rec.Name) {
				toVisit = append(toVisit, candidate.Name)
			}
		}
	}

	return tombstoned, nil
}

func findRecordByName(records []v1alpha1.MemoryRecord, name string) (v1alpha1.MemoryRecord, bool) {
	for _, rec := range records {
		if rec.Name == name {
			return rec, true
		}
	}
	return v1alpha1.MemoryRecord{}, false
}

func referencesRecord(rec v1alpha1.MemoryRecord, targetName string) bool {
	if rec.Spec.Provenance != nil {
		for _, parent := range rec.Spec.Provenance.DerivedFromRecordNames {
			if strings.TrimSpace(parent) == targetName {
				return true
			}
		}
	}

	if rec.Spec.Metadata == nil {
		return false
	}

	if strings.TrimSpace(rec.Spec.Metadata["derivedFromRecord"]) == targetName {
		return true
	}

	for _, key := range []string{"derivedFromRecords", "sourceRecords"} {
		if raw := strings.TrimSpace(rec.Spec.Metadata[key]); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if strings.TrimSpace(part) == targetName {
					return true
				}
			}
		}
	}

	return false
}

func tombstoneRecord(ctx context.Context, s store.Store, rec *v1alpha1.MemoryRecord, sourceRecordName string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if isRTBFTombstonedBy(rec, sourceRecordName) {
			return ensureRTBFAuditEvent(ctx, s, rec, sourceRecordName)
		}
		applyRTBFTombstone(rec, sourceRecordName)
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("marshal MemoryRecord: %w", err)
		}
		if err := s.Update(ctx, memoryRecordKind, rec.Namespace, rec.Name, data, rec.ResourceVersion); err != nil {
			if !errors.Is(err, store.ErrConflict) {
				return err
			}
			raw, getErr := s.Get(ctx, memoryRecordKind, rec.Namespace, rec.Name)
			if getErr != nil {
				return fmt.Errorf("re-read MemoryRecord after conflict: %w", getErr)
			}
			var latest v1alpha1.MemoryRecord
			if getErr := json.Unmarshal(raw, &latest); getErr != nil {
				return fmt.Errorf("unmarshal MemoryRecord after conflict: %w", getErr)
			}
			*rec = latest
			continue
		}
		return appendRTBFAuditEvent(ctx, s, rec, sourceRecordName)
	}
	return store.ErrConflict
}

func applyRTBFTombstone(rec *v1alpha1.MemoryRecord, sourceRecordName string) {
	rec.Spec.Content = "[TOMBSTONED]"
	if rec.Spec.Metadata == nil {
		rec.Spec.Metadata = map[string]string{}
	}
	rec.Spec.Metadata["rtbfTombstoned"] = "true"
	rec.Spec.Metadata["rtbfSourceRecord"] = sourceRecordName
	rec.Status.Phase = v1alpha1.PhaseDeleted
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(rec.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"RightToBeForgotten",
		rtbfTombstoneMessage,
	)
	rec.Status.Conditions = lifecycleToAPIConditions(lc)
}

func isRTBFTombstonedBy(rec *v1alpha1.MemoryRecord, sourceRecordName string) bool {
	return rec.Spec.Metadata != nil &&
		rec.Spec.Metadata["rtbfTombstoned"] == "true" &&
		rec.Spec.Metadata["rtbfSourceRecord"] == sourceRecordName
}

func ensureRTBFAuditEvent(ctx context.Context, s store.Store, rec *v1alpha1.MemoryRecord, sourceRecordName string) error {
	events, err := s.ListEvents(ctx, memoryRecordKind, rec.Namespace, rec.Name)
	if err != nil {
		return fmt.Errorf("list RTBF audit events: %w", err)
	}
	for _, event := range events {
		if event.Type != "RTBF_TOMBSTONED" {
			continue
		}
		var details map[string]string
		if json.Unmarshal(event.Details, &details) == nil && details["sourceRecord"] == sourceRecordName {
			return nil
		}
	}
	return appendRTBFAuditEvent(ctx, s, rec, sourceRecordName)
}

func appendRTBFAuditEvent(ctx context.Context, s store.Store, rec *v1alpha1.MemoryRecord, sourceRecordName string) error {
	details, _ := json.Marshal(map[string]string{
		"sourceRecord": sourceRecordName,
		"record":       rec.Name,
	})
	if err := s.AppendEvent(ctx, memoryRecordKind, rec.Namespace, rec.Name, store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      "RTBF_TOMBSTONED",
		Reason:    "RightToBeForgotten",
		Message:   rtbfTombstoneMessage,
		Details:   details,
	}); err != nil {
		return fmt.Errorf("append RTBF audit event: %w", err)
	}

	return nil
}
