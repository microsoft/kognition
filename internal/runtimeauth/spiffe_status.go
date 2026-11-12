// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

const workloadIdentityStatusPath = "/v1/workload-identity/status"

func NewWorkloadIdentityStatusHandler(source *WorkloadIdentity, controllerIdentity string) (http.Handler, error) {
	controller, err := spiffeid.FromString(controllerIdentity)
	if source == nil || err != nil || controller.Path() == "" || controller.TrustDomain() != source.expected.TrustDomain() || controller == source.expected {
		return nil, errors.New("workload status requires a distinct pinned controller identity in the trusted domain")
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if request.Method != http.MethodGet || request.URL.Path != workloadIdentityStatusPath || request.URL.RawQuery != "" || request.URL.RawPath != "" {
			http.NotFound(writer, request)
			return
		}
		if !source.Status().Ready {
			http.Error(writer, "workload identity unavailable", http.StatusServiceUnavailable)
			return
		}
		if request.TLS == nil {
			http.Error(writer, "controller identity required", http.StatusUnauthorized)
			return
		}
		peer, _, err := x509svid.Verify(request.TLS.PeerCertificates, source)
		if err != nil || peer != controller {
			http.Error(writer, "controller identity not authorized", http.StatusUnauthorized)
			return
		}
		status := source.Status()
		if !status.Ready {
			http.Error(writer, "workload identity unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(status); err != nil {
			return
		}
	}), nil
}

func NewWorkloadIdentityStatusServer(source *WorkloadIdentity, controllerIdentity string) (*http.Server, error) {
	handler, err := NewWorkloadIdentityStatusHandler(source, controllerIdentity)
	if err != nil {
		return nil, err
	}
	controller := spiffeid.RequireFromString(controllerIdentity)
	configuration := tlsconfig.MTLSServerConfig(source, source, tlsconfig.AuthorizeID(controller))
	configuration.MinVersion = tls.VersionTLS13
	return &http.Server{Handler: handler, TLSConfig: configuration, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}, nil
}

type WorkloadIdentityStatusClient struct {
	client    *http.Client
	endpoint  string
	expected  string
	domain    string
	namespace string
	agentName string
	agentUID  string
}

func NewWorkloadIdentityStatusClient(source *WorkloadIdentity, endpoint, trustDomain, namespace, agentName, agentUID string) (*WorkloadIdentityStatusClient, error) {
	expected, err := AgentIncarnationSPIFFEID(trustDomain, namespace, agentName, agentUID)
	origin, urlErr := url.Parse(endpoint)
	if source == nil || err != nil || urlErr != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") || source.expected.String() == expected {
		return nil, errors.New("workload status client requires a fixed HTTPS origin and distinct Agent incarnation")
	}
	client, err := source.HTTPClient(expected)
	if err != nil {
		return nil, err
	}
	client.Timeout = 5 * time.Second
	return &WorkloadIdentityStatusClient{client: client, endpoint: strings.TrimRight(endpoint, "/") + workloadIdentityStatusPath, expected: expected, domain: trustDomain, namespace: namespace, agentName: agentName, agentUID: agentUID}, nil
}

func (client *WorkloadIdentityStatusClient) AgentStatus(namespace, agentName, agentUID string) (WorkloadIdentityStatus, error) {
	if client == nil || namespace != client.namespace || agentName != client.agentName || agentUID != client.agentUID {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.endpoint, nil)
	if err != nil {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	response, err := client.client.Do(request)
	if err != nil {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(body) > 8192 {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	status, err := decodeWorkloadIdentityStatus(body)
	now := time.Now()
	if err != nil || status.SPIFFEID != client.expected || status.TrustDomain != client.domain || !status.Ready || status.Reason != "Ready" || status.Generation == 0 || status.Generation > math.MaxInt64 || !status.NotBefore.Before(status.ExpiresAt) || now.Before(status.NotBefore) || !now.Before(status.ExpiresAt) {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 || !status.NotBefore.Equal(response.TLS.PeerCertificates[0].NotBefore) || !status.ExpiresAt.Equal(response.TLS.PeerCertificates[0].NotAfter) {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	return status, nil
}

func decodeWorkloadIdentityStatus(body []byte) (WorkloadIdentityStatus, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	fields := map[string]bool{"SPIFFEID": false, "TrustDomain": false, "NotBefore": false, "ExpiresAt": false, "Generation": false, "Ready": false, "Reason": false}
	count := 0
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
		}
		name, ok := key.(string)
		seen, known := fields[name]
		if !ok || !known || seen {
			return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
		}
		fields[name] = true
		count++
	}
	closing, err := decoder.Token()
	var trailing json.RawMessage
	if err != nil || closing != json.Delim('}') || count != len(fields) || decoder.Decode(&trailing) != io.EOF {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	var status WorkloadIdentityStatus
	if json.Unmarshal(body, &status) != nil {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	return status, nil
}

func (client *WorkloadIdentityStatusClient) Close() {
	if client != nil {
		client.client.CloseIdleConnections()
	}
}
