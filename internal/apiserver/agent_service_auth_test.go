// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/auth"
	"go.uber.org/zap"
)

func TestAgentServiceCredentialCannotAccessControlPlane(t *testing.T) {
	server := New(Config{}, newMemStore(), zap.NewNop(),
		WithAuthenticator(auth.NewStaticTokenAuthenticator(map[string]string{"operator-token": "operator"}), nil),
		WithAgentServiceToken("dedicated-service-token"))
	for _, scenario := range []struct {
		method, path, token string
		status              int
	}{
		{"POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/missing/invoke", "dedicated-service-token", http.StatusNotFound},
		{"GET", "/apis/kognition.dev/v1alpha1/namespaces/default/secrets", "dedicated-service-token", http.StatusForbidden},
		{"POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", "dedicated-service-token", http.StatusForbidden},
		{"DELETE", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/agent", "dedicated-service-token", http.StatusForbidden},
		{"POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agents/agent/chat", "dedicated-service-token", http.StatusForbidden},
		{"GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/missing/invoke", "dedicated-service-token", http.StatusForbidden},
		{"POST", "/apis/kognition.dev/v1alpha1/namespaces/default/agentservices/missing/invoke", "runtime-token", http.StatusUnauthorized},
		{"GET", "/apis/kognition.dev/v1alpha1/namespaces/default/agents", "operator-token", http.StatusOK},
	} {
		t.Run(scenario.method+scenario.path+scenario.token, func(t *testing.T) {
			request := httptest.NewRequest(scenario.method, scenario.path, strings.NewReader(`{"message":"hello"}`))
			request.Header.Set("Authorization", "Bearer "+scenario.token)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != scenario.status {
				t.Fatalf("status=%d, want %d", response.Code, scenario.status)
			}
		})
	}
}
