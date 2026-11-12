// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/observability"
)

type dashboardTrustStore struct {
	entries []observability.AuditEntry
	filter  observability.AuditFilter
	filters []observability.AuditFilter
	err     error
	errors  map[string]error
}

func TestEffectiveDashboardAuditNamespace(t *testing.T) {
	previousAuditNamespace, previousKubeNamespace := dashboardAuditNamespace, kubeNamespace
	t.Cleanup(func() { dashboardAuditNamespace, kubeNamespace = previousAuditNamespace, previousKubeNamespace })

	dashboardAuditNamespace, kubeNamespace = " control-plane ", "api-system"
	if got := effectiveDashboardAuditNamespace(); got != "control-plane" {
		t.Fatalf("explicit audit namespace = %q, want control-plane", got)
	}
	dashboardAuditNamespace = ""
	if got := effectiveDashboardAuditNamespace(); got != "api-system" {
		t.Fatalf("fallback audit namespace = %q, want api-system", got)
	}
}

func (s *dashboardTrustStore) Query(_ context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	s.filter = filter
	s.filters = append(s.filters, filter)
	if filter.Workspace == "" {
		return nil, errors.New("unpartitioned audit scan exceeds the page limit")
	}
	if err := s.errors[filter.Workspace]; err != nil {
		return nil, err
	}
	entries := make([]observability.AuditEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		if entry.Workspace == filter.Workspace {
			entries = append(entries, entry)
		}
	}
	return entries, s.err
}

func TestDashboardTrustDecisionsKubernetesQueriesConfigMapStoreContract(t *testing.T) {
	previousMode, previousFactory := k8sMode, dashboardAuditStoreFactory
	k8sMode = true
	store := &dashboardTrustStore{entries: []observability.AuditEntry{dashboardTrustEntry(t, "decision-a")}}
	dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) { return store, nil }
	t.Cleanup(func() { k8sMode, dashboardAuditStoreFactory = previousMode, previousFactory })

	request := httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=team-a&plane=tool&outcome=allowed&limit=10", nil)
	response := httptest.NewRecorder()
	handleDashTrustDecisions(response, request)

	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.APIVersion != observability.TrustDecisionAPIVersion || len(result.Items) != 1 || result.Items[0].ID != "decision-a" {
		t.Fatalf("status=%d result=%#v", response.Code, result)
	}
	if store.filter.Workspace != "team-a" || store.filter.Plane != "tool" || store.filter.Outcome != "allowed" || store.filter.Limit != 11 || !store.filter.Descending {
		t.Fatalf("filter = %#v", store.filter)
	}
}

func TestDashboardTrustDecisionsKubernetesQueriesAllNamespaces(t *testing.T) {
	previousMode, previousFactory, previousListNamespaces := k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister
	k8sMode = true
	store := &dashboardTrustStore{entries: []observability.AuditEntry{
		dashboardTrustEntryForWorkspace(t, "decision-a", "team-a"),
		dashboardTrustEntryForWorkspace(t, "decision-b", "team-b"),
	}}
	dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) { return store, nil }
	dashboardTrustNamespaceLister = func() ([]string, error) { return []string{"team-a", "team-b"}, nil }
	t.Cleanup(func() {
		k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister = previousMode, previousFactory, previousListNamespaces
	})

	request := httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=", nil)
	response := httptest.NewRecorder()
	handleDashTrustDecisions(response, request)

	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	workspaces := map[string]bool{}
	for _, decision := range result.Items {
		workspaces[decision.Workspace] = true
	}
	if response.Code != http.StatusOK || len(result.Items) != 2 || !workspaces["team-a"] || !workspaces["team-b"] {
		t.Fatalf("status=%d result=%#v", response.Code, result)
	}
	if len(store.filters) != 2 || store.filters[0].Workspace != "team-a" || store.filters[1].Workspace != "team-b" {
		t.Fatalf("workspace filters = %#v, want indexed namespace queries", store.filters)
	}
}

