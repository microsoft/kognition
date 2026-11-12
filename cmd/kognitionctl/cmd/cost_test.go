// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/observability"
)

func TestCLICostQueryParsesFilters(t *testing.T) {
	originalNamespace, originalStart, originalEnd := namespace, costStart, costEnd
	originalAgent, originalSolution, originalRevision := costAgentIdentity, costSolution, costSolutionRevision
	originalGroups, originalLimit, originalOffset := costGroupBy, costLimit, costOffset
	t.Cleanup(func() {
		namespace, costStart, costEnd = originalNamespace, originalStart, originalEnd
		costAgentIdentity, costSolution, costSolutionRevision = originalAgent, originalSolution, originalRevision
		costGroupBy, costLimit, costOffset = originalGroups, originalLimit, originalOffset
	})
	namespace = "team-a"
	costStart, costEnd = "2026-08-21T10:00:00Z", "2026-08-21T11:00:00Z"
	costAgentIdentity, costSolution, costSolutionRevision = "spiffe://planner", "research", "3"
	costGroupBy, costLimit, costOffset = "agent,model", 20, 5

	query, err := cliCostQuery()
	if err != nil {
		t.Fatal(err)
	}
	if query.Workspace != "team-a" || query.AgentIdentity != "spiffe://planner" || query.Solution != "research" || query.SolutionRev == nil || *query.SolutionRev != 3 || !query.Start.Equal(time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)) || !query.End.Equal(time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC)) || len(query.GroupBy) != 2 || query.GroupBy[0] != observability.GroupByAgent || query.GroupBy[1] != observability.GroupByModel || query.Limit != 20 || query.Offset != 5 {
		t.Fatalf("query = %#v", query)
	}
}

func TestCLICostQueryRejectsInvalidGrouping(t *testing.T) {
	originalNamespace, originalGroups := namespace, costGroupBy
	t.Cleanup(func() { namespace, costGroupBy = originalNamespace, originalGroups })
	namespace, costGroupBy = "team-a", "unknown"
	if _, err := cliCostQuery(); err == nil {
		t.Fatal("unsupported group-by value was accepted")
	}
}
