// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	jsonpatch "gomodules.xyz/jsonpatch/v2"

	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

const internalSecretDataAnnotation = "kognition.dev/internal-secret-data"

// pluralToKind maps plural resource paths to singular Kind names.
var pluralToKind = map[string]string{
	"agents":                      "Agent",
	"agentinstances":              "AgentInstance",
	"agentjobs":                   "AgentJob",
	"workflows":                   "Workflow",
	"workflowruns":                "WorkflowRun",
	"memories":                    "Memory",
	"memoryclasses":               "MemoryClass",
	"memoryrecords":               "MemoryRecord",
	"memoryclaims":                "MemoryClaim",
	"memoryvolumes":               "MemoryVolume",
	"knowledgebases":              "KnowledgeBase",
	"knowledgebaseclasses":        "KnowledgeBaseClass",
	"knowledgebaseclaims":         "KnowledgeBaseClaim",
	"knowledgebaseindexruns":      "KnowledgeBaseIndexRun",
	"knowledgebaseindexschedules": "KnowledgeBaseIndexSchedule",
	"toolclasses":                 "ToolClass",
	"tools":                       "Tool",
	"toolgrants":                  "ToolGrant",
	"toolinvocationruns":          "ToolInvocationRun",
	"modelclasses":                "ModelClass",
	"models":                      "Model",
	"modelgrants":                 "ModelGrant",
	"modelinvocationprofiles":     "ModelInvocationProfile",
	"agentpolicies":               "AgentPolicy",
	"cognitiveadmissionpolicies":  "CognitiveAdmissionPolicy",
	"cognitivequotas":             "CognitiveQuota",
	"cognitivelimitranges":        "CognitiveLimitRange",
	"workspaces":                  "Workspace",
	"agentidentities":             "AgentIdentity",
	"agentroles":                  "AgentRole",
	"agentrolebindings":           "AgentRoleBinding",
	"agentcommunicationpolicies":  "AgentCommunicationPolicy",
	"memorygrants":                "MemoryGrant",
	"autonomyclasses":             "AutonomyClass",
	"trustprofiles":               "TrustProfile",
	"autonomyreviews":             "AutonomyReview",
	"agentservices":               "AgentService",
	"intentgateways":              "IntentGateway",
	"prompts":                     "Prompt",
	"secretrefs":                  "SecretRef",
	"solutions":                   "Solution",
	"evaluations":                 "Evaluation",
	"evaluationdatasets":          "EvaluationDataset",
	"evaluationruns":              "EvaluationRun",
	"crossnamespacegrants":        "CrossNamespaceGrant",
	"agentautoscalers":            "AgentAutoscaler",
	"modelautoscalers":            "ModelAutoscaler",
	"approvaldecisions":           "ApprovalDecision",
}

// resolveKind maps the URL plural resource name to the internal kind.
func resolveKind(resource string) string {
	if k, ok := pluralToKind[strings.ToLower(resource)]; ok {
		return k
	}
	return ""
}

func resolveRequestKind(w http.ResponseWriter, resource string) (string, bool) {
	kind := resolveKind(resource)
	if kind == "" {
		writeError(w, http.StatusNotFound, fmt.Sprintf("resource %q not found", resource))
		return "", false
	}
	return kind, true
}

func canonicalNamespace(kind, namespace string) string {
	switch kind {
	case "AutonomyClass", "KnowledgeBaseClass", "MemoryClass", "ModelClass", "ToolClass":
		return ""
	default:
		return namespace
	}
}

func (s *Server) recordResourceAudit(ctx context.Context, kind, namespace, name, verb string) error {
	if s.auditLog == nil {
		return nil
	}
	actor := "anonymous"
	if identity, ok := auth.IdentityFromContext(ctx); ok && identity != nil && identity.Method != "anonymous" {
		actor = strings.TrimSpace(identity.Subject)
		if actor == "" {
			actor = strings.TrimSpace(identity.Username)
		}
		if actor == "" {
			actor = "unknown"
		}
	}
	eventType := observability.AuditSpecChange
	if strings.HasSuffix(kind, "Grant") {
		eventType = observability.AuditGrantChange
	}
	workspace := namespace
	if workspace == "" {
		workspace = "cluster"
	}
	return s.auditLog.RecordBeforeExec(ctx, observability.AuditEntry{
		ID:            uuid.NewString(),
		Workspace:     workspace,
		AgentIdentity: actor,
		EventType:     eventType,
		SubjectKind:   kind,
		SubjectRef:    namespace + "/" + name,
		Verb:          verb,
	})
}

