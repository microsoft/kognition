// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/internal/observability"
)

var _ observability.AuditStore = (*Store)(nil)
var _ observability.AuditApplyStore = (*Store)(nil)

// Append persists one immutable CognitiveAuditLog entry before returning.
func (s *Store) Append(ctx context.Context, entry observability.AuditEntry) error {
	_, err := s.ApplyAuditEntry(ctx, entry)
	return err
}

// ApplyAuditEntry persists one immutable entry and reports whether it was new.
func (s *Store) ApplyAuditEntry(ctx context.Context, entry observability.AuditEntry) (bool, error) {
	if entry.Workspace == "" || entry.EventType == "" {
		return false, fmt.Errorf("cognitive audit entry requires workspace and event type")
	}
	if entry.ID == "" {
		entry.ID = uuid.NewString()
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO cognitive_audit_log
		(id, created_at, agent_identity, workspace, solution, solution_rev, session_id, event_type, subject_kind, subject_ref, verb, details, trace_id, span_id, canonical_payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, COALESCE($15, ''::bytea))
		ON CONFLICT (id) DO NOTHING`,
		entry.ID, entry.Timestamp, entry.AgentIdentity, entry.Workspace, entry.Solution, entry.SolutionRev,
		entry.SessionID, entry.EventType, entry.SubjectKind, entry.SubjectRef, entry.Verb, entry.Details,
		entry.TraceID, entry.SpanID, []byte(entry.Details),
	)
	if err != nil {
		return false, fmt.Errorf("append cognitive audit entry: %w", err)
	}
	if result.RowsAffected() == 0 {
		var stored []byte
		if err := s.pool.QueryRow(ctx, `SELECT canonical_payload FROM cognitive_audit_log WHERE id = $1`, entry.ID).Scan(&stored); err != nil {
			return false, fmt.Errorf("load duplicate cognitive audit entry: %w", err)
		}
		if !bytes.Equal(stored, entry.Details) {
			return false, fmt.Errorf("cognitive audit entry ID %q was reused with different content", entry.ID)
		}
		return false, nil
	}
	return true, nil
}

// AppendBatch persists audit entries atomically.
func (s *Store) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	_, err := s.ApplyAuditBatch(ctx, entries)
	return err
}

// ApplyAuditBatch atomically persists entries and reports which were new.
func (s *Store) ApplyAuditBatch(ctx context.Context, entries []observability.AuditEntry) ([]bool, error) {
	if len(entries) == 0 {
		return []bool{}, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin cognitive audit batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	applied := make([]bool, len(entries))
	for index := range entries {
		entry := entries[index]
		if entry.Workspace == "" || entry.EventType == "" {
			return nil, fmt.Errorf("cognitive audit entry %d requires workspace and event type", index)
		}
		if entry.ID == "" {
			entry.ID = uuid.NewString()
		}
		if entry.Timestamp.IsZero() {
			entry.Timestamp = time.Now().UTC()
		}
		result, err := tx.Exec(ctx, `INSERT INTO cognitive_audit_log
			(id, created_at, agent_identity, workspace, solution, solution_rev, session_id, event_type, subject_kind, subject_ref, verb, details, trace_id, span_id, canonical_payload)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, COALESCE($15, ''::bytea))
			ON CONFLICT (id) DO NOTHING`,
			entry.ID, entry.Timestamp, entry.AgentIdentity, entry.Workspace, entry.Solution, entry.SolutionRev,
			entry.SessionID, entry.EventType, entry.SubjectKind, entry.SubjectRef, entry.Verb, entry.Details,
			entry.TraceID, entry.SpanID, []byte(entry.Details),
		)
		if err != nil {
			return nil, fmt.Errorf("append cognitive audit entry %d: %w", index, err)
		}
		applied[index] = result.RowsAffected() > 0
		if !applied[index] {
			var stored []byte
			if err := tx.QueryRow(ctx, `SELECT canonical_payload FROM cognitive_audit_log WHERE id = $1`, entry.ID).Scan(&stored); err != nil {
				return nil, fmt.Errorf("load duplicate cognitive audit entry %d: %w", index, err)
			}
			if !bytes.Equal(stored, entry.Details) {
				return nil, fmt.Errorf("cognitive audit entry ID %q was reused with different content", entry.ID)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit cognitive audit batch: %w", err)
	}
	return applied, nil
}

// Query returns audit entries in deterministic chronological order.
func (s *Store) Query(ctx context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	clauses := make([]string, 0, 6)
	arguments := make([]any, 0, 6)
	addClause := func(clause string, value any) {
		arguments = append(arguments, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(arguments)))
	}
	if filter.Workspace != "" {
		addClause("workspace = $%d", filter.Workspace)
	}
	if filter.AgentIdentity != "" {
		addClause("agent_identity = $%d", filter.AgentIdentity)
	}
	if filter.SessionID != "" {
		addClause("session_id = $%d", filter.SessionID)
	}
	if filter.EventType != "" {
		addClause("event_type = $%d", filter.EventType)
	}
	if len(filter.EventTypes) > 0 {
		placeholders := make([]string, 0, len(filter.EventTypes))
		for _, eventType := range filter.EventTypes {
			arguments = append(arguments, eventType)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(arguments)))
		}
		clauses = append(clauses, "event_type IN ("+strings.Join(placeholders, ", ")+")")
	}
	if filter.SchemaVersion > 0 {
		addClause("details ->> 'schemaVersion' = $%d", strconv.Itoa(filter.SchemaVersion))
	}
	if filter.Plane != "" {
		addClause("details ->> 'plane' = $%d", filter.Plane)
	}
	if filter.Outcome != "" {
		addClause("details ->> 'outcome' = $%d", filter.Outcome)
	}
	if !filter.Since.IsZero() {
		addClause("created_at >= $%d", filter.Since)
	}
	if !filter.Until.IsZero() {
		addClause("created_at <= $%d", filter.Until)
	}
	query := `SELECT id, created_at, agent_identity, workspace, solution, solution_rev, session_id, event_type, subject_kind, subject_ref, verb, details, trace_id, span_id FROM cognitive_audit_log`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	if filter.Descending {
		query += " ORDER BY created_at DESC, id DESC"
	} else {
		query += " ORDER BY created_at, id"
	}
	if filter.Limit > 0 {
		arguments = append(arguments, filter.Limit)
		query += fmt.Sprintf(" LIMIT $%d", len(arguments))
	}
	rows, err := s.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query cognitive audit log: %w", err)
	}
	defer rows.Close()
	entries := make([]observability.AuditEntry, 0)
	for rows.Next() {
		var entry observability.AuditEntry
		if err := rows.Scan(&entry.ID, &entry.Timestamp, &entry.AgentIdentity, &entry.Workspace, &entry.Solution, &entry.SolutionRev, &entry.SessionID, &entry.EventType, &entry.SubjectKind, &entry.SubjectRef, &entry.Verb, &entry.Details, &entry.TraceID, &entry.SpanID); err != nil {
			return nil, fmt.Errorf("scan cognitive audit entry: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
