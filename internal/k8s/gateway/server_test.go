// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	coregateway "github.com/kognition-ai/kognition/internal/gateway"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/store"
	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

type testStore struct {
	store.Store
	objects map[string][]byte
	pingErr error
}

func newTestStore() *testStore {
	return &testStore{objects: make(map[string][]byte)}
}

func (s *testStore) Ping(context.Context) error { return s.pingErr }

func (s *testStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	value, found := s.objects[kind+"/"+namespace+"/"+name]
	if !found {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), value...), nil
}

func (s *testStore) Create(_ context.Context, kind, namespace, name string, value []byte) error {
	key := kind + "/" + namespace + "/" + name
	if _, exists := s.objects[key]; exists {
		return store.ErrAlreadyExists
	}
	s.objects[key] = append([]byte(nil), value...)
	return nil
}

func putObject(t *testing.T, st *testStore, kind, namespace, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	st.objects[kind+"/"+namespace+"/"+name] = data
}

func TestServerSubmitGoalCreatesTargetedSolution(t *testing.T) {
	st := newTestStore()
	putObject(t, st, "IntentGateway", "tenant-a", "support", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support", Namespace: "tenant-a"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			Pattern: "deploy", TargetKind: "Workflow", TargetRef: v1alpha1.LocalObjectReference{Name: "release"},
		}}},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})
	putObject(t, st, "Workflow", "tenant-a", "release", v1alpha1.Workflow{ObjectMeta: v1alpha1.ObjectMeta{Name: "release", Namespace: "tenant-a"}})
	server := New(Config{}, st, zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals", strings.NewReader(`{"goal":"Deploy the release"}`))
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var response coregateway.GoalResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	solutionData, err := st.Get(context.Background(), "Solution", "tenant-a", response.SolutionRef.Name)
	if err != nil {
		t.Fatal(err)
	}
	var solution v1alpha1.Solution
	if err := json.Unmarshal(solutionData, &solution); err != nil {
		t.Fatal(err)
	}
	if solution.Spec.Target == nil || solution.Spec.Target.Kind != "Workflow" || solution.Spec.Target.Ref.Name != "release" {
		t.Fatalf("solution target = %#v", solution.Spec.Target)
	}
}

func TestServerSubmitGoalCreatesAgentServiceTargetedSolution(t *testing.T) {
	st := newTestStore()
	putObject(t, st, "AgentService", "tenant-a", "planner-service", v1alpha1.AgentService{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-service", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
		Status:     v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1},
	})
	putObject(t, st, "IntentGateway", "tenant-a", "support", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support", Namespace: "tenant-a"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			Pattern: "*", TargetKind: "AgentService", TargetRef: v1alpha1.LocalObjectReference{Name: "planner-service"},
		}}},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})
	server := New(Config{}, st, zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals", strings.NewReader(`{"goal":"Deploy the release"}`))
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var response coregateway.GoalResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	solutionData, err := st.Get(context.Background(), "Solution", "tenant-a", response.SolutionRef.Name)
	if err != nil {
		t.Fatal(err)
	}
	var solution v1alpha1.Solution
	if err := json.Unmarshal(solutionData, &solution); err != nil {
		t.Fatal(err)
	}
	if solution.Spec.Target == nil || solution.Spec.Target.Kind != "AgentService" || solution.Spec.Target.Ref.Name != "planner-service" {
		t.Fatalf("solution target = %#v", solution.Spec.Target)
	}
}

func TestServerHealthAndReadinessReflectStoreState(t *testing.T) {
	st := newTestStore()
	server := New(Config{}, st, zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()

	for _, test := range []struct {
		path       string
		pingErr    error
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusOK, wantBody: "ok"},
		{path: "/readyz", wantStatus: http.StatusOK, wantBody: "ok"},
		{path: "/readyz", pingErr: errors.New("store unavailable"), wantStatus: http.StatusServiceUnavailable, wantBody: "not ready"},
	} {
		t.Run(test.path+"/"+http.StatusText(test.wantStatus), func(t *testing.T) {
			st.pingErr = test.pingErr
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), test.wantBody) {
				t.Fatalf("GET %s = (%d, %q), want (%d, body containing %q)", test.path, recorder.Code, recorder.Body.String(), test.wantStatus, test.wantBody)
			}
		})
	}
}

