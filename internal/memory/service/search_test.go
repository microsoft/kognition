// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package service

import (
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/memory"
)

func TestRankBM25HandlesEmptyDocumentsAndDeterministicTies(t *testing.T) {
	if results := rankBM25([]memory.Record{{ID: "empty"}}, "query", 1, 0); results != nil {
		t.Fatalf("empty document results = %+v, want nil", results)
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	records := []memory.Record{
		{ID: "weaker", Content: "term other other", Timestamp: base.Add(2 * time.Hour)},
		{ID: "older", Content: "term term", Timestamp: base},
		{ID: "z-id", Content: "term term", Timestamp: base.Add(time.Hour)},
		{ID: "a-id", Content: "term term", Timestamp: base.Add(time.Hour)},
	}
	results := rankBM25(records, "term", 3, 0)
	if len(results) != 3 {
		t.Fatalf("results = %+v, want top three", results)
	}
	if results[0].Record.ID != "a-id" || results[1].Record.ID != "z-id" || results[2].Record.ID != "older" {
		t.Fatalf("tie order = %q, %q, %q", results[0].Record.ID, results[1].Record.ID, results[2].Record.ID)
	}
}

func TestFuseSearchResultsBreaksScoreTiesByIDAndLimits(t *testing.T) {
	semantic := []memory.SearchResult{
		{Record: memory.Record{ID: "z-id"}, Score: 0.9},
		{Record: memory.Record{ID: "a-id"}, Score: 0.8},
	}
	lexical := []memory.SearchResult{
		{Record: memory.Record{ID: "a-id"}, Score: 0.9},
		{Record: memory.Record{ID: "z-id"}, Score: 0.8},
	}
	results := fuseSearchResults(semantic, lexical, 1)
	if len(results) != 1 || results[0].Record.ID != "a-id" {
		t.Fatalf("fused results = %+v, want a-id", results)
	}
}
