// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const maxRuntimeApprovalEvidenceRef = 512

type runtimeApprovalDecisionInput struct {
	InvocationID            string `json:"invocationID"`
	Decision                string `json:"decision"`
	ExpectedResourceVersion int64  `json:"expectedResourceVersion"`
}

type runtimeApprovalReconciliationInput struct {
	InvocationID            string `json:"invocationID"`
	Disposition             string `json:"disposition"`
	EvidenceRef             string `json:"evidenceRef"`
	ExpectedResourceVersion int64  `json:"expectedResourceVersion"`
}

func (s *Server) handleRuntimeApprovalDecision(response http.ResponseWriter, request *http.Request) {
	var input runtimeApprovalDecisionInput
	if !decodeRuntimeApprovalInput(response, request, &input) {
		return
	}
	workspace, name := request.PathValue("namespace"), request.PathValue("name")
	if input.InvocationID == "" || input.ExpectedResourceVersion <= 0 || name != runtimeapproval.CanonicalName(workspace, input.InvocationID) {
		writeError(response, http.StatusBadRequest, "invalid RuntimeApproval target")
		return
	}
	decision := api.RuntimeApprovalDecisionRejected
	if input.Decision == "approve" {
		decision = api.RuntimeApprovalDecisionApproved
	} else if input.Decision != "reject" {
		writeError(response, http.StatusBadRequest, "decision must be approve or reject")
		return
	}

	identity, approval, ok := s.authorizeRuntimeApproval(response, request, workspace, name, input.InvocationID, input.Decision)
	if !ok {
		return
	}
	updated, err := s.runtimeApprovals.DecideRuntimeApproval(request.Context(), workspace, input.InvocationID, store.RuntimeApprovalDecision{
		Decision: decision, ExpectedResourceVersion: input.ExpectedResourceVersion, Issuer: identity.Issuer, Subject: identity.Subject,
	})
	if err != nil {
		writeRuntimeApprovalStoreError(response, err, "decision failed")
		return
	}
	_ = approval
	writeJSON(response, http.StatusOK, updated)
}

func (s *Server) handleRuntimeApprovalReconciliation(response http.ResponseWriter, request *http.Request) {
	var input runtimeApprovalReconciliationInput
	if !decodeRuntimeApprovalInput(response, request, &input) {
		return
	}
	workspace, name := request.PathValue("namespace"), request.PathValue("name")
	if input.InvocationID == "" || input.ExpectedResourceVersion <= 0 || name != runtimeapproval.CanonicalName(workspace, input.InvocationID) {
		writeError(response, http.StatusBadRequest, "invalid RuntimeApproval target")
		return
	}
	if err := runtimeapproval.ValidateReconciliationDisposition(input.Disposition); err != nil || len(input.EvidenceRef) < 1 || len(input.EvidenceRef) > maxRuntimeApprovalEvidenceRef {
		writeError(response, http.StatusBadRequest, "invalid reconciliation request")
		return
	}

	identity, _, ok := s.authorizeRuntimeApproval(response, request, workspace, name, input.InvocationID, "reconcile")
	if !ok {
		return
	}
	updated, err := s.runtimeApprovals.ReconcileRuntimeApprovalExecution(request.Context(), workspace, input.InvocationID, input.ExpectedResourceVersion, input.Disposition, input.EvidenceRef, identity.Issuer, identity.Subject)
	if err != nil {
		writeRuntimeApprovalStoreError(response, err, "reconciliation failed")
		return
	}
	writeJSON(response, http.StatusOK, updated)
}

func (s *Server) authorizeRuntimeApproval(response http.ResponseWriter, request *http.Request, workspace, name, invocationID, verb string) (*auth.Identity, api.RuntimeApproval, bool) {
	if s.runtimeApprovals == nil || s.approvalAuthorizer == nil {
		writeError(response, http.StatusServiceUnavailable, "RuntimeApproval operations are unavailable")
		return nil, api.RuntimeApproval{}, false
	}
	identity, authenticated := auth.IdentityFromContext(request.Context())
	if !authenticated || !auth.IsAuthenticated(request.Context()) {
		writeError(response, http.StatusUnauthorized, "authentication required")
		return nil, api.RuntimeApproval{}, false
	}
	approval, err := s.runtimeApprovals.GetRuntimeApprovalByInvocation(request.Context(), workspace, invocationID)
	if err != nil {
		writeRuntimeApprovalStoreError(response, err, "RuntimeApproval lookup failed")
		return nil, api.RuntimeApproval{}, false
	}
	requestingWorkload, err := s.isRequestingWorkload(request.Context(), identity.Subject, approval)
	if err != nil {
		writeError(response, http.StatusServiceUnavailable, "requesting workload identity unavailable")
		return nil, api.RuntimeApproval{}, false
	}
	if approval.Name != name || requestingWorkload {
		writeError(response, http.StatusForbidden, "forbidden")
		return nil, api.RuntimeApproval{}, false
	}
	if err := s.approvalAuthorizer.AuthorizeRuntimeApproval(request.Context(), identity, workspace, name, verb); err != nil {
		if errors.Is(err, controlplaneauth.ErrUnavailable) {
			writeError(response, http.StatusServiceUnavailable, "authorization unavailable")
		} else {
			writeError(response, http.StatusForbidden, "forbidden")
		}
		return nil, api.RuntimeApproval{}, false
	}
	return identity, approval, true
}

func (s *Server) isRequestingWorkload(ctx context.Context, subject string, approval api.RuntimeApproval) (bool, error) {
	for _, reference := range []api.RuntimeApprovalReference{approval.Spec.AgentRef, approval.Spec.AgentIdentityRef, approval.Spec.AgentInstanceRef} {
		if subject == reference.Name || subject == reference.UID {
			return true, nil
		}
	}
	if !strings.HasPrefix(subject, "spiffe://") {
		return false, nil
	}
	reference := approval.Spec.AgentIdentityRef
	namespace := reference.Namespace
	if namespace == "" {
		namespace = approval.Spec.Workspace
	}
	data, err := s.store.Get(ctx, "AgentIdentity", namespace, reference.Name)
	if err != nil {
		return false, err
	}
	var identity api.AgentIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		return false, err
	}
	return subject == identity.Spec.SPIFFEID, nil
}

func decodeRuntimeApprovalInput(response http.ResponseWriter, request *http.Request, target any) bool {
	raw, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		writeError(response, http.StatusBadRequest, "invalid request")
		return false
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(raw)
	if err != nil {
		writeError(response, http.StatusBadRequest, "invalid request")
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(string(canonical)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(response, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func writeRuntimeApprovalStoreError(response http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(response, http.StatusNotFound, "RuntimeApproval not found")
	case errors.Is(err, store.ErrConflict):
		writeError(response, http.StatusConflict, "RuntimeApproval conflict")
	default:
		writeError(response, http.StatusInternalServerError, fallback)
	}
}
