// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"net/http"
	"strings"

	"github.com/kognition-ai/kognition/internal/auth"
)

func (s *Server) agentServiceAuthentication(next http.Handler) http.Handler {
	authenticator := auth.NewStaticTokenAuthenticator(map[string]string{s.agentServiceToken: "agent-communication-gateway"})
	serviceMux := http.NewServeMux()
	serviceMux.HandleFunc("POST /apis/kognition.dev/v1alpha1/namespaces/{namespace}/agentservices/{name}/invoke", s.handleAgentServiceInvoke)
	serviceMux.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, "service credential is restricted to AgentService invocation", http.StatusForbidden)
	})
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		scheme, token, found := strings.Cut(request.Header.Get("Authorization"), " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			if identity, err := authenticator.Authenticate(request.Context(), strings.TrimSpace(token)); err == nil {
				serviceMux.ServeHTTP(response, request.WithContext(auth.WithIdentity(request.Context(), identity)))
				return
			}
		}
		next.ServeHTTP(response, request)
	})
}