func (s *Server) handleQueryTrustDecisions(w http.ResponseWriter, r *http.Request) {
	query, err := observability.ParseTrustDecisionQuery(r.PathValue("namespace"), r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.auditLog == nil {
		writeError(w, http.StatusServiceUnavailable, "trust decision audit store is unavailable")
		return
	}
	result, err := observability.QueryTrustDecisions(r.Context(), s.auditLog, query, time.Now())
	if err != nil {
		if errors.Is(err, observability.ErrInvalidTrustDecisionQuery) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleList lists resources in a namespace.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	// Parse pagination parameters.
	limit := 0
	if q := r.URL.Query().Get("limit"); q != "" {
		var err error
		limit, err = strconv.Atoi(q)
		if err != nil || limit < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit parameter")
			return
		}
	}
	continueToken := r.URL.Query().Get("continue")
	currentOffset := 0
	if continueToken != "" {
		if limit == 0 {
			writeError(w, http.StatusBadRequest, "continue parameter requires a positive limit")
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(continueToken)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid continue parameter")
			return
		}
		currentOffset, err = strconv.Atoi(string(raw))
		if err != nil || currentOffset < 0 {
			writeError(w, http.StatusBadRequest, "invalid continue parameter")
			return
		}
	}

	items, err := s.store.List(r.Context(), kind, store.ListOptions{
		Namespace: namespace,
		Limit:     limit,
		Continue:  continueToken,
	})
	if err != nil {
		s.logger.Error("list failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Detect if there are more pages.
	var nextContinue string
	if limit > 0 && len(items) > limit {
		items = items[:limit]
		nextContinue = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(currentOffset + limit)))
	}

	// Build JSON array from raw items.
	jsonItems := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		jsonItems = append(jsonItems, json.RawMessage(sanitizeResourceForAPI(kind, item)))
	}

	metadata := map[string]any{}
	if nextContinue != "" {
		metadata["continue"] = nextContinue
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       kind + "List",
		"metadata":   metadata,
		"items":      jsonItems,
	})
}

// handleGet retrieves a single resource.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	data, err := s.store.Get(r.Context(), kind, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found in namespace %q", kind, name, namespace))
			return
		}
		s.logger.Error("get failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sanitizeResourceForAPI(kind, data))
}

