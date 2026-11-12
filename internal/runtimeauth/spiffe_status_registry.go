// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

type WorkloadIdentityStatusRegistration struct {
	Namespace string `json:"namespace"`
	AgentName string `json:"agentName"`
	AgentUID  string `json:"agentUID"`
	Endpoint  string `json:"endpoint"`
}

type WorkloadIdentityStatusRegistry struct {
	clients map[string]*WorkloadIdentityStatusClient
}

func NewWorkloadIdentityStatusRegistry(source *WorkloadIdentity, domain string, registrations []WorkloadIdentityStatusRegistration) (*WorkloadIdentityStatusRegistry, error) {
	if source == nil || len(registrations) == 0 || len(registrations) > 128 {
		return nil, errors.New("workload status registry requires 1 to 128 operator registrations")
	}
	registry := &WorkloadIdentityStatusRegistry{clients: map[string]*WorkloadIdentityStatusClient{}}
	for _, registration := range registrations {
		key := registration.Namespace + "/" + registration.AgentName
		if registry.clients[key] != nil {
			registry.Close()
			return nil, errors.New("duplicate workload status registration")
		}
		client, err := NewWorkloadIdentityStatusClient(source, registration.Endpoint, domain, registration.Namespace, registration.AgentName, registration.AgentUID)
		if err != nil {
			registry.Close()
			return nil, err
		}
		registry.clients[key] = client
	}
	return registry, nil
}

func (registry *WorkloadIdentityStatusRegistry) AgentStatus(namespace, name, uid string) (WorkloadIdentityStatus, error) {
	if registry == nil {
		return WorkloadIdentityStatus{}, ErrWorkloadIdentityUnavailable
	}
	return registry.clients[namespace+"/"+name].AgentStatus(namespace, name, uid)
}

func (registry *WorkloadIdentityStatusRegistry) Close() {
	if registry != nil {
		for _, client := range registry.clients {
			client.Close()
		}
	}
}

func LoadWorkloadIdentityStatusRegistry(ctx context.Context, lookup func(string) string) (*WorkloadIdentityStatusRegistry, func(), error) {
	return loadWorkloadIdentityStatusRegistry(ctx, lookup, NewWorkloadIdentity)
}

func loadWorkloadIdentityStatusRegistry(ctx context.Context, lookup func(string) string, newSource func(context.Context, WorkloadIdentityConfig) (*WorkloadIdentity, error)) (*WorkloadIdentityStatusRegistry, func(), error) {
	path := lookup("KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE")
	if path == "" {
		return nil, func() {}, nil
	}
	domain, identity := lookup("KOGNITION_SPIFFE_TRUST_DOMAIN"), lookup("KOGNITION_SPIFFE_ID")
	controller, err := spiffeid.FromString(identity)
	if err != nil || controller.TrustDomain().String() != domain || controller.Path() == "" {
		return nil, nil, errors.New("status reporting requires a configured controller SPIFFE identity")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errors.New("cannot read operator workload status registry")
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(body) > 65536 {
		return nil, nil, errors.New("workload status registry exceeds bounds")
	}
	registrations, err := decodeWorkloadStatusRegistrations(body, domain)
	if err != nil {
		return nil, nil, err
	}
	source, err := newSource(ctx, WorkloadIdentityConfig{SocketAddress: lookup("KOGNITION_SPIFFE_ENDPOINT_SOCKET"), ExpectedID: identity, TrustDomain: domain})
	if err != nil {
		return nil, nil, ErrWorkloadIdentityUnavailable
	}
	registry, err := NewWorkloadIdentityStatusRegistry(source, domain, registrations)
	if err != nil {
		_ = source.Close()
		return nil, nil, err
	}
	return registry, func() { registry.Close(); _ = source.Close() }, nil
}

func decodeWorkloadStatusRegistrations(body []byte, domain string) ([]WorkloadIdentityStatusRegistration, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('[') {
		return nil, errors.New("invalid operator workload status registry")
	}
	var registrations []WorkloadIdentityStatusRegistration
	seenAgents := map[string]bool{}
	for decoder.More() {
		opening, err := decoder.Token()
		if err != nil || opening != json.Delim('{') || len(registrations) >= 128 {
			return nil, errors.New("invalid workload status registration")
		}
		fields := map[string]string{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || (name != "namespace" && name != "agentName" && name != "agentUID" && name != "endpoint") {
				return nil, errors.New("unknown workload status field")
			}
			if _, exists := fields[name]; exists {
				return nil, errors.New("duplicate workload status field")
			}
			var value json.RawMessage
			var text string
			if decoder.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil || text == "" {
				return nil, errors.New("invalid workload status field")
			}
			fields[name] = text
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') || len(fields) != 4 {
			return nil, errors.New("incomplete workload status registration")
		}
		registration := WorkloadIdentityStatusRegistration{Namespace: fields["namespace"], AgentName: fields["agentName"], AgentUID: fields["agentUID"], Endpoint: fields["endpoint"]}
		if _, err := AgentIncarnationSPIFFEID(domain, registration.Namespace, registration.AgentName, registration.AgentUID); err != nil {
			return nil, errors.New("invalid registered Agent incarnation")
		}
		origin, err := url.Parse(registration.Endpoint)
		if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" || origin.RawPath != "" || (origin.Path != "" && origin.Path != "/") {
			return nil, errors.New("registered status endpoint must be a fixed HTTPS origin")
		}
		key := registration.Namespace + "/" + registration.AgentName
		if seenAgents[key] {
			return nil, errors.New("duplicate registered Agent")
		}
		seenAgents[key] = true
		registrations = append(registrations, registration)
	}
	closing, err := decoder.Token()
	var trailing json.RawMessage
	if err != nil || closing != json.Delim(']') || len(registrations) == 0 || decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("invalid operator workload status registry")
	}
	return registrations, nil
}
