package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kognition-ai/kognition/internal/store"
)

const workflowActivationRecordKind = "WorkflowActivationRecord"

var _ store.WorkflowActivationStore = (*Store)(nil)

func workflowActivationRecordName(key store.WorkflowActivationKey) string {
	encoded, _ := json.Marshal(key)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (storage *Store) CreateWorkflowActivation(ctx context.Context, spec store.WorkflowActivationSpec) (store.WorkflowActivation, error) {
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, workflowActivationRecordKind+":"+spec.Key.Workspace+":"+workflowActivationRecordName(spec.Key)); err != nil {
		return store.WorkflowActivation{}, err
	}
	var now time.Time
	if err := transaction.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return store.WorkflowActivation{}, err
	}
	proposed, err := store.NewWorkflowActivation(spec, now)
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	encoded, err := json.Marshal(proposed)
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	var exists bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM resources WHERE kind=$1 AND namespace=$2 AND name=$3)`, workflowActivationRecordKind, spec.Key.Workspace, workflowActivationRecordName(spec.Key)).Scan(&exists); err != nil {
		return store.WorkflowActivation{}, err
	}
	if !exists {
		if _, err := transaction.Exec(ctx, `INSERT INTO resources(kind,namespace,name,data) VALUES($1,$2,$3,$4)`, workflowActivationRecordKind, spec.Key.Workspace, workflowActivationRecordName(spec.Key), encoded); err != nil {
			return store.WorkflowActivation{}, err
		}
	}
	retained, err := readWorkflowActivation(ctx, transaction, spec.Key, false)
	if exists && errors.Is(err, store.ErrNotFound) {
		return store.WorkflowActivation{}, store.ErrConflict
	}
	if err != nil {
		return store.WorkflowActivation{}, err
	}
	if !reflect.DeepEqual(retained.Spec, proposed.Spec) {
		return store.WorkflowActivation{}, store.ErrConflict
	}
	if err := transaction.Commit(ctx); err != nil {
		return store.WorkflowActivation{}, err
	}
	return retained, nil
}

type workflowActivationReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readWorkflowActivation(ctx context.Context, reader workflowActivationReader, key store.WorkflowActivationKey, lock bool) (store.WorkflowActivation, error) {
	if !store.ValidWorkflowActivationKey(key) {
		return store.WorkflowActivation{}, store.ErrConflict
	}
	query := `SELECT data FROM resources WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	var encoded []byte
	if err := reader.QueryRow(ctx, query, workflowActivationRecordKind, key.Workspace, workflowActivationRecordName(key)).Scan(&encoded); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.WorkflowActivation{}, store.ErrNotFound
		}
		return store.WorkflowActivation{}, err
	}
	if len(encoded) > 2*store.WorkflowGateMaxBytes {
		return store.WorkflowActivation{}, store.ErrConflict
	}
	var activation store.WorkflowActivation
	if err := json.Unmarshal(encoded, &activation); err != nil {
		return store.WorkflowActivation{}, errors.Join(store.ErrConflict, err)
	}
	if activation.Spec.Key != key || store.ValidateWorkflowActivation(activation) != nil {
		return store.WorkflowActivation{}, store.ErrConflict
	}
	return activation, nil
}

func (storage *Store) GetWorkflowActivation(ctx context.Context, key store.WorkflowActivationKey) (store.WorkflowActivation, error) {
	return readWorkflowActivation(ctx, storage.pool, key, false)
}

func (storage *Store) MutateWorkflowActivation(ctx context.Context, key store.WorkflowActivationKey, mutation store.WorkflowActivationMutation) (store.WorkflowActivationResult, error) {
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		return store.WorkflowActivationResult{}, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	activation, err := readWorkflowActivation(ctx, transaction, key, true)
	if err != nil {
		return store.WorkflowActivationResult{}, err
	}
	var now time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return store.WorkflowActivationResult{}, err
	}
	result, err := store.ApplyWorkflowActivation(activation, mutation, now)
	if err != nil {
		return store.WorkflowActivationResult{}, err
	}
	if result.Changed {
		encoded, err := json.Marshal(result.Activation)
		if err != nil {
			return store.WorkflowActivationResult{}, err
		}
		_, err = transaction.Exec(ctx, `UPDATE resources SET data=$4,resource_version=resource_version+1,updated_at=clock_timestamp() WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL`, workflowActivationRecordKind, key.Workspace, workflowActivationRecordName(key), encoded)
		if err != nil {
			return store.WorkflowActivationResult{}, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return store.WorkflowActivationResult{}, err
	}
	return result, nil
}
