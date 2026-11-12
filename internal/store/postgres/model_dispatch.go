// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kognition-ai/kognition/internal/store"
)

var _ store.ModelDispatchStore = (*Store)(nil)

func (s *Store) ClaimModelDispatch(ctx context.Context, namespace, operation, sessionID string) (bool, error) {
	if namespace == "" || operation == "" || sessionID == "" {
		return false, fmt.Errorf("model dispatch requires namespace, operation and session")
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO model_dispatches (namespace, operation, session_id)
		VALUES ($1, $2, $3) ON CONFLICT (namespace, operation) DO NOTHING`, namespace, operation, sessionID)
	if err != nil {
		return false, fmt.Errorf("claim model dispatch: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *Store) CompleteModelDispatch(ctx context.Context, namespace, operation, sessionID string, response json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response, &object); err != nil || object == nil {
		return fmt.Errorf("model completion requires a JSON object")
	}
	result, err := s.pool.Exec(ctx, `UPDATE model_dispatches SET response = $4, completed_at = transaction_timestamp()
		WHERE namespace = $1 AND operation = $2 AND session_id = $3 AND response IS NULL`, namespace, operation, sessionID, response)
	if err != nil {
		return fmt.Errorf("complete model dispatch: %w", err)
	}
	if result.RowsAffected() != 1 {
		return store.ErrConflict
	}
	return nil
}

func (s *Store) GetModelDispatch(ctx context.Context, namespace, operation string) (store.ModelDispatchRecord, error) {
	var record store.ModelDispatchRecord
	err := s.pool.QueryRow(ctx, `SELECT session_id, response FROM model_dispatches
		WHERE namespace = $1 AND operation = $2`, namespace, operation).Scan(&record.SessionID, &record.Response)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, store.ErrNotFound
	}
	if err != nil {
		return record, fmt.Errorf("get model dispatch: %w", err)
	}
	return record, nil
}
