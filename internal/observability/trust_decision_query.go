// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	TrustDecisionAPIVersion    = "kognition.dev/v1alpha1"
	TrustDecisionSchemaVersion = 1
	DefaultTrustDecisionLimit  = 100
	MaxTrustDecisionLimit      = 500
)

var ErrInvalidTrustDecisionQuery = errors.New("invalid trust decision query")

type TrustDecisionQuery struct {
	Namespace     string
	RunID         string
	AgentIdentity string
	SessionID     string
	Plane         string
	Outcome       string
	Since         time.Time
	Until         time.Time
	Limit         int
}

type TrustDecisionQueryResult struct {
	APIVersion string            `json:"apiVersion"`
	Items      []TrustDecisionV1 `json:"items"`
	Truncated  bool              `json:"truncated"`
}

// ParseTrustDecisionQuery parses the public API filters. Defaults and
// cross-field validation are applied by QueryTrustDecisions.
func ParseTrustDecisionQuery(namespace string, values url.Values) (TrustDecisionQuery, error) {
	query := TrustDecisionQuery{
		Namespace: namespace, RunID: values.Get("runId"), AgentIdentity: values.Get("agentIdentity"), SessionID: values.Get("sessionId"),
		Plane: values.Get("plane"), Outcome: values.Get("outcome"),
	}
	for key, target := range map[string]*time.Time{"since": &query.Since, "until": &query.Until} {
		value := strings.TrimSpace(values.Get(key))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return query, fmt.Errorf("%w: %s must be an RFC3339 timestamp", ErrInvalidTrustDecisionQuery, key)
		}
		*target = parsed
	}
	if value := strings.TrimSpace(values.Get("limit")); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil {
			return query, fmt.Errorf("%w: limit must be an integer", ErrInvalidTrustDecisionQuery)
		}
		query.Limit = limit
	}
	return query, nil
}

// QueryTrustDecisions returns normalized four-plane decisions from any
// audit store with identical validation, filtering, and ordering semantics.
func QueryTrustDecisions(ctx context.Context, auditStore AuditQueryStore, query TrustDecisionQuery, now time.Time) (*TrustDecisionQueryResult, error) {
	if auditStore == nil {
		return nil, errors.New("trust decision audit store is unavailable")
	}
	normalized, err := normalizeTrustDecisionQuery(query, now)
	if err != nil {
		return nil, err
	}
	entries, err := auditStore.Query(ctx, AuditFilter{
		Workspace: normalized.Namespace, AgentIdentity: normalized.AgentIdentity, SessionID: normalized.SessionID,
		EventTypes: []AuditEventType{AuditToolCall, AuditMemoryOp, AuditModelCall, AuditCommunication}, Plane: normalized.Plane, Outcome: normalized.Outcome,
		SchemaVersion: TrustDecisionSchemaVersion, Since: normalized.Since, Until: normalized.Until, Limit: normalized.Limit + 1, Descending: true,
	})
	if err != nil {
		return nil, fmt.Errorf("query trust decision audit store: %w", err)
	}
	decisions := make([]TrustDecisionV1, 0, len(entries))
	for _, entry := range entries {
		if !isTrustDecisionEventType(entry.EventType) {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(entry.Details, &fields); err != nil {
			return nil, fmt.Errorf("decode trust decision audit entry %q: %w", entry.ID, err)
		}
		if _, claimsTrustDecisionContract := fields["schemaVersion"]; !claimsTrustDecisionContract {
			continue
		}
		var decision TrustDecisionV1
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			return nil, fmt.Errorf("decode trust decision audit entry %q: %w", entry.ID, err)
		}
		if err := validateStoredTrustDecision(entry, decision); err != nil {
			return nil, err
		}
		if !trustDecisionMatches(decision, entry, normalized) {
			continue
		}
		decisions = append(decisions, decision)
	}
	sort.Slice(decisions, func(left, right int) bool {
		if decisions[left].Timestamp.Equal(decisions[right].Timestamp) {
			return decisions[left].ID > decisions[right].ID
		}
		return decisions[left].Timestamp.After(decisions[right].Timestamp)
	})
	truncated := len(decisions) > normalized.Limit
	if truncated {
		decisions = decisions[:normalized.Limit]
	}
	return &TrustDecisionQueryResult{APIVersion: TrustDecisionAPIVersion, Items: decisions, Truncated: truncated}, nil
}

