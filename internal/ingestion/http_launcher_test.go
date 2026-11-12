// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

type stubPayloadBuilder struct {
	payload     map[string]any
	environment []EnvVar
	err         error
}

func (b stubPayloadBuilder) Build(context.Context, *api.KnowledgeBase, *api.KnowledgeBaseClass) (map[string]any, []EnvVar, error) {
	return b.payload, b.environment, b.err
}

func TestHTTPLauncherSubmitsAndPollsKnowledgeBaseTask(t *testing.T) {
	var mu sync.Mutex
	var submitted taskRequest
	getCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet:
			mu.Lock()
			getCount++
			current := getCount
			mu.Unlock()
			if current == 1 {
				http.NotFound(response, request)
				return
			}
			json.NewEncoder(response).Encode(taskStatus{ID: knowledgeBaseTaskID(testKnowledgeBase()), State: "succeeded", Documents: 2, Chunks: 6, SourceDigest: "sha256:done"})
		case request.Method == http.MethodPost:
			if err := json.NewDecoder(request.Body).Decode(&submitted); err != nil {
				t.Fatalf("decode task request: %v", err)
			}
			response.WriteHeader(http.StatusAccepted)
			json.NewEncoder(response).Encode(taskStatus{ID: submitted.ID, State: "running"})
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	launcher := NewHTTPLauncher(nil, server.URL, server.Client(), nil)
	launcher.builder = stubPayloadBuilder{
		payload:     map[string]any{"kb": map[string]any{"name": "docs"}},
		environment: []EnvVar{{Name: "AZURE_CLIENT_SECRET", Value: "sensitive"}},
	}
	kb := testKnowledgeBase()
	class := &api.KnowledgeBaseClass{}

	state, _, err := launcher.Ensure(context.Background(), kb, class)
	if err != nil || state != intctrl.IngestRunning {
		t.Fatalf("first Ensure = (%v, %v), want running", state, err)
	}
	if submitted.ID != knowledgeBaseTaskID(kb) {
		t.Fatalf("submitted task ID = %q, want %q", submitted.ID, knowledgeBaseTaskID(kb))
	}
	if submitted.Environment["AZURE_CLIENT_SECRET"] != "sensitive" {
		t.Fatal("task-scoped environment was not sent")
	}

	state, stats, err := launcher.Ensure(context.Background(), kb, class)
	if err != nil || state != intctrl.IngestSucceeded {
		t.Fatalf("second Ensure = (%v, %v), want succeeded", state, err)
	}
	if stats.Documents != 2 || stats.Chunks != 6 || stats.SourceDigest != "sha256:done" {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestHTTPLauncherMapsFailureAndResetsTask(t *testing.T) {
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodGet:
			response.Header().Set("Content-Type", "application/json")
			json.NewEncoder(response).Encode(taskStatus{ID: knowledgeBaseTaskID(testKnowledgeBase()), State: "failed", Error: "source unavailable"})
		case http.MethodDelete:
			deleted = true
			response.WriteHeader(http.StatusNoContent)
		default:
			response.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	launcher := NewHTTPLauncher(nil, server.URL, server.Client(), nil)
	state, stats, err := launcher.Ensure(context.Background(), testKnowledgeBase(), &api.KnowledgeBaseClass{})
	if err != nil || state != intctrl.IngestFailed || stats.Error != "source unavailable" {
		t.Fatalf("Ensure = (%v, %+v, %v), want failed", state, stats, err)
	}
	if err := launcher.Reset(context.Background(), testKnowledgeBase()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if !deleted {
		t.Fatal("Reset did not delete the terminal task")
	}
}

func TestHTTPLauncherReturnsPayloadBuildFailureAsIngestFailure(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	launcher := NewHTTPLauncher(nil, server.URL, server.Client(), nil)
	launcher.builder = stubPayloadBuilder{err: errors.New("missing credential")}
	state, stats, err := launcher.Ensure(context.Background(), testKnowledgeBase(), &api.KnowledgeBaseClass{})
	if err != nil || state != intctrl.IngestFailed || stats.Error != "missing credential" {
		t.Fatalf("Ensure = (%v, %+v, %v), want payload failure", state, stats, err)
	}
}

func TestHTTPLauncherLaunchRunUsesStableDistinctTask(t *testing.T) {
	var submitted taskRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodGet {
			http.NotFound(response, request)
			return
		}
		json.NewDecoder(request.Body).Decode(&submitted)
		response.WriteHeader(http.StatusAccepted)
		json.NewEncoder(response).Encode(taskStatus{ID: submitted.ID, State: "running"})
	}))
	defer server.Close()

	launcher := NewHTTPLauncher(nil, server.URL, server.Client(), nil)
	launcher.builder = stubPayloadBuilder{payload: map[string]any{"sources": []any{}}}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "manual", UID: "run-uid"}}
	state, _, taskID, err := launcher.LaunchRun(context.Background(), testKnowledgeBase(), &api.KnowledgeBaseClass{}, run)
	if err != nil || state != intctrl.IngestRunning {
		t.Fatalf("LaunchRun = (%v, %q, %v), want running", state, taskID, err)
	}
	if taskID != indexRunTaskID(run) || taskID == knowledgeBaseTaskID(testKnowledgeBase()) {
		t.Fatalf("task ID = %q", taskID)
	}
}

func testKnowledgeBase() *api.KnowledgeBase {
	return &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"},
		Status:     api.KnowledgeBaseStatus{Generation: 2},
	}
}
