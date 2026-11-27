// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
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

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func (s *Server) handleControlPlanePolicyList(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.authorizeControlPlaneAdministrator(response, request); !ok {
		return
	}
	kind, ok := controlPlanePolicyKind(request.URL.Path)
	if !ok {
		writeError(response, http.StatusNotFound, "policy resource not found")
		return
	}
	items, err := s.store.List(request.Context(), kind, store.ListOptions{Namespace: request.PathValue("namespace")})
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	resources := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		resources = append(resources, json.RawMessage(item))
	}
	writeJSON(response, http.StatusOK, map[string]any{
		"apiVersion": api.APIVersion,
		"kind":       kind + "List",
		"items":      resources,
	})
}

func (s *Server) handleControlPlanePolicyGet(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.authorizeControlPlaneAdministrator(response, request); !ok {
		return
	}
	kind, ok := controlPlanePolicyKind(request.URL.Path)
	if !ok {
		writeError(response, http.StatusNotFound, "policy resource not found")
		return
	}
	object, err := s.store.Get(request.Context(), kind, request.PathValue("namespace"), request.PathValue("name"))
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, json.RawMessage(object))
}

func (s *Server) handleControlPlanePolicyCreate(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.authorizeControlPlaneAdministrator(response, request); !ok {
		return
	}
	kind, object, name, _, ok := decodeControlPlanePolicy(response, request, "", 0)
	if !ok {
		return
	}
	if err := validateControlPlanePolicy(request.Context(), s.store, kind, request.PathValue("namespace"), object); err != nil {
		writeError(response, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var policies store.ControlPlanePolicyStore
	if kind == "ControlPlaneRoleBinding" {
		var supported bool
		policies, supported = s.store.(store.ControlPlanePolicyStore)
		if !supported {
			writeError(response, http.StatusInternalServerError, "control-plane policy storage failed")
			return
		}
	}
	if err := s.recordResourceAudit(request.Context(), kind, request.PathValue("namespace"), name, "create"); err != nil {
		s.logger.Error("audit policy create failed", zap.Error(err), requestid.ZapField(request.Context()))
		writeError(response, http.StatusInternalServerError, "audit durability failed")
		return
	}
	var stored []byte
	var err error
	if kind == "ControlPlaneRoleBinding" {
		var binding api.ControlPlaneRoleBinding
		if unmarshalErr := json.Unmarshal(object, &binding); unmarshalErr != nil {
			writeError(response, http.StatusInternalServerError, "failed to serialize policy")
			return
		}
		stored, err = policies.CreateControlPlaneRoleBinding(request.Context(), request.PathValue("namespace"), name, binding.Spec.RoleRef.Name, object)
	} else {
		if resultStore, supported := s.store.(store.CreateResultStore); supported {
			stored, err = resultStore.CreateAndGet(request.Context(), kind, request.PathValue("namespace"), name, object)
		} else {
			err = s.store.Create(request.Context(), kind, request.PathValue("namespace"), name, object)
			stored = object
		}
	}
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusCreated, json.RawMessage(stored))
}

func (s *Server) handleControlPlanePolicyUpdate(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.authorizeControlPlaneAdministrator(response, request); !ok {
		return
	}
	kind, object, name, version, ok := decodeControlPlanePolicy(response, request, request.PathValue("name"), 1)
	if !ok {
		return
	}
	if err := validateControlPlanePolicy(request.Context(), s.store, kind, request.PathValue("namespace"), object); err != nil {
		writeError(response, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var policies store.ControlPlanePolicyStore
	if kind == "ControlPlaneRoleBinding" {
		var supported bool
		policies, supported = s.store.(store.ControlPlanePolicyStore)
		if !supported {
			writeError(response, http.StatusInternalServerError, "control-plane policy storage failed")
			return
		}
	}
	if err := s.recordResourceAudit(request.Context(), kind, request.PathValue("namespace"), name, "update"); err != nil {
		s.logger.Error("audit policy update failed", zap.Error(err), requestid.ZapField(request.Context()))
		writeError(response, http.StatusInternalServerError, "audit durability failed")
		return
	}
	var updated []byte
	var err error
	if kind == "ControlPlaneRoleBinding" {
		var binding api.ControlPlaneRoleBinding
		if unmarshalErr := json.Unmarshal(object, &binding); unmarshalErr != nil {
			writeError(response, http.StatusInternalServerError, "failed to serialize policy")
			return
		}
		updated, err = policies.UpdateControlPlaneRoleBinding(request.Context(), request.PathValue("namespace"), name, binding.Spec.RoleRef.Name, object, version)
	} else {
		updated, err = s.store.UpdateAndGet(request.Context(), kind, request.PathValue("namespace"), name, object, version)
	}
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, json.RawMessage(updated))
}

func (s *Server) handleControlPlanePolicyDelete(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.authorizeControlPlaneAdministrator(response, request); !ok {
		return
	}
	kind, ok := controlPlanePolicyKind(request.URL.Path)
	if !ok {
		writeError(response, http.StatusNotFound, "policy resource not found")
		return
	}
	version, err := strconv.ParseInt(request.URL.Query().Get("resourceVersion"), 10, 64)
	if err != nil || version <= 0 {
		writeError(response, http.StatusConflict, "resourceVersion query parameter is required for delete")
		return
	}
	policies, supported := s.store.(store.ControlPlanePolicyStore)
	if !supported {
		writeError(response, http.StatusInternalServerError, "control-plane policy storage failed")
		return
	}
	if err := s.recordResourceAudit(request.Context(), kind, request.PathValue("namespace"), request.PathValue("name"), "delete"); err != nil {
		s.logger.Error("audit policy delete failed", zap.Error(err), requestid.ZapField(request.Context()))
		writeError(response, http.StatusInternalServerError, "audit durability failed")
		return
	}
	if kind == "ControlPlaneRole" {
		if err := policies.DeleteControlPlaneRole(request.Context(), request.PathValue("namespace"), request.PathValue("name"), version); err != nil {
			writeControlPlanePolicyStoreError(response, err)
			return
		}
	} else if err := policies.DeleteControlPlaneRoleBinding(request.Context(), request.PathValue("namespace"), request.PathValue("name"), version); err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizeControlPlaneAdministrator(response http.ResponseWriter, request *http.Request) (*auth.Identity, bool) {
	identity, authenticated := auth.IdentityFromContext(request.Context())
	if !authenticated || !auth.IsAuthenticated(request.Context()) {
		writeError(response, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	if identity.Method == "token" || identity.Method == "anonymous" || identity.Issuer == "" || identity.Subject == "" || !intersects(identity.Groups, s.controlPlaneAdminGroups) {
		writeError(response, http.StatusForbidden, "forbidden")
		return nil, false
	}
	return identity, true
}

func decodeControlPlanePolicy(response http.ResponseWriter, request *http.Request, expectedName string, minimumVersion int64) (string, []byte, string, int64, bool) {
	kind, ok := controlPlanePolicyKind(request.URL.Path)
	if !ok {
		writeError(response, http.StatusNotFound, "policy resource not found")
		return "", nil, "", 0, false
	}
	var object any
	var metadata *api.ObjectMeta
	var typeMetadata *api.TypeMeta
	switch kind {
	case "ControlPlaneRole":
		policy := &api.ControlPlaneRole{}
		object, metadata, typeMetadata = policy, &policy.ObjectMeta, &policy.TypeMeta
	case "ControlPlaneRoleBinding":
		policy := &api.ControlPlaneRoleBinding{}
		object, metadata, typeMetadata = policy, &policy.ObjectMeta, &policy.TypeMeta
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		writeError(response, http.StatusBadRequest, "invalid request")
		return "", nil, "", 0, false
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(raw)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid request")
		return "", nil, "", 0, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(canonical)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(object); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(response, http.StatusBadRequest, "invalid request")
		return "", nil, "", 0, false
	}
	name := metadata.Name
	if name == "" || (expectedName != "" && name != expectedName) {
		writeError(response, http.StatusBadRequest, "metadata.name must match the request target")
		return "", nil, "", 0, false
	}
	version := metadata.ResourceVersion
	if minimumVersion > 0 && version < minimumVersion {
		writeError(response, http.StatusBadRequest, "metadata.resourceVersion must be a positive integer")
		return "", nil, "", 0, false
	}
	metadata.Namespace = request.PathValue("namespace")
	if minimumVersion == 0 {
		metadata.UID = uuid.NewString()
		metadata.ResourceVersion = 1
		metadata.CreationTimestamp = time.Now().UTC()
	}
	typeMetadata.APIVersion = api.APIVersion
	typeMetadata.Kind = kind
	encoded, err := json.Marshal(object)
	if err != nil {
		writeError(response, http.StatusInternalServerError, "failed to serialize policy")
		return "", nil, "", 0, false
	}
	return kind, encoded, name, version, true
}

func validateControlPlanePolicy(ctx context.Context, policies store.Store, kind, namespace string, data []byte) error {
	switch kind {
	case "ControlPlaneRole":
		var role api.ControlPlaneRole
		if err := json.Unmarshal(data, &role); err != nil {
			return fmt.Errorf("invalid ControlPlaneRole: %w", err)
		}
		if len(role.Spec.Rules) == 0 && len(role.Spec.ExecutionRules) == 0 {
			return errors.New("at least one rule is required")
		}
		if len(role.Spec.ExecutionRules) > controlplaneauth.MaxExecutionRules {
			return errors.New("at most 128 execution rules are supported")
		}
		for _, rule := range role.Spec.ExecutionRules {
			if err := controlplaneauth.ValidateExecutionRule(rule); err != nil {
				return errors.New("execution rules must name an exact supported operation, workspace, resource UID, and Workflow revision")
			}
		}
		for _, rule := range role.Spec.Rules {
			if rule.Effect != "allow" && rule.Effect != "deny" {
				return errors.New("rule effect must be allow or deny")
			}
			if !onlyAllowed(rule.Resources, "RuntimeApproval") || !onlyAllowed(rule.Verbs, "approve", "reject", "reconcile") || len(rule.Workspaces) == 0 || containsBlank(rule.Workspaces) {
				return errors.New("rule resources, verbs, and workspaces must be explicit and supported")
			}
			if containsBlank(rule.RuntimeApprovalNames) || (rule.RuntimeApprovalNameSelector != nil && strings.TrimSpace(rule.RuntimeApprovalNameSelector.Prefix) == "") || (len(rule.RuntimeApprovalNames) == 0 && rule.RuntimeApprovalNameSelector == nil) {
				return errors.New("rule must bound RuntimeApproval names")
			}
		}
	case "ControlPlaneRoleBinding":
		var binding api.ControlPlaneRoleBinding
		if err := json.Unmarshal(data, &binding); err != nil {
			return fmt.Errorf("invalid ControlPlaneRoleBinding: %w", err)
		}
		if strings.TrimSpace(binding.Spec.RoleRef.Name) == "" || len(binding.Spec.Subjects) == 0 {
			return errors.New("roleRef and at least one subject are required")
		}
		if _, err := policies.Get(ctx, "ControlPlaneRole", namespace, binding.Spec.RoleRef.Name); err != nil {
			return errors.New("referenced ControlPlaneRole does not exist")
		}
		for _, subject := range binding.Spec.Subjects {
			if strings.TrimSpace(subject.Method) == "" || subject.Method == "token" || subject.Method == "anonymous" || strings.TrimSpace(subject.Issuer) == "" || strings.TrimSpace(subject.Subject) == "" {
				return errors.New("subjects must use canonical non-bootstrap identities")
			}
		}
	default:
		return errors.New("unsupported policy kind")
	}
	return nil
}

func controlPlanePolicyKind(path string) (string, bool) {
	if strings.Contains(path, "/controlplanerolebindings") {
		return "ControlPlaneRoleBinding", true
	}
	if strings.Contains(path, "/controlplaneroles") {
		return "ControlPlaneRole", true
	}
	return "", false
}

func onlyAllowed(values []string, allowed ...string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !containsString(allowed, value) {
			return false
		}
	}
	return true
}

func containsBlank(values []string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
	}
	return false
}

func intersects(left, right []string) bool {
	for _, value := range left {
		if containsString(right, value) {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func writeControlPlanePolicyStoreError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrAlreadyExists), errors.Is(err, store.ErrConflict):
		writeError(response, http.StatusConflict, "control-plane policy conflict")
	case errors.Is(err, store.ErrNotFound):
		writeError(response, http.StatusNotFound, "control-plane policy not found")
	default:
		writeError(response, http.StatusInternalServerError, "control-plane policy storage failed")
	}
}
