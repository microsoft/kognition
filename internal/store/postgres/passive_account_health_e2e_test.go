// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store/postgres"
)

// revokableProvider rejects the API keys it has revoked, like a provider does for
// one account, and serves every other key. Its active probe always passes.
type revokableProvider struct {
	mu      sync.Mutex
	revoked map[string]bool
}

func (p *revokableProvider) setRevoked(key string, revoked bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.revoked == nil {
		p.revoked = map[string]bool{}
	}
	p.revoked[key] = revoked
}

func (p *revokableProvider) Name() string { return "anthropic" }
func (p *revokableProvider) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (p *revokableProvider) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true, Reason: "ProbeOK", Message: "probe passed", Capabilities: []string{"chat"}}, nil
}
func (p *revokableProvider) Complete(_ context.Context, request *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if request.Creds != nil && p.revoked[request.Creds.Data["apiKey"]] {
		return nil, &cpi.ProviderError{Provider: "anthropic", Status: http.StatusUnauthorized, Reason: cpi.ReasonAuthenticationFailed}
	}
	return &cpi.CompletionResponse{Content: "ok"}, nil
}
func (p *revokableProvider) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

// TestPassiveModelHealthKeepsAccountsApartThroughPostgres drives two Models that
// share a provider, endpoint and model ID but use different credentials. Calls
// made with one account's revoked key must degrade only that account's Model, even
// though the destination, which the other account keeps using, is fine.
func TestPassiveModelHealthKeepsAccountsApartThroughPostgres(t *testing.T) {
	ctx := context.Background()
	pool := postgres.SharedTestPool()
	backend := postgres.New(pool, zaptest.NewLogger(t))
	namespace := "accounts-" + uuid.NewString()
	reporterID := namespace + "/reporter"
	endpoint := "https://api.example/" + namespace
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM resources WHERE namespace = $1", namespace)
		_, _ = pool.Exec(ctx, "DELETE FROM model_call_health WHERE reporter = $1", reporterID)
	})

	provider := &revokableProvider{}
	registry := cpi.NewRegistry()
	registry.MustRegister(provider)
	tracker := modelhealth.NewTracker()
	registry.Observe(tracker)
	reporter := modelhealth.NewReporter(tracker, backend, reporterID, zaptest.NewLogger(t))

	keys := map[string]string{"account-a": "key-a", "account-b": "key-b"}
	for name, key := range keys {
		secret := v1alpha1.SecretRef{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: name + "-key", Namespace: namespace, Annotations: map[string]string{"kognition.dev/internal-secret-data": `{"type":"api-key","apiKey":"` + key + `"}`}},
			Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "secret/" + name, Key: "apiKey"},
		}
		model := v1alpha1.Model{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       v1alpha1.ModelSpec{Provider: "anthropic", ModelID: "claude-sonnet", Endpoint: endpoint, CredentialRef: &v1alpha1.LocalObjectReference{Name: name + "-key"}},
		}
		for kind, object := range map[string]struct {
			name  string
			value any
		}{"SecretRef": {secret.ObjectMeta.Name, secret}, "Model": {name, model}} {
			data, err := json.Marshal(object.value)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.Create(ctx, kind, namespace, object.name, data); err != nil {
				t.Fatal(err)
			}
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
	// callAs makes calls the way the call sites do: with the secret of the Model's
	// SecretRef, which they present without its "type" entry.
	callAs := func(name string, calls int) {
		t.Helper()
		driver, _ := registry.Get("anthropic")
		for i := 0; i < calls; i++ {
			_, _ = driver.Complete(ctx, &cpi.CompletionRequest{
				Model: "claude-sonnet", Endpoint: endpoint,
				Creds: &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": keys[name]}},
			})
		}
		if err := reporter.Flush(ctx); err != nil {
			t.Fatalf("Flush() error: %v", err)
		}
	}
	ourKeys := func() []string {
		var keys []string
		for _, key := range queue.drain() {
			if len(key) > len(namespace) && key[:len(namespace)] == namespace {
				keys = append(keys, key)
			}
		}
		return keys
	}
	readyReason := func(model v1alpha1.Model) string {
		for _, condition := range model.Status.Conditions {
			if condition.Type == "Ready" {
				return condition.Reason
			}
		}
		return ""
	}

	for name := range keys {
		if model := reconcile(name); !model.Status.Available {
			t.Fatalf("%s must start available, got %+v", name, model.Status)
		}
	}

	// Account A's key is revoked while account B keeps calling the same model.
	provider.setRevoked("key-a", true)
	callAs("account-a", 3)
	callAs("account-b", 3)
	if err := watcher.Check(ctx); err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if got := ourKeys(); len(got) != 1 || got[0] != namespace+"/account-a" {
		t.Fatalf("watcher queued %v, want only %s/account-a", got, namespace)
	}
	degraded := reconcile("account-a")
	if degraded.Status.Available || readyReason(degraded) != cpi.ReasonAuthenticationFailed {
		t.Fatalf("account-a = %+v (%q), want it degraded for its rejected credentials", degraded.Status, readyReason(degraded))
	}
	if bystander := reconcile("account-b"); !bystander.Status.Available || bystander.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("account-b = %+v, want it untouched by another account's revoked key", bystander.Status)
	}

	// The key is restored: three successes recover the account.
	provider.setRevoked("key-a", false)
	callAs("account-a", 3)
	if err := watcher.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ourKeys(); len(got) != 1 || got[0] != namespace+"/account-a" {
		t.Fatalf("watcher queued %v, want the recovered %s/account-a", got, namespace)
	}
	if recovered := reconcile("account-a"); !recovered.Status.Available || recovered.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("account-a = %+v, want it recovered once its key works again", recovered.Status)
	}
}
