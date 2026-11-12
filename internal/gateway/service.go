// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ErrCredentialUnavailable indicates that the configured credential cannot be
// resolved by the deployment-specific credential backend.
var ErrCredentialUnavailable = errors.New("gateway credential unavailable")

// APIKeyResolver resolves the expected API key for an IntentGateway SecretRef.
type APIKeyResolver interface {
	ResolveAPIKey(context.Context, string, string) (string, error)
}

// SubmissionRateLimiter admits an authenticated goal submission for a gateway key.
type SubmissionRateLimiter interface {
	Allow(string) bool
}

// GoalSubmission is the input accepted by an IntentGateway.
type GoalSubmission struct {
	Goal        string   `json:"goal"`
	Constraints []string `json:"constraints,omitempty"`
}

// GoalResponse identifies the Solution created for a goal submission.
type GoalResponse struct {
	SolutionRef SolutionReference `json:"solutionRef"`
}

// SolutionReference identifies a Solution resource.
type SolutionReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// SubmissionRequest contains transport-neutral goal-submission input.
type SubmissionRequest struct {
	Namespace   string
	GatewayName string
	Submission  GoalSubmission
	HeaderValue func(string) string
}

// ErrorKind classifies failures so transport adapters can preserve their API
// contract without reimplementing submission behavior.
type ErrorKind string

const (
	ErrorInvalidRequest ErrorKind = "InvalidRequest"
	ErrorNotFound       ErrorKind = "NotFound"
	ErrorUnavailable    ErrorKind = "Unavailable"
	ErrorUnauthorized   ErrorKind = "Unauthorized"
	ErrorRateLimited    ErrorKind = "RateLimited"
	ErrorNoRoute        ErrorKind = "NoRoute"
	ErrorInternal       ErrorKind = "Internal"
)

// SubmissionError is a classified goal-submission failure.
type SubmissionError struct {
	Kind    ErrorKind
	Message string
	Err     error
}

func (e *SubmissionError) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Err)
}

// Service executes the IntentGateway goal-submission contract.
type Service struct {
	store       store.Store
	credentials APIKeyResolver
	rateLimiter SubmissionRateLimiter
}

// NewService creates a shared IntentGateway submission service.
func NewService(st store.Store, credentials APIKeyResolver, rateLimiter SubmissionRateLimiter) *Service {
	return &Service{store: st, credentials: credentials, rateLimiter: rateLimiter}
}

