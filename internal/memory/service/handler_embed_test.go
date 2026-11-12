// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/memory/service"
)

// fakeEmbedder implements service.Embedder for tests.
type fakeEmbedder struct {
	vec   []float32
	err   error
	calls int
}

type failingSearchBackend struct {
	memory.Backend
}

type failingListBackend struct {
	memory.Backend
}

type recordingSearchBackend struct {
	memory.Backend
	request memory.SearchRequest
}

type fixedScoreBackend struct {
	memory.Backend
	score float64
}

func (b failingSearchBackend) Search(context.Context, memory.SearchRequest) ([]memory.SearchResult, error) {
	return nil, errors.New("semantic backend unavailable")
}

func (b failingListBackend) List(context.Context, memory.ListRequest) ([]memory.Record, error) {
	return nil, errors.New("lexical backend unavailable")
}

func (b *recordingSearchBackend) Search(_ context.Context, req memory.SearchRequest) ([]memory.SearchResult, error) {
	b.request = req
	return []memory.SearchResult{{
		Record: memory.Record{ID: "r1", Content: "precomputed", Metadata: map[string]string{"tag": "shared"}},
		Score:  0.73,
	}}, nil
}

func (b fixedScoreBackend) Search(_ context.Context, req memory.SearchRequest) ([]memory.SearchResult, error) {
	if b.score < req.MinScore {
		return nil, nil
	}
	return []memory.SearchResult{{
		Record: memory.Record{ID: "candidate", Content: "unrelated"},
		Score:  b.score,
	}}, nil
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

func setupServerWithEmbedder(t *testing.T, emb service.Embedder) *httptest.Server {
	t.Helper()
	backend := inmemory.New()
	logger := zaptest.NewLogger(t)
	h := service.NewHandler(backend, logger, service.WithEmbedder(emb))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

func setupServerWithBackend(t *testing.T, backend memory.Backend, emb service.Embedder) *httptest.Server {
	t.Helper()
	h := service.NewHandler(backend, zaptest.NewLogger(t), service.WithEmbedder(emb))
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return httptest.NewServer(mux)
}

func provision(t *testing.T, url, vol string) {
	t.Helper()
	body, _ := json.Marshal(service.ProvisionRequest{VolumeHandle: vol, EmbeddingDimension: 3})
	resp, err := http.Post(url+"/v1/memory/provision", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	resp.Body.Close()
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return resp
}

// TC-0279 (search half): raw content remains searchable when no embedder is
// configured, so deployments degrade to lexical recall instead of losing memory.
func TestSearch_TextQueryFallsBackWithoutEmbedder(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "policy",
		Content:      "Every release must include a software bill of materials.",
	})
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "software bill materials",
		TopK:         3,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 fallback search, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "policy" {
		t.Fatalf("expected lexical policy hit, got %+v", results)
	}
}

// TC-0277: an unrelated text query produces no service results; the runtime
// converts this empty response to the literal "No relevant memories found."
func TestSearch_UnrelatedTextReturnsNoResults(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	srv := setupServerWithEmbedder(t, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "unrelated",
		Content:      "The build completed on Tuesday.",
		Embedding:    []float32{0, 1, 0},
	})
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "shared policy",
		MinScore:     0.5,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no unrelated results, got %+v", results)
	}
}

func TestSearch_SemanticMinScoreBoundary(t *testing.T) {
	tests := []struct {
		name        string
		score       float32
		wantResults int
	}{
		{name: "below", score: 0.49, wantResults: 0},
		{name: "above", score: 0.51, wantResults: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emb := &fakeEmbedder{vec: []float32{1, 0}}
			srv := setupServerWithEmbedder(t, emb)
			defer srv.Close()
			provision(t, srv.URL, "vol")

			other := float32(1 - float64(tc.score)*float64(tc.score))
			resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
				VolumeHandle: "vol",
				ID:           "candidate",
				Content:      "unrelated",
				Embedding:    []float32{tc.score, float32(math.Sqrt(float64(other)))},
			})
			resp.Body.Close()

			resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
				VolumeHandle: "vol",
				Query:        "semantic query",
				MinScore:     0.5,
			})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			var results []service.SearchMemoryResult
			if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(results) != tc.wantResults {
				t.Fatalf("score %.2f returned %d results, want %d: %+v", tc.score, len(results), tc.wantResults, results)
			}
		})
	}
}

