// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const workflowExecutorAuthorityKind = "WorkflowExecutorAuthorityRecord"
const workflowRunAcceptanceKind = "WorkflowRunAcceptanceRecord"

var _ runtimeapproval.WorkflowExecutorBindingStore = (*Store)(nil)
var _ runtimeapproval.WorkflowRunSubmissionStore = (*Store)(nil)

func (storage *Store) WorkflowExecutorClock(ctx context.Context) (time.Time, error) {
	var now time.Time
	if err := storage.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

func privateWorkflowRecord(kind string) bool {
	return kind == workflowActivationRecordKind || kind == workflowExecutorAuthorityKind || kind == workflowRunAcceptanceKind
}

func workflowRunAcceptanceName(runUID string) string {
	digest := sha256.Sum256([]byte(runUID))
	return hex.EncodeToString(digest[:])
}

func (storage *Store) CreateWorkflowExecutorAuthority(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	authority.Reference.UID = uuid.NewString()
	authority.Reference.ResourceVersion = 1
	if err := runtimeapproval.ValidateWorkflowExecutorAuthority(authority); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	if err := validateWorkflowExecutorDefinition(ctx, transaction, authority); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	encoded, err := json.Marshal(authority)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	_, err = transaction.Exec(ctx, `INSERT INTO resources(id,kind,namespace,name,data,resource_version) VALUES($1,$2,$3,$4,$5,1)`,
		authority.Reference.UID, workflowExecutorAuthorityKind, authority.Reference.Namespace, authority.Reference.Name, encoded)
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" {
		err = store.ErrAlreadyExists
	}
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	return authority, transaction.Commit(ctx)
}

func validateWorkflowExecutorDefinition(ctx context.Context, transaction pgx.Tx, authority runtimeapproval.WorkflowExecutorAuthority) error {
	var raw []byte
	err := transaction.QueryRow(ctx, `SELECT data FROM resources WHERE kind='Workflow' AND namespace=$1 AND name=$2 AND deleted_at IS NULL FOR SHARE`, authority.Workflow.Namespace, authority.Workflow.Name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := runtimeapproval.ValidateWorkflowExecutorAuthorityDefinition(authority, raw); err != nil {
		return errors.Join(store.ErrConflict, err)
	}
	return nil
}

func readWorkflowExecutorAuthority(ctx context.Context, reader workflowActivationReader, reference api.RuntimeApprovalReference, lock bool) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if reference.Kind != "WorkflowExecutionBinding" || reference.Namespace == "" || reference.Name == "" {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrConflict
	}
	query := `SELECT data,id::text,resource_version FROM resources WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL`
	if lock {
		query += ` FOR UPDATE`
	}
	var raw []byte
	var uid string
	var version int64
	err := reader.QueryRow(ctx, query, workflowExecutorAuthorityKind, reference.Namespace, reference.Name).Scan(&raw, &uid, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrNotFound
	}
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	authority, err := runtimeapproval.DecodeWorkflowExecutorAuthority(raw)
	if err != nil || authority.Reference.Namespace != reference.Namespace || authority.Reference.Name != reference.Name ||
		authority.Reference.UID != uid || authority.Reference.ResourceVersion != version ||
		reference.UID != "" && reference.UID != uid || reference.ResourceVersion != 0 && reference.ResourceVersion != version {
		return runtimeapproval.WorkflowExecutorAuthority{}, errors.Join(store.ErrConflict, err)
	}
	return authority, nil
}

func (storage *Store) GetWorkflowExecutorAuthority(ctx context.Context, reference api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error) {
	return readWorkflowExecutorAuthority(ctx, storage.pool, reference, false)
}

func (storage *Store) UpdateWorkflowExecutorAuthority(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if err := runtimeapproval.ValidateWorkflowExecutorAuthority(authority); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	prior, err := readWorkflowExecutorAuthority(ctx, transaction, authority.Reference, true)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	if prior.Workflow != authority.Workflow || prior.WorkflowRevisionDigest != authority.WorkflowRevisionDigest || prior.Executor != authority.Executor {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrConflict
	}
	if authority.Enabled {
		if err := validateWorkflowExecutorDefinition(ctx, transaction, authority); err != nil {
			return runtimeapproval.WorkflowExecutorAuthority{}, err
		}
	}
	authority.Reference.ResourceVersion++
	encoded, err := json.Marshal(authority)
	if err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	if _, err := transaction.Exec(ctx, `UPDATE resources SET data=$4,resource_version=resource_version+1,updated_at=clock_timestamp() WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL`,
		workflowExecutorAuthorityKind, authority.Reference.Namespace, authority.Reference.Name, encoded); err != nil {
		return runtimeapproval.WorkflowExecutorAuthority{}, err
	}
	return authority, transaction.Commit(ctx)
}

