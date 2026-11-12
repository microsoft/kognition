// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewedDashboardActionsUseOwnedRealAPIContracts(test *testing.T) {
	for _, scenario := range []struct{ action, body, method, path string }{
		{"search", `{"action":"search","query":"fixture","tag":["owned"]}`, "GET", "/api/search"},
		{"get", `{"action":"get","uid":"fixture-dash"}`, "GET", "/api/dashboards/uid/fixture-dash"},
		{"create", `{"action":"create","dashboard":{"uid":"fixture-dash","title":"fixture"}}`, "POST", "/api/dashboards/db"},
		{"update", `{"action":"update","dashboard":{"uid":"fixture-dash","title":"updated"},"overwrite":true}`, "POST", "/api/dashboards/db"},
		{"delete", `{"action":"delete","uid":"fixture-dash"}`, "DELETE", "/api/dashboards/uid/fixture-dash"},
		{"versions", `{"action":"get_versions","uid":"fixture-dash"}`, "GET", "/api/dashboards/uid/fixture-dash/versions"},
		{"version", `{"action":"get_version","uid":"fixture-dash","version_id":1}`, "GET", "/api/dashboards/uid/fixture-dash/versions/1"},
		{"restore", `{"action":"restore_version","uid":"fixture-dash","version_id":1}`, "POST", "/api/dashboards/uid/fixture-dash/restore"},
		{"permissions", `{"action":"get_permissions","uid":"fixture-dash"}`, "GET", "/api/dashboards/uid/fixture-dash/permissions"},
		{"update permissions", `{"action":"update_permissions","uid":"fixture-dash","permissions":[{"role":"Viewer","permission":1}]}`, "POST", "/api/dashboards/uid/fixture-dash/permissions"},
		{"diff", `{"action":"calculate_diff","base_uid":"fixture-dash","new_uid":"fixture-new","base_version":1}`, "POST", "/api/dashboards/calculate-diff"},
	} {
		test.Run(scenario.action, func(test *testing.T) {
			calls := 0
			backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/dashboards/uid/") && !strings.Contains(request.URL.Path, "/versions") && !strings.Contains(request.URL.Path, "/permissions") {
					if scenario.action == "get" {
						calls++
					}
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write([]byte(`{"meta":{"folderUid":"fixture-folder"},"dashboard":{"id":10,"uid":"fixture-dash"}}`))
					return
				}
				calls++
				if request.Method != scenario.method || request.URL.Path != scenario.path || request.Header.Get("Authorization") != "Bearer fixture-token" {
					test.Error("reviewed Grafana request changed")
				}
				if scenario.action == "search" && request.URL.Query().Get("folderUIDs") != "fixture-folder" {
					test.Error("dashboard search escaped owned folder")
				}
				if scenario.action == "create" || scenario.action == "update" {
					var payload map[string]any
					if json.NewDecoder(request.Body).Decode(&payload) != nil || payload["folderUid"] != "fixture-folder" {
						test.Error("dashboard write escaped owned folder")
					}
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"verified":true}`))
			}))
			defer backend.Close()
			bridge, err := NewObservabilityBridge(ObservabilityConfig{Namespace: "fixture", Kubeconfig: "/operator/kubeconfig", PrometheusURL: backend.URL, GrafanaURL: backend.URL + "/api", GrafanaToken: "fixture-token", DashboardPrefix: "fixture-", FolderUID: "fixture-folder", HTTPClient: backend.Client(), RunKubectl: func(context.Context, []string) (string, error) { return "", nil }})
			if err != nil {
				test.Fatal(err)
			}
			response := httptest.NewRecorder()
			bridge.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/tools/DashboardManagementTool", strings.NewReader(scenario.body)))
			if response.Code != http.StatusOK || calls != 1 {
				test.Fatalf("owned action result=%d calls=%d", response.Code, calls)
			}
			response = httptest.NewRecorder()
			bridge.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/tools/DashboardManagementTool", strings.NewReader(`{"action":"delete","uid":"unowned-dashboard"}`)))
			if response.Code != http.StatusForbidden || calls != 1 {
				test.Fatal("unowned dashboard reached backend")
			}
		})
	}
}

func TestObservabilityBridgePreservesReadToolRequests(test *testing.T) {
	var command []string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/query" || request.URL.Query().Get("query") != "up" {
			test.Error("Prometheus request semantics changed")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer backend.Close()
	bridge, err := NewObservabilityBridge(ObservabilityConfig{Namespace: "fixture", Kubeconfig: "/operator/kubeconfig", PrometheusURL: backend.URL, GrafanaURL: backend.URL, DashboardPrefix: "fixture-", FolderUID: "fixture-folder", HTTPClient: backend.Client(), RunKubectl: func(_ context.Context, args []string) (string, error) { command = args; return `{"items":[]}`, nil }})
	if err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/tools/get_resources", strings.NewReader(`{"resource_type":"pods","name":null,"all_namespaces":false,"ns":"fixture","output":"json"}`))
	response := httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Join(command, " ") != "--kubeconfig /operator/kubeconfig get pods -n fixture -o json" {
		test.Fatalf("read mapping status=%d args=%v", response.Code, command)
	}
	command = nil
	request = httptest.NewRequest(http.MethodPost, "/tools/get_resources", strings.NewReader(`{"resource_type":"secrets","ns":"fixture","output":"json"}`))
	response = httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || command != nil {
		test.Fatal("restricted resource reached Kubernetes")
	}
	request = httptest.NewRequest(http.MethodPost, "/tools/QueryTool", strings.NewReader(`{"query":"up","time":123,"timeout":"5s"}`))
	response = httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	var result map[string]any
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result["status"] != "success" {
		test.Fatal("Prometheus result changed")
	}
	bridge.config.GeneratePromQL = func(context.Context, string, string) (string, error) { return "", ErrNestedOutcomeUncertain }
	request = httptest.NewRequest(http.MethodPost, "/tools/GeneratePromQLTool", strings.NewReader(`{"query_description":"generate uptime query"}`))
	request.Header.Set("X-Kognition-Tool-Invocation-Id", "approved-invocation")
	response = httptest.NewRecorder()
	bridge.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &result) != nil || result["runtime_approval_state"] != "Unavailable" {
		test.Fatal("unknown nested model outcome lost its runtime barrier")
	}
}
