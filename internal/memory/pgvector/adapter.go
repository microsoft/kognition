// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package pgvector implements the memory.Backend interface using PostgreSQL
// with the pgvector extension for vector similarity search.
package pgvector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/memory"
)

// Adapter implements memory.Backend backed by pgvector.
type Adapter struct {
	pool   database
	logger *zap.Logger
}

type database interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Ping(context.Context) error
}

const (
	defaultEmbeddingDimension = 1536
	maxEmbeddingDimension     = 16000
)

// New creates a new pgvector memory backend adapter.
func New(pool *pgxpool.Pool, logger *zap.Logger) *Adapter {
	return &Adapter{pool: pool, logger: logger}
}

func (a *Adapter) Provision(ctx context.Context, req memory.ProvisionRequest) error {
	dimension := req.EmbeddingDimension
	if dimension == 0 {
		dimension = defaultEmbeddingDimension
	}
	if dimension < 1 || dimension > maxEmbeddingDimension {
		return fmt.Errorf("embedding dimension must be between 1 and %d", maxEmbeddingDimension)
	}
	var storedDimension int
	err := a.pool.QueryRow(ctx, `INSERT INTO memory_volume_dimensions (volume_id, embedding_dimension)
		VALUES ($1, $2) ON CONFLICT (volume_id) DO UPDATE
		SET embedding_dimension = memory_volume_dimensions.embedding_dimension
		WHERE memory_volume_dimensions.embedding_dimension = EXCLUDED.embedding_dimension
		RETURNING embedding_dimension`, req.BackendHandle, dimension).Scan(&storedDimension)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("memory volume %q already has a different embedding dimension", req.BackendHandle)
	}
	if err != nil {
		return fmt.Errorf("provision memory volume: %w", err)
	}
	a.logger.Info("provisioned memory volume", zap.String("handle", req.BackendHandle), zap.Int("dimension", storedDimension))
	return nil
}

func (a *Adapter) Deprovision(ctx context.Context, req memory.DeprovisionRequest) error {
	_, err := a.pool.Exec(ctx, "DELETE FROM memory_volume_dimensions WHERE volume_id = $1", req.BackendHandle)
	if err != nil {
		return fmt.Errorf("deprovision volume %s: %w", req.BackendHandle, err)
	}
	a.logger.Info("deprovisioned memory volume", zap.String("handle", req.BackendHandle))
	return nil
}

func (a *Adapter) Store(ctx context.Context, req memory.StoreRequest) (memory.StoreResponse, error) {
	metaJSON, err := json.Marshal(req.Metadata)
	if err != nil {
		return memory.StoreResponse{}, fmt.Errorf("marshal metadata: %w", err)
	}

	ts := req.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	var emb *pgvector.Vector
	if len(req.Embedding) > 0 {
		v := pgvector.NewVector(req.Embedding)
		emb = &v
	}

	_, err = a.pool.Exec(ctx,
		`INSERT INTO memory_vectors (id, volume_id, content, embedding, metadata, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (id, volume_id) DO UPDATE SET content = EXCLUDED.content, embedding = EXCLUDED.embedding, metadata = EXCLUDED.metadata, created_at = EXCLUDED.created_at`,
		req.ID, req.BackendHandle, req.Content, emb, metaJSON, ts,
	)
	if err != nil {
		return memory.StoreResponse{}, fmt.Errorf("store record: %w", err)
	}
	return memory.StoreResponse{ID: req.ID}, nil
}

func (a *Adapter) Get(ctx context.Context, req memory.GetRequest) (memory.Record, error) {
	row := a.pool.QueryRow(ctx,
		`SELECT id, content, embedding, metadata, created_at FROM memory_vectors WHERE id = $1 AND volume_id = $2`,
		req.ID, req.BackendHandle,
	)
	return scanRecord(row)
}

