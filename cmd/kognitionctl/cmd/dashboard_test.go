// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// ---------------------------------------------------------------------------
// TC-0275: Dashboard events endpoint — verify /api/meta returns hasEvents: true
// and /api/events/{kind}/{name} responds with an items array.
// ---------------------------------------------------------------------------

func TestDashboardMeta_HasEventsTrue(t *testing.T) {
	// TC-0275: /api/meta must expose hasEvents: true so the UI enables the events tab.
	req := httptest.NewRequest(http.MethodGet, "/api/meta", nil)
	w := httptest.NewRecorder()
	handleDashMeta(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var meta struct {
		Mode      string `json:"mode"`
		HasEvents bool   `json:"hasEvents"`
	}
	if err := json.NewDecoder(w.Body).Decode(&meta); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !meta.HasEvents {
		t.Error("hasEvents = false, want true")
	}
}

func TestDashboardMeta_ExposesKubernetesAuditNamespace(t *testing.T) {
	previousMode, previousAuditNamespace := k8sMode, dashboardAuditNamespace
	k8sMode, dashboardAuditNamespace = true, "control-plane"
	t.Cleanup(func() { k8sMode, dashboardAuditNamespace = previousMode, previousAuditNamespace })
	w := httptest.NewRecorder()

	handleDashMeta(w, httptest.NewRequest(http.MethodGet, "/api/meta", nil))

	var meta dashboardMeta
	if err := json.NewDecoder(w.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.AuditNamespace != "control-plane" {
		t.Fatalf("audit namespace = %q, want control-plane", meta.AuditNamespace)
	}
}

func TestDashboardMeta_ExposesOnlyPublicObservabilityURLs(t *testing.T) {
	t.Setenv("KOGNITION_DASHBOARD_JAEGER_URL", " https://observe.example.test/jaeger/ ")
	t.Setenv("KOGNITION_DASHBOARD_GRAFANA_URL", "https://user:secret@grafana.example.test/d/tool?token=secret")
	w := httptest.NewRecorder()

	handleDashMeta(w, httptest.NewRequest(http.MethodGet, "/api/meta", nil))

	var meta dashboardMeta
	if err := json.NewDecoder(w.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.Observability.JaegerURL != "https://observe.example.test/jaeger" {
		t.Fatalf("jaeger URL = %q", meta.Observability.JaegerURL)
	}
	if meta.Observability.GrafanaURL != "" {
		t.Fatalf("credential-bearing Grafana URL was exposed: %q", meta.Observability.GrafanaURL)
	}
}

func TestDashboardPublicURLRejectsUnsafeOrNonHTTPURLs(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)",
		"//observe.example.test",
		"https://user:secret@observe.example.test",
		"https://observe.example.test/?token=secret",
		"https://observe.example.test/#secret",
	} {
		if got := dashboardPublicURL(raw); got != "" {
			t.Errorf("dashboardPublicURL(%q) = %q, want empty", raw, got)
		}
	}
}

func TestDashboardRunProofReturnsAllowlistedProjection(t *testing.T) {
	path := t.TempDir() + "/report.json"
	report := `{"demoRunId":"demo-a","passed":true,"secret":"not-exposed","assertions":[{"passed":true,"evidence":"private"},{"passed":false}],"runProof":{"schemaVersion":1,"scenario":"Delegated research","protectedAction":"Publish result","mode":"standalone-fallback","reportRef":"run-report.json"}}`
	if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := dashboardRunProofPath
	dashboardRunProofPath = path
	t.Cleanup(func() { dashboardRunProofPath = previous })

	w := httptest.NewRecorder()
	handleDashRunProof(w, httptest.NewRequest(http.MethodGet, "/api/run-proof?runId=demo-a", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var proof dashboardRunProof
	if err := json.NewDecoder(w.Body).Decode(&proof); err != nil {
		t.Fatal(err)
	}
	if proof.RunID != "demo-a" || proof.Assertions != (dashboardRunProofAssertions{Total: 2, Passed: 1, Failed: 1}) {
		t.Fatalf("unexpected run proof: %+v", proof)
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("response exposed report internals: %s", w.Body.String())
	}
}

func TestDashboardRunProofFailsClosed(t *testing.T) {
	previous := dashboardRunProofPath
	t.Cleanup(func() { dashboardRunProofPath = previous })
	for _, test := range []struct {
		name  string
		path  string
		runID string
	}{
		{name: "not configured", runID: "demo-a"},
		{name: "missing run ID", path: "missing.json"},
		{name: "missing report", path: "missing.json", runID: "demo-a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dashboardRunProofPath = test.path
			w := httptest.NewRecorder()
			handleDashRunProof(w, httptest.NewRequest(http.MethodGet, "/api/run-proof?runId="+test.runID, nil))
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", w.Code)
			}
		})
	}
}

func TestDashboardRunProofRejectsInvalidReports(t *testing.T) {
	tests := []struct {
		name   string
		report string
		runID  string
	}{
		{name: "malformed", report: `{`, runID: "demo-a"},
		{name: "mismatched run", report: `{"demoRunId":"demo-b","passed":true,"assertions":[],"runProof":{"schemaVersion":1,"scenario":"Research","protectedAction":"Publish","mode":"fallback"}}`, runID: "demo-a"},
		{name: "missing scenario", report: `{"demoRunId":"demo-a","passed":true,"assertions":[],"runProof":{"schemaVersion":1,"protectedAction":"Publish","mode":"fallback"}}`, runID: "demo-a"},
		{name: "oversized scenario", report: `{"demoRunId":"demo-a","passed":true,"assertions":[],"runProof":{"schemaVersion":1,"scenario":"` + strings.Repeat("a", 257) + `","protectedAction":"Publish","mode":"fallback"}}`, runID: "demo-a"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir() + "/report.json"
			if err := os.WriteFile(path, []byte(test.report), 0o600); err != nil {
				t.Fatal(err)
			}
			previous := dashboardRunProofPath
			dashboardRunProofPath = path
			t.Cleanup(func() { dashboardRunProofPath = previous })

			w := httptest.NewRecorder()
			handleDashRunProof(w, httptest.NewRequest(http.MethodGet, "/api/run-proof?runId="+test.runID, nil))
			if w.Code != http.StatusUnprocessableEntity || w.Body.String() != "{\"error\":\"run proof is not available\"}\n" {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestDashboardRunProofRejectsOversizedReport(t *testing.T) {
	path := t.TempDir() + "/report.json"
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxRunProofReportBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := dashboardRunProofPath
	dashboardRunProofPath = path
	t.Cleanup(func() { dashboardRunProofPath = previous })

	w := httptest.NewRecorder()
	handleDashRunProof(w, httptest.NewRequest(http.MethodGet, "/api/run-proof?runId=demo-a", nil))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestDashboardRunProofOmitsUnsafeReportReference(t *testing.T) {
	path := t.TempDir() + "/report.json"
	report := `{"demoRunId":"demo-a","passed":true,"assertions":[],"runProof":{"schemaVersion":1,"scenario":"Research","protectedAction":"Publish","mode":"fallback","reportRef":"C:\\secret\\report.json?token=private"}}`
	if err := os.WriteFile(path, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := dashboardRunProofPath
	dashboardRunProofPath = path
	t.Cleanup(func() { dashboardRunProofPath = previous })

	w := httptest.NewRecorder()
	handleDashRunProof(w, httptest.NewRequest(http.MethodGet, "/api/run-proof?runId=demo-a", nil))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestDashboardCatalog_IncludesEvaluationResults(t *testing.T) {
	groups := dashboardCatalog()
	for _, group := range groups {
		if group.Title != "Quality" {
			continue
		}
		got := make([]string, 0, len(group.Items))
		for _, item := range group.Items {
			got = append(got, item.Plural)
		}
		if strings.Join(got, ",") != "evaluationdatasets,evaluations,evaluationruns" {
			t.Fatalf("quality resources = %v, want evaluationdatasets, evaluations and evaluationruns", got)
		}
		return
	}
	t.Fatal("dashboard catalog missing Quality group")
}

// TC-0309: Trust governance resources and the virtual decision view are available in the dashboard catalog.
func TestDashboardCatalog_IncludesTrustGovernanceAndDecisionView(t *testing.T) {
	groups := dashboardCatalog()
	for _, group := range groups {
		if group.Title != "Governance" {
			continue
		}
		got := make(map[string]dashboardResource, len(group.Items))
		for _, item := range group.Items {
			got[item.Label] = item
		}
		if got["Trust Profiles"].Plural != "trustprofiles" {
			t.Fatalf("Trust Profiles = %#v, want trustprofiles resource", got["Trust Profiles"])
		}
		if got["Approval Decisions"].Plural != "approvaldecisions" {
			t.Fatalf("Approval Decisions = %#v, want approvaldecisions resource", got["Approval Decisions"])
		}
		if got["Trust Decisions"].View != "trust-decisions" || got["Trust Decisions"].Plural != "" {
			t.Fatalf("Trust Decisions = %#v, want read-only trust-decisions view", got["Trust Decisions"])
		}
		return
	}
	t.Fatal("dashboard catalog missing Governance group")
}

func TestDashboardAssets_TrustDecisionsUsesDedicatedQueryView(t *testing.T) {
	app, err := dashboardAssets.ReadFile("dashboard_assets/app.js")
	if err != nil {
		t.Fatalf("read dashboard app: %v", err)
	}
	script := string(app)
	for _, want := range []string{
		`state.current.view === "trust-decisions"`,
		`buildTrustDecisionQuery(ns, readTrustDecisionFilters())`,
		`renderTrustDecisions(items)`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("dashboard app missing Trust Decisions contract %q", want)
		}
	}
	index, err := dashboardAssets.ReadFile("dashboard_assets/index.html")
	if err != nil {
		t.Fatalf("read dashboard index: %v", err)
	}
	for _, want := range []string{`id="trust-filters"`, `id="trust-agent"`, `id="trust-session"`, `id="trust-plane"`, `id="trust-outcome"`, `id="trust-since"`, `id="trust-until"`, `id="trust-limit"`} {
		if !strings.Contains(string(index), want) {
			t.Fatalf("dashboard index missing Trust Decisions filter %q", want)
		}
	}
}

func TestDashboardAssetsIncludeBrandMark(t *testing.T) {
	index, err := dashboardAssets.ReadFile("dashboard_assets/index.html")
	if err != nil {
		t.Fatalf("read dashboard index: %v", err)
	}
	if !strings.Contains(string(index), `src="/kognition-mark.svg"`) || !strings.Contains(string(index), `rel="icon"`) {
		t.Fatal("dashboard index missing Kognition header mark or favicon")
	}
	if _, err := dashboardAssets.ReadFile("dashboard_assets/kognition-mark.svg"); err != nil {
		t.Fatalf("read dashboard brand mark: %v", err)
	}
}

// TC-0310: The timeline renders decision evidence without inferring absent fields.
// TC-0311: Dashboard behavior tests cover safe rendering, keyboard navigation, and responsive layout assertions.
func TestDashboardAssets_TrustDecisionTimelineBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to run dashboard behavior tests")
	}
	command := exec.Command(node, "--test", "dashboard_assets/app.test.js")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dashboard behavior tests failed: %v\n%s", err, output)
	}
}

func TestDashboardEvents_UnknownKindReturnsEmptyItems(t *testing.T) {
	// TC-0275: requesting events for an unrecognized kind should return an empty items array
	// rather than an error, ensuring the UI gracefully shows "no events".
	origK8sMode := k8sMode
	k8sMode = true
	defer func() { k8sMode = origK8sMode }()

	req := httptest.NewRequest(http.MethodGet, "/api/events/nonexistent/foo", nil)
	req.SetPathValue("kind", "nonexistent")
	req.SetPathValue("name", "foo")
	w := httptest.NewRecorder()
	handleDashEvents(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Items []any `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Items == nil {
		t.Error("items should be an empty array, got nil")
	}
	if len(resp.Items) != 0 {
		t.Errorf("items should be empty, got %d items", len(resp.Items))
	}
}

func TestDashboardEvents_StandaloneForwardsNamespacedResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/events" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[{"reason":"Ready"}]}`))
	}))
	defer server.Close()
	restore := configureGetTest(server)
	defer restore()
	k8sMode = false

	req := dashboardRequest(http.MethodGet, "/api/events/agents/planner?namespace=team-a", "", map[string]string{"kind": "agents", "name": "planner"})
	recorder := httptest.NewRecorder()

	handleDashEvents(recorder, req)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"reason":"Ready"`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestDashboardEvents_KubernetesNormalizesNewestFirst(t *testing.T) {
	previousMode, previousCoreClient := k8sMode, k8sCoreClient
	k8sMode = true
	t.Cleanup(func() {
		k8sMode, k8sCoreClient = previousMode, previousCoreClient
	})
	older := metav1.NewTime(time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC))
	newer := metav1.NewTime(older.Add(time.Minute))
	k8sCoreClient = kubernetesfake.NewSimpleClientset(
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "older", Namespace: "team-a"}, InvolvedObject: corev1.ObjectReference{Kind: "Agent", Name: "planner"}, Reason: "Scheduled", Type: corev1.EventTypeNormal, Message: "scheduled", LastTimestamp: older, Count: 1},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "newer", Namespace: "team-a", CreationTimestamp: newer}, InvolvedObject: corev1.ObjectReference{Kind: "Agent", Name: "planner"}, Reason: "Failed", Type: corev1.EventTypeWarning, Message: "model unavailable", Count: 3},
	)
	req := dashboardRequest(http.MethodGet, "/api/events/agents/planner?namespace=team-a", "", map[string]string{"kind": "agents", "name": "planner"})
	recorder := httptest.NewRecorder()

	handleDashEvents(recorder, req)

	var response struct {
		Items []struct {
			Reason    string `json:"reason"`
			Type      string `json:"type"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
			Count     int32  `json:"count"`
		} `json:"items"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(response.Items) != 2 {
		t.Fatalf("status = %d, response = %#v", recorder.Code, response)
	}
	if got := response.Items[0]; got.Reason != "Failed" || got.Type != corev1.EventTypeWarning || got.Message != "model unavailable" || got.Timestamp != newer.Format(time.RFC3339) || got.Count != 3 {
		t.Fatalf("newest event = %#v", got)
	}
}

func TestDashboardEvents_KubernetesReportsListFailure(t *testing.T) {
	previousMode, previousCoreClient := k8sMode, k8sCoreClient
	k8sMode, k8sCoreClient = true, nil
	t.Cleanup(func() {
		k8sMode, k8sCoreClient = previousMode, previousCoreClient
	})
	req := dashboardRequest(http.MethodGet, "/api/events/agents/planner", "", map[string]string{"kind": "agents", "name": "planner"})
	recorder := httptest.NewRecorder()

	handleDashEvents(recorder, req)

	if recorder.Code == http.StatusOK || !strings.Contains(recorder.Body.String(), "kube client not initialized") {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestDashboardCreate_PreservesURLNamespace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var obj map[string]any
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			t.Fatal(err)
		}
		meta, _ := obj["metadata"].(map[string]any)
		if meta["name"] != "planner" || meta["namespace"] != "team-a" {
			t.Errorf("metadata = %#v", meta)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner","namespace":"team-a"}}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := dashboardRequest(http.MethodPost, "/api/resources/agents", `kind: Agent
