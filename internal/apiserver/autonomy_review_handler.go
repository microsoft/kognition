// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/store"
)

type autonomyReviewDecisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

func (s *Server) handleAutonomyReviewDecision(w http.ResponseWriter, r *http.Request) {
	identity, ok := auth.IdentityFromContext(r.Context())
	if !ok || identity == nil || identity.Method == "anonymous" {
		writeError(w, http.StatusUnauthorized, "authenticated reviewer identity is required")
		return
	}
	var request autonomyReviewDecisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	decision := strings.ToLower(strings.TrimSpace(request.Decision))
	if decision != "approve" && decision != "deny" {
		writeError(w, http.StatusBadRequest, "decision must be 'approve' or 'deny'")
		return
	}
	reviewer := identity.Username
	if reviewer == "" {
		reviewer = identity.Subject
	}
	reconciler := controller.NewAutonomyReviewReconciler(s.store, s.logger.Named("autonomy-review-decision")).SetAuditLog(s.auditLog)
	var err error
	if decision == "approve" {
		err = reconciler.Approve(r.Context(), r.PathValue("namespace"), r.PathValue("name"), reviewer, request.Reason)
	} else {
		err = reconciler.Deny(r.Context(), r.PathValue("namespace"), r.PathValue("name"), reviewer, request.Reason)
	}
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, store.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, store.ErrConflict), errors.Is(err, controller.ErrAutonomyReviewNotPending), errors.Is(err, controller.ErrAutonomyReviewExpired):
			status = http.StatusConflict
		}
		writeError(w, status, "review decision could not be applied")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"decision": decision, "decidedBy": reviewer})
}
