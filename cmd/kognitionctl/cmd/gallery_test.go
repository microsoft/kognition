// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestLoadGallery_ReturnsSortedItemsAndResources(t *testing.T) {
	items, err := loadGallery()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4", len(items))
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if !slices.IsSorted(ids) {
		t.Errorf("gallery IDs are not sorted: %v", ids)
	}
	item, ok := findGalleryItem("hello-agent")
	if !ok {
		t.Fatal("hello-agent not found")
	}
	if len(item.Resources) != 4 || item.Resources[3] != (galleryResource{Kind: "Agent", Name: "hello-agent"}) {
		t.Errorf("resources = %#v", item.Resources)
	}
}

func TestGalleryListAndYAML(t *testing.T) {
	listRecorder := httptest.NewRecorder()
	handleGalleryList(listRecorder, httptest.NewRequest(http.MethodGet, "/api/gallery", nil))
	if listRecorder.Code != http.StatusOK || !strings.Contains(listRecorder.Body.String(), `"hello-agent"`) {
		t.Fatalf("list status = %d, body = %s", listRecorder.Code, listRecorder.Body.String())
	}

	yamlReq := galleryRequest(http.MethodGet, "/api/gallery/hello-agent/yaml", "hello-agent")
	yamlRecorder := httptest.NewRecorder()
	handleGalleryYAML(yamlRecorder, yamlReq)
	if yamlRecorder.Code != http.StatusOK {
		t.Fatalf("YAML status = %d, body = %s", yamlRecorder.Code, yamlRecorder.Body.String())
	}
	if yamlRecorder.Header().Get("Content-Type") != "application/yaml" || !strings.Contains(yamlRecorder.Body.String(), "kind: Agent") {
		t.Errorf("content type = %q, body = %s", yamlRecorder.Header().Get("Content-Type"), yamlRecorder.Body.String())
	}

	missingReq := galleryRequest(http.MethodGet, "/api/gallery/missing/yaml", "missing")
	missingRecorder := httptest.NewRecorder()
	handleGalleryYAML(missingRecorder, missingReq)
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func TestGalleryImport_CreatesEveryDocumentInRequestedNamespace(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		paths = append(paths, r.URL.Path)
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatal(err)
		}
		meta, _ := obj["metadata"].(map[string]any)
		if meta["namespace"] != "project-b" {
			t.Errorf("namespace = %#v, want project-b", meta["namespace"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := galleryRequest(http.MethodPost, "/api/gallery/hello-agent/import?namespace=project-b", "hello-agent")
	recorder := httptest.NewRecorder()

	handleGalleryImport(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	wantPaths := []string{
		"/apis/kognition.dev/v1alpha1/namespaces/project-b/modelclasses",
		"/apis/kognition.dev/v1alpha1/namespaces/project-b/secretrefs",
		"/apis/kognition.dev/v1alpha1/namespaces/project-b/models",
		"/apis/kognition.dev/v1alpha1/namespaces/project-b/agents",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("paths = %v, want %v", paths, wantPaths)
	}
	var response struct {
		Results []galleryImportResult `json:"results"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 4 {
		t.Fatalf("results = %#v", response.Results)
	}
	for _, result := range response.Results {
		if result.Status != "created" || result.Namespace != "project-b" {
			t.Errorf("result = %#v", result)
		}
	}
}

func TestGalleryImport_ReportsDuplicatesAndHardErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/modelclasses"):
			http.Error(w, "already exists", http.StatusConflict)
		case strings.HasSuffix(r.URL.Path, "/secretrefs"):
			http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"created":true}`))
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := galleryRequest(http.MethodPost, "/api/gallery/hello-agent/import", "hello-agent")
	recorder := httptest.NewRecorder()

	handleGalleryImport(recorder, req)

	if recorder.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Results []galleryImportResult `json:"results"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	statuses := make([]string, 0, len(response.Results))
	for _, result := range response.Results {
		statuses = append(statuses, result.Status)
		if result.Namespace != "team-a" {
			t.Errorf("namespace = %q, want team-a", result.Namespace)
		}
	}
	if !slices.Equal(statuses, []string{"exists", "error", "created", "created"}) {
		t.Errorf("statuses = %v", statuses)
	}
}

func TestGalleryImport_MissingItem(t *testing.T) {
	req := galleryRequest(http.MethodPost, "/api/gallery/missing/import", "missing")
	recorder := httptest.NewRecorder()
	handleGalleryImport(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestIsAlreadyExistsError(t *testing.T) {
	for _, message := range []string{"resource already exists", "AlreadyExists", "ALREADY EXISTS"} {
		if !isAlreadyExistsError(message) {
			t.Errorf("isAlreadyExistsError(%q) = false", message)
		}
	}
	if isAlreadyExistsError("connection refused") {
		t.Error("connection failure classified as already exists")
	}
}

func galleryRequest(method, target, id string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.SetPathValue("id", id)
	return req
}
