// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"os"
	"testing"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"go.uber.org/zap/zaptest"
)

func TestKnowledgeCapabilitiesGeneratedWireCompatibility(t *testing.T) {
	generated := &protocol.GetProviderCapabilitiesResponse{Knowledge: &protocol.KnowledgeProviderCapabilities{
		ProtocolVersion:    KnowledgeProtocolVersion,
		ConfigurationKinds: []*protocol.KnowledgeConfigurationKind{{ApiGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend"}},
		Operations:         []string{"keyword"},
	}}
	codec := JSONCodec{}
	payload, err := codec.Marshal(generated)
	if err != nil {
		t.Fatal(err)
	}
	var response GetProviderCapabilitiesResponse
	if err := codec.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if response.Knowledge == nil || response.Knowledge.ProtocolVersion != KnowledgeProtocolVersion || len(response.Knowledge.ConfigurationKinds) != 1 || response.Knowledge.ConfigurationKinds[0].APIGroup != "pgvector.knowledge.kognition.dev" {
		t.Fatalf("generated provider capability metadata was lost: %#v", response.Knowledge)
	}
}

func TestKnowledgeProviderDiscovery(t *testing.T) {
	directory, err := os.MkdirTemp("", "cpi-knowledge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	reference := v1alpha1.KnowledgeBackendReference{APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared"}
	provider := &fakeCPIProvider{
		name: "pgvector", version: "1.0.0", dimension: "knowledge", healthy: true,
		capabilities: []string{"knowledge"},
		knowledge: &KnowledgeProviderCapabilities{
			ProtocolVersion:    KnowledgeProtocolVersion,
			ConfigurationKinds: []KnowledgeConfigurationKind{{APIGroup: reference.APIGroup, Kind: reference.Kind}},
			Operations:         []string{"keyword", "citations"},
		},
	}
	socketPath, server := startFakeProvider(t, directory, "pgvector", provider)
	config := DefaultProviderHostConfig()
	config.SocketDir = directory
	host := NewProviderHost(config, zaptest.NewLogger(t))
	t.Cleanup(host.Stop)
	if err := host.discover(); err != nil {
		t.Fatal(err)
	}
	waitForProviderReady(t, host, socketPath, providerHostWait)
	for _, snapshot := range [][]v1alpha1.ProviderEntry{host.GetProviders(), host.GetProvidersByDimension("knowledge")} {
		if len(snapshot) != 1 || snapshot[0].Knowledge == nil {
			t.Fatalf("missing discovered metadata: %#v", snapshot)
		}
		snapshot[0].Knowledge.ProtocolVersion = "changed"
		snapshot[0].Knowledge.Operations[0] = "archive"
		snapshot[0].Knowledge.ConfigurationKinds[0].Kind = "Changed"
	}
	entry, err := host.GetReadyProvider("knowledge", "pgvector")
	if err != nil {
		t.Fatal(err)
	}
	entry.Knowledge.Operations[0] = "archive"
	resolved, err := host.ResolveKnowledgeProvider("pgvector", reference, []string{"keyword", "citations"})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SocketPath != socketPath {
		t.Fatalf("resolved socket = %q, want %q", resolved.SocketPath, socketPath)
	}
	server.Stop()
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := host.discover(); err != nil {
		t.Fatal(err)
	}
	if entry, err := host.ResolveKnowledgeProvider("pgvector", reference, []string{"keyword"}); err == nil || entry != nil {
		t.Fatalf("removed provider still resolved: entry=%#v, error=%v", entry, err)
	}
}

func TestResolveKnowledgeProvider(t *testing.T) {
	reference := v1alpha1.KnowledgeBackendReference{APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared"}
	for _, test := range []struct {
		name      string
		mutate    func(*ProviderHost, *v1alpha1.ProviderEntry)
		wantError bool
	}{
		{name: "compatible"},
		{name: "missing", wantError: true, mutate: func(host *ProviderHost, _ *v1alpha1.ProviderEntry) { host.providers = nil }},
		{name: "wrong provider", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) { entry.Name = "another" }},
		{name: "wrong dimension", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) { entry.Dimension = "memory" }},
		{name: "unhealthy", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) {
			entry.HealthStatus = v1alpha1.ProviderHealthUnavailable
		}},
		{name: "disconnected", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) {
			entry.ConnectionState = v1alpha1.ProviderConnectionDisconnected
		}},
		{name: "legacy", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) { entry.Knowledge = nil }},
		{name: "incompatible version", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) {
			entry.Knowledge.ProtocolVersion = "kognition.knowledge/v2"
		}},
		{name: "wrong group", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) {
			entry.Knowledge.ConfigurationKinds[0].APIGroup = "other.example"
		}},
		{name: "wrong kind", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) {
			entry.Knowledge.ConfigurationKinds[0].Kind = "OtherBackend"
		}},
		{name: "missing operation", wantError: true, mutate: func(_ *ProviderHost, entry *v1alpha1.ProviderEntry) { entry.Knowledge.Operations = []string{"keyword"} }},
		{name: "duplicate identity", wantError: true, mutate: func(host *ProviderHost, entry *v1alpha1.ProviderEntry) {
			host.providers["other.sock"] = &ProviderConnection{Info: cloneProviderEntry(*entry)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := &ProviderConnection{Info: v1alpha1.ProviderEntry{
				Name: "pgvector", Dimension: "knowledge", SocketPath: "provider.sock",
				HealthStatus: v1alpha1.ProviderHealthReady, ConnectionState: v1alpha1.ProviderConnectionConnected,
				Knowledge: &v1alpha1.KnowledgeProviderCapabilities{
					ProtocolVersion:    KnowledgeProtocolVersion,
					ConfigurationKinds: []v1alpha1.KnowledgeConfigurationKind{{APIGroup: reference.APIGroup, Kind: reference.Kind}},
					Operations:         []string{"keyword", "citations", "refresh", "delete"},
				},
			}}
			host := NewProviderHost(DefaultProviderHostConfig(), zaptest.NewLogger(t))
			host.providers["provider.sock"] = connection
			if test.mutate != nil {
				test.mutate(host, &connection.Info)
			}
			entry, err := host.ResolveKnowledgeProvider("pgvector", reference, []string{"keyword", "citations"})
			if (err != nil) != test.wantError {
				t.Fatalf("entry = %#v, error = %v", entry, err)
			}
			if test.wantError {
				if entry != nil {
					t.Fatal("failed resolution returned a provider")
				}
				return
			}
			entry.Knowledge.Operations[0] = "archive"
			entry.Knowledge.ConfigurationKinds[0].Kind = "Changed"
			if _, err := host.ResolveKnowledgeProvider("pgvector", reference, []string{"keyword", "citations"}); err != nil {
				t.Fatalf("caller mutated registration: %v", err)
			}
			if _, err := host.ResolveKnowledgeProvider("", reference, nil); err == nil {
				t.Fatal("empty provider name accepted")
			}
			if _, err := host.ResolveKnowledgeProvider("pgvector", reference, []string{"unknown"}); err == nil {
				t.Fatal("unknown operation accepted")
			}
		})
	}
}
