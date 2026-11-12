// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package apiserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
	pgstore "github.com/kognition-ai/kognition/internal/store/postgres"
)

const handlerWatchTestSchema = `
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

CREATE OR REPLACE FUNCTION notify_resource_change() RETURNS trigger AS $$
DECLARE
	op TEXT := TG_ARGV[0];
BEGIN
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
	created_by       TEXT NOT NULL DEFAULT ''
);

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
`

func TestWatch_SSECrossStorePgNotifyFanout(t *testing.T) {
	dsn := os.Getenv("KOGNITION_PGVECTOR_TEST_DSN")
	if dsn == "" {
		t.Fatal("KOGNITION_PGVECTOR_TEST_DSN is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		cancel()
		t.Fatalf("connect admin pgx pool: %v", err)
	}
	schema := fmt.Sprintf("apiserver_watch_%d", time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		adminPool.Close()
		cancel()
		t.Fatalf("create test schema: %v", err)
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse pgx pool config: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect isolated pgx pool: %v", err)
	}
	watchStore := pgstore.New(pool, zap.NewNop())
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := watchStore.StopNotifyListener(stopCtx); err != nil {
			t.Errorf("stop notify listener: %v", err)
		}
		stopCancel()
		cancel()
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
		adminPool.Close()
	})

	if _, err := pool.Exec(ctx, handlerWatchTestSchema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM instance_events; DELETE FROM resource_revisions; DELETE FROM resources;"); err != nil {
		t.Fatalf("clean schema tables: %v", err)
	}

	writerStore := pgstore.New(pool, zap.NewNop())
	if err := watchStore.StartNotifyListener(ctx); err != nil {
		t.Fatalf("start notify listener: %v", err)
	}

	srv := New(Config{Addr: ":0"}, watchStore, zap.NewNop())
	httpSrv := httptest.NewServer(srv.mux)
	defer httpSrv.Close()

	watchReq, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	if err != nil {
		t.Fatalf("build watch request: %v", err)
	}
	resp, err := http.DefaultClient.Do(watchReq)
	if err != nil {
		t.Fatalf("start watch request: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content type, got %q", got)
	}

	dataLineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	reader := bufio.NewReader(resp.Body)
	go func() {
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				errCh <- readErr
				return
			}
			if strings.HasPrefix(line, "data: ") {
				dataLineCh <- strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				return
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	if err := writerStore.Create(ctx, "Agent", "default", "sse-agent", makeWatchAgentResource("sse-agent", 1)); err != nil {
		t.Fatalf("writer create: %v", err)
	}

	select {
	case payload := <-dataLineCh:
		var event struct {
			Type   string         `json:"type"`
			Object map[string]any `json:"object"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("decode SSE payload: %v; payload=%s", err, payload)
		}
		if event.Type != string(store.EventCreated) {
			t.Fatalf("expected event type %q, got %q", store.EventCreated, event.Type)
		}
		meta, _ := event.Object["metadata"].(map[string]any)
		if meta["name"] != "sse-agent" {
			t.Fatalf("expected object name sse-agent, got %v", meta["name"])
		}
		if meta["resourceVersion"] != float64(1) {
			t.Fatalf("expected resourceVersion 1, got %v", meta["resourceVersion"])
		}
	case readErr := <-errCh:
		if readErr != nil && !strings.Contains(readErr.Error(), "context canceled") {
			t.Fatalf("watch stream error: %v", readErr)
		}
		t.Fatal("watch stream ended before event arrived")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}