func TestDashboardTrustDecisionsAllNamespacesRunIDIncludesRetainedEvidence(t *testing.T) {
	previousMode, previousFactory, previousListNamespaces := k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister
	k8sMode = true
	entry := dashboardTrustEntryForWorkspaceWithRunID(t, "retained-model", "team-a", "retained-run")
	entry.Timestamp = time.Now().Add(-25 * time.Hour)
	var decision observability.TrustDecisionV1
	if err := json.Unmarshal(entry.Details, &decision); err != nil {
		t.Fatal(err)
	}
	decision.Timestamp = entry.Timestamp
	decision.Plane = "model"
	entry.EventType = observability.AuditModelCall
	entry.Details, _ = json.Marshal(decision)
	store := &dashboardTrustStore{entries: []observability.AuditEntry{entry}}
	dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) { return store, nil }
	dashboardTrustNamespaceLister = func() ([]string, error) { return []string{"team-a"}, nil }
	t.Cleanup(func() {
		k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister = previousMode, previousFactory, previousListNamespaces
	})

	response := httptest.NewRecorder()
	handleDashTrustDecisions(response, httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=&runId=retained-run&limit=500", nil))

	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(result.Items) != 1 || result.Items[0].ID != "retained-model" {
		t.Fatalf("status=%d result=%#v", response.Code, result)
	}
	if len(store.filters) != 1 || !store.filters[0].Since.IsZero() {
		t.Fatalf("filters = %#v, want an unbounded retained-history lower limit", store.filters)
	}
}

func TestDashboardTrustDecisionsAllNamespacesMarksPageLimitedPartitionsTruncated(t *testing.T) {
	previousMode, previousFactory, previousListNamespaces := k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister
	k8sMode = true
	store := &dashboardTrustStore{
		entries: []observability.AuditEntry{dashboardTrustEntryForWorkspace(t, "decision-visible", "team-visible")},
		errors:  map[string]error{"team-noisy": fmt.Errorf("%w: test partition", k8saudit.ErrQueryPageLimit)},
	}
	dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) { return store, nil }
	dashboardTrustNamespaceLister = func() ([]string, error) { return []string{"team-noisy", "team-visible"}, nil }
	t.Cleanup(func() {
		k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister = previousMode, previousFactory, previousListNamespaces
	})

	response := httptest.NewRecorder()
	handleDashTrustDecisions(response, httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=", nil))

	var result observability.TrustDecisionQueryResult
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(result.Items) != 1 || result.Items[0].ID != "decision-visible" || !result.Truncated {
		t.Fatalf("status=%d result=%#v", response.Code, result)
	}
}

func TestDashboardTrustDecisionsAllNamespacesReportsUnexpectedFailures(t *testing.T) {
	tests := []struct {
		name           string
		listNamespaces func() ([]string, error)
		store          *dashboardTrustStore
		wantError      string
	}{
		{
			name:           "namespace list",
			listNamespaces: func() ([]string, error) { return nil, errors.New("cluster unavailable") },
			store:          &dashboardTrustStore{},
			wantError:      "list namespaces for trust decision query: cluster unavailable",
		},
		{
			name:           "partition query",
			listNamespaces: func() ([]string, error) { return []string{"team-a"}, nil },
			store:          &dashboardTrustStore{errors: map[string]error{"team-a": errors.New("audit read failed")}},
			wantError:      `query trust decisions in namespace "team-a": query trust decision audit store: audit read failed`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			previousMode, previousFactory, previousListNamespaces := k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister
			k8sMode = true
			dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) { return test.store, nil }
			dashboardTrustNamespaceLister = test.listNamespaces
			t.Cleanup(func() {
				k8sMode, dashboardAuditStoreFactory, dashboardTrustNamespaceLister = previousMode, previousFactory, previousListNamespaces
			})

			response := httptest.NewRecorder()
			handleDashTrustDecisions(response, httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=", nil))

			var result map[string]string
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusInternalServerError || result["error"] != test.wantError {
				t.Fatalf("status=%d error=%q, want %q", response.Code, result["error"], test.wantError)
			}
		})
	}
}

