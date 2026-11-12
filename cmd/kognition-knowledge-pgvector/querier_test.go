// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	pgvectorprovider "github.com/kognition-ai/kognition/providers/pgvector"
)

// querierFixture provides a valid PgvectorBackend configuration + committed
// scope in a fake client. Tests reach only into the pre-pool code paths
// (validation, reader lookup, spec decoding); actually opening a pool would
// require a running database and is exercised by the pgvector provider's own
// integration tests.
type querierFixture struct {
	configuration *unstructured.Unstructured
	scope         *protocol.KnowledgeScope
	reader        client.Reader
}

func newQuerierFixture(t *testing.T, withConfiguration bool) querierFixture {
	t.Helper()
	kind := schema.GroupVersionKind{Group: pgvectorprovider.APIGroup, Version: pgvectorprovider.APIVersion, Kind: pgvectorprovider.Kind}
	configuration := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"host":                "pgvector.tenant-a.svc.cluster.local",
			"port":                int64(5432),
			"database":            "kb",
			"table":               "chunks",
			"embeddingDimensions": int64(3),
			"credentialsSecretRef": map[string]interface{}{
				"name":        "database",
				"usernameKey": "user",
				"passwordKey": "pass",
			},
			"tls": map[string]interface{}{"mode": "verify-full"},
		},
	}}
	configuration.SetGroupVersionKind(kind)
	configuration.SetNamespace("tenant-a")
	configuration.SetName("shared")
	configuration.SetUID("config-uid")
	digest, err := knowledge.ConfigurationDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}

	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if withConfiguration {
		builder = builder.WithObjects(configuration)
	}
	return querierFixture{
		configuration: configuration,
		reader:        builder.Build(),
		scope: &protocol.KnowledgeScope{
			Namespace:             "tenant-a",
			KnowledgeBaseUid:      "kb-uid",
			Generation:            3,
			OperationId:           "index-3",
			ConfigurationApiGroup: kind.Group,
			ConfigurationKind:     kind.Kind,
			ConfigurationName:     "shared",
			ConfigurationUid:      "config-uid",
			ConfigurationDigest:   digest,
		},
	}
}

func newQuery(scope *protocol.KnowledgeScope) *protocol.KnowledgeQueryRequest {
	return &protocol.KnowledgeQueryRequest{Scope: scope, RequestId: "q-1", Mode: "dense", Vector: []float64{1, 0, 0}, TopK: 1}
}

