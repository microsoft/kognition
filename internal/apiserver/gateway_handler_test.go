// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	intentgatewayv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/gateway"
	"go.uber.org/zap"
)

// TC-0064: HTTP goal submission creates Solution.
func TestSubmitGoal_CreatesSolution(t *testing.T) {
	srv, ms := newTestServer(t)

	// Seed an IntentGateway with a ready phase and a matching route.
	gw := v1alpha1.IntentGateway{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "IntentGateway",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "test-gw",
			Namespace:       "default",
			ResourceVersion: 1,
		},
		Spec: v1alpha1.IntentGatewaySpec{
			Routes: []v1alpha1.IntentRoute{
				{
					Pattern:    "deploy",
					TargetRef:  v1alpha1.LocalObjectReference{Name: "deploy-agent"},
					TargetKind: "Agent",
				},
			},
		},
		Status: v1alpha1.IntentGatewayStatus{
			Phase: v1alpha1.PhaseReady,
		},
	}
	gwData, _ := json.Marshal(gw)
	if err := ms.Create(context.Background(), "IntentGateway", "default", "test-gw", gwData); err != nil {
		t.Fatalf("seed IntentGateway: %v", err)
	}

	// Also seed the target Agent so the gateway is valid.
	agent := map[string]any{
		"apiVersion": v1alpha1.APIVersion,
		"kind":       "Agent",
		"metadata":   map[string]any{"name": "deploy-agent", "namespace": "default", "resourceVersion": 1},
		"spec":       map[string]any{"model": "gpt-4"},
	}
	agentData, _ := json.Marshal(agent)
	if err := ms.Create(context.Background(), "Agent", "default", "deploy-agent", agentData); err != nil {
		t.Fatalf("seed Agent: %v", err)
	}

	// Submit a goal.
	body := map[string]any{
		"goal":        "deploy my application to production",
		"constraints": []string{"use canary strategy"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/test-gw/goals", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify response contains solutionRef.
	var resp GoalResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.SolutionRef.Name == "" {
		t.Fatal("solutionRef.name is empty")
	}
	if resp.SolutionRef.Namespace != "default" {
		t.Fatalf("want namespace 'default', got %q", resp.SolutionRef.Namespace)
	}

	// Verify the Solution was actually created in the store.
	solData, err := ms.Get(context.Background(), "Solution", "default", resp.SolutionRef.Name)
	if err != nil {
		t.Fatalf("Solution not found in store: %v", err)
	}

	var sol v1alpha1.Solution
	if err := json.Unmarshal(solData, &sol); err != nil {
		t.Fatalf("unmarshal Solution: %v", err)
	}
	if sol.Spec.Goal != "deploy my application to production" {
		t.Fatalf("want goal 'deploy my application to production', got %q", sol.Spec.Goal)
	}
	if sol.Spec.Target == nil || sol.Spec.Target.Kind != "Agent" || sol.Spec.Target.Ref.Name != "deploy-agent" {
		t.Fatalf("solution target = %#v, want Agent deploy-agent", sol.Spec.Target)
	}
	if sol.Status.Phase != v1alpha1.PhasePending {
		t.Fatalf("want phase Pending, got %s", sol.Status.Phase)
	}
}

func TestMatchRouteSupportsDocumentedWildcardsAndFallback(t *testing.T) {
	routes := []v1alpha1.IntentRoute{
		{Pattern: "research *", TargetKind: "Workflow", TargetRef: v1alpha1.LocalObjectReference{Name: "research"}},
		{Pattern: "summarize", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "summarizer"}},
		{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "fallback"}},
	}
	for _, test := range []struct {
		goal string
		want string
	}{
		{goal: "Research quarterly trends", want: "research"},
		{goal: "please summarize this", want: "summarizer"},
		{goal: "unclassified request", want: "fallback"},
	} {
		route := gateway.MatchRoute(routes, test.goal)
		if route == nil || route.TargetRef.Name != test.want {
			t.Fatalf("matchRoute(%q) = %+v, want %q", test.goal, route, test.want)
		}
	}
}

