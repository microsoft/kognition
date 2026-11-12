// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/memory"
)

func TestValidateTopKResults(t *testing.T) {
	results := []memory.SearchResult{
		{Record: memory.Record{ID: "first"}, Score: 1},
		{Record: memory.Record{ID: "second"}, Score: 0.5},
	}
	if err := validateTopKResults(results, 2, "first"); err != nil {
		t.Fatalf("valid results: %v", err)
	}
	if err := validateTopKResults(results, 1, "first"); err == nil || !strings.Contains(err.Error(), "at most 1") {
		t.Fatalf("limit error = %v", err)
	}
	if err := validateTopKResults(results, 2, "other"); err == nil || !strings.Contains(err.Error(), "expected other") {
		t.Fatalf("top result error = %v", err)
	}
}

func TestValidateDescendingScores(t *testing.T) {
	if err := validateDescendingScores([]memory.SearchResult{{Score: 1}, {Score: 0.5}, {Score: 0.5}}); err != nil {
		t.Fatalf("descending scores: %v", err)
	}
	if err := validateDescendingScores([]memory.SearchResult{{Score: 0.5}, {Score: 1}}); err == nil || !strings.Contains(err.Error(), "scores not descending") {
		t.Fatalf("ordering error = %v", err)
	}
}

func TestValidateCount(t *testing.T) {
	tests := []struct {
		name     string
		actual   int
		expected int
		mode     countMode
		wantErr  bool
	}{
		{name: "exact", actual: 2, expected: 2, mode: countExact},
		{name: "exact mismatch", actual: 1, expected: 2, mode: countExact, wantErr: true},
		{name: "minimum", actual: 3, expected: 2, mode: countAtLeast},
		{name: "below minimum", actual: 1, expected: 2, mode: countAtLeast, wantErr: true},
		{name: "maximum", actual: 1, expected: 2, mode: countAtMost},
		{name: "above maximum", actual: 3, expected: 2, mode: countAtMost, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCount("records", test.actual, test.expected, test.mode)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateCount() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