func (storage *Store) CreateWorkflowRunAcceptance(ctx context.Context, acceptance runtimeapproval.WorkflowRunAcceptance) (runtimeapproval.WorkflowRunAcceptance, error) {
	if err := runtimeapproval.ValidateWorkflowRunAcceptance(acceptance); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	retained, err := storage.createWorkflowRunAcceptance(ctx, transaction, acceptance)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	return retained, transaction.Commit(ctx)
}

func (storage *Store) createWorkflowRunAcceptance(ctx context.Context, transaction pgx.Tx, acceptance runtimeapproval.WorkflowRunAcceptance) (runtimeapproval.WorkflowRunAcceptance, error) {
	var runUID string
	var version int64
	var runJSON []byte
	err := transaction.QueryRow(ctx, `SELECT id::text,resource_version,data FROM resources WHERE kind='WorkflowRun' AND namespace=$1 AND name=$2 AND deleted_at IS NULL FOR UPDATE`,
		acceptance.Workspace, acceptance.Run.Name).Scan(&runUID, &version, &runJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrNotFound
	}
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if acceptance.RunUID != runUID || acceptance.Run.ResourceVersion != version {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	var run api.WorkflowRun
	if err := json.Unmarshal(runJSON, &run); err != nil || run.Spec.WorkflowRef.Name != acceptance.Workflow.Name {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	authority, err := readWorkflowExecutorAuthority(ctx, transaction, acceptance.ExecutionBinding, true)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	var workflowJSON []byte
	err = transaction.QueryRow(ctx, `SELECT data FROM resources WHERE kind='Workflow' AND namespace=$1 AND name=$2 AND deleted_at IS NULL FOR SHARE`,
		acceptance.Workflow.Namespace, acceptance.Workflow.Name).Scan(&workflowJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrNotFound
	}
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceDefinitions(ctx, acceptance, authority, workflowJSON, runJSON); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	identity, _ := auth.IdentityFromContext(ctx)
	policyReader := &workflowAcceptancePolicyReader{Store: storage, transaction: transaction}
	if err := controlplaneauth.New(policyReader).AuthorizeWorkflowUse(ctx, identity, acceptance.Workspace, acceptance.Workflow.Name, acceptance.Workflow.UID, acceptance.WorkflowRevisionDigest); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	var now time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceAuthority(acceptance, authority, now); err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	encoded, err := json.Marshal(acceptance)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	name := workflowRunAcceptanceName(acceptance.RunUID)
	_, err = transaction.Exec(ctx, `INSERT INTO resources(kind,namespace,name,data) VALUES($1,$2,$3,$4) ON CONFLICT(kind,namespace,name) WHERE deleted_at IS NULL DO NOTHING`,
		workflowRunAcceptanceKind, acceptance.Workspace, name, encoded)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	retained, err := readWorkflowRunAcceptance(ctx, transaction, acceptance.Workspace, acceptance.RunUID)
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if !reflect.DeepEqual(retained, acceptance) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	return retained, nil
}

type workflowAcceptancePolicyReader struct {
	store.Store
	transaction pgx.Tx
}

func (reader *workflowAcceptancePolicyReader) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "ControlPlaneRole" {
		return nil, controlplaneauth.ErrUnavailable
	}
	var raw []byte
	err := reader.transaction.QueryRow(ctx, `SELECT data FROM resources WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL FOR SHARE`, kind, namespace, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if len(raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return nil, controlplaneauth.ErrUnavailable
	}
	return raw, err
}

func (reader *workflowAcceptancePolicyReader) List(ctx context.Context, kind string, _ store.ListOptions) ([][]byte, error) {
	if kind != "ControlPlaneRoleBinding" {
		return nil, controlplaneauth.ErrUnavailable
	}
	rows, err := reader.transaction.Query(ctx, `SELECT data FROM resources WHERE kind=$1 AND deleted_at IS NULL LIMIT 513 FOR SHARE`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if len(raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes || len(result) >= 512 {
			return nil, controlplaneauth.ErrUnavailable
		}
		result = append(result, raw)
	}
	return result, rows.Err()
}

func (storage *Store) CreateAcceptedWorkflowRun(ctx context.Context, binding api.RuntimeApprovalReference, runJSON []byte, durationSeconds int64) (api.WorkflowRun, runtimeapproval.WorkflowRunAcceptance, error) {
	fail := func(err error) (api.WorkflowRun, runtimeapproval.WorkflowRunAcceptance, error) {
		return api.WorkflowRun{}, runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if len(runJSON) == 0 || len(runJSON) > runtimeapproval.WorkflowExecutorRecordMaxBytes || durationSeconds < 1 || durationSeconds > 86400 {
		return fail(store.ErrConflict)
	}
	if _, err := runtimeapproval.CanonicalizeJSON(runJSON); err != nil {
		return fail(err)
	}
	var run api.WorkflowRun
	decoder := json.NewDecoder(bytes.NewReader(runJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&run); err != nil || run.UID != "" || run.ResourceVersion != 0 || run.Namespace != binding.Namespace {
		return fail(store.ErrConflict)
	}
	initiator, err := runtimeapproval.AuthenticatedWorkflowExecutor(ctx)
	if err != nil {
		return fail(err)
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fail(err)
	}
	defer func() { _ = transaction.Rollback(ctx) }()
	authority, err := readWorkflowExecutorAuthority(ctx, transaction, binding, true)
	if err != nil {
		return fail(err)
	}
	var now time.Time
	if err := transaction.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fail(err)
	}
	if durationSeconds > authority.Limits.MaxDurationSeconds {
		return fail(store.ErrConflict)
	}
	run.UID, run.ResourceVersion = uuid.NewString(), 1
	stored, err := json.Marshal(run)
	if err != nil {
		return fail(err)
	}
	_, err = transaction.Exec(ctx, `INSERT INTO resources(id,kind,namespace,name,data,resource_version) VALUES($1,'WorkflowRun',$2,$3,$4,1)`, run.UID, run.Namespace, run.Name, stored)
	if err != nil {
		var conflict *pgconn.PgError
		if errors.As(err, &conflict) && conflict.Code == "23505" {
			err = store.ErrAlreadyExists
		}
		return fail(err)
	}
	expiry := now.Add(time.Duration(durationSeconds) * time.Second)
	if expiry.After(authority.ExpiresAt) {
		expiry = authority.ExpiresAt
	}
	acceptance := runtimeapproval.WorkflowRunAcceptance{Workspace: run.Namespace, RunUID: run.UID,
		Run:      api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: run.Namespace, Name: run.Name, UID: run.UID, ResourceVersion: 1},
		Workflow: authority.Workflow, WorkflowRevisionDigest: authority.WorkflowRevisionDigest, ExecutionBinding: authority.Reference, Executor: authority.Executor, Initiator: initiator, AcceptedAt: now.UTC(), ExpiresAt: expiry.UTC()}
	acceptance.RequestDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(stored)
	if err != nil {
		return fail(err)
	}
	acceptance.BindingDigest, err = runtimeapproval.WorkflowRunExecutionScopeDigest(acceptance, authority)
	if err != nil {
		return fail(err)
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptance(acceptance); err != nil {
		return fail(err)
	}
	retained, err := storage.createWorkflowRunAcceptance(ctx, transaction, acceptance)
	if err != nil {
		return fail(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		return fail(err)
	}
	storage.notifyLocal(store.WatchEvent{Type: store.EventCreated, Kind: "WorkflowRun", Namespace: run.Namespace, Name: run.Name, ResourceVersion: 1, Object: stored})
	return run, retained, nil
}

func readWorkflowRunAcceptance(ctx context.Context, reader workflowActivationReader, workspace, runUID string) (runtimeapproval.WorkflowRunAcceptance, error) {
	if workspace == "" || runUID == "" {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	var encoded []byte
	err := reader.QueryRow(ctx, `SELECT data FROM resources WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL`,
		workflowRunAcceptanceKind, workspace, workflowRunAcceptanceName(runUID)).Scan(&encoded)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrNotFound
	}
	if err != nil {
		return runtimeapproval.WorkflowRunAcceptance{}, err
	}
	acceptance, err := runtimeapproval.DecodeWorkflowRunAcceptance(encoded)
	if err != nil || acceptance.Workspace != workspace || acceptance.RunUID != runUID {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.Join(store.ErrConflict, err)
	}
	return acceptance, nil
}

func (storage *Store) GetWorkflowRunAcceptance(ctx context.Context, workspace, runUID string) (runtimeapproval.WorkflowRunAcceptance, error) {
	return readWorkflowRunAcceptance(ctx, storage.pool, workspace, runUID)
}
