// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap"
)

func TestStudioProfileDigestUsesOfflineCanonicalValidation(t *testing.T) {
	// TC-0325: the UI and CLI must bind exactly the same reviewed profile.
	profile := `{"apiVersion":"kognition.dev/v1alpha1","kind":"ModelInvocationProfile","metadata":{"name":"portable"},"spec":{"operation":"chat","defaults":{"maxCompletionTokens":128}}}`
	expected, err := readInvocationProfileDigest(strings.NewReader(profile), "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handleStudioProfileDigest(response, httptest.NewRequest(http.MethodPost, "/api/studio/profile-digest", strings.NewReader(profile)))
	var actual map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &actual); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || actual["digest"] != expected {
		t.Fatalf("digest mismatch: %d %#v", response.Code, actual)
	}
	response = httptest.NewRecorder()
	handleStudioProfileDigest(response, httptest.NewRequest(http.MethodPost, "/api/studio/profile-digest", strings.NewReader(`{"kind":"SecretRef"}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("non-profile accepted: %d", response.Code)
	}
}

type studioAuthoringStore struct {
	apiserver.APIServerStore
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *studioAuthoringStore) Get(_ context.Context, kind, ns, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[kind+"/"+ns+"/"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return bytes.Clone(data), nil
}

func (s *studioAuthoringStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result [][]byte
	for key, data := range s.objects {
		if strings.HasPrefix(key, kind+"/"+opts.Namespace+"/") {
			result = append(result, bytes.Clone(data))
		}
	}
	return result, nil
}

func (s *studioAuthoringStore) Create(_ context.Context, kind, ns, name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := kind + "/" + ns + "/" + name
	if _, ok := s.objects[key]; ok {
		return store.ErrAlreadyExists
	}
	s.objects[key] = bytes.Clone(data)
	return nil
}

func (s *studioAuthoringStore) Update(_ context.Context, kind, ns, name string, data []byte, rv int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := kind + "/" + ns + "/" + name
	current, ok := s.objects[key]
	if !ok {
		return store.ErrNotFound
	}
	var old, next map[string]any
	if err := json.Unmarshal(current, &old); err != nil {
		return err
	}
	if old["metadata"].(map[string]any)["resourceVersion"] != float64(rv) {
		return store.ErrConflict
	}
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	next["metadata"].(map[string]any)["resourceVersion"] = rv + 1
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	s.objects[key] = encoded
	return nil
}

func (s *studioAuthoringStore) UpdateAndGet(ctx context.Context, kind, ns, name string, data []byte, rv int64) ([]byte, error) {
	if err := s.Update(ctx, kind, ns, name, data, rv); err != nil {
		return nil, err
	}
	return s.Get(ctx, kind, ns, name)
}

func TestStudioAuthoringRoundTripsThroughStandaloneAPI(t *testing.T) {
	// TC-0322, TC-0323, TC-0324: real Studio adapter and API server, isolated store.
	api := apiserver.New(apiserver.Config{}, &studioAuthoringStore{objects: map[string][]byte{}}, zap.NewNop())
	upstream := httptest.NewServer(api)
	defer upstream.Close()
	restore := configureGetTest(upstream)
	defer restore()
	previousMode := k8sMode
	k8sMode = false
	t.Cleanup(func() { k8sMode = previousMode })
	assets, err := fs.Sub(studioAssets, "studio_assets")
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	registerResourceAPI(mux)
	for _, path := range []string{"/", "/resources.js", "/resource-catalog.json"} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("embedded asset %s: %d", path, response.Code)
		}
	}
	for _, tc := range []struct {
		kind, plural, namespace, field string
		spec                           map[string]any
		updated                        any
	}{
		{"AutonomyClass", "autonomyclasses", "default", "level", map[string]any{"level": 0, "requiresApproval": false}, float64(10)},
		{"MemoryClaim", "memoryclaims", "team-a", "capacity", map[string]any{"memoryClassName": "episodic", "type": "episodic", "capacity": "100Mi"}, "200Mi"},
		{"SecretRef", "secretrefs", "team-a", "backendRef", map[string]any{"backendType": "env", "backendRef": "MODEL_API_KEY", "key": "api-key"}, "OTHER_MODEL_API_KEY"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			obj := map[string]any{"apiVersion": "kognition.dev/v1alpha1", "kind": tc.kind, "metadata": map[string]any{"name": "studio-example"}, "spec": tc.spec}
			send := func(method, path string, body map[string]any) map[string]any {
				t.Helper()
				var raw []byte
				if body != nil {
					var err error
					raw, err = json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
				}
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, httptest.NewRequest(method, path+"?namespace="+tc.namespace, bytes.NewReader(raw)))
				if response.Code >= http.StatusBadRequest {
					t.Fatalf("%s %s: %d %s", method, path, response.Code, response.Body.String())
				}
				var result map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			path := "/api/resources/" + tc.plural
			created := send(http.MethodPost, path, obj)
			meta := created["metadata"].(map[string]any)
			uid := meta["uid"]
			if uid == "" || meta["resourceVersion"] != float64(1) {
				t.Fatalf("server identity/version missing: %#v", meta)
			}
			path += "/studio-example"
			loaded := send(http.MethodGet, path, nil)
			loaded["spec"].(map[string]any)[tc.field] = tc.updated
			send(http.MethodPut, path, loaded)
			reloaded := send(http.MethodGet, path, nil)
			if reloaded["spec"].(map[string]any)[tc.field] != tc.updated || reloaded["metadata"].(map[string]any)["uid"] != uid {
				t.Fatalf("update/reload lost data or identity: %#v", reloaded)
			}
		})
	}
}
