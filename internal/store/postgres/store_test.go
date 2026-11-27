// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

var testPool *pgxpool.Pool

// testSchema contains only what the Store struct actually uses (no pgvector).
const testSchema = `
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE IF NOT EXISTS resources (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL DEFAULT 'default',
    name             TEXT NOT NULL,
    data             JSONB NOT NULL,
    resource_version BIGINT NOT NULL DEFAULT 1,
    generation       BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at       TIMESTAMPTZ,
    CONSTRAINT uq_resources_kind_ns_name UNIQUE (kind, namespace, name)
);

CREATE INDEX idx_resources_kind_ns ON resources (kind, namespace) WHERE deleted_at IS NULL;
CREATE INDEX idx_resources_watch ON resources (kind, resource_version) WHERE deleted_at IS NULL;

CREATE OR REPLACE FUNCTION notify_resource_change() RETURNS trigger AS $$
DECLARE
    op TEXT := TG_ARGV[0];
BEGIN
    -- Detect soft-delete: if deleted_at was just set, treat as DELETE.
    IF op = 'UPDATE' AND NEW.deleted_at IS NOT NULL AND (OLD.deleted_at IS NULL) THEN
        op := 'DELETE';
    END IF;

    PERFORM pg_notify('resource_changes', json_build_object(
        'operation', op,
        'kind', NEW.kind,
        'namespace', NEW.namespace,
        'name', NEW.name,
        'resource_version', NEW.resource_version
    )::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_resources_insert
    AFTER INSERT ON resources
    FOR EACH ROW EXECUTE FUNCTION notify_resource_change('INSERT');

CREATE TRIGGER trg_resources_update
    AFTER UPDATE ON resources
    FOR EACH ROW
    WHEN (OLD.deleted_at IS NULL)
    EXECUTE FUNCTION notify_resource_change('UPDATE');

CREATE TABLE IF NOT EXISTS resource_revisions (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind             TEXT NOT NULL,
    namespace        TEXT NOT NULL,
    name             TEXT NOT NULL,
    revision         BIGINT NOT NULL,
    spec             JSONB NOT NULL,
    data             JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by       TEXT NOT NULL DEFAULT '',
    CONSTRAINT uq_revisions_kind_ns_name_rev UNIQUE (kind, namespace, name, revision)
);

CREATE INDEX idx_revisions_resource ON resource_revisions (kind, namespace, name, revision);

CREATE TABLE IF NOT EXISTS instance_events (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    kind       TEXT NOT NULL,
    namespace  TEXT NOT NULL,
    name       TEXT NOT NULL,
    event_type TEXT NOT NULL,
    reason     TEXT NOT NULL,
    message    TEXT NOT NULL,
    details    JSONB,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_instance_events_resource ON instance_events (kind, namespace, name, created_at);

CREATE TABLE IF NOT EXISTS cognitive_audit_log (
	id             TEXT PRIMARY KEY,
	created_at     TIMESTAMPTZ NOT NULL,
	agent_identity TEXT NOT NULL DEFAULT '',
	workspace      TEXT NOT NULL,
	solution       TEXT NOT NULL DEFAULT '',
	solution_rev   BIGINT NOT NULL DEFAULT 0,
	session_id     TEXT NOT NULL DEFAULT '',
	event_type     TEXT NOT NULL,
	subject_kind   TEXT NOT NULL DEFAULT '',
	subject_ref    TEXT NOT NULL DEFAULT '',
	verb           TEXT NOT NULL DEFAULT '',
	details        JSONB,
	trace_id       TEXT NOT NULL DEFAULT '',
	span_id        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS token_reservations (
	reservation_id  TEXT PRIMARY KEY,
	namespace       TEXT NOT NULL,
	reserved_input  BIGINT NOT NULL CHECK (reserved_input >= 0),
	reserved_output BIGINT NOT NULL CHECK (reserved_output >= 0),
	quota_periods    JSONB NOT NULL DEFAULT '{}'::jsonb,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tool_invocation_receipts (
	namespace      TEXT NOT NULL,
	invocation_id  TEXT NOT NULL,
	binding        TEXT NOT NULL,
	status         TEXT NOT NULL CHECK (status IN ('Pending', 'Succeeded', 'Failed')),
	output         JSONB,
	execution_mode TEXT NOT NULL DEFAULT '',
	duration_ms    BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
	error_message  TEXT NOT NULL DEFAULT '',
	created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	completed_at   TIMESTAMPTZ,
	PRIMARY KEY (namespace, invocation_id)
);
`

