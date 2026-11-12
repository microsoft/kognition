// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRuntimeApprovalMigrationsUpgradeAndRerun(t *testing.T) {
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createRuntimeApprovalMigrationSchema(t, ctx, tx)

	var agentID, profileID, orphanProfileID string
	if err := tx.QueryRow(ctx, `INSERT INTO resources (kind, namespace, name, data) VALUES ('Agent', 'team-a', 'agent-a', '{"metadata":{},"spec":{}}') RETURNING id::text`).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO resources (kind, namespace, name, data) VALUES ('TrustProfile', 'team-a', 'profile-a', '{"metadata":{},"spec":{"agentRef":{"name":"agent-a"}}}') RETURNING id::text`).Scan(&profileID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO resources (kind, namespace, name, data) VALUES ('TrustProfile', 'team-a', 'orphan-profile', '{"metadata":{},"spec":{"agentRef":{"name":"missing-agent"}}}') RETURNING id::text`).Scan(&orphanProfileID); err != nil {
		t.Fatal(err)
	}

	migration019 := readRuntimeApprovalMigration(t, "migrations/019_runtime_approval_foundation.sql")
	for range 2 {
		if _, err := tx.Exec(ctx, migration019); err != nil {
			t.Fatalf("apply migration 019: %v", err)
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text, data #>> '{metadata,uid}', COALESCE(data #>> '{spec,agentIncarnationUID}', '') FROM resources ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string][2]string{}
	for rows.Next() {
		var id, uid, incarnationUID string
		if err := rows.Scan(&id, &uid, &incarnationUID); err != nil {
			t.Fatal(err)
		}
		seen[id] = [2]string{uid, incarnationUID}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen[agentID][0] != agentID || seen[profileID] != [2]string{profileID, agentID} || seen[orphanProfileID] != [2]string{orphanProfileID, "orphan-" + orphanProfileID} {
		t.Fatalf("migration 019 backfills = %#v", seen)
	}

	for _, decision := range []string{"Pending", "NotRequired"} {
		if _, err := tx.Exec(ctx, `INSERT INTO runtime_approvals (name, workspace, invocation_id, binding_tag, binding_key_version, decision_state, execution_state, expires_at, data) VALUES ($1, 'team-a', $1, 'tag', 1, $2, 'NotStarted', clock_timestamp() + interval '1 hour', jsonb_build_object('spec', jsonb_build_object('binding', jsonb_build_object('keyVersion', 1))))`, strings.ToLower(decision), decision); err != nil {
			t.Fatal(err)
		}
	}
	migration021 := readRuntimeApprovalMigration(t, "migrations/021_runtime_binding_key_epochs.sql")
	for range 2 {
		if _, err := tx.Exec(ctx, migration021); err != nil {
			t.Fatalf("apply migration 021: %v", err)
		}
	}
	approvalRows, err := tx.Query(ctx, `SELECT decision_state, binding_epoch, data #>> '{spec,binding,epoch}', data #>> '{spec,initialDecision}' FROM runtime_approvals ORDER BY decision_state`)
	if err != nil {
		t.Fatal(err)
	}
	defer approvalRows.Close()
	wantInitial := map[string]string{"NotRequired": "NotRequired", "Pending": "Pending"}
	for approvalRows.Next() {
		var decision, jsonEpoch, initialDecision string
		var epoch int64
		if err := approvalRows.Scan(&decision, &epoch, &jsonEpoch, &initialDecision); err != nil {
			t.Fatal(err)
		}
		if epoch != 1 || jsonEpoch != "1" || initialDecision != wantInitial[decision] {
			t.Fatalf("migration 021 backfill for %s = epoch %d/%q, initialDecision %q", decision, epoch, jsonEpoch, initialDecision)
		}
	}
	if err := approvalRows.Err(); err != nil {
		t.Fatal(err)
	}

	migration022 := readRuntimeApprovalMigration(t, "migrations/022_runtime_approval_execution.sql")
	for range 2 {
		if _, err := tx.Exec(ctx, migration022); err != nil {
			t.Fatalf("apply migration 022: %v", err)
		}
	}
	// After 022, all new execution columns must exist and default to NULL for
	// pre-existing rows.
	var executionDisposition, prepToken, workloadUID *string
	var prepDeadline, claimedAt *time.Time
	var fencingToken *int64
	if err := tx.QueryRow(ctx, `SELECT execution_disposition, preparation_token, preparation_deadline, claim_workload_uid, claim_fencing_token, claim_claimed_at FROM runtime_approvals WHERE decision_state = 'Pending'`).Scan(&executionDisposition, &prepToken, &prepDeadline, &workloadUID, &fencingToken, &claimedAt); err != nil {
		t.Fatalf("scan migration 022 columns: %v", err)
	}
	if executionDisposition != nil || prepToken != nil || prepDeadline != nil || workloadUID != nil || fencingToken != nil || claimedAt != nil {
		t.Fatalf("migration 022 columns should be NULL on existing rows: disposition=%v token=%v deadline=%v uid=%v fencing=%v claimed=%v", executionDisposition, prepToken, prepDeadline, workloadUID, fencingToken, claimedAt)
	}
	// Constraint: invalid disposition is rejected.
	var constraintCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = 'chk_runtime_approval_execution_disposition' AND conrelid = 'runtime_approvals'::regclass`).Scan(&constraintCount); err != nil {
		t.Fatalf("query constraint: %v", err)
	}
	if constraintCount != 1 {
		t.Fatalf("migration 022 expected disposition constraint present, got count=%d", constraintCount)
	}
	assertViolation := func(name, sql, expect string) {
		t.Helper()
		if _, err := tx.Exec(ctx, "SAVEPOINT "+name); err != nil {
			t.Fatalf("savepoint %s: %v", name, err)
		}
		_, err := tx.Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), expect) {
			t.Fatalf("migration 022 %s expected error containing %q, got %v", name, expect, err)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+name); err != nil {
			t.Fatalf("rollback %s: %v", name, err)
		}
	}
	assertViolation("bogus_disposition",
		`UPDATE runtime_approvals SET execution_disposition = 'BogusDisposition' WHERE decision_state = 'Pending'`,
		"chk_runtime_approval_execution_disposition")
	assertViolation("partial_preparation",
		`UPDATE runtime_approvals SET preparation_token = repeat('a', 43) WHERE decision_state = 'Pending'`,
		"chk_runtime_approval_preparation_fields")
	assertViolation("partial_claim",
		`UPDATE runtime_approvals SET claim_workload_uid = 'workload-a' WHERE decision_state = 'Pending'`,
		"chk_runtime_approval_claim_fields")
	// Valid preparation fields accepted.
	if _, err := tx.Exec(ctx, `UPDATE runtime_approvals SET preparation_token = repeat('a', 43), preparation_deadline = clock_timestamp() + interval '1 minute' WHERE decision_state = 'Pending'`); err != nil {
		t.Fatalf("migration 022 must accept valid preparation fields: %v", err)
	}
	// Valid claim fields accepted.
	if _, err := tx.Exec(ctx, `UPDATE runtime_approvals SET claim_workload_uid = 'workload-a', claim_process_start_nonce = repeat('n', 16), claim_fencing_token = 1, claim_claimed_at = clock_timestamp() WHERE decision_state = 'Pending'`); err != nil {
		t.Fatalf("migration 022 must accept valid claim fields: %v", err)
	}
	// Reap index must exist.
	var indexCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname IN ('idx_runtime_approvals_preparation_reap', 'idx_runtime_approvals_claim_lease') AND schemaname = current_schema()`).Scan(&indexCount); err != nil {
		t.Fatalf("scan index count: %v", err)
	}
	if indexCount != 2 {
		t.Fatalf("migration 022 expected 2 indexes, got %d", indexCount)
	}

	migration028 := readRuntimeApprovalMigration(t, "migrations/028_runtime_approval_reconciliation_actor.sql")
	for range 2 {
		if _, err := tx.Exec(ctx, migration028); err != nil {
			t.Fatalf("apply migration 028: %v", err)
		}
	}
	var reconciledByIssuer, reconciledBySubject *string
	if err := tx.QueryRow(ctx, `SELECT reconciled_by_issuer, reconciled_by_subject FROM runtime_approvals WHERE decision_state = 'Pending'`).Scan(&reconciledByIssuer, &reconciledBySubject); err != nil {
		t.Fatalf("scan migration 028 columns: %v", err)
	}
	if reconciledByIssuer != nil || reconciledBySubject != nil {
		t.Fatalf("migration 028 columns should be NULL on existing rows: issuer=%v subject=%v", reconciledByIssuer, reconciledBySubject)
	}
	assertViolation("partial_reconciliation_actor",
		`UPDATE runtime_approvals SET reconciled_by_issuer = 'issuer-a' WHERE decision_state = 'Pending'`,
		"chk_runtime_approval_reconciliation_actor")
	assertViolation("oversized_reconciliation_actor",
		`UPDATE runtime_approvals SET reconciled_by_issuer = repeat('i', 513), reconciled_by_subject = 'subject-a' WHERE decision_state = 'Pending'`,
		"chk_runtime_approval_reconciliation_actor")
	if _, err := tx.Exec(ctx, `UPDATE runtime_approvals SET reconciled_by_issuer = 'kubernetes.io/tokenreview', reconciled_by_subject = 'operator-a' WHERE decision_state = 'Pending'`); err != nil {
		t.Fatalf("migration 028 must accept valid reconciliation actor: %v", err)
	}
}

func TestRuntimeApprovalMigration019RejectsDuplicateProfiles(t *testing.T) {
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createRuntimeApprovalMigrationSchema(t, ctx, tx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO resources (kind, namespace, name, data) VALUES
			('Agent', 'team-a', 'agent-a', '{"metadata":{},"spec":{}}'),
			('TrustProfile', 'team-a', 'profile-a', '{"metadata":{},"spec":{"agentRef":{"name":"agent-a"}}}'),
			('TrustProfile', 'team-a', 'profile-b', '{"metadata":{},"spec":{"agentRef":{"name":"agent-a"}}}')`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, readRuntimeApprovalMigration(t, "migrations/019_runtime_approval_foundation.sql"))
	if err == nil || !strings.Contains(err.Error(), "duplicate live TrustProfiles") {
		t.Fatalf("migration 019 duplicate profile error = %v", err)
	}
}

func TestRuntimeApprovalCompactionMigrationIsAtomicAndRetiresTicket(t *testing.T) {
	ctx := context.Background()
	tx, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createRuntimeApprovalMigrationSchema(t, ctx, tx)
	for _, migration := range []string{
		"migrations/019_runtime_approval_foundation.sql",
		"migrations/021_runtime_binding_key_epochs.sql",
		"migrations/022_runtime_approval_execution.sql",
		"migrations/023_runtime_approval_compaction.sql",
		"migrations/023_runtime_approval_compaction.sql",
	} {
		if _, err := tx.Exec(ctx, readRuntimeApprovalMigration(t, migration)); err != nil {
			t.Fatalf("apply %s: %v", migration, err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runtime_approvals
		(name, workspace, invocation_id, binding_tag, binding_epoch, binding_key_version,
		 decision_state, execution_state, execution_disposition, expires_at, data)
		VALUES ('approval-a', 'team-a', 'invocation-a', 'tag-a', 1, 1,
		 'Approved', 'Completed', 'Succeeded', clock_timestamp() - interval '31 days',
		 '{"metadata":{"name":"approval-a","namespace":"team-a"},"spec":{"workspace":"team-a","invocationID":"invocation-a","initialDecision":"Pending","plane":"Tool","operation":"call","agentRef":{"kind":"Agent","scope":"Namespaced","namespace":"team-a","name":"agent-a","uid":"agent-uid","resourceVersion":1},"binding":{"format":"f","algorithm":"a","domain":"d","epoch":1,"keyVersion":1,"tag":"tag-a"},"createdAt":"2026-01-01T00:00:00Z","expiresAt":"2026-01-01T01:00:00Z"},"status":{"decisionState":"Approved","decisionEvidenceDelivered":true,"executionState":"Completed","executionDisposition":"Succeeded"}}')`); err != nil {
		t.Fatal(err)
	}
	var compacted bool
	if err := tx.QueryRow(ctx, `SELECT compact_runtime_approval('team-a', 'invocation-a', interval '90 days')`).Scan(&compacted); err != nil || !compacted {
		t.Fatalf("compact runtime approval = %v, %v", compacted, err)
	}
	var liveCount, tombstoneCount, ticketCount int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM runtime_approvals WHERE workspace = 'team-a' AND invocation_id = 'invocation-a'),
		(SELECT count(*) FROM runtime_approval_tombstones WHERE workspace = 'team-a' AND invocation_id = 'invocation-a'),
		(SELECT count(*) FROM runtime_approval_quota_tickets WHERE workspace = 'team-a' AND invocation_id = 'invocation-a' AND phase = 'Compacted' AND retention_deadline IS NOT NULL)`).Scan(&liveCount, &tombstoneCount, &ticketCount); err != nil {
		t.Fatal(err)
	}
	if liveCount != 0 || tombstoneCount != 1 || ticketCount != 1 {
		t.Fatalf("post-compaction counts live=%d tombstone=%d ticket=%d", liveCount, tombstoneCount, ticketCount)
	}
	if err := tx.QueryRow(ctx, `SELECT retire_runtime_approval_tombstones(clock_timestamp() + interval '91 days')`).Scan(&tombstoneCount); err != nil || tombstoneCount != 1 {
		t.Fatalf("retire runtime approval tombstones = %d, %v", tombstoneCount, err)
	}
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM runtime_approval_tombstones),
		(SELECT count(*) FROM runtime_approval_quota_tickets)`).Scan(&tombstoneCount, &ticketCount); err != nil {
		t.Fatal(err)
	}
	if tombstoneCount != 0 || ticketCount != 0 {
		t.Fatalf("post-retention counts tombstone=%d ticket=%d", tombstoneCount, ticketCount)
	}
}

