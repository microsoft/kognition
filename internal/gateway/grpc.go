// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// GRPCServer adapts IntentGateway gRPC requests to the shared submission service.
type GRPCServer struct {
	intentgatewayv1alpha1.UnimplementedIntentGatewayServer
	service *Service
}

// NewGRPCServer creates an IntentGateway gRPC adapter backed by service.
func NewGRPCServer(service *Service) *GRPCServer {
	return &GRPCServer{service: service}
}

// SubmitGoal validates, authorizes, routes, and persists a goal submission.
func (s *GRPCServer) SubmitGoal(ctx context.Context, request *intentgatewayv1alpha1.SubmitGoalRequest) (*intentgatewayv1alpha1.SubmitGoalResponse, error) {
	response, submissionErr := s.service.SubmitGoal(ctx, SubmissionRequest{
		Namespace:   request.GetNamespace(),
		GatewayName: request.GetGatewayName(),
		Submission:  GoalSubmission{Goal: request.GetGoal(), Constraints: request.GetConstraints()},
		HeaderValue: func(configuredHeader string) string {
			if request.GetApiKeyHeader() != "" && !strings.EqualFold(request.GetApiKeyHeader(), configuredHeader) {
				return ""
			}
			return request.GetApiKey()
		},
	})
	if submissionErr != nil {
		return nil, grpcSubmissionError(submissionErr)
	}
	return &intentgatewayv1alpha1.SubmitGoalResponse{
		SolutionName:      response.SolutionRef.Name,
		SolutionNamespace: response.SolutionRef.Namespace,
	}, nil
}

func grpcSubmissionError(submissionErr *SubmissionError) error {
	code := codes.Internal
	switch submissionErr.Kind {
	case ErrorInvalidRequest:
		code = codes.InvalidArgument
	case ErrorNotFound:
		code = codes.NotFound
	case ErrorUnavailable:
		code = codes.Unavailable
	case ErrorUnauthorized:
		code = codes.Unauthenticated
	case ErrorRateLimited:
		code = codes.ResourceExhausted
	case ErrorNoRoute:
		code = codes.FailedPrecondition
	case ErrorInternal:
		code = codes.Internal
	}
	return status.Error(code, submissionErr.Message)
}
