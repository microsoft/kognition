// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-knowledge-gateway authorizes runtime Knowledge queries and
// forwards exact committed scopes to an installation-owned provider route.
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/knowledgegateway"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	"github.com/kognition-ai/kognition/providers/azuresearch"
	"github.com/kognition-ai/kognition/providers/pgvector"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const shutdownGrace = 10 * time.Second

type runtimeConfig struct {
	listenAddress          string
	providerName           string
	providerAddress        string
	configurationKind      schema.GroupVersionKind
	certificateFile        string
	privateKeyFile         string
	trustBundleFile        string
	runtimeCertificateFile string
	runtimePrivateKeyFile  string
	providerPeerURI        string
	providerServerName     string
	capabilityKeyID        string
	capabilityKey          string
	previousKeyID          string
	previousKey            string
}

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = logger.Sync() }()
	if err := run(logger); err != nil {
		logger.Error("Knowledge Gateway exited", zap.Error(err))
		os.Exit(1)
	}
}

func loadConfig() (runtimeConfig, error) {
	config := runtimeConfig{
		listenAddress:   envOrDefault("LISTEN_ADDR", ":8095"),
		providerName:    os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_NAME"),
		providerAddress: os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_ADDRESS"),
		configurationKind: schema.GroupVersionKind{
			Group:   os.Getenv("KOGNITION_KNOWLEDGE_CONFIGURATION_API_GROUP"),
			Version: os.Getenv("KOGNITION_KNOWLEDGE_CONFIGURATION_API_VERSION"),
			Kind:    os.Getenv("KOGNITION_KNOWLEDGE_CONFIGURATION_KIND"),
		},
		certificateFile:        os.Getenv("KOGNITION_KNOWLEDGE_TLS_CERT_FILE"),
		privateKeyFile:         os.Getenv("KOGNITION_KNOWLEDGE_TLS_KEY_FILE"),
		trustBundleFile:        os.Getenv("KOGNITION_KNOWLEDGE_TLS_CA_FILE"),
		runtimeCertificateFile: os.Getenv("KOGNITION_KNOWLEDGE_RUNTIME_TLS_CERT_FILE"),
		runtimePrivateKeyFile:  os.Getenv("KOGNITION_KNOWLEDGE_RUNTIME_TLS_KEY_FILE"),
		providerPeerURI:        os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_PEER_URI"),
		providerServerName:     os.Getenv("KOGNITION_KNOWLEDGE_PROVIDER_SERVER_NAME"),
		capabilityKeyID:        os.Getenv("KNOWLEDGE_CAPABILITY_HMAC_KEY_ID"),
		capabilityKey:          os.Getenv("KNOWLEDGE_CAPABILITY_HMAC_KEY"),
		previousKeyID:          os.Getenv("KNOWLEDGE_CAPABILITY_HMAC_PREVIOUS_KEY_ID"),
		previousKey:            os.Getenv("KNOWLEDGE_CAPABILITY_HMAC_PREVIOUS_KEY"),
	}
	if _, _, err := net.SplitHostPort(config.listenAddress); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid LISTEN_ADDR %q: %w", config.listenAddress, err)
	}
	if _, _, err := net.SplitHostPort(config.providerAddress); err != nil {
		return runtimeConfig{}, fmt.Errorf("invalid KOGNITION_KNOWLEDGE_PROVIDER_ADDRESS %q: %w", config.providerAddress, err)
	}
	if strings.TrimSpace(config.providerName) == "" || config.configurationKind.Empty() || config.certificateFile == "" || config.privateKeyFile == "" || config.trustBundleFile == "" ||
		config.runtimeCertificateFile == "" || config.runtimePrivateKeyFile == "" ||
		strings.TrimSpace(config.providerPeerURI) == "" || strings.TrimSpace(config.providerServerName) == "" {
		return runtimeConfig{}, errors.New("knowledge provider mTLS and runtime-facing server TLS configuration are required")
	}
	return config, nil
}