func TestSearch_SemanticMinScoreIsInclusive(t *testing.T) {
	backend := fixedScoreBackend{Backend: inmemory.New(), score: 0.5}
	srv := setupServerWithBackend(t, backend, &fakeEmbedder{vec: []float32{1, 0}})
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "semantic query",
		MinScore:     0.5,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Score != 0.5 {
		t.Fatalf("expected exact threshold score to be included, got %+v", results)
	}
}

// TC-0278: metadata filtering returns the shared record and excludes records
// with other tags before ranking and top-k selection.
func TestSearch_LexicalFallbackHonorsMetadataTopKAndMinScore(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	records := []service.StoreMemoryRequest{
		{VolumeHandle: "vol", ID: "best", Content: "alpha alpha beta", Metadata: map[string]string{"tag": "shared"}},
		{VolumeHandle: "vol", ID: "weak", Content: "alpha unrelated words", Metadata: map[string]string{"tag": "shared"}},
		{VolumeHandle: "vol", ID: "filtered", Content: "alpha alpha beta", Metadata: map[string]string{"tag": "private"}},
	}
	for _, record := range records {
		resp := postJSON(t, srv.URL+"/v1/memory/store", record)
		resp.Body.Close()
	}

	resp := postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "alpha beta",
		TopK:         1,
		MinScore:     0.75,
		Metadata:     map[string]string{"tag": "shared"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "best" || results[0].Score != 1 {
		t.Fatalf("expected one normalized best hit, got %+v", results)
	}
}

func TestSearch_HybridFusionPrioritizesSharedHit(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	srv := setupServerWithEmbedder(t, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	records := []service.StoreMemoryRequest{
		{VolumeHandle: "vol", ID: "both", Content: "release policy", Embedding: []float32{1, 0, 0}},
		{VolumeHandle: "vol", ID: "semantic", Content: "unrelated", Embedding: []float32{0.8, 0.6, 0}},
		{VolumeHandle: "vol", ID: "lexical", Content: "release policy release policy", Embedding: []float32{0, 1, 0}},
	}
	for _, record := range records {
		resp := postJSON(t, srv.URL+"/v1/memory/store", record)
		resp.Body.Close()
	}

	resp := postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "release policy",
		TopK:         3,
		MinScore:     0.6,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "both" || results[0].Score != 1 {
		t.Fatalf("expected only shared hybrid hit above min_score, got %+v", results)
	}
}

func TestSearch_EmbedFailureUsesLexicalFallback(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("embedder down")}
	srv := setupServerWithEmbedder(t, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "fallback",
		Content:      "durable fallback content",
		Embedding:    []float32{1, 0, 0},
	})
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "durable content",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 fallback search, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "fallback" {
		t.Fatalf("expected lexical fallback hit, got %+v", results)
	}
}

func TestSearch_SemanticFailureUsesLexicalFallback(t *testing.T) {
	backend := failingSearchBackend{Backend: inmemory.New()}
	srv := setupServerWithBackend(t, backend, &fakeEmbedder{vec: []float32{1, 0, 0}})
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "fallback",
		Content:      "durable fallback content",
		Metadata:     map[string]string{"scope": "shared"},
	})
	resp.Body.Close()
	resp = postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "excluded",
		Content:      "durable fallback content",
		Metadata:     map[string]string{"scope": "private"},
	})
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "durable content",
		Metadata:     map[string]string{"scope": "shared"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 lexical fallback, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Record.ID != "fallback" {
		t.Fatalf("expected lexical fallback hit, got %+v", results)
	}
}

func TestSearch_DirectVectorFailureReturnsInternalServerError(t *testing.T) {
	backend := failingSearchBackend{Backend: inmemory.New()}
	srv := setupServerWithBackend(t, backend, nil)
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Vector:       []float32{1, 0, 0},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	var body service.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "search operation failed" {
		t.Fatalf("unexpected error response: %+v", body)
	}
}

