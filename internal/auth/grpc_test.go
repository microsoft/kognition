// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestGRPCUnaryInterceptor(t *testing.T) {
	interceptor := GRPCUnaryInterceptor(NewStaticTokenAuthenticator(map[string]string{"expected": "caller"}), nil)
	handler := func(ctx context.Context, _ any) (any, error) {
		identity, ok := IdentityFromContext(ctx)
		if !ok || identity.Subject != "caller" {
			t.Fatalf("identity = %#v, present = %t", identity, ok)
		}
		return "ok", nil
	}
	_, err := interceptor(context.Background(), nil, nil, handler)
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("missing token status = %s, want %s", got, codes.Unauthenticated)
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer expected"))
	response, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, handler)
	if err != nil || response != "ok" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestGRPCUnaryInterceptorRequiresConfiguredGroup(t *testing.T) {
	authenticator := fakeAuth{id: &Identity{Subject: "caller", Groups: []string{"operators"}, Method: "token"}}
	interceptor := GRPCUnaryInterceptor(authenticator, []string{"admins"})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer token"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		t.Fatal("handler must not run")
		return nil, nil
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("group status = %s, want %s", got, codes.PermissionDenied)
	}
}
