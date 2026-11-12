// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/observability"
)

func TestFixtureTrustDecisionRouteSupportsDemoFilters(t *testing.T) {
	server := httptest.NewServer(newFixtureHandler("demo-space", "demo-run"))
	t.Cleanup(server.Close)

	assertItems(t, server.URL, 0)
	request(t, http.MethodPost, server.URL+"/fixture/grant", http.StatusCreated)
	request(t, http.MethodPost, server.URL+"/fixture/operations/tool/weather-read", http.StatusOK)
	request(t, http.MethodPost, server.URL+"/fixture/operations/memory/research-notes", http.StatusOK)
	request(t, http.MethodPost, server.URL+"/fixture/operations/tool/admin-write", http.StatusForbidden)
	request(t, http.MethodDelete, server.URL+"/fixture/grant", http.StatusOK)
	request(t, http.MethodPost, server.URL+"/fixture/operations/memory/research-notes", http.StatusForbidden)

	response, err := http.Get(server.URL + "/apis/kognition.dev/v1alpha1/namespaces/demo-space/trust-decisions?runId=demo-run&plane=memory")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].ID != "demo-run:memory-revoked" || result.Items[1].ID != "demo-run:memory-allow" {
		t.Fatalf("memory decisions = %#v", result.Items)
	}
	if result.Items[0].Execution.TargetReached == nil || *result.Items[0].Execution.TargetReached {
		t.Fatalf("revocation execution = %#v", result.Items[0].Execution)
	}
	response, err = http.Get(server.URL + "/fixture/counters")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var counters map[string]int
	if err := json.NewDecoder(response.Body).Decode(&counters); err != nil {
		t.Fatal(err)
	}
	if counters["tool:weather-read"] != 1 || counters["memory:research-notes"] != 1 || counters["tool:admin-write"] != 0 {
		t.Fatalf("target counters = %#v", counters)
	}
}

func request(t *testing.T, method, url string, wantStatus int) {
	t.Helper()
	request, err := http.NewRequest(method, url, bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d", method, url, response.StatusCode, wantStatus)
	}
}

func assertItems(t *testing.T, serverURL string, want int) {
	t.Helper()
	response, err := http.Get(serverURL + "/apis/kognition.dev/v1alpha1/namespaces/demo-space/trust-decisions?runId=demo-run")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != want {
		t.Fatalf("items = %d, want %d", len(result.Items), want)
	}
}

func TestFixtureTrustDecisionRouteRejectsOtherRuns(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/demo-space/trust-decisions?runId=other-run", nil)
	response := httptest.NewRecorder()
	newFixtureHandler("demo-space", "demo-run").ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("items = %#v", result.Items)
	}
}