metadata:
  name: planner
`, map[string]string{"kind": "agents"})
	recorder := httptest.NewRecorder()

	handleDashCreate(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
}

func TestDashboardCreate_ApprovalDecisionUsesAuthenticatedKubernetesUser(t *testing.T) {
	previousMode, previousDynamicClient, previousCoreClient := k8sMode, k8sDynClient, k8sCoreClient
	k8sMode = true
	dynamicClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	k8sDynClient = dynamicClient
	coreClient := kubernetesfake.NewSimpleClientset()
	coreClient.Fake.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.SelfSubjectReview{
			Status: authenticationv1.SelfSubjectReviewStatus{
				UserInfo: authenticationv1.UserInfo{Username: "kubernetes-admin"},
			},
		}, nil
	})
	k8sCoreClient = coreClient
	t.Cleanup(func() {
		k8sMode, k8sDynClient, k8sCoreClient = previousMode, previousDynamicClient, previousCoreClient
	})

	req := dashboardRequest(http.MethodPost, "/api/resources/approvaldecisions?namespace=default", `
apiVersion: kognition.dev/v1alpha1
kind: ApprovalDecision
metadata:
  name: approve-publish
spec:
  workflowRunRef:
    name: blog-run
  step: publish
  decision: approve
  approver: studio-user
