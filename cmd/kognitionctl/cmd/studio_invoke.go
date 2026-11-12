// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
)

// registerStudioInvokeAPI wires the studio-only interactive-preview endpoints
// that proxy chat to an Agent, create a WorkflowRun for a Workflow, and
// directly invoke a Tool for ad-hoc testing. These are deliberately scoped to
// the studio because the dashboard is read-only-ish and doesn't need an
// "execute" surface.
func registerStudioInvokeAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/invoke/agent/{name}", handleStudioInvokeAgent)
	mux.HandleFunc("POST /api/invoke/workflow/{name}", handleStudioInvokeWorkflow)
	mux.HandleFunc("POST /api/invoke/tool/{name}", handleStudioInvokeTool)
	mux.HandleFunc("GET /api/tools/{name}/schema", handleStudioToolSchema)
}

// decodeTool unmarshals a Tool from k8sGet/apiserver output while tolerating
// the metadata type mismatch between k8s core (resourceVersion: string) and
// our v1alpha1.ObjectMeta (int64). We only ever use spec here, so we decode
// spec separately and let metadata land in a generic map.
func decodeTool(raw []byte) (v1alpha1.Tool, error) {
	var shim struct {
		TypeMeta v1alpha1.TypeMeta   `json:",inline"`
		Spec     v1alpha1.ToolSpec   `json:"spec"`
		Status   v1alpha1.ToolStatus `json:"status,omitempty"`
		Metadata json.RawMessage     `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &shim); err != nil {
		return v1alpha1.Tool{}, err
	}
	tool := v1alpha1.Tool{
		TypeMeta: shim.TypeMeta,
		Spec:     shim.Spec,
		Status:   shim.Status,
	}
	// Best-effort extract of name/namespace — the fields we actually consult.
	if len(shim.Metadata) > 0 {
		var meta struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			Labels      map[string]string `json:"labels,omitempty"`
			Annotations map[string]string `json:"annotations,omitempty"`
		}
		_ = json.Unmarshal(shim.Metadata, &meta)
		tool.ObjectMeta.Name = meta.Name
		tool.ObjectMeta.Namespace = meta.Namespace
		tool.ObjectMeta.Labels = meta.Labels
		tool.ObjectMeta.Annotations = meta.Annotations
	}
	return tool, nil
}

// handleStudioInvokeAgent forwards a chat message to the agent and waits for
// the response. In standalone mode the apiserver's /chat subresource is
// synchronous so we just relay the body. In k8s mode the runtime exposes an
// async /invoke + /status pair, so we ack-then-poll until the turn completes.
// Response shape is normalised to {response, sessionID, toolCalls, usage?} so
// the frontend doesn't need to know which path served the request.
func handleStudioInvokeAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	if ns == "" {
		ns = "default"
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	var req struct {
		Message        string `json:"message"`
		SessionID      string `json:"sessionID,omitempty"`
		ConversationID string `json:"conversationID,omitempty"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse body: %v", err)})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message is required"})
		return
	}

	if k8sMode {
		studioInvokeAgentK8s(w, r, ns, name, req.Message, req.ConversationID)
		return
	}
	studioInvokeAgentStandalone(w, r, ns, name, req.Message, req.SessionID)
}

