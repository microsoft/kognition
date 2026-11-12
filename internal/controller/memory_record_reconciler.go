// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const memoryRecordKind = "MemoryRecord"

const workspaceKind = "Workspace"

var (
	emailPIIRegex = regexp.MustCompile(`(?i)[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}`)
	phonePIIRegex = regexp.MustCompile(`\+?\d[\d\s()\-]{7,}\d`)
)

// MemoryRecordReconciler reconciles MemoryRecord resources.
//
// On each reconciliation it:
//  1. Validates that all required provenance fields are present
//     (SourceAgentIdentity, SessionID, ReasoningStepID).
//  2. Auto-stamps Provenance.Timestamp to now (UTC) when it is zero.
//  3. Sets ConditionReady=True when all checks pass, or ConditionReady=False
//     with a descriptive message when provenance is incomplete.
type MemoryRecordReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewMemoryRecordReconciler creates a new MemoryRecordReconciler.
func NewMemoryRecordReconciler(s store.Store, logger *zap.Logger) *MemoryRecordReconciler {
	return &MemoryRecordReconciler{store: s, logger: logger}
}

// Reconcile implements controller.Reconciler.
func (r *MemoryRecordReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	logger := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	record, err := r.getRecord(ctx, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get MemoryRecord %s/%s: %w", req.Namespace, req.Name, err)
	}

	// Ensure Provenance is initialised before any further checks.
	if record.Spec.Provenance == nil {
		record.Spec.Provenance = &v1alpha1.MemoryProvenance{}
	}

	// Auto-stamp timestamp when zero (T-0052: write side of read-your-writes).
	if record.Spec.Provenance.Timestamp.IsZero() {
		record.Spec.Provenance.Timestamp = time.Now().UTC()
	}

	// T-0058: redact configured PII patterns at memory-write time.
	redactedContent, piiRedacted := r.redactPII(ctx, req.Namespace, record.Spec.Content)
	if piiRedacted {
		record.Spec.Content = redactedContent
		if record.Spec.Metadata == nil {
			record.Spec.Metadata = map[string]string{}
		}
		record.Spec.Metadata["piiRedacted"] = "true"
		record.Spec.Metadata["piiRuleSource"] = "workspace"
	}

	// Validate required provenance fields (T-0053).
	if msg := missingProvenanceFields(record.Spec.Provenance); msg != "" {
		lc := lifecycle.SetCondition(apiToLifecycleConditions(record.Status.Conditions), lifecycle.ConditionReady, lifecycle.ConditionFalse, "MissingProvenance", msg)
		record.Status.Conditions = lifecycleToAPIConditions(lc)
		logger.Warn("MemoryRecord missing required provenance fields", zap.String("reason", msg))
		return Result{}, r.updateRecord(ctx, record)
	}

	lc := lifecycle.SetCondition(apiToLifecycleConditions(record.Status.Conditions), lifecycle.ConditionReady, lifecycle.ConditionTrue, "ProvenanceVerified", "All provenance fields are present")
	record.Status.Conditions = lifecycleToAPIConditions(lc)
	// Keep summarized records in PhaseSummarized so they do not re-enter
	// the active-record view after provenance reconciliation.
	if record.Status.Phase != v1alpha1.PhaseSummarized {
		record.Status.Phase = v1alpha1.PhaseReady
	}

	logger.Info("MemoryRecord reconciled", zap.String("sessionId", record.Spec.Provenance.SessionID))
	return Result{}, r.updateRecord(ctx, record)
}

func (r *MemoryRecordReconciler) redactPII(ctx context.Context, namespace, content string) (string, bool) {
	rules := map[string]bool{"email": true, "phone": true}

	// Workspace-scoped overrides (T-0058) via annotation:
	// kognition.dev/memory-pii-rules: comma-separated values (email,phone,none).
	rawWorkspace, err := r.store.Get(ctx, workspaceKind, "", namespace)
	if err == nil {
		var ws v1alpha1.Workspace
		if json.Unmarshal(rawWorkspace, &ws) == nil && ws.ObjectMeta.Annotations != nil {
			if raw := strings.TrimSpace(ws.ObjectMeta.Annotations["kognition.dev/memory-pii-rules"]); raw != "" {
				rules = map[string]bool{}
				for _, tok := range strings.Split(raw, ",") {
					switch strings.ToLower(strings.TrimSpace(tok)) {
					case "email":
						rules["email"] = true
					case "phone":
						rules["phone"] = true
					case "none":
						return content, false
					}
				}
			}
		}
	}

	redacted := content
	changed := false
	if rules["email"] {
		next := emailPIIRegex.ReplaceAllString(redacted, "[REDACTED_EMAIL]")
		if next != redacted {
			changed = true
			redacted = next
		}
	}
	if rules["phone"] {
		next := phonePIIRegex.ReplaceAllString(redacted, "[REDACTED_PHONE]")
		if next != redacted {
			changed = true
			redacted = next
		}
	}

	return redacted, changed
}

