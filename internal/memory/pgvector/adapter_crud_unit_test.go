// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory"
)

func TestNew(t *testing.T) {
	pool := &pgxpool.Pool{}
	logger := zap.NewNop()
	adapter := New(pool, logger)
	if adapter.pool != pool || adapter.logger != logger {
		t.Fatalf("New() = %#v", adapter)
	}
}

func TestAdapterProvision(t *testing.T) {
	for _, dimension := range []int{0, 384, 1536, 16000} {
		database := stubDatabase{queryRow: func(_ context.Context, _ string, args ...any) pgx.Row {
			expected := dimension
			if expected == 0 {
				expected = 1536
			}
			if args[0] != "volume-a" || args[1] != expected {
				t.Fatalf("provision arguments = %#v", args)
			}
			return dimensionRow(expected)
		}}
		if err := newTestAdapter(database).Provision(context.Background(), memory.ProvisionRequest{
			BackendHandle: "volume-a", EmbeddingDimension: dimension,
		}); err != nil {
			t.Fatalf("Provision(%d) error = %v", dimension, err)
		}
	}
	for _, dimension := range []int{-1, 16001} {
		if err := newTestAdapter(stubDatabase{}).Provision(context.Background(), memory.ProvisionRequest{
			EmbeddingDimension: dimension,
		}); err == nil {
			t.Fatalf("Provision(%d) must fail before database access", dimension)
		}
	}
	for _, cause := range []error{pgx.ErrNoRows, errors.New("database unavailable")} {
		database := stubDatabase{queryRow: func(context.Context, string, ...any) pgx.Row {
			return stubRow{scan: func(...any) error { return cause }}
		}}
		if err := newTestAdapter(database).Provision(context.Background(), memory.ProvisionRequest{}); err == nil {
			t.Fatalf("Provision() must report %v", cause)
		}
	}
}

func TestAdapterStore(t *testing.T) {
	timestamp := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)
	database := stubDatabase{exec: func(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
		if !strings.Contains(query, "INSERT INTO memory_vectors") || !strings.Contains(query, "ON CONFLICT") {
			t.Fatalf("query = %q", query)
		}
		if len(args) != 6 || args[0] != "record-a" || args[1] != "volume-a" || args[2] != "content" {
			t.Fatalf("args = %#v", args)
		}
		vector, ok := args[3].(*pgvector.Vector)
		if !ok || !reflect.DeepEqual(vector.Slice(), []float32{0.1, 0.2}) {
			t.Fatalf("embedding = %#v", args[3])
		}
		if string(args[4].([]byte)) != `{"kind":"fact"}` {
			t.Fatalf("metadata = %s", args[4].([]byte))
		}
		if args[5] != timestamp {
			t.Fatalf("timestamp = %v, want %v", args[5], timestamp)
		}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	}}

	response, err := newTestAdapter(database).Store(context.Background(), memory.StoreRequest{
		BackendHandle: "volume-a",
		ID:            "record-a",
		Content:       "content",
		Embedding:     []float32{0.1, 0.2},
		Metadata:      map[string]string{"kind": "fact"},
		Timestamp:     timestamp,
	})
	if err != nil {
		t.Fatalf("Store() error = %v", err)
	}
	if response.ID != "record-a" {
		t.Fatalf("Store() ID = %q", response.ID)
	}
}

func TestAdapterStoreDefaultsAndFailure(t *testing.T) {
	databaseError := errors.New("write failed")
	database := stubDatabase{exec: func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
		if args[3] != (*pgvector.Vector)(nil) {
			t.Fatalf("embedding = %#v, want nil", args[3])
		}
		if args[5].(time.Time).IsZero() {
			t.Fatal("default timestamp is zero")
		}
		return pgconn.CommandTag{}, databaseError
	}}

	_, err := newTestAdapter(database).Store(context.Background(), memory.StoreRequest{ID: "record-a"})
	if !errors.Is(err, databaseError) || !strings.Contains(err.Error(), "store record") {
		t.Fatalf("Store() error = %v", err)
	}
}