func TestMain(m *testing.M) {
	runtimePath, err := os.MkdirTemp("", "kognition-store-postgres-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create embedded postgres runtime directory: %v\n", err)
		os.Exit(1)
	}

	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(15432).
		Version(embeddedpostgres.V16).
		RuntimePath(runtimePath))

	if err := pg.Start(); err != nil {
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to start embedded postgres: %v\n", err)
		os.Exit(1)
	}

	dsn := "postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable"
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
		os.Exit(1)
	}

	if _, err := pool.Exec(ctx, testSchema); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply schema: %v\n", err)
		os.Exit(1)
	}
	migration, err := os.ReadFile("migrations/010_cost_records.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read cost records migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply cost records migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/011_usage_event_accounting.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read usage event migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply usage event migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/012_usage_event_immutability.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read usage immutability migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply usage immutability migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/013_canary_operations.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read canary operations migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply canary operations migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/014_canary_finalization.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read canary finalization migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply canary finalization migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/015_audit_canonical_payload.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read audit canonical payload migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply audit canonical payload migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/016_canary_reservation_presence.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read canary reservation presence migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply canary reservation presence migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/017_usage_purpose.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read usage purpose migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply usage purpose migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/018_canary_operation_tombstones.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read canary tombstone migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply canary tombstone migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/019_runtime_approval_foundation.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read RuntimeApproval foundation migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply RuntimeApproval foundation migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/020_tool_grant_rate_limits.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read ToolGrant rate migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply ToolGrant rate migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/021_runtime_binding_key_epochs.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read RuntimeApproval binding-key epoch migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply RuntimeApproval binding-key epoch migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/022_runtime_approval_execution.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read RuntimeApproval execution migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply RuntimeApproval execution migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/023_runtime_approval_compaction.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read RuntimeApproval compaction migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply RuntimeApproval compaction migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/024_tool_invocation_awaiting_approval.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read Tool invocation approval migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply Tool invocation approval migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/025_tool_invocation_preparation.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read Tool invocation preparation migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply Tool invocation preparation migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/026_tool_call_reservations.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read Tool call reservation migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply Tool call reservation migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/027_memory_mutation_receipts.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read Memory mutation receipt migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply Memory mutation receipt migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/028_runtime_approval_reconciliation_actor.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read RuntimeApproval reconciliation actor migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply RuntimeApproval reconciliation actor migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/029_cost_attribution.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read cost attribution migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply cost attribution migration: %v\n", err)
		os.Exit(1)
	}
	migration, err = os.ReadFile("migrations/030_usage_reporting.sql")
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to read usage reporting migration: %v\n", err)
		os.Exit(1)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply usage reporting migration: %v\n", err)
		os.Exit(1)
	}

	migration, err = os.ReadFile("migrations/031_tool_resilience.sql")
	if err == nil {
		_, err = pool.Exec(ctx, string(migration))
	}
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply Tool resilience migration: %v\n", err)
		os.Exit(1)
	}

	migration, err = os.ReadFile("migrations/032_model_dispatches.sql")
	if err == nil {
		_, err = pool.Exec(ctx, string(migration))
	}
	if err != nil {
		pool.Close()
		_ = pg.Stop()
		_ = os.RemoveAll(runtimePath)
		fmt.Fprintf(os.Stderr, "failed to apply model dispatch migration: %v\n", err)
		os.Exit(1)
	}

	for _, name := range []string{"033_model_canary_purpose.sql", "034_runtime_recovery.sql", "037_model_call_health.sql", "038_workflow_executor_approvals.sql"} {
		migration, err = os.ReadFile("migrations/" + name)
		if err == nil {
			_, err = pool.Exec(ctx, string(migration))
		}
		if err != nil {
			pool.Close()
			_ = pg.Stop()
			_ = os.RemoveAll(runtimePath)
			fmt.Fprintf(os.Stderr, "failed to apply %s: %v\n", name, err)
			os.Exit(1)
		}
	}

	testPool = pool
	code := m.Run()

	pool.Close()
	_ = pg.Stop()
	_ = os.RemoveAll(runtimePath)
	os.Exit(code)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	// Clean tables between tests.
	if _, err := testPool.Exec(ctx, "DELETE FROM runtime_recovery"); err != nil {
		t.Fatalf("failed to clean runtime_recovery: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM runtime_approval_tombstones"); err != nil {
		t.Fatalf("failed to clean runtime_approval_tombstones: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM runtime_approval_quota_tickets"); err != nil {
		t.Fatalf("failed to clean runtime_approval_quota_tickets: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM runtime_approvals"); err != nil {
		t.Fatalf("failed to clean runtime_approvals: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM runtime_binding_key_state"); err != nil {
		t.Fatalf("failed to clean runtime_binding_key_state: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM tool_grant_rate_limits"); err != nil {
		t.Fatalf("failed to clean tool_grant_rate_limits: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM cost_records"); err != nil {
		t.Fatalf("failed to clean cost_records: %v", err)
	}
	if _, err := testPool.Exec(ctx, "TRUNCATE usage_event_receipts"); err != nil {
		t.Fatalf("failed to clean usage_event_receipts: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM resources WHERE kind = 'CognitiveQuota'"); err != nil {
		t.Fatalf("failed to clean CognitiveQuota resources: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM tool_invocation_receipts"); err != nil {
		t.Fatalf("failed to clean tool_invocation_receipts: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM tool_call_reservations"); err != nil {
		t.Fatalf("failed to clean tool_call_reservations: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM memory_mutation_receipts"); err != nil {
		t.Fatalf("failed to clean memory_mutation_receipts: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM token_reservations"); err != nil {
		t.Fatalf("failed to clean token_reservations: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM canary_reservation_dispositions"); err != nil {
		t.Fatalf("failed to clean canary_reservation_dispositions: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM canary_operations"); err != nil {
		t.Fatalf("failed to clean canary_operations: %v", err)
	}
	if _, err := testPool.Exec(ctx, "TRUNCATE canary_operation_tombstones"); err != nil {
		t.Fatalf("failed to clean canary_operation_tombstones: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM instance_events"); err != nil {
		t.Fatalf("failed to clean instance_events: %v", err)
	}
	if _, err := testPool.Exec(ctx, "TRUNCATE cognitive_audit_log"); err != nil {
		t.Fatalf("failed to clean cognitive_audit_log: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM resource_revisions"); err != nil {
		t.Fatalf("failed to clean resource_revisions: %v", err)
	}
	if _, err := testPool.Exec(ctx, "DELETE FROM resources"); err != nil {
		t.Fatalf("failed to clean resources: %v", err)
	}
	return New(testPool, zap.NewNop())
}

func TestToolGrantRateReservationPersistsAcrossStoreInstances(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	reservation := store.ToolGrantRateReservation{Key: "team/grant@1/agent/tool", Now: time.Now().UTC(), CallsPerMinute: 1}
	if err := s.ReserveToolGrantInvocation(ctx, reservation); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	if err := New(testPool, zap.NewNop()).ReserveToolGrantInvocation(ctx, reservation); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("second reservation error = %v, want ErrQuotaExceeded", err)
	}
}

func TestMemoryMutationReceiptPreparationAndCompletion(t *testing.T) {
	first := newTestStore(t)
	second := New(testPool, zap.NewNop())
	ctx := context.Background()
	deadline := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	receipt := store.MemoryMutationReceipt{
		Namespace: "team-a", InvocationID: "memory-1", Binding: "binding-a", Operation: "store",
		Status: store.MemoryMutationPrepared, PreparationToken: "preparation-token-00000000000001",
		PreparationDeadline: &deadline, CreatedAt: time.Now().UTC(),
	}
	prepared, stored, err := first.PrepareMemoryMutation(ctx, receipt)
	if err != nil || !prepared || stored.Status != store.MemoryMutationPrepared {
		t.Fatalf("PrepareMemoryMutation() = (%v, %#v, %v), want prepared receipt", prepared, stored, err)
	}
	prepared, resumed, err := second.PrepareMemoryMutation(ctx, receipt)
	if err != nil || !prepared || resumed.PreparationToken != receipt.PreparationToken {
		t.Fatalf("resumed PrepareMemoryMutation() = (%v, %#v, %v), want exact preparation", prepared, resumed, err)
	}
	changed := receipt
	changed.Binding = "binding-b"
	if _, _, err := first.PrepareMemoryMutation(ctx, changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-binding PrepareMemoryMutation() error = %v, want ErrConflict", err)
	}
	completedAt := time.Now().UTC()
	completed := receipt
	completed.Status = store.MemoryMutationSucceeded
	completed.Result = json.RawMessage(`{"record_id":"record-1"}`)
	completed.PreparationToken = ""
	completed.PreparationDeadline = nil
	completed.CompletedAt = &completedAt
	if err := second.CompleteMemoryMutation(ctx, completed); err != nil {
		t.Fatalf("CompleteMemoryMutation() error = %v", err)
	}
	if released, err := first.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("release completed Memory mutation = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if err := first.CompleteMemoryMutation(ctx, completed); err != nil {
		t.Fatalf("replayed CompleteMemoryMutation() error = %v", err)
	}
}

func TestReleasePreparedMemoryMutationRequiresExactToken(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	deadline := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	receipt := store.MemoryMutationReceipt{
		Namespace: "team-a", InvocationID: "memory-2", Binding: "binding-a", Operation: "delete",
		Status: store.MemoryMutationPrepared, PreparationToken: "preparation-token-00000000000001",
		PreparationDeadline: &deadline, CreatedAt: time.Now().UTC(),
	}
	if _, _, err := s.PrepareMemoryMutation(ctx, receipt); err != nil {
		t.Fatalf("PrepareMemoryMutation() error = %v", err)
	}
	if released, err := s.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, "different-preparation-token-0000001"); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong-token release = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if released, err := s.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil || !released {
		t.Fatalf("exact-token release = (%v, %v), want (true, nil)", released, err)
	}
	if released, err := s.ReleasePreparedMemoryMutation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil || released {
		t.Fatalf("replayed release = (%v, %v), want (false, nil)", released, err)
	}
}

func TestListExpiredMemoryMutationsReturnsBoundedDeadlineOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var cutoff time.Time
	if err := testPool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	cutoff = cutoff.Truncate(time.Microsecond)
	receipts := []store.MemoryMutationReceipt{
		{Namespace: "team-b", InvocationID: "memory-later", Binding: "binding-later", Operation: "store", PreparationToken: "token-later", PreparationDeadline: timePointer(cutoff)},
		{Namespace: "team-a", InvocationID: "memory-first", Binding: "binding-first", Operation: "delete", PreparationToken: "token-first", PreparationDeadline: timePointer(cutoff.Add(-time.Minute))},
		{Namespace: "team-a", InvocationID: "memory-later", Binding: "binding-later-a", Operation: "store", PreparationToken: "token-later-a", PreparationDeadline: timePointer(cutoff)},
		{Namespace: "team-a", InvocationID: "memory-future", Binding: "binding-future", Operation: "delete", PreparationToken: "token-future", PreparationDeadline: timePointer(cutoff.Add(time.Minute))},
	}
	for _, receipt := range receipts {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO memory_mutation_receipts
			 (namespace, invocation_id, binding, operation, status, preparation_token, preparation_deadline)
			 VALUES ($1, $2, $3, $4, 'Prepared', $5, $6)`,
			receipt.Namespace, receipt.InvocationID, receipt.Binding, receipt.Operation,
			receipt.PreparationToken, receipt.PreparationDeadline,
		); err != nil {
			t.Fatalf("insert Memory mutation receipt: %v", err)
		}
	}

	got, err := s.ListExpiredMemoryMutations(ctx, 2)
	if err != nil {
		t.Fatalf("ListExpiredMemoryMutations() error = %v", err)
	}
	want := []store.MemoryMutationReceipt{receipts[1], receipts[2]}
	if len(got) != len(want) {
		t.Fatalf("ListExpiredMemoryMutations() length = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].Namespace != want[index].Namespace || got[index].InvocationID != want[index].InvocationID ||
			got[index].Binding != want[index].Binding || got[index].Operation != want[index].Operation ||
			got[index].PreparationToken != want[index].PreparationToken || got[index].PreparationDeadline == nil ||
			!got[index].PreparationDeadline.Equal(*want[index].PreparationDeadline) {
			t.Fatalf("ListExpiredMemoryMutations()[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
	if got, err := s.ListExpiredMemoryMutations(ctx, 0); err != nil || got != nil {
		t.Fatalf("ListExpiredMemoryMutations(0) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestConcurrentToolGrantRateReservationsCannotOvercommit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	reservation := store.ToolGrantRateReservation{Key: "team/grant@1/agent/tool", Now: time.Now().UTC(), CallsPerMinute: 5}
	var waitGroup sync.WaitGroup
	results := make(chan error, 20)
	for index := 0; index < 20; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			results <- s.ReserveToolGrantInvocation(ctx, reservation)
		}()
	}
	waitGroup.Wait()
	close(results)
	succeeded := 0
	denied := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrQuotaExceeded):
			denied++
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if succeeded != 5 || denied != 15 {
		t.Fatalf("reservations = %d succeeded, %d denied; want 5 and 15", succeeded, denied)
	}
}

func TestToolGrantRateReservationChecksAllWindowsAtomically(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	reservation := store.ToolGrantRateReservation{Key: "team/grant@1/agent/tool", Now: now, CallsPerMinute: 1, CallsPerHour: 1}
	if err := s.ReserveToolGrantInvocation(ctx, reservation); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	reservation.Now = now.Add(2 * time.Minute)
	if err := s.ReserveToolGrantInvocation(ctx, reservation); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("hour-limited reservation error = %v, want ErrQuotaExceeded", err)
	}
	var minuteCount, hourCount int64
	if err := testPool.QueryRow(ctx, `SELECT minute_count, hour_count FROM tool_grant_rate_limits WHERE rate_key = $1`, reservation.Key).Scan(&minuteCount, &hourCount); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if minuteCount != 1 || hourCount != 1 {
		t.Fatalf("rejected reservation changed counters to minute=%d hour=%d", minuteCount, hourCount)
	}
}

func TestCanaryOperationClaimIsDurableAndImmutable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := store.CanaryOperation{OperationID: "canary-1", RequestDigest: "sha256:request-a", Phase: store.CanaryClaimed}

	claimed, operation, err := s.ClaimCanaryOperation(ctx, claim)
	if err != nil || !claimed || operation.OperationID != claim.OperationID || operation.Phase != store.CanaryClaimed || operation.CreatedAt.IsZero() {
		t.Fatalf("ClaimCanaryOperation() = (%v, %#v, %v), want durable claim", claimed, operation, err)
	}
	claimed, operation, err = New(testPool, zap.NewNop()).ClaimCanaryOperation(ctx, claim)
	if err != nil || claimed || operation.RequestDigest != claim.RequestDigest {
		t.Fatalf("duplicate ClaimCanaryOperation() = (%v, %#v, %v), want existing claim", claimed, operation, err)
	}
	claim.RequestDigest = "sha256:request-b"
	if _, _, err := s.ClaimCanaryOperation(ctx, claim); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-digest claim error = %v, want store.ErrConflict", err)
	}
}

func TestCanaryOperationTransitionUsesCompareAndSet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := store.CanaryOperation{OperationID: "canary-1", RequestDigest: "sha256:request-a", Phase: store.CanaryClaimed}
	if _, _, err := s.ClaimCanaryOperation(ctx, claim); err != nil {
		t.Fatal(err)
	}

	fallback := &store.CanaryTerminalRecord{Result: json.RawMessage(`{"outcome":"AccountingUncertain"}`), UsageEvent: json.RawMessage(`{"tokens":16}`), AuditEvent: json.RawMessage(`{"id":"fallback-audit"}`)}
	transitioned, operation, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, fallback)
	if err != nil || !transitioned || operation.Phase != store.CanaryProviderStarted {
		t.Fatalf("TransitionCanaryOperation() = (%v, %#v, %v), want ProviderStarted", transitioned, operation, err)
	}
	transitioned, operation, err = s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, nil)
	if err != nil || transitioned || operation.Phase != store.CanaryProviderStarted {
		t.Fatalf("lost CAS transition = (%v, %#v, %v), want current ProviderStarted", transitioned, operation, err)
	}
	if _, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, "sha256:request-b", store.CanaryProviderStarted, store.CanaryProviderCompleted, nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-digest transition error = %v, want store.ErrConflict", err)
	}
	if _, _, err := s.TransitionCanaryOperation(ctx, "missing", claim.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing transition error = %v, want store.ErrNotFound", err)
	}
}

func TestCanaryOperationPersistsPreCallFinalizationWithoutUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := store.CanaryOperation{OperationID: "canary-pre-call", RequestDigest: "sha256:request", Phase: store.CanaryClaimed}
	if _, _, err := s.ClaimCanaryOperation(ctx, claim); err != nil {
		t.Fatal(err)
	}

	terminal := &store.CanaryTerminalRecord{
		Result:             json.RawMessage(`{"outcome":"ExecutionFailed","closedReason":"TimedOut"}`),
		AuditEvent:         json.RawMessage(`{"id":"pre-call-audit"}`),
		ReservationCreated: true,
	}
	transitioned, operation, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryFinalizing, terminal)
	if err != nil || !transitioned {
		t.Fatalf("pre-call finalization = (%v, %v), want success", transitioned, err)
	}
	if operation.Phase != store.CanaryFinalizing || !operation.ReservationCreated || len(operation.UsageEvent) != 0 {
		t.Fatalf("operation = %#v, want finalizing reservation with no usage", operation)
	}

	reloaded, err := New(testPool, zap.NewNop()).GetCanaryOperation(ctx, claim.OperationID)
	if err != nil || !reloaded.ReservationCreated || len(reloaded.UsageEvent) != 0 {
		t.Fatalf("reloaded operation = (%#v, %v), want durable reservation metadata without usage", reloaded, err)
	}
}

func TestCanaryOperationRejectsPhaseJumpsAndResultOverwrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := store.CanaryOperation{OperationID: "canary-1", RequestDigest: "sha256:request-a", Phase: store.CanaryClaimed}
	if _, _, err := s.ClaimCanaryOperation(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryCompleted, nil); err == nil {
		t.Fatal("phase jump succeeded, want rejection")
	}
	if _, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, &store.CanaryTerminalRecord{Result: json.RawMessage(`{"matched":false}`), AuditEvent: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("incomplete ProviderStarted fallback succeeded, want rejection")
	}
	fallback := &store.CanaryTerminalRecord{Result: json.RawMessage(`{"matched":false}`), UsageEvent: json.RawMessage(`{"tokens":16}`), AuditEvent: json.RawMessage(`{"id":"fallback-audit"}`)}
	if _, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, fallback); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"matched":true}`)
	terminal := &store.CanaryTerminalRecord{Result: result, UsageEvent: json.RawMessage(`{"tokens":3}`), AuditEvent: json.RawMessage(`{"id":"audit"}`)}
	if transitioned, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryProviderStarted, store.CanaryProviderCompleted, terminal); err != nil || !transitioned {
		t.Fatalf("provider completion = (%v, %v), want success", transitioned, err)
	}
	if _, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryProviderCompleted, store.CanaryFinalizing, &store.CanaryTerminalRecord{Result: json.RawMessage(`{"matched":false}`), AuditEvent: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("result overwrite succeeded, want rejection")
	}
	if transitioned, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryProviderCompleted, store.CanaryFinalizing, nil); err != nil || !transitioned {
		t.Fatalf("finalizing transition = (%v, %v), want success", transitioned, err)
	}
	transitioned, completed, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryFinalizing, store.CanaryCompleted, nil)
	var completedResult map[string]bool
	decodeErr := json.Unmarshal(completed.Result, &completedResult)
	if err != nil || !transitioned || decodeErr != nil || !completedResult["matched"] {
		t.Fatalf("completed result = (%v, %s, %v), want preserved %s", transitioned, completed.Result, err, result)
	}
}

func TestCanaryFinalizationAcknowledgementsAreGuardedAndDurable(t *testing.T) {
	ctx := context.Background()
	s := New(testPool, zap.NewNop())
	claim := store.CanaryOperation{OperationID: "canary-finalization", RequestDigest: "sha256:request", Phase: store.CanaryClaimed}
	if _, _, err := s.ClaimCanaryOperation(ctx, claim); err != nil {
		t.Fatalf("ClaimCanaryOperation() error = %v", err)
	}
	if _, err := s.AcknowledgeCanaryFinalization(ctx, claim.OperationID, claim.RequestDigest, store.CanaryUsageApplied); err == nil {
		t.Fatal("AcknowledgeCanaryFinalization() before finalizing succeeded, want error")
	}
	for _, transition := range [][2]store.CanaryOperationPhase{
		{store.CanaryClaimed, store.CanaryProviderStarted},
		{store.CanaryProviderStarted, store.CanaryProviderCompleted},
		{store.CanaryProviderCompleted, store.CanaryFinalizing},
	} {
		var terminal *store.CanaryTerminalRecord
		if transition[1] == store.CanaryProviderStarted || transition[1] == store.CanaryProviderCompleted {
			terminal = &store.CanaryTerminalRecord{Result: json.RawMessage(`{"outcome":"Passed"}`), UsageEvent: json.RawMessage(`{"tokens":3}`), AuditEvent: json.RawMessage(`{"id":"audit"}`)}
		}
		if transitioned, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, transition[0], transition[1], terminal); err != nil || !transitioned {
			t.Fatalf("TransitionCanaryOperation(%s, %s) = (%v, %v), want success", transition[0], transition[1], transitioned, err)
		}
	}
	if _, err := s.AcknowledgeCanaryFinalization(ctx, claim.OperationID, "sha256:other", store.CanaryUsageApplied); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("AcknowledgeCanaryFinalization() changed digest error = %v, want ErrConflict", err)
	}
	for _, step := range []store.CanaryFinalizationStep{
		store.CanaryReservationFinalized,
		store.CanaryUsageApplied,
		store.CanaryAuditDelivered,
		store.CanaryUsageApplied,
	} {
		if _, err := s.AcknowledgeCanaryFinalization(ctx, claim.OperationID, claim.RequestDigest, step); err != nil {
			t.Fatalf("AcknowledgeCanaryFinalization(%s) error = %v", step, err)
		}
	}
	operation, err := New(testPool, zap.NewNop()).AcknowledgeCanaryFinalization(ctx, claim.OperationID, claim.RequestDigest, store.CanaryAuditDelivered)
	if err != nil {
		t.Fatalf("durable AcknowledgeCanaryFinalization() error = %v", err)
	}
	if !operation.ReservationFinalized || !operation.UsageApplied || !operation.AuditDelivered {
		t.Fatalf("durable acknowledgements = %#v, want all true", operation)
	}
	if _, err := s.AcknowledgeCanaryFinalization(ctx, claim.OperationID, claim.RequestDigest, "unknown"); err == nil {
		t.Fatal("AcknowledgeCanaryFinalization() unknown step succeeded, want error")
	}
}

func TestCanaryOperationCompactionPreservesPermanentReplayFence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := store.CanaryOperation{OperationID: "canary-compacted", RequestDigest: "sha256:request", Phase: store.CanaryClaimed}
	if _, _, err := s.ClaimCanaryOperation(ctx, claim); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"operationID":"canary-compacted","expectedBytes":12,"actualBytes":12,"expectedSHA256":"expected","actualSHA256":"actual","promptTokens":7,"completionTokens":3,"totalTokens":10,"providerDurationMillis":25,"traceID":"trace-1","spanID":"span-1","outcome":"Passed"}`)
	terminal := &store.CanaryTerminalRecord{
		Result:             result,
		UsageEvent:         json.RawMessage(`{"event":"usage"}`),
		AuditEvent:         json.RawMessage(`{"event":"audit","secondaryEvidence":"discard-me"}`),
		ReservationCreated: true,
	}
	for _, transition := range [][2]store.CanaryOperationPhase{
		{store.CanaryClaimed, store.CanaryProviderStarted},
		{store.CanaryProviderStarted, store.CanaryProviderCompleted},
		{store.CanaryProviderCompleted, store.CanaryFinalizing},
	} {
		var record *store.CanaryTerminalRecord
		if transition[1] == store.CanaryProviderStarted || transition[1] == store.CanaryProviderCompleted {
			record = terminal
		}
		if transitioned, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, transition[0], transition[1], record); err != nil || !transitioned {
			t.Fatalf("TransitionCanaryOperation(%s, %s) = (%v, %v), want success", transition[0], transition[1], transitioned, err)
		}
	}
	for _, step := range []store.CanaryFinalizationStep{store.CanaryReservationFinalized, store.CanaryUsageApplied, store.CanaryAuditDelivered} {
		if _, err := s.AcknowledgeCanaryFinalization(ctx, claim.OperationID, claim.RequestDigest, step); err != nil {
			t.Fatal(err)
		}
	}
	if transitioned, _, err := s.TransitionCanaryOperation(ctx, claim.OperationID, claim.RequestDigest, store.CanaryFinalizing, store.CanaryCompleted, nil); err != nil || !transitioned {
		t.Fatalf("complete operation = (%v, %v), want success", transitioned, err)
	}
	beforeCompaction, err := s.GetCanaryOperation(ctx, claim.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-2 * time.Hour)
	if _, err := testPool.Exec(ctx, `UPDATE canary_operations SET updated_at = $2 WHERE operation_id = $1`, claim.OperationID, old); err != nil {
		t.Fatal(err)
	}

	var compacted [2]int64
	var compactErrors [2]error
	var compactWG sync.WaitGroup
	for index := range compacted {
		compactWG.Add(1)
		go func() {
			defer compactWG.Done()
			compacted[index], compactErrors[index] = s.CompactCanaryOperationsBefore(ctx, old.Add(time.Minute))
		}()
	}
	compactWG.Wait()
	if compactErrors[0] != nil || compactErrors[1] != nil || compacted[0]+compacted[1] != 1 {
		t.Fatalf("concurrent compaction = (%v, %v), errors = (%v, %v), want one compacted row", compacted[0], compacted[1], compactErrors[0], compactErrors[1])
	}
	reloaded, err := New(testPool, zap.NewNop()).GetCanaryOperation(ctx, claim.OperationID)
	if err != nil || reloaded.Phase != store.CanaryCompleted || !bytes.Equal(reloaded.Result, beforeCompaction.Result) {
		t.Fatalf("compacted replay = (%#v, %v), want identical completed result %s", reloaded, err, beforeCompaction.Result)
	}
	claimed, duplicate, err := New(testPool, zap.NewNop()).ClaimCanaryOperation(ctx, claim)
	if err != nil || claimed || !bytes.Equal(duplicate.Result, beforeCompaction.Result) {
		t.Fatalf("duplicate compacted claim = (%v, %#v, %v), want replay", claimed, duplicate, err)
	}
	changed := claim
	changed.RequestDigest = "sha256:changed"
	if _, _, err := s.ClaimCanaryOperation(ctx, changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-digest compacted claim error = %v, want ErrConflict", err)
	}
	repeated, err := s.CompactCanaryOperationsBefore(ctx, time.Now().UTC())
	if err != nil || repeated != 0 {
		t.Fatalf("repeated compaction = (%d, %v), want (0, nil)", repeated, err)
	}
	var activeCount, tombstoneCount int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM canary_operations WHERE operation_id = $1`, claim.OperationID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM canary_operation_tombstones WHERE operation_id = $1`, claim.OperationID).Scan(&tombstoneCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 0 || tombstoneCount != 1 {
		t.Fatalf("active rows = %d, tombstones = %d, want 0 and 1", activeCount, tombstoneCount)
	}
	if _, err := testPool.Exec(ctx, `UPDATE canary_operation_tombstones SET request_digest = 'changed' WHERE operation_id = $1`, claim.OperationID); err == nil {
		t.Fatal("tombstone update succeeded")
	}
	if _, err := testPool.Exec(ctx, `DELETE FROM canary_operation_tombstones WHERE operation_id = $1`, claim.OperationID); err == nil {
		t.Fatal("tombstone deletion succeeded")
	}
	if err := s.Create(ctx, "AgentInstance", "default", "instance", []byte(`{"metadata":{"name":"instance","namespace":"default"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "AgentInstance", "default", "instance"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCanaryOperation(ctx, claim.OperationID); err != nil {
		t.Fatalf("tombstone after AgentInstance deletion: %v", err)
	}
}

func TestCanaryOperationCompactionSkipsIneligibleRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour)
	rows := []struct {
		id              string
		phase           store.CanaryOperationPhase
		updatedAt       time.Time
		reservationDone bool
		usageEvent      []byte
		usageDone       bool
		auditDone       bool
	}{
		{id: "too-new", phase: store.CanaryCompleted, updatedAt: time.Now().UTC(), reservationDone: true, usageEvent: []byte(`{}`), usageDone: true, auditDone: true},
		{id: "not-completed", phase: store.CanaryFinalizing, updatedAt: old, reservationDone: true, usageEvent: []byte(`{}`), usageDone: true, auditDone: true},
		{id: "reservation-pending", phase: store.CanaryCompleted, updatedAt: old, usageEvent: []byte(`{}`), usageDone: true, auditDone: true},
		{id: "usage-pending", phase: store.CanaryCompleted, updatedAt: old, reservationDone: true, usageEvent: []byte(`{}`), auditDone: true},
		{id: "audit-pending", phase: store.CanaryCompleted, updatedAt: old, reservationDone: true, usageEvent: []byte(`{}`), usageDone: true},
	}
	for _, row := range rows {
		if _, err := testPool.Exec(ctx, `INSERT INTO canary_operations
			(operation_id, request_digest, phase, result, usage_event, audit_event, reservation_finalized, usage_applied, audit_delivered, created_at, updated_at)
			VALUES ($1, 'sha256:request', $2, '{}'::jsonb, $3, '{}'::bytea, $4, $5, $6, $7, $7)`,
			row.id, row.phase, row.usageEvent, row.reservationDone, row.usageDone, row.auditDone, row.updatedAt); err != nil {
			t.Fatal(err)
		}
	}
	compacted, err := s.CompactCanaryOperationsBefore(ctx, old.Add(time.Minute))
	if err != nil || compacted != 0 {
		t.Fatalf("CompactCanaryOperationsBefore() = (%d, %v), want (0, nil)", compacted, err)
	}
	var activeCount, tombstoneCount int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM canary_operations`).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM canary_operation_tombstones`).Scan(&tombstoneCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != len(rows) || tombstoneCount != 0 {
		t.Fatalf("active rows = %d, tombstones = %d, want %d and 0", activeCount, tombstoneCount, len(rows))
	}
}

func TestCanaryOperationCompactionPreservesActiveRowWhenTombstoneExceedsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	operationID := "canary-oversized-result"
	old := time.Now().UTC().Add(-2 * time.Hour)
	result, err := json.Marshal(map[string]string{"payload": string(bytes.Repeat([]byte("x"), 65536))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO canary_operations
		(operation_id, request_digest, phase, result, reservation_finalized, usage_applied, audit_delivered, created_at, updated_at)
		VALUES ($1, 'sha256:request', $2, $3, TRUE, TRUE, TRUE, $4, $4)`,
		operationID, store.CanaryCompleted, result, old); err != nil {
		t.Fatal(err)
	}

	compacted, err := s.CompactCanaryOperationsBefore(ctx, old.Add(time.Minute))
	if err == nil || compacted != 0 {
		t.Fatalf("CompactCanaryOperationsBefore() = (%d, %v), want (0, size constraint error)", compacted, err)
	}
	var activeCount, tombstoneCount int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM canary_operations WHERE operation_id = $1`, operationID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM canary_operation_tombstones WHERE operation_id = $1`, operationID).Scan(&tombstoneCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 || tombstoneCount != 0 {
		t.Fatalf("active rows = %d, tombstones = %d, want 1 and 0", activeCount, tombstoneCount)
	}
}

func TestCanaryOperationConcurrentClaimHasSingleWinner(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	claim := store.CanaryOperation{OperationID: "canary-race", RequestDigest: "sha256:request", Phase: store.CanaryClaimed}
	const contenders = 8
	var wg sync.WaitGroup
	results := make(chan bool, contenders)
	errorsCh := make(chan error, contenders)
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, _, err := s.ClaimCanaryOperation(ctx, claim)
			results <- claimed
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	winners := 0
	for claimed := range results {
		if claimed {
			winners++
		}
	}
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent ClaimCanaryOperation() error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent claim winners = %d, want 1", winners)
	}
}

func TestToolInvocationReceiptPersistsAcrossStoreInstances(t *testing.T) {
	first := newTestStore(t)
	ctx := context.Background()
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	claimed, existing, err := first.ClaimToolInvocation(ctx, receipt)
	if err != nil || !claimed || existing.InvocationID != receipt.InvocationID {
		t.Fatalf("ClaimToolInvocation() = (%v, %#v, %v), want claimed receipt", claimed, existing, err)
	}
	claimed, existing, err = New(testPool, zap.NewNop()).ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || existing.Status != store.ToolInvocationPending {
		t.Fatalf("duplicate ClaimToolInvocation() = (%v, %#v, %v), want stored pending receipt", claimed, existing, err)
	}
	now := time.Now().UTC()
	receipt.Status = store.ToolInvocationSucceeded
	receipt.Output = json.RawMessage(`{"updated":true}`)
	receipt.ExecutionMode = "sandbox"
	receipt.DurationMS = 12
	receipt.CompletedAt = &now
	if err := New(testPool, zap.NewNop()).CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("CompleteToolInvocation() error = %v", err)
	}
	claimed, existing, err = first.ClaimToolInvocation(ctx, receipt)
	var output map[string]bool
	if decodeErr := json.Unmarshal(existing.Output, &output); decodeErr != nil {
		t.Fatalf("unmarshal stored receipt output: %v", decodeErr)
	}
	if err != nil || claimed || existing.Status != store.ToolInvocationSucceeded || !output["updated"] {
		t.Fatalf("completed duplicate ClaimToolInvocation() = (%v, %#v, %v)", claimed, existing, err)
	}
}

func TestToolInvocationReceiptRejectsConflictingBinding(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, _, err := s.ClaimToolInvocation(ctx, store.ToolInvocationReceipt{Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.ClaimToolInvocation(ctx, store.ToolInvocationReceipt{Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-b", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting ClaimToolInvocation() error = %v, want store.ErrConflict", err)
	}
}

func TestToolInvocationReceiptReclaimsAwaitingApproval(t *testing.T) {
	first := newTestStore(t)
	ctx := context.Background()
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-awaiting-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := first.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("initial claim = (%t, %v), want claimed", claimed, err)
	}
	receipt.Status = store.ToolInvocationAwaitingApproval
	if err := first.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("park awaiting approval: %v", err)
	}
	second := New(testPool, zap.NewNop())
	claimed, reclaimed, err := second.ClaimToolInvocation(ctx, receipt)
	if err != nil || !claimed || reclaimed.Status != store.ToolInvocationPending {
		t.Fatalf("approval reclaim = (%t, %#v, %v), want claimed pending", claimed, reclaimed, err)
	}
	reclaimed.Status = store.ToolInvocationSucceeded
	if err := second.CompleteToolInvocation(ctx, reclaimed); err != nil {
		t.Fatalf("complete reclaimed invocation: %v", err)
	}
	claimed, stored, err := first.ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || stored.Status != store.ToolInvocationSucceeded {
		t.Fatalf("terminal replay = (%t, %#v, %v)", claimed, stored, err)
	}
}

func TestToolInvocationReceiptPreparationResumesExactToken(t *testing.T) {
	ctx := context.Background()
	st := New(testPool, zap.NewNop())
	deadline := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-prepared-1", Binding: "binding-a",
		Status: store.ToolInvocationPrepared, PreparationToken: "preparation-token-00000000000001",
		PreparationDeadline: &deadline, CreatedAt: time.Now().UTC(),
	}

	prepared, stored, err := st.PrepareToolInvocation(ctx, receipt)
	if err != nil || !prepared {
		t.Fatalf("PrepareToolInvocation() = (%v, %#v, %v), want prepared receipt", prepared, stored, err)
	}
	prepared, resumed, err := New(testPool, zap.NewNop()).PrepareToolInvocation(ctx, receipt)
	if err != nil || !prepared || resumed.PreparationToken != receipt.PreparationToken || resumed.PreparationDeadline == nil || !resumed.PreparationDeadline.Equal(deadline) {
		t.Fatalf("resumed PrepareToolInvocation() = (%v, %#v, %v), want exact preparation", prepared, resumed, err)
	}

	changed := receipt
	changed.PreparationToken = "preparation-token-00000000000002"
	if _, _, err := st.PrepareToolInvocation(ctx, changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-token PrepareToolInvocation() error = %v, want store.ErrConflict", err)
	}
	if released, err := st.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, changed.PreparationToken); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-token ReleasePreparedToolInvocation() = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if released, err := st.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil || !released {
		t.Fatalf("ReleasePreparedToolInvocation() = (%v, %v), want (true, nil)", released, err)
	}
	if released, err := st.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil || released {
		t.Fatalf("replayed ReleasePreparedToolInvocation() = (%v, %v), want (false, nil)", released, err)
	}
}

func TestToolInvocationReceiptPromotesPendingPreparationAndCompletes(t *testing.T) {
	ctx := context.Background()
	st := New(testPool, zap.NewNop())
	deadline := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-promoted-1", Binding: "binding-a",
		Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := st.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("ClaimToolInvocation() = (%v, %v), want (true, nil)", claimed, err)
	}
	receipt.Status = store.ToolInvocationPrepared
	receipt.PreparationToken = "preparation-token-00000000000001"
	receipt.PreparationDeadline = &deadline
	if prepared, stored, err := st.PrepareToolInvocation(ctx, receipt); err != nil || !prepared || stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("PrepareToolInvocation() = (%v, %#v, %v), want promoted preparation", prepared, stored, err)
	}
	receipt.Status = store.ToolInvocationSucceeded
	completedAt := time.Now().UTC()
	receipt.CompletedAt = &completedAt
	if err := st.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("CompleteToolInvocation(): %v", err)
	}
	claimed, stored, err := st.ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || stored.Status != store.ToolInvocationSucceeded || stored.PreparationToken != "" || stored.PreparationDeadline != nil {
		t.Fatalf("terminal receipt = (%v, %#v, %v), want completed receipt without preparation", claimed, stored, err)
	}
}

func TestToolInvocationReceiptReleasePreservesNonPreparedReceipt(t *testing.T) {
	ctx := context.Background()
	st := New(testPool, zap.NewNop())
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-pending-1", Binding: "binding-a",
		Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := st.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("ClaimToolInvocation() = (%v, %v), want (true, nil)", claimed, err)
	}
	if released, err := st.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, "preparation-token-00000000000001"); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReleasePreparedToolInvocation() = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if claimed, existing, err := st.ClaimToolInvocation(ctx, receipt); err != nil || claimed || existing.Status != store.ToolInvocationPending {
		t.Fatalf("preserved ClaimToolInvocation() = (%v, %#v, %v), want existing pending receipt", claimed, existing, err)
	}
}

func TestToolInvocationReceiptConcurrentApprovalReclaimHasSingleWinner(t *testing.T) {
	first := newTestStore(t)
	ctx := context.Background()
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-awaiting-concurrent", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := first.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("initial claim = (%t, %v), want claimed", claimed, err)
	}
	receipt.Status = store.ToolInvocationAwaitingApproval
	if err := first.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("park awaiting approval: %v", err)
	}

	start := make(chan struct{})
	type result struct {
		claimed bool
		status  store.ToolInvocationReceiptStatus
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func(adapter *Store) {
			<-start
			claimed, stored, err := adapter.ClaimToolInvocation(ctx, receipt)
			results <- result{claimed: claimed, status: stored.Status, err: err}
		}(New(testPool, zap.NewNop()))
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil || result.status != store.ToolInvocationPending {
			t.Fatalf("concurrent reclaim = (%t, %q, %v), want pending without error", result.claimed, result.status, result.err)
		}
		if result.claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent approval reclaim winners = %d, want 1", winners)
	}
}

func TestTokenReservationPersistsAndSettlesAcrossStoreInstances(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 1000)

	reservation := store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}
	created, err := s.CreateTokenReservation(ctx, reservation)
	if err != nil || !created {
		t.Fatalf("CreateTokenReservation() = (%v, %v), want (true, nil)", created, err)
	}
	created, err = New(testPool, zap.NewNop()).CreateTokenReservation(ctx, reservation)
	if err != nil || created {
		t.Fatalf("idempotent CreateTokenReservation() = (%v, %v), want (false, nil)", created, err)
	}

	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 400 || usage.OutputTokens != 300 || usage.TotalTokens != 700 {
		t.Fatalf("reserved usage = %+v, want 400/300/700", usage)
	}

	found, err := New(testPool, zap.NewNop()).SettleTokenReservation(ctx, "run-1", 125, 75)
	if err != nil || !found {
		t.Fatalf("SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
	}
	usage = getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 125 || usage.OutputTokens != 75 || usage.TotalTokens != 200 {
		t.Fatalf("settled usage = %+v, want 125/75/200", usage)
	}
	found, err = s.SettleTokenReservation(ctx, "run-1", 125, 75)
	if err != nil || found {
		t.Fatalf("idempotent SettleTokenReservation() = (%v, %v), want (false, nil)", found, err)
	}
}

func TestToolCallReservationPersistsAndResumesAcrossStoreInstances(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createToolCallQuota(t, s, "team-a", 1)
	reservation := store.ToolCallReservation{
		Namespace:           "team-a",
		InvocationID:        "invoke-1",
		Binding:             "binding-a",
		PreparationToken:    "token-a",
		PreparationDeadline: time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
	}

	created, err := s.PrepareToolCallReservation(ctx, reservation)
	if err != nil || !created {
		t.Fatalf("PrepareToolCallReservation() = (%v, %v), want (true, nil)", created, err)
	}
	created, err = New(testPool, zap.NewNop()).PrepareToolCallReservation(ctx, reservation)
	if err != nil || created {
		t.Fatalf("replayed PrepareToolCallReservation() = (%v, %v), want (false, nil)", created, err)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 1 {
		t.Fatalf("Tool call quota usage = %d, want 1", got)
	}

	conflicts := []store.ToolCallReservation{reservation, reservation, reservation}
	conflicts[0].Binding = "binding-b"
	conflicts[1].PreparationToken = "token-b"
	conflicts[2].PreparationDeadline = reservation.PreparationDeadline.Add(time.Second)
	for _, conflicting := range conflicts {
		if _, err := s.PrepareToolCallReservation(ctx, conflicting); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("conflicting PrepareToolCallReservation(%+v) error = %v, want ErrConflict", conflicting, err)
		}
	}
}

func TestToolCallReservationRejectsExpiredDeadlineWithoutCharging(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createToolCallQuota(t, s, "team-a", 1)
	reservation := store.ToolCallReservation{
		Namespace:           "team-a",
		InvocationID:        "invoke-expired",
		Binding:             "binding-a",
		PreparationToken:    "token-a",
		PreparationDeadline: time.Now().UTC().Add(-time.Minute),
	}

	if _, err := s.PrepareToolCallReservation(ctx, reservation); err == nil || !strings.Contains(err.Error(), "must be in the future") {
		t.Fatalf("PrepareToolCallReservation() error = %v, want future-deadline error", err)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 0 {
		t.Fatalf("Tool call quota usage = %d, want 0", got)
	}
}

func TestListExpiredToolCallReservationsReturnsBoundedDeadlineOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	var cutoff time.Time
	if err := testPool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	cutoff = cutoff.Truncate(time.Microsecond)
	reservations := []store.ToolCallReservation{
		{Namespace: "team-b", InvocationID: "invoke-later", Binding: "binding-later", PreparationToken: "token-later", PreparationDeadline: cutoff},
		{Namespace: "team-a", InvocationID: "invoke-first", Binding: "binding-first", PreparationToken: "token-first", PreparationDeadline: cutoff.Add(-time.Minute)},
		{Namespace: "team-a", InvocationID: "invoke-later", Binding: "binding-later-a", PreparationToken: "token-later-a", PreparationDeadline: cutoff},
		{Namespace: "team-a", InvocationID: "invoke-future", Binding: "binding-future", PreparationToken: "token-future", PreparationDeadline: cutoff.Add(time.Minute)},
	}
	for _, reservation := range reservations {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO tool_call_reservations
			 (namespace, invocation_id, binding, preparation_token, preparation_deadline, quota_periods)
			 VALUES ($1, $2, $3, $4, $5, '{}'::jsonb)`,
			reservation.Namespace, reservation.InvocationID, reservation.Binding,
			reservation.PreparationToken, reservation.PreparationDeadline,
		); err != nil {
			t.Fatalf("insert Tool call reservation: %v", err)
		}
	}

	got, err := s.ListExpiredToolCallReservations(ctx, 2)
	if err != nil {
		t.Fatalf("ListExpiredToolCallReservations() error = %v", err)
	}
	want := []store.ToolCallReservation{reservations[1], reservations[2]}
	if len(got) != len(want) {
		t.Fatalf("ListExpiredToolCallReservations() length = %d, want %d", len(got), len(want))
	}
	for index := range want {
		if got[index].Namespace != want[index].Namespace || got[index].InvocationID != want[index].InvocationID ||
			got[index].Binding != want[index].Binding || got[index].PreparationToken != want[index].PreparationToken ||
			!got[index].PreparationDeadline.Equal(want[index].PreparationDeadline) {
			t.Fatalf("ListExpiredToolCallReservations()[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
	if got, err := s.ListExpiredToolCallReservations(ctx, 0); err != nil || got != nil {
		t.Fatalf("ListExpiredToolCallReservations(limit=0) = (%+v, %v), want (nil, nil)", got, err)
	}
}

func TestToolCallReservationExhaustionLeavesNoReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createToolCallQuota(t, s, "team-a", 1)
	deadline := time.Now().UTC().Add(5 * time.Minute)
	first := store.ToolCallReservation{Namespace: "team-a", InvocationID: "invoke-1", Binding: "binding-a", PreparationToken: "token-a", PreparationDeadline: deadline}
	second := store.ToolCallReservation{Namespace: "team-a", InvocationID: "invoke-2", Binding: "binding-b", PreparationToken: "token-b", PreparationDeadline: deadline}
	if _, err := s.PrepareToolCallReservation(ctx, first); err != nil {
		t.Fatalf("first PrepareToolCallReservation() error = %v", err)
	}
	if _, err := s.PrepareToolCallReservation(ctx, second); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("second PrepareToolCallReservation() error = %v, want ErrQuotaExceeded", err)
	}
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM tool_call_reservations WHERE namespace = $1 AND invocation_id = $2`, second.Namespace, second.InvocationID).Scan(&count); err != nil {
		t.Fatalf("count exhausted Tool call reservation: %v", err)
	}
	if count != 0 {
		t.Fatalf("exhausted Tool call reservation count = %d, want 0", count)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 1 {
		t.Fatalf("Tool call quota usage = %d, want 1", got)
	}
}

func TestConcurrentToolCallReservationsCannotOvercommitQuota(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createToolCallQuota(t, s, "team-a", 1)
	deadline := time.Now().UTC().Add(5 * time.Minute)
	start := make(chan struct{})
	errorsByReservation := make(chan error, 2)
	var group sync.WaitGroup
	for _, id := range []string{"invoke-1", "invoke-2"} {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := s.PrepareToolCallReservation(ctx, store.ToolCallReservation{
				Namespace: "team-a", InvocationID: id, Binding: "binding-" + id,
				PreparationToken: "token-" + id, PreparationDeadline: deadline,
			})
			errorsByReservation <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsByReservation)

	var succeeded, exhausted int
	for err := range errorsByReservation {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrQuotaExceeded):
			exhausted++
		default:
			t.Fatalf("PrepareToolCallReservation() unexpected error = %v", err)
		}
	}
	if succeeded != 1 || exhausted != 1 {
		t.Fatalf("concurrent results = %d success, %d exhausted; want 1 each", succeeded, exhausted)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 1 {
		t.Fatalf("Tool call quota usage = %d, want 1", got)
	}
}

func TestToolCallReservationSettlementPreservesCharge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createToolCallQuota(t, s, "team-a", 1)
	reservation := store.ToolCallReservation{
		Namespace: "team-a", InvocationID: "invoke-1", Binding: "binding-a",
		PreparationToken: "token-a", PreparationDeadline: time.Now().UTC().Add(5 * time.Minute),
	}
	if _, err := s.PrepareToolCallReservation(ctx, reservation); err != nil {
		t.Fatalf("PrepareToolCallReservation() error = %v", err)
	}

	found, err := New(testPool, zap.NewNop()).SettleToolCallReservation(ctx, reservation.Namespace, reservation.InvocationID, reservation.PreparationToken)
	if err != nil || !found {
		t.Fatalf("SettleToolCallReservation() = (%v, %v), want (true, nil)", found, err)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 1 {
		t.Fatalf("settled Tool call quota usage = %d, want 1", got)
	}
	found, err = s.SettleToolCallReservation(ctx, reservation.Namespace, reservation.InvocationID, reservation.PreparationToken)
	if err != nil || found {
		t.Fatalf("replayed SettleToolCallReservation() = (%v, %v), want (false, nil)", found, err)
	}
}

func TestToolCallReservationReleaseRefundsOwnedCharge(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createToolCallQuota(t, s, "team-a", 1)
	reservation := store.ToolCallReservation{
		Namespace: "team-a", InvocationID: "invoke-1", Binding: "binding-a",
		PreparationToken: "token-a", PreparationDeadline: time.Now().UTC().Add(5 * time.Minute),
	}
	if _, err := s.PrepareToolCallReservation(ctx, reservation); err != nil {
		t.Fatalf("PrepareToolCallReservation() error = %v", err)
	}
	if released, err := s.ReleaseToolCallReservation(ctx, reservation.Namespace, reservation.InvocationID, "token-b"); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched ReleaseToolCallReservation() = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 1 {
		t.Fatalf("Tool call quota usage after mismatched release = %d, want 1", got)
	}

	released, err := New(testPool, zap.NewNop()).ReleaseToolCallReservation(ctx, reservation.Namespace, reservation.InvocationID, reservation.PreparationToken)
	if err != nil || !released {
		t.Fatalf("ReleaseToolCallReservation() = (%v, %v), want (true, nil)", released, err)
	}
	if got := getToolCallQuotaUsage(t, s, "team-a"); got != 0 {
		t.Fatalf("released Tool call quota usage = %d, want 0", got)
	}
	released, err = s.ReleaseToolCallReservation(ctx, reservation.Namespace, reservation.InvocationID, reservation.PreparationToken)
	if err != nil || released {
		t.Fatalf("replayed ReleaseToolCallReservation() = (%v, %v), want (false, nil)", released, err)
	}
}

func TestTokenReservationRejectsConflictingReplay(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 1000)
	if _, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 100, ReservedOutput: 100,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}
	_, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 101, ReservedOutput: 100,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting CreateTokenReservation() error = %v, want ErrConflict", err)
	}
}

func TestCanaryReservationSettlementIsPermanentAndImmutable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 1000)
	reservation := store.TokenReservation{ID: "canary-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300}
	if created, err := s.CreateCanaryReservation(ctx, reservation); err != nil || !created {
		t.Fatalf("CreateCanaryReservation() = (%v, %v), want (true, nil)", created, err)
	}
	want := store.CanaryReservationDisposition{ReservationID: "canary-1", State: store.CanaryReservationSettled, InputTokens: 125, OutputTokens: 75}
	got, err := New(testPool, zap.NewNop()).SettleCanaryReservation(ctx, "canary-1", 125, 75)
	if err != nil || got != want {
		t.Fatalf("SettleCanaryReservation() = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 125 || usage.OutputTokens != 75 || usage.TotalTokens != 200 {
		t.Fatalf("settled usage = %+v, want 125/75/200", usage)
	}
	got, err = s.SettleCanaryReservation(ctx, "canary-1", 125, 75)
	if err != nil || got != want {
		t.Fatalf("idempotent SettleCanaryReservation() = (%+v, %v), want (%+v, nil)", got, err, want)
	}
	if _, err := s.SettleCanaryReservation(ctx, "canary-1", 126, 75); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed SettleCanaryReservation() error = %v, want ErrConflict", err)
	}
	if _, err := s.ReleaseCanaryReservation(ctx, "canary-1"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReleaseCanaryReservation() after settle error = %v, want ErrConflict", err)
	}
}

func TestCanaryReservationsInProtectedOperationPhasesSurviveReaper(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 10000)
	fallback := &store.CanaryTerminalRecord{Result: json.RawMessage(`{"outcome":"AccountingUncertain"}`), UsageEvent: json.RawMessage(`{"tokens":700}`), AuditEvent: json.RawMessage(`{"id":"fallback-audit"}`)}
	terminal := &store.CanaryTerminalRecord{Result: json.RawMessage(`{"outcome":"Matched"}`), UsageEvent: json.RawMessage(`{"tokens":200}`), AuditEvent: json.RawMessage(`{"id":"terminal-audit"}`)}
	protectedPhases := []store.CanaryOperationPhase{store.CanaryClaimed, store.CanaryProviderStarted, store.CanaryProviderCompleted}
	for index, phase := range protectedPhases {
		operationID := fmt.Sprintf("canary-protected-%d", index)
		reservation := store.TokenReservation{ID: operationID, Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300}
		if _, err := s.CreateCanaryReservation(ctx, reservation); err != nil {
			t.Fatalf("CreateCanaryReservation(%s) error = %v", phase, err)
		}
		claim := store.CanaryOperation{OperationID: operationID, RequestDigest: "sha256:" + operationID, Phase: store.CanaryClaimed}
		if _, _, err := s.ClaimCanaryOperation(ctx, claim); err != nil {
			t.Fatalf("ClaimCanaryOperation(%s) error = %v", phase, err)
		}
		if phase == store.CanaryProviderStarted || phase == store.CanaryProviderCompleted {
			if transitioned, _, err := s.TransitionCanaryOperation(ctx, operationID, claim.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, fallback); err != nil || !transitioned {
				t.Fatalf("transition %s to ProviderStarted = (%v, %v)", phase, transitioned, err)
			}
		}
		if phase == store.CanaryProviderCompleted {
			if transitioned, _, err := s.TransitionCanaryOperation(ctx, operationID, claim.RequestDigest, store.CanaryProviderStarted, store.CanaryProviderCompleted, terminal); err != nil || !transitioned {
				t.Fatalf("transition to ProviderCompleted = (%v, %v)", transitioned, err)
			}
		}
	}
	if _, err := testPool.Exec(ctx, `UPDATE token_reservations SET created_at = $1 WHERE canary`, time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatalf("backdate canary reservations: %v", err)
	}
	if reaped, err := s.ReapExpiredTokenReservations(ctx, time.Now().Add(-24*time.Hour), 100); err != nil || reaped != 0 {
		t.Fatalf("ReapExpiredTokenReservations() = (%d, %v), want (0, nil)", reaped, err)
	}
	for index, phase := range protectedPhases {
		reservationID := fmt.Sprintf("canary-protected-%d", index)
		want := store.CanaryReservationDisposition{ReservationID: reservationID, State: store.CanaryReservationReleased}
		got, err := s.ReleaseCanaryReservation(ctx, reservationID)
		if err != nil || got != want {
			t.Fatalf("ReleaseCanaryReservation(%s) = (%+v, %v), want (%+v, nil)", phase, got, err, want)
		}
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.TotalTokens != 0 {
		t.Fatalf("released usage = %+v, want zero", usage)
	}
}

func TestConcurrentTokenReservationsCannotOvercommitQuota(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 100)

	start := make(chan struct{})
	errorsByReservation := make(chan error, 2)
	var group sync.WaitGroup
	for _, id := range []string{"run-1", "run-2"} {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := s.CreateTokenReservation(ctx, store.TokenReservation{
				ID: id, Namespace: "team-a", ReservedInput: 60,
			})
			errorsByReservation <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsByReservation)

	var succeeded, exhausted int
	for err := range errorsByReservation {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrQuotaExceeded):
			exhausted++
		default:
			t.Fatalf("CreateTokenReservation() unexpected error = %v", err)
		}
	}
	if succeeded != 1 || exhausted != 1 {
		t.Fatalf("concurrent results = %d success, %d exhausted; want 1 each", succeeded, exhausted)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.TotalTokens != 60 {
		t.Fatalf("usage after concurrent reservations = %+v, want total 60", usage)
	}
}

func TestReapExpiredTokenReservationsRefundsOnlyExpiredRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 2000)
	for _, reservation := range []store.TokenReservation{
		{ID: "expired", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300},
		{ID: "active", Namespace: "team-a", ReservedInput: 100, ReservedOutput: 50},
	} {
		if _, err := s.CreateTokenReservation(ctx, reservation); err != nil {
			t.Fatalf("CreateTokenReservation(%q) error = %v", reservation.ID, err)
		}
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE token_reservations SET created_at = $1 WHERE reservation_id = 'expired'`,
		time.Now().Add(-25*time.Hour),
	); err != nil {
		t.Fatalf("backdate reservation: %v", err)
	}

	reaped, err := s.ReapExpiredTokenReservations(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredTokenReservations() = (%d, %v), want (1, nil)", reaped, err)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 100 || usage.OutputTokens != 50 || usage.TotalTokens != 150 {
		t.Fatalf("usage after reap = %+v, want active reservation usage 100/50/150", usage)
	}
	found, err := s.SettleTokenReservation(ctx, "expired", 0, 0)
	if err != nil || found {
		t.Fatalf("settle reaped reservation = (%v, %v), want (false, nil)", found, err)
	}
	found, err = s.SettleTokenReservation(ctx, "active", 80, 40)
	if err != nil || !found {
		t.Fatalf("settle active reservation = (%v, %v), want (true, nil)", found, err)
	}
}

func TestSettleTokenReservationAfterQuotaResetChargesActualUsage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 2000)
	if _, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}
	createdAt := time.Now().Add(-2 * time.Hour)
	if _, err := testPool.Exec(ctx,
		`UPDATE token_reservations SET created_at = $1 WHERE reservation_id = 'run-1'`, createdAt,
	); err != nil {
		t.Fatalf("backdate reservation: %v", err)
	}

	raw, err := s.Get(ctx, "CognitiveQuota", "team-a", "default")
	if err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	var quota v1alpha1.CognitiveQuota
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("unmarshal CognitiveQuota: %v", err)
	}
	quota.Status.TokenUsage = nil
	quota.Status.LastResetTime = createdAt.Add(time.Hour).UTC().Format(time.RFC3339)
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatalf("marshal CognitiveQuota: %v", err)
	}
	if err := s.Update(ctx, "CognitiveQuota", "team-a", "default", data, quota.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("reset CognitiveQuota: %v", err)
	}

	found, err := s.SettleTokenReservation(ctx, "run-1", 125, 75)
	if err != nil || !found {
		t.Fatalf("SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 125 || usage.OutputTokens != 75 || usage.TotalTokens != 200 {
		t.Fatalf("usage after reset-aware settlement = %+v, want 125/75/200", usage)
	}
}

