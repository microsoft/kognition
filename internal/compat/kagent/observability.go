// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ObservabilityConfig struct {
	Namespace       string
	Kubeconfig      string
	PrometheusURL   string
	GrafanaURL      string
	GrafanaToken    string
	DashboardPrefix string
	FolderUID       string
	HTTPClient      *http.Client
	RunKubectl      func(context.Context, []string) (string, error)
	GeneratePromQL  func(context.Context, string, string) (string, error)
}

type ObservabilityBridge struct {
	config ObservabilityConfig
	client *http.Client
}

func NewObservabilityBridge(config ObservabilityConfig) (*ObservabilityBridge, error) {
	if config.Namespace == "" || config.Kubeconfig == "" || config.RunKubectl == nil || config.DashboardPrefix == "" || config.FolderUID == "" {
		return nil, errors.New("observability bridge requires explicit namespace, command runner and owned dashboard scope")
	}
	for _, address := range []string{config.PrometheusURL, config.GrafanaURL} {
		endpoint, err := url.Parse(address)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return nil, errors.New("operator backend URL is invalid")
		}
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if config.HTTPClient != nil {
		copy := *config.HTTPClient
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ObservabilityBridge{config: config, client: client}, nil
}

func (bridge *ObservabilityBridge) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		http.Error(writer, "tool input exceeds bounds", http.StatusBadRequest)
		return
	}
	var result any
	switch request.URL.Path {
	case "/tools/get_resources":
		result, err = bridge.resources(request.Context(), body)
	case "/tools/get_available_api_resources":
		var args map[string]any
		if json.Unmarshal(body, &args) != nil || len(args) != 0 {
			err = errors.New("API discovery takes no arguments")
		} else {
			result, err = bridge.config.RunKubectl(request.Context(), []string{"--kubeconfig", bridge.config.Kubeconfig, "api-resources"})
		}
	case "/tools/QueryTool":
		var args struct {
			Query   string  `json:"query"`
			Time    any     `json:"time"`
			Timeout *string `json:"timeout"`
		}
		if json.Unmarshal(body, &args) != nil || strings.TrimSpace(args.Query) == "" {
			err = errors.New("Prometheus query is required")
		} else {
			result, err = bridge.backend(request.Context(), bridge.config.PrometheusURL, http.MethodGet, "/query", url.Values{"query": {args.Query}}, nil, "")
		}
	case "/tools/GeneratePromQLTool":
		var args struct {
			Description string `json:"query_description"`
		}
		invocation := request.Header.Get("X-Kognition-Tool-Invocation-Id")
		if json.Unmarshal(body, &args) != nil || strings.TrimSpace(args.Description) == "" || invocation == "" || bridge.config.GeneratePromQL == nil {
			err = errors.New("governed PromQL generation requires description and stable invocation")
		} else {
			result, err = bridge.config.GeneratePromQL(request.Context(), invocation, args.Description)
		}
	case "/tools/DashboardManagementTool":
		result, err = bridge.dashboard(request.Context(), body)
	default:
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if errors.Is(err, ErrNestedOutcomeUncertain) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(writer).Encode(map[string]string{"runtime_approval_state": "Unavailable", "error": "Nested model completion is unconfirmed; retain the invocation and AgentJob."})
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusForbidden)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(result)
}

