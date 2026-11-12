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
	"github.com/jackc/pgx/v5/pgconn"
	pgvector "github.com/pgvector/pgvector-go"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory"
)

type stubDatabase struct {
	exec     func(context.Context, string, ...any) (pgconn.CommandTag, error)
	query    func(context.Context, string, ...any) (pgx.Rows, error)
	queryRow func(context.Context, string, ...any) pgx.Row
	ping     func(context.Context) error
}

func (s stubDatabase) Exec(ctx context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if s.exec == nil {
		panic("unexpected Exec call")
	}
	return s.exec(ctx, query, args...)
}

func (s stubDatabase) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if s.query == nil {
		panic("unexpected Query call")
	}
	return s.query(ctx, query, args...)
}

func (s stubDatabase) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if s.queryRow == nil {
		panic("unexpected QueryRow call")
	}
	return s.queryRow(ctx, query, args...)
}

func (s stubDatabase) Ping(ctx context.Context) error {
	if s.ping == nil {
		panic("unexpected Ping call")
	}
	return s.ping(ctx)
}

type stubRow struct {
	scan func(...any) error
}

func (r stubRow) Scan(dest ...any) error {
	return r.scan(dest...)
}

type stubRows struct {
	scans  []func(...any) error
	err    error
	index  int
	closed bool
}

func newStubRows(scans ...func(...any) error) *stubRows {
	return &stubRows{scans: scans, index: -1}
}

func (r *stubRows) Close()                                       { r.closed = true }
func (r *stubRows) Err() error                                   { return r.err }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Values() ([]any, error)                       { return nil, nil }
func (r *stubRows) RawValues() [][]byte                          { return nil }
func (r *stubRows) Conn() *pgx.Conn                              { return nil }

func (r *stubRows) Next() bool {
	r.index++
	if r.index >= len(r.scans) {
		r.closed = true
		return false
	}
	return true
}

func (r *stubRows) Scan(dest ...any) error {
	return r.scans[r.index](dest...)
}

func scanValues(id, content string, embedding []float32, metadata []byte, timestamp time.Time, score *float64) func(...any) error {
	return func(dest ...any) error {
		*dest[0].(*string) = id
		*dest[1].(*string) = content
		if embedding != nil {
			vector := pgvector.NewVector(embedding)
			*dest[2].(**pgvector.Vector) = &vector
		}
		*dest[3].(*[]byte) = metadata
		*dest[4].(*time.Time) = timestamp
		if score != nil {
			*dest[5].(*float64) = *score
		}
		return nil
	}
}

func newTestAdapter(database database) *Adapter {
	return &Adapter{pool: database, logger: zap.NewNop()}
}

func TestAdapterDeprovision(t *testing.T) {
	t.Run("deletes all records for the volume", func(t *testing.T) {
		const handle = "volume-a"
		database := stubDatabase{exec: func(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
			if !strings.Contains(query, "DELETE FROM memory_volume_dimensions WHERE volume_id = $1") {
				t.Fatalf("query = %q", query)
			}
			if len(args) != 1 || args[0] != handle {
				t.Fatalf("args = %#v, want [%q]", args, handle)
			}
			return pgconn.NewCommandTag("DELETE 2"), nil
		}}

		adapter := newTestAdapter(database)
		if err := adapter.Deprovision(context.Background(), memory.DeprovisionRequest{BackendHandle: handle}); err != nil {
			t.Fatalf("Deprovision() error = %v", err)
		}
	})

	t.Run("wraps database failures", func(t *testing.T) {
		databaseError := errors.New("database unavailable")
		database := stubDatabase{exec: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, databaseError
		}}

		adapter := newTestAdapter(database)
		err := adapter.Deprovision(context.Background(), memory.DeprovisionRequest{BackendHandle: "volume-b"})
		if !errors.Is(err, databaseError) {
			t.Fatalf("Deprovision() error = %v, want wrapped database error", err)
		}
	})
}
