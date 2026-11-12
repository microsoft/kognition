// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

//go:embed dashboard_assets
var dashboardAssets embed.FS

var (
	dashboardPort           int
	dashboardBind           string
	dashboardOpen           bool
	dashboardRunProofPath   string
	dashboardAuditNamespace string
)

const defaultDashboardAuditNamespace = "kognition-system"

var dashboardCmd = &cobra.Command{
	Use:   "dashboard",
	Short: "Launch the local Kognition dashboard",
	Long: `Start a local web dashboard for browsing Kognition resources
(agents, workflows, memory, models, tools, governance, ...) in a browser.

The dashboard runs in-process and reuses the same backend as the rest of the
CLI (standalone apiserver or Kubernetes mode — see --mode). It prints the URL
on startup; open it in your browser to use the UI. Press Ctrl+C to stop.

Trust Focus can expose optional observability links configured through
KOGNITION_DASHBOARD_JAEGER_URL and KOGNITION_DASHBOARD_GRAFANA_URL. Only
credential-free HTTP(S) URLs without query parameters or fragments are used.`,
	RunE: runDashboard,
}

func init() {
	dashboardCmd.Flags().IntVar(&dashboardPort, "port", 0, "Port to bind (0 = pick a free port)")
	dashboardCmd.Flags().StringVar(&dashboardBind, "bind", "127.0.0.1", "Address to bind the dashboard server")
	dashboardCmd.Flags().BoolVar(&dashboardOpen, "open", false, "Open the dashboard in the default browser after start")
	dashboardCmd.Flags().StringVar(&dashboardRunProofPath, "run-proof", "", "Optional path to a Run Proof report")
	dashboardCmd.Flags().StringVar(&dashboardAuditNamespace, "audit-namespace", defaultDashboardAuditNamespace, "Kubernetes namespace containing immutable audit ConfigMaps (empty = --kube-namespace)")
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runDashboardServer(ctx, localUIServerOptions{})
}

func runDashboardServer(ctx context.Context, opts localUIServerOptions) error {
	assets, err := fs.Sub(dashboardAssets, "dashboard_assets")
	if err != nil {
		return fmt.Errorf("load dashboard assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	registerResourceAPI(mux)
	mux.HandleFunc("GET /api/events/{kind}/{name}", handleDashEvents)
	mux.HandleFunc("GET /api/run-proof", handleDashRunProof)

	if opts.name == "" {
		opts.name = "dashboard"
	}
	if opts.bind == "" {
		opts.bind = dashboardBind
	}
	if opts.port == 0 {
		opts.port = dashboardPort
	}
	if !opts.openBrowser {
		opts.openBrowser = dashboardOpen
	}
	opts.handler = mux
	return runLocalUIServer(ctx, opts)
}

func currentModeLabel() string {
	if k8sMode {
		return "kubernetes"
	}
	return "standalone"
}

// dashboardGroup describes a sidebar group of resource kinds.
type dashboardGroup struct {
	Title string              `json:"title"`
	Items []dashboardResource `json:"items"`
}

type dashboardResource struct {
	Label    string `json:"label"`
	Singular string `json:"singular"`
	Plural   string `json:"plural"`
	View     string `json:"view,omitempty"`
}

func dashboardCatalog() []dashboardGroup {
	r := func(label, singular string) dashboardResource {
		return dashboardResource{Label: label, Singular: singular, Plural: pluralize(singular)}
	}
	v := func(label, view string) dashboardResource {
		return dashboardResource{Label: label, View: view}
	}
	return []dashboardGroup{
		{Title: "Compute", Items: []dashboardResource{
			r("Agents", "agent"),
			r("Agent Instances", "agentinstance"),
			r("Agent Jobs", "agentjob"),
		}},
		{Title: "Workflows", Items: []dashboardResource{
			r("Workflows", "workflow"),
			r("Workflow Runs", "workflowrun"),
		}},
		{Title: "Quality", Items: []dashboardResource{
			r("Evaluation Datasets", "evaluationdataset"),
			r("Evaluations", "evaluation"),
			r("Evaluation Runs", "evaluationrun"),
		}},
		{Title: "Models", Items: []dashboardResource{
			r("Models", "model"),
			r("Model Classes", "modelclass"),
		}},
		{Title: "Tools", Items: []dashboardResource{
			r("Tools", "tool"),
			r("Tool Classes", "toolclass"),
		}},
		{Title: "Memory", Items: []dashboardResource{
			r("Memory Claims", "memoryclaim"),
			r("Memory Classes", "memoryclass"),
			r("Memory Volumes", "memoryvolume"),
		}},
		{Title: "Knowledge", Items: []dashboardResource{
			r("Knowledge Bases", "knowledgebase"),
			r("Knowledge Base Classes", "knowledgebaseclass"),
			r("Knowledge Base Claims", "knowledgebaseclaim"),
			r("Prompts", "prompt"),
		}},
		{Title: "Governance", Items: []dashboardResource{
			r("Agent Policies", "agentpolicy"),
			r("Admission Policies", "cognitiveadmissionpolicy"),
			r("Quotas", "cognitivequota"),
			r("Limit Ranges", "cognitivelimitrange"),
			r("Trust Profiles", "trustprofile"),
			r("Approval Decisions", "approvaldecision"),
			v("Trust Decisions", "trust-decisions"),
		}},
		{Title: "Identity", Items: []dashboardResource{
			r("Agent Identities", "agentidentity"),
			r("Agent Roles", "agentrole"),
			r("Agent Role Bindings", "agentrolebinding"),
			r("Secret Refs", "secretref"),
		}},
		{Title: "Authoring", Items: []dashboardResource{
			r("Workspaces", "workspace"),
		}},
	}
}

type dashboardMeta struct {
	Mode           string                 `json:"mode"`
	Namespace      string                 `json:"namespace"`
	AuditNamespace string                 `json:"auditNamespace,omitempty"`
	Groups         []dashboardGroup       `json:"groups"`
	HasEvents      bool                   `json:"hasEvents"`
	Observability  dashboardObservability `json:"observability"`
}

type dashboardObservability struct {
	JaegerURL  string `json:"jaegerUrl,omitempty"`
	GrafanaURL string `json:"grafanaUrl,omitempty"`
}

func dashboardPublicURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.TrimRight(parsed.String(), "/")
}

func handleDashMeta(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, dashboardMetadata())
}