`, map[string]string{"kind": "approvaldecisions"})
	recorder := httptest.NewRecorder()

	handleDashCreate(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	approvalGVR := schema.GroupVersionResource{Group: "kognition.dev", Version: "v1alpha1", Resource: "approvaldecisions"}
	created, err := dynamicClient.Resource(approvalGVR).Namespace("default").Get(context.Background(), "approve-publish", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if approver, _, _ := unstructured.NestedString(created.Object, "spec", "approver"); approver != "kubernetes-admin" {
		t.Fatalf("spec.approver = %q, want authenticated user", approver)
	}
}

func TestDashboardUpdate_InjectsCurrentResourceVersion(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"metadata":{"name":"planner","resourceVersion":"19"}}`))
		case http.MethodPut:
			var obj map[string]any
			if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
				t.Fatal(err)
			}
			meta, _ := obj["metadata"].(map[string]any)
			if meta["resourceVersion"] != "19" {
				t.Errorf("resourceVersion = %#v, want 19", meta["resourceVersion"])
			}
			if meta["name"] != "planner" || meta["namespace"] != "team-a" {
				t.Errorf("metadata = %#v", meta)
			}
			_, _ = w.Write([]byte(`{"metadata":{"name":"planner","resourceVersion":"20"}}`))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := dashboardRequest(http.MethodPut, "/api/resources/agents/planner", `kind: Agent
metadata: {}
spec:
  modelRef: primary
`, map[string]string{"kind": "agents", "name": "planner"})
	recorder := httptest.NewRecorder()

	handleDashUpdate(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Join(methods, ",") != "GET,PUT" {
		t.Fatalf("methods = %v, want GET then PUT", methods)
	}
}