func TestSettleTokenReservationChargesQuotaCreatedAfterReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}
	createTokenQuota(t, s, "team-a", 2000)

	found, err := s.SettleTokenReservation(ctx, "run-1", 125, 75)
	if err != nil || !found {
		t.Fatalf("SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 125 || usage.OutputTokens != 75 || usage.TotalTokens != 200 {
		t.Fatalf("new quota usage after settlement = %+v, want 125/75/200", usage)
	}
}

func TestSettleTokenReservationChargesRecreatedQuota(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 2000)
	if _, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`DELETE FROM resources WHERE kind = 'CognitiveQuota' AND namespace = 'team-a' AND name = 'default'`,
	); err != nil {
		t.Fatalf("remove original CognitiveQuota: %v", err)
	}
	createTokenQuota(t, s, "team-a", 2000)

	found, err := s.SettleTokenReservation(ctx, "run-1", 125, 75)
	if err != nil || !found {
		t.Fatalf("SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 125 || usage.OutputTokens != 75 || usage.TotalTokens != 200 {
		t.Fatalf("recreated quota usage after settlement = %+v, want 125/75/200", usage)
	}
}

func TestReapExpiredTokenReservationDoesNotRefundNewQuotaPeriod(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 2000)
	if _, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "expired", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}
	createdAt := time.Now().Add(-25 * time.Hour)
	if _, err := testPool.Exec(ctx,
		`UPDATE token_reservations SET created_at = $1 WHERE reservation_id = 'expired'`, createdAt,
	); err != nil {
		t.Fatalf("backdate reservation: %v", err)
	}

	raw, err := s.Get(ctx, "CognitiveQuota", "team-a", "default")
	if err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	var quota v1alpha1.CognitiveQuota
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("unmarshal CognitiveQuota: %v", err)
	}
	quota.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{InputTokens: 20, OutputTokens: 10, TotalTokens: 30}
	quota.Status.LastResetTime = createdAt.Add(time.Hour).UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatalf("marshal CognitiveQuota: %v", err)
	}
	if err := s.Update(ctx, "CognitiveQuota", "team-a", "default", data, quota.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("reset CognitiveQuota: %v", err)
	}

	reaped, err := s.ReapExpiredTokenReservations(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredTokenReservations() = (%d, %v), want (1, nil)", reaped, err)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	if usage.InputTokens != 20 || usage.OutputTokens != 10 || usage.TotalTokens != 30 {
		t.Fatalf("new-period usage after reap = %+v, want unchanged 20/10/30", usage)
	}
}