func TestSubmitGoalRequiresConfiguredAPIKey(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "Agent", "default", "planner", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "default"}})
	putJSON(memory, "SecretRef", "default", "gateway-key", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","token":"expected"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "gateway-key", Key: "token"},
	})
	putJSON(memory, "IntentGateway", "default", "secured", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "secured", Namespace: "default"},
		Spec: v1alpha1.IntentGatewaySpec{
			AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "gateway-key"}},
			Routes:     []v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}},
		},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})

	for _, test := range []struct {
		name string
		key  string
		code int
	}{
		{name: "missing", code: http.StatusUnauthorized},
		{name: "wrong", key: "wrong", code: http.StatusUnauthorized},
		{name: "correct", key: "expected", code: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/secured/goals", strings.NewReader(`{"goal":"anything"}`))
			if test.key != "" {
				req.Header.Set("X-Kognition-API-Key", test.key)
			}
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, req)
			if rec.Code != test.code {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSubmitGoalRejectsUnavailableAgentServiceTarget(t *testing.T) {
	srv, memory := newTestServer(t)
	putJSON(memory, "AgentService", "default", "planner-service", v1alpha1.AgentService{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-service", Namespace: "default"},
		Spec:       v1alpha1.AgentServiceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}},
		Status:     v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhasePending},
	})
	putJSON(memory, "IntentGateway", "default", "support", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "support", Namespace: "default"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			Pattern: "*", TargetKind: "AgentService", TargetRef: v1alpha1.LocalObjectReference{Name: "planner-service"},
		}}},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})

	recorder := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/support/goals", map[string]any{"goal": "plan deployment"})
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
}

func TestGRPCSubmitGoalCreatesSolutionWithAPIKey(t *testing.T) {
	server, memory := newTestServer(t)
	putJSON(memory, "Agent", "default", "planner", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "default"}})
	putJSON(memory, "SecretRef", "default", "gateway-key", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","token":"expected"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "gateway-key", Key: "token"},
	})
	putJSON(memory, "IntentGateway", "default", "secured", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "secured", Namespace: "default"},
		Spec: v1alpha1.IntentGatewaySpec{
			AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "gateway-key"}},
			Routes:     []v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}},
		},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})

	client := newAPIServerGRPCClient(t, server)
	response, err := client.SubmitGoal(context.Background(), &intentgatewayv1alpha1.SubmitGoalRequest{
		Namespace: "default", GatewayName: "secured", Goal: "plan deployment", ApiKey: "expected",
	})
	if err != nil {
		t.Fatalf("SubmitGoal() error = %v", err)
	}
	if response.GetSolutionName() == "" || response.GetSolutionNamespace() != "default" {
		t.Fatalf("response = %#v", response)
	}
	if _, err := memory.Get(context.Background(), "Solution", "default", response.GetSolutionName()); err != nil {
		t.Fatalf("created Solution: %v", err)
	}
}

func newAPIServerGRPCClient(t *testing.T, server *Server) intentgatewayv1alpha1.IntentGatewayClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := server.newGRPCServer()
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
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

func TestSubmitGoalInvalidAPIKeyDoesNotConsumeRateLimit(t *testing.T) {
	srv, memory := newTestServer(t)
	srv.rateLimiter = NewRateLimiter(0, 1)
	putJSON(memory, "Agent", "default", "planner", v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "default"}})
	putJSON(memory, "SecretRef", "default", "gateway-key", v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","token":"expected"}`}},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "gateway-key", Key: "token"},
	})
	putJSON(memory, "IntentGateway", "default", "secured", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "secured", Namespace: "default"},
		Spec: v1alpha1.IntentGatewaySpec{
			AuthConfig: &v1alpha1.IntentGatewayAuthConfig{Mode: "apiKey", APIKeySecretRef: &v1alpha1.LocalObjectReference{Name: "gateway-key"}},
			Routes:     []v1alpha1.IntentRoute{{Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "planner"}}},
		},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})

	invalid := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/secured/goals", strings.NewReader(`{"goal":"anything"}`))
	invalid.Header.Set("X-Kognition-API-Key", "wrong")
	invalidRecorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(invalidRecorder, invalid)
	if invalidRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("invalid API key status = %d, want %d", invalidRecorder.Code, http.StatusUnauthorized)
	}

	valid := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/secured/goals", strings.NewReader(`{"goal":"anything"}`))
	valid.Header.Set("X-Kognition-API-Key", "expected")
	validRecorder := httptest.NewRecorder()
	srv.mux.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusCreated {
		t.Fatalf("valid API key status = %d, want %d: %s", validRecorder.Code, http.StatusCreated, validRecorder.Body.String())
	}
}

