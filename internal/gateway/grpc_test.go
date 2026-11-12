// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

type grpcTestStore struct {
	objects map[string][]byte
	getErr  error
}

func newGRPCTestStore() *grpcTestStore {
	return &grpcTestStore{objects: make(map[string][]byte)}
}

func (s *grpcTestStore) Ping(context.Context) error { return nil }

func (s *grpcTestStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	value, found := s.objects[kind+"/"+namespace+"/"+name]
	if !found {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (s *grpcTestStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return nil, nil
}

func (s *grpcTestStore) Create(_ context.Context, kind, namespace, name string, value []byte) error {
	key := kind + "/" + namespace + "/" + name
	if _, exists := s.objects[key]; exists {
		return store.ErrAlreadyExists
	}
	s.objects[key] = append([]byte(nil), value...)
	return nil
}

func (s *grpcTestStore) Update(context.Context, string, string, string, []byte, int64) error {
	return nil
}

func (s *grpcTestStore) Delete(context.Context, string, string, string) error { return nil }

func (s *grpcTestStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}

func (s *grpcTestStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}

func (s *grpcTestStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}

func (s *grpcTestStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}

func (s *grpcTestStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}

func (s *grpcTestStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

type grpcTestCredentials string

func (c grpcTestCredentials) ResolveAPIKey(context.Context, string, string) (string, error) {
	return string(c), nil
}

type grpcTestRateLimiter bool

func (l grpcTestRateLimiter) Allow(string) bool { return bool(l) }

func putGRPCTestObject(t *testing.T, st *grpcTestStore, kind, namespace, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	st.objects[kind+"/"+namespace+"/"+name] = data
}

func newGRPCTestClient(t *testing.T, service *Service) intentgatewayv1alpha1.IntentGatewayClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	intentgatewayv1alpha1.RegisterIntentGatewayServer(server, NewGRPCServer(service))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	connection, err := grpc.DialContext(context.Background(), "bufnet", //nolint:staticcheck // Bufconn test relies on blocking grpc 1.x dialing semantics.
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return intentgatewayv1alpha1.NewIntentGatewayClient(connection)
}

func readyGRPCTestGateway(routes []v1alpha1.IntentRoute) v1alpha1.IntentGateway {
	return v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "gateway", Namespace: "default"},
		Spec:       v1alpha1.IntentGatewaySpec{Routes: routes},
		Status:     v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	}
}

func TestGRPCServerSubmitGoalMapsSubmissionErrors(t *testing.T) {
	request := &intentgatewayv1alpha1.SubmitGoalRequest{Namespace: "default", GatewayName: "gateway", Goal: "deploy"}
	matchingRoute := []v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "agent"}}}

	tests := []struct {
		name  string
		setup func(*grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest)
		code  codes.Code
	}{
		{
			name: "invalid request",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				return NewService(st, nil, nil), &intentgatewayv1alpha1.SubmitGoalRequest{}
			},
			code: codes.InvalidArgument,
		},
		{
			name: "gateway not found",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				return NewService(st, nil, nil), request
			},
			code: codes.NotFound,
		},
		{
			name: "gateway unavailable",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				gateway := readyGRPCTestGateway(matchingRoute)
				gateway.Status.Phase = v1alpha1.PhasePending
				putGRPCTestObject(t, st, "IntentGateway", "default", "gateway", gateway)
				return NewService(st, nil, nil), request
			},
			code: codes.Unavailable,
		},
		{
			name: "unauthorized",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				gateway := readyGRPCTestGateway(matchingRoute)
				gateway.Spec.AuthConfig = &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "key"}}
				putGRPCTestObject(t, st, "IntentGateway", "default", "gateway", gateway)
				return NewService(st, grpcTestCredentials("expected"), nil), request
			},
			code: codes.Unauthenticated,
		},
		{
			name: "rate limited",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				putGRPCTestObject(t, st, "IntentGateway", "default", "gateway", readyGRPCTestGateway(matchingRoute))
				putGRPCTestObject(t, st, "Agent", "default", "agent", v1alpha1.Agent{})
				return NewService(st, nil, grpcTestRateLimiter(false)), request
			},
			code: codes.ResourceExhausted,
		},
		{
			name: "no route",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				putGRPCTestObject(t, st, "IntentGateway", "default", "gateway", readyGRPCTestGateway(nil))
				return NewService(st, nil, nil), request
			},
			code: codes.FailedPrecondition,
		},
		{
			name: "internal failure",
			setup: func(st *grpcTestStore) (*Service, *intentgatewayv1alpha1.SubmitGoalRequest) {
				st.getErr = errors.New("storage failure")
				return NewService(st, nil, nil), request
			},
			code: codes.Internal,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newGRPCTestStore()
			service, rpcRequest := test.setup(st)
			_, err := newGRPCTestClient(t, service).SubmitGoal(context.Background(), rpcRequest)
			if got := status.Code(err); got != test.code {
				t.Fatalf("status code = %s, want %s; error = %v", got, test.code, err)
			}
		})
	}
}

func TestGRPCServerSubmitGoalUsesConfiguredAPIKeyHeader(t *testing.T) {
	st := newGRPCTestStore()
	gateway := readyGRPCTestGateway([]v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "agent"}}})
	gateway.Spec.AuthConfig = &v1alpha1.IntentGatewayAuthConfig{
		Mode:            "apiKey",
		Header:          "X-Custom-API-Key",
		APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "key"},
	}
	putGRPCTestObject(t, st, "IntentGateway", "default", "gateway", gateway)
	putGRPCTestObject(t, st, "Agent", "default", "agent", v1alpha1.Agent{})
	client := newGRPCTestClient(t, NewService(st, grpcTestCredentials("expected"), nil))

	valid, err := client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{
		Namespace: "default", GatewayName: "gateway", Goal: "deploy", ApiKey: "expected", ApiKeyHeader: "X-Custom-API-Key",
	})
	if err != nil {
		t.Fatalf("SubmitGoal() error = %v", err)
	}
	if valid.GetSolutionName() == "" || valid.GetSolutionNamespace() != "default" {
		t.Fatalf("response = %#v", valid)
	}

	_, err = client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{
		Namespace: "default", GatewayName: "gateway", Goal: "deploy", ApiKey: "expected", ApiKeyHeader: "X-Caller-Selected-Key",
	})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("status code = %s, want %s; error = %v", got, codes.Unauthenticated, err)
	}
}
