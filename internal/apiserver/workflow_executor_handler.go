// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

type workflowExecutorBindingManager interface {
	CreateWorkflowExecutorAuthority(context.Context, runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error)
	UpdateWorkflowExecutorAuthority(context.Context, runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error)
	GetWorkflowExecutorAuthority(context.Context, api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error)
}

func (s *Server) workflowBindingAdministrator(response http.ResponseWriter, request *http.Request) (api.ControlPlaneRoleSubject, bool) {
	if _, ok := s.authorizeControlPlaneAdministrator(response, request); !ok {
		return api.ControlPlaneRoleSubject{}, false
	}
	actor, err := runtimeapproval.AuthenticatedWorkflowExecutor(request.Context())
	if err != nil || len(s.workflowExecutorPrincipals) == 0 {
		writeError(response, http.StatusForbidden, "binding administrator and configured workload principals are required")
		return api.ControlPlaneRoleSubject{}, false
	}
	for _, executor := range s.workflowExecutorPrincipals {
		if actor == executor {
			writeError(response, http.StatusForbidden, "executors cannot administer their execution bindings")
			return api.ControlPlaneRoleSubject{}, false
		}
	}
	return actor, true
}

func decodeWorkflowBindingRequest(request *http.Request, operator api.ControlPlaneRoleSubject, update bool) (runtimeapproval.WorkflowExecutorAuthority, error) {
	var authority runtimeapproval.WorkflowExecutorAuthority
	raw, err := io.ReadAll(io.LimitReader(request.Body, runtimeapproval.WorkflowExecutorRecordMaxBytes+1))
	if err != nil || len(raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		return authority, fmt.Errorf("binding exceeds its request bound")
	}
	if _, err := runtimeapproval.CanonicalizeJSON(raw); err != nil {
		return authority, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authority); err != nil {
		return authority, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return authority, fmt.Errorf("binding has trailing content")
	}
	authority.ApprovedBy = operator
	if authority.Reference.Namespace != request.PathValue("namespace") || update && authority.Reference.Name != request.PathValue("name") ||
		!update && (authority.Reference.UID != "" || authority.Reference.ResourceVersion != 0) ||
		update && (authority.Reference.UID == "" || authority.Reference.ResourceVersion <= 0) {
		return authority, fmt.Errorf("binding target and persisted identity must match the operation")
	}
	return authority, runtimeapproval.ValidateWorkflowExecutorAuthority(authority)
}

func (s *Server) handleWorkflowBindingCreate(response http.ResponseWriter, request *http.Request) {
	s.writeWorkflowBinding(response, request, false, false)
}

func (s *Server) handleWorkflowBindingUpdate(response http.ResponseWriter, request *http.Request) {
	s.writeWorkflowBinding(response, request, true, false)
}

func (s *Server) handleWorkflowBindingRevoke(response http.ResponseWriter, request *http.Request) {
	s.writeWorkflowBinding(response, request, true, true)
}

func (s *Server) writeWorkflowBinding(response http.ResponseWriter, request *http.Request, update, revoke bool) {
	operator, ok := s.workflowBindingAdministrator(response, request)
	if !ok {
		return
	}
	authority, err := decodeWorkflowBindingRequest(request, operator, update)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid execution binding")
		return
	}
	allowed := false
	for _, executor := range s.workflowExecutorPrincipals {
		allowed = allowed || executor == authority.Executor
	}
	if !allowed {
		writeError(response, http.StatusForbidden, "executor is not an operator-configured workload principal")
		return
	}
	storage, ok := s.store.(workflowExecutorBindingManager)
	if !ok {
		writeError(response, http.StatusServiceUnavailable, "protected execution binding storage is unavailable")
		return
	}
	verb := "create"
	if update {
		verb = "update"
	}
	if revoke {
		authority.Enabled, verb = false, "revoke"
	}
	if authority.Enabled {
		workflowJSON, err := s.store.Get(request.Context(), "Workflow", authority.Workflow.Namespace, authority.Workflow.Name)
		if err != nil {
			writeControlPlanePolicyStoreError(response, err)
			return
		}
		if err := runtimeapproval.ValidateWorkflowExecutorAuthorityDefinition(authority, workflowJSON); err != nil {
			writeError(response, http.StatusConflict, "execution binding does not match the live Workflow definition")
			return
		}
	}
	if s.auditLog == nil {
		writeError(response, http.StatusServiceUnavailable, "execution binding administration requires durable audit")
		return
	}
	if err := s.recordResourceAudit(request.Context(), "WorkflowExecutionBinding", authority.Reference.Namespace, authority.Reference.Name, verb); err != nil {
		writeError(response, http.StatusServiceUnavailable, "execution binding audit durability failed")
		return
	}
	var saved runtimeapproval.WorkflowExecutorAuthority
	if update {
		saved, err = storage.UpdateWorkflowExecutorAuthority(request.Context(), authority)
	} else {
		saved, err = storage.CreateWorkflowExecutorAuthority(request.Context(), authority)
	}
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	status := http.StatusOK
	if !update {
		status = http.StatusCreated
	}
	writeJSON(response, status, saved)
}

