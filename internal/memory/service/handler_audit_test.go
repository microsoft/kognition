// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package service_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/memory/service"
	"github.com/kognition-ai/kognition/internal/observability"
)

type auditStore struct {
	entries []observability.AuditEntry
	err     error
}

func (s *auditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *auditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := s.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}

func (s *auditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return append([]observability.AuditEntry(nil), s.entries...), nil
}

type countingBackend struct {
	memory.Backend
	storeCalls  int
	deleteCalls int
}

func (b *countingBackend) Store(ctx context.Context, request memory.StoreRequest) (memory.StoreResponse, error) {
	b.storeCalls++
	return b.Backend.Store(ctx, request)
}

func (b *countingBackend) Delete(ctx context.Context, request memory.DeleteRequest) error {
	b.deleteCalls++
	return b.Backend.Delete(ctx, request)
}

func TestMemoryAuditWriteAheadAndFailureBlocking(t *testing.T) {
	backend := &countingBackend{Backend: inmemory.New()}
	audit := &auditStore{}
	handler := service.NewHandler(backend, zaptest.NewLogger(t), service.WithAuditLog(observability.NewCognitiveAuditLog(audit)))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	provision := httptest.NewRequest(http.MethodPost, "/v1/memory/provision", bytes.NewBufferString(`{"volume_handle":"workspace-a"}`))
	provisionResponse := httptest.NewRecorder()
	mux.ServeHTTP(provisionResponse, provision)
	if provisionResponse.Code != http.StatusCreated {
		t.Fatalf("provision status = %d", provisionResponse.Code)
	}
	storeRequest := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"workspace-a","id":"record-1","content":"secret content"}`))
	storeResponse := httptest.NewRecorder()
	mux.ServeHTTP(storeResponse, storeRequest)
	if storeResponse.Code != http.StatusCreated || backend.storeCalls != 1 || len(audit.entries) != 1 || audit.entries[0].Verb != "store" || audit.entries[0].Workspace != "workspace-a" {
		t.Fatalf("store status=%d calls=%d audit=%#v", storeResponse.Code, backend.storeCalls, audit.entries)
	}
	if bytes.Contains(audit.entries[0].Details, []byte("secret content")) {
		t.Fatalf("memory audit details leaked content: %s", audit.entries[0].Details)
	}
	audit.err = errors.New("audit unavailable")
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/memory/delete/workspace-a/record-1", nil)
	deleteResponse := httptest.NewRecorder()
	mux.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusInternalServerError || backend.deleteCalls != 0 {
		t.Fatalf("delete status=%d calls=%d, want audit failure before backend delete", deleteResponse.Code, backend.deleteCalls)
	}
}

func TestMemoryAuditUsesIdentityOnlyAfterInternalAuthentication(t *testing.T) {
	backend := &countingBackend{Backend: inmemory.New()}
	audit := &auditStore{}
	handler := service.NewHandler(backend, zaptest.NewLogger(t), service.WithInternalToken("internal-token"), service.WithAuditLog(observability.NewCognitiveAuditLog(audit)))
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	provision := httptest.NewRequest(http.MethodPost, "/v1/memory/provision", bytes.NewBufferString(`{"volume_handle":"workspace-a"}`))
	provision.Header.Set("Authorization", "Bearer internal-token")
	provisionResponse := httptest.NewRecorder()
	mux.ServeHTTP(provisionResponse, provision)
	if provisionResponse.Code != http.StatusCreated {
		t.Fatalf("provision status = %d", provisionResponse.Code)
	}

	spoofed := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"workspace-a","id":"record-1","content":"secret"}`))
	spoofed.Header.Set(memory.TrustedAgentIdentityHeader, "spiffe://spoofed")
	spoofed.Header.Set(memory.TrustedWorkspaceHeader, "spoofed-workspace")
	spoofedResponse := httptest.NewRecorder()
	mux.ServeHTTP(spoofedResponse, spoofed)
	if spoofedResponse.Code != http.StatusUnauthorized || len(audit.entries) != 0 {
		t.Fatalf("unauthenticated spoof status=%d audit=%#v", spoofedResponse.Code, audit.entries)
	}

	authorized := httptest.NewRequest(http.MethodPost, "/v1/memory/store", bytes.NewBufferString(`{"volume_handle":"workspace-a","id":"record-1","content":"secret"}`))
	authorized.Header.Set("Authorization", "Bearer internal-token")
	authorized.Header.Set(memory.TrustedAgentIdentityHeader, "spiffe://kognition.local/workspace/team-a/agent/planner")
	authorized.Header.Set(memory.TrustedWorkspaceHeader, "team-a")
	authorizedResponse := httptest.NewRecorder()
	mux.ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusCreated || len(audit.entries) != 1 || audit.entries[0].AgentIdentity != "spiffe://kognition.local/workspace/team-a/agent/planner" || audit.entries[0].Workspace != "team-a" {
		t.Fatalf("authorized status=%d audit=%#v", authorizedResponse.Code, audit.entries)
	}
}