func TestServerRejectsMalformedGoalSubmissions(t *testing.T) {
	server := New(Config{}, newTestStore(), zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()
	const path = "/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals"
	for _, test := range []struct {
		name        string
		body        string
		wantMessage string
	}{
		{name: "missing body", wantMessage: "request body is required"},
		{name: "invalid JSON", body: "{", wantMessage: "invalid JSON body"},
		{name: "blank goal", body: `{"goal":" "}`, wantMessage: "goal is required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.body)))
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.wantMessage) {
				t.Fatalf("POST = (%d, %s), want 400 containing %q", recorder.Code, recorder.Body.String(), test.wantMessage)
			}
		})
	}
}

func TestServerMapsSubmissionErrorsToHTTPResponses(t *testing.T) {
	server := New(Config{}, newTestStore(), zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()
	for _, test := range []struct {
		kind             coregateway.ErrorKind
		wantStatus       int
		wantAuthenticate bool
	}{
		{kind: coregateway.ErrorInvalidRequest, wantStatus: http.StatusBadRequest},
		{kind: coregateway.ErrorNotFound, wantStatus: http.StatusNotFound},
		{kind: coregateway.ErrorUnavailable, wantStatus: http.StatusServiceUnavailable},
		{kind: coregateway.ErrorUnauthorized, wantStatus: http.StatusUnauthorized, wantAuthenticate: true},
		{kind: coregateway.ErrorRateLimited, wantStatus: http.StatusTooManyRequests},
		{kind: coregateway.ErrorNoRoute, wantStatus: http.StatusUnprocessableEntity},
		{kind: coregateway.ErrorInternal, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.writeSubmissionError(recorder, &coregateway.SubmissionError{Kind: test.kind, Message: "submission failed"})
			if recorder.Code != test.wantStatus || !strings.Contains(recorder.Body.String(), "submission failed") {
				t.Fatalf("response = (%d, %s), want (%d, submission error)", recorder.Code, recorder.Body.String(), test.wantStatus)
			}
			if got := recorder.Header().Get("WWW-Authenticate"); (got == "ApiKey") != test.wantAuthenticate {
				t.Fatalf("WWW-Authenticate = %q, want API key header=%t", got, test.wantAuthenticate)
			}
		})
	}
}

func TestServerRejectsInvalidAPIKey(t *testing.T) {
	st := newTestStore()
	putObject(t, st, "SecretRef", "tenant-a", "gateway-key", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","token":"expected"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", Key: "token"},
	})
	putObject(t, st, "IntentGateway", "tenant-a", "support", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support", Namespace: "tenant-a"},
		Spec: v1alpha1.IntentGatewaySpec{
			AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "gateway-key"}},
			Routes:     []v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}},
		},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})
	putObject(t, st, "Agent", "tenant-a", "planner", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}})
	server := New(Config{}, st, zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals", strings.NewReader(`{"goal":"anything"}`))
	request.Header.Set("X-Kognition-API-Key", "wrong")
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || recorder.Header().Get("WWW-Authenticate") != "ApiKey" {
		t.Fatalf("status = %d, authenticate = %q", recorder.Code, recorder.Header().Get("WWW-Authenticate"))
	}
}

func TestGRPCSubmitGoalAuthenticatesAndCreatesSolution(t *testing.T) {
	st := newTestStore()
	putObject(t, st, "SecretRef", "tenant-a", "gateway-key", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","token":"expected"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", Key: "token"},
	})
	putObject(t, st, "IntentGateway", "tenant-a", "support", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support", Namespace: "tenant-a"},
		Spec: v1alpha1.IntentGatewaySpec{
			AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "gateway-key"}},
			Routes:     []v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}},
		},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})
	putObject(t, st, "Agent", "tenant-a", "planner", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}})
	server := New(Config{}, st, zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()
	client := newKubernetesGatewayGRPCClient(t, server)

	_, err := client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{
		Namespace: "tenant-a", GatewayName: "support", Goal: "anything", ApiKey: "wrong",
	})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("invalid key status = %s, want %s; error = %v", got, codes.Unauthenticated, err)
	}

	response, err := client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{
		Namespace: "tenant-a", GatewayName: "support", Goal: "anything", ApiKey: "expected",
	})
	if err != nil {
		t.Fatalf("SubmitGoal() error = %v", err)
	}
	if _, err := st.Get(context.Background(), "Solution", "tenant-a", response.GetSolutionName()); err != nil {
		t.Fatalf("created Solution: %v", err)
	}
}