// handleCreate creates a new resource.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	// Parse as generic JSON to inject metadata.
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Ensure metadata block exists.
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
	}

	// Extract name from metadata or body.
	name, _ := meta["name"].(string)
	if name == "" {
		writeError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	// Inject server-assigned fields.
	meta["namespace"] = namespace
	meta["uid"] = uuid.New().String()
	meta["resourceVersion"] = int64(1)
	meta["creationTimestamp"] = time.Now().UTC().Format(time.RFC3339)
	obj["metadata"] = meta

	// Normalize apiVersion and kind.
	obj["apiVersion"] = "kognition.dev/v1alpha1"
	obj["kind"] = kind
	if err := rejectGenericAutonomyReviewStatusWrite(kind, obj, nil); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	data, err := json.Marshal(obj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to serialize resource")
		return
	}

	// Run admission pipeline (mutating → validating).
	admitted, err := s.admission.Admit(r.Context(), &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
		Object:    data,
		UserInfo:  admissionUserInfo(r.Context()),
	})
	if err != nil {
		var denied *admission.DeniedError
		if errors.As(err, &denied) {
			writeError(w, http.StatusUnprocessableEntity, denied.Error())
			return
		}
		s.logger.Error("admission failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "admission error")
		return
	}
	data = admitted
	if err := s.recordResourceAudit(r.Context(), kind, namespace, name, "create"); err != nil {
		s.logger.Error("audit create failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "audit durability failed")
		return
	}

	var createErr error
	if resultStore, ok := s.store.(store.CreateResultStore); ok {
		data, createErr = resultStore.CreateAndGet(r.Context(), kind, namespace, name, data)
	} else {
		createErr = s.store.Create(r.Context(), kind, namespace, name, data)
	}
	if createErr != nil {
		if errors.Is(createErr, store.ErrAlreadyExists) {
			writeError(w, http.StatusConflict, fmt.Sprintf("%s %q already exists in namespace %q", kind, name, namespace))
			return
		}
		s.logger.Error("create failed", zap.Error(createErr), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(sanitizeResourceForAPI(kind, data))
}

// handleUpdate updates an existing resource.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Extract resourceVersion for optimistic concurrency.
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		writeError(w, http.StatusBadRequest, "metadata is required")
		return
	}

	rvRaw, ok := meta["resourceVersion"]
	if !ok {
		writeError(w, http.StatusBadRequest, "metadata.resourceVersion is required for updates")
		return
	}
	rv, ok := toInt64(rvRaw)
	if !ok || rv < 1 {
		writeError(w, http.StatusBadRequest, "metadata.resourceVersion must be a positive integer")
		return
	}

	// Set the new resourceVersion (will be rv+1 in the DB).
	meta["namespace"] = namespace
	meta["name"] = name
	obj["metadata"] = meta
	obj["apiVersion"] = "kognition.dev/v1alpha1"
	obj["kind"] = kind

	data, err := json.Marshal(obj)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to serialize resource")
		return
	}

	// Fetch the old object for admission context.
	oldData, oldErr := s.store.Get(r.Context(), kind, namespace, name)
	if oldErr != nil && !errors.Is(oldErr, store.ErrNotFound) {
		s.logger.Error("failed to fetch old object for admission", zap.Error(oldErr), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := rejectGenericAutonomyReviewStatusWrite(kind, obj, oldData); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Run admission pipeline (mutating → validating).
	admitted, err := s.admission.Admit(r.Context(), &admission.Request{
		Operation: admission.OperationUpdate,
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
		Object:    data,
		OldObject: oldData,
		UserInfo:  admissionUserInfo(r.Context()),
	})
	if err != nil {
		var denied *admission.DeniedError
		if errors.As(err, &denied) {
			writeError(w, http.StatusUnprocessableEntity, denied.Error())
			return
		}
		s.logger.Error("admission failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "admission error")
		return
	}
	data = admitted
	if err := json.Unmarshal(data, &obj); err != nil {
		s.logger.Error("failed to decode admitted update", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "admission error")
		return
	}
	meta, _ = obj["metadata"].(map[string]any)
	if err := s.recordResourceAudit(r.Context(), kind, namespace, name, "update"); err != nil {
		s.logger.Error("audit update failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "audit durability failed")
		return
	}

	freshData, err := s.store.UpdateAndGet(r.Context(), kind, namespace, name, data, rv)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(w, http.StatusConflict, "resource version conflict — fetch the latest version and retry")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found in namespace %q", kind, name, namespace))
			return
		}
		s.logger.Error("update failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// If the resource has deletionTimestamp and no remaining finalizers, auto-delete.
	if _, hasTS := meta["deletionTimestamp"]; hasTS {
		finalizers := extractFinalizers(meta)
		if len(finalizers) == 0 {
			if err := s.store.Delete(r.Context(), kind, namespace, name); err != nil {
				s.logger.Error("auto-delete after finalizer removal failed", zap.Error(err), requestid.ZapField(r.Context()))
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sanitizeResourceForAPI(kind, freshData))
}

func rejectGenericAutonomyReviewStatusWrite(kind string, object map[string]any, oldData []byte) error {
	if kind != "AutonomyReview" {
		return nil
	}
	status, hasStatus := object["status"]
	if len(oldData) == 0 {
		if hasStatus {
			if statusMap, ok := status.(map[string]any); !ok || len(statusMap) > 0 {
				return fmt.Errorf("AutonomyReview status is controller-owned; use the decision subresource")
			}
		}
		return nil
	}
	var oldObject map[string]any
	if err := json.Unmarshal(oldData, &oldObject); err != nil {
		return fmt.Errorf("decode current AutonomyReview status: %w", err)
	}
	oldSpec, _ := json.Marshal(oldObject["spec"])
	newSpec, _ := json.Marshal(object["spec"])
	if string(oldSpec) != string(newSpec) {
		return fmt.Errorf("AutonomyReview spec is immutable after creation")
	}
	oldStatus, oldHasStatus := oldObject["status"]
	oldJSON, _ := json.Marshal(oldStatus)
	newJSON, _ := json.Marshal(status)
	if oldHasStatus != hasStatus || string(oldJSON) != string(newJSON) {
		return fmt.Errorf("AutonomyReview status is controller-owned; use the decision subresource")
	}
	return nil
}

func admissionUserInfo(ctx context.Context) admission.UserInfo {
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok || identity == nil || identity.Method == "anonymous" {
		return admission.UserInfo{}
	}
	return admission.UserInfo{
		Username: identity.Username,
		Groups:   append([]string(nil), identity.Groups...),
	}
}

// handleDelete deletes a resource. If the resource has finalizers, it sets
// deletionTimestamp instead of deleting, allowing controllers to clean up.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	// Fetch the current resource to check for finalizers.
	data, err := s.store.Get(r.Context(), kind, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found in namespace %q", kind, name, namespace))
			return
		}
		s.logger.Error("get for delete failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := s.admission.Admit(r.Context(), &admission.Request{
		Operation: admission.OperationDelete,
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
		Object:    data,
		OldObject: data,
		UserInfo:  admissionUserInfo(r.Context()),
	}); err != nil {
		var denied *admission.DeniedError
		if errors.As(err, &denied) {
			writeError(w, http.StatusUnprocessableEntity, denied.Error())
			return
		}
		s.logger.Error("delete admission failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "admission error")
		return
	}

	// Parse metadata to check finalizers and deletionTimestamp.
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		s.logger.Error("failed to parse resource for delete", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
	}

	// If already deleting, return the resource as-is.
	if _, hasTS := meta["deletionTimestamp"]; hasTS {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(sanitizeResourceForAPI(kind, data))
		return
	}

	// Check for finalizers.
	finalizers := extractFinalizers(meta)
	if len(finalizers) > 0 {
		// Set deletionTimestamp and update the resource instead of deleting.
		// Retry on conflict since reconcilers may be updating concurrently.
		const maxDeleteRetries = 10
		for attempt := 0; attempt < maxDeleteRetries; attempt++ {
			meta["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
			obj["metadata"] = meta

			rv, _ := toInt64(meta["resourceVersion"])

			updated, err := json.Marshal(obj)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to serialize resource")
				return
			}

			persisted, err := s.store.UpdateAndGet(r.Context(), kind, namespace, name, updated, rv)
			if err != nil {
				if errors.Is(err, store.ErrConflict) {
					// Re-fetch and retry.
					data, err = s.store.Get(r.Context(), kind, namespace, name)
					if err != nil {
						writeError(w, http.StatusInternalServerError, "internal error")
						return
					}
					obj = nil
					if err := json.Unmarshal(data, &obj); err != nil {
						writeError(w, http.StatusInternalServerError, "internal error")
						return
					}
					meta, _ = obj["metadata"].(map[string]any)
					if meta == nil {
						meta = make(map[string]any)
					}
					// If someone else set deletionTimestamp, we're done.
					if _, hasTS := meta["deletionTimestamp"]; hasTS {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write(sanitizeResourceForAPI(kind, data))
						return
					}
					continue
				}
				s.logger.Error("update for delete failed", zap.Error(err), requestid.ZapField(r.Context()))
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(sanitizeResourceForAPI(kind, persisted))
			return
		}
		writeError(w, http.StatusConflict, "resource version conflict — fetch the latest version and retry")
		return
	}

	// No finalizers — proceed with immediate deletion.
	if err := s.recordResourceAudit(r.Context(), kind, namespace, name, "delete"); err != nil {
		s.logger.Error("audit delete failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "audit durability failed")
		return
	}
	if err := s.store.Delete(r.Context(), kind, namespace, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found in namespace %q", kind, name, namespace))
			return
		}
		s.logger.Error("delete failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// extractFinalizers extracts the finalizers slice from metadata.
func extractFinalizers(meta map[string]any) []string {
	raw, ok := meta["finalizers"]
	if !ok {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// handleWatch streams resource change events via SSE.
//
// Cross-replica delivery is provided by the store's Watch implementation, not by
// this handler. The PostgreSQL store fans events out to every API server and
// controller replica via LISTEN/NOTIFY once StartNotifyListener is running
// (wired in cmd/kognition-apiserver); the in-memory Go channels are only the
// final per-process delivery hop. A store without an active NOTIFY listener
// (e.g. a single-process test setup) delivers to local subscribers only.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	ch, err := s.store.Watch(r.Context(), kind, store.ListOptions{Namespace: namespace})
	if err != nil {
		s.logger.Error("watch setup failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "watch setup failed")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if evt.Type == store.EventResyncRequired {
				_, _ = fmt.Fprint(w, "data: {\"type\":\"ERROR\",\"error\":\"watch history lost; relist required\"}\n\n")
				flusher.Flush()
				return
			}
			// Filter by namespace if specified.
			if namespace != "" && evt.Namespace != namespace {
				continue
			}
			data, err := json.Marshal(map[string]any{
				"type":   evt.Type,
				"object": json.RawMessage(sanitizeResourceForAPI(kind, evt.Object)),
			})
			if err != nil {
				s.logger.Error("watch event marshal failed", zap.Error(err), requestid.ZapField(r.Context()))
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleListRevisions returns all revisions for a named resource.
func (s *Server) handleListRevisions(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)
	if !s.requireRevisionParent(w, r, kind, namespace, name) {
		return
	}

	revisions, err := s.store.ListRevisions(r.Context(), kind, namespace, name)
	if err != nil {
		s.logger.Error("list revisions failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if revisions == nil {
		revisions = []store.Revision{}
	}
	for i := range revisions {
		revisions[i].Data = sanitizeResourceForAPI(kind, revisions[i].Data)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "RevisionList",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
		},
		"items": revisions,
	})
}

// handleGetRevision returns a specific revision of a named resource.
func (s *Server) handleGetRevision(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	revStr := r.PathValue("revision")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	rev, err := strconv.ParseInt(revStr, 10, 64)
	if err != nil || rev < 1 {
		writeError(w, http.StatusBadRequest, "revision must be a positive integer")
		return
	}
	if !s.requireRevisionParent(w, r, kind, namespace, name) {
		return
	}

	revision, err := s.store.GetRevision(r.Context(), kind, namespace, name, rev)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("revision %d not found for %s %q", rev, kind, name))
			return
		}
		s.logger.Error("get revision failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Revision",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
			"revision":  revision.Revision,
			"createdAt": revision.CreatedAt,
		},
		"spec": json.RawMessage(revision.Spec),
		"data": json.RawMessage(sanitizeResourceForAPI(kind, revision.Data)),
	})
}

// handleDiffRevisions returns an RFC 6902 patch between two immutable
// revision specs. It intentionally excludes metadata and status so callers
// can inspect only the declared configuration change.
func (s *Server) handleDiffRevisions(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)
	if !s.requireRevisionParent(w, r, kind, namespace, name) {
		return
	}

	from, to, err := revisionDiffQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	fromRevision, err := s.store.GetRevision(r.Context(), kind, namespace, name, from)
	if err != nil {
		s.writeRevisionLookupError(w, r, err, from, kind, name)
		return
	}
	toRevision, err := s.store.GetRevision(r.Context(), kind, namespace, name, to)
	if err != nil {
		s.writeRevisionLookupError(w, r, err, to, kind, name)
		return
	}

	patch, err := jsonpatch.CreatePatch(fromRevision.Spec, toRevision.Spec)
	if err != nil {
		s.logger.Error("create revision diff failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "revision snapshots contain invalid JSON")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "RevisionDiff",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
			"resource":  resource,
		},
		"fromRevision": from,
		"toRevision":   to,
		"patch":        patch,
	})
}

func (s *Server) requireRevisionParent(w http.ResponseWriter, r *http.Request, kind, namespace, name string) bool {
	if _, err := s.store.Get(r.Context(), kind, namespace, name); err == nil {
		return true
	} else if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s %q not found in namespace %q", kind, name, namespace))
		return false
	} else {
		s.logger.Error("get revision parent failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return false
	}
}

func revisionDiffQuery(r *http.Request) (int64, int64, error) {
	from, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if err != nil || from < 1 {
		return 0, 0, errors.New("from must be a positive revision number")
	}
	to, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
	if err != nil || to < 1 {
		return 0, 0, errors.New("to must be a positive revision number")
	}
	if from == to {
		return 0, 0, errors.New("from and to must name different revisions")
	}
	return from, to, nil
}

func (s *Server) writeRevisionLookupError(w http.ResponseWriter, r *http.Request, err error, revision int64, kind, name string) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("revision %d not found for %s %q", revision, kind, name))
		return
	}
	s.logger.Error("get revision failed", zap.Error(err), requestid.ZapField(r.Context()))
	writeError(w, http.StatusInternalServerError, "internal error")
}

func sanitizeResourceForAPI(kind string, data []byte) []byte {
	if kind != "SecretRef" || len(data) == 0 {
		return data
	}

	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return data
	}

	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return data
	}

	annotations, _ := meta["annotations"].(map[string]any)
	if annotations == nil {
		return data
	}

	if _, ok := annotations[internalSecretDataAnnotation]; !ok {
		return data
	}

	delete(annotations, internalSecretDataAnnotation)
	if len(annotations) == 0 {
		delete(meta, "annotations")
	} else {
		meta["annotations"] = annotations
	}
	obj["metadata"] = meta

	sanitized, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return sanitized
}