func (s *Server) handleWorkflowBindingGet(response http.ResponseWriter, request *http.Request) {
	if _, ok := s.workflowBindingAdministrator(response, request); !ok {
		return
	}
	storage, ok := s.store.(workflowExecutorBindingManager)
	if !ok {
		writeError(response, http.StatusServiceUnavailable, "protected execution binding storage is unavailable")
		return
	}
	authority, err := storage.GetWorkflowExecutorAuthority(request.Context(), api.RuntimeApprovalReference{Kind: "WorkflowExecutionBinding", Scope: "Namespaced",
		Namespace: request.PathValue("namespace"), Name: request.PathValue("name")})
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	writeJSON(response, http.StatusOK, authority)
}

type workflowExecutorInitiationRequest struct {
	Action     runtimeapproval.WorkflowExecutorAction               `json:"action"`
	Capability runtimeapproval.WorkflowExecutorInitiationCapability `json:"capability"`
	Approval   api.RuntimeApproval                                  `json:"approval"`
}

func (s *Server) readWorkflowExecutorInitiation(response http.ResponseWriter, request *http.Request) (workflowExecutorInitiationRequest, bool) {
	var input workflowExecutorInitiationRequest
	if s.workflowExecutorInitiation == nil {
		writeError(response, http.StatusServiceUnavailable, "qualified executor initiation is not configured")
		return input, false
	}
	actor, err := runtimeapproval.AuthenticatedWorkflowExecutor(request.Context())
	if err != nil {
		writeError(response, http.StatusForbidden, "initiation requires an authenticated workload executor")
		return input, false
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, 3*runtimeapproval.WorkflowExecutorRecordMaxBytes+1))
	if err != nil || len(raw) > 3*runtimeapproval.WorkflowExecutorRecordMaxBytes {
		writeError(response, http.StatusBadRequest, "initiation exceeds its request bound")
		return input, false
	}
	if _, err := runtimeapproval.CanonicalizeJSON(raw); err != nil {
		writeError(response, http.StatusBadRequest, "invalid initiation request")
		return input, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(response, http.StatusBadRequest, "invalid initiation request")
		return input, false
	}
	if actor != input.Action.Executor || input.Action.Activation.Workspace != request.PathValue("namespace") {
		writeError(response, http.StatusForbidden, "initiation does not match its authenticated executor and workspace")
		return input, false
	}
	return input, true
}

func (s *Server) handleWorkflowExecutorCapability(response http.ResponseWriter, request *http.Request) {
	input, ok := s.readWorkflowExecutorInitiation(response, request)
	if !ok {
		return
	}
	capability, err := s.workflowExecutorInitiation.Issue(request.Context(), input.Action)
	if err != nil {
		writeError(response, http.StatusForbidden, "initiation capability denied")
		return
	}
	writeJSON(response, http.StatusCreated, capability)
}

func (s *Server) handleWorkflowExecutorInitiationResolve(response http.ResponseWriter, request *http.Request) {
	input, ok := s.readWorkflowExecutorInitiation(response, request)
	if !ok {
		return
	}
	result, err := s.workflowExecutorInitiation.Resolve(request.Context(), input.Action, input.Capability)
	if err != nil {
		writeError(response, http.StatusForbidden, "initiation resolution denied")
		return
	}
	writeJSON(response, http.StatusOK, result)
}

func (s *Server) handleWorkflowExecutorInitiationDispatch(response http.ResponseWriter, request *http.Request) {
	input, ok := s.readWorkflowExecutorInitiation(response, request)
	if !ok {
		return
	}
	if s.workflowExecutorInitiationTarget == nil {
		writeError(response, http.StatusServiceUnavailable, "qualified initiation transport is not configured")
		return
	}
	result, err := s.workflowExecutorInitiation.Dispatch(request.Context(), input.Action, input.Capability, input.Approval, func(ctx context.Context) error { return s.workflowExecutorInitiationTarget(ctx, input.Action) })
	if err != nil {
		writeJSON(response, http.StatusConflict, struct {
			runtimeapproval.WorkflowDispatchResult
			Error string `json:"error"`
		}{result, "initiation dispatch was not acknowledged; retained commit does not authorize replay"})
		return
	}
	writeJSON(response, http.StatusOK, result)
}
