// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/postgres"
)

// flakyProvider is a CPI driver whose calls fail with a provider error until
// healed, and whose active probe always passes.
type flakyProvider struct {
	mu     sync.Mutex
	failed bool
}

func (p *flakyProvider) setFailing(failing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed = failing
}

func (p *flakyProvider) Name() string { return "anthropic" }
func (p *flakyProvider) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (p *flakyProvider) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true, Reason: "ProbeOK", Message: "probe passed", Capabilities: []string{"chat"}}, nil
}
func (p *flakyProvider) Complete(context.Context, *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failed {
		return nil, &cpi.ProviderError{Provider: "anthropic", Status: http.StatusTooManyRequests, Reason: cpi.ReasonRateLimited}
	}
	return &cpi.CompletionResponse{Content: "ok"}, nil
}
func (p *flakyProvider) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

type queuedKeys struct {
	mu   sync.Mutex
	keys []string
}

func (q *queuedKeys) add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.keys = append(q.keys, key)
}

func (q *queuedKeys) drain() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	keys := q.keys
	q.keys = nil
	return keys
}

// TestPassiveModelHealthFollowsRealCallsThroughPostgres drives the whole chain
// with the real tracker, reporter, SQL store, reconciler and watcher: failing
// calls made through an observed registry degrade the Model they were made for,
// and healed calls recover it, while an unrelated Model is untouched.
func TestPassiveModelHealthFollowsRealCallsThroughPostgres(t *testing.T) {
	ctx := context.Background()
	pool := postgres.SharedTestPool()
	backend := postgres.New(pool, zaptest.NewLogger(t))
	namespace := "passive-" + uuid.NewString()
	reporterID := namespace + "/reporter"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM resources WHERE namespace = $1", namespace)
		_, _ = pool.Exec(ctx, "DELETE FROM model_call_health WHERE reporter = $1", reporterID)
	})

	provider := &flakyProvider{}
	registry := cpi.NewRegistry()
	registry.MustRegister(provider)
	tracker := modelhealth.NewTracker()
	registry.Observe(tracker)
	reporter := modelhealth.NewReporter(tracker, backend, reporterID, zaptest.NewLogger(t))

	newModel := func(name, modelID string) v1alpha1.Model {
		return v1alpha1.Model{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       v1alpha1.ModelSpec{Provider: "anthropic", ModelID: modelID, Endpoint: "https://api.example/" + namespace},
		}
	}
	for _, model := range []v1alpha1.Model{newModel("claude", "claude-sonnet"), newModel("bystander", "claude-haiku")} {
		data, err := json.Marshal(model)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Create(ctx, "Model", namespace, model.ObjectMeta.Name, data); err != nil {
			t.Fatal(err)
		}
	}

	reconciler := controller.NewModelReconciler(backend, registry, zaptest.NewLogger(t))
	queue := &queuedKeys{}
	watcher := controller.NewModelHealthWatcher(backend, backend, queue.add, zaptest.NewLogger(t))
	reconcile := func(name string) v1alpha1.Model {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, controller.Request{Namespace: namespace, Name: name, Kind: "Model"}); err != nil {
			t.Fatalf("reconcile %s: %v", name, err)
		}
		raw, err := backend.Get(ctx, "Model", namespace, name)
		if err != nil {
			t.Fatal(err)
		}
		var model v1alpha1.Model
		if err := json.Unmarshal(raw, &model); err != nil {
			t.Fatal(err)
		}
		return model
	}
	callThroughRegistry := func(model v1alpha1.Model, calls int) {
		t.Helper()
		driver, ok := registry.Get(model.Spec.Provider)
		if !ok {
			t.Fatal("driver is not registered")
		}
		for i := 0; i < calls; i++ {
			_, _ = driver.Complete(ctx, &cpi.CompletionRequest{Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint})
		}
	}
	// ourKeys ignores Models left by other tests that share the embedded database.
	ourKeys := func() []string {
		var keys []string
		for _, key := range queue.drain() {
			if len(key) > len(namespace) && key[:len(namespace)] == namespace {
				keys = append(keys, key)
			}
		}
		return keys
	}

	for _, name := range []string{"claude", "bystander"} {
		if model := reconcile(name); !model.Status.Available {
			t.Fatalf("%s must start available, got %+v", name, model.Status)
		}
	}

	// Real calls to claude-sonnet start failing; nothing is probed or reconciled.
	provider.setFailing(true)
	claude := newModel("claude", "claude-sonnet")
	callThroughRegistry(claude, 3)
	if err := reporter.Flush(ctx); err != nil {
		t.Fatalf("Flush() error: %v", err)
	}
	if err := watcher.Check(ctx); err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if got := ourKeys(); len(got) != 1 || got[0] != namespace+"/claude" {
		t.Fatalf("watcher queued %v, want only %s/claude", got, namespace)
	}

	degraded := reconcile("claude")
	var ready *v1alpha1.Condition
	for i := range degraded.Status.Conditions {
		if degraded.Status.Conditions[i].Type == "Ready" {
			ready = &degraded.Status.Conditions[i]
		}
	}
	if degraded.Status.Available || degraded.Status.Phase != v1alpha1.PhaseFailed || ready == nil ||
		ready.Reason != cpi.ReasonRateLimited || ready.Status != "False" {
		t.Fatalf("claude = %+v, want it degraded for RateLimited from real traffic", degraded.Status)
	}
	if bystander := reconcile("bystander"); !bystander.Status.Available {
		t.Fatal("a Model for a different target must not be degraded")
	}

	// The same evidence no longer asks for a reconcile.
	if err := watcher.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ourKeys(); len(got) != 0 {
		t.Fatalf("watcher queued %v for a Model that already reflects the evidence", got)
	}

	// The provider heals: three successes recover the Model.
	provider.setFailing(false)
	callThroughRegistry(claude, 3)
	if err := reporter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := watcher.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ourKeys(); len(got) != 1 || got[0] != namespace+"/claude" {
		t.Fatalf("watcher queued %v, want the recovered %s/claude", got, namespace)
	}
	if recovered := reconcile("claude"); !recovered.Status.Available || recovered.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("claude = %+v, want it recovered after the provider healed", recovered.Status)
	}

	// Published evidence is bounded in time: once stale, readers ignore it.
	records, err := backend.ListModelCallHealth(ctx, time.Minute)
	if err != nil || len(records) == 0 {
		t.Fatalf("ListModelCallHealth() = %v, %v, want the published record", records, err)
	}
	if _, err := pool.Exec(ctx, "UPDATE model_call_health SET updated_at = now() - interval '10 minutes' WHERE reporter = $1", reporterID); err != nil {
		t.Fatal(err)
	}
	var fresh []store.ModelCallHealth
	if fresh, err = backend.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter); err != nil {
		t.Fatal(err)
	}
	for _, record := range fresh {
		if record.Target == modelhealth.TargetKey("anthropic", claude.Spec.Endpoint, claude.Spec.ModelID) {
			t.Fatal("a stale reporter's record must not be returned")
		}
	}
}
