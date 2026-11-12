// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestRuntimeApprovalReconciliationHandlerAuthenticatesAuthorizesAndReconciles(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	tokenReviews := &recordingTokenReviewer{response: authenticatedTokenReview()}
	accessReviews := &recordingSubjectAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	reconciliations := &recordingRuntimeApprovalReconciliationStore{result: api.RuntimeApproval{
		ObjectMeta: api.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 8},
		Spec:       api.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: invocationID},
		Status:     api.RuntimeApprovalStatus{ExecutionState: api.RuntimeApprovalExecutionCompleted},
	}}
	handler := NewRuntimeApprovalReconciliationHandler(tokenReviews, accessReviews, reconciliations)

	response := serveRuntimeApprovalReconciliation(t, handler, "token-a", map[string]any{
		"namespace": "team-a", "name": name, "invocationID": invocationID,
		"disposition": runtimeapproval.ExecutionDispositionReconciledCompleted,
		"evidenceRef": "ticket://incident-42", "expectedResourceVersion": 7,
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	wantAttributes := authorizationv1.ResourceAttributes{
		Namespace: "team-a", Verb: "reconcile", Group: "kognition.dev",
		Resource: "runtimeapprovals", Subresource: "reconcile", Name: name,
	}
	if tokenReviews.token != "token-a" || accessReviews.spec.User != "alice" || accessReviews.spec.ResourceAttributes == nil || *accessReviews.spec.ResourceAttributes != wantAttributes {
		t.Fatalf("reviews = token %q, access %+v; want token-a and attributes %+v", tokenReviews.token, accessReviews.spec, wantAttributes)
	}
	if reconciliations.workspace != "team-a" || reconciliations.invocationID != invocationID || reconciliations.expectedResourceVersion != 7 || reconciliations.disposition != runtimeapproval.ExecutionDispositionReconciledCompleted || reconciliations.evidenceRef != "ticket://incident-42" || reconciliations.issuer != KubernetesTokenReviewIssuer || reconciliations.subject != "alice" {
		t.Fatalf("reconciliation = %+v, want verified alice reconciliation at resource version 7", reconciliations)
	}
}

func TestRuntimeApprovalReconciliationHandlerFailsClosedBeforeMutation(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	tests := []struct {
		name           string
		tokenResponse  *authenticationv1.TokenReview
		tokenError     error
		accessResponse *authorizationv1.SubjectAccessReview
		accessError    error
		wantStatus     int
	}{
		{name: "invalid token", tokenResponse: &authenticationv1.TokenReview{}, wantStatus: http.StatusUnauthorized},
		{name: "token review unavailable", tokenError: errors.New("unavailable"), wantStatus: http.StatusServiceUnavailable},
		{name: "access denied", tokenResponse: authenticatedTokenReview(), accessResponse: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Denied: true}}, wantStatus: http.StatusForbidden},
		{name: "explicit denial overrides allow", tokenResponse: authenticatedTokenReview(), accessResponse: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true, Denied: true}}, wantStatus: http.StatusForbidden},
		{name: "access review unavailable", tokenResponse: authenticatedTokenReview(), accessError: errors.New("unavailable"), wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reconciliations := &recordingRuntimeApprovalReconciliationStore{}
			handler := NewRuntimeApprovalReconciliationHandler(
				&recordingTokenReviewer{response: test.tokenResponse, err: test.tokenError},
				&recordingSubjectAccessReviewer{response: test.accessResponse, err: test.accessError},
				reconciliations,
			)
			response := serveRuntimeApprovalReconciliation(t, handler, "token-a", map[string]any{
				"namespace": "team-a", "name": name, "invocationID": invocationID,
				"disposition": runtimeapproval.ExecutionDispositionReconciledNotExecuted,
				"evidenceRef": "ticket://incident-42", "expectedResourceVersion": 7,
			})
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if reconciliations.calls != 0 {
				t.Fatalf("ReconcileRuntimeApprovalExecution calls = %d, want 0", reconciliations.calls)
			}
		})
	}
}