func TestConcurrentSettleAndReapAdjustReservationOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "team-a", 2000)
	if _, err := s.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "expired", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE token_reservations SET created_at = $1 WHERE reservation_id = 'expired'`,
		time.Now().Add(-25*time.Hour),
	); err != nil {
		t.Fatalf("backdate reservation: %v", err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := s.SettleTokenReservation(ctx, "expired", 125, 75)
		results <- err
	}()
	go func() {
		<-start
		_, err := s.ReapExpiredTokenReservations(ctx, time.Now().Add(-24*time.Hour), 100)
		results <- err
	}()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent settlement/reap error = %v", err)
		}
	}

	var reservationCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM token_reservations WHERE reservation_id = 'expired'`,
	).Scan(&reservationCount); err != nil {
		t.Fatalf("count token reservation: %v", err)
	}
	if reservationCount != 0 {
		t.Fatalf("reservation count = %d, want 0", reservationCount)
	}
	usage := getTokenQuotaUsage(t, s, "team-a")
	settled := usage.InputTokens == 125 && usage.OutputTokens == 75 && usage.TotalTokens == 200
	reaped := usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0
	if !settled && !reaped {
		t.Fatalf("usage after concurrent settlement/reap = %+v, want settled 125/75/200 or reaped 0/0/0", usage)
	}
}

