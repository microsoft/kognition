// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-knowledge-pgvector serves the pgvector Knowledge Provider
// gRPC adapter (kognition.knowledge/v1alpha1) built on top of pkg/pluginsdk and
// providers/pgvector. It authorizes every query against the KnowledgeBase's
// committed backend binding, materializes and pools a namespace-local database
// connection per (configuration UID, configuration digest), and streams metrics
// and probes on a separate admin HTTP listener.
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

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	pgvectorprovider "github.com/kognition-ai/kognition/providers/pgvector"
)

const (
	providerName  = "pgvector"
	defaultListen = ":9443"
	defaultAdmin  = ":9092"
	shutdownGrace = 15 * time.Second
)

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("pgvector knowledge adapter exited", zap.Error(err))
		os.Exit(1)
	}
}

type runtimeConfig struct {
	listenAddress    string
	adminAddress     string
	providerVersion  string
	certificateFile  string
	privateKeyFile   string
	trustBundleFile  string
	clientPeerURI    string
	watchedNamespace string
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
	return config, nil
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
	return pluginsdk.KnowledgeTLSConfig{
		Certificate: certificate,
		Roots:       roots,
		PeerURI:     config.clientPeerURI,
	}, nil
}

func setupScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(k8s.AddToScheme(scheme))
	return scheme
}

func newManager(config runtimeConfig) (ctrl.Manager, error) {
	mgrOpts := ctrl.Options{
		Scheme:                        setupScheme(),
		Metrics:                       metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:        "0",
		LeaderElection:                false,
		LeaderElectionReleaseOnCancel: true,
		WebhookServer:                 ctrlwebhook.NewServer(ctrlwebhook.Options{Port: 0}),
	}
	if strings.TrimSpace(config.watchedNamespace) != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{config.watchedNamespace: {}}}
	}
	return ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOpts)
}

func pgvectorConfigurationKind() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   pgvectorprovider.APIGroup,
		Version: pgvectorprovider.APIVersion,
		Kind:    pgvectorprovider.Kind,
	}
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

	mgr, err := newManager(config)
	if err != nil {
		return fmt.Errorf("create controller-runtime manager: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	uidIndex := newKnowledgeBaseUIDIndex(logger.Named("uid-index"))
	if err := uidIndex.Register(ctx, mgr); err != nil {
		return fmt.Errorf("register KnowledgeBase informer: %w", err)
	}

	apiReader := mgr.GetAPIReader()
	resolver, err := knowledge.NewBindingResolver(apiReader, []knowledge.Registration{{
		ProviderName:      providerName,
		ConfigurationKind: pgvectorConfigurationKind(),
		EmbeddingDimension: func(configuration *unstructured.Unstructured) (int, error) {
			spec, err := pgvectorprovider.DecodeBackend(configuration)
			return int(spec.EmbeddingDimensions), err
		},
	}})
	if err != nil {
		return fmt.Errorf("configure knowledge binding resolver: %w", err)
	}

	authorizer := &scopeAuthorizer{index: uidIndex, resolver: resolver}
	databaseAddress := os.Getenv("KOGNITION_PGVECTOR_DATABASE_ADDRESS")
	host, port, err := net.SplitHostPort(databaseAddress)
	if err != nil || host == "" || port == "" {
		return errors.New("KOGNITION_PGVECTOR_DATABASE_ADDRESS must explicitly approve the database host and port")
	}
	querier := newPgvectorQuerier(apiReader, logger.Named("querier"), databaseAddress)
	defer querier.Close()

	grpcServer, err := pluginsdk.NewKnowledgeServer(pluginsdk.KnowledgeServerConfig{
		Name:    providerName,
		Version: config.providerVersion,
		TLS:     tlsConfig,
		Capabilities: &protocol.KnowledgeProviderCapabilities{
			ProtocolVersion: pluginsdk.KnowledgeProtocolVersion,
			Operations:      []string{"dense", "citations", "filter", "index", "cleanup-delete", "cleanup-retain"},
			ConfigurationKinds: []*protocol.KnowledgeConfigurationKind{{
				ApiGroup: pgvectorprovider.APIGroup,
				Kind:     pgvectorprovider.Kind,
			}},
		},
		AuthorizeQueryScope: authorizer.AuthorizeScope,
		AuthorizeIndexScope: authorizer.AuthorizeIndexScope,
		AuthorizeCleanup:    authorizer.AuthorizeCleanup,
		Probe:               uidIndex.Probe,
		Querier:             querier,
		Indexer:             querier,
		Cleaner:             querier,
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
	go func() { managerErrors <- mgr.Start(ctx) }()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		return errors.New("KnowledgeBase informer failed to sync before shutdown")
	}
	uidIndex.MarkSynced()

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", config.listenAddress)
	if err != nil {
		return fmt.Errorf("open knowledge gRPC listener: %w", err)
	}
	grpcErrors := make(chan error, 1)
	go func() {
		logger.Info("pgvector knowledge adapter serving", zap.String("address", listener.Addr().String()))
		grpcErrors <- grpcServer.Serve(listener)
	}()

	return waitForShutdown(ctx, logger, grpcServer, managerErrors, grpcErrors, adminErrors)
}

func waitForShutdown(ctx context.Context, logger *zap.Logger, grpcServer *grpc.Server,
	managerErrors, grpcErrors, adminErrors <-chan error) error {
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
		logger.Info("pgvector knowledge adapter shutdown requested")
	}
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
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
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/readyz", func(writer http.ResponseWriter, request *http.Request) {
		if err := index.Probe(request.Context()); err != nil {
			http.Error(writer, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("pgvector knowledge adapter admin listening", zap.String("address", server.Addr))
		errCh <- server.ListenAndServe()
	}()
	return server, errCh
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