func dashboardMetadata() dashboardMeta {
	auditNamespace := ""
	if k8sMode {
		auditNamespace = effectiveDashboardAuditNamespace()
	}
	return dashboardMeta{
		Mode:           currentModeLabel(),
		Namespace:      namespace,
		AuditNamespace: auditNamespace,
		Groups:         filterCatalog(dashboardCatalog()),
		HasEvents:      true,
		Observability: dashboardObservability{
			JaegerURL:  dashboardPublicURL(os.Getenv("KOGNITION_DASHBOARD_JAEGER_URL")),
			GrafanaURL: dashboardPublicURL(os.Getenv("KOGNITION_DASHBOARD_GRAFANA_URL")),
		},
	}
}

// filterCatalog removes resource kinds that are not installed in the cluster
// (k8s mode only). In standalone mode the full catalog is returned.
// On discovery failure in k8s mode this returns an empty catalog (fail-closed)
// rather than the full catalog, which would expose kinds whose CRDs may not exist.
func filterCatalog(groups []dashboardGroup) []dashboardGroup {
	if !k8sMode {
		return groups
	}
	available, err := k8sAvailableKognitionResources()
	if err != nil || available == nil {
		return []dashboardGroup{}
	}
	out := make([]dashboardGroup, 0, len(groups))
	for _, g := range groups {
		items := make([]dashboardResource, 0, len(g.Items))
		for _, it := range g.Items {
			if it.View != "" || available[it.Plural] {
				items = append(items, it)
			}
		}
		if len(items) > 0 {
			out = append(out, dashboardGroup{Title: g.Title, Items: items})
		}
	}
	return out
}

// handleDashNamespaces returns the list of namespaces in the cluster (k8s mode only).
// In standalone mode it returns an empty list so the UI can fall back to free-text input.
func handleDashNamespaces(w http.ResponseWriter, _ *http.Request) {
	if !k8sMode {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	ns, err := k8sListNamespaces()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func handleDashList(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	ns := dashboardNamespace(r)
	body, err := dashList(ns, kind)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, body)
}

func handleDashGet(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	body, err := dashGet(ns, kind, name)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, body)
}

