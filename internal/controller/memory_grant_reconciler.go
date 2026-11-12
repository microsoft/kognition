// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// MemoryGrantFinalizer is applied to MemoryGrant resources for cascade cleanup.
	MemoryGrantFinalizer = "kognition.dev/memory-grant-cleanup"
	// MemoryGrantProvenanceMetadataKey identifies the MemoryGrant that
	// authorized a backend memory write.
	MemoryGrantProvenanceMetadataKey = "kognition.dev/memory-grant"
	// MemoryGrantRevocationsAnnotation stores a JSON array of grant names that
	// have been revoked for a MemoryClaim or MemoryRecord.
	MemoryGrantRevocationsAnnotation = "kognition.dev/revoked-memory-grants"

	memoryGrantKind = "MemoryGrant"
)

// MemoryGrantReconciler reconciles MemoryGrant resources.
type MemoryGrantReconciler struct {
	store    store.Store
	logger   *zap.Logger
	auditLog *observability.CognitiveAuditLog
}

// MemoryGrantReconcilerOption configures optional MemoryGrant reconciliation behavior.
type MemoryGrantReconcilerOption func(*MemoryGrantReconciler)

// WithMemoryGrantAuditLog writes durable grant lifecycle and cascade entries.
func WithMemoryGrantAuditLog(auditLog *observability.CognitiveAuditLog) MemoryGrantReconcilerOption {
	return func(reconciler *MemoryGrantReconciler) { reconciler.auditLog = auditLog }
}

// NewMemoryGrantReconciler creates a new MemoryGrantReconciler.
func NewMemoryGrantReconciler(s store.Store, logger *zap.Logger, options ...MemoryGrantReconcilerOption) *MemoryGrantReconciler {
	reconciler := &MemoryGrantReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "memory-grant")),
	}
	for _, option := range options {
		option(reconciler)
	}
	return reconciler
}

// Reconcile implements the Reconciler interface for MemoryGrant resources.
func (r *MemoryGrantReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, memoryGrantKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("memory grant not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get memory grant: %w", err)
	}

	var grant v1alpha1.MemoryGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		return Result{}, fmt.Errorf("unmarshal memory grant: %w", err)
	}

	// Handle deletion with cascade.
	if grant.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &grant, log)
	}

	// Ensure finalizer.
	if !lifecycle.HasFinalizer(grant.ObjectMeta.Finalizers, MemoryGrantFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(grant.ObjectMeta.Finalizers, MemoryGrantFinalizer)
		grant.ObjectMeta.Finalizers = finalizers
		if err := r.updateGrant(ctx, &grant); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	if err := r.recordLifecycleAudit(ctx, &grant, "evaluate", false, nil); err != nil {
		return Result{}, fmt.Errorf("record MemoryGrant evaluation audit: %w", err)
	}

	// Keep structured logs for operator visibility alongside durable audit.
	r.logLifecycle(log, &grant, "evaluated")

	return Result{}, nil
}