func TestDashboardDelete_ReturnsDeletedResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := dashboardRequest(http.MethodDelete, "/api/resources/agents/planner", "", map[string]string{
		"kind": "agents", "name": "planner",
	})
	recorder := httptest.NewRecorder()

	handleDashDelete(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var result map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["deleted"] != true || result["namespace"] != "team-a" {
		t.Errorf("result = %#v", result)
	}
}

func TestDashboardParseYAML_NormalizesNestedValues(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/parse-yaml", strings.NewReader(`spec:
  labels:
    tier: worker
  values:
    - one
    - two
`))
	recorder := httptest.NewRecorder()

	handleDashParseYAML(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var obj map[string]any
	if err := json.NewDecoder(recorder.Body).Decode(&obj); err != nil {
		t.Fatal(err)
	}
	spec, _ := obj["spec"].(map[string]any)
	labels, _ := spec["labels"].(map[string]any)
	values, _ := spec["values"].([]any)
	if labels["tier"] != "worker" || len(values) != 2 || values[1] != "two" {
		t.Errorf("normalized object = %#v", obj)
	}
}

func TestNormalizeYAMLDecoded_ConvertsNonStringMapKeys(t *testing.T) {
	input := map[any]any{
		1: map[any]any{"enabled": true},
		2: []any{map[any]any{"name": "planner"}},
	}

	got, ok := normalizeYAMLDecoded(input).(map[string]any)
	if !ok {
		t.Fatalf("normalized value = %#v", input)
	}
	nested, _ := got["1"].(map[string]any)
	items, _ := got["2"].([]any)
	item, _ := items[0].(map[string]any)
	if nested["enabled"] != true || item["name"] != "planner" {
		t.Fatalf("normalized value = %#v", got)
	}
}

func TestDashboardListAndGet_ForwardBackendResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents":
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"planner"}}]}`))
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner":
			_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner"}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	tests := []struct {
		name         string
		handler      func(http.ResponseWriter, *http.Request)
		target       string
		pathValues   map[string]string
		bodyContains string
	}{
		{
			name:         "list",
			handler:      handleDashList,
			target:       "/api/resources/agents",
			pathValues:   map[string]string{"kind": "agents"},
			bodyContains: `"items"`,
		},
		{
			name:         "get",
			handler:      handleDashGet,
			target:       "/api/resources/agents/planner",
			pathValues:   map[string]string{"kind": "agents", "name": "planner"},
			bodyContains: `"kind":"Agent"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := dashboardRequest(http.MethodGet, test.target, "", test.pathValues)
			recorder := httptest.NewRecorder()
			test.handler(recorder, req)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), test.bodyContains) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDashboardCreate_UsesManifestNamespaceWhenQueryIsExplicitlyEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/project-b/agents" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"metadata":{"name":"planner","namespace":"project-b"}}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	req := dashboardRequest(http.MethodPost, "/api/resources/agents?namespace=", `kind: Agent
metadata:
  name: planner
  namespace: project-b
`, map[string]string{"kind": "agents"})
	recorder := httptest.NewRecorder()

	handleDashCreate(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestDashboardManifestValidationRejectsMismatchesWithoutBackendRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid manifest reached backend")
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	tests := []struct {
		name       string
		handler    func(http.ResponseWriter, *http.Request)
		method     string
		target     string
		body       string
		pathValues map[string]string
		want       string
	}{
		{
			name:       "create requires name",
			handler:    handleDashCreate,
			method:     http.MethodPost,
			target:     "/api/resources/agents",
			body:       "kind: Agent\nmetadata: {}\n",
			pathValues: map[string]string{"kind": "agents"},
			want:       "metadata.name is required",
		},
		{
			name:       "create rejects kind",
			handler:    handleDashCreate,
			method:     http.MethodPost,
			target:     "/api/resources/agents",
			body:       "kind: Tool\nmetadata:\n  name: planner\n",
			pathValues: map[string]string{"kind": "agents"},
			want:       "does not match URL",
		},
		{
			name:       "update rejects name",
			handler:    handleDashUpdate,
			method:     http.MethodPut,
			target:     "/api/resources/agents/planner",
			body:       "kind: Agent\nmetadata:\n  name: other\n",
			pathValues: map[string]string{"kind": "agents", "name": "planner"},
			want:       "does not match URL name",
		},
		{
			name:       "update rejects namespace",
			handler:    handleDashUpdate,
			method:     http.MethodPut,
			target:     "/api/resources/agents/planner",
			body:       "kind: Agent\nmetadata:\n  name: planner\n  namespace: project-b\n",
			pathValues: map[string]string{"kind": "agents", "name": "planner"},
			want:       "does not match URL namespace",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := dashboardRequest(test.method, test.target, test.body, test.pathValues)
			recorder := httptest.NewRecorder()
			test.handler(recorder, req)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDashboardBackendErrorsMapToHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/missing":
			http.Error(w, "not found", http.StatusNotFound)
		case "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/forbidden":
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()
	tests := []struct {
		name       string
		resource   string
		wantStatus int
	}{
		{name: "not found", resource: "missing", wantStatus: http.StatusNotFound},
		{name: "forbidden", resource: "forbidden", wantStatus: http.StatusForbidden},
		{name: "upstream failure", resource: "broken", wantStatus: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := dashboardRequest(http.MethodGet, "/api/resources/agents/"+test.resource, "", map[string]string{
				"kind": "agents", "name": test.resource,
			})
			recorder := httptest.NewRecorder()
			handleDashGet(recorder, req)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestDashboardResourceAPIRoutes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents" &&
			r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner" {
			t.Errorf("backend path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, "/planner") {
				_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner","resourceVersion":"1"}}`))
			} else {
				_, _ = w.Write([]byte(`{"items":[]}`))
			}
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner"}}`))
		case http.MethodPut:
			_, _ = w.Write([]byte(`{"kind":"Agent","metadata":{"name":"planner","resourceVersion":"2"}}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("backend method = %s", r.Method)
		}
	}))
	defer backend.Close()
	restore := configureGetTest(backend)
	defer restore()

	mux := http.NewServeMux()
	registerResourceAPI(mux)
	for _, tc := range []struct {
		name       string
		method     string
		target     string
		body       string
		wantStatus int
		wantBody   string
	}{
		{name: "meta", method: http.MethodGet, target: "/api/meta", wantStatus: http.StatusOK, wantBody: `"hasEvents":true`},
		{name: "namespaces", method: http.MethodGet, target: "/api/namespaces", wantStatus: http.StatusOK, wantBody: `[]`},
		{name: "parse yaml", method: http.MethodPost, target: "/api/parse-yaml", body: "spec:\n  enabled: true\n", wantStatus: http.StatusOK, wantBody: `"enabled":true`},
		{name: "list", method: http.MethodGet, target: "/api/resources/agents", wantStatus: http.StatusOK, wantBody: `"items":[]`},
		{name: "create", method: http.MethodPost, target: "/api/resources/agents", body: "kind: Agent\nmetadata:\n  name: planner\n", wantStatus: http.StatusCreated, wantBody: `"name":"planner"`},
		{name: "get", method: http.MethodGet, target: "/api/resources/agents/planner", wantStatus: http.StatusOK, wantBody: `"resourceVersion":"1"`},
		{name: "update", method: http.MethodPut, target: "/api/resources/agents/planner", body: "kind: Agent\nmetadata:\n  name: planner\n  resourceVersion: \"1\"\n", wantStatus: http.StatusOK, wantBody: `"resourceVersion":"2"`},
		{name: "delete", method: http.MethodDelete, target: "/api/resources/agents/planner", wantStatus: http.StatusOK, wantBody: `"deleted":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != tc.wantStatus || !strings.Contains(response.Body.String(), tc.wantBody) {
				t.Fatalf("status = %d, body = %s; want status %d containing %q", response.Code, response.Body.String(), tc.wantStatus, tc.wantBody)
			}
		})
	}
}

