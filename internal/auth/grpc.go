// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"os"
	"strings"

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
