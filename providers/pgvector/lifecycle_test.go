// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"github.com/kognition-ai/kognition/providers/pgvector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestLifecycleRejectsInvalidBindingsBeforeDatabaseAccess(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://unused@127.0.0.1:1/unused?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	spec, err := pgvector.DecodeBackend(validBackend())
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := pgvector.NewLifecycle(pool, spec)
	if err != nil {
		t.Fatal(err)
	}
	scope := queryRequest().Scope
	for name, mutate := range map[string]func(*protocol.KnowledgeScope){
		"namespace":     func(scope *protocol.KnowledgeScope) { scope.Namespace = "" },
		"configuration": func(scope *protocol.KnowledgeScope) { scope.ConfigurationKind = "OtherBackend" },
		"operation":     func(scope *protocol.KnowledgeScope) { scope.OperationId = "foreign" },
		"generation":    func(scope *protocol.KnowledgeScope) { scope.Generation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := proto.Clone(scope).(*protocol.KnowledgeScope)
			mutate(changed)
			if _, err := lifecycle.BeginIndex(context.Background(), changed, scope.OperationId, "sha256:"+strings.Repeat("b", 64)); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("invalid binding reached database access: %v", err)
			}
		})
	}
	if _, err := lifecycle.BeginIndex(context.Background(), nil, scope.OperationId, "sha256:"+strings.Repeat("b", 64)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing scope accepted: %v", err)
	}
	if _, err := lifecycle.BeginIndex(context.Background(), scope, scope.OperationId, "not-a-digest"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid source digest accepted: %v", err)
	}
}
