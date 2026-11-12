// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kognition-ai/kognition/internal/memory"
)

func dimensionRow(dimension int) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		*dest[0].(*int) = dimension
		return nil
	}}
}

func volumeDimension(dimension int) func(context.Context, string, ...any) pgx.Row {
	return func(context.Context, string, ...any) pgx.Row { return dimensionRow(dimension) }
}

func TestAdapterSearchDimensionFailures(t *testing.T) {
	database := stubDatabase{queryRow: volumeDimension(384)}
	if _, err := newTestAdapter(database).Search(context.Background(), memory.SearchRequest{
		BackendHandle: "local", Vector: make([]float32, 1536),
	}); err == nil || !strings.Contains(err.Error(), "expects 384 dimensions, got 1536") {
		t.Fatalf("incompatible search must fail before querying vectors: %v", err)
	}
	cause := errors.New("dimension metadata unavailable")
	database.queryRow = func(context.Context, string, ...any) pgx.Row {
		return stubRow{scan: func(...any) error { return cause }}
	}
	if _, err := newTestAdapter(database).Search(context.Background(), memory.SearchRequest{
		Vector: []float32{1},
	}); !errors.Is(err, cause) {
		t.Fatalf("metadata read failure must remain explicit: %v", err)
	}
}

func TestAdapterList(t *testing.T) {
	start := time.Date(2026, time.March, 19, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rows := newStubRows(scanValues("record-a", "content", []float32{0.1, 0.2}, []byte(`{"kind":"fact"}`), end, nil))
	database := stubDatabase{query: func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		for _, fragment := range []string{"volume_id = $1", "created_at >= $2", "created_at <= $3", "metadata @> $4", "ORDER BY created_at DESC", "LIMIT $5"} {
			if !strings.Contains(query, fragment) {
				t.Fatalf("query %q missing %q", query, fragment)
			}
		}
		if len(args) != 5 || args[0] != "volume-a" || args[1] != start || args[2] != end || args[4] != 2 {
			t.Fatalf("args = %#v", args)
		}
		if string(args[3].([]byte)) != `{"kind":"fact"}` {
			t.Fatalf("metadata arg = %s", args[3].([]byte))
		}
		return rows, nil
	}}

	records, err := newTestAdapter(database).List(context.Background(), memory.ListRequest{
		BackendHandle: "volume-a", StartTime: &start, EndTime: &end, Metadata: map[string]string{"kind": "fact"}, Limit: 2,
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != 1 || records[0].ID != "record-a" || len(records[0].Embedding) != 2 || records[0].Metadata["kind"] != "fact" || !rows.closed {
		t.Fatalf("List() records = %#v, closed = %v", records, rows.closed)
	}
}

func TestAdapterListFailures(t *testing.T) {
	databaseError := errors.New("query failed")
	database := stubDatabase{query: func(context.Context, string, ...any) (pgx.Rows, error) { return nil, databaseError }}
	if _, err := newTestAdapter(database).List(context.Background(), memory.ListRequest{}); !errors.Is(err, databaseError) {
		t.Fatalf("List() query error = %v", err)
	}

	for _, test := range []struct {
		name string
		rows *stubRows
	}{
		{name: "scan", rows: newStubRows(func(...any) error { return errors.New("scan failed") })},
		{name: "metadata", rows: newStubRows(scanValues("id", "content", nil, []byte("{"), time.Time{}, nil))},
		{name: "iteration", rows: &stubRows{err: errors.New("stream failed"), index: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := stubDatabase{query: func(context.Context, string, ...any) (pgx.Rows, error) { return test.rows, nil }}
			if _, err := newTestAdapter(database).List(context.Background(), memory.ListRequest{}); err == nil {
				t.Fatal("List() error = nil")
			}
		})
	}
}

func TestAdapterSearch(t *testing.T) {
	if _, err := newTestAdapter(stubDatabase{}).Search(context.Background(), memory.SearchRequest{}); err == nil {
		t.Fatal("Search() empty vector error = nil")
	}

	score := 0.95
	rows := newStubRows(scanValues("record-a", "content", []float32{0.1, 0.2}, []byte(`{"kind":"fact"}`), time.Time{}, &score))
	database := stubDatabase{queryRow: volumeDimension(2), query: func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		for _, fragment := range []string{"embedding <=> $1", "volume_id = $2", "metadata @> $3", "1 - (embedding <=> $1) >= $4", "LIMIT $5"} {
			if !strings.Contains(query, fragment) {
				t.Fatalf("query %q missing %q", query, fragment)
			}
		}
		if len(args) != 5 || args[1] != "volume-a" || args[3] != 0.8 || args[4] != 3 {
			t.Fatalf("args = %#v", args)
		}
		return rows, nil
	}}

	results, err := newTestAdapter(database).Search(context.Background(), memory.SearchRequest{
		BackendHandle: "volume-a", Vector: []float32{0.1, 0.2}, Metadata: map[string]string{"kind": "fact"}, MinScore: 0.8, TopK: 3,
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "record-a" || results[0].Score != score || !rows.closed {
		t.Fatalf("Search() results = %#v, closed = %v", results, rows.closed)
	}
}

func TestAdapterSearchDefaultsAndFailures(t *testing.T) {
	defaultRows := newStubRows()
	database := stubDatabase{queryRow: volumeDimension(1), query: func(_ context.Context, query string, args ...any) (pgx.Rows, error) {
		if !strings.Contains(query, "LIMIT $3") || len(args) != 3 || args[2] != 10 {
			t.Fatalf("default search query = %q, args = %#v", query, args)
		}
		return defaultRows, nil
	}}
	if _, err := newTestAdapter(database).Search(context.Background(), memory.SearchRequest{Vector: []float32{1}}); err != nil {
		t.Fatalf("Search() default error = %v", err)
	}

	databaseError := errors.New("query failed")
	database = stubDatabase{queryRow: volumeDimension(1), query: func(context.Context, string, ...any) (pgx.Rows, error) { return nil, databaseError }}
	if _, err := newTestAdapter(database).Search(context.Background(), memory.SearchRequest{Vector: []float32{1}}); !errors.Is(err, databaseError) {
		t.Fatalf("Search() query error = %v", err)
	}

	for _, test := range []struct {
		name string
		rows *stubRows
	}{
		{name: "scan", rows: newStubRows(func(...any) error { return errors.New("scan failed") })},
		{name: "metadata", rows: newStubRows(scanValues("id", "content", nil, []byte("{"), time.Time{}, new(float64)))},
		{name: "iteration", rows: &stubRows{err: errors.New("stream failed"), index: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := stubDatabase{queryRow: volumeDimension(1), query: func(context.Context, string, ...any) (pgx.Rows, error) { return test.rows, nil }}
			if _, err := newTestAdapter(database).Search(context.Background(), memory.SearchRequest{Vector: []float32{1}}); err == nil {
				t.Fatal("Search() error = nil")
			}
		})
	}
}