// TC-0065: Empty body returns structured error.
func TestSubmitGoal_EmptyBody_ReturnsError(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/test-gw/goals", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)

	// Verify structured error response.
	if resp["kind"] != "Status" {
		t.Fatalf("want kind 'Status', got %v", resp["kind"])
	}
	if resp["status"] != "Failure" {
		t.Fatalf("want status 'Failure', got %v", resp["status"])
	}
	msg, _ := resp["message"].(string)
	if msg != "request body is required" {
		t.Fatalf("want message 'request body is required', got %q", msg)
	}
	code, _ := resp["code"].(float64)
	if int(code) != http.StatusBadRequest {
		t.Fatalf("want code 400, got %v", resp["code"])
	}
}

// TestSubmitGoal_MissingGoalField validates that a body without a goal field returns 400.
func TestSubmitGoal_MissingGoalField(t *testing.T) {
	srv, _ := newTestServer(t)

	body := map[string]any{
		"constraints": []string{"fast"},
	}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/test-gw/goals", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	msg, _ := resp["message"].(string)
	if msg != "goal is required and cannot be empty" {
		t.Fatalf("want 'goal is required and cannot be empty', got %q", msg)
	}
}

// TestSubmitGoal_GatewayNotFound validates 404 when gateway doesn't exist.
func TestSubmitGoal_GatewayNotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	body := map[string]any{"goal": "do something"}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/nonexistent/goals", body)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSubmitGoal_GatewayNotReady validates 503 when gateway is not in Ready phase.
func TestSubmitGoal_GatewayNotReady(t *testing.T) {
	srv, ms := newTestServer(t)

	gw := v1alpha1.IntentGateway{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "IntentGateway",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "pending-gw",
			Namespace:       "default",
			ResourceVersion: 1,
		},
		Spec: v1alpha1.IntentGatewaySpec{
			Routes: []v1alpha1.IntentRoute{
				{Pattern: "test", TargetRef: v1alpha1.LocalObjectReference{Name: "agent"}, TargetKind: "Agent"},
			},
		},
		Status: v1alpha1.IntentGatewayStatus{
			Phase: v1alpha1.PhasePending,
		},
	}
	gwData, _ := json.Marshal(gw)
	if err := ms.Create(context.Background(), "IntentGateway", "default", "pending-gw", gwData); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := map[string]any{"goal": "test something"}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/pending-gw/goals", body)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSubmitGoal_ReadyGatewayRejectsDeletedTarget(t *testing.T) {
	srv, ms := newTestServer(t)
	putJSON(ms, "IntentGateway", "default", "stale-gateway", v1alpha1.IntentGateway{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "stale-gateway", Namespace: "default"},
		Spec: v1alpha1.IntentGatewaySpec{Routes: []v1alpha1.IntentRoute{{
			Pattern: "*", TargetKind: "Agent", TargetRef: v1alpha1.LocalObjectReference{Name: "deleted-agent"},
		}}},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	})

	rec := doReq(t, srv, http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/stale-gateway/goals", map[string]any{"goal": "do work"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

// TestSubmitGoal_RateLimitExceeded validates 429 when rate limit is reached.
func TestSubmitGoal_RateLimitExceeded(t *testing.T) {
	srv, ms := newTestServer(t)

	// Override the rate limiter to a very restrictive one: 0 rate, burst of 1.
	srv.rateLimiter = NewRateLimiter(0, 1)

	gw := v1alpha1.IntentGateway{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "IntentGateway",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "limited-gw",
			Namespace:       "default",
			ResourceVersion: 1,
		},
		Spec: v1alpha1.IntentGatewaySpec{
			Routes: []v1alpha1.IntentRoute{
				{Pattern: "test", TargetRef: v1alpha1.LocalObjectReference{Name: "agent"}, TargetKind: "Agent"},
			},
		},
		Status: v1alpha1.IntentGatewayStatus{
			Phase: v1alpha1.PhaseReady,
		},
	}
	gwData, _ := json.Marshal(gw)
	if err := ms.Create(context.Background(), "IntentGateway", "default", "limited-gw", gwData); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := map[string]any{"goal": "test something"}

	// First request should succeed (burst of 1).
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/limited-gw/goals", body)
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("first request should not be rate limited")
	}

	// Second request should be rate limited (0 rate, no replenishment).
	rec = doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/limited-gw/goals", body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSubmitGoal_InvalidJSON validates that a malformed JSON body returns 400.
func TestSubmitGoal_InvalidJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := doReqRaw(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/test-gw/goals", []byte(`{not json`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	msg, _ := resp["message"].(string)
	if msg != "invalid JSON body" {
		t.Fatalf("want 'invalid JSON body', got %q", msg)
	}
}