func isTrustDecisionEventType(eventType AuditEventType) bool {
	switch eventType {
	case AuditToolCall, AuditMemoryOp, AuditModelCall, AuditCommunication:
		return true
	default:
		return false
	}
}

func normalizeTrustDecisionQuery(query TrustDecisionQuery, now time.Time) (TrustDecisionQuery, error) {
	sinceMissing := query.Since.IsZero()
	untilMissing := query.Until.IsZero()
	query.Namespace = strings.TrimSpace(query.Namespace)
	query.RunID = strings.TrimSpace(query.RunID)
	query.AgentIdentity = strings.TrimSpace(query.AgentIdentity)
	query.SessionID = strings.TrimSpace(query.SessionID)
	query.Plane = strings.TrimSpace(query.Plane)
	query.Outcome = strings.TrimSpace(query.Outcome)
	if query.Limit == 0 {
		query.Limit = DefaultTrustDecisionLimit
	}
	if query.Limit < 1 || query.Limit > MaxTrustDecisionLimit {
		return query, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidTrustDecisionQuery, MaxTrustDecisionLimit)
	}
	if query.Plane != "" && !validTrustDecisionPlane(query.Plane) {
		return query, fmt.Errorf("%w: unsupported plane %q", ErrInvalidTrustDecisionQuery, query.Plane)
	}
	if query.Outcome != "" && !validTrustDecisionOutcome(query.Outcome) {
		return query, fmt.Errorf("%w: unsupported outcome %q", ErrInvalidTrustDecisionQuery, query.Outcome)
	}
	now = now.UTC()
	if query.Until.IsZero() {
		query.Until = now
	}
	if sinceMissing && (query.RunID == "" || !untilMissing) {
		query.Since = query.Until.Add(-24 * time.Hour)
	}
	query.Since = query.Since.UTC()
	query.Until = query.Until.UTC()
	if query.Since.After(query.Until) {
		return query, fmt.Errorf("%w: since must not be after until", ErrInvalidTrustDecisionQuery)
	}
	return query, nil
}

func validateStoredTrustDecision(entry AuditEntry, decision TrustDecisionV1) error {
	if decision.SchemaVersion != TrustDecisionSchemaVersion || decision.ID == "" || decision.Timestamp.IsZero() || decision.Workspace == "" || !validTrustDecisionPlane(decision.Plane) || !validTrustDecisionOutcome(decision.Outcome) {
		return fmt.Errorf("decode trust decision audit entry %q: payload is not a valid Trust Decision v1", entry.ID)
	}
	return nil
}

func trustDecisionMatches(decision TrustDecisionV1, entry AuditEntry, query TrustDecisionQuery) bool {
	if (query.Namespace != "" && decision.Workspace != query.Namespace) || decision.Timestamp.Before(query.Since) || decision.Timestamp.After(query.Until) {
		return false
	}
	if query.RunID != "" && decision.RunID != query.RunID {
		return false
	}
	if query.AgentIdentity != "" && entry.AgentIdentity != query.AgentIdentity {
		return false
	}
	if query.SessionID != "" && entry.SessionID != query.SessionID {
		return false
	}
	if query.Plane != "" && decision.Plane != query.Plane {
		return false
	}
	return query.Outcome == "" || decision.Outcome == query.Outcome
}

func validTrustDecisionPlane(value string) bool {
	switch value {
	case "tool", "memory", "model", "communication":
		return true
	default:
		return false
	}
}

func validTrustDecisionOutcome(value string) bool {
	switch value {
	case "allowed", "denied", "revoked", "replayed", "pending", "approved", "rejected", "completed", "failed":
		return true
	default:
		return false
	}
}
