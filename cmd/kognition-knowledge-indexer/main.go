// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/ingestion"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

const (
	defaultSpoolFile  = "/var/run/kognition/index/chunks.jsonl"
	defaultResultFile = "/dev/termination-log"
	maxConfigBytes    = 64 << 10
)

type transportConfig struct {
	ProviderName          string                   `json:"providerName"`
	ProviderAddress       string                   `json:"providerAddress"`
	OperationID           string                   `json:"operationId"`
	Dimension             int                      `json:"dimension"`
	ClassUID              string                   `json:"classUID"`
	ClassGeneration       int64                    `json:"classGeneration"`
	Scope                 *protocol.KnowledgeScope `json:"scope"`
	TLS                   transportTLSConfig       `json:"tls"`
	EmbeddingModelBinding *api.ModelVersionBinding `json:"embeddingModelBinding,omitempty"`
}

type transportTLSConfig struct {
	CertificateFile string `json:"certificateFile"`
	PrivateKeyFile  string `json:"privateKeyFile"`
	TrustBundleFile string `json:"trustBundleFile"`
	ProviderPeerURI string `json:"providerPeerUri"`
	ServerName      string `json:"serverName"`
}

type indexClient interface {
	Index(context.Context, []*protocol.KnowledgeIndexBatchRequest) (*protocol.KnowledgeIndexResponse, error)
}

type transportResult struct {
	Documents            int64                `json:"documents"`
	Chunks               int64                `json:"chunks"`
	SourceDigest         string               `json:"sourceDigest"`
	BackendBinding       backendBindingResult `json:"backendBinding"`
	ProviderConfirmation providerConfirmation `json:"providerConfirmation"`
}

type backendBindingResult struct {
	Generation          int64  `json:"generation"`
	ProviderName        string `json:"providerName"`
	ClassUID            string `json:"classUID"`
	ClassGeneration     int64  `json:"classGeneration"`
	ConfigurationUID    string `json:"configurationUID"`
	ConfigurationDigest string `json:"configurationDigest"`
}

type providerConfirmation struct {
	OperationID        string `json:"operationId"`
	DocumentCount      int64  `json:"documentCount"`
	IndexedChunks      int64  `json:"indexedChunks"`
	SourceDigest       string `json:"sourceDigest"`
	BackendOperationID string `json:"backendOperationId"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configFile := strings.TrimSpace(os.Getenv("KOGNITION_KNOWLEDGE_INDEX_CONFIG_FILE"))
	if configFile == "" {
		return errors.New("KOGNITION_KNOWLEDGE_INDEX_CONFIG_FILE is required")
	}
	config, err := loadTransportConfig(configFile)
	if err != nil {
		return err
	}
	tlsConfig, err := loadTLSMaterial(config.TLS)
	if err != nil {
		return err
	}
	client, err := pluginsdk.NewKnowledgeClient(pluginsdk.KnowledgeClientConfig{
		Name: config.ProviderName, Address: config.ProviderAddress, TLS: tlsConfig,
	})
	if err != nil {
		return fmt.Errorf("configure knowledge provider client: %w", err)
	}
	defer func() { _ = client.Close() }()
	spoolFile := envOrDefault("KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE", defaultSpoolFile)
	spool, err := os.Open(spoolFile)
	if err != nil {
		return fmt.Errorf("open provider spool: %w", err)
	}
	defer func() { _ = spool.Close() }()
	result, err := transport(context.Background(), config, spool, client)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode provider confirmation: %w", err)
	}
	fmt.Println(string(encoded))
	resultFile := envOrDefault("KOGNITION_KNOWLEDGE_INDEX_RESULT_FILE", defaultResultFile)
	if err := os.WriteFile(resultFile, encoded, 0o600); err != nil {
		return fmt.Errorf("write provider confirmation: %w", err)
	}
	return nil
}

func loadTransportConfig(path string) (transportConfig, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return transportConfig{}, fmt.Errorf("read knowledge index configuration: %w", err)
	}
	if len(content) == 0 || len(content) > maxConfigBytes {
		return transportConfig{}, errors.New("knowledge index configuration has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var config transportConfig
	if err := decoder.Decode(&config); err != nil {
		return transportConfig{}, fmt.Errorf("decode knowledge index configuration: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return transportConfig{}, errors.New("knowledge index configuration contains trailing JSON")
	}
	if strings.TrimSpace(config.ProviderName) == "" || strings.TrimSpace(config.ProviderAddress) == "" || strings.TrimSpace(config.OperationID) == "" ||
		config.Dimension < 1 || strings.TrimSpace(config.ClassUID) == "" || config.ClassGeneration < 1 || config.Scope == nil {
		return transportConfig{}, errors.New("knowledge index configuration is incomplete")
	}
	return config, nil
}

func loadTLSMaterial(config transportTLSConfig) (pluginsdk.KnowledgeTLSConfig, error) {
	if config.CertificateFile == "" || config.PrivateKeyFile == "" || config.TrustBundleFile == "" || strings.TrimSpace(config.ProviderPeerURI) == "" || strings.TrimSpace(config.ServerName) == "" {
		return pluginsdk.KnowledgeTLSConfig{}, errors.New("knowledge index mTLS configuration is required")
	}
	certificate, err := tls.LoadX509KeyPair(config.CertificateFile, config.PrivateKeyFile)
	if err != nil {
		return pluginsdk.KnowledgeTLSConfig{}, fmt.Errorf("load knowledge index client certificate: %w", err)
	}
	bundle, err := os.ReadFile(config.TrustBundleFile)
	if err != nil {
		return pluginsdk.KnowledgeTLSConfig{}, fmt.Errorf("load knowledge index trust bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return pluginsdk.KnowledgeTLSConfig{}, errors.New("knowledge index trust bundle contains no usable certificates")
	}
	return pluginsdk.KnowledgeTLSConfig{Certificate: certificate, Roots: roots, PeerURI: config.ProviderPeerURI, ServerName: config.ServerName}, nil
}

func transport(ctx context.Context, config transportConfig, spool io.Reader, client indexClient) (*transportResult, error) {
	prepared, err := ingestion.ReadProviderSpool(spool, ingestion.ProviderSpoolOptions{
		Scope: config.Scope, OperationID: config.OperationID, ExpectedDimension: config.Dimension,
	})
	if err != nil {
		return nil, fmt.Errorf("validate provider spool: %w", err)
	}
	response, err := client.Index(ctx, prepared.Batches)
	if err != nil {
		return nil, fmt.Errorf("index provider spool: %w", err)
	}
	if err := ingestion.ProviderSpoolConfirmation(prepared, response); err != nil {
		return nil, err
	}
	return &transportResult{
		Documents: prepared.SourceDocuments, Chunks: response.IndexedChunks, SourceDigest: response.SourceDigest,
		BackendBinding: backendBindingResult{
			Generation: response.Scope.Generation, ProviderName: config.ProviderName,
			ClassUID: config.ClassUID, ClassGeneration: config.ClassGeneration,
			ConfigurationUID: response.Scope.ConfigurationUid, ConfigurationDigest: response.Scope.ConfigurationDigest,
		},
		ProviderConfirmation: providerConfirmation{
			OperationID: response.OperationId, DocumentCount: response.DocumentCount,
			IndexedChunks: response.IndexedChunks, SourceDigest: response.SourceDigest,
			BackendOperationID: response.BackendOperationId,
		},
	}, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