// TestSubmitGoal_NoRouteMatched validates 422 when no route matches the submitted goal.
func TestSubmitGoal_NoRouteMatched(t *testing.T) {
	srv, ms := newTestServer(t)

	gw := v1alpha1.IntentGateway{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "IntentGateway",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:            "route-gw",
			Namespace:       "default",
			ResourceVersion: 1,
		},
		Spec: v1alpha1.IntentGatewaySpec{
			Routes: []v1alpha1.IntentRoute{
				{Pattern: "deploy", TargetRef: v1alpha1.LocalObjectReference{Name: "agent"}, TargetKind: "Agent"},
			},
		},
		Status: v1alpha1.IntentGatewayStatus{
			Phase: v1alpha1.PhaseReady,
		},
	}
	gwData, _ := json.Marshal(gw)
	if err := ms.Create(context.Background(), "IntentGateway", "default", "route-gw", gwData); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Submit a goal that does NOT match the "deploy" pattern.
	body := map[string]any{"goal": "run unit tests"}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/route-gw/goals", body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	msg, _ := resp["message"].(string)
	if msg != "no route matched the submitted goal" {
		t.Fatalf("want 'no route matched the submitted goal', got %q", msg)
	}
}

// TestSubmitGoal_WhitespaceOnlyGoal validates that a goal with only whitespace returns 400.
func TestSubmitGoal_WhitespaceOnlyGoal(t *testing.T) {
	srv, _ := newTestServer(t)

	body := map[string]any{"goal": "   \t  "}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/test-gw/goals", body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}

	resp := decodeJSON(t, rec)
	msg, _ := resp["message"].(string)
	if msg != "goal is required and cannot be empty" {
		t.Fatalf("want 'goal is required and cannot be empty', got %q", msg)
	}
}

// TestSubmitGoal_StoreGetError verifies that an unexpected store.Get error returns 500.
func TestSubmitGoal_StoreGetError(t *testing.T) {
	ms := newMemStore()
	es := &errStore{memStore: ms, getErr: errors.New("disk failure")}
	logger, _ := zap.NewDevelopment()
	srv := New(Config{Addr: ":0"}, es, logger)

	body := map[string]any{"goal": "deploy the app"}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/my-gw/goals", body)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	msg, _ := resp["message"].(string)
	if msg != "internal error" {
		t.Fatalf("want 'internal error', got %q", msg)
	}
}

// TestSubmitGoal_CorruptedGatewayData verifies that corrupt gateway data in store returns 500.
func TestSubmitGoal_CorruptedGatewayData(t *testing.T) {
	ms := newMemStore()
	logger, _ := zap.NewDevelopment()
	srv := New(Config{Addr: ":0"}, ms, logger)

	// Seed corrupted data for the gateway.
	if err := ms.Create(context.Background(), "IntentGateway", "default", "bad-gw", []byte("{not valid json")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := map[string]any{"goal": "deploy the app"}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/bad-gw/goals", body)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestSubmitGoal_StoreCreateError verifies that a store.Create failure returns 500.
func TestSubmitGoal_StoreCreateError(t *testing.T) {
	ms := newMemStore()
	es := &errStore{memStore: ms, createErr: errors.New("write failed")}
	logger, _ := zap.NewDevelopment()
	srv := New(Config{Addr: ":0"}, es, logger)

	// Seed a ready IntentGateway with a matching route.
	gw := v1alpha1.IntentGateway{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "IntentGateway"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "create-gw", Namespace: "default"},
		Spec: v1alpha1.IntentGatewaySpec{
			Routes: []v1alpha1.IntentRoute{
				{Pattern: "deploy", TargetRef: v1alpha1.LocalObjectReference{Name: "agent-1"}, TargetKind: "Agent"},
			},
		},
		Status: v1alpha1.IntentGatewayStatus{Phase: v1alpha1.PhaseReady},
	}
	gwData, _ := json.Marshal(gw)
	if err := ms.Create(context.Background(), "IntentGateway", "default", "create-gw", gwData); err != nil {
		t.Fatalf("seed: %v", err)
	}
	targetData, _ := json.Marshal(v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-1", Namespace: "default"}})
	if err := ms.Create(context.Background(), "Agent", "default", "agent-1", targetData); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	body := map[string]any{"goal": "deploy the service"}
	rec := doReq(t, srv, "POST", "/apis/kognition.dev/v1alpha1/namespaces/default/intentgateways/create-gw/goals", body)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	resp := decodeJSON(t, rec)
	msg, _ := resp["message"].(string)
	if msg != "failed to create solution" {
		t.Fatalf("want 'failed to create solution', got %q", msg)
	}
}
