// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

type workflowRunAcceptanceRequest struct {
	Binding         api.RuntimeApprovalReference `json:"binding"`
	Run             api.WorkflowRun              `json:"run"`
	DurationSeconds int64                        `json:"durationSeconds"`
}

func (s *Server) handleAcceptedWorkflowRun(response http.ResponseWriter, request *http.Request) {
	initiator, err := runtimeapproval.AuthenticatedWorkflowExecutor(request.Context())
	if err != nil {
		writeError(response, http.StatusUnauthorized, "authenticated submitter required")
		return
	}
	for _, executor := range s.workflowExecutorPrincipals {
		if initiator == executor {
			writeError(response, http.StatusForbidden, "an executor cannot submit as an original initiator")
			return
		}
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, runtimeapproval.WorkflowExecutorRecordMaxBytes+1))
	if err != nil || len(raw) > runtimeapproval.WorkflowExecutorRecordMaxBytes {
		writeError(response, http.StatusBadRequest, "invalid run submission")
		return
	}
	if _, err := runtimeapproval.CanonicalizeJSON(raw); err != nil {
		writeError(response, http.StatusBadRequest, "invalid run submission")
		return
	}
	var input workflowRunAcceptanceRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(response, http.StatusBadRequest, "invalid run submission")
		return
	}
	namespace := request.PathValue("namespace")
	if input.Binding.Namespace != namespace || input.Run.Namespace != namespace || input.Run.UID != "" || input.Run.ResourceVersion != 0 ||
		input.Run.Name == "" || input.DurationSeconds < 1 || input.DurationSeconds > 86400 {
		writeError(response, http.StatusBadRequest, "run scope, persisted identity, and finite duration are required")
		return
	}
	records, ok := s.store.(runtimeapproval.WorkflowExecutorBindingStore)
	creator, canCreate := s.store.(runtimeapproval.WorkflowRunSubmissionStore)
	if !ok || !canCreate || s.workflowExecutorClock == nil || s.auditLog == nil {
		writeError(response, http.StatusServiceUnavailable, "trusted run acceptance is not configured")
		return
	}
	authority, err := records.GetWorkflowExecutorAuthority(request.Context(), input.Binding)
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	configuredExecutor := false
	for _, executor := range s.workflowExecutorPrincipals {
		if executor == authority.Executor {
			configuredExecutor = true
			break
		}
	}
	if !configuredExecutor {
		writeError(response, http.StatusServiceUnavailable, "approved executor is not configured")
		return
	}
	if input.Run.Spec.WorkflowRef.Name != authority.Workflow.Name {
		writeError(response, http.StatusBadRequest, "run must use the approved Workflow revision")
		return
	}
	workflowJSON, err := s.store.Get(request.Context(), "Workflow", namespace, authority.Workflow.Name)
	if err != nil {
		writeControlPlanePolicyStoreError(response, err)
		return
	}
	var workflow api.Workflow
	digest, digestErr := runtimeapproval.WorkflowExecutorSpecDigest(workflowJSON)
	if err := json.Unmarshal(workflowJSON, &workflow); err != nil || digestErr != nil || workflow.UID != authority.Workflow.UID ||
		workflow.ResourceVersion != authority.Workflow.ResourceVersion || digest != authority.WorkflowRevisionDigest {
		writeError(response, http.StatusConflict, "Workflow revision no longer matches its approved execution binding")
		return
	}
	identity, _ := auth.IdentityFromContext(request.Context())
	if err := controlplaneauth.New(s.store).AuthorizeWorkflowUse(request.Context(), identity, namespace, workflow.Name, workflow.UID, digest); err != nil {
		writeError(response, http.StatusForbidden, "submitter cannot use this exact Workflow revision")
		return
	}
	now, err := s.workflowExecutorClock(request.Context())
	if err != nil || now.IsZero() || !authority.Enabled || now.Before(authority.NotBefore) || !authority.ExpiresAt.After(now) ||
		input.DurationSeconds > authority.Limits.MaxDurationSeconds {
		writeError(response, http.StatusConflict, "execution binding is unavailable, expired, revoked, or exceeds its run allowance")
		return
	}
	input.Run.Kind, input.Run.APIVersion = "WorkflowRun", api.APIVersion
	input.Run.Status = api.WorkflowRunStatus{}
	if input.Run.Annotations == nil {
		input.Run.Annotations = make(map[string]string)
	}
	input.Run.Annotations[runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation] = "true"
	runJSON, err := json.Marshal(input.Run)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid run submission")
		return
	}
	admitted, err := s.admission.Admit(request.Context(), &admission.Request{Operation: admission.OperationCreate, Kind: "WorkflowRun", Namespace: namespace,
		Name: input.Run.Name, Object: runJSON, UserInfo: admissionUserInfo(request.Context())})
	if err != nil {
		writeError(response, http.StatusUnprocessableEntity, "run admission denied")
		return
	}
	if admitted != nil {
		runJSON = admitted
	}
	if err := s.recordResourceAudit(request.Context(), "WorkflowRun", namespace, input.Run.Name, "create"); err != nil {
		writeError(response, http.StatusServiceUnavailable, "run acceptance audit durability failed")
		return
	}
	if err := s.recordResourceAudit(request.Context(), "WorkflowRunAcceptance", namespace, input.Run.Name, "accept"); err != nil {
		writeError(response, http.StatusServiceUnavailable, "run acceptance audit durability failed")
		return
	}
	run, retained, err := creator.CreateAcceptedWorkflowRun(request.Context(), authority.Reference, runJSON, input.DurationSeconds)
	if err != nil {
		switch {
		case errors.Is(err, controlplaneauth.ErrForbidden):
			writeError(response, http.StatusForbidden, "final Workflow use authorization denied")
		case errors.Is(err, controlplaneauth.ErrUnavailable):
			writeError(response, http.StatusServiceUnavailable, "final Workflow use authorization unavailable")
		default:
			writeControlPlanePolicyStoreError(response, err)
		}
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"run": run, "acceptance": retained})
}
