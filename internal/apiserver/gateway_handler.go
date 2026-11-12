// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/gateway"
	"github.com/kognition-ai/kognition/internal/requestid"
)

// GoalSubmission is the request body for submitting a goal to an IntentGateway.
type GoalSubmission = gateway.GoalSubmission

// GoalResponse is the response returned after a goal is submitted.
type GoalResponse = gateway.GoalResponse

// SolutionReference identifies the created Solution resource.
type SolutionReference = gateway.SolutionReference

// handleSubmitGoal accepts a goal submission and creates a Solution resource
// routed through the specified IntentGateway.
func (s *Server) handleSubmitGoal(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	gatewayName := r.PathValue("name")

	// Read and validate request body.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body is required")
		return
	}

	var submission gateway.GoalSubmission
	if err := json.Unmarshal(body, &submission); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	response, submissionErr := gateway.NewService(s.store, s, s.rateLimiter).SubmitGoal(r.Context(), gateway.SubmissionRequest{
		Namespace:   namespace,
		GatewayName: gatewayName,
		Submission:  submission,
		HeaderValue: r.Header.Get,
	})
	if submissionErr != nil {
		s.writeGoalSubmissionError(w, r, submissionErr)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) ResolveAPIKey(ctx context.Context, namespace, name string) (string, error) {
	return gateway.NewStoreAPIKeyResolver(s.store, nil).ResolveAPIKey(ctx, namespace, name)
}

func (s *Server) writeGoalSubmissionError(w http.ResponseWriter, r *http.Request, submissionErr *gateway.SubmissionError) {
	if submissionErr.Kind == gateway.ErrorInternal && submissionErr.Err != nil {
		s.logger.Error("intent gateway goal submission failed", zap.Error(submissionErr.Err), requestid.ZapField(r.Context()))
	}
	switch submissionErr.Kind {
	case gateway.ErrorInvalidRequest:
		writeError(w, http.StatusBadRequest, submissionErr.Message)
	case gateway.ErrorNotFound:
		writeError(w, http.StatusNotFound, submissionErr.Message)
	case gateway.ErrorUnavailable:
		writeError(w, http.StatusServiceUnavailable, submissionErr.Message)
	case gateway.ErrorUnauthorized:
		w.Header().Set("WWW-Authenticate", "ApiKey")
		writeError(w, http.StatusUnauthorized, submissionErr.Message)
	case gateway.ErrorRateLimited:
		writeError(w, http.StatusTooManyRequests, submissionErr.Message)
	case gateway.ErrorNoRoute:
		writeError(w, http.StatusUnprocessableEntity, submissionErr.Message)
	default:
		writeError(w, http.StatusInternalServerError, submissionErr.Message)
	}
}
