// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package studiotutorial

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

type rejectTransport struct{ calls atomic.Int32 }

func (t *rejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return nil, io.ErrUnexpectedEOF
}

func newPracticeTest(t *testing.T) (*Server, *httptest.Server, *http.Client, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "progress.json")
	service, err := New(os.DirFS(filepath.Join("..", "..", "cmd", "kognitionctl", "cmd", "studio_assets")), NewProgressStore(path))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(func() { server.Close(); client.CloseIdleConnections() })
	code, meta := practiceRequest(t, client, server.URL, http.MethodGet, "/api/meta", "", nil)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	return service, server, client, meta["tutorial"].(map[string]any)["token"].(string), path
}

func practiceRequest(t *testing.T, client *http.Client, base, method, path, token string, value any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	var err error
	if value != nil {
		raw, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	request, err := http.NewRequest(method, base+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Kognition-Practice", token)
	if method != http.MethodGet && (strings.HasPrefix(path, "/api/resources/") || strings.HasPrefix(path, "/api/invoke/")) {
		_, meta := practiceRequest(t, client, base, http.MethodGet, "/api/meta", "", nil)
		generation := meta["tutorial"].(map[string]any)["progress"].(map[string]any)["generation"]
		request.Header.Set("X-Kognition-Practice-Generation", fmt.Sprint(generation))
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func TestPracticeHasNoLiveFallbackAndEnforcesOrigin(t *testing.T) {
	// TC-0340: route isolation and owned-origin enforcement.
	_, server, client, token, _ := newPracticeTest(t)
	trap := &rejectTransport{}
	previous := http.DefaultTransport
	http.DefaultTransport = trap
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, path := range []string{
		"/api/invoke/workflow/real", "/api/invoke/tool/real", "/api/gallery",
		"/api/gallery/starter/import", "/api/tools/real/schema", "/api/events/agents/real",
		"/api/resources/runtimeapprovals", "/api/resources/controlplaneroles",
	} {
		code, _ := practiceRequest(t, client, server.URL, http.MethodPost, path, token, map[string]any{})
		if code < http.StatusBadRequest {
			t.Fatalf("unsupported operation succeeded: %s (%d)", path, code)
		}
	}
	if trap.calls.Load() != 0 {
		t.Fatalf("practice made %d outbound calls", trap.calls.Load())
	}
	for _, tc := range []struct {
		origin, host, token, site string
	}{
		{"https://attacker.invalid", "", token, ""},
		{"", "attacker.invalid:80", token, ""},
		{"", "", "", ""},
		{"", "", token, "same-site"},
	} {
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/tutorial", strings.NewReader(`{"action":"reset","generation":1}`))
		request.Header.Set("Origin", tc.origin)
		request.Header.Set("Sec-Fetch-Site", tc.site)
		request.Header.Set("X-Kognition-Practice", tc.token)
		if tc.host != "" {
			request.Host = tc.host
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign/ownerless request accepted: %+v (%d)", tc, response.StatusCode)
		}
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/", nil)
	request.Header.Set("Sec-Fetch-Site", "same-site")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatal("announced new-tab navigation or network isolation failed")
	}
}

func TestPracticeLessonUsesOutcomesAndPersistsNoLearnerContent(t *testing.T) {
	// TC-0341, TC-0342: semantic outcomes and metadata-only persistence.
	_, server, client, token, path := newPracticeTest(t)
	generation := float64(1)
	act := func(action string, fields map[string]any) map[string]any {
		t.Helper()
		if fields == nil {
			fields = map[string]any{}
		}
		fields["action"], fields["generation"] = action, generation
		code, result := practiceRequest(t, client, server.URL, http.MethodPost, "/api/tutorial", token, fields)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %#v", action, code, result)
		}
		generation = result["progress"].(map[string]any)["generation"].(float64)
		return result
	}
	completed := func(id string) bool {
		t.Helper()
		_, result := practiceRequest(t, client, server.URL, http.MethodGet, "/api/tutorial", token, nil)
		for _, value := range result["progress"].(map[string]any)["completed"].([]any) {
			if value == id {
				return true
			}
		}
		return false
	}
	code, _ := practiceRequest(t, client, server.URL, http.MethodPost, "/api/tutorial", token, map[string]any{"action": "next", "generation": 1})
	if code != http.StatusUnprocessableEntity {
		t.Fatal("a Next click completed an unmet task")
	}
	act("start", nil)
	draft := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": "private-learner-name", "namespace": "tutorial"},
		"spec":     map[string]any{"modelRef": map[string]any{"name": "tutorial-model"}, "instructions": map[string]any{"inline": "PRIVATE_LEARNER_INSTRUCTIONS"}},
	}
	act("check", map[string]any{"draft": draft, "view": "builder"})
	if completed("save") || completed("preview") || completed("yaml") {
		t.Fatal("draft editing falsely completed unrelated tasks")
	}
	act("check", map[string]any{"draft": draft, "view": "yaml"})
	act("check", map[string]any{"draft": draft, "view": "builder"})
	code, created := practiceRequest(t, client, server.URL, http.MethodPost, "/api/resources/agents?namespace=tutorial", token, draft)
	if code != http.StatusOK {
		t.Fatalf("save: %d %#v", code, created)
	}
	code, output := practiceRequest(t, client, server.URL, http.MethodPost, "/api/invoke/agent/private-learner-name", token, map[string]string{"message": "PRIVATE_CHAT_CONTENT"})
	if code != http.StatusOK || output["simulated"] != true || !strings.Contains(output["response"].(string), "Simulated") {
		t.Fatalf("preview is not clearly simulated: %d %#v", code, output)
	}
	created["spec"].(map[string]any)["instructions"] = map[string]any{"inline": "A DIFFERENT PRIVATE INSTRUCTION"}
	act("check", map[string]any{"draft": created, "view": "builder", "answer": "live"})
	if completed("challenge") {
		t.Fatal("incorrect safety answer completed the lesson")
	}
	final := act("check", map[string]any{"draft": created, "view": "builder", "answer": "practice"})
	if len(final["progress"].(map[string]any)["completed"].([]any)) != 7 {
		t.Fatalf("valid lesson did not complete: %#v", final)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, sensitive := range []string{"private-learner-name", "PRIVATE", token, "example.invalid", "uid", "draft", "namespace"} {
		if bytes.Contains(persisted, []byte(sensitive)) {
			t.Fatalf("learner content persisted: %s", sensitive)
		}
	}
	restarted, err := New(os.DirFS(filepath.Join("..", "..", "cmd", "kognitionctl", "cmd", "studio_assets")), NewProgressStore(path))
	if err != nil {
		t.Fatal(err)
	}
	restartedHTTP := httptest.NewServer(restarted)
	defer restartedHTTP.Close()
	_, resumed := practiceRequest(t, client, restartedHTTP.URL, http.MethodGet, "/api/tutorial", "", nil)
	encoded, err := json.Marshal(resumed)
	if err != nil {
		t.Fatal(err)
	}
	if resumed["draft"].(map[string]any)["metadata"].(map[string]any)["name"] != "practice-agent" || bytes.Contains(encoded, []byte("PRIVATE")) {
		t.Fatal("resume did not reconstruct a synthetic checkpoint")
	}
	oldGeneration := generation
	act("reset", nil)
	_, objects := practiceRequest(t, client, server.URL, http.MethodGet, "/api/resources/agents", token, nil)
	if len(objects["items"].([]any)) != 0 || completed("save") {
		t.Fatal("reset retained learner data")
	}
	code, _ = practiceRequest(t, client, server.URL, http.MethodPost, "/api/tutorial", token, map[string]any{"action": "check", "generation": oldGeneration})
	if code != http.StatusConflict {
		t.Fatal("stale action crossed a reset generation")
	}
	staleBody, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := http.NewRequest(http.MethodPost, server.URL+"/api/resources/agents", bytes.NewReader(staleBody))
	stale.Header.Set("X-Kognition-Practice", token)
	stale.Header.Set("X-Kognition-Practice-Generation", fmt.Sprint(oldGeneration))
	staleResponse, err := client.Do(stale)
	if err != nil {
		t.Fatal(err)
	}
	staleResponse.Body.Close()
	if staleResponse.StatusCode != http.StatusConflict {
		t.Fatal("stale resource write crossed a reset generation")
	}
}

func TestPracticeCompositionAndPureHelpersStayIsolated(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if !strings.HasSuffix(name, "_test.go") {
			file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range file.Imports {
				path, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				for _, forbidden := range []string{"internal/apiserver", "internal/controller", "internal/cpi", "/providers/", "os/exec", "client-go", "database/sql", "pgx"} {
					if strings.Contains(path, forbidden) {
						t.Fatalf("practice imported a privileged adapter: %s", path)
					}
				}
			}
		}
	}
	_, server, client, nonce, _ := newPracticeTest(t)
	code, _ := practiceRequest(t, client, server.URL, http.MethodPost, "/api/studio/profile-digest", nonce, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1", "kind": "ModelInvocationProfile",
		"spec": map[string]any{"operation": "chat", "unexpected": true},
	})
	if code != http.StatusBadRequest {
		t.Fatal("profile helper silently discarded unknown fields")
	}
}

func TestPracticeUnavailablePersistenceIsVisible(t *testing.T) {
	assets := os.DirFS(filepath.Join("..", "..", "cmd", "kognitionctl", "cmd", "studio_assets"))
	service, err := New(assets, NewProgressStore(""))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service)
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{}}
	defer client.CloseIdleConnections()
	_, meta := practiceRequest(t, client, server.URL, http.MethodGet, "/api/meta", "", nil)
	tutorial := meta["tutorial"].(map[string]any)
	if tutorial["warning"] == "" {
		t.Fatal("unavailable progress was silently defaulted")
	}
	code, result := practiceRequest(t, client, server.URL, http.MethodPost, "/api/tutorial", tutorial["token"].(string),
		map[string]any{"action": "start", "generation": 1})
	if code != http.StatusOK || result["warning"] == "" {
		t.Fatalf("memory-only mode was not explicit: %d %#v", code, result)
	}
}

func TestPracticeFailuresStayExplicit(t *testing.T) {
	_, server, client, token, _ := newPracticeTest(t)
	draft := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1", "kind": "Agent",
		"metadata": map[string]any{"name": "example"},
		"spec":     map[string]any{"modelRef": map[string]any{"name": "real-model"}},
	}
	code, _ := practiceRequest(t, client, server.URL, http.MethodPost, "/api/resources/agents", token, draft)
	if code != http.StatusUnprocessableEntity {
		t.Fatal("missing fixture reference was accepted")
	}
	oversized := `{"apiVersion":"kognition.dev/v1alpha1","kind":"Agent","metadata":{"name":"large"},"spec":{"modelRef":{"name":"tutorial-model"},"instructions":{"inline":"` + strings.Repeat("x", bodyLimit+1) + `"}}}`
	if !json.Valid([]byte(oversized)) {
		t.Fatal("body-limit test fixture must be valid JSON")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/api/resources/agents", strings.NewReader(oversized))
	request.Header.Set("X-Kognition-Practice", token)
	request.Header.Set("X-Kognition-Practice-Generation", "1")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode < http.StatusBadRequest {
		t.Fatal("oversized input succeeded")
	}
}