func TestServerStartAndShutdownServeGRPC(t *testing.T) {
	grpcListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve gRPC address: %v", err)
	}
	grpcAddress := grpcListener.Addr().String()
	_ = grpcListener.Close()

	server := New(Config{Addr: "127.0.0.1:0", GRPCAddr: grpcAddress}, newTestStore(), zaptest.NewLogger(t))
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Start() }()

	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(dialCtx, grpcAddress, grpc.WithBlock(), grpc.WithTransportCredentials(insecure.NewCredentials())) //nolint:staticcheck // Test requires blocking grpc 1.x dialing semantics.
	if err != nil {
		t.Fatalf("dial gRPC server: %v", err)
	}
	defer connection.Close()
	_, err = intentgatewayv1alpha1.NewIntentGatewayClient(connection).SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("SubmitGoal status = %s, want %s; error = %v", got, codes.InvalidArgument, err)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Start() error = %v, want %v", err, http.ErrServerClosed)
	}
}

func newKubernetesGatewayGRPCClient(t *testing.T, server *Server) intentgatewayv1alpha1.IntentGatewayClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := server.newGRPCServer()
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	connection, err := grpc.DialContext(context.Background(), "bufnet", //nolint:staticcheck // Bufconn test relies on grpc 1.x dialing semantics.
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return intentgatewayv1alpha1.NewIntentGatewayClient(connection)
}

func TestServerWithKubernetesStoreAdapterCreatesNativeSolution(t *testing.T) {
	scheme := k8sctrl.SetupScheme()
	intentGateway := &k8s.IntentGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "tenant-a"},
		Spec: k8s.IntentGatewaySpec{Routes: []k8s.IntentRoute{{
			Pattern: "deploy", TargetKind: "Agent", TargetRef: k8s.LocalObjectReference{Name: "planner"},
		}}},
		Status: k8s.IntentGatewayStatus{Phase: string(v1alpha1.PhaseReady)},
	}
	agent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(intentGateway, agent).Build()
	server := New(Config{}, k8sctrl.NewStoreAdapter(k8sClient), zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/tenant-a/intentgateways/support/goals", strings.NewReader(`{"goal":"Deploy the release"}`))
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	var response coregateway.GoalResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	created := &k8s.Solution{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: response.SolutionRef.Name}, created); err != nil {
		t.Fatalf("get native Solution: %v", err)
	}
	if created.Spec.Target == nil || created.Spec.Target.Kind != "Agent" || created.Spec.Target.Ref.Name != "planner" {
		t.Fatalf("solution target = %#v", created.Spec.Target)
	}
}

func TestServerGRPCUsesConfiguredAuthenticator(t *testing.T) {
	server := New(Config{Authenticator: auth.NewStaticTokenAuthenticator(map[string]string{"expected": "caller"})}, newTestStore(), zaptest.NewLogger(t))
	client := newKubernetesGatewayGRPCClient(t, server)

	_, err := client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("missing token status = %s, want %s; error = %v", got, codes.Unauthenticated, err)
	}
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer expected"))
	_, err = client.SubmitGoal(ctx, &intentgatewayv1alpha1.SubmitGoalRequest{})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("authenticated status = %s, want %s; error = %v", got, codes.InvalidArgument, err)
	}
}

func TestServerHTTPUsesConfiguredAuthenticator(t *testing.T) {
	server := New(Config{Authenticator: auth.NewStaticTokenAuthenticator(map[string]string{"expected": "caller"})}, newTestStore(), zaptest.NewLogger(t))
	defer func() { _ = server.Shutdown(context.Background()) }()

	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/gateway/goals", strings.NewReader(`{"goal":"deploy"}`))
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	request.Header.Set("Authorization", "Bearer expected")
	recorder = httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("authenticated status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}