func studioInvokeAgentStandalone(w http.ResponseWriter, r *http.Request, ns, name, message, sessionID string) {
	body, _ := json.Marshal(map[string]string{"message": message, "sessionID": sessionID})
	url := fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/agents/%s/chat", serverURL, ns, name)
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("build request: %v", err)})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("invoke agent: %v", err)})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		writeJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("agent returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))})
		return
	}
	var parsed struct {
		SessionID string         `json:"sessionID"`
		Response  string         `json:"response"`
		Usage     map[string]any `json:"usage"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	out := map[string]any{
		"response":  parsed.Response,
		"sessionID": parsed.SessionID,
	}
	if parsed.Usage != nil {
		out["usage"] = parsed.Usage
	}
	writeJSON(w, http.StatusOK, out)
}

// studioInvokeAgentK8s routes the turn through the AgentInstance controller so
// the runtime receives the same scoped capabilities as production invocations.
func studioInvokeAgentK8s(w http.ResponseWriter, r *http.Request, namespace, name, message, conversationID string) {
	result, err := invokeInteractiveAgentK8s(r.Context(), namespace, name, message, conversationID, "studio")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	out := map[string]any{
		"response":        result.Output,
		"sessionID":       result.SessionID,
		"conversationID":  conversationID,
		"toolCalls":       result.ToolCalls,
		"toolCallsDetail": result.ToolCallsDetail,
	}
	if result.TokenUsage != nil {
		out["usage"] = result.TokenUsage
	}
	writeJSON(w, http.StatusOK, out)
}

// studioInvokeAgentRuntimeDirect drives the runtime's async invoke/status loop.
//
// The runtime treats `session_id` as a *turn identifier* (one execution of the
// graph). Posting the same session_id while phase is RUNNING or COMPLETED is
// treated as an idempotent retry and short-circuits without running the graph,
// so we always mint a fresh per-turn id here.
//
// Multi-turn conversation memory is keyed by `conversation_id`: the runtime
// keeps per-conversation message history and prepends it to each turn so the
// LLM sees the full chat. The client supplies a stable conversation id per
// chat tab; when missing, each turn is independent (no history).
func studioInvokeAgentRuntimeDirect(w http.ResponseWriter, r *http.Request, namespace, name, message, conversationID string) {
	sessionID := fmt.Sprintf("studio-%d", time.Now().UnixNano())
	invokeBody, _ := json.Marshal(map[string]string{
		"message":         message,
		"session_id":      sessionID,
		"conversation_id": conversationID,
	})

	baseURL, err := resolveAgentBaseURL(r.Context(), namespace, name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("resolve agent URL: %v", err)})
		return
	}
	authorization, err := resolveAgentRuntimeAuthorization(r.Context(), namespace, name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	invokeURL := baseURL + "/invoke"
	statusURL := baseURL + "/status"

	// 1. Submit the invoke, retrying through 409 (runtime busy with a prior
	// turn that hasn't drained yet). The runtime serialises turns so the only
	// way forward is to wait for the in-flight turn to finish.
	invokeDeadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, invokeURL, bytes.NewReader(invokeBody))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("build request: %v", err)})
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("invoke agent: %v", err)})
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusConflict {
			if time.Now().After(invokeDeadline) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent is still processing a previous turn; please wait and try again"})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		if resp.StatusCode >= 400 {
			writeJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("agent returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))})
			return
		}
		break
	}

	// 2. Poll /status until the runtime reports our session is Completed or
	// Failed. The runtime keeps the last completed output sticky, so we use
	// session_id to make sure we're observing our turn and not a previous one.
	// If the port-forward tunnel dies (pod replaced), we try to re-establish
	// it once and update statusURL.
	pollDeadline := time.Now().Add(5 * time.Minute)
	tunnelRetried := false
	var st struct {
		Phase           string           `json:"phase"`
		SessionID       string           `json:"session_id"`
		TokenUsage      map[string]any   `json:"token_usage"`
		ToolCalls       int              `json:"tool_calls"`
		ToolCallsDetail []map[string]any `json:"tool_calls_detail"`
		Output          string           `json:"output"`
		Error           string           `json:"error"`
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, statusURL, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("build status request: %v", err)})
			return
		}
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			// Connection error — the port-forward tunnel may have died.
			// Try to re-establish it once.
			if !tunnelRetried {
				tunnelRetried = true
				invalidatePortForward(namespace, name)
				newBase, pfErr := resolveAgentBaseURL(r.Context(), namespace, name)
				if pfErr == nil {
					statusURL = newBase + "/status"
				}
			}
			if time.Now().After(pollDeadline) {
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": fmt.Sprintf("status poll failed: %v", err)})
				return
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			writeJSON(w, resp.StatusCode, map[string]string{"error": fmt.Sprintf("status returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))})
			return
		}
		if err := json.Unmarshal(body, &st); err != nil {
			continue
		}
		if st.SessionID != sessionID {
			if time.Now().After(pollDeadline) {
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "timed out waiting for agent to start our turn"})
				return
			}
			continue
		}
		switch st.Phase {
		case "Completed":
			out := map[string]any{
				"response":        st.Output,
				"sessionID":       st.SessionID,
				"conversationID":  conversationID,
				"toolCalls":       st.ToolCalls,
				"toolCallsDetail": st.ToolCallsDetail,
			}
			if st.TokenUsage != nil {
				out["usage"] = st.TokenUsage
			}
			writeJSON(w, http.StatusOK, out)
			return
		case "Failed":
			msg := st.Error
			if msg == "" {
				msg = "agent run failed"
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
			return
		}
		if time.Now().After(pollDeadline) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "timed out waiting for agent response"})
			return
		}
	}
}

// handleStudioInvokeWorkflow creates a WorkflowRun for the named Workflow and
// returns the created run object. The frontend polls
// /api/resources/workflowruns/{name} to watch progress and pick up final
// status.output / status.phase.
func handleStudioInvokeWorkflow(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	if ns == "" {
		ns = "default"
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	var req struct {
		Input      string            `json:"input,omitempty"`
		Parameters map[string]string `json:"parameters,omitempty"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse body: %v", err)})
			return
		}
	}

	runName := studioRunName(name)
	spec := map[string]any{
		"workflowRef": map[string]any{"name": name},
	}
	if req.Input != "" {
		spec["input"] = req.Input
	}
	if len(req.Parameters) > 0 {
		spec["parameters"] = req.Parameters
	}
	obj := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "WorkflowRun",
		"metadata": map[string]any{
			"name":      runName,
			"namespace": ns,
			"labels": map[string]string{
				"kognition.dev/created-by": "studio",
				"kognition.dev/workflow":   name,
			},
		},
		"spec": spec,
	}
	created, err := dashCreate(ns, "workflowruns", obj)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeRaw(w, created)
}

