// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

type healthCapableStore struct {
	gatewaybackend.Store
	store.ModelCallHealthStore
}

type drainHealthSink struct {
	store.ModelCallHealthStore
	published chan []store.ModelCallHealth
}

func (s *drainHealthSink) ReplaceModelCallHealth(_ context.Context, _ string, records []store.ModelCallHealth) error {
	s.published <- records
	return nil
}

func TestServeModelGatewayFlushesAfterInFlightRequestsDrain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	tracker := modelhealth.NewTracker()
	outcome := cpi.CallOutcome{Provider: "openai", Operation: cpi.OperationComplete, Model: "m", Reason: cpi.ReasonRateLimited, ProviderFault: true}
	tracker.RecordCall(outcome)
	sink := &drainHealthSink{published: make(chan []store.ModelCallHealth, 2)}
	reporter := modelhealth.NewReporter(tracker, sink, "gateway-pod", zap.NewNop())
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		tracker.RecordCall(outcome)
		w.WriteHeader(http.StatusNoContent)
	}), ReadHeaderTimeout: time.Second}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listenerClosed := make(chan struct{})
	serve := func(_ context.Context, server *http.Server) error {
		err := server.Serve(listener)
		close(listenerClosed)
		return err
	}
	done := make(chan error, 1)
	go func() { done <- serveModelGateway(ctx, server, reporter, serve, zap.NewNop()) }()
	requestDone := make(chan error, 1)
	go func() {
		httpClient := &http.Client{Timeout: 5 * time.Second}
		response, err := httpClient.Get("http://" + listener.Addr().String())
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never started")
	}
	cancel()
	select {
	case <-listenerClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not close the listener")
	}
	select {
	case records := <-sink.published:
		t.Fatalf("reporter stopped before the request drained: %+v", records)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not finish draining")
	}
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	select {
	case records := <-sink.published:
		if len(records) != 1 || records[0].Calls != 2 || records[0].Faults != 2 {
			t.Fatalf("final publication = %+v, want both calls including the one completed during drain", records)
		}
	default:
		t.Fatal("gateway returned before the final publication completed")
	}
}

func TestServeModelGatewayReturnsListenerErrors(t *testing.T) {
	for _, wantErr := range []error{errors.New("listen failed"), http.ErrServerClosed} {
		serve := func(context.Context, *http.Server) error { return wantErr }
		err := serveModelGateway(context.Background(), &http.Server{}, nil, serve, zap.NewNop())
		if errors.Is(wantErr, http.ErrServerClosed) {
			if err != nil {
				t.Fatalf("closed server error = %v, want nil", err)
			}
		} else if !errors.Is(err, wantErr) {
			t.Fatalf("listener error = %v, want %v", err, wantErr)
		}
	}
}

type plainStore struct{ gatewaybackend.Store }

func TestModelCallHealthStoreFollowsTheBackendMode(t *testing.T) {
	logger := zap.NewNop()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	kubernetes := &gatewaybackend.Backend{KubernetesClient: kubeClient, Store: plainStore{}}
	configMaps, err := modelCallHealthStore(kubernetes, "kognition-system", logger)
	if err != nil {
		t.Fatalf("Kubernetes mode error: %v", err)
	}
	if _, isConfigMapStore := configMaps.(*callhealth.ConfigMapStore); !isConfigMapStore {
		t.Fatalf("Kubernetes mode returned %T, want the ConfigMap store", configMaps)
	}
	// Admission policy binds each writer identity to the component in the object
	// name, so the gateway's objects must be named for the model gateway.
	if err := configMaps.ReplaceModelCallHealth(context.Background(), "gateway-pod", []store.ModelCallHealth{{Target: "t", Calls: 1}}); err != nil {
		t.Fatalf("ReplaceModelCallHealth() error: %v", err)
	}
	var published corev1.ConfigMapList
	if err := kubeClient.List(context.Background(), &published, client.InNamespace("kognition-system")); err != nil {
		t.Fatal(err)
	}
	if len(published.Items) != 1 || !strings.HasPrefix(published.Items[0].Name, callhealth.NamePrefix+"model-gateway-") ||
		published.Items[0].Labels[callhealth.HealthLabel] != "model-gateway" {
		t.Fatalf("published %+v, want one ConfigMap named and labeled for the model gateway", published.Items)
	}

	standalone := &gatewaybackend.Backend{Store: healthCapableStore{}}
	if got, err := modelCallHealthStore(standalone, "ignored", logger); err != nil || got == nil {
		t.Fatalf("standalone mode returned %v, %v, want the PostgreSQL store's health support", got, err)
	}

	if got, err := modelCallHealthStore(&gatewaybackend.Backend{Store: plainStore{}}, "ignored", logger); err != nil || got != nil {
		t.Fatalf("a backend that cannot share health returned %v, %v, want nil", got, err)
	}

	if _, err := modelCallHealthStore(kubernetes, "", logger); err == nil {
		t.Fatal("a Kubernetes backend without a namespace must be rejected")
	}
}
