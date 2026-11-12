// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
)

func (s *Server) handleQueryCosts(w http.ResponseWriter, r *http.Request) {
	if s.costStore == nil {
		writeError(w, http.StatusServiceUnavailable, "cost aggregation is not configured")
		return
	}
	query, err := parseCostQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.costStore.QueryCosts(r.Context(), query)
	if err != nil {
		s.logger.Error("query costs failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "cost aggregation failed")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseCostQuery(r *http.Request) (observability.CostQuery, error) {
	values := r.URL.Query()
	query := observability.CostQuery{
		Workspace:     r.PathValue("namespace"),
		AgentIdentity: values.Get("agentIdentity"),
		Solution:      values.Get("solution"),
	}
	if query.Workspace == "" {
		return observability.CostQuery{}, fmt.Errorf("namespace is required")
	}
	var err error
	if query.Start, err = parseCostTime(values.Get("start")); err != nil {
		return observability.CostQuery{}, fmt.Errorf("invalid start: %w", err)
	}
	if query.End, err = parseCostTime(values.Get("end")); err != nil {
		return observability.CostQuery{}, fmt.Errorf("invalid end: %w", err)
	}
	if !query.Start.IsZero() && !query.End.IsZero() && query.End.Before(query.Start) {
		return observability.CostQuery{}, fmt.Errorf("end must not be before start")
	}
	if value := values.Get("solutionRevision"); value != "" {
		revision, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil || revision < 0 {
			return observability.CostQuery{}, fmt.Errorf("solutionRevision must be a non-negative integer")
		}
		query.SolutionRev = &revision
	}
	if query.Limit, err = parseCostPageValue(values.Get("limit"), "limit"); err != nil {
		return observability.CostQuery{}, err
	}
	if query.Offset, err = parseCostPageValue(values.Get("offset"), "offset"); err != nil {
		return observability.CostQuery{}, err
	}
	if query.GroupBy, err = parseCostGroups(values.Get("groupBy")); err != nil {
		return observability.CostQuery{}, err
	}
	return query, nil
}

func parseCostTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func parseCostPageValue(value, name string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func parseCostGroups(value string) ([]observability.CostGroupBy, error) {
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	groups := make([]observability.CostGroupBy, 0, len(parts))
	seen := make(map[observability.CostGroupBy]struct{}, len(parts))
	for _, part := range parts {
		group := observability.CostGroupBy(strings.TrimSpace(part))
		switch group {
		case observability.GroupByWorkspace, observability.GroupByAgent, observability.GroupBySolution, observability.GroupByModel, observability.GroupByOperationType:
		default:
			return nil, fmt.Errorf("unsupported groupBy value %q", part)
		}
		if _, exists := seen[group]; exists {
			return nil, fmt.Errorf("duplicate groupBy value %q", part)
		}
		seen[group] = struct{}{}
		groups = append(groups, group)
	}
	return groups, nil
}
