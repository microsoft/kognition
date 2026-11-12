// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kognition-ai/kognition/internal/observability"
)

type WorkloadGatewayEndpoint struct {
	URL    string `json:"url"`
	PeerID string `json:"peerID"`
}

type workloadGateway struct {
	endpoint string
	client   *http.Client
}

type WorkloadProxy struct {
	source   *WorkloadIdentity
	gateways map[string]workloadGateway
}

var workloadOperations = map[string]map[string]bool{
	"model":         {"/v1/chat/completions": true},
	"tool":          {"/v1/tools/capabilities": true, "/v1/tools/invoke": true},
	"memory":        {"/v1/memory/search": true, "/v1/memory/store": true},
	"communication": {"/v1/agent-services/capabilities": true, "/v1/agent-services/invoke": true},
}

func NewWorkloadProxy(source *WorkloadIdentity, endpoints map[string]WorkloadGatewayEndpoint) (*WorkloadProxy, error) {
	if source == nil || len(endpoints) != len(workloadOperations) {
		return nil, errors.New("workload proxy requires its identity and four gateway endpoints")
	}
	proxy := &WorkloadProxy{source: source, gateways: map[string]workloadGateway{}}
	for plane := range workloadOperations {
		endpoint := endpoints[plane]
		parsed, err := url.Parse(endpoint.URL)
		if err != nil || parsed == nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
			return nil, errors.New("workload gateway must use a pinned HTTPS origin")
		}
		client, err := source.HTTPClient(endpoint.PeerID)
		if err != nil {
			return nil, errors.New("invalid workload gateway peer identity")
		}
		proxy.gateways[plane] = workloadGateway{endpoint: strings.TrimRight(endpoint.URL, "/"), client: client}
	}
	return proxy, nil
}

func (proxy *WorkloadProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && (request.URL.Path == "/readyz" || request.URL.Path == "/healthz") {
		if !proxy.source.Status().Ready {
			http.Error(writer, "workload identity unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(request.URL.Path, "/"), "/", 2)
	if len(parts) != 2 || request.Method != http.MethodPost || request.URL.RawQuery != "" || request.URL.RawPath != "" {
		http.Error(writer, "workload operation unavailable", http.StatusNotFound)
		return
	}
	plane, path := parts[0], "/"+parts[1]
	gateway, configured := proxy.gateways[plane]
	if !configured || !workloadOperations[plane][path] {
		http.Error(writer, "workload operation unavailable", http.StatusNotFound)
		return
	}
	if !proxy.source.Status().Ready || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") || len(request.Header.Get("Authorization")) > 32768 {
		http.Error(writer, "workload authentication unavailable", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		http.Error(writer, "workload request exceeds bounds", http.StatusRequestEntityTooLarge)
		return
	}
	upstream, err := http.NewRequestWithContext(request.Context(), http.MethodPost, gateway.endpoint+path, bytes.NewReader(body))
	if err != nil {
		http.Error(writer, "workload gateway unavailable", http.StatusBadGateway)
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	for _, name := range []string{"Authorization", "traceparent", "tracestate", observability.TrustDecisionRunIDHeader} {
		if value := request.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	response, err := gateway.client.Do(upstream)
	if err != nil {
		http.Error(writer, "workload gateway unavailable", http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(responseBody) > 4<<20 || (response.StatusCode >= 300 && response.StatusCode < 400) {
		http.Error(writer, "workload gateway response unavailable", http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(responseBody)
}

func (proxy *WorkloadProxy) Close() {
	for _, gateway := range proxy.gateways {
		gateway.client.CloseIdleConnections()
	}
}
