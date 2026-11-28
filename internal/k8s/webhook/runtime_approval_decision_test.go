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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestRuntimeApprovalDecisionHandlerAuthenticatesAuthorizesAndDecides(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	tokenReviews := &recordingTokenReviewer{response: &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "alice", UID: "alice-uid", Groups: []string{"approvers"}},
	}}}
	accessReviews := &recordingSubjectAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	decisions := &recordingRuntimeApprovalDecisionStore{result: api.RuntimeApproval{
		ObjectMeta: api.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 8},
		Spec:       api.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: invocationID},
		Status:     api.RuntimeApprovalStatus{DecisionState: api.RuntimeApprovalDecisionApproved},
	}}
	handler := NewRuntimeApprovalDecisionHandler(tokenReviews, accessReviews, decisions)

	response := serveRuntimeApprovalDecision(t, handler, "token-a", map[string]any{
		"namespace": "team-a", "name": name, "invocationID": invocationID,
		"decision": "approve", "expectedResourceVersion": 7,
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if tokenReviews.token != "token-a" {
		t.Fatalf("TokenReview token = %q, want token-a", tokenReviews.token)
	}
	wantAttributes := authorizationv1.ResourceAttributes{
		Namespace: "team-a", Verb: "approve", Group: "kognition.dev",
		Resource: "runtimeapprovals", Subresource: "decision", Name: name,
	}
	if accessReviews.spec.User != "alice" || accessReviews.spec.UID != "alice-uid" || !equalStrings(accessReviews.spec.Groups, []string{"approvers"}) || accessReviews.spec.ResourceAttributes == nil || *accessReviews.spec.ResourceAttributes != wantAttributes {
		t.Fatalf("SubjectAccessReview spec = %+v, want user alice and attributes %+v", accessReviews.spec, wantAttributes)
	}
	if decisions.workspace != "team-a" || decisions.invocationID != invocationID {
		t.Fatalf("decision target = %s/%s, want team-a/%s", decisions.workspace, decisions.invocationID, invocationID)
	}
	if decisions.decision.Decision != api.RuntimeApprovalDecisionApproved || decisions.decision.ExpectedResourceVersion != 7 || decisions.decision.Issuer != KubernetesTokenReviewIssuer || decisions.decision.Subject != "alice" {
		t.Fatalf("decision = %+v, want verified alice approval at resource version 7", decisions.decision)
	}
}

func TestRuntimeApprovalDecisionHandlerFailsClosedBeforeMutation(t *testing.T) {
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
		{name: "evaluation error is unavailable even when allowed", tokenResponse: authenticatedTokenReview(), accessResponse: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true, EvaluationError: "policy unavailable"}}, wantStatus: http.StatusServiceUnavailable},
		{name: "evaluation error without an allow is unavailable", tokenResponse: authenticatedTokenReview(), accessResponse: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{EvaluationError: "policy unavailable"}}, wantStatus: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decisions := &recordingRuntimeApprovalDecisionStore{}
			handler := NewRuntimeApprovalDecisionHandler(
				&recordingTokenReviewer{response: test.tokenResponse, err: test.tokenError},
				&recordingSubjectAccessReviewer{response: test.accessResponse, err: test.accessError},
				decisions,
			)
			response := serveRuntimeApprovalDecision(t, handler, "token-a", map[string]any{
				"namespace": "team-a", "name": name, "invocationID": invocationID,
				"decision": "reject", "expectedResourceVersion": 7,
			})
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if decisions.calls != 0 {
				t.Fatalf("DecideRuntimeApproval calls = %d, want 0", decisions.calls)
			}
		})
	}
}

func TestRuntimeApprovalDecisionHandlerRejectsRuntimeIdentityBeforeAuthorization(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	runtimeUsername := "system:serviceaccount:kognition-system:kognition-tool-gateway"
	tokenReviews := &recordingTokenReviewer{response: &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: runtimeUsername, UID: "gateway-uid"},
	}}}
	accessReviews := &recordingSubjectAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	decisions := &recordingRuntimeApprovalDecisionStore{}
	handler := NewRuntimeApprovalDecisionHandler(tokenReviews, accessReviews, decisions, runtimeUsername)

	response := serveRuntimeApprovalDecision(t, handler, "runtime-token", map[string]any{
		"namespace": "team-a", "name": name, "invocationID": invocationID,
		"decision": "approve", "expectedResourceVersion": 7,
	})

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if accessReviews.calls != 0 {
		t.Fatalf("SubjectAccessReview calls = %d, want 0", accessReviews.calls)
	}
	if decisions.calls != 0 {
		t.Fatalf("DecideRuntimeApproval calls = %d, want 0", decisions.calls)
	}
}

func TestRuntimeApprovalDecisionHandlerMapsSelfApprovalToForbidden(t *testing.T) {
	invocationID := "invocation-a"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	decisions := &recordingRuntimeApprovalDecisionStore{err: store.ErrSelfApproval}
	handler := NewRuntimeApprovalDecisionHandler(
		&recordingTokenReviewer{response: authenticatedTokenReview()},
		&recordingSubjectAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}},
		decisions,
	)
	response := serveRuntimeApprovalDecision(t, handler, "token-a", map[string]any{
		"namespace": "team-a", "name": name, "invocationID": invocationID,
		"decision": "approve", "expectedResourceVersion": 7,
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
	}
}

func authenticatedTokenReview() *authenticationv1.TokenReview {
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: "alice", UID: "alice-uid", Groups: []string{"approvers"}},
	}}
}

func serveRuntimeApprovalDecision(t *testing.T, handler http.Handler, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, RuntimeApprovalDecisionPath, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type recordingTokenReviewer struct {
	response *authenticationv1.TokenReview
	err      error
	token    string
}

func (r *recordingTokenReviewer) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	r.token = review.Spec.Token
	return r.response, r.err
}

type recordingSubjectAccessReviewer struct {
	response *authorizationv1.SubjectAccessReview
	err      error
	calls    int
	spec     authorizationv1.SubjectAccessReviewSpec
}

func (r *recordingSubjectAccessReviewer) Create(_ context.Context, review *authorizationv1.SubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	r.calls++
	r.spec = review.Spec
	return r.response, r.err
}

type recordingRuntimeApprovalDecisionStore struct {
	result       api.RuntimeApproval
	err          error
	calls        int
	workspace    string
	invocationID string
	decision     store.RuntimeApprovalDecision
}

func (s *recordingRuntimeApprovalDecisionStore) DecideRuntimeApproval(_ context.Context, workspace, invocationID string, decision store.RuntimeApprovalDecision) (api.RuntimeApproval, error) {
	s.calls++
	s.workspace = workspace
	s.invocationID = invocationID
	s.decision = decision
	return s.result, s.err
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