// missingProvenanceFields returns a human-readable description of which
// required provenance fields are absent, or "" when all are present.
func missingProvenanceFields(p *v1alpha1.MemoryProvenance) string {
	var missing []string
	if p.SourceAgentIdentity == "" {
		missing = append(missing, "sourceAgentIdentity")
	}
	if p.SessionID == "" {
		missing = append(missing, "sessionId")
	}
	if p.ReasoningStepID == "" {
		missing = append(missing, "reasoningStepId")
	}
	if len(missing) == 0 {
		return ""
	}
	msg := "missing required provenance fields:"
	for _, f := range missing {
		msg += " " + f
	}
	return msg
}

// getRecord loads a MemoryRecord from the store.
func (r *MemoryRecordReconciler) getRecord(ctx context.Context, ns, name string) (v1alpha1.MemoryRecord, error) {
	raw, err := r.store.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		return v1alpha1.MemoryRecord{}, err
	}
	var record v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return v1alpha1.MemoryRecord{}, fmt.Errorf("unmarshal MemoryRecord: %w", err)
	}
	return record, nil
}

// updateRecord persists the updated MemoryRecord back to the store.
func (r *MemoryRecordReconciler) updateRecord(ctx context.Context, record v1alpha1.MemoryRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal MemoryRecord: %w", err)
	}
	return r.store.Update(ctx, memoryRecordKind, record.Namespace, record.Name, data, record.ResourceVersion)
}

// ---------------------------------------------------------------------------
// Provenance query API (T-0054)
// ---------------------------------------------------------------------------

// ListRecordsBySession returns all MemoryRecord resources in the given
// namespace whose Provenance.SessionID matches sessionID. Because the store
// write is synchronous, the result is immediately consistent within the same
// session (T-0052 read-your-writes guarantee).
func ListRecordsBySession(ctx context.Context, s store.Store, ns, sessionID string) ([]v1alpha1.MemoryRecord, error) {
	return listRecordsByPredicate(ctx, s, ns, func(r v1alpha1.MemoryRecord) bool {
		return r.Spec.Provenance != nil && r.Spec.Provenance.SessionID == sessionID
	})
}

// ListRecordsByReasoningStep returns all MemoryRecord resources in the given
// namespace whose Provenance.ReasoningStepID matches reasoningStepID.
func ListRecordsByReasoningStep(ctx context.Context, s store.Store, ns, reasoningStepID string) ([]v1alpha1.MemoryRecord, error) {
	return listRecordsByPredicate(ctx, s, ns, func(r v1alpha1.MemoryRecord) bool {
		return r.Spec.Provenance != nil && r.Spec.Provenance.ReasoningStepID == reasoningStepID
	})
}

// GetRecordProvenance returns the MemoryProvenance of the named MemoryRecord,
// or an error when the record is not found.
func GetRecordProvenance(ctx context.Context, s store.Store, ns, name string) (*v1alpha1.MemoryProvenance, error) {
	raw, err := s.Get(ctx, memoryRecordKind, ns, name)
	if err != nil {
		return nil, fmt.Errorf("get MemoryRecord %s/%s: %w", ns, name, err)
	}
	var record v1alpha1.MemoryRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("unmarshal MemoryRecord: %w", err)
	}
	return record.Spec.Provenance, nil
}

// listRecordsByPredicate scans all MemoryRecords in ns and returns those for
// which pred returns true.
func listRecordsByPredicate(ctx context.Context, s store.Store, ns string, pred func(v1alpha1.MemoryRecord) bool) ([]v1alpha1.MemoryRecord, error) {
	raws, err := s.List(ctx, memoryRecordKind, store.ListOptions{Namespace: ns})
	if err != nil {
		return nil, fmt.Errorf("list MemoryRecords in %q: %w", ns, err)
	}
	var out []v1alpha1.MemoryRecord
	for _, raw := range raws {
		var rec v1alpha1.MemoryRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("unmarshal MemoryRecord: %w", err)
		}
		if pred(rec) {
			out = append(out, rec)
		}
	}
	return out, nil
}