// handleDeletion performs cascade cleanup and removes the finalizer.
func (r *MemoryGrantReconciler) handleDeletion(ctx context.Context, grant *v1alpha1.MemoryGrant, log *zap.Logger) (Result, error) {
	log.Info("handling memory grant deletion, cascading revocation")
	if err := r.recordLifecycleAudit(ctx, grant, "revoke", true, nil); err != nil {
		return Result{}, fmt.Errorf("record MemoryGrant revocation audit: %w", err)
	}
	if err := r.cascadeRevocation(ctx, grant); err != nil {
		return Result{}, fmt.Errorf("cascade memory grant revocation: %w", err)
	}

	// Audit the revocation.
	r.logLifecycle(log, grant, "revoked")

	// Remove finalizer.
	finalizers, removed := lifecycle.RemoveFinalizer(grant.ObjectMeta.Finalizers, MemoryGrantFinalizer)
	if removed {
		grant.ObjectMeta.Finalizers = finalizers
		if err := r.updateGrant(ctx, grant); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return Result{}, nil
}

func (r *MemoryGrantReconciler) cascadeRevocation(ctx context.Context, grant *v1alpha1.MemoryGrant) error {
	if err := r.markClaimGrantRevoked(ctx, grant); err != nil {
		return err
	}
	return r.markProvenanceRecordsRevoked(ctx, grant)
}

func (r *MemoryGrantReconciler) markClaimGrantRevoked(ctx context.Context, grant *v1alpha1.MemoryGrant) error {
	claimName := grant.Spec.MemoryClaimRef.Name
	if claimName == "" {
		return nil
	}
	raw, err := r.store.Get(ctx, "MemoryClaim", grant.ObjectMeta.Namespace, claimName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get MemoryClaim %q: %w", claimName, err)
	}
	var claim v1alpha1.MemoryClaim
	if err := json.Unmarshal(raw, &claim); err != nil {
		return fmt.Errorf("unmarshal MemoryClaim %q: %w", claimName, err)
	}
	updated, err := addRevokedMemoryGrant(&claim.ObjectMeta.Annotations, grant.ObjectMeta.Name)
	if err != nil {
		return fmt.Errorf("read MemoryClaim %q revocations: %w", claimName, err)
	}
	if !updated {
		return nil
	}
	if err := r.recordCascadeAudit(ctx, grant, "claim", claim.ObjectMeta.Name); err != nil {
		return err
	}
	data, err := json.Marshal(&claim)
	if err != nil {
		return fmt.Errorf("marshal MemoryClaim %q: %w", claimName, err)
	}
	if err := r.store.Update(ctx, "MemoryClaim", claim.ObjectMeta.Namespace, claim.ObjectMeta.Name, data, claim.ObjectMeta.ResourceVersion); err != nil {
		return fmt.Errorf("mark MemoryClaim %q revoked: %w", claimName, err)
	}
	return nil
}

func (r *MemoryGrantReconciler) markProvenanceRecordsRevoked(ctx context.Context, grant *v1alpha1.MemoryGrant) error {
	rawRecords, err := r.store.List(ctx, memoryRecordKind, store.ListOptions{Namespace: grant.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list MemoryRecords: %w", err)
	}
	records := make(map[string]*v1alpha1.MemoryRecord, len(rawRecords))
	for _, raw := range rawRecords {
		var record v1alpha1.MemoryRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return fmt.Errorf("unmarshal MemoryRecord: %w", err)
		}
		records[record.ObjectMeta.Name] = &record
	}

	revoked := make(map[string]bool)
	for name, record := range records {
		if record.Spec.Provenance != nil && memoryRecordUsesGrant(record, grant) {
			revoked[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, record := range records {
			if revoked[name] || record.Spec.Provenance == nil {
				continue
			}
			for _, parent := range record.Spec.Provenance.DerivedFromRecordNames {
				if revoked[parent] {
					revoked[name] = true
					changed = true
					break
				}
			}
		}
	}

	revokedNames := make([]string, 0, len(revoked))
	for name := range revoked {
		revokedNames = append(revokedNames, name)
	}
	sort.Strings(revokedNames)
	for _, name := range revokedNames {
		record := records[name]
		updated, err := addRevokedMemoryGrant(&record.ObjectMeta.Annotations, grant.ObjectMeta.Name)
		if err != nil {
			return fmt.Errorf("read MemoryRecord %q revocations: %w", name, err)
		}
		if !updated {
			continue
		}
		if err := r.recordCascadeAudit(ctx, grant, "record", name); err != nil {
			return err
		}
		data, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("marshal MemoryRecord %q: %w", name, err)
		}
		if err := r.store.Update(ctx, memoryRecordKind, record.ObjectMeta.Namespace, name, data, record.ObjectMeta.ResourceVersion); err != nil {
			return fmt.Errorf("mark MemoryRecord %q revoked: %w", name, err)
		}
	}
	return nil
}

func memoryRecordUsesGrant(record *v1alpha1.MemoryRecord, grant *v1alpha1.MemoryGrant) bool {
	grantID := record.Spec.Provenance.GrantID
	return grantID == grant.ObjectMeta.Name || grantID == grant.ObjectMeta.Namespace+"/"+grant.ObjectMeta.Name
}

// RevokedMemoryGrantIDs reads the durable revocation tombstone from resource
// annotations. Callers must deny access when the annotation is malformed.
func RevokedMemoryGrantIDs(annotations map[string]string) (map[string]struct{}, error) {
	revoked := make(map[string]struct{})
	if annotations == nil || annotations[MemoryGrantRevocationsAnnotation] == "" {
		return revoked, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(annotations[MemoryGrantRevocationsAnnotation]), &names); err != nil {
		return nil, err
	}
	for _, name := range names {
		if name != "" {
			revoked[name] = struct{}{}
		}
	}
	return revoked, nil
}

func addRevokedMemoryGrant(annotations *map[string]string, grantName string) (bool, error) {
	if grantName == "" {
		return false, nil
	}
	revoked, err := RevokedMemoryGrantIDs(*annotations)
	if err != nil {
		return false, err
	}
	if _, found := revoked[grantName]; found {
		return false, nil
	}
	revoked[grantName] = struct{}{}
	names := make([]string, 0, len(revoked))
	for name := range revoked {
		names = append(names, name)
	}
	sort.Strings(names)
	encoded, err := json.Marshal(names)
	if err != nil {
		return false, err
	}
	if *annotations == nil {
		*annotations = make(map[string]string)
	}
	(*annotations)[MemoryGrantRevocationsAnnotation] = string(encoded)
	return true, nil
}

func (r *MemoryGrantReconciler) recordLifecycleAudit(ctx context.Context, grant *v1alpha1.MemoryGrant, verb string, writeAhead bool, details map[string]any) error {
	if r.auditLog == nil {
		return nil
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal MemoryGrant audit details: %w", err)
	}
	auditKey := fmt.Sprintf("%s/%s|%s|%d|%s|%s", grant.ObjectMeta.Namespace, grant.ObjectMeta.Name, grant.ObjectMeta.UID, grant.ObjectMeta.Generation, verb, encoded)
	entry := observability.AuditEntry{
		ID:            uuid.NewSHA1(uuid.NameSpaceOID, []byte(auditKey)).String(),
		Workspace:     grant.ObjectMeta.Namespace,
		AgentIdentity: grant.Spec.AgentRef.Name,
		EventType:     observability.AuditGrantChange,
		SubjectKind:   memoryGrantKind,
		SubjectRef:    grant.ObjectMeta.Namespace + "/" + grant.ObjectMeta.Name,
		Verb:          verb,
		Details:       encoded,
	}
	if writeAhead {
		return r.auditLog.RecordBeforeExec(ctx, entry)
	}
	return r.auditLog.Record(ctx, entry)
}

func (r *MemoryGrantReconciler) recordCascadeAudit(ctx context.Context, grant *v1alpha1.MemoryGrant, resourceKind, resourceName string) error {
	return r.recordLifecycleAudit(ctx, grant, "revoke-block", true, map[string]any{
		"resourceKind": resourceKind,
		"resourceName": resourceName,
		"grant":        grant.ObjectMeta.Name,
	})
}

// logLifecycle emits a structured log for grant operations.
func (r *MemoryGrantReconciler) logLifecycle(log *zap.Logger, grant *v1alpha1.MemoryGrant, action string) {
	log.Info("memory grant audit",
		zap.String("action", action),
		zap.String("agent", grant.Spec.AgentRef.Name),
		zap.String("memoryClaim", grant.Spec.MemoryClaimRef.Name),
		zap.String("accessMode", grant.Spec.AccessMode),
		zap.String("namespace", grant.ObjectMeta.Namespace),
		zap.Time("timestamp", time.Now()),
	)
}

// updateGrant persists the MemoryGrant to the store.
func (r *MemoryGrantReconciler) updateGrant(ctx context.Context, grant *v1alpha1.MemoryGrant) error {
	data, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("marshal memory grant: %w", err)
	}
	return r.store.Update(ctx, memoryGrantKind, grant.ObjectMeta.Namespace, grant.ObjectMeta.Name, data, grant.ObjectMeta.ResourceVersion)
}

// EvaluateMemoryAccess checks if an agent has been granted access to a memory claim
// with the required access mode. Implements dual-gate: both MemoryGrant and RBAC
// must allow access.
func EvaluateMemoryAccess(ctx context.Context, s store.Store, rbac *RBACEngine, identity *v1alpha1.AgentIdentity, memoryClaimName, requiredMode string) error {
	_, err := EvaluateMemoryAccessDecision(ctx, s, rbac, identity, memoryClaimName, requiredMode)
	return err
}

// MemoryAccessDecision captures the live MemoryGrant selected for an
// authorized data-plane operation.
type MemoryAccessDecision struct {
	GrantName string
}

// EvaluateMemoryAccessDecision applies the same dual gate as
// EvaluateMemoryAccess and returns the deterministic grant provenance for a
// backend write.
func EvaluateMemoryAccessDecision(ctx context.Context, s store.Store, rbac *RBACEngine, identity *v1alpha1.AgentIdentity, memoryClaimName, requiredMode string) (MemoryAccessDecision, error) {
	namespace := identity.ObjectMeta.Namespace
	agentName := identity.Spec.AgentRef.Name

	// Gate 1: MemoryGrant must exist.
	grants, err := s.List(ctx, memoryGrantKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return MemoryAccessDecision{}, fmt.Errorf("list memory grants: %w", err)
	}

	grantName := ""
	for _, raw := range grants {
		var grant v1alpha1.MemoryGrant
		if err := json.Unmarshal(raw, &grant); err != nil {
			continue
		}
		if grant.ObjectMeta.DeletionTimestamp != nil {
			continue
		}
		if grant.Spec.AgentRef.Name == agentName && grant.Spec.MemoryClaimRef.Name == memoryClaimName {
			if accessModePermits(grant.Spec.AccessMode, requiredMode) {
				if grantName == "" || grant.ObjectMeta.Name < grantName {
					grantName = grant.ObjectMeta.Name
				}
			}
		}
	}

	if grantName == "" {
		return MemoryAccessDecision{}, fmt.Errorf("no MemoryGrant found for agent %q to access memory claim %q with mode %q", agentName, memoryClaimName, requiredMode)
	}

	// Gate 2: RBAC must allow access to memory resources.
	if rbac != nil {
		verb := accessModeToVerb(requiredMode)
		allowed, err := rbac.Evaluate(ctx, identity, verb, "Memory")
		if err != nil {
			return MemoryAccessDecision{}, fmt.Errorf("rbac evaluation: %w", err)
		}
		if !allowed {
			return MemoryAccessDecision{}, fmt.Errorf("RBAC denies agent %q %s access to Memory resources", agentName, verb)
		}
	}

	return MemoryAccessDecision{GrantName: grantName}, nil
}

// accessModePermits checks if the granted mode satisfies the required mode.
func accessModePermits(granted, required string) bool {
	if granted == "ReadWrite" {
		return true
	}
	return granted == required
}

// accessModeToVerb converts an access mode to an RBAC verb.
func accessModeToVerb(mode string) string {
	switch mode {
	case "ReadOnly":
		return "get"
	case "ReadWrite":
		return "update"
	default:
		return "get"
	}
}
