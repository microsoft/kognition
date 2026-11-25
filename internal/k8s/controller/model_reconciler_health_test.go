// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

type healthyOpenAIDriver struct{}

func (healthyOpenAIDriver) Name() string { return "openai-compatible" }
func (healthyOpenAIDriver) Validate(context.Context, *api.ModelSpec, *api.ModelClassSpec) error {
	return nil
}
func (healthyOpenAIDriver) Probe(context.Context, *api.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true, Reason: "ProbeOK", Message: "probe passed"}, nil
}
func (healthyOpenAIDriver) Complete(context.Context, *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	return nil, nil
}
func (healthyOpenAIDriver) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

type staticCallHealth struct{ records []store.ModelCallHealth }

func (s staticCallHealth) ReplaceModelCallHealth(context.Context, string, []store.ModelCallHealth) error {
	return nil
}

func (s staticCallHealth) ListModelCallHealth(context.Context, time.Duration) ([]store.ModelCallHealth, error) {
	return s.records, nil
}

func TestModelReconcilerAdapter_ConfigureCallHealthDegradesModelsFromRealCalls(t *testing.T) {
	registry := cpi.NewRegistry()
	registry.MustRegister(healthyOpenAIDriver{})
	model := api.Model{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Model"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "gpt", ResourceVersion: 1},
		Spec:       api.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o", Endpoint: "https://gateway.example/v1", Capabilities: []string{"chat"}},
	}
	data, _ := json.Marshal(model)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "gpt"}}
	storedModel := func(fs *fakeStore) api.Model {
		var stored api.Model
		if err := json.Unmarshal(fs.getData["Model/default/gpt"], &stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}

	withoutHealth := newFakeStore()
	withoutHealth.getData["Model/default/gpt"] = data
	adapter := NewModelReconcilerAdapter(withoutHealth, registry, testLogger(t))
	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if !storedModel(withoutHealth).Status.Available {
		t.Fatal("a model without passive health must stay available")
	}

	now := time.Now()
	evidence := staticCallHealth{records: []store.ModelCallHealth{{
		Target: modelhealth.TargetKey("openai-compatible", model.Spec.Endpoint, model.Spec.ModelID),
		Calls:  3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonRateLimited, LastFaultAt: now,
	}}}
	withHealth := newFakeStore()
	withHealth.getData["Model/default/gpt"] = data
	adapter = NewModelReconcilerAdapter(withHealth, registry, testLogger(t))
	adapter.ConfigureCallHealth(evidence)
	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if degraded := storedModel(withHealth); degraded.Status.Available || degraded.Status.Phase != api.PhaseFailed {
		t.Fatalf("status = %+v, want the model degraded by failing real calls", degraded.Status)
	}
}

func TestModelClassReconcilerAdapter_ConfigureCallHealthDegradesClassesFromRealCalls(t *testing.T) {
	registry := cpi.NewRegistry()
	registry.MustRegister(healthyOpenAIDriver{})
	class := api.ModelClass{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "ModelClass"},
		ObjectMeta: api.ObjectMeta{Name: "fast", ResourceVersion: 1, Finalizers: []string{"kognition.dev/model-class-protection"}},
		Spec: api.ModelClassSpec{
			Provisioner: "openai-compatible",
			HealthProbe: &api.HealthProbeSpec{Endpoint: "https://gateway.example/health", Type: "error-rate-threshold", ErrorRateThreshold: 50, FailureThreshold: 1},
		},
		Status: api.ModelClassStatus{Phase: api.PhaseReady, Provisioned: true, Health: "Healthy"},
	}
	model := api.Model{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Model"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "gpt"},
		Spec:       api.ModelSpec{ClassName: "fast", ModelID: "gpt-4o", Endpoint: "https://gateway.example/v1"},
	}
	classData, _ := json.Marshal(class)
	modelData, _ := json.Marshal(model)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: "fast"}}
	newStore := func() *classListingStore {
		s := &classListingStore{fakeStore: newFakeStore(), models: [][]byte{modelData}}
		s.getData["ModelClass//fast"] = classData
		return s
	}
	storedClass := func(s *classListingStore) api.ModelClass {
		var stored api.ModelClass
		if err := json.Unmarshal(s.getData["ModelClass//fast"], &stored); err != nil {
			t.Fatal(err)
		}
		return stored
	}
	evidence := staticCallHealth{records: []store.ModelCallHealth{{
		Target: modelhealth.TargetKey("openai-compatible", model.Spec.Endpoint, model.Spec.ModelID),
		Calls:  10, Faults: 8, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: time.Now(),
	}}}

	without := newStore()
	if _, err := NewModelClassReconcilerAdapter(without, registry, testLogger(t)).Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if got := storedClass(without).Status.Phase; got != api.PhaseReady {
		t.Fatalf("phase = %s, want a class without passive health to stay Ready", got)
	}

	with := newStore()
	adapter := NewModelClassReconcilerAdapter(with, registry, testLogger(t))
	adapter.ConfigureCallHealth(evidence)
	if _, err := adapter.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error: %v", err)
	}
	if got := storedClass(with).Status.Phase; got != api.PhaseDegraded {
		t.Fatalf("phase = %s, want the class degraded by an 80%% error rate", got)
	}
}