func handleDashUpdate(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	ns := dashboardNamespace(r)

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty request body"})
		return
	}

	// yaml.v3 parses JSON as well (JSON is valid YAML); decode once for both content types.
	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse manifest: %v", err)})
		return
	}
	if obj == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "manifest is empty or not an object"})
		return
	}

	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	// Cross-check / fill name + namespace from URL.
	if mn, ok := meta["name"].(string); ok && mn != "" && mn != name {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("metadata.name %q does not match URL name %q", mn, name)})
		return
	}
	meta["name"] = name
	if ns != "" {
		if mns, ok := meta["namespace"].(string); ok && mns != "" && mns != ns {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("metadata.namespace %q does not match URL namespace %q", mns, ns)})
			return
		}
		meta["namespace"] = ns
	} else if mns, ok := meta["namespace"].(string); ok && mns != "" {
		ns = mns
	} else {
		ns = "default"
		meta["namespace"] = ns
	}
	// Validate kind against URL (compare via singular and plural forms).
	if k, ok := obj["kind"].(string); ok && k != "" {
		if !strings.EqualFold(pluralize(k), kind) && !strings.EqualFold(k, kind) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("kind %q does not match URL %q", k, kind)})
			return
		}
	}

	// Make sure we send a resourceVersion: fetch current if missing so the user
	// can paste a clean manifest without it.
	if _, ok := meta["resourceVersion"]; !ok || meta["resourceVersion"] == "" {
		if existing, err := dashGet(ns, kind, name); err == nil && len(existing) > 0 {
			var cur map[string]any
			if jsonErr := json.Unmarshal(existing, &cur); jsonErr == nil {
				if cm, ok := cur["metadata"].(map[string]any); ok {
					if rv, ok := cm["resourceVersion"]; ok {
						meta["resourceVersion"] = rv
					}
				}
			}
		}
	}

	body, err := dashUpdate(ns, kind, name, obj)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, body)
}

func handleDashEvents(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	if k8sMode {
		k8sKind := kindFor(kind)
		if k8sKind == "" {
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
			return
		}
		evs, err := k8sListEvents(r.Context(), ns, k8sKind, name)
		if err != nil {
			writeErr(w, err)
			return
		}
		items := make([]map[string]any, 0, len(evs))
		for _, ev := range evs {
			items = append(items, map[string]any{
				"reason":    ev.Reason,
				"type":      ev.Type,
				"message":   ev.Message,
				"timestamp": eventTime(ev).Format(time.RFC3339),
				"count":     ev.Count,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	url := fmt.Sprintf("%s/events", apiURLName(ns, kind, name))
	body, err := doGet(url)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, body)
}

// registerResourceAPI wires the shared `/api/*` CRUD routes used by both the
// dashboard and the studio. The dashboard additionally wires `/api/events/...`
// itself; the studio doesn't need it.
func registerResourceAPI(mux *http.ServeMux) {
	registerResourceAPIWithMeta(mux, handleDashMeta)
}

func registerResourceAPIWithMeta(mux *http.ServeMux, meta http.HandlerFunc) {
	mux.HandleFunc("GET /api/meta", meta)
	mux.HandleFunc("GET /api/namespaces", handleDashNamespaces)
	mux.HandleFunc("GET /api/trust-decisions", handleDashTrustDecisions)
	mux.HandleFunc("POST /api/parse-yaml", handleDashParseYAML)
	mux.HandleFunc("GET /api/resources/{kind}", handleDashList)
	mux.HandleFunc("POST /api/resources/{kind}", handleDashCreate)
	mux.HandleFunc("GET /api/resources/{kind}/{name}", handleDashGet)
	mux.HandleFunc("PUT /api/resources/{kind}/{name}", handleDashUpdate)
	mux.HandleFunc("DELETE /api/resources/{kind}/{name}", handleDashDelete)
	registerGalleryAPI(mux)
}

func handleDashCreate(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	ns := dashboardNamespace(r)

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty request body"})
		return
	}
	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse manifest: %v", err)})
		return
	}
	if obj == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "manifest is empty or not an object"})
		return
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	if _, ok := meta["name"].(string); !ok || meta["name"] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "metadata.name is required"})
		return
	}
	if ns == "" {
		if mns, ok := meta["namespace"].(string); ok && mns != "" {
			ns = mns
		} else {
			ns = "default"
		}
	}
	meta["namespace"] = ns
	if k, ok := obj["kind"].(string); ok && k != "" {
		if !strings.EqualFold(pluralize(k), kind) && !strings.EqualFold(k, kind) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("kind %q does not match URL %q", k, kind)})
			return
		}
	}
	if pluralize(kind) == "approvaldecisions" {
		if err := bindApprovalDecisionApprover(r.Context(), obj); err != nil {
			writeErr(w, err)
			return
		}
	}
	body, err := dashCreate(ns, kind, obj)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeRaw(w, body)
}