func TestDashboardMeta_FiltersUnavailableKubernetesResources(t *testing.T) {
	resetDiscoveryCache(t)
	previousMode := k8sMode
	k8sMode = true
	t.Cleanup(func() { k8sMode = previousMode })
	primeDiscoveryCache(map[string]bool{"agents": true, "workflows": true}, nil)

	response := httptest.NewRecorder()
	handleDashMeta(response, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var meta dashboardMeta
	if err := json.NewDecoder(response.Body).Decode(&meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if meta.Mode != "kubernetes" {
		t.Fatalf("mode = %q, want kubernetes", meta.Mode)
	}
	var resources []string
	for _, group := range meta.Groups {
		for _, resource := range group.Items {
			resources = append(resources, resource.Plural)
		}
	}
	if strings.Join(resources, ",") != "agents,workflows," {
		t.Fatalf("resources = %v, want installed agents and workflows plus the virtual Trust Decisions view", resources)
	}
	governance := meta.Groups[len(meta.Groups)-1]
	if governance.Title != "Governance" || len(governance.Items) != 1 || governance.Items[0].View != "trust-decisions" {
		t.Fatalf("last group = %#v, want virtual Trust Decisions governance view", governance)
	}
}

func TestDashboardMeta_DiscoveryFailureReturnsEmptyCatalog(t *testing.T) {
	resetDiscoveryCache(t)
	previousMode := k8sMode
	k8sMode = true
	t.Cleanup(func() { k8sMode = previousMode })
	primeDiscoveryCache(nil, errors.New("discovery unavailable"))

	response := httptest.NewRecorder()
	handleDashMeta(response, httptest.NewRequest(http.MethodGet, "/api/meta", nil))
	var meta dashboardMeta
	if err := json.NewDecoder(response.Body).Decode(&meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if len(meta.Groups) != 0 {
		t.Fatalf("groups = %#v, want fail-closed empty catalog", meta.Groups)
	}
}

func TestDashboardNamespaces_KubernetesResultsAndFailure(t *testing.T) {
	resetDiscoveryCache(t)
	previousMode := k8sMode
	k8sMode = true
	t.Cleanup(func() { k8sMode = previousMode })

	namespacesGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(namespacesGVR.GroupVersion().WithKind("Namespace"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(namespacesGVR.GroupVersion().WithKind("NamespaceList"), &unstructured.UnstructuredList{})
	k8sDynClient = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{namespacesGVR: "NamespaceList"},
		&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "team-a"}}},
		&unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "team-b"}}},
	)

	response := httptest.NewRecorder()
	handleDashNamespaces(response, httptest.NewRequest(http.MethodGet, "/api/namespaces", nil))
	if response.Code != http.StatusOK || response.Body.String() != "[\"team-a\",\"team-b\"]\n" {
		t.Fatalf("successful response: status = %d, body = %s", response.Code, response.Body.String())
	}

	k8sDynClient = nil
	response = httptest.NewRecorder()
	handleDashNamespaces(response, httptest.NewRequest(http.MethodGet, "/api/namespaces", nil))
	if response.Code < http.StatusBadRequest || !strings.Contains(response.Body.String(), "error") {
		t.Fatalf("failure response: status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestDashboardManifestHandlersRejectMalformedInput(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid manifest reached backend")
	}))
	defer backend.Close()
	restore := configureGetTest(backend)
	defer restore()

	oversized := strings.Repeat("x", (4<<20)+1)
	for _, tc := range []struct {
		name       string
		handler    func(http.ResponseWriter, *http.Request)
		method     string
		target     string
		pathValues map[string]string
		body       string
		want       string
	}{
		{name: "create empty", handler: handleDashCreate, method: http.MethodPost, target: "/api/resources/agents", pathValues: map[string]string{"kind": "agents"}, want: "empty request body"},
		{name: "create malformed", handler: handleDashCreate, method: http.MethodPost, target: "/api/resources/agents", pathValues: map[string]string{"kind": "agents"}, body: ":\n", want: "parse manifest"},
		{name: "create non-object", handler: handleDashCreate, method: http.MethodPost, target: "/api/resources/agents", pathValues: map[string]string{"kind": "agents"}, body: "null\n", want: "manifest is empty or not an object"},
		{name: "create oversized", handler: handleDashCreate, method: http.MethodPost, target: "/api/resources/agents", pathValues: map[string]string{"kind": "agents"}, body: oversized, want: "request body too large"},
		{name: "update empty", handler: handleDashUpdate, method: http.MethodPut, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}, want: "empty request body"},
		{name: "update malformed", handler: handleDashUpdate, method: http.MethodPut, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}, body: ":\n", want: "parse manifest"},
		{name: "update non-object", handler: handleDashUpdate, method: http.MethodPut, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}, body: "null\n", want: "manifest is empty or not an object"},
		{name: "update wrong kind", handler: handleDashUpdate, method: http.MethodPut, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}, body: "kind: Tool\nmetadata:\n  name: planner\n", want: "does not match URL"},
		{name: "update oversized", handler: handleDashUpdate, method: http.MethodPut, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}, body: oversized, want: "request body too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := dashboardRequest(tc.method, tc.target, tc.body, tc.pathValues)
			response := httptest.NewRecorder()
			tc.handler(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), tc.want) {
				t.Fatalf("status = %d, body = %s; want 400 containing %q", response.Code, response.Body.String(), tc.want)
			}
		})
	}
}

