// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	KubernetesTokenReviewIssuer = "kubernetes.io/tokenreview"
	RuntimeApprovalDecisionPath = "/runtimeapprovals/decision"
)

type tokenReviewer interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

type subjectAccessReviewer interface {
	Create(context.Context, *authorizationv1.SubjectAccessReview, metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error)
}

type runtimeApprovalDecisionStore interface {
	DecideRuntimeApproval(context.Context, string, string, store.RuntimeApprovalDecision) (api.RuntimeApproval, error)
}

type RuntimeApprovalDecisionHandler struct {
	tokenReviews      tokenReviewer
	accessReviews     subjectAccessReviewer
	store             runtimeApprovalDecisionStore
	forbiddenSubjects map[string]struct{}
}

type runtimeApprovalDecisionRequest struct {
	Namespace               string `json:"namespace"`
	Name                    string `json:"name"`
	InvocationID            string `json:"invocationID"`
	Decision                string `json:"decision"`
	ExpectedResourceVersion int64  `json:"expectedResourceVersion"`
}

func NewRuntimeApprovalDecisionHandler(tokenReviews tokenReviewer, accessReviews subjectAccessReviewer, decisionStore runtimeApprovalDecisionStore, forbiddenSubjects ...string) *RuntimeApprovalDecisionHandler {
	forbidden := make(map[string]struct{}, len(forbiddenSubjects))
	for _, subject := range forbiddenSubjects {
		if subject = strings.TrimSpace(subject); subject != "" {
			forbidden[subject] = struct{}{}
		}
	}
	return &RuntimeApprovalDecisionHandler{tokenReviews: tokenReviews, accessReviews: accessReviews, store: decisionStore, forbiddenSubjects: forbidden}
}

func (h *RuntimeApprovalDecisionHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.Header().Set("Allow", http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input runtimeApprovalDecisionRequest
	raw, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		http.Error(response, "invalid decision request", http.StatusBadRequest)
		return
	}
	canonical, err := runtimeapproval.CanonicalizeJSON(raw)
	if err != nil {
		http.Error(response, "invalid decision request", http.StatusBadRequest)
		return
	}
	decoder := json.NewDecoder(strings.NewReader(string(canonical)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(response, "invalid decision request", http.StatusBadRequest)
		return
	}
	if input.Namespace == "" || input.Name == "" || input.InvocationID == "" || input.ExpectedResourceVersion <= 0 || input.Name != runtimeapproval.CanonicalName(input.Namespace, input.InvocationID) {
		http.Error(response, "invalid RuntimeApproval target", http.StatusBadRequest)
		return
	}

	decision, verb := api.RuntimeApprovalDecisionRejected, input.Decision
	if input.Decision == "approve" {
		decision = api.RuntimeApprovalDecisionApproved
	} else if input.Decision != "reject" {
		http.Error(response, "decision must be approve or reject", http.StatusBadRequest)
		return
	}

	token := bearerToken(request.Header.Get("Authorization"))
	if token == "" {
		http.Error(response, "authentication required", http.StatusUnauthorized)
		return
	}
	tokenReview, err := h.tokenReviews.Create(request.Context(), &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{Token: token}}, metav1.CreateOptions{})
	if err != nil {
		http.Error(response, "authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	if tokenReview == nil || !tokenReview.Status.Authenticated || tokenReview.Status.User.Username == "" {
		http.Error(response, "invalid credentials", http.StatusUnauthorized)
		return
	}

	user := tokenReview.Status.User
	if _, forbidden := h.forbiddenSubjects[user.Username]; forbidden {
		http.Error(response, "forbidden", http.StatusForbidden)
		return
	}
	accessReview, err := h.accessReviews.Create(request.Context(), &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: user.Username, UID: user.UID, Groups: user.Groups, Extra: authorizationExtra(user.Extra),
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: input.Namespace, Verb: verb, Group: "kognition.dev", Resource: "runtimeapprovals", Subresource: "decision", Name: input.Name,
		},
	}}, metav1.CreateOptions{})
	if err != nil || accessReview == nil || accessReview.Status.EvaluationError != "" {
		http.Error(response, "authorization unavailable", http.StatusServiceUnavailable)
		return
	}
	if !accessReview.Status.Allowed || accessReview.Status.Denied {
		http.Error(response, "forbidden", http.StatusForbidden)
		return
	}

	approval, err := h.store.DecideRuntimeApproval(request.Context(), input.Namespace, input.InvocationID, store.RuntimeApprovalDecision{
		Decision: decision, ExpectedResourceVersion: input.ExpectedResourceVersion,
		Issuer: KubernetesTokenReviewIssuer, Subject: user.Username,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			http.Error(response, "RuntimeApproval not found", http.StatusNotFound)
		case errors.Is(err, store.ErrConflict):
			http.Error(response, "RuntimeApproval conflict", http.StatusConflict)
		default:
			http.Error(response, "decision failed", http.StatusInternalServerError)
		}
		return
	}
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(approval); err != nil {
		return
	}
}

func bearerToken(value string) string {
	scheme, token, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) != token || token == "" {
		return ""
	}
	return token
}

func authorizationExtra(extra map[string]authenticationv1.ExtraValue) map[string]authorizationv1.ExtraValue {
	converted := make(map[string]authorizationv1.ExtraValue, len(extra))
	for key, values := range extra {
		converted[key] = authorizationv1.ExtraValue(values)
	}
	return converted
}