// handleListEvents returns all execution events for a named resource instance.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	resource := r.PathValue("resource")
	name := r.PathValue("name")
	kind, ok := resolveRequestKind(w, resource)
	if !ok {
		return
	}
	namespace = canonicalNamespace(kind, namespace)

	events, err := s.store.ListEvents(r.Context(), kind, namespace, name)
	if err != nil {
		s.logger.Error("list events failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if events == nil {
		events = []store.InstanceEvent{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "InstanceEventList",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
		},
		"items": events,
	})
}

// handleMemoryRecordRTBF invokes right-to-be-forgotten cascade for a source
// MemoryRecord and returns the list of tombstoned records.
func (s *Server) handleMemoryRecordRTBF(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	name := r.PathValue("name")

	tombstoned, err := controller.RightToBeForgotten(r.Context(), s.store, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("MemoryRecord %q not found in namespace %q", name, namespace))
			return
		}
		s.logger.Error("rtbf failed", zap.Error(err), zap.String("namespace", namespace), zap.String("name", name), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "rtbf operation failed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "RightToBeForgottenResult",
		"metadata": map[string]any{
			"namespace": namespace,
			"name":      name,
		},
		"tombstonedRecords": tombstoned,
	})
}

func (s *Server) handleSubmitApprovalDecision(w http.ResponseWriter, r *http.Request) {
	if s.approvalBroker == nil {
		writeError(w, http.StatusNotImplemented, "approval API is not enabled")
		return
	}

	approvalID := strings.TrimSpace(r.PathValue("approvalID"))
	if approvalID == "" {
		writeError(w, http.StatusBadRequest, "approvalID is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	var decision runtime.ApprovalDecision
	if err := json.Unmarshal(body, &decision); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := s.approvalBroker.SubmitDecision(approvalID, decision); err != nil {
		switch {
		case errors.Is(err, runtime.ErrApprovalNotFound):
			writeError(w, http.StatusNotFound, err.Error())
			return
		case errors.Is(err, runtime.ErrApprovalClosed):
			writeError(w, http.StatusConflict, err.Error())
			return
		default:
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"approvalId": approvalID,
		"state":      decision.State,
		"message":    "decision accepted",
	})
}

// workflowApprovedStepsAnnotation mirrors the controller-side annotation key
// (kognition.dev/approved-steps) used by the workflow reconciler to detect
// step approvals via the annotation-based bridge.
const (
	workflowApprovedStepsAnnotation = "kognition.dev/approved-steps"
	workflowRejectedStepsAnnotation = "kognition.dev/rejected-steps"
	// workflowApproversAnnotationPrefix tracks the set of distinct approvers
	// for a given step, used to enforce the gate's requiredApprovers quorum.
	// Full key: kognition.dev/approvers.<step>
	workflowApproversAnnotationPrefix = "kognition.dev/approvers."
)

// workflowRunApprovalRequest is the payload for the /approval subresource.
type workflowRunApprovalRequest struct {
	Step     string `json:"step"`
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
	Approver string `json:"approver,omitempty"`
}

// handleWorkflowRunApproval records an approve/reject decision for a specific
// step of a WorkflowRun. The decision is persisted as a CSV annotation on the
// WorkflowRun so the workflow reconciler's annotation-based gate logic
// (T-0310 backward-compat bridge) continues to function unchanged.
func (s *Server) handleWorkflowRunApproval(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(r.PathValue("namespace"))
	name := strings.TrimSpace(r.PathValue("name"))
	if namespace == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var req workflowRunApprovalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Step = strings.TrimSpace(req.Step)
	req.Decision = strings.ToLower(strings.TrimSpace(req.Decision))
	if req.Step == "" {
		writeError(w, http.StatusBadRequest, "step is required")
		return
	}
	approved := false
	switch req.Decision {
	case "approve", "approved":
		approved = true
	case "reject", "rejected":
		approved = false
	default:
		writeError(w, http.StatusBadRequest, "decision must be 'approve' or 'reject'")
		return
	}
	approverID := strings.TrimSpace(req.Approver)
	// Prefer a verified identity when the request is authenticated so that
	// quorum and audit records cannot be attributed to a caller-asserted claim.
	if id, ok := auth.IdentityFromContext(r.Context()); ok && id.Method != "anonymous" {
		approverID = id.Username
	}

	const maxRetries = 10
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := s.store.Get(r.Context(), "WorkflowRun", namespace, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, fmt.Sprintf("WorkflowRun %q not found in namespace %q", name, namespace))
				return
			}
			s.logger.Error("get workflowrun for approval failed", zap.Error(err), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to parse WorkflowRun")
			return
		}
		meta, _ := obj["metadata"].(map[string]any)
		if meta == nil {
			meta = map[string]any{}
			obj["metadata"] = meta
		}
		annotations, _ := meta["annotations"].(map[string]any)
		if annotations == nil {
			annotations = map[string]any{}
			meta["annotations"] = annotations
		}

		key := workflowApprovedStepsAnnotation
		if !approved {
			key = workflowRejectedStepsAnnotation
			existing, _ := annotations[key].(string)
			annotations[key] = appendCSVToken(existing, req.Step)
		} else {
			required, lookupErr := s.lookupRequiredApprovers(r.Context(), obj, req.Step)
			if lookupErr != nil {
				s.logger.Error("lookup required approvers failed", zap.String("namespace", namespace), zap.String("workflowrun", name), requestid.ZapField(r.Context()))
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			if required > 1 && approverID == "" {
				writeError(w, http.StatusBadRequest, "approver is required when requiredApprovers > 1")
				return
			}
			approversKey := workflowApproversAnnotationPrefix + req.Step
			approversExisting, _ := annotations[approversKey].(string)
			if approverID != "" {
				approversExisting = appendCSVToken(approversExisting, approverID)
				annotations[approversKey] = approversExisting
			}
			approverCount := countCSVTokens(approversExisting)
			// Anonymous approvals count as a single vote only when no
			// distinct approvers have been recorded, preserving the legacy
			// single-approval flow for requiredApprovers <= 1.
			if approverID == "" && approverCount == 0 {
				approverCount = 1
			}
			if required <= 1 || approverCount >= int(required) {
				existing, _ := annotations[key].(string)
				annotations[key] = appendCSVToken(existing, req.Step)
			}
		}

		// T-0309: append an audit record to status.approvalRecords.
		status, _ := obj["status"].(map[string]any)
		if status == nil {
			status = map[string]any{}
			obj["status"] = status
		}
		records, _ := status["approvalRecords"].([]any)
		records = append(records, map[string]any{
			"step":      req.Step,
			"decision":  req.Decision,
			"approver":  approverID,
			"comment":   req.Comment,
			"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		})
		status["approvalRecords"] = records

		rv, _ := toInt64(meta["resourceVersion"])
		updated, err := json.Marshal(obj)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to serialize WorkflowRun")
			return
		}
		if err := s.store.Update(r.Context(), "WorkflowRun", namespace, name, updated, rv); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			s.logger.Error("update workflowrun for approval failed", zap.String("namespace", namespace), zap.String("workflowrun", name), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"apiVersion": "kognition.dev/v1alpha1",
			"kind":       "WorkflowRunApprovalResult",
			"metadata": map[string]any{
				"namespace": namespace,
				"name":      name,
			},
			"step":     req.Step,
			"decision": req.Decision,
			"approver": approverID,
			"comment":  req.Comment,
		})
		return
	}

	writeError(w, http.StatusConflict, "conflict updating WorkflowRun after retries")
}

// appendCSVToken adds token to a CSV string, dedup'd, preserving order.
func appendCSVToken(existing, token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return existing
	}
	if existing == "" {
		return token
	}
	for _, p := range strings.Split(existing, ",") {
		if strings.TrimSpace(p) == token {
			return existing
		}
	}
	return existing + "," + token
}