func TestTokenReservationStoreRejectsInvalidAccounting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, reservation := range []store.TokenReservation{
		{Namespace: "team-a"},
		{ID: "run-1"},
		{ID: "run-1", Namespace: "team-a", ReservedInput: -1},
		{ID: "run-1", Namespace: "team-a", ReservedOutput: -1},
	} {
		if _, err := s.CreateTokenReservation(ctx, reservation); err == nil {
			t.Fatalf("CreateTokenReservation(%+v) error = nil", reservation)
		}
	}
	for _, test := range []struct {
		id     string
		input  int64
		output int64
	}{
		{input: 1},
		{id: "run-1", input: -1},
		{id: "run-1", output: -1},
	} {
		if _, err := s.SettleTokenReservation(ctx, test.id, test.input, test.output); err == nil {
			t.Fatalf("SettleTokenReservation(%q, %d, %d) error = nil", test.id, test.input, test.output)
		}
	}
}

func TestTokenReservationQuotaPeriodsMigrationIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	createTokenQuota(t, s, "unreset", 1000)
	createTokenQuota(t, s, "historical", 1000)

	raw, err := s.Get(ctx, "CognitiveQuota", "historical", "default")
	if err != nil {
		t.Fatalf("get historical CognitiveQuota: %v", err)
	}
	var quota v1alpha1.CognitiveQuota
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("unmarshal historical CognitiveQuota: %v", err)
	}
	quota.Status.LastResetTime = "legacy-invalid-marker"
	data, err := json.Marshal(quota)
	if err != nil {
		t.Fatalf("marshal historical CognitiveQuota: %v", err)
	}
	if err := s.Update(ctx, "CognitiveQuota", "historical", "default", data, quota.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update historical CognitiveQuota: %v", err)
	}

	for _, namespace := range []string{"unreset", "historical", "no-quota"} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO token_reservations
			 (reservation_id, namespace, reserved_input, reserved_output, quota_periods)
			 VALUES ($1, $2, 1, 1, '{}'::jsonb)`, "legacy-"+namespace, namespace,
		); err != nil {
			t.Fatalf("insert legacy reservation %q: %v", namespace, err)
		}
	}
	migration, err := os.ReadFile("migrations/007_token_reservation_quota_periods.sql")
	if err != nil {
		t.Fatalf("read migration 007: %v", err)
	}
	for attempt := range 2 {
		if _, err := testPool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply migration 007 attempt %d: %v", attempt+1, err)
		}
	}

	for _, test := range []struct {
		namespace string
	}{
		{namespace: "unreset"},
		{namespace: "historical"},
		{namespace: "no-quota"},
	} {
		var periodData []byte
		if err := testPool.QueryRow(ctx,
			`SELECT quota_periods FROM token_reservations WHERE reservation_id = $1`,
			"legacy-"+test.namespace,
		).Scan(&periodData); err != nil {
			t.Fatalf("get migrated reservation %q: %v", test.namespace, err)
		}
		var got map[string]quotaPeriod
		if err := json.Unmarshal(periodData, &got); err != nil {
			t.Fatalf("unmarshal migrated reservation %q: %v", test.namespace, err)
		}
		if test.namespace == "unreset" {
			period, ok := got["default"]
			if !ok || period.ResourceID == "" || period.ResetTime != "" {
				t.Fatalf("quota periods for %q = %v, want default resource identity with empty reset time", test.namespace, got)
			}
		} else if len(got) != 0 {
			t.Fatalf("quota periods for %q = %v, want empty", test.namespace, got)
		}
	}
}

func TestUsageEventAccountingMigrationIsIdempotent(t *testing.T) {
	newTestStore(t)
	migration, err := os.ReadFile("migrations/011_usage_event_accounting.sql")
	if err != nil {
		t.Fatalf("read migration 011: %v", err)
	}
	for attempt := range 2 {
		if _, err := testPool.Exec(context.Background(), string(migration)); err != nil {
			t.Fatalf("apply migration 011 attempt %d: %v", attempt+1, err)
		}
	}
}

func TestUsageEventImmutabilityMigrationIsIdempotent(t *testing.T) {
	newTestStore(t)
	migration, err := os.ReadFile("migrations/012_usage_event_immutability.sql")
	if err != nil {
		t.Fatalf("read migration 012: %v", err)
	}
	for attempt := range 2 {
		if _, err := testPool.Exec(context.Background(), string(migration)); err != nil {
			t.Fatalf("apply migration 012 attempt %d: %v", attempt+1, err)
		}
	}
}

func TestUsageEventImmutabilityMigrationMatchesHelmBundle(t *testing.T) {
	standalone, err := os.ReadFile("migrations/012_usage_event_immutability.sql")
	if err != nil {
		t.Fatal(err)
	}
	helm, err := os.ReadFile("../../../deploy/helm/kognition/migrations/012_usage_event_immutability.sql")
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(value []byte) []byte {
		return bytes.TrimSpace(bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n")))
	}
	if !bytes.Equal(normalize(standalone), normalize(helm)) {
		t.Fatal("standalone and Helm migration 012 differ")
	}
}

func TestUsagePurposeMigrationIsIdempotentAndMatchesHelmBundle(t *testing.T) {
	newTestStore(t)
	standalone, err := os.ReadFile("migrations/017_usage_purpose.sql")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		if _, err := testPool.Exec(context.Background(), string(standalone)); err != nil {
			t.Fatalf("apply migration 017 attempt %d: %v", attempt+1, err)
		}
	}
	helm, err := os.ReadFile("../../../deploy/helm/kognition/migrations/017_usage_purpose.sql")
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(value []byte) []byte {
		return bytes.TrimSpace(bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n")))
	}
	if !bytes.Equal(normalize(standalone), normalize(helm)) {
		t.Fatal("standalone and Helm migration 017 differ")
	}
}

func TestCanaryOperationTombstoneMigrationIsIdempotentAndMatchesHelmBundle(t *testing.T) {
	newTestStore(t)
	standalone, err := os.ReadFile("migrations/018_canary_operation_tombstones.sql")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		if _, err := testPool.Exec(context.Background(), string(standalone)); err != nil {
			t.Fatalf("apply migration 018 attempt %d: %v", attempt+1, err)
		}
	}
	helm, err := os.ReadFile("../../../deploy/helm/kognition/migrations/018_canary_operation_tombstones.sql")
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(value []byte) []byte {
		return bytes.TrimSpace(bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n")))
	}
	if !bytes.Equal(normalize(standalone), normalize(helm)) {
		t.Fatal("standalone and Helm migration 018 differ")
	}
}

func TestToolGrantRateMigrationIsIdempotentAndMatchesHelmBundle(t *testing.T) {
	newTestStore(t)
	standalone, err := os.ReadFile("migrations/020_tool_grant_rate_limits.sql")
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		if _, err := testPool.Exec(context.Background(), string(standalone)); err != nil {
			t.Fatalf("apply migration 020 attempt %d: %v", attempt+1, err)
		}
	}
	helm, err := os.ReadFile("../../../deploy/helm/kognition/migrations/020_tool_grant_rate_limits.sql")
	if err != nil {
		t.Fatal(err)
	}
	normalize := func(value []byte) []byte {
		return bytes.TrimSpace(bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n")))
	}
	if !bytes.Equal(normalize(standalone), normalize(helm)) {
		t.Fatal("standalone and Helm migration 020 differ")
	}
}

func createTokenQuota(t *testing.T, s *Store, namespace string, maxTotal int64) {
	t.Helper()
	data := fmt.Appendf(nil, `{
		"apiVersion":"kognition.dev/v1alpha1",
		"kind":"CognitiveQuota",
		"metadata":{"name":"default","namespace":%q},
		"spec":{"hard":{},"tokenBudget":{"maxTotalTokens":%d}}
	}`, namespace, maxTotal)
	if err := s.Create(context.Background(), "CognitiveQuota", namespace, "default", data); err != nil {
		t.Fatalf("create CognitiveQuota: %v", err)
	}
}

func createToolCallQuota(t *testing.T, s *Store, namespace string, maxCalls int64) {
	t.Helper()
	data := fmt.Appendf(nil, `{
		"apiVersion":"kognition.dev/v1alpha1",
		"kind":"CognitiveQuota",
		"metadata":{"name":"default","namespace":%q},
		"spec":{"hard":{},"costBudget":{"maxToolCallsPerPeriod":%d}}
	}`, namespace, maxCalls)
	if err := s.Create(context.Background(), "CognitiveQuota", namespace, "default", data); err != nil {
		t.Fatalf("create CognitiveQuota: %v", err)
	}
}

func getToolCallQuotaUsage(t *testing.T, s *Store, namespace string) int64 {
	t.Helper()
	raw, err := s.Get(context.Background(), "CognitiveQuota", namespace, "default")
	if err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	var quota struct {
		Status struct {
			CostUsage struct {
				ToolCalls int64 `json:"toolCalls"`
			} `json:"costUsage"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("unmarshal CognitiveQuota: %v", err)
	}
	return quota.Status.CostUsage.ToolCalls
}