func (a *Adapter) List(ctx context.Context, req memory.ListRequest) ([]memory.Record, error) {
	query := `SELECT id, content, embedding, metadata, created_at FROM memory_vectors WHERE volume_id = $1`
	args := []any{req.BackendHandle}
	argIdx := 2

	if req.StartTime != nil {
		query += fmt.Sprintf(" AND created_at >= $%d", argIdx)
		args = append(args, *req.StartTime)
		argIdx++
	}
	if req.EndTime != nil {
		query += fmt.Sprintf(" AND created_at <= $%d", argIdx)
		args = append(args, *req.EndTime)
		argIdx++
	}
	if len(req.Metadata) > 0 {
		metaJSON, err := json.Marshal(req.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata filter: %w", err)
		}
		query += fmt.Sprintf(" AND metadata @> $%d", argIdx)
		args = append(args, metaJSON)
		argIdx++
	}

	query += " ORDER BY created_at DESC"
	if req.Limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", argIdx)
		args = append(args, req.Limit)
	}

	rows, err := a.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	defer rows.Close()

	var records []memory.Record
	for rows.Next() {
		r, err := scanRows(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (a *Adapter) Search(ctx context.Context, req memory.SearchRequest) ([]memory.SearchResult, error) {
	if len(req.Vector) == 0 {
		return nil, errors.New("search vector must not be empty")
	}
	var dimension int
	if err := a.pool.QueryRow(ctx, `SELECT COALESCE(
		(SELECT embedding_dimension FROM memory_volume_dimensions WHERE volume_id = $1), $2)`,
		req.BackendHandle, defaultEmbeddingDimension).Scan(&dimension); err != nil {
		return nil, fmt.Errorf("resolve memory volume dimensions: %w", err)
	}
	if len(req.Vector) != dimension {
		return nil, fmt.Errorf("memory volume %q expects %d dimensions, got %d", req.BackendHandle, dimension, len(req.Vector))
	}

	queryVec := pgvector.NewVector(req.Vector)

	expression := "embedding"
	if dimension == 384 || dimension == defaultEmbeddingDimension {
		expression = fmt.Sprintf("embedding::vector(%d)", dimension)
	}
	query := fmt.Sprintf(`SELECT id, content, embedding, metadata, created_at, 1 - (%s <=> $1) AS score
		FROM memory_vectors WHERE volume_id = $2 AND embedding IS NOT NULL AND vector_dims(embedding) = %d`,
		expression, dimension)
	args := []any{queryVec, req.BackendHandle}
	argIdx := 3

	if len(req.Metadata) > 0 {
		metaJSON, err := json.Marshal(req.Metadata)
		if err != nil {
			return nil, fmt.Errorf("marshal metadata filter: %w", err)
		}
		query += fmt.Sprintf(" AND metadata @> $%d", argIdx)
		args = append(args, metaJSON)
		argIdx++
	}
	if req.MinScore > 0 {
		query += fmt.Sprintf(" AND 1 - (%s <=> $1) >= $%d", expression, argIdx)
		args = append(args, req.MinScore)
		argIdx++
	}

	query += fmt.Sprintf(" ORDER BY %s <=> $1", expression)
	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, topK)

	rows, err := a.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search records: %w", err)
	}
	defer rows.Close()

	var results []memory.SearchResult
	for rows.Next() {
		var r memory.Record
		var score float64
		var embVec *pgvector.Vector
		var metaJSON []byte

		if err := rows.Scan(&r.ID, &r.Content, &embVec, &metaJSON, &r.Timestamp, &score); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		if embVec != nil {
			r.Embedding = embVec.Slice()
		}
		if metaJSON != nil {
			if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
				return nil, fmt.Errorf("unmarshal metadata: %w", err)
			}
		}
		results = append(results, memory.SearchResult{Record: r, Score: score})
	}
	return results, rows.Err()
}

func (a *Adapter) Delete(ctx context.Context, req memory.DeleteRequest) error {
	tag, err := a.pool.Exec(ctx,
		`DELETE FROM memory_vectors WHERE id = $1 AND volume_id = $2`,
		req.ID, req.BackendHandle,
	)
	if err != nil {
		return fmt.Errorf("delete record: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record %s: %w", req.ID, memory.ErrRecordNotFound)
	}
	return nil
}

func (a *Adapter) Health(ctx context.Context) error {
	return a.pool.Ping(ctx)
}

// scanRecord scans a single row into a Record.
func scanRecord(row pgx.Row) (memory.Record, error) {
	var r memory.Record
	var embVec *pgvector.Vector
	var metaJSON []byte

	if err := row.Scan(&r.ID, &r.Content, &embVec, &metaJSON, &r.Timestamp); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return r, memory.ErrRecordNotFound
		}
		return r, fmt.Errorf("scan record: %w", err)
	}
	if embVec != nil {
		r.Embedding = embVec.Slice()
	}
	if metaJSON != nil {
		if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
			return r, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	return r, nil
}

// scanRows scans the current row from pgx.Rows into a Record.
func scanRows(rows pgx.Rows) (memory.Record, error) {
	var r memory.Record
	var embVec *pgvector.Vector
	var metaJSON []byte

	if err := rows.Scan(&r.ID, &r.Content, &embVec, &metaJSON, &r.Timestamp); err != nil {
		return r, fmt.Errorf("scan record: %w", err)
	}
	if embVec != nil {
		r.Embedding = embVec.Slice()
	}
	if metaJSON != nil {
		if err := json.Unmarshal(metaJSON, &r.Metadata); err != nil {
			return r, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}
	return r, nil
}
