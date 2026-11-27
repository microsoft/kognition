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

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type knowledgeHealthSink struct {
	store.ModelCallHealthStore
	published chan []store.ModelCallHealth
}

func (s *knowledgeHealthSink) ReplaceModelCallHealth(_ context.Context, _ string, records []store.ModelCallHealth) error {
	s.published <- records
	return nil
}

type knowledgeHealthBackend struct {
	gatewaybackend.Store
	store.ModelCallHealthStore
}

func TestKnowledgeCallHealthUsesItsOwnBackendIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	backend := &gatewaybackend.Backend{KubernetesClient: kubeClient}
	base := observability.NewCPICallMetrics(prometheus.NewRegistry())
	recorder, reporter, err := knowledgeCallHealth(backend, "control", base, zap.NewNop())
	if err != nil || recorder == nil || reporter == nil {
		t.Fatalf("assembly = %v, %v, %v", recorder, reporter, err)
	}
	recorder.RecordCall(cpi.CallOutcome{Provider: "openai", Operation: cpi.OperationEmbed, Model: "m", Reason: cpi.ReasonRateLimited, ProviderFault: true})
	if err := reporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var objects corev1.ConfigMapList
	if err := kubeClient.List(context.Background(), &objects, client.InNamespace("control")); err != nil {
		t.Fatal(err)
	}
	if len(objects.Items) != 1 || !strings.HasPrefix(objects.Items[0].Name, callhealth.NamePrefix+"knowledge-gateway-") ||
		objects.Items[0].Labels[callhealth.HealthLabel] != "knowledge-gateway" {
		t.Fatalf("publications = %+v, want the Knowledge Gateway's own component", objects.Items)
	}
	if recorder, reporter, err := knowledgeCallHealth(backend, "", base, zap.NewNop()); err == nil || recorder != nil || reporter != nil {
		t.Fatalf("invalid namespace silently accepted: %v, %v, %v", recorder, reporter, err)
	}
	sink := &knowledgeHealthSink{published: make(chan []store.ModelCallHealth, 1)}
	shared := &gatewaybackend.Backend{Store: knowledgeHealthBackend{ModelCallHealthStore: sink}}
	recorder, reporter, err = knowledgeCallHealth(shared, "", base, zap.NewNop())
	if err != nil || reporter == nil {
		t.Fatalf("shared backend = %v, %v", reporter, err)
	}
	recorder.RecordCall(cpi.CallOutcome{Provider: "openai", Operation: cpi.OperationEmbed, Model: "m", Reason: cpi.ReasonTimeout, ProviderFault: true})
	if err := reporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if records := <-sink.published; len(records) != 1 || records[0].Faults != 1 {
		t.Fatalf("shared publication = %+v", records)
	}
	if recorder, reporter, err := knowledgeCallHealth(&gatewaybackend.Backend{}, "", base, zap.NewNop()); err != nil || recorder != base || reporter != nil {
		t.Fatalf("backend without shared health = %v, %v, %v", recorder, reporter, err)
	}
}

func TestServeKnowledgeGatewayPublishesAfterRequestsDrain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	tracker := modelhealth.NewTracker()
	sink := &knowledgeHealthSink{published: make(chan []store.ModelCallHealth, 2)}
	reporter := modelhealth.NewReporter(tracker, sink, "knowledge-pod", zap.NewNop())
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		tracker.RecordCall(cpi.CallOutcome{Provider: "openai", Operation: cpi.OperationEmbed, Model: "m", Reason: cpi.ReasonRateLimited, ProviderFault: true})
		w.WriteHeader(http.StatusNoContent)
	})}
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveKnowledgeGateway(ctx, server, reporter, func() error {
			err := server.Serve(listener)
			close(closed)
			return err
		})
	}()
	requestDone := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			err = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never started")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown never closed the listener")
	}
	select {
	case records := <-sink.published:
		t.Fatalf("published before drain completed: %+v", records)
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
		if len(records) != 1 || records[0].Faults != 1 {
			t.Fatalf("final publication = %+v, want the drained embedding failure", records)
		}
	default:
		t.Fatal("gateway returned before final publication")
	}
}

func TestServeKnowledgeGatewayPreservesListenerErrors(t *testing.T) {
	for _, want := range []error{errors.New("listen failed"), http.ErrServerClosed} {
		err := serveKnowledgeGateway(context.Background(), &http.Server{}, nil, func() error { return want })
		if errors.Is(want, http.ErrServerClosed) {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, want) {
			t.Fatalf("listener error = %v, want %v", err, want)
		}
	}
}