func TestDashboardParseYAML_InputBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
		wantBody   string
	}{
		{name: "empty", wantStatus: http.StatusOK, wantBody: "{}"},
		{name: "malformed", body: ":\n", wantStatus: http.StatusBadRequest, wantBody: "parse yaml"},
		{name: "oversized", body: strings.Repeat("x", (4<<20)+1), wantStatus: http.StatusBadRequest, wantBody: "request body too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/parse-yaml", strings.NewReader(tc.body))
			response := httptest.NewRecorder()
			handleDashParseYAML(response, request)
			if response.Code != tc.wantStatus || !strings.Contains(response.Body.String(), tc.wantBody) {
				t.Fatalf("status = %d, body = %s; want status %d containing %q", response.Code, response.Body.String(), tc.wantStatus, tc.wantBody)
			}
		})
	}
}

func TestDashboardMutationBackendErrorsMapToHTTPStatus(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
	}))
	defer backend.Close()
	restore := configureGetTest(backend)
	defer restore()

	for _, tc := range []struct {
		name       string
		handler    func(http.ResponseWriter, *http.Request)
		method     string
		target     string
		pathValues map[string]string
		body       string
	}{
		{name: "create", handler: handleDashCreate, method: http.MethodPost, target: "/api/resources/agents", pathValues: map[string]string{"kind": "agents"}, body: "kind: Agent\nmetadata:\n  name: planner\n"},
		{name: "update", handler: handleDashUpdate, method: http.MethodPut, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}, body: "kind: Agent\nmetadata:\n  name: planner\n  resourceVersion: \"1\"\n"},
		{name: "delete", handler: handleDashDelete, method: http.MethodDelete, target: "/api/resources/agents/planner", pathValues: map[string]string{"kind": "agents", "name": "planner"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := dashboardRequest(tc.method, tc.target, tc.body, tc.pathValues)
			response := httptest.NewRecorder()
			tc.handler(response, request)
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "server error (503)") {
				t.Fatalf("status = %d, body = %s; want 502 with upstream status", response.Code, response.Body.String())
			}
		})
	}
}

func TestDashboardMutationsPropagateTransportErrors(t *testing.T) {
	previousMode, previousClient := k8sMode, httpClient
	k8sMode = false
	httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport unavailable")
	})}
	t.Cleanup(func() { k8sMode, httpClient = previousMode, previousClient })

	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "create", run: func() error {
			_, err := dashCreate("team-a", "agents", map[string]any{"kind": "Agent"})
			return err
		}},
		{name: "update", run: func() error {
			_, err := dashUpdate("team-a", "agents", "planner", map[string]any{"kind": "Agent"})
			return err
		}},
		{name: "delete", run: func() error { return dashDelete("team-a", "agents", "planner") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "transport unavailable") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func dashboardRequest(method, target, body string, pathValues map[string]string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for key, value := range pathValues {
		req.SetPathValue(key, value)
	}
	return req
}