func TestAdapterGet(t *testing.T) {
	timestamp := time.Date(2026, time.March, 20, 12, 0, 0, 0, time.UTC)
	database := stubDatabase{queryRow: func(_ context.Context, query string, args ...any) pgx.Row {
		if !strings.Contains(query, "WHERE id = $1 AND volume_id = $2") || !reflect.DeepEqual(args, []any{"record-a", "volume-a"}) {
			t.Fatalf("query = %q, args = %#v", query, args)
		}
		return stubRow{scan: scanValues("record-a", "content", []float32{0.1, 0.2}, []byte(`{"kind":"fact"}`), timestamp, nil)}
	}}

	record, err := newTestAdapter(database).Get(context.Background(), memory.GetRequest{ID: "record-a", BackendHandle: "volume-a"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if record.ID != "record-a" || record.Content != "content" || !reflect.DeepEqual(record.Embedding, []float32{0.1, 0.2}) || record.Metadata["kind"] != "fact" || record.Timestamp != timestamp {
		t.Fatalf("Get() record = %#v", record)
	}
}

func TestAdapterGetFailures(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		database := stubDatabase{queryRow: func(context.Context, string, ...any) pgx.Row {
			return stubRow{scan: func(...any) error { return pgx.ErrNoRows }}
		}}
		_, err := newTestAdapter(database).Get(context.Background(), memory.GetRequest{})
		if !errors.Is(err, memory.ErrRecordNotFound) {
			t.Fatalf("Get() error = %v, want ErrRecordNotFound", err)
		}
	})

	t.Run("scan failure", func(t *testing.T) {
		scanError := errors.New("decode failed")
		database := stubDatabase{queryRow: func(context.Context, string, ...any) pgx.Row {
			return stubRow{scan: func(...any) error { return scanError }}
		}}
		_, err := newTestAdapter(database).Get(context.Background(), memory.GetRequest{})
		if !errors.Is(err, scanError) || !strings.Contains(err.Error(), "scan record") {
			t.Fatalf("Get() error = %v", err)
		}
	})

	t.Run("malformed metadata", func(t *testing.T) {
		database := stubDatabase{queryRow: func(context.Context, string, ...any) pgx.Row {
			return stubRow{scan: scanValues("id", "content", nil, []byte("{"), time.Time{}, nil)}
		}}
		_, err := newTestAdapter(database).Get(context.Background(), memory.GetRequest{})
		if err == nil || !strings.Contains(err.Error(), "unmarshal metadata") {
			t.Fatalf("Get() error = %v", err)
		}
	})
}

func TestAdapterDelete(t *testing.T) {
	tests := []struct {
		name string
		tag  pgconn.CommandTag
		err  error
	}{
		{name: "deleted", tag: pgconn.NewCommandTag("DELETE 1")},
		{name: "not found", tag: pgconn.NewCommandTag("DELETE 0")},
		{name: "failure", err: errors.New("delete failed")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database := stubDatabase{exec: func(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
				if !strings.Contains(query, "id = $1 AND volume_id = $2") || !reflect.DeepEqual(args, []any{"record-a", "volume-a"}) {
					t.Fatalf("query = %q, args = %#v", query, args)
				}
				return test.tag, test.err
			}}
			err := newTestAdapter(database).Delete(context.Background(), memory.DeleteRequest{ID: "record-a", BackendHandle: "volume-a"})
			switch test.name {
			case "deleted":
				if err != nil {
					t.Fatalf("Delete() error = %v", err)
				}
			case "not found":
				if !errors.Is(err, memory.ErrRecordNotFound) {
					t.Fatalf("Delete() error = %v, want ErrRecordNotFound", err)
				}
			case "failure":
				if !errors.Is(err, test.err) {
					t.Fatalf("Delete() error = %v, want wrapped database error", err)
				}
			}
		})
	}
}

func TestAdapterHealth(t *testing.T) {
	databaseError := errors.New("unhealthy")
	database := stubDatabase{ping: func(context.Context) error { return databaseError }}
	if err := newTestAdapter(database).Health(context.Background()); !errors.Is(err, databaseError) {
		t.Fatalf("Health() error = %v", err)
	}
}