// studioRunName generates a k8s-safe WorkflowRun name. Truncates the workflow
// name so the suffix fits within the 253-char DNS limit and uses the current
// time to keep names unique within a single workflow.
func studioRunName(workflow string) string {
	const suffix = "-studio-"
	wf := strings.ToLower(workflow)
	if len(wf) > 200 {
		wf = wf[:200]
	}
	return fmt.Sprintf("%s%s%d", wf, suffix, time.Now().UnixNano())
}

// handleStudioInvokeTool directly executes a Tool with the caller-supplied
// input, bypassing the ToolGrantEngine (studio is a trusted, interactive
// context). Only network protocols (OpenAPI, MCP) can be tested —
// browser/shell tools require an in-process runtime host that the
// controller does not carry, so we reject them with a friendly explanation.
//
// This deliberately does not touch the ToolGrant system: studio users own
// the cluster or namespace they are pointing at and grants are an
// agent-scoped policy, not a suitability check for a human tester.
func handleStudioInvokeTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	if ns == "" {
		ns = "default"
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	var body struct {
		Input          map[string]any `json:"input"`
		TimeoutSeconds int            `json:"timeoutSeconds,omitempty"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("parse body: %v", err)})
			return
		}
	}
	if body.Input == nil {
		body.Input = map[string]any{}
	}

	toolRaw, err := dashGet(ns, "tools", name)
	if err != nil {
		writeErr(w, err)
		return
	}
	tool, err := decodeTool(toolRaw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("decode tool: %v", err)})
		return
	}

	protocol := string(tool.Spec.EffectiveProtocol())
	switch tool.Spec.EffectiveProtocol() {
	case v1alpha1.ToolProtocolOpenAPI, v1alpha1.ToolProtocolMCP:
		// supported
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":    fmt.Sprintf("studio can't test %q tools — only OpenAPI and MCP protocols have a controller-side executor. Wire this tool up through an Agent and test it there.", protocol),
			"protocol": protocol,
		})
		return
	}

	// A per-request executor is fine — it wraps a shared http.Client under
	// the hood and carries no per-invocation state. We hand it the
	// cluster-aware client so requests to *.svc.cluster.local get transparently
	// routed through the k8s apiserver proxy when studio is run on a workstation.
	exec := toolexec.New(clusterAwareHTTPClient())
	dispatcher := runtime.NewToolDispatcher(exec, exec)

	// Studio callers should get a bounded wait even if the tool's sandbox
	// timeout is huge. Default to 30s, honor an explicit request override up
	// to 5 minutes.
	timeout := 30 * time.Second
	if body.TimeoutSeconds > 0 {
		d := time.Duration(body.TimeoutSeconds) * time.Second
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	res, err := dispatcher.Execute(ctx, runtime.ToolExecutionRequest{
		Tool:  tool,
		Input: body.Input,
	})
	elapsed := time.Since(start)
	if err != nil {
		out := map[string]any{
			"error":      err.Error(),
			"protocol":   protocol,
			"durationMs": elapsed.Milliseconds(),
		}
		var structured *runtime.StructuredToolError
		if errors.As(err, &structured) && structured != nil {
			out["code"] = structured.Code
			out["retryable"] = structured.Retryable
		}
		writeJSON(w, http.StatusBadGateway, out)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"output":        res.Output,
		"executionMode": res.ExecutionMode,
		"durationMs":    res.Duration.Milliseconds(),
		"protocol":      protocol,
	})
}

// handleStudioToolSchema introspects a Tool's endpoint for its live inputSchema.
// For MCP tools we send `tools/list` and pick the entry whose `name` matches
// (falling back to the sole entry). For OpenAPI tools we GET the endpoint and
// look for `inputSchema` in the JSON body. When live introspection fails we
// fall back to whatever the CR declares in spec.schema / spec.inputSchema so
// callers always get something to render.
//
// Response shape:
//
//	{
//	  protocol:    "MCP" | "OpenAPI",
//	  inputSchema: <json schema>,     // may be null
//	  description: string,
//	  source:      "live" | "declared" | "none",
//	  warning:     string,            // present when live probe failed
//	  raw:         <object>,          // matched MCP tools/list entry, or the OpenAPI document returned by the endpoint
//	}
func handleStudioToolSchema(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ns := dashboardNamespace(r)
	if ns == "" {
		ns = "default"
	}

	toolRaw, err := dashGet(ns, "tools", name)
	if err != nil {
		writeErr(w, err)
		return
	}
	tool, err := decodeTool(toolRaw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": fmt.Sprintf("decode tool: %v", err)})
		return
	}

	protocol := string(tool.Spec.EffectiveProtocol())
	out := map[string]any{
		"protocol":    protocol,
		"source":      "none",
		"description": tool.Spec.Description,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	var (
		liveSchema map[string]any
		liveDesc   string
		liveRaw    any
		liveErr    error
	)
	switch tool.Spec.EffectiveProtocol() {
	case v1alpha1.ToolProtocolMCP:
		liveSchema, liveDesc, liveRaw, liveErr = introspectMCPSchema(ctx, tool)
	case v1alpha1.ToolProtocolOpenAPI:
		liveSchema, liveDesc, liveRaw, liveErr = introspectOpenAPISchema(ctx, tool)
	default:
		// Browser/shell tools have no wire protocol we can hit.
		liveErr = fmt.Errorf("introspection not supported for %q tools", protocol)
	}

	if liveErr == nil && liveSchema != nil {
		out["inputSchema"] = liveSchema
		out["source"] = "live"
		if liveDesc != "" {
			out["description"] = liveDesc
		}
		if liveRaw != nil {
			out["raw"] = liveRaw
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	// Fall back to declared schema — spec.inputSchema wins for MCP tools,
	// spec.schema is OpenAPI's source of truth (may contain a full OpenAPI
	// document, in which case we surface it whole and let the UI figure out
	// how much of it is a JSON Schema for the request body).
	declared := tool.Spec.InputSchema
	if declared == nil {
		declared = tool.Spec.Schema
	}
	if declared != nil {
		out["inputSchema"] = declared
		out["source"] = "declared"
	}
	if liveErr != nil {
		out["warning"] = liveErr.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

// introspectMCPSchema sends a JSON-RPC tools/list and picks the entry that
// matches the Tool's name. Returns the entry's inputSchema, description, and
// the full raw JSON-RPC result (for the builder to display verbatim).
func introspectMCPSchema(ctx context.Context, tool v1alpha1.Tool) (schema map[string]any, description string, raw any, err error) {
	if tool.Spec.Endpoint == "" {
		return nil, "", nil, errors.New("tool endpoint is empty")
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tool.Spec.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := clusterAwareHTTPClient().Do(req)
	if err != nil {
		return nil, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, "", nil, fmt.Errorf("MCP server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var parsed struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, "", nil, fmt.Errorf("decode tools/list: %w", err)
	}
	if parsed.Error != nil {
		return nil, "", nil, fmt.Errorf("MCP error %d: %s", parsed.Error.Code, parsed.Error.Message)
	}
	if len(parsed.Result.Tools) == 0 {
		return nil, "", nil, errors.New("MCP server returned no tools")
	}
	// Prefer the entry whose name matches the CR; fall back to first.
	match := parsed.Result.Tools[0]
	for _, t := range parsed.Result.Tools {
		if t.Name == tool.Name {
			match = t
			break
		}
	}
	// Return the matched tool entry (name/description/inputSchema) as raw so
	// the builder can show the exact metadata the MCP server advertises.
	matchAny := map[string]any{
		"name":        match.Name,
		"description": match.Description,
		"inputSchema": match.InputSchema,
	}
	return match.InputSchema, match.Description, matchAny, nil
}

// introspectOpenAPISchema GETs the OpenAPI tool endpoint and expects one of:
//   - an OpenAPI 3.x document (preferred) — request-body schema is pulled
//     from paths[<endpoint path>].post.requestBody.content.application/json.schema;
//   - a legacy {inputSchema, description} object (our earlier sandbox shape).
//
// Raw payload is always returned so the builder can display the full spec
// verbatim.
func introspectOpenAPISchema(ctx context.Context, tool v1alpha1.Tool) (schema map[string]any, description string, raw any, err error) {
	if tool.Spec.Endpoint == "" {
		return nil, "", nil, errors.New("tool endpoint is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tool.Spec.Endpoint, nil)
	if err != nil {
		return nil, "", nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := clusterAwareHTTPClient().Do(req)
	if err != nil {
		return nil, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, "", nil, fmt.Errorf("OpenAPI GET returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var parsed map[string]any
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, "", nil, fmt.Errorf("decode schema response: %w", err)
	}
	// OpenAPI 3.x document.
	if _, ok := parsed["openapi"]; ok {
		s, d := extractOpenAPIRequestSchema(parsed, tool.Spec.Endpoint)
		if s == nil {
			return nil, "", parsed, errors.New("OpenAPI document has no matching operation with a JSON requestBody schema")
		}
		return s, d, parsed, nil
	}
	// Legacy {inputSchema, description} shape.
	if is, ok := parsed["inputSchema"].(map[string]any); ok {
		desc, _ := parsed["description"].(string)
		return is, desc, parsed, nil
	}
	return nil, "", parsed, errors.New("response has no OpenAPI paths or inputSchema field")
}

// extractOpenAPIRequestSchema pulls the request-body JSON schema out of an
// OpenAPI 3.x document for the operation matching the tool's endpoint path.
// Prefers POST but falls back to PUT/GET so tools that use other verbs still
// get a schema. When the exact path isn't found (server-relative vs absolute
// mismatch), it falls back to the first path in the document.
func extractOpenAPIRequestSchema(doc map[string]any, endpointURL string) (map[string]any, string) {
	paths, _ := doc["paths"].(map[string]any)
	if len(paths) == 0 {
		return nil, ""
	}
	var pathItem map[string]any
	if u, err := url.Parse(endpointURL); err == nil && u.Path != "" {
		if pi, ok := paths[u.Path].(map[string]any); ok {
			pathItem = pi
		}
	}
	if pathItem == nil {
		// Fallback: first path entry — sandbox and small OpenAPIs usually
		// have one operation, so this is good enough.
		for _, v := range paths {
			if m, ok := v.(map[string]any); ok {
				pathItem = m
				break
			}
		}
	}
	if pathItem == nil {
		return nil, ""
	}
	var op map[string]any
	for _, verb := range []string{"post", "put", "patch", "get", "delete"} {
		if m, ok := pathItem[verb].(map[string]any); ok {
			op = m
			break
		}
	}
	if op == nil {
		return nil, ""
	}
	desc, _ := op["summary"].(string)
	if d2, ok := op["description"].(string); ok && d2 != "" {
		desc = d2
	}
	rb, _ := op["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	ct, _ := content["application/json"].(map[string]any)
	schema, _ := ct["schema"].(map[string]any)
	return schema, desc
}