func TestDashboardTrustDecisionsStandaloneForwardsCanonicalRoute(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/trust-decisions" || r.URL.Query().Get("namespace") != "" || r.URL.Query().Get("sessionId") != "session-a" {
			t.Errorf("backend request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"kognition.dev/v1alpha1","items":[],"truncated":false}`))
	}))
	defer backend.Close()
	previousMode, previousURL, previousClient := k8sMode, serverURL, httpClient
	k8sMode, serverURL, httpClient = false, backend.URL, backend.Client()
	t.Cleanup(func() { k8sMode, serverURL, httpClient = previousMode, previousURL, previousClient })

	request := httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=team-a&sessionId=session-a", nil)
	response := httptest.NewRecorder()
	handleDashTrustDecisions(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"apiVersion":"kognition.dev/v1alpha1"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDashboardTrustDecisionsStandaloneRejectsAllNamespaces(t *testing.T) {
	previousMode := k8sMode
	k8sMode = false
	t.Cleanup(func() { k8sMode = previousMode })

	response := httptest.NewRecorder()
	handleDashTrustDecisions(response, httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=", nil))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "require Kubernetes mode") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDashboardTrustDecisionsStandaloneAndKubernetesResponsesMatch(t *testing.T) {
	entries := []observability.AuditEntry{
		dashboardTrustEntryWithRunID(t, "decision-parity", "run-focus"),
		dashboardTrustEntryWithRunID(t, "decision-other-run", "run-other"),
	}
	expected, err := observability.QueryTrustDecisions(context.Background(), &dashboardTrustStore{entries: entries}, observability.TrustDecisionQuery{Namespace: "team-a", RunID: "run-focus"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("runId") != "run-focus" {
			t.Errorf("backend query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer backend.Close()

	previousMode, previousURL, previousClient, previousFactory := k8sMode, serverURL, httpClient, dashboardAuditStoreFactory
	t.Cleanup(func() {
		k8sMode, serverURL, httpClient, dashboardAuditStoreFactory = previousMode, previousURL, previousClient, previousFactory
	})
	query := "/api/trust-decisions?namespace=team-a&runId=run-focus"
	k8sMode, serverURL, httpClient = false, backend.URL, backend.Client()
	standaloneResponse := httptest.NewRecorder()
	handleDashTrustDecisions(standaloneResponse, httptest.NewRequest(http.MethodGet, query, nil))

	k8sMode = true
	dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) {
		return &dashboardTrustStore{entries: entries}, nil
	}
	kubernetesResponse := httptest.NewRecorder()
	handleDashTrustDecisions(kubernetesResponse, httptest.NewRequest(http.MethodGet, query, nil))

	var standaloneResult, kubernetesResult observability.TrustDecisionQueryResult
	if err := json.NewDecoder(standaloneResponse.Body).Decode(&standaloneResult); err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(kubernetesResponse.Body).Decode(&kubernetesResult); err != nil {
		t.Fatal(err)
	}
	if standaloneResponse.Code != http.StatusOK || kubernetesResponse.Code != http.StatusOK || len(standaloneResult.Items) != 1 || standaloneResult.Items[0].ID != "decision-parity" || !reflect.DeepEqual(standaloneResult, kubernetesResult) {
		t.Fatalf("standalone status=%d result=%#v; kubernetes status=%d result=%#v", standaloneResponse.Code, standaloneResult, kubernetesResponse.Code, kubernetesResult)
	}
}

func TestDashboardTrustDecisionsValidatesAndReportsStoreErrors(t *testing.T) {
	previousMode, previousFactory := k8sMode, dashboardAuditStoreFactory
	k8sMode = true
	dashboardAuditStoreFactory = func() (observability.AuditQueryStore, error) {
		return &dashboardTrustStore{err: errors.New("ConfigMap page limit exceeded; narrow the filter")}, nil
	}
	t.Cleanup(func() { k8sMode, dashboardAuditStoreFactory = previousMode, previousFactory })

	overflow := httptest.NewRecorder()
	handleDashTrustDecisions(overflow, httptest.NewRequest(http.MethodGet, "/api/trust-decisions?namespace=team-a", nil))
	if overflow.Code != http.StatusInternalServerError || !strings.Contains(overflow.Body.String(), "narrow the filter") {
		t.Fatalf("overflow status=%d body=%s", overflow.Code, overflow.Body.String())
	}
}

func dashboardTrustEntry(t *testing.T, id string) observability.AuditEntry {
	return dashboardTrustEntryForWorkspace(t, id, "team-a")
}

func dashboardTrustEntryWithRunID(t *testing.T, id, runID string) observability.AuditEntry {
	return dashboardTrustEntryForWorkspaceWithRunID(t, id, "team-a", runID)
}

func dashboardTrustEntryForWorkspace(t *testing.T, id, workspace string) observability.AuditEntry {
	return dashboardTrustEntryForWorkspaceWithRunID(t, id, workspace, "")
}

func dashboardTrustEntryForWorkspaceWithRunID(t *testing.T, id, workspace, runID string) observability.AuditEntry {
	t.Helper()
	timestamp := time.Now().UTC().Add(-time.Minute)
	decision := observability.TrustDecisionV1{SchemaVersion: 1, ID: id, Timestamp: timestamp, Plane: "tool", Outcome: "allowed", ReasonCode: "authorized", Workspace: workspace, RunID: runID, Request: observability.TrustDecisionRequest{Operation: "invoke", TargetRef: observability.TrustDecisionResourceRef{Kind: "Tool", Name: "weather"}}, Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}, Execution: observability.TrustDecisionExecution{Status: "not_started"}}
	details, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return observability.AuditEntry{ID: id, Timestamp: timestamp, Workspace: workspace, EventType: observability.AuditToolCall, Details: details}
}
