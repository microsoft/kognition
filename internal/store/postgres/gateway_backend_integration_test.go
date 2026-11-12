// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/observability"
)

func TestStandaloneGatewayBackendPersistsAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	config := gatewaybackend.Config{Mode: "standalone", DatabaseURL: "postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable"}
	backend, err := gatewaybackend.Open(ctx, config, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	workspace := "gateway-" + uuid.NewString()
	resource := json.RawMessage(`{"apiVersion":"kognition.dev/v1alpha1","kind":"Agent","metadata":{"name":"source"},"spec":{}}`)
	if err := backend.Store.Create(ctx, "Agent", workspace, "source", resource); err != nil {
		backend.Close()
		t.Fatal(err)
	}
	entry := observability.AuditEntry{ID: uuid.NewString(), Timestamp: time.Now().UTC(), Workspace: workspace, AgentIdentity: "source", EventType: observability.AuditCommunication, Details: json.RawMessage(`{"decision":"deny"}`)}
	if applied, err := backend.Audit.ApplyAuditEntry(ctx, entry); err != nil || !applied {
		backend.Close()
		t.Fatalf("persist evidence = %v, %v", applied, err)
	}
	backend.Close()
	backend, err = gatewaybackend.Open(ctx, config, zaptest.NewLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	defer func() { _ = backend.Store.Delete(context.Background(), "Agent", workspace, "source") }()
	if _, err := backend.Store.Get(ctx, "Agent", workspace, "source"); err != nil {
		t.Fatalf("resource lost after restart: %v", err)
	}
	if applied, err := backend.Audit.ApplyAuditEntry(ctx, entry); err != nil || applied {
		t.Fatalf("replayed evidence = %v, %v; want idempotent replay", applied, err)
	}
	entries, err := backend.Audit.Query(ctx, observability.AuditFilter{Workspace: workspace})
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("evidence after restart = %v, %v; want one original entry", entries, err)
	}
}