// classListingStore lists the Models of the test, which fakeStore does not.
type classListingStore struct {
	*fakeStore
	models [][]byte
}

func (s *classListingStore) List(_ context.Context, kind string, _ store.ListOptions) ([][]byte, error) {
	if kind == "Model" {
		return s.models, nil
	}
	return nil, nil
}

func TestModelReconcilerAdapter_EnqueueQueuesModelsWithoutBlocking(t *testing.T) {
	adapter := NewModelReconcilerAdapter(newFakeStore(), cpi.NewRegistry(), testLogger(t))

	for _, key := range []string{"", "no-slash", "/name", "namespace/"} {
		adapter.Enqueue(key)
	}
	select {
	case queued := <-adapter.healthEvents:
		t.Fatalf("malformed keys must be ignored, got %v", queued.Object)
	default:
	}

	adapter.Enqueue("tenant-a/model-1")
	queued := <-adapter.healthEvents
	if queued.Object.GetNamespace() != "tenant-a" || queued.Object.GetName() != "model-1" {
		t.Fatalf("queued %s/%s, want tenant-a/model-1", queued.Object.GetNamespace(), queued.Object.GetName())
	}
	if _, isModel := queued.Object.(*k8sapi.Model); !isModel {
		t.Fatalf("queued object is %T, want a Model", queued.Object)
	}

	// A full buffer drops further keys instead of blocking the watcher.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < healthEventBuffer+50; i++ {
			adapter.Enqueue("tenant-a/model-1")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Enqueue() blocked on a full buffer")
	}
	if got := len(adapter.healthEvents); got != healthEventBuffer {
		t.Fatalf("buffered %d events, want the buffer capped at %d", got, healthEventBuffer)
	}
}

func TestModelReconcilerAdapter_SetupWithManagerWatchesTheHealthSource(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8sapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	manager, err := ctrl.NewManager(&rest.Config{Host: "http://127.0.0.1:1"}, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller:     config.Controller{SkipNameValidation: ptr.To(true)},
		MapperProvider: func(*rest.Config, *http.Client) (meta.RESTMapper, error) { return k8sapiMapper(scheme), nil },
		NewClient:      func(*rest.Config, client.Options) (client.Client, error) { return kube, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewModelReconcilerAdapter(newFakeStore(), cpi.NewRegistry(), testLogger(t))
	if err := adapter.SetupWithManager(manager); err != nil {
		t.Fatalf("SetupWithManager() error: %v", err)
	}
}

// k8sapiMapper maps every Kognition type in scheme so controller setup can
// resolve the Model kind without a cluster.
func k8sapiMapper(scheme *runtime.Scheme) meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	for gvk := range scheme.AllKnownTypes() {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	return mapper
}