func getTokenQuotaUsage(t *testing.T, s *Store, namespace string) storeQuotaTokenUsage {
	t.Helper()
	raw, err := s.Get(context.Background(), "CognitiveQuota", namespace, "default")
	if err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	var quota struct {
		Status struct {
			TokenUsage storeQuotaTokenUsage `json:"tokenUsage"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("unmarshal CognitiveQuota: %v", err)
	}
	return quota.Status.TokenUsage
}

type storeQuotaTokenUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
	TotalTokens  int64 `json:"totalTokens"`
}

func startTestNotifyListener(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	if err := s.StartNotifyListener(ctx); err != nil {
		t.Fatalf("StartNotifyListener: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.StopNotifyListener(stopCtx); err != nil {
			t.Errorf("StopNotifyListener: %v", err)
		}
	})
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var objA, objB any
	if err := json.Unmarshal(a, &objA); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &objB); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	// Compare re-serialized canonical forms.
	canA, _ := json.Marshal(objA)
	canB, _ := json.Marshal(objB)
	return string(canA) == string(canB)
}

func makeResource(name string, version int64) []byte {
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Agent",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "default",
			"resourceVersion": version,
		},
		"spec": map[string]any{"model": "gpt-4"},
	})
	return data
}

func makeControlPlaneRole(name string) []byte {
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "ControlPlaneRole",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "control",
			"resourceVersion": 1,
		},
		"spec": map[string]any{"rules": []any{map[string]any{
			"effect":               "allow",
			"resources":            []string{"RuntimeApproval"},
			"verbs":                []string{"approve"},
			"workspaces":           []string{"team-a"},
			"runtimeApprovalNames": []string{"approval-a"},
		}}},
	})
	return data
}

func makeControlPlaneRoleBinding(name, roleName string, version int64) []byte {
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "ControlPlaneRoleBinding",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       "control",
			"resourceVersion": version,
		},
		"spec": map[string]any{
			"roleRef": map[string]any{"name": roleName},
			"subjects": []any{map[string]any{
				"method":  "oidc",
				"issuer":  "https://issuer.example",
				"subject": "operator",
			}},
		},
	})
	return data
}

// --- Ping ---

func TestPing(t *testing.T) {
	s := newTestStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping failed: %v", err)
	}
}

// --- Create ---

func TestCreate_Success(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := makeResource("agent-1", 1)
	err := s.Create(ctx, "Agent", "default", "agent-1", data)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Verify it's stored.
	got, err := s.Get(ctx, "Agent", "default", "agent-1")
	if err != nil {
		t.Fatalf("Get after Create failed: %v", err)
	}
	uid, resourceVersion := resourceIdentity(t, got)
	if uid == "" || resourceVersion != 1 {
		t.Fatalf("created identity = uid %q, resourceVersion %d", uid, resourceVersion)
	}
	want := injectResourceIdentity(data, uid, resourceVersion)
	if !jsonEqual(t, got, want) {
		t.Errorf("data mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestControlPlanePolicyPersistenceIdentityNotificationsAndCAS(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.CreateAndGet(ctx, "ControlPlaneRole", "control", "decision", makeControlPlaneRole("decision")); err != nil {
		t.Fatal(err)
	}
	events, err := s.Watch(ctx, "ControlPlaneRoleBinding", store.ListOptions{Namespace: "control"})
	if err != nil {
		t.Fatal(err)
	}
	forged := injectResourceIdentity(makeControlPlaneRoleBinding("operators", "decision", 1), "forged-uid", 99)
	created, err := s.CreateControlPlaneRoleBinding(ctx, "control", "operators", "decision", forged)
	if err != nil {
		t.Fatal(err)
	}
	createdUID, createdVersion := resourceIdentity(t, created)
	if createdUID == "" || createdUID == "forged-uid" || createdVersion != 1 {
		t.Fatalf("created identity = (%q, %d), want backend UID and version 1", createdUID, createdVersion)
	}
	persisted, err := s.Get(ctx, "ControlPlaneRoleBinding", "control", "operators")
	if err != nil {
		t.Fatal(err)
	}
	persistedUID, persistedVersion := resourceIdentity(t, persisted)
	if persistedUID != createdUID || persistedVersion != 1 {
		t.Fatalf("persisted identity = (%q, %d), want (%q, 1)", persistedUID, persistedVersion, createdUID)
	}
	select {
	case event := <-events:
		eventUID, eventVersion := resourceIdentity(t, event.Object)
		if event.Type != store.EventCreated || event.Name != "operators" || eventUID != createdUID || eventVersion != 1 {
			t.Fatalf("create event = %+v identity=(%q, %d)", event, eventUID, eventVersion)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for binding create event")
	}

	updated, err := s.UpdateControlPlaneRoleBinding(ctx, "control", "operators", "decision", makeControlPlaneRoleBinding("operators", "decision", 1), 1)
	if err != nil {
		t.Fatal(err)
	}
	updatedUID, updatedVersion := resourceIdentity(t, updated)
	if updatedUID != createdUID || updatedVersion != 2 {
		t.Fatalf("updated identity = (%q, %d), want (%q, 2)", updatedUID, updatedVersion, createdUID)
	}
	if _, err := s.UpdateControlPlaneRoleBinding(ctx, "control", "operators", "decision", makeControlPlaneRoleBinding("operators", "decision", 1), 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale binding update error = %v, want ErrConflict", err)
	}
	if err := s.DeleteControlPlaneRole(ctx, "control", "decision", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("referenced role delete error = %v, want ErrConflict", err)
	}
	if err := s.DeleteControlPlaneRoleBinding(ctx, "control", "operators", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale binding delete error = %v, want ErrConflict", err)
	}
	if err := s.DeleteControlPlaneRoleBinding(ctx, "control", "operators", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteControlPlaneRole(ctx, "control", "decision", 1); err != nil {
		t.Fatal(err)
	}
}

func TestControlPlaneRoleDeleteRacesBindingCreateWithoutDanglingReference(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for iteration := 0; iteration < 10; iteration++ {
		roleName := fmt.Sprintf("create-target-%d", iteration)
		bindingName := fmt.Sprintf("create-binding-%d", iteration)
		if _, err := s.CreateAndGet(ctx, "ControlPlaneRole", "control", roleName, makeControlPlaneRole(roleName)); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		deleteResult := make(chan error, 1)
		createResult := make(chan error, 1)
		go func() {
			<-start
			deleteResult <- s.DeleteControlPlaneRole(ctx, "control", roleName, 1)
		}()
		go func() {
			<-start
			_, err := s.CreateControlPlaneRoleBinding(ctx, "control", bindingName, roleName, makeControlPlaneRoleBinding(bindingName, roleName, 1))
			createResult <- err
		}()
		close(start)
		deleteErr, createErr := <-deleteResult, <-createResult
		if !((deleteErr == nil && errors.Is(createErr, store.ErrNotFound)) || (errors.Is(deleteErr, store.ErrConflict) && createErr == nil)) {
			t.Fatalf("iteration %d results: delete=%v create=%v", iteration, deleteErr, createErr)
		}
		_, roleErr := s.Get(ctx, "ControlPlaneRole", "control", roleName)
		_, bindingErr := s.Get(ctx, "ControlPlaneRoleBinding", "control", bindingName)
		if bindingErr == nil && roleErr != nil {
			t.Fatalf("iteration %d left binding without role: role=%v binding=%v", iteration, roleErr, bindingErr)
		}
	}
}

func TestControlPlaneRoleDeleteRacesBindingRetargetWithoutDanglingReference(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for iteration := 0; iteration < 10; iteration++ {
		sourceName := fmt.Sprintf("source-%d", iteration)
		targetName := fmt.Sprintf("retarget-%d", iteration)
		bindingName := fmt.Sprintf("retarget-binding-%d", iteration)
		if _, err := s.CreateAndGet(ctx, "ControlPlaneRole", "control", sourceName, makeControlPlaneRole(sourceName)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateAndGet(ctx, "ControlPlaneRole", "control", targetName, makeControlPlaneRole(targetName)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateControlPlaneRoleBinding(ctx, "control", bindingName, sourceName, makeControlPlaneRoleBinding(bindingName, sourceName, 1)); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		deleteResult := make(chan error, 1)
		updateResult := make(chan error, 1)
		go func() {
			<-start
			deleteResult <- s.DeleteControlPlaneRole(ctx, "control", targetName, 1)
		}()
		go func() {
			<-start
			_, err := s.UpdateControlPlaneRoleBinding(ctx, "control", bindingName, targetName, makeControlPlaneRoleBinding(bindingName, targetName, 1), 1)
			updateResult <- err
		}()
		close(start)
		deleteErr, updateErr := <-deleteResult, <-updateResult
		if !((deleteErr == nil && errors.Is(updateErr, store.ErrNotFound)) || (errors.Is(deleteErr, store.ErrConflict) && updateErr == nil)) {
			t.Fatalf("iteration %d results: delete=%v update=%v", iteration, deleteErr, updateErr)
		}
		bindingData, err := s.Get(ctx, "ControlPlaneRoleBinding", "control", bindingName)
		if err != nil {
			t.Fatal(err)
		}
		var binding v1alpha1.ControlPlaneRoleBinding
		if err := json.Unmarshal(bindingData, &binding); err != nil {
			t.Fatal(err)
		}
		if binding.Spec.RoleRef.Name == targetName {
			if _, err := s.Get(ctx, "ControlPlaneRole", "control", targetName); err != nil {
				t.Fatalf("iteration %d retargeted binding references missing role: %v", iteration, err)
			}
		}
	}
}

// TC-0003: Postgres-backed resources remain available after API/process restart.
func TestDurability_AcrossStoreReinitialization(t *testing.T) {
	s1 := newTestStore(t)
	ctx := context.Background()

	data := makeResource("agent-durable", 1)
	if err := s1.Create(ctx, "Agent", "default", "agent-durable", data); err != nil {
		t.Fatalf("create durable resource: %v", err)
	}

	// Simulate a process restart by constructing a fresh store from the same pool.
	s2 := New(testPool, zap.NewNop())
	got, err := s2.Get(ctx, "Agent", "default", "agent-durable")
	if err != nil {
		t.Fatalf("get durable resource after re-init: %v", err)
	}
	uid, resourceVersion := resourceIdentity(t, got)
	if uid == "" || resourceVersion != 1 {
		t.Fatalf("durable identity = uid %q, resourceVersion %d", uid, resourceVersion)
	}
	want := injectResourceIdentity(data, uid, resourceVersion)
	if !jsonEqual(t, got, want) {
		t.Fatalf("durability mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestCreate_AlreadyExists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := makeResource("agent-dup", 1)
	if err := s.Create(ctx, "Agent", "default", "agent-dup", data); err != nil {
		t.Fatalf("first Create: %v", err)
	}

	err := s.Create(ctx, "Agent", "default", "agent-dup", data)
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got: %v", err)
	}
}

// --- Get ---

func TestGet_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Get(context.Background(), "Agent", "default", "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestGet_IgnoresSoftDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := makeResource("deleted-agent", 1)
	if err := s.Create(ctx, "Agent", "default", "deleted-agent", data); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "Agent", "default", "deleted-agent"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Get(ctx, "Agent", "default", "deleted-agent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for soft-deleted resource, got: %v", err)
	}
}

// --- List ---

func TestList_Empty(t *testing.T) {
	s := newTestStore(t)
	results, err := s.List(context.Background(), "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty list, got %d items", len(results))
	}
}

func TestList_MultipleResources(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for i := range 3 {
		name := fmt.Sprintf("agent-%d", i)
		if err := s.Create(ctx, "Agent", "default", name, makeResource(name, 1)); err != nil {
			t.Fatal(err)
		}
	}

	results, err := s.List(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 items, got %d", len(results))
	}
}

func TestList_FilterByNamespace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "Agent", "ns-a", "a1", makeResource("a1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "Agent", "ns-b", "b1", makeResource("b1", 1)); err != nil {
		t.Fatal(err)
	}

	results, err := s.List(ctx, "Agent", store.ListOptions{Namespace: "ns-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 item in ns-a, got %d", len(results))
	}
}

func TestList_ExcludesSoftDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "Agent", "default", "alive", makeResource("alive", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, "Agent", "default", "dead", makeResource("dead", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "Agent", "default", "dead"); err != nil {
		t.Fatal(err)
	}

	results, err := s.List(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 item (soft-deleted excluded), got %d", len(results))
	}
}

func TestList_PaginatesWithinNamespace(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, resource := range []struct {
		namespace string
		name      string
	}{
		{namespace: "tenant-a", name: "a-1"},
		{namespace: "tenant-b", name: "b-1"},
		{namespace: "tenant-a", name: "a-2"},
		{namespace: "tenant-a", name: "a-3"},
	} {
		if err := s.Create(ctx, "Agent", resource.namespace, resource.name, makeResource(resource.name, 1)); err != nil {
			t.Fatalf("create %s/%s: %v", resource.namespace, resource.name, err)
		}
	}

	firstPage, err := s.List(ctx, "Agent", store.ListOptions{Namespace: "tenant-a", Limit: 1})
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if len(firstPage) != 2 {
		t.Fatalf("first page returned %d rows, want limit+1 rows for continuation detection", len(firstPage))
	}
	assertResourceName(t, firstPage[0], "a-1")
	assertResourceName(t, firstPage[1], "a-2")

	secondPage, err := s.List(ctx, "Agent", store.ListOptions{
		Namespace: "tenant-a",
		Limit:     1,
		Continue:  EncodeContinue(1),
	})
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if len(secondPage) != 2 {
		t.Fatalf("second page returned %d rows, want 2", len(secondPage))
	}
	assertResourceName(t, secondPage[0], "a-2")
	assertResourceName(t, secondPage[1], "a-3")

	lastPage, err := s.List(ctx, "Agent", store.ListOptions{
		Namespace: "tenant-a",
		Limit:     1,
		Continue:  EncodeContinue(2),
	})
	if err != nil {
		t.Fatalf("list last page: %v", err)
	}
	if len(lastPage) != 1 {
		t.Fatalf("last page returned %d rows, want 1", len(lastPage))
	}
	assertResourceName(t, lastPage[0], "a-3")
}

func TestList_InvalidContinuationStartsAtBeginning(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"agent-1", "agent-2"} {
		if err := s.Create(ctx, "Agent", "default", name, makeResource(name, 1)); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	for _, token := range []string{"not-base64!", EncodeContinue(-1), EncodeContinue(0)} {
		results, err := s.List(ctx, "Agent", store.ListOptions{Limit: 1, Continue: token})
		if err != nil {
			t.Fatalf("List(Continue=%q) error = %v", token, err)
		}
		if len(results) != 2 {
			t.Fatalf("List(Continue=%q) returned %d rows, want 2", token, len(results))
		}
		assertResourceName(t, results[0], "agent-1")
	}
}

func TestGet_InjectsResourceVersionIntoIrregularJSON(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		data []byte
	}{
		{name: "missing-metadata", data: []byte(`{"kind":"Agent"}`)},
		{name: "non-object-metadata", data: []byte(`{"kind":"Agent","metadata":"invalid"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.Create(ctx, "Agent", "default", tt.name, tt.data); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			got, err := s.Get(ctx, "Agent", "default", tt.name)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			var resource map[string]any
			if err := json.Unmarshal(got, &resource); err != nil {
				t.Fatalf("unmarshal resource: %v", err)
			}
			metadata, ok := resource["metadata"].(map[string]any)
			if !ok || metadata["resourceVersion"] != float64(1) {
				t.Fatalf("metadata = %#v, want injected resourceVersion 1", resource["metadata"])
			}
		})
	}

	invalid := []byte(`not-json`)
	if got := injectResourceVersion(invalid, 7); string(got) != string(invalid) {
		t.Fatalf("injectResourceVersion(invalid JSON) = %q, want original data", got)
	}
}