func TestRuntimeApprovalReconciliationHandlerRejectsRuntimeIdentityBeforeAuthorization(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	runtimeUsername := "system:serviceaccount:kognition-system:kognition-tool-gateway"
	tokenReviews := &recordingTokenReviewer{response: &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: runtimeUsername, UID: "gateway-uid"},
	}}}
	accessReviews := &recordingSubjectAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	reconciliations := &recordingRuntimeApprovalReconciliationStore{}
	handler := NewRuntimeApprovalReconciliationHandler(tokenReviews, accessReviews, reconciliations, runtimeUsername)

	response := serveRuntimeApprovalReconciliation(t, handler, "runtime-token", map[string]any{
		"namespace": "team-a", "name": name, "invocationID": invocationID,
		"disposition": runtimeapproval.ExecutionDispositionReconciledCompleted,
		"evidenceRef": "ticket://incident-42", "expectedResourceVersion": 7,
	})

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if accessReviews.calls != 0 {
		t.Fatalf("SubjectAccessReview calls = %d, want 0", accessReviews.calls)
	}
	if reconciliations.calls != 0 {
		t.Fatalf("ReconcileRuntimeApprovalExecution calls = %d, want 0", reconciliations.calls)
	}
}

func TestRuntimeApprovalReconciliationHandlerRejectsInvalidRequestBeforeAuthentication(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	valid := map[string]any{
		"namespace": "team-a", "name": name, "invocationID": invocationID,
		"disposition": runtimeapproval.ExecutionDispositionReconciledCompleted,
		"evidenceRef": "ticket://incident-42", "expectedResourceVersion": 7,
	}
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "noncanonical target", field: "name", value: "other"},
		{name: "invalid disposition", field: "disposition", value: "Succeeded"},
		{name: "empty evidence", field: "evidenceRef", value: ""},
		{name: "oversized evidence", field: "evidenceRef", value: string(bytes.Repeat([]byte{'a'}, maxReconciliationRequestEvidenceRef+1))},
		{name: "invalid resource version", field: "expectedResourceVersion", value: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := make(map[string]any, len(valid))
			for key, value := range valid {
				body[key] = value
			}
			body[test.field] = test.value
			tokenReviews := &recordingTokenReviewer{response: authenticatedTokenReview()}
			reconciliations := &recordingRuntimeApprovalReconciliationStore{}
			handler := NewRuntimeApprovalReconciliationHandler(tokenReviews, &recordingSubjectAccessReviewer{}, reconciliations)

			response := serveRuntimeApprovalReconciliation(t, handler, "token-a", body)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
			if tokenReviews.token != "" || reconciliations.calls != 0 {
				t.Fatalf("authentication token = %q, reconciliation calls = %d; want no authentication or mutation", tokenReviews.token, reconciliations.calls)
			}
		})
	}
}

func TestRuntimeApprovalReconciliationHandlerMapsStoreErrors(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "not found", err: store.ErrNotFound, wantStatus: http.StatusNotFound},
		{name: "conflict", err: store.ErrConflict, wantStatus: http.StatusConflict},
		{name: "internal failure", err: errors.New("write failed"), wantStatus: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reconciliations := &recordingRuntimeApprovalReconciliationStore{err: test.err}
			handler := NewRuntimeApprovalReconciliationHandler(
				&recordingTokenReviewer{response: authenticatedTokenReview()},
				&recordingSubjectAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}},
				reconciliations,
			)

			response := serveRuntimeApprovalReconciliation(t, handler, "token-a", map[string]any{
				"namespace": "team-a", "name": name, "invocationID": invocationID,
				"disposition": runtimeapproval.ExecutionDispositionReconciledCompleted,
				"evidenceRef": "ticket://incident-42", "expectedResourceVersion": 7,
			})

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if reconciliations.calls != 1 {
				t.Fatalf("ReconcileRuntimeApprovalExecution calls = %d, want 1", reconciliations.calls)
			}
		})
	}
}

func serveRuntimeApprovalReconciliation(t *testing.T, handler http.Handler, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, RuntimeApprovalReconciliationPath, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type recordingRuntimeApprovalReconciliationStore struct {
	result                  api.RuntimeApproval
	err                     error
	calls                   int
	workspace               string
	invocationID            string
	expectedResourceVersion int64
	disposition             string
	evidenceRef             string
	issuer                  string
	subject                 string
}

func (s *recordingRuntimeApprovalReconciliationStore) ReconcileRuntimeApprovalExecution(_ context.Context, workspace, invocationID string, expectedResourceVersion int64, disposition, evidenceRef, issuer, subject string) (api.RuntimeApproval, error) {
	s.calls++
	s.workspace = workspace
	s.invocationID = invocationID
	s.expectedResourceVersion = expectedResourceVersion
	s.disposition = disposition
	s.evidenceRef = evidenceRef
	s.issuer = issuer
	s.subject = subject
	return s.result, s.err
}
