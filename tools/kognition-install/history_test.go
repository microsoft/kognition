// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
)

func TestFailedHistoryReview(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		history  string
		revision int
		deployed int
		valid    bool
	}{
		{"failed-install", `[{"revision":1,"status":"failed"}]`, 1, 0, true},
		{"failed-upgrade", `[{"revision":1,"status":"deployed"},{"revision":2,"status":"failed"}]`, 2, 1, true},
		{"unsorted", `[{"revision":3,"status":"failed"},{"revision":1,"status":"deployed"},{"revision":2,"status":"failed"}]`, 3, 1, true},
		{"unreviewed-deployed", `[{"revision":1,"status":"deployed"},{"revision":2,"status":"failed"}]`, 2, 0, false},
		{"advanced", `[{"revision":3,"status":"failed"}]`, 2, 0, false},
		{"missing-current", `[{"revision":1,"status":"failed"}]`, 2, 0, false},
		{"pending", `[{"revision":1,"status":"pending-install"}]`, 1, 0, false},
		{"rollback", `[{"revision":1,"status":"pending-rollback"}]`, 1, 0, false},
		{"duplicate", `[{"revision":1,"status":"failed"},{"revision":1,"status":"failed"}]`, 1, 0, false},
		{"invalid-revision", `[{"revision":0,"status":"failed"}]`, 1, 0, false},
		{"empty", `[]`, 1, 0, false},
		{"malformed", `{`, 1, 0, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			err := validateFailedHistory([]byte(scenario.history), installation.Plan{ReleaseRevision: scenario.revision, PreviousDeployedRevision: scenario.deployed})
			if (err == nil) != scenario.valid {
				test.Fatalf("history review: %v", err)
			}
		})
	}
}
