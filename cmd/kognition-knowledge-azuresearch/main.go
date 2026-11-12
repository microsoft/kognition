// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-knowledge-azuresearch serves the Azure AI Search Knowledge
// Provider gRPC adapter using Microsoft Entra workload identity.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	azuresearchprovider "github.com/kognition-ai/kognition/providers/azuresearch"
)

const (
	providerName  = "azuresearch"
	defaultListen = ":9443"
	defaultAdmin  = ":9092"
	shutdownGrace = 15 * time.Second
)

type runtimeConfig struct {
	listenAddress    string
	adminAddress     string
	providerVersion  string
	certificateFile  string
	privateKeyFile   string
	trustBundleFile  string
	clientPeerURI    string
	watchedNamespace string
	azureTenantID    string
	azureClientID    string
	azureTokenFile   string
}

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("azure search knowledge adapter exited", zap.Error(err))
		os.Exit(1)
	}
}

func loadConfig() (runtimeConfig, error) {
	config := runtimeConfig{
		listenAddress:    envOrDefault("KOGNITION_KNOWLEDGE_LISTEN_ADDRESS", defaultListen),
		adminAddress:     envOrDefault("KOGNITION_KNOWLEDGE_ADMIN_ADDRESS", defaultAdmin),
		providerVersion:  envOrDefault("KOGNITION_KNOWLEDGE_PROVIDER_VERSION", "0.0.0-dev"),
		certificateFile:  os.Getenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE"),
		privateKeyFile:   os.Getenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE"),
		trustBundleFile:  os.Getenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE"),
		clientPeerURI:    os.Getenv("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI"),
		watchedNamespace: os.Getenv("KOGNITION_KNOWLEDGE_WATCH_NAMESPACE"),
		azureTenantID:    strings.TrimSpace(os.Getenv("AZURE_TENANT_ID")),
		azureClientID:    strings.TrimSpace(os.Getenv("AZURE_CLIENT_ID")),
		azureTokenFile:   strings.TrimSpace(os.Getenv("AZURE_FEDERATED_TOKEN_FILE")),
	}
	if _, _, err := net.SplitHostPort(config.listenAddress); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid KOGNITION_KNOWLEDGE_LISTEN_ADDRESS %q: %w", config.listenAddress, err)
	}
	if _, _, err := net.SplitHostPort(config.adminAddress); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid KOGNITION_KNOWLEDGE_ADMIN_ADDRESS %q: %w", config.adminAddress, err)
	}
	if config.certificateFile == "" || config.privateKeyFile == "" || config.trustBundleFile == "" {
		return runtimeConfig{}, errors.New("KOGNITION_KNOWLEDGE_TLS_CERT_FILE, _KEY_FILE and _CA_FILE are required")
	}
	if strings.TrimSpace(config.clientPeerURI) == "" {
		return runtimeConfig{}, errors.New("KOGNITION_KNOWLEDGE_CLIENT_PEER_URI is required")
	}
	if config.azureTenantID == "" || config.azureClientID == "" || config.azureTokenFile == "" {
		return runtimeConfig{}, errors.New("AZURE_TENANT_ID, AZURE_CLIENT_ID and AZURE_FEDERATED_TOKEN_FILE are required")
	}
	return config, nil
}

func newAzureCredential(config runtimeConfig) (*azidentity.WorkloadIdentityCredential, error) {
	credential, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		TenantID: config.azureTenantID, ClientID: config.azureClientID, TokenFilePath: config.azureTokenFile,
	})
	if err != nil {
		return nil, fmt.Errorf("create Azure workload identity credential: %w", err)
	}
	return credential, nil
}

func loadTLSMaterial(config runtimeConfig) (pluginsdk.KnowledgeTLSConfig, error) {
	certificate, err := tls.LoadX509KeyPair(config.certificateFile, config.privateKeyFile)
	if err != nil {
		return pluginsdk.KnowledgeTLSConfig{}, fmt.Errorf("load knowledge server certificate: %w", err)
	}
	trustBundle, err := os.ReadFile(config.trustBundleFile)
	if err != nil {
		return pluginsdk.KnowledgeTLSConfig{}, fmt.Errorf("load knowledge trust bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trustBundle) {
		return pluginsdk.KnowledgeTLSConfig{}, errors.New("knowledge trust bundle contains no usable certificates")
	}
	return pluginsdk.KnowledgeTLSConfig{Certificate: certificate, Roots: roots, PeerURI: config.clientPeerURI}, nil
}

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(k8s.AddToScheme(scheme))
	return scheme
}

func newManager(config runtimeConfig) (ctrl.Manager, error) {
	options := ctrl.Options{
		Scheme: setupScheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		LeaderElection: false, LeaderElectionReleaseOnCancel: true,
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{Port: 0}),
	}
	if strings.TrimSpace(config.watchedNamespace) != "" {
		options.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{config.watchedNamespace: {}}}
	}
	return ctrl.NewManager(ctrl.GetConfigOrDie(), options)
}

func azureSearchConfigurationKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: azuresearchprovider.APIGroup, Version: azuresearchprovider.APIVersion, Kind: azuresearchprovider.Kind}
}

func run(logger *zap.Logger) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	tlsConfig, err := loadTLSMaterial(config)
	if err != nil {
		return err
	}
	credential, err := newAzureCredential(config)
	if err != nil {
		return err
	}
	manager, err := newManager(config)
	if err != nil {
		return fmt.Errorf("create controller-runtime manager: %w", err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	uidIndex := newKnowledgeBaseUIDIndex(logger.Named("uid-index"))
	if err := uidIndex.Register(ctx, manager); err != nil {
		return fmt.Errorf("register KnowledgeBase informer: %w", err)
	}
	resolver, err := knowledge.NewBindingResolver(manager.GetAPIReader(), []knowledge.Registration{{ProviderName: providerName, ConfigurationKind: azureSearchConfigurationKind()}})
	if err != nil {
		return fmt.Errorf("configure knowledge binding resolver: %w", err)
	}
	querier := newAzureSearchQuerier(manager.GetAPIReader(), credential, logger.Named("querier"))
	indexer := newAzureSearchIndexer(manager.GetAPIReader(), credential)
	cleaner := newAzureSearchCleaner(credential)
	defer querier.Close()
	authorizer := &scopeAuthorizer{index: uidIndex, resolver: resolver}
	grpcServer, err := pluginsdk.NewKnowledgeServer(pluginsdk.KnowledgeServerConfig{
		Name: providerName, Version: config.providerVersion, TLS: tlsConfig,
		Capabilities: &protocol.KnowledgeProviderCapabilities{
			ProtocolVersion:    pluginsdk.KnowledgeProtocolVersion,
			Operations:         []string{"keyword", "dense", "hybrid", "citations", "index", "cleanup-delete", "cleanup-retain"},
			ConfigurationKinds: []*protocol.KnowledgeConfigurationKind{{ApiGroup: azuresearchprovider.APIGroup, Kind: azuresearchprovider.Kind}},
		},
		AuthorizeQueryScope: authorizer.AuthorizeQueryScope,
		AuthorizeIndexScope: authorizer.AuthorizeIndexScope,
		AuthorizeCleanup:    authorizer.AuthorizeCleanup,
		Probe:               uidIndex.Probe, Querier: querier, Indexer: indexer, Cleaner: cleaner,
	})
	if err != nil {
		return fmt.Errorf("create knowledge gRPC server: %w", err)
	}
	adminServer, adminErrors := startAdminServer(config.adminAddress, uidIndex, logger)
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancelShutdown()
		if err := adminServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown admin server", zap.Error(err))
		}
	}()
	managerErrors := make(chan error, 1)
	go func() { managerErrors <- manager.Start(ctx) }()
	if !manager.GetCache().WaitForCacheSync(ctx) {
		return errors.New("KnowledgeBase informer failed to sync before shutdown")
	}
	uidIndex.MarkSynced()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", config.listenAddress)
	if err != nil {
		return fmt.Errorf("open knowledge gRPC listener: %w", err)
	}
	grpcErrors := make(chan error, 1)
	go func() {
		logger.Info("azure search knowledge adapter serving", zap.String("address", listener.Addr().String()))
		grpcErrors <- grpcServer.Serve(listener)
	}()
	return waitForShutdown(ctx, logger, grpcServer, managerErrors, grpcErrors, adminErrors)
}

func waitForShutdown(ctx context.Context, logger *zap.Logger, grpcServer *grpc.Server, managerErrors, grpcErrors, adminErrors <-chan error) error {
	select {
	case err := <-managerErrors:
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("controller-runtime manager exited: %w", err)
		}
	case err := <-grpcErrors:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("knowledge gRPC server exited: %w", err)
		}
	case err := <-adminErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("admin HTTP server exited: %w", err)
		}
	case <-ctx.Done():
		logger.Info("azure search knowledge adapter shutdown requested")
	}
	stopped := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(shutdownGrace):
		grpcServer.Stop()
	}
	return nil
}

func startAdminServer(address string, index *knowledgeBaseUIDIndex, logger *zap.Logger) (*http.Server, <-chan error) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, request *http.Request) {
		if err := index.Probe(request.Context()); err != nil {
			http.Error(writer, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	errors := make(chan error, 1)
	go func() {
		logger.Info("azure search knowledge adapter admin listening", zap.String("address", server.Addr))
		errors <- server.ListenAndServe()
	}()
	return server, errors
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