func TestPgvectorQuerierQueryRejectsMissingInputs(t *testing.T) {
	q := newPgvectorQuerier(fake.NewClientBuilder().Build(), zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")
	defer q.Close()

	if _, err := q.Query(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil request must be InvalidArgument, got %v", err)
	}
	if _, err := q.Query(context.Background(), &protocol.KnowledgeQueryRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("request with nil scope must be InvalidArgument, got %v", err)
	}
}

func TestPgvectorQuerierRejectsForeignConfiguration(t *testing.T) {
	fixture := newQuerierFixture(t, true)
	q := newPgvectorQuerier(fixture.reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")
	defer q.Close()

	// Wrong API group.
	scope := proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	scope.ConfigurationApiGroup = "chroma.knowledge.kognition.dev"
	if _, err := q.Query(context.Background(), newQuery(scope)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("foreign apiGroup must be FailedPrecondition, got %v", err)
	}

	// Wrong Kind.
	scope = proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	scope.ConfigurationKind = "OtherBackend"
	if _, err := q.Query(context.Background(), newQuery(scope)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("foreign kind must be FailedPrecondition, got %v", err)
	}
}

func TestPgvectorQuerierMissingConfigurationIsFailedPrecondition(t *testing.T) {
	fixture := newQuerierFixture(t, false)
	q := newPgvectorQuerier(fixture.reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")
	defer q.Close()

	if _, err := q.Query(context.Background(), newQuery(fixture.scope)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing configuration must be FailedPrecondition, got %v", err)
	}
}

func TestPgvectorQuerierInvalidConfigurationIsFailedPrecondition(t *testing.T) {
	fixture := newQuerierFixture(t, false)
	// Rebuild the reader with a configuration whose spec cannot decode.
	broken := fixture.configuration.DeepCopy()
	// Empty spec → DecodeBackend Validate() rejects.
	if err := unstructured.SetNestedMap(broken.Object, map[string]interface{}{}, "spec"); err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(broken).Build()
	q := newPgvectorQuerier(reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")
	defer q.Close()

	if _, err := q.Query(context.Background(), newQuery(fixture.scope)); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("invalid spec must be FailedPrecondition, got %v", err)
	}
}

func TestPgvectorQuerierCloseIsIdempotentAndRejectsSubsequentQueries(t *testing.T) {
	fixture := newQuerierFixture(t, true)
	q := newPgvectorQuerier(fixture.reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")

	q.Close()
	// Second close must not panic and must leave the querier closed.
	q.Close()

	// Any further Query must be surfaced as Unavailable by acquire, even for
	// otherwise well-formed scopes.
	if _, err := q.Query(context.Background(), newQuery(fixture.scope)); status.Code(err) != codes.Unavailable {
		t.Fatalf("Query after Close must be Unavailable, got %v", err)
	}
}

func TestPgvectorQuerierAcquireHitsCachedEntry(t *testing.T) {
	// This test bypasses build() by pre-populating the cache directly.
	// A real *pgxpool.Pool is not required because Close is never called on
	// the entry in this test (the querier is not closed), and acquire only
	// dereferences entries[key].
	fixture := newQuerierFixture(t, true)
	q := newPgvectorQuerier(fixture.reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")

	stub := &querierEntry{} // pool/querier stay nil; only pointer identity matters
	q.entries[scopeKey(fixture.scope)] = stub

	entry, err := q.acquire(context.Background(), fixture.scope)
	if err != nil {
		t.Fatalf("acquire on pre-populated cache returned %v", err)
	}
	if entry != stub {
		t.Fatalf("acquire returned a fresh entry instead of the cached one")
	}

	// A distinct scope must miss the cache and go to build (which will fail
	// with FailedPrecondition because the configuration name does not
	// resolve). This proves the cache key discriminates on scope fields.
	other := proto.Clone(fixture.scope).(*protocol.KnowledgeScope)
	other.ConfigurationName = "does-not-exist"
	if _, err := q.acquire(context.Background(), other); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("cache miss must fall through to build, got %v", err)
	}
	// Failing builds must not populate the cache.
	if _, ok := q.entries[scopeKey(other)]; ok {
		t.Fatalf("failed build should not cache an entry")
	}
}

func TestScopeKeyDistinguishesEveryField(t *testing.T) {
	base := &protocol.KnowledgeScope{
		Namespace:             "tenant-a",
		KnowledgeBaseUid:      "kb-uid",
		Generation:            3,
		ConfigurationApiGroup: pgvectorprovider.APIGroup,
		ConfigurationKind:     pgvectorprovider.Kind,
		ConfigurationName:     "shared",
		ConfigurationUid:      "config-uid",
		ConfigurationDigest:   "digest-a",
	}
	baseKey := scopeKey(base)

	// scopeKey must be stable for identical input.
	if scopeKey(proto.Clone(base).(*protocol.KnowledgeScope)) != baseKey {
		t.Fatalf("scopeKey must be stable for identical scopes")
	}

	mutations := map[string]func(*protocol.KnowledgeScope){
		"Namespace":             func(s *protocol.KnowledgeScope) { s.Namespace = "tenant-b" },
		"KnowledgeBaseUid":      func(s *protocol.KnowledgeScope) { s.KnowledgeBaseUid = "kb-other" },
		"Generation":            func(s *protocol.KnowledgeScope) { s.Generation = 4 },
		"ConfigurationApiGroup": func(s *protocol.KnowledgeScope) { s.ConfigurationApiGroup = "other.group" },
		"ConfigurationKind":     func(s *protocol.KnowledgeScope) { s.ConfigurationKind = "OtherKind" },
		"ConfigurationName":     func(s *protocol.KnowledgeScope) { s.ConfigurationName = "renamed" },
		"ConfigurationUid":      func(s *protocol.KnowledgeScope) { s.ConfigurationUid = "config-other" },
		"ConfigurationDigest":   func(s *protocol.KnowledgeScope) { s.ConfigurationDigest = "digest-b" },
	}
	for name, mutate := range mutations {
		clone := proto.Clone(base).(*protocol.KnowledgeScope)
		mutate(clone)
		if scopeKey(clone) == baseKey {
			t.Fatalf("scopeKey must differ when %s changes", name)
		}
	}
}

// TestPgvectorQuerierBuildFailsWhenSecretMissing drives build() past
// DecodeBackend into providers/pgvector.LoadConnectionMaterial so the reader
// is consulted for the credentials Secret. When the Secret is absent the
// adapter must surface FailedPrecondition (never Internal) and must not
// populate the entry cache.
func TestPgvectorQuerierBuildFailsWhenSecretMissing(t *testing.T) {
	fixture := newQuerierFixture(t, false)
	// Register both the kognition types and core/v1 so the fake client can
	// service the Secret Get with a proper NotFound.
	scheme := runtime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(fixture.configuration).Build()
	q := newPgvectorQuerier(reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")
	defer q.Close()

	_, err := q.Query(context.Background(), newQuery(fixture.scope))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing Secret must yield FailedPrecondition, got %v", err)
	}
	if _, ok := q.entries[scopeKey(fixture.scope)]; ok {
		t.Fatalf("failed build must not cache an entry")
	}

	// Now provide the Secret but leave the credentials empty so the
	// downstream validation (utf8/length) inside LoadConnectionMaterial
	// rejects it. This exercises a distinct branch: the Secret is present
	// but its payload fails the material check.
	badSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: fixture.configuration.GetNamespace(), Name: "database"},
		Data:       map[string][]byte{"user": []byte(""), "pass": []byte("")},
	}
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(fixture.configuration, badSecret).Build()
	q2 := newPgvectorQuerier(reader, zap.NewNop(), "pgvector.tenant-a.svc.cluster.local:5432")
	defer q2.Close()

	_, err = q2.Query(context.Background(), newQuery(fixture.scope))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("empty credentials must yield FailedPrecondition, got %v", err)
	}
}