func TestSearch_LexicalListFailureReturnsInternalServerError(t *testing.T) {
	backend := failingListBackend{Backend: inmemory.New()}
	srv := setupServerWithBackend(t, backend, nil)
	defer srv.Close()

	resp := postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "durable content",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}
	var body service.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "search operation failed" {
		t.Fatalf("unexpected error response: %+v", body)
	}
}

func TestSearch_VectorOrQueryRequired(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{VolumeHandle: "vol"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TC-0279 (store half): with no embedder, store succeeds and persists no embedding.
func TestStore_SucceedsWithoutEmbedder(t *testing.T) {
	srv := setupTestServer(t)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "r1",
		Content:      "an unembedded note",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	rec := getRecord(t, srv.URL, "vol", "r1")
	if len(rec.Embedding) != 0 {
		t.Fatalf("expected no embedding, got %v", rec.Embedding)
	}
}

func TestStore_EmbedsOnWrite(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	srv := setupServerWithEmbedder(t, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "r1",
		Content:      "embed me",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	if emb.calls != 1 {
		t.Fatalf("expected embedder called once on write, got %d", emb.calls)
	}

	rec := getRecord(t, srv.URL, "vol", "r1")
	if len(rec.Embedding) != 3 || rec.Embedding[0] != 1 {
		t.Fatalf("expected embedding [1 0 0], got %v", rec.Embedding)
	}
}

func TestStore_EmbedFailureStillStores(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("embedder down")}
	srv := setupServerWithEmbedder(t, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "r1",
		Content:      "keep me even if embedding fails",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 (best-effort), got %d", resp.StatusCode)
	}
	rec := getRecord(t, srv.URL, "vol", "r1")
	if len(rec.Embedding) != 0 {
		t.Fatalf("expected no embedding after failure, got %v", rec.Embedding)
	}
}

// TC-0276: semantic search ranks the SBOM record first even though its content
// has no lexical overlap with the "shared policy" query.
func TestSearch_TextQueryEmbedsAndReturnsMatch(t *testing.T) {
	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	srv := setupServerWithEmbedder(t, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	// Store a record whose embedding matches what the embedder produces.
	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "sbom",
		Content:      "Every release must include an SBOM.",
		Embedding:    []float32{1, 0, 0},
	})
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Query:        "shared policy",
		TopK:         3,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) == 0 || results[0].Record.ID != "sbom" {
		t.Fatalf("expected top hit sbom, got %+v", results)
	}
}

func TestSearch_PrecomputedVectorBypassesEmbedder(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("must not be called")}
	backend := &recordingSearchBackend{Backend: inmemory.New()}
	srv := setupServerWithBackend(t, backend, emb)
	defer srv.Close()
	provision(t, srv.URL, "vol")

	resp := postJSON(t, srv.URL+"/v1/memory/store", service.StoreMemoryRequest{
		VolumeHandle: "vol",
		ID:           "r1",
		Content:      "precomputed",
		Embedding:    []float32{1, 0, 0},
	})
	resp.Body.Close()

	resp = postJSON(t, srv.URL+"/v1/memory/search", service.SearchMemoryRequest{
		VolumeHandle: "vol",
		Vector:       []float32{1, 0, 0},
		TopK:         2,
		MinScore:     0.6,
		Metadata:     map[string]string{"tag": "shared"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if emb.calls != 0 {
		t.Fatalf("embedder must not be called when a vector is supplied, calls=%d", emb.calls)
	}
	if backend.request.BackendHandle != "vol" || backend.request.TopK != 2 || backend.request.MinScore != 0.6 {
		t.Fatalf("vector search options were not preserved: %+v", backend.request)
	}
	if len(backend.request.Vector) != 3 || backend.request.Vector[0] != 1 || backend.request.Metadata["tag"] != "shared" {
		t.Fatalf("vector search payload was not preserved: %+v", backend.request)
	}
	var results []service.SearchMemoryResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(results) != 1 || results[0].Score != 0.73 {
		t.Fatalf("expected unchanged backend score 0.73, got %+v", results)
	}
}

// getRecord fetches a record DTO via the GET endpoint.
func getRecord(t *testing.T, url, vol, id string) service.RecordDTO {
	t.Helper()
	resp, err := http.Get(url + "/v1/memory/get/" + vol + "/" + id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", resp.StatusCode)
	}
	var rec service.RecordDTO
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	return rec
}