// countCSVTokens returns the number of non-empty, distinct tokens in a CSV string.
func countCSVTokens(existing string) int {
	if strings.TrimSpace(existing) == "" {
		return 0
	}
	seen := map[string]struct{}{}
	for _, p := range strings.Split(existing, ",") {
		t := strings.TrimSpace(p)
		if t == "" {
			continue
		}
		seen[t] = struct{}{}
	}
	return len(seen)
}

// lookupRequiredApprovers resolves the requiredApprovers value for the given
// step on the WorkflowRun's referenced Workflow. Returns 0 if the workflow,
// gate, or value cannot be determined (which means "single approval suffices").
func (s *Server) lookupRequiredApprovers(ctx context.Context, runObj map[string]any, step string) (int32, error) {
	spec, _ := runObj["spec"].(map[string]any)
	if spec == nil {
		return 0, nil
	}
	ref, _ := spec["workflowRef"].(map[string]any)
	if ref == nil {
		return 0, nil
	}
	wfName, _ := ref["name"].(string)
	if wfName == "" {
		return 0, nil
	}
	meta, _ := runObj["metadata"].(map[string]any)
	ns, _ := meta["namespace"].(string)
	if ns == "" {
		return 0, nil
	}
	data, err := s.store.Get(ctx, "Workflow", ns, wfName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	var wf map[string]any
	if err := json.Unmarshal(data, &wf); err != nil {
		return 0, fmt.Errorf("parse Workflow %q: %w", wfName, err)
	}
	wfSpec, _ := wf["spec"].(map[string]any)
	if wfSpec == nil {
		return 0, nil
	}
	gates, _ := wfSpec["approvalGates"].([]any)
	for _, g := range gates {
		gm, _ := g.(map[string]any)
		if gm == nil {
			continue
		}
		to, _ := gm["to"].(string)
		if to != step {
			continue
		}
		if v, ok := toInt64(gm["requiredApprovers"]); ok {
			return int32(v), nil
		}
	}
	return 0, nil
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Status",
		"status":     "Failure",
		"message":    message,
		"code":       status,
	})
}

// toInt64 attempts to convert a JSON number (which may be float64) to int64.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