func TestRuntimeApprovalMigrationCopiesStayEquivalent(t *testing.T) {
	internal021 := []byte(readRuntimeApprovalMigration(t, "migrations/021_runtime_binding_key_epochs.sql"))
	helm021 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/021_runtime_binding_key_epochs.sql"))
	if !bytes.Equal(internal021, helm021) {
		t.Fatal("migration 021 internal and Helm copies differ")
	}
	internal019 := []byte(readRuntimeApprovalMigration(t, "migrations/019_runtime_approval_foundation.sql"))
	helm019 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/019_runtime_approval_foundation.sql"))
	if !bytes.Equal(internal019, helm019) {
		t.Fatal("migration 019 internal and Helm copies differ")
	}
	internal022 := []byte(readRuntimeApprovalMigration(t, "migrations/022_runtime_approval_execution.sql"))
	helm022 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/022_runtime_approval_execution.sql"))
	if !bytes.Equal(internal022, helm022) {
		t.Fatal("migration 022 internal and Helm copies differ")
	}
	internal023 := []byte(readRuntimeApprovalMigration(t, "migrations/023_runtime_approval_compaction.sql"))
	helm023 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/023_runtime_approval_compaction.sql"))
	if !bytes.Equal(internal023, helm023) {
		t.Fatal("migration 023 internal and Helm copies differ")
	}
	internal024 := []byte(readRuntimeApprovalMigration(t, "migrations/024_tool_invocation_awaiting_approval.sql"))
	helm024 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/024_tool_invocation_awaiting_approval.sql"))
	if !bytes.Equal(internal024, helm024) {
		t.Fatal("migration 024 internal and Helm copies differ")
	}
	internal025 := []byte(readRuntimeApprovalMigration(t, "migrations/025_tool_invocation_preparation.sql"))
	helm025 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/025_tool_invocation_preparation.sql"))
	if !bytes.Equal(internal025, helm025) {
		t.Fatal("migration 025 internal and Helm copies differ")
	}
	internal026 := []byte(readRuntimeApprovalMigration(t, "migrations/026_tool_call_reservations.sql"))
	helm026 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/026_tool_call_reservations.sql"))
	if !bytes.Equal(internal026, helm026) {
		t.Fatal("migration 026 internal and Helm copies differ")
	}
	internal027 := []byte(readRuntimeApprovalMigration(t, "migrations/027_memory_mutation_receipts.sql"))
	helm027 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/027_memory_mutation_receipts.sql"))
	if !bytes.Equal(internal027, helm027) {
		t.Fatal("migration 027 internal and Helm copies differ")
	}
	internal028 := []byte(readRuntimeApprovalMigration(t, "migrations/028_runtime_approval_reconciliation_actor.sql"))
	helm028 := []byte(readRuntimeApprovalMigration(t, "../../../deploy/helm/kognition/migrations/028_runtime_approval_reconciliation_actor.sql"))
	if !bytes.Equal(internal028, helm028) {
		t.Fatal("migration 028 internal and Helm copies differ")
	}
}

func createRuntimeApprovalMigrationSchema(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	schema := "runtime_approval_migration_" + time.Now().UTC().Format("20060102150405.000000000")
	schema = strings.ReplaceAll(schema, ".", "")
	if _, err := tx.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+schema+", public"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE resources (
		id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
		kind TEXT NOT NULL,
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		data JSONB NOT NULL,
		deleted_at TIMESTAMPTZ
	)`); err != nil {
		t.Fatal(err)
	}
}

func readRuntimeApprovalMigration(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
