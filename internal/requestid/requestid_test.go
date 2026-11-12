// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package requestid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestNewContextAndFromContext(t *testing.T) {
	ctx := context.Background()
	if got := FromContext(ctx); got != "" {
		t.Fatalf("expected empty string from bare context, got %q", got)
	}

	uid := "test-uid-123"
	ctx = NewContext(ctx, uid)
	if got := FromContext(ctx); got != uid {
		t.Fatalf("expected %q, got %q", uid, got)
	}
}

func TestGenerate(t *testing.T) {
	uid1 := Generate()
	uid2 := Generate()

	if uid1 == "" {
		t.Fatal("Generate returned empty string")
	}
	if uid1 == uid2 {
		t.Fatal("Generate returned duplicate UIDs")
	}
	// UUID v4 format: 8-4-4-4-12
	if len(uid1) != 36 {
		t.Fatalf("expected UUID length 36, got %d", len(uid1))
	}
}

func TestZapField_WithUID(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	ctx := NewContext(context.Background(), "req-abc")
	logger.Info("test", ZapField(ctx))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["requestUID"] != "req-abc" {
		t.Fatalf("expected requestUID=req-abc, got %v", fields["requestUID"])
	}
}

func TestZapField_WithoutUID(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	ctx := context.Background()
	logger.Info("test", ZapField(ctx))

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 log entry, got %d", len(entries))
	}
	fields := entries[0].ContextMap()
	if _, exists := fields["requestUID"]; exists {
		t.Fatal("expected no requestUID field when context has no UID")
	}
}

func TestMiddleware(t *testing.T) {
	var capturedUID string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedUID = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	handler := Middleware(inner)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if capturedUID == "" {
		t.Fatal("middleware did not inject requestUID into context")
	}
	if len(capturedUID) != 36 {
		t.Fatalf("expected UUID length 36, got %d (%q)", len(capturedUID), capturedUID)
	}
}

func TestMiddleware_UniquePer_Request(t *testing.T) {
	var uids []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uids = append(uids, FromContext(r.Context()))
	})

	handler := Middleware(inner)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	if len(uids) != 3 {
		t.Fatalf("expected 3 UIDs, got %d", len(uids))
	}
	seen := make(map[string]bool)
	for _, uid := range uids {
		if seen[uid] {
			t.Fatalf("duplicate UID: %s", uid)
		}
		seen[uid] = true
	}
}