func (bridge *ObservabilityBridge) resources(ctx context.Context, body []byte) (any, error) {
	var args struct {
		Name          *string `json:"name"`
		ResourceType  string  `json:"resource_type"`
		AllNamespaces *bool   `json:"all_namespaces"`
		Namespace     *string `json:"ns"`
		Output        *string `json:"output"`
	}
	if json.Unmarshal(body, &args) != nil {
		return nil, errors.New("invalid Kubernetes read input")
	}
	allowed := map[string]bool{"pods": true, "pod": true, "services": true, "service": true, "deployments": true, "deployment": true, "configmaps": true, "configmap": true}
	if !allowed[args.ResourceType] || (args.AllNamespaces != nil && *args.AllNamespaces) || (args.Namespace != nil && *args.Namespace != "" && *args.Namespace != bridge.config.Namespace) {
		return nil, errors.New("Kubernetes read is outside the reviewed namespace/resource scope")
	}
	command := []string{"--kubeconfig", bridge.config.Kubeconfig, "get", args.ResourceType}
	if args.Name != nil && *args.Name != "" {
		if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}$`).MatchString(*args.Name) {
			return nil, errors.New("invalid Kubernetes resource name")
		}
		command = append(command, *args.Name)
	}
	command = append(command, "-n", bridge.config.Namespace)
	if args.Output != nil && *args.Output != "" {
		switch *args.Output {
		case "json", "yaml", "wide":
			command = append(command, "-o", *args.Output)
		default:
			return nil, errors.New("unreviewed Kubernetes output format")
		}
	}
	return bridge.config.RunKubectl(ctx, command)
}

func (bridge *ObservabilityBridge) backend(ctx context.Context, base, method, path string, query url.Values, payload any, token string) (any, error) {
	endpoint, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return nil, errors.New("invalid operator endpoint")
	}
	endpoint.RawQuery = query.Encode()
	var reader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := bridge.client.Do(request)
	if err != nil {
		return nil, errors.New("operator backend unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("backend response exceeds bounds")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("operator backend returned HTTP %d", response.StatusCode)
	}
	var result any
	if json.Unmarshal(body, &result) != nil {
		return nil, errors.New("backend response is not JSON")
	}
	return result, nil
}

func (bridge *ObservabilityBridge) dashboard(ctx context.Context, body []byte) (any, error) {
	var args struct {
		Action      string           `json:"action"`
		Query       *string          `json:"query"`
		Tags        []string         `json:"tag"`
		FolderUIDs  []string         `json:"folder_uids"`
		Starred     *bool            `json:"starred"`
		Limit       *int             `json:"limit"`
		UID         string           `json:"uid"`
		Dashboard   map[string]any   `json:"dashboard"`
		FolderUID   string           `json:"folder_uid"`
		Overwrite   bool             `json:"overwrite"`
		Message     string           `json:"message"`
		VersionID   int              `json:"version_id"`
		BaseUID     string           `json:"base_uid"`
		BaseVersion int              `json:"base_version"`
		NewUID      string           `json:"new_uid"`
		NewVersion  int              `json:"new_version"`
		DiffType    string           `json:"diff_type"`
		Permissions []map[string]any `json:"permissions"`
	}
	if json.Unmarshal(body, &args) != nil {
		return nil, errors.New("invalid dashboard input")
	}
	owned := func(uid string) bool {
		return strings.HasPrefix(uid, bridge.config.DashboardPrefix) && regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`).MatchString(uid)
	}
	call := func(method, path string, query url.Values, payload any) (any, error) {
		return bridge.backend(ctx, bridge.config.GrafanaURL, method, path, query, payload, bridge.config.GrafanaToken)
	}
	if args.Action == "search" {
		for _, folder := range args.FolderUIDs {
			if folder != bridge.config.FolderUID {
				return nil, errors.New("unowned dashboard folder")
			}
		}
		query := url.Values{"folderUIDs": {bridge.config.FolderUID}}
		if args.Query != nil {
			query.Set("query", *args.Query)
		}
		for _, tag := range args.Tags {
			query.Add("tag", tag)
		}
		if args.Starred != nil {
			query.Set("starred", strconv.FormatBool(*args.Starred))
		}
		if args.Limit != nil {
			query.Set("limit", strconv.Itoa(*args.Limit))
		}
		return call(http.MethodGet, "/search", query, nil)
	}
	if args.Action == "create" || args.Action == "update" {
		uid, _ := args.Dashboard["uid"].(string)
		if !owned(uid) || (args.FolderUID != "" && args.FolderUID != bridge.config.FolderUID) {
			return nil, errors.New("dashboard write is outside owned scope")
		}
		existing, err := bridge.ownedDashboard(ctx, uid)
		if err != nil && !(args.Action == "create" && err.Error() == "operator backend returned HTTP 404") {
			return nil, err
		}
		if requestedID, present := args.Dashboard["id"]; present && requestedID != nil && requestedID != float64(0) {
			dashboard, _ := existing["dashboard"].(map[string]any)
			if dashboard == nil || dashboard["id"] != requestedID {
				return nil, errors.New("dashboard ID is outside the owned UID scope")
			}
		}
		payload := map[string]any{"dashboard": args.Dashboard, "overwrite": args.Overwrite, "folderUid": bridge.config.FolderUID}
		if args.Message != "" {
			payload["message"] = args.Message
		}
		return call(http.MethodPost, "/dashboards/db", nil, payload)
	}
	if args.Action == "calculate_diff" {
		if !owned(args.BaseUID) || !owned(args.NewUID) {
			return nil, errors.New("unowned dashboard diff")
		}
		if _, err := bridge.ownedDashboard(ctx, args.BaseUID); err != nil {
			return nil, err
		}
		if _, err := bridge.ownedDashboard(ctx, args.NewUID); err != nil {
			return nil, err
		}
		base, newVersion := map[string]any{"dashboardId": args.BaseUID}, map[string]any{"dashboardId": args.NewUID}
		if args.BaseVersion != 0 {
			base["version"] = args.BaseVersion
		}
		if args.NewVersion != 0 {
			newVersion["version"] = args.NewVersion
		}
		diff := args.DiffType
		if diff == "" {
			diff = "basic"
		}
		return call(http.MethodPost, "/dashboards/calculate-diff", nil, map[string]any{"base": base, "new": newVersion, "diffType": diff})
	}
	if !owned(args.UID) {
		return nil, errors.New("unowned dashboard UID")
	}
	verified, err := bridge.ownedDashboard(ctx, args.UID)
	if err != nil {
		return nil, err
	}
	path := "/dashboards/uid/" + url.PathEscape(args.UID)
	switch args.Action {
	case "get":
		return verified, nil
	case "delete":
		return call(http.MethodDelete, path, nil, nil)
	case "get_versions":
		query := url.Values{}
		if args.Limit != nil {
			query.Set("limit", strconv.Itoa(*args.Limit))
		}
		return call(http.MethodGet, path+"/versions", query, nil)
	case "get_version":
		if args.VersionID <= 0 {
			return nil, errors.New("dashboard version required")
		}
		return call(http.MethodGet, path+"/versions/"+strconv.Itoa(args.VersionID), nil, nil)
	case "restore_version":
		if args.VersionID <= 0 {
			return nil, errors.New("dashboard version required")
		}
		return call(http.MethodPost, path+"/restore", nil, map[string]any{"version": args.VersionID})
	case "get_permissions":
		return call(http.MethodGet, path+"/permissions", nil, nil)
	case "update_permissions":
		if len(args.Permissions) == 0 {
			return nil, errors.New("permissions required")
		}
		return call(http.MethodPost, path+"/permissions", nil, map[string]any{"items": args.Permissions})
	default:
		return nil, errors.New("unsupported reviewed dashboard action")
	}
}

func (bridge *ObservabilityBridge) ownedDashboard(ctx context.Context, uid string) (map[string]any, error) {
	result, err := bridge.backend(ctx, bridge.config.GrafanaURL, http.MethodGet, "/dashboards/uid/"+url.PathEscape(uid), nil, nil, bridge.config.GrafanaToken)
	if err != nil {
		return nil, err
	}
	document, _ := result.(map[string]any)
	metadata, _ := document["meta"].(map[string]any)
	if metadata == nil || metadata["folderUid"] != bridge.config.FolderUID {
		return nil, errors.New("dashboard is not in the owned folder")
	}
	return document, nil
}