func bindApprovalDecisionApprover(ctx context.Context, obj map[string]any) error {
	spec, ok := obj["spec"].(map[string]any)
	if !ok {
		return fmt.Errorf("ApprovalDecision spec must be an object")
	}
	approver := "studio-user"
	if k8sMode {
		var err error
		approver, err = k8sCurrentUsername(ctx)
		if err != nil {
			return err
		}
	}
	spec["approver"] = approver
	return nil
}

func handleDashDelete(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	if err := dashDelete(ns, kind, name); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "kind": kind, "name": name, "namespace": ns})
}

// handleDashParseYAML converts a YAML document to its JSON equivalent so the
// studio's YAML editor can round-trip back into the structured builder state.
func handleDashParseYAML(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	var obj any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse yaml: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, normalizeYAMLDecoded(obj))
}

// normalizeYAMLDecoded recursively converts map[interface{}]interface{} (which
// yaml.v3 sometimes returns for nested maps) into map[string]any so the result
// JSON-marshals cleanly.
func normalizeYAMLDecoded(v any) any {
	switch t := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[fmt.Sprintf("%v", k)] = normalizeYAMLDecoded(vv)
		}
		return m
	case map[string]any:
		for k, vv := range t {
			t[k] = normalizeYAMLDecoded(vv)
		}
		return t
	case []any:
		for i, vv := range t {
			t[i] = normalizeYAMLDecoded(vv)
		}
		return t
	default:
		return v
	}
}

func dashboardNamespace(r *http.Request) string {
	q := r.URL.Query()
	// If the client explicitly sends ?namespace= (even empty), honor it.
	// Empty string means "all namespaces" in k8s mode.
	if q.Has("namespace") {
		return q.Get("namespace")
	}
	return namespace
}

func dashList(ns, kind string) ([]byte, error) {
	if k8sMode {
		return k8sList(ns, kind)
	}
	return doGet(apiURL(ns, kind))
}

func dashGet(ns, kind, name string) ([]byte, error) {
	if k8sMode {
		return k8sGet(ns, kind, name)
	}
	return doGet(apiURLName(ns, kind, name))
}

func dashUpdate(ns, kind, name string, obj map[string]any) ([]byte, error) {
	if k8sMode {
		return k8sUpdate(ns, obj)
	}
	jsonData, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	url := apiURLName(ns, kind, name)
	resp, err := doHTTP(context.Background(), "PUT", url, jsonData)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func dashCreate(ns, kind string, obj map[string]any) ([]byte, error) {
	if k8sMode {
		return k8sCreate(ns, obj)
	}
	jsonData, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	url := apiURL(ns, kind)
	resp, err := doHTTP(context.Background(), "POST", url, jsonData)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func dashDelete(ns, kind, name string) error {
	if k8sMode {
		return k8sDelete(ns, kind, name)
	}
	url := apiURLName(ns, kind, name)
	resp, err := doHTTP(context.Background(), "DELETE", url, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server error (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, body []byte) {
	if json.Valid(body) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	_, _ = w.Write(body)
}

func writeErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	status := http.StatusBadGateway
	switch {
	case strings.Contains(msg, "not found"), strings.Contains(msg, "(404)"):
		status = http.StatusNotFound
	case strings.Contains(msg, "(401)"), strings.Contains(msg, "(403)"):
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]string{"error": msg})
}
