// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"fmt"
	"testing"
	"time"
)

// deployedAcceptanceMinimumBudget is the wall-clock time the reviewed-target
// run needs on a loaded shared host: native policies, enforcing-CNI attachment,
// production stdio, ingestion, Agent lifecycle and exact fixture cleanup. Each
// phase keeps its own context; this only prevents the process-wide go test
// alarm from killing the run while temporary privileged grants still exist.
const deployedAcceptanceMinimumBudget = 25 * time.Minute

func checkDeployedAcceptanceBudget(deadline time.Time, hasDeadline bool, now time.Time, minimum time.Duration) error {
	if !hasDeadline {
		return nil
	}
	if remaining := deadline.Sub(now); remaining < minimum {
		return fmt.Errorf("reviewed-target acceptance needs at least %s before the go test deadline but only %s remains; rerun with -timeout 30m so temporary controller grants and fixtures are never left by a killed process", minimum, remaining.Round(time.Second))
	}
	return nil
}

func requireDeployedAcceptanceBudget(test *testing.T) {
	test.Helper()
	deadline, ok := test.Deadline()
	if err := checkDeployedAcceptanceBudget(deadline, ok, time.Now(), deployedAcceptanceMinimumBudget); err != nil {
		test.Fatal(err)
	}
}

func TestDeployedAcceptanceBudgetGuard(test *testing.T) {
	now := time.Date(2026, time.November, 9, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name      string
		deadline  time.Time
		has       bool
		wantError bool
	}{
		{"no deadline", time.Time{}, false, false},
		{"exactly the minimum", now.Add(deployedAcceptanceMinimumBudget), true, false},
		{"thirty minutes", now.Add(30 * time.Minute), true, false},
		{"fifteen minutes is rejected", now.Add(15 * time.Minute), true, true},
		{"already expired", now.Add(-time.Second), true, true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			err := checkDeployedAcceptanceBudget(scenario.deadline, scenario.has, now, deployedAcceptanceMinimumBudget)
			if (err != nil) != scenario.wantError {
				test.Fatalf("budget check error = %v, want error %t", err, scenario.wantError)
			}
		})
	}
}