func run(logger *zap.Logger) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(config.capabilityKeyID, []byte(config.capabilityKey), config.previousKeyID, []byte(config.previousKey))
	if err != nil {
		return fmt.Errorf("configure knowledge capability key ring: %w", err)
	}
	issuer, err := runtimeauth.NewKnowledgeSessionCapabilityIssuerWithKeyRing(keys, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("configure knowledge capability verification: %w", err)
	}
	tlsConfig, err := loadTLSMaterial(config)
	if err != nil {
		return err
	}
	provider, err := pluginsdk.NewKnowledgeClient(pluginsdk.KnowledgeClientConfig{Name: config.providerName, Address: config.providerAddress, TLS: tlsConfig})
	if err != nil {
		return fmt.Errorf("configure knowledge provider client: %w", err)
	}
	defer func() { _ = provider.Close() }()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(k8s.AddToScheme(scheme))
	kubeConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	reader, err := ctrlclient.New(kubeConfig, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes reader: %w", err)
	}
	registration := knowledge.Registration{ProviderName: config.providerName, ConfigurationKind: config.configurationKind}
	switch config.configurationKind {
	case (schema.GroupVersionKind{Group: pgvector.APIGroup, Version: pgvector.APIVersion, Kind: pgvector.Kind}):
		registration.EmbeddingDimension = func(configuration *unstructured.Unstructured) (int, error) {
			spec, err := pgvector.DecodeBackend(configuration)
			return int(spec.EmbeddingDimensions), err
		}
	case (schema.GroupVersionKind{Group: azuresearch.APIGroup, Version: azuresearch.APIVersion, Kind: azuresearch.Kind}):
		registration.EmbeddingDimension = func(configuration *unstructured.Unstructured) (int, error) {
			spec, err := azuresearch.DecodeBackend(configuration)
			return int(spec.EmbeddingDimensions), err
		}
	}
	resolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{registration})
	if err != nil {
		return fmt.Errorf("configure knowledge binding resolver: %w", err)
	}
	validator, err := knowledgegateway.NewKubernetesCapabilityBindingValidator(reader)
	if err != nil {
		return fmt.Errorf("configure knowledge capability binding validator: %w", err)
	}
	backendContext, cancelBackend := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelBackend()
	controlNamespace := os.Getenv("KOGNITION_COST_NAMESPACE")
	backend, err := gatewaybackend.Open(backendContext, gatewaybackend.Config{
		Mode: os.Getenv("KOGNITION_MODE"), DatabaseURL: os.Getenv("DATABASE_URL"),
		ControlNamespace: controlNamespace, AuditNamespace: controlNamespace,
	}, logger)
	if err != nil {
		return fmt.Errorf("configure Knowledge Gateway state backend: %w", err)
	}
	defer backend.Close()
	policies := controller.NewInvocationPolicyEngine(
		controller.NewToolGrantEngine(backend.Store, logger.Named("embedding-grants")),
		controller.NewAgentPolicyReconciler(backend.Store, opa.NewEngine(), logger.Named("embedding-policies")),
	)
	quota := controller.NewQuotaEnforcer(backend.Store, controller.NewCognitiveQuotaReconciler(backend.Store, logger.Named("quota")), logger.Named("quota"))
	tracePipeline, err := observability.NewTracePipelineFromEnv(context.Background())
	if err != nil {
		return fmt.Errorf("configure Knowledge Gateway tracing: %w", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tracePipeline.Shutdown(shutdown); err != nil {
			logger.Error("shutdown Knowledge Gateway tracing", zap.Error(err))
		}
	}()
	usageWriter := observability.NewModelUsageMetrics(ctrlmetrics.Registry).Wrap(backend.Cost)
	tracePipeline.Tracer.SetFallibleCostSink(func(record observability.CostRecord) error {
		write, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := usageWriter.ApplyUsageEvent(write, record)
		return err
	})
	audit := observability.NewCognitiveAuditLog(observability.NewTrustDecisionMetrics(ctrlmetrics.Registry).Wrap(backend.Audit))
	embedder, err := modelgateway.NewEmbeddingExecutor(backend.Store, policies, &http.Client{}, logger,
		modelgateway.WithQuotaChecker(quota), modelgateway.WithAuditLog(audit), modelgateway.WithObservability(tracePipeline.Tracer, tracePipeline.Collector),
		modelgateway.WithCallRecorder(observability.NewCPICallMetrics(ctrlmetrics.Registry)))
	if err != nil {
		return fmt.Errorf("configure governed query embedding: %w", err)
	}
	server, err := knowledgegateway.New(issuer, validator, resolver, map[string]knowledgegateway.QueryClient{config.providerName: provider}, logger,
		knowledgegateway.WithQueryEmbedder(embedder), knowledgegateway.WithQueryAuditLog(audit))
	if err != nil {
		return fmt.Errorf("create Knowledge Gateway: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	mux.Handle("/", server)
	httpServer := &http.Server{Addr: config.listenAddress, Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 35 * time.Second, WriteTimeout: 35 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("Knowledge Gateway starting", zap.String("address", httpServer.Addr), zap.String("provider", config.providerName))
		serverErrors <- httpServer.ListenAndServeTLS(config.runtimeCertificateFile, config.runtimePrivateKeyFile)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancelShutdown()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func loadTLSMaterial(config runtimeConfig) (pluginsdk.KnowledgeTLSConfig, error) {
	certificate, err := tls.LoadX509KeyPair(config.certificateFile, config.privateKeyFile)
	if err != nil {
		return pluginsdk.KnowledgeTLSConfig{}, fmt.Errorf("load knowledge client certificate: %w", err)
	}
	bundle, err := os.ReadFile(config.trustBundleFile)
	if err != nil {
		return pluginsdk.KnowledgeTLSConfig{}, fmt.Errorf("load knowledge trust bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return pluginsdk.KnowledgeTLSConfig{}, errors.New("knowledge trust bundle contains no usable certificates")
	}
	return pluginsdk.KnowledgeTLSConfig{Certificate: certificate, Roots: roots, PeerURI: config.providerPeerURI, ServerName: config.providerServerName}, nil
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