func TestContinueToken_RoundTrip(t *testing.T) {
	for _, offset := range []int{0, 1, 1000} {
		token := EncodeContinue(offset)
		got, err := decodeContinue(token)
		if err != nil || got != offset {
			t.Fatalf("decodeContinue(EncodeContinue(%d)) = %d, %v", offset, got, err)
		}
	}
	if _, err := decodeContinue("not-base64!"); err == nil {
		t.Fatal("decodeContinue() accepted malformed base64")
	}
	if _, err := decodeContinue(base64.RawURLEncoding.EncodeToString([]byte("not-an-offset"))); err == nil {
		t.Fatal("decodeContinue() accepted a non-numeric offset")
	}
}

func assertResourceName(t *testing.T, data []byte, want string) {
	t.Helper()
	var resource struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &resource); err != nil {
		t.Fatalf("unmarshal resource: %v", err)
	}
	if resource.Metadata.Name != want {
		t.Fatalf("resource name = %q, want %q", resource.Metadata.Name, want)
	}
}

func resourceIdentity(t *testing.T, data []byte) (string, int64) {
	t.Helper()
	var resource struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion int64  `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(data, &resource); err != nil {
		t.Fatalf("unmarshal resource identity: %v", err)
	}
	return resource.Metadata.UID, resource.Metadata.ResourceVersion
}

// --- Update ---

func TestUpdate_Success(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	data := makeResource("updatable", 1)
	created, err := s.CreateAndGet(ctx, "Agent", "default", "updatable", data)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := resourceIdentity(t, created)

	updated := makeResource("updatable", 2)
	err = s.Update(ctx, "Agent", "default", "updatable", updated, 1)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	got, err := s.Get(ctx, "Agent", "default", "updatable")
	if err != nil {
		t.Fatal(err)
	}
	gotUID, gotVersion := resourceIdentity(t, got)
	if gotUID != uid || gotVersion != 2 {
		t.Errorf("identity after update = (%q, %d), want (%q, 2)", gotUID, gotVersion, uid)
	}
}

func TestUpdateAndGet_ReturnsPersistedResourceVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	created, err := s.CreateAndGet(ctx, "Agent", "default", "returned", makeResource("returned", 1))
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := resourceIdentity(t, created)

	forged := injectResourceIdentity(makeResource("returned", 1), "attacker-controlled", 1)
	updated, err := s.UpdateAndGet(ctx, "Agent", "default", "returned", forged, 1)
	if err != nil {
		t.Fatalf("UpdateAndGet failed: %v", err)
	}
	gotUID, gotVersion := resourceIdentity(t, updated)
	if gotUID != uid || gotVersion != 2 {
		t.Fatalf("returned identity = (%q, %d), want (%q, 2)", gotUID, gotVersion, uid)
	}
	persisted, err := s.Get(ctx, "Agent", "default", "returned")
	if err != nil {
		t.Fatal(err)
	}
	persistedUID, _ := resourceIdentity(t, persisted)
	if persistedUID != uid {
		t.Fatalf("persisted UID = %q, want row-backed UID %q", persistedUID, uid)
	}
}

func TestUpdate_ConflictWrongVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "Agent", "default", "conflict", makeResource("conflict", 1)); err != nil {
		t.Fatal(err)
	}

	err := s.Update(ctx, "Agent", "default", "conflict", makeResource("conflict", 2), 99)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict, got: %v", err)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.Update(ctx, "Agent", "default", "ghost", makeResource("ghost", 2), 1)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestUpdate_IncreasesResourceVersion(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "Agent", "default", "versioned", makeResource("versioned", 1)); err != nil {
		t.Fatal(err)
	}

	// Update at v1 → should become v2
	if err := s.Update(ctx, "Agent", "default", "versioned", makeResource("versioned", 2), 1); err != nil {
		t.Fatal(err)
	}

	// Update at v1 again → conflict (now at v2)
	err := s.Update(ctx, "Agent", "default", "versioned", makeResource("versioned", 3), 1)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("expected ErrConflict on stale version, got: %v", err)
	}

	// Update at v2 → success
	if err := s.Update(ctx, "Agent", "default", "versioned", makeResource("versioned", 3), 2); err != nil {
		t.Fatalf("Update at v2 should succeed: %v", err)
	}
}

// --- Delete ---

func TestDelete_Success(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "Agent", "default", "to-delete", makeResource("to-delete", 1)); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, "Agent", "default", "to-delete"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err := s.Get(ctx, "Agent", "default", "to-delete")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got: %v", err)
	}
}

func TestDelete_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.Delete(context.Background(), "Agent", "default", "nonexistent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

func TestDelete_AlreadyDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, "Agent", "default", "double-del", makeResource("double-del", 1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "Agent", "default", "double-del"); err != nil {
		t.Fatal(err)
	}

	// Second delete should return not found (already soft-deleted).
	err := s.Delete(ctx, "Agent", "default", "double-del")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound on double-delete, got: %v", err)
	}
}

// --- Watch ---

func TestWatch_ReceivesCreateEvent(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	data := makeResource("watched", 1)
	if err := s.Create(ctx, "Agent", "default", "watched", data); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		if evt.Type != store.EventCreated {
			t.Errorf("expected ADDED, got %s", evt.Type)
		}
		if evt.Kind != "Agent" || evt.Name != "watched" {
			t.Errorf("unexpected event: %+v", evt)
		}
		if evt.ResourceVersion != 1 {
			t.Errorf("expected resourceVersion=1, got %d", evt.ResourceVersion)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}

func TestWatch_ReceivesUpdateEvent(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created, err := s.CreateAndGet(ctx, "Agent", "default", "watched-upd", makeResource("watched-upd", 1))
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := resourceIdentity(t, created)

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Update(ctx, "Agent", "default", "watched-upd", makeResource("watched-upd", 2), 1); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		if evt.Type != store.EventUpdated {
			t.Errorf("expected MODIFIED, got %s", evt.Type)
		}
		if evt.ResourceVersion != 2 {
			t.Errorf("expected resourceVersion=2, got %d", evt.ResourceVersion)
		}
		gotUID, _ := resourceIdentity(t, evt.Object)
		if gotUID != uid {
			t.Errorf("update watch UID = %q, want %q", gotUID, uid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}

func TestWatch_ReceivesDeleteEvent(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created, err := s.CreateAndGet(ctx, "Agent", "default", "watched-del", makeResource("watched-del", 1))
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := resourceIdentity(t, created)

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, "Agent", "default", "watched-del"); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		if evt.Type != store.EventDeleted {
			t.Errorf("expected DELETED, got %s", evt.Type)
		}
		gotUID, _ := resourceIdentity(t, evt.Object)
		if gotUID != uid {
			t.Errorf("delete tombstone UID = %q, want %q", gotUID, uid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for watch event")
	}
}

func TestWatch_UnsubscribesOnContextCancel(t *testing.T) {
	s := newTestStore(t)
	canceledCtx, cancel := context.WithCancel(context.Background())
	activeCtx, activeCancel := context.WithCancel(context.Background())
	defer activeCancel()

	_, err := s.Watch(canceledCtx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.Watch(activeCtx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	s.mu.RLock()
	before := len(s.subscribers["Agent"])
	s.mu.RUnlock()
	if before != 2 {
		t.Fatalf("expected 2 subscribers, got %d", before)
	}

	cancel()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.RLock()
		subscribers := s.subscribers["Agent"]
		cleanedUp := len(subscribers) == 1 && subscribers[0].ch == active
		s.mu.RUnlock()
		if cleanedUp {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("timed out waiting for canceled subscriber cleanup")
		}
	}

	s.notify(store.WatchEvent{Type: store.EventCreated, Kind: "Agent", Name: "still-active"})
	select {
	case evt := <-active:
		if evt.Name != "still-active" {
			t.Fatalf("active watch event = %+v, want still-active", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("active watcher did not receive event after peer cancellation")
	}
}

func TestWatch_OnlyReceivesMatchingKind(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Create a different kind — should not appear on the Agent watch channel.
	if err := s.Create(ctx, "Tool", "default", "my-tool", makeResource("my-tool", 1)); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		t.Fatalf("unexpected event on Agent channel: %+v", evt)
	case <-time.After(100 * time.Millisecond):
		// Expected — no event.
	}
}

func TestWatch_OnlyReceivesMatchingNamespace(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{Namespace: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}

	s.notify(store.WatchEvent{Type: store.EventCreated, Kind: "Agent", Namespace: "tenant-b", Name: "foreign"})
	s.notify(store.WatchEvent{Type: store.EventCreated, Kind: "Agent", Namespace: "tenant-a", Name: "local"})

	select {
	case evt := <-ch:
		if evt.Namespace != "tenant-a" || evt.Name != "local" {
			t.Fatalf("watch event = %+v, want tenant-a/local", evt)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for namespace-scoped watch event")
	}
}

// --- handleNotification ---

func TestHandleNotification_ParsesAndDispatches(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pre-create the resource so handleNotification can fetch it.
	data := makeResource("notified", 1)
	if err := s.Create(ctx, "Agent", "default", "notified", data); err != nil {
		t.Fatal(err)
	}

	// Drain the create event from watch.
	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate an external pg_notify payload.
	payload := `{"operation":"UPDATE","kind":"Agent","namespace":"default","name":"notified"}`
	s.handleNotification(payload)

	select {
	case evt := <-ch:
		if evt.Type != store.EventUpdated {
			t.Errorf("expected MODIFIED, got %s", evt.Type)
		}
		if evt.Name != "notified" {
			t.Errorf("expected name=notified, got %s", evt.Name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification event")
	}
}

func TestHandleNotification_InvalidPayload(t *testing.T) {
	s := newTestStore(t)
	// Should not panic on invalid JSON.
	s.handleNotification("not json at all")
	s.handleNotification(`{"operation":"UNKNOWN","kind":"Agent","namespace":"default","name":"x"}`)
}

// --- notify with full channel ---

func TestNotify_RequiresResyncWhenChannelFull(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Fill the channel (capacity 64).
	for i := range 64 {
		s.notify(store.WatchEvent{
			Type: store.EventCreated,
			Kind: "Agent",
			Name: fmt.Sprintf("fill-%d", i),
		})
	}

	// Next notify should terminate the lagging watch without blocking.
	done := make(chan struct{})
	go func() {
		s.notify(store.WatchEvent{
			Type: store.EventCreated,
			Kind: "Agent",
			Name: "overflow",
		})
		close(done)
	}()

	select {
	case <-done:
		// Good — notify returned without blocking.
	case <-time.After(time.Second):
		t.Fatal("notify blocked on full channel")
	}

	evt, ok := <-ch
	if !ok || evt.Type != store.EventResyncRequired {
		t.Fatalf("overflow event = %+v, open=%v; want resync-required event", evt, ok)
	}
	if _, open := <-ch; open {
		t.Fatal("lagging watch channel must close after the resync event")
	}
	s.mu.RLock()
	remaining := len(s.subscribers["Agent"])
	s.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("lagging subscriber remains registered: %d", remaining)
	}
}

// --- StartNotifyListener (pg_notify integration) ---

func TestStartNotifyListener_ReceivesDBTriggerEvents(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the LISTEN goroutine.
	startTestNotifyListener(t, s, ctx)

	// Subscribe to watch.
	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Insert directly via SQL to trigger the DB trigger (bypasses Store.Create's notify).
	_, err = testPool.Exec(ctx,
		`INSERT INTO resources (kind, namespace, name, data, resource_version) VALUES ($1, $2, $3, $4, 1)`,
		"Agent", "default", "trigger-test", makeResource("trigger-test", 1),
	)
	if err != nil {
		t.Fatalf("direct insert: %v", err)
	}

	// The trigger fires pg_notify → StartNotifyListener → handleNotification → watch.
	select {
	case evt := <-ch:
		if evt.Type != store.EventCreated {
			t.Errorf("expected ADDED from trigger, got %s", evt.Type)
		}
		if evt.Name != "trigger-test" {
			t.Errorf("expected name=trigger-test, got %s", evt.Name)
		}
		if evt.ResourceVersion != 1 {
			t.Errorf("expected resourceVersion=1, got %d", evt.ResourceVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pg_notify trigger event")
	}
}

func TestStartNotifyListener_IdempotentNoDuplicateDelivery(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestNotifyListener(t, s, ctx)
	if err := s.StartNotifyListener(ctx); err != nil {
		t.Fatalf("StartNotifyListener second call: %v", err)
	}

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Create(ctx, "Agent", "default", "idempotent-once", makeResource("idempotent-once", 1)); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		if evt.Type != store.EventCreated || evt.Name != "idempotent-once" {
			t.Fatalf("unexpected first event: %+v", evt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for create event")
	}

	select {
	case evt := <-ch:
		t.Fatalf("unexpected duplicate event after double listener start: %+v", evt)
	case <-time.After(500 * time.Millisecond):
		// Expected.
	}
}

func TestStartNotifyListener_StopIsIdempotentAndAllowsRestart(t *testing.T) {
	s := newTestStore(t)
	for attempt := 0; attempt < 2; attempt++ {
		listenerCtx, cancel := context.WithCancel(context.Background())
		if err := s.StartNotifyListener(listenerCtx); err != nil {
			cancel()
			t.Fatalf("StartNotifyListener attempt %d: %v", attempt+1, err)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := s.StopNotifyListener(stopCtx); err != nil {
			stopCancel()
			cancel()
			t.Fatalf("StopNotifyListener attempt %d: %v", attempt+1, err)
		}
		if err := s.StopNotifyListener(stopCtx); err != nil {
			stopCancel()
			cancel()
			t.Fatalf("idempotent StopNotifyListener attempt %d: %v", attempt+1, err)
		}
		stopCancel()
		cancel()
	}
	if err := testPool.Ping(context.Background()); err != nil {
		t.Fatalf("pool unusable after listener restart: %v", err)
	}
}

// TestStartNotifyListener_SingleDeliveryViaPgNotify verifies that while the
// listener is active a Store write is delivered exactly once (via pg_notify),
// not also duplicated by the write-path notify.
func TestStartNotifyListener_SingleDeliveryViaPgNotify(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestNotifyListener(t, s, ctx)
	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Create(ctx, "Agent", "default", "once", makeResource("once", 1)); err != nil {
		t.Fatal(err)
	}

	// Exactly one create event should arrive (via pg_notify), carrying the object.
	select {
	case evt := <-ch:
		if evt.Type != store.EventCreated || evt.Name != "once" {
			t.Fatalf("unexpected event: %+v", evt)
		}
		if len(evt.Object) == 0 {
			t.Error("expected object payload on create event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for create event")
	}

	// No duplicate should follow.
	select {
	case evt := <-ch:
		t.Fatalf("unexpected duplicate event: %+v", evt)
	case <-time.After(500 * time.Millisecond):
		// Expected — single delivery.
	}
}

// TestStartNotifyListener_DeleteCarriesTombstonePayload verifies that DELETE
// events delivered via pg_notify include the last-known object (the soft-deleted
// row), matching the write-path behavior.
func TestStartNotifyListener_DeleteCarriesTombstonePayload(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	created, err := s.CreateAndGet(ctx, "Agent", "default", "doomed", makeResource("doomed", 1))
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := resourceIdentity(t, created)
	startTestNotifyListener(t, s, ctx)
	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, "Agent", "default", "doomed"); err != nil {
		t.Fatal(err)
	}

	select {
	case evt := <-ch:
		if evt.Type != store.EventDeleted || evt.Name != "doomed" {
			t.Fatalf("unexpected event: %+v", evt)
		}
		if len(evt.Object) == 0 {
			t.Error("expected tombstone object payload on delete event")
		}
		gotUID, _ := resourceIdentity(t, evt.Object)
		if gotUID != uid {
			t.Errorf("pg_notify tombstone UID = %q, want %q", gotUID, uid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for delete event")
	}
}

// TestStartNotifyListener_CrossStoreFanout verifies watch delivery across
// separate Store instances (simulating distinct processes/replicas) sharing
// the same database.
func TestStartNotifyListener_CrossStoreFanout(t *testing.T) {
	writer := newTestStore(t)
	watcher := New(testPool, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestNotifyListener(t, watcher, ctx)

	ch, err := watcher.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := writer.Create(ctx, "Agent", "default", "xproc", makeResource("xproc", 1)); err != nil {
		t.Fatalf("writer create: %v", err)
	}
	select {
	case evt := <-ch:
		if evt.Type != store.EventCreated || evt.Name != "xproc" {
			t.Fatalf("unexpected create event: %+v", evt)
		}
		if evt.ResourceVersion != 1 {
			t.Fatalf("expected create rv=1, got %d", evt.ResourceVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-store create event")
	}

	if err := writer.Update(ctx, "Agent", "default", "xproc", makeResource("xproc", 2), 1); err != nil {
		t.Fatalf("writer update: %v", err)
	}
	select {
	case evt := <-ch:
		if evt.Type != store.EventUpdated || evt.Name != "xproc" {
			t.Fatalf("unexpected update event: %+v", evt)
		}
		if evt.ResourceVersion != 2 {
			t.Fatalf("expected update rv=2, got %d", evt.ResourceVersion)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-store update event")
	}

	if err := writer.Delete(ctx, "Agent", "default", "xproc"); err != nil {
		t.Fatalf("writer delete: %v", err)
	}
	select {
	case evt := <-ch:
		if evt.Type != store.EventDeleted || evt.Name != "xproc" {
			t.Fatalf("unexpected delete event: %+v", evt)
		}
		if len(evt.Object) == 0 {
			t.Fatal("expected tombstone payload on cross-store delete event")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cross-store delete event")
	}
}

func TestRunListener_ReconnectsAfterConnectionLoss(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn, err := s.acquireAndListen(ctx)
	if err != nil {
		t.Fatalf("acquire listener connection: %v", err)
	}
	var backendPID uint32
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&backendPID); err != nil {
		conn.Release()
		t.Fatalf("get listener backend PID: %v", err)
	}
	s.listening.Store(true)
	listenerDone := make(chan struct{})
	go func() {
		s.runListener(ctx, conn)
		close(listenerDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-listenerDone:
		case <-time.After(2 * time.Second):
			t.Error("timed out waiting for reconnect listener shutdown")
		}
	})

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := testPool.QueryRow(ctx, "SELECT pg_terminate_backend($1)", backendPID).Scan(&terminated); err != nil {
		t.Fatalf("terminate listener backend: %v", err)
	}
	if !terminated {
		t.Fatal("listener backend was not terminated")
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	insertedNames := make(map[string]bool)
	for attempt := 1; ; attempt++ {
		name := fmt.Sprintf("reconnected-%d", attempt)
		if _, err := testPool.Exec(ctx,
			`INSERT INTO resources (kind, namespace, name, data, resource_version) VALUES ($1, $2, $3, $4, 1)`,
			"Agent", "default", name, makeResource(name, 1),
		); err != nil {
			t.Fatalf("insert after listener loss: %v", err)
		}
		insertedNames[name] = true

		select {
		case evt := <-ch:
			if evt.Type != store.EventCreated || !insertedNames[evt.Name] {
				t.Fatalf("event after reconnect = %+v, want create for a post-termination insert", evt)
			}
			return
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("timed out waiting for notification after listener reconnect")
		}
	}
}

// TestNotifyLocal_SuppressedWhenListening verifies the write-path notify is
// suppressed while the listener is active (delivery arrives via pg_notify) and
// dispatches directly otherwise.
func TestNotifyLocal_SuppressedWhenListening(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := s.Watch(ctx, "Agent", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Not listening → notifyLocal dispatches directly.
	s.notifyLocal(store.WatchEvent{Type: store.EventCreated, Kind: "Agent", Name: "a"})
	select {
	case evt := <-ch:
		if evt.Name != "a" {
			t.Fatalf("got %s, want a", evt.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("expected direct delivery when not listening")
	}

	// Listening → notifyLocal is a no-op.
	s.listening.Store(true)
	s.notifyLocal(store.WatchEvent{Type: store.EventCreated, Kind: "Agent", Name: "b"})
	select {
	case evt := <-ch:
		t.Fatalf("expected suppression when listening, got %s", evt.Name)
	case <-time.After(300 * time.Millisecond):
		// Expected.
	}
}

func TestNextBackoff_DoublesWithCap(t *testing.T) {
	if got := nextBackoff(250 * time.Millisecond); got != 500*time.Millisecond {
		t.Errorf("nextBackoff(250ms) = %v, want 500ms", got)
	}
	if got := nextBackoff(20 * time.Second); got != notifyMaxBackoff {
		t.Errorf("nextBackoff(20s) = %v, want %v", got, notifyMaxBackoff)
	}
	if got := nextBackoff(notifyMaxBackoff); got != notifyMaxBackoff {
		t.Errorf("nextBackoff(max) = %v, want %v", got, notifyMaxBackoff)
	}
}

func TestWithJitter_WithinHalfToFull(t *testing.T) {
	base := time.Second
	for range 1000 {
		d := withJitter(base)
		if d < base/2 || d > base {
			t.Fatalf("withJitter(%v) = %v, want in [%v,%v]", base, d, base/2, base)
		}
	}
	if withJitter(0) != 0 {
		t.Error("withJitter(0) = non-zero, want 0")
	}
}

// --- Revision tests ---

func TestCreateRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	spec := []byte(`{"model":"gpt-4"}`)
	data := []byte(`{"status":"ready"}`)

	err := s.CreateRevision(ctx, "Agent", "default", "rev-test", 1, spec, data)
	if err != nil {
		t.Fatalf("CreateRevision: %v", err)
	}

	// Creating same revision again should fail (unique constraint).
	err = s.CreateRevision(ctx, "Agent", "default", "rev-test", 1, spec, data)
	if err == nil {
		t.Fatal("expected error on duplicate revision, got nil")
	}
}

func TestListRevisions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Create multiple revisions.
	for i := int64(1); i <= 3; i++ {
		spec := []byte(fmt.Sprintf(`{"rev":%d}`, i))
		data := []byte(`{}`)
		if err := s.CreateRevision(ctx, "Agent", "default", "list-rev", i, spec, data); err != nil {
			t.Fatalf("CreateRevision %d: %v", i, err)
		}
	}

	revs, err := s.ListRevisions(ctx, "Agent", "default", "list-rev")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 3 {
		t.Fatalf("expected 3 revisions, got %d", len(revs))
	}
	// Should be ordered ascending.
	if revs[0].Revision != 1 || revs[2].Revision != 3 {
		t.Errorf("unexpected ordering: %+v", revs)
	}
}

func TestListRevisions_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	revs, err := s.ListRevisions(ctx, "Agent", "default", "nonexistent")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 0 {
		t.Fatalf("expected 0 revisions, got %d", len(revs))
	}
}

func TestGetRevision(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	spec := []byte(`{"model":"gpt-4"}`)
	data := []byte(`{"output":"hello"}`)
	if err := s.CreateRevision(ctx, "Agent", "default", "get-rev", 5, spec, data); err != nil {
		t.Fatalf("CreateRevision: %v", err)
	}

	rev, err := s.GetRevision(ctx, "Agent", "default", "get-rev", 5)
	if err != nil {
		t.Fatalf("GetRevision: %v", err)
	}
	if rev.Revision != 5 {
		t.Errorf("expected revision 5, got %d", rev.Revision)
	}
	if !jsonEqual(t, rev.Spec, spec) {
		t.Errorf("spec mismatch: %s", rev.Spec)
	}
	if !jsonEqual(t, rev.Data, data) {
		t.Errorf("data mismatch: %s", rev.Data)
	}
}

func TestGetRevision_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetRevision(ctx, "Agent", "default", "no-such", 1)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// --- Instance Events ---

func TestAppendEvent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.AppendEvent(ctx, "AgentInstance", "default", "run-1", store.InstanceEvent{
		Timestamp: "2024-06-01T10:00:00Z",
		Type:      "Normal",
		Reason:    "Started",
		Message:   "Instance started successfully",
	})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func TestAppendEvent_WithDetails(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	details := json.RawMessage(`{"exitCode":1,"stderr":"segfault"}`)
	err := s.AppendEvent(ctx, "AgentInstance", "default", "run-2", store.InstanceEvent{
		Timestamp: "2024-06-01T11:00:00Z",
		Type:      "Warning",
		Reason:    "Failed",
		Message:   "Instance crashed",
		Details:   details,
	})
	if err != nil {
		t.Fatalf("AppendEvent with details: %v", err)
	}

	events, err := s.ListEvents(ctx, "AgentInstance", "default", "run-2")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	if events[0].Details == nil {
		t.Fatal("expected non-nil Details")
	}
	var parsed map[string]any
	if err := json.Unmarshal(events[0].Details, &parsed); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}
	if parsed["exitCode"] != float64(1) {
		t.Fatalf("want exitCode=1, got %v", parsed["exitCode"])
	}
}

func TestListEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Append events with different timestamps to verify ordering.
	events := []store.InstanceEvent{
		{Timestamp: "2024-06-01T10:00:00Z", Type: "Normal", Reason: "Started", Message: "first"},
		{Timestamp: "2024-06-01T10:01:00Z", Type: "Normal", Reason: "Running", Message: "second"},
		{Timestamp: "2024-06-01T10:02:00Z", Type: "Warning", Reason: "OOMKill", Message: "third"},
	}
	for _, ev := range events {
		if err := s.AppendEvent(ctx, "AgentInstance", "default", "run-3", ev); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	got, err := s.ListEvents(ctx, "AgentInstance", "default", "run-3")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events, got %d", len(got))
	}

	// Verify ordering (ASC by created_at).
	if got[0].Reason != "Started" {
		t.Errorf("event[0]: want Started, got %s", got[0].Reason)
	}
	if got[1].Reason != "Running" {
		t.Errorf("event[1]: want Running, got %s", got[1].Reason)
	}
	if got[2].Reason != "OOMKill" {
		t.Errorf("event[2]: want OOMKill, got %s", got[2].Reason)
	}

	// Verify field values on first event.
	if got[0].Type != "Normal" {
		t.Errorf("event[0].Type: want Normal, got %s", got[0].Type)
	}
	if got[0].Message != "first" {
		t.Errorf("event[0].Message: want 'first', got %s", got[0].Message)
	}
	if got[0].Timestamp != "2024-06-01T10:00:00Z" {
		t.Errorf("event[0].Timestamp: want 2024-06-01T10:00:00Z, got %s", got[0].Timestamp)
	}
}

func TestListEvents_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.ListEvents(ctx, "AgentInstance", "default", "nonexistent")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil slice for no events, got %v", got)
	}
}