// SubmitGoal validates, authenticates, routes, and persists a Solution for a
// goal submission. Authentication always precedes rate limiting.
func (s *Service) SubmitGoal(ctx context.Context, request SubmissionRequest) (*GoalResponse, *SubmissionError) {
	if strings.TrimSpace(request.Submission.Goal) == "" {
		return nil, submissionError(ErrorInvalidRequest, "goal is required and cannot be empty", nil)
	}

	rawGateway, err := s.store.Get(ctx, "IntentGateway", request.Namespace, request.GatewayName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, submissionError(ErrorNotFound, fmt.Sprintf("IntentGateway %q not found in namespace %q", request.GatewayName, request.Namespace), err)
		}
		return nil, submissionError(ErrorInternal, "internal error", err)
	}
	var intentGateway v1alpha1.IntentGateway
	if err := json.Unmarshal(rawGateway, &intentGateway); err != nil {
		return nil, submissionError(ErrorInternal, "internal error", err)
	}
	if intentGateway.Status.Phase != v1alpha1.PhaseReady {
		return nil, submissionError(ErrorUnavailable, fmt.Sprintf("IntentGateway %q is not ready (phase: %s)", request.GatewayName, intentGateway.Status.Phase), nil)
	}
	if submissionErr := s.authorize(ctx, request, &intentGateway); submissionErr != nil {
		return nil, submissionErr
	}
	if s.rateLimiter != nil && !s.rateLimiter.Allow(request.Namespace+"/"+request.GatewayName) {
		return nil, submissionError(ErrorRateLimited, "rate limit exceeded for gateway", nil)
	}

	route := MatchRoute(intentGateway.Spec.Routes, request.Submission.Goal)
	if route == nil {
		return nil, submissionError(ErrorNoRoute, "no route matched the submitted goal", nil)
	}
	target, err := s.store.Get(ctx, route.TargetKind, request.Namespace, route.TargetRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, submissionError(ErrorUnavailable, "matched route target is unavailable", err)
		}
		return nil, submissionError(ErrorInternal, "internal error", err)
	}
	if route.TargetKind == "AgentService" {
		var service v1alpha1.AgentService
		if err := json.Unmarshal(target, &service); err != nil {
			return nil, submissionError(ErrorInternal, "internal error", err)
		}
		if service.Status.Phase != v1alpha1.PhaseReady || service.Status.ReadyInstances < 1 {
			return nil, submissionError(ErrorUnavailable, "matched AgentService target is unavailable", nil)
		}
	}

	solutionName := fmt.Sprintf("sol-%s", uuid.New().String()[:8])
	solution := v1alpha1.Solution{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Solution"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              solutionName,
			Namespace:         request.Namespace,
			UID:               uuid.New().String(),
			ResourceVersion:   1,
			CreationTimestamp: time.Now().UTC(),
			Labels: map[string]string{
				"kognition.dev/gateway":     request.GatewayName,
				"kognition.dev/target-kind": route.TargetKind,
				"kognition.dev/target-name": route.TargetRef.Name,
			},
		},
		Spec: v1alpha1.SolutionSpec{
			Goal:        request.Submission.Goal,
			Constraints: request.Submission.Constraints,
			Target:      &v1alpha1.SolutionTarget{Kind: route.TargetKind, Ref: route.TargetRef},
		},
		Status: v1alpha1.SolutionStatus{Phase: v1alpha1.PhasePending},
	}
	data, err := json.Marshal(solution)
	if err != nil {
		return nil, submissionError(ErrorInternal, "internal error", err)
	}
	if err := s.store.Create(ctx, "Solution", request.Namespace, solutionName, data); err != nil {
		return nil, submissionError(ErrorInternal, "failed to create solution", err)
	}
	return &GoalResponse{SolutionRef: SolutionReference{Name: solutionName, Namespace: request.Namespace}}, nil
}

func (s *Service) authorize(ctx context.Context, request SubmissionRequest, intentGateway *v1alpha1.IntentGateway) *SubmissionError {
	auth := intentGateway.Spec.AuthConfig
	if auth == nil || auth.Mode == "" || auth.Mode == "none" {
		return nil
	}
	if auth.Mode != "apiKey" || auth.APIKeySecretRef == nil || auth.APIKeySecretRef.Name == "" || s.credentials == nil {
		return submissionError(ErrorUnavailable, "IntentGateway authentication is not configured", nil)
	}
	expected, err := s.credentials.ResolveAPIKey(ctx, request.Namespace, auth.APIKeySecretRef.Name)
	if err != nil {
		if errors.Is(err, ErrCredentialUnavailable) {
			return submissionError(ErrorUnavailable, "IntentGateway authentication secret is unavailable", err)
		}
		return submissionError(ErrorInternal, "internal error", err)
	}
	if expected == "" {
		return submissionError(ErrorUnavailable, "IntentGateway authentication secret is unavailable", nil)
	}
	header := auth.Header
	if header == "" {
		header = "X-Kognition-API-Key"
	}
	provided := ""
	if request.HeaderValue != nil {
		provided = request.HeaderValue(header)
	}
	if len(provided) == 0 || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return submissionError(ErrorUnauthorized, "invalid API key", nil)
	}
	return nil
}

func submissionError(kind ErrorKind, message string, err error) *SubmissionError {
	return &SubmissionError{Kind: kind, Message: message, Err: err}
}

// MatchRoute finds the first route whose pattern matches the goal. Patterns
// containing glob metacharacters use path.Match; plain patterns use a
// case-insensitive substring match for backwards compatibility.
func MatchRoute(routes []v1alpha1.IntentRoute, goal string) *v1alpha1.IntentRoute {
	goalLower := strings.ToLower(strings.TrimSpace(goal))
	for index := range routes {
		pattern := strings.ToLower(strings.TrimSpace(routes[index].Pattern))
		if pattern == "" {
			continue
		}
		matched := strings.Contains(goalLower, pattern)
		if strings.ContainsAny(pattern, "*?[") {
			var err error
			matched, err = path.Match(pattern, goalLower)
			if err != nil {
				continue
			}
		}
		if matched {
			return &routes[index]
		}
	}
	return nil
}
