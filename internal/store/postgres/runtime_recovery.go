// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kognition-ai/kognition/internal/store"
)

var _ store.RuntimeRecoveryStore = (*Store)(nil)

func validRuntimeRecovery(record store.RuntimeRecoveryRecord) bool {
	return record.Workspace != "" && len(record.Workspace) <= 63 && record.SessionID != "" && len(record.SessionID) <= 128 && len(record.BindingDigest) == 64 && !record.ExpiresAt.IsZero() && len(record.Ciphertext) > 0 && len(record.Ciphertext) <= 768<<10 && json.Valid(record.Ciphertext)
}

func (s *Store) PrepareRuntimeRecovery(ctx context.Context, record store.RuntimeRecoveryRecord) (store.RuntimeRecoveryRecord, error) {
	if !validRuntimeRecovery(record) {
		return store.RuntimeRecoveryRecord{}, store.ErrConflict
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO runtime_recovery(namespace,session_id,binding_digest,phase,ciphertext,expires_at) VALUES($1,$2,$3,'prepared',$4,$5) ON CONFLICT(namespace,session_id) DO NOTHING`, record.Workspace, record.SessionID, record.BindingDigest, record.Ciphertext, record.ExpiresAt)
	if err != nil {
		return store.RuntimeRecoveryRecord{}, err
	}
	stored, err := s.GetRuntimeRecovery(ctx, record.Workspace, record.SessionID)
	if err == nil && stored.BindingDigest != record.BindingDigest {
		return store.RuntimeRecoveryRecord{}, store.ErrConflict
	}
	return stored, err
}

func (s *Store) GetRuntimeRecovery(ctx context.Context, namespace, sessionID string) (store.RuntimeRecoveryRecord, error) {
	var record store.RuntimeRecoveryRecord
	err := s.pool.QueryRow(ctx, `SELECT namespace,session_id,binding_digest,phase,owner,version,ciphertext,expires_at FROM runtime_recovery WHERE namespace=$1 AND session_id=$2`, namespace, sessionID).Scan(&record.Workspace, &record.SessionID, &record.BindingDigest, &record.Phase, &record.Owner, &record.Version, &record.Ciphertext, &record.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, store.ErrNotFound
	}
	return record, err
}

func (s *Store) ClaimRuntimeRecovery(ctx context.Context, namespace, sessionID string, version int64, owner string) (store.RuntimeRecoveryRecord, error) {
	if owner == "" || len(owner) > 128 {
		return store.RuntimeRecoveryRecord{}, store.ErrConflict
	}
	result, err := s.pool.Exec(ctx, `UPDATE runtime_recovery SET phase='claimed',owner=$4,version=version+1 WHERE namespace=$1 AND session_id=$2 AND version=$3 AND phase='prepared' AND expires_at>transaction_timestamp()`, namespace, sessionID, version, owner)
	if err != nil {
		return store.RuntimeRecoveryRecord{}, err
	}
	if result.RowsAffected() != 1 {
		return store.RuntimeRecoveryRecord{}, store.ErrConflict
	}
	return s.GetRuntimeRecovery(ctx, namespace, sessionID)
}

func (s *Store) CompleteRuntimeRecovery(ctx context.Context, record store.RuntimeRecoveryRecord) error {
	if !validRuntimeRecovery(record) || record.Owner == "" {
		return store.ErrConflict
	}
	result, err := s.pool.Exec(ctx, `UPDATE runtime_recovery SET phase='terminal',ciphertext=$6,expires_at=$7,version=version+1 WHERE namespace=$1 AND session_id=$2 AND binding_digest=$3 AND version=$4 AND owner=$5 AND phase='claimed'`, record.Workspace, record.SessionID, record.BindingDigest, record.Version, record.Owner, record.Ciphertext, record.ExpiresAt)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return store.ErrConflict
	}
	return nil
}

func (s *Store) ExpireRuntimeRecovery(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit < 1 || limit > 100 {
		return 0, store.ErrConflict
	}
	result, err := s.pool.Exec(ctx, `WITH expired AS (SELECT namespace,session_id FROM runtime_recovery WHERE phase='terminal' AND expires_at<=$1 ORDER BY expires_at,namespace,session_id LIMIT $2 FOR UPDATE SKIP LOCKED) UPDATE runtime_recovery AS records SET phase='retired',ciphertext=NULL,version=version+1 FROM expired WHERE records.namespace=expired.namespace AND records.session_id=expired.session_id`, now, limit)
	return int(result.RowsAffected()), err
}
