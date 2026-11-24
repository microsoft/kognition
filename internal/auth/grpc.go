// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"os"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCUnaryInterceptor applies the same bearer authentication and group policy
// used by the HTTP middleware to unary gRPC methods.
func GRPCUnaryInterceptor(authenticator Authenticator, requiredGroups []string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if authenticator == nil {
			return handler(ctx, request)
		}
		identity, err := authenticator.Authenticate(ctx, grpcBearerToken(ctx))
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "authentication required")
		}
		if len(requiredGroups) > 0 && !hasAnyGroup(identity.Groups, requiredGroups) {
			return nil, status.Error(codes.PermissionDenied, "forbidden: caller lacks a required group")
		}
		return handler(WithIdentity(ctx, identity), request)
	}
}

func WorkflowExecutorGRPCUnaryInterceptor(authenticator Authenticator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info == nil || (info.FullMethod != "/lf.a2a.v1.A2AService/SendMessage" && info.FullMethod != "/lf.a2a.v1.A2AService/GetTask") {
			return nil, status.Error(codes.Unimplemented, "unsupported Workflow execution method")
		}
		if authenticator == nil || handler == nil {
			return nil, status.Error(codes.Unavailable, "Workflow execution authentication unavailable")
		}
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		token, ok := strictGRPCBearerToken(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "Workflow executor authentication required")
		}
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		identity, err := authenticator.Authenticate(bounded, token)
		if err != nil || identity == nil {
			return nil, status.Error(codes.Unauthenticated, "Workflow executor authentication required")
		}
		if _, err := AuthenticatedWorkflowControllerUID(WithIdentity(bounded, identity)); err != nil {
			return nil, status.Error(codes.Unauthenticated, "Workflow executor authentication required")
		}
		if err := bounded.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		return handler(WithIdentity(ctx, identity), request)
	}
}

func WorkflowExecutorGRPCServerOptions(authenticator Authenticator) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(WorkflowExecutorGRPCUnaryInterceptor(authenticator)),
		grpc.ChainStreamInterceptor(func(_ any, _ grpc.ServerStream, _ *grpc.StreamServerInfo, _ grpc.StreamHandler) error {
			return status.Error(codes.Unimplemented, "Workflow execution streaming is unsupported")
		}),
	}
}

// NewEnvironmentAuthenticator builds a chain from the KOGNITION_AUTH_*,
// KOGNITION_OIDC_*, and KOGNITION_BOOTSTRAP_* environment variables.
func NewEnvironmentAuthenticator(ctx context.Context) (Authenticator, []string, error) {
	var chain Chain
	if token := os.Getenv("KOGNITION_BOOTSTRAP_TOKEN"); token != "" {
		subject := os.Getenv("KOGNITION_BOOTSTRAP_SUBJECT")
		if subject == "" {
			subject = "bootstrap"
		}
		chain = append(chain, NewStaticTokenAuthenticator(map[string]string{token: subject}))
	}
	if issuer := os.Getenv("KOGNITION_OIDC_ISSUER"); issuer != "" {
		oidcAuthenticator, err := NewOIDCAuthenticator(ctx, []ProviderConfig{{
			IssuerURL:     issuer,
			Audience:      os.Getenv("KOGNITION_OIDC_AUDIENCE"),
			UsernameClaim: os.Getenv("KOGNITION_OIDC_USERNAME_CLAIM"),
			GroupsClaim:   os.Getenv("KOGNITION_OIDC_GROUPS_CLAIM"),
		}})
		if err != nil {
			return nil, nil, err
		}
		chain = append(chain, oidcAuthenticator)
	}
	if len(chain) == 0 {
		return nil, nil, ErrNoToken
	}
	return chain, splitGroups(os.Getenv("KOGNITION_OIDC_REQUIRED_GROUPS")), nil
}

// strictGRPCBearerToken accepts exactly one bounded Bearer credential without whitespace or control characters.
func strictGRPCBearerToken(ctx context.Context) (string, bool) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 || len(values[0]) > (64<<10)+7 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > 64<<10 || strings.ContainsFunc(token, func(character rune) bool {
		return unicode.IsSpace(character) || unicode.IsControl(character)
	}) {
		return "", false
	}
	return token, true
}

func grpcBearerToken(ctx context.Context) string {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) == 0 {
		return ""
	}
	const prefix = "Bearer "
	if len(values[0]) <= len(prefix) || !strings.EqualFold(values[0][:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(values[0][len(prefix):])
}

func splitGroups(value string) []string {
	groups := make([]string, 0)
	for _, group := range strings.Split(value, ",") {
		if group = strings.TrimSpace(group); group != "" {
			groups = append(groups, group)
		}
	}
	return groups
}
