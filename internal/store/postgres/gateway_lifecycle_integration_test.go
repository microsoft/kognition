// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/gatewaybackend"
	"github.com/kognition-ai/kognition/internal/store"
)

type warmSessionDriver struct{ calls int }

func (*warmSessionDriver) Name() string { return "warm-session-test" }
func (*warmSessionDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*warmSessionDriver) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (driver *warmSessionDriver) Complete(context.Context, *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	driver.calls++
	return &cpi.CompletionResponse{Content: "response", Usage: cpi.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5, Reported: true}}, nil
}
func (*warmSessionDriver) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, nil
}

func TestStandaloneControllerSessionPersistsAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := zaptest.NewLogger(t)
	config := gatewaybackend.Config{Mode: "standalone", DatabaseURL: "postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable"}
	backend, err := gatewaybackend.Open(ctx, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if backend != nil {
			backend.Close()
		}
	}()
	workspace := "lifecycle-" + uuid.NewString()
	className := "class-" + uuid.NewString()
	create := func(kind, namespace, name string, resource any) {
		t.Helper()
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Store.Create(ctx, kind, namespace, name, data); err != nil {
			t.Fatal(err)
		}
	}
	read := func(kind, namespace, name string, resource any) {
		t.Helper()
		data, err := backend.Store.Get(ctx, kind, namespace, name)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, resource); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func(reconciler controller.Reconciler, name string) {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, controller.Request{Namespace: workspace, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	create("ModelClass", "", className, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: className},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "warm-session-test"},
	})
	create("Model", workspace, "model", v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "model", Namespace: workspace},
		Spec:       v1alpha1.ModelSpec{ClassName: className, ModelID: "controlled-model"},
	})
	create("Agent", workspace, "source", v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "source", Namespace: workspace},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}},
	})
	defer func() {
		for _, kind := range []string{"AgentInstance", "AgentIdentity", "Agent", "Model"} {
			resources, err := backend.Store.List(context.Background(), kind, store.ListOptions{Namespace: workspace})
			if err != nil {
				t.Error(err)
				continue
			}
			for _, data := range resources {
				var resource struct {
					Metadata v1alpha1.ObjectMeta `json:"metadata"`
				}
				if err := json.Unmarshal(data, &resource); err != nil {
					t.Error(err)
					continue
				}
				if err := backend.Store.Delete(context.Background(), kind, workspace, resource.Metadata.Name); err != nil {
					t.Error(err)
				}
			}
		}
		if err := backend.Store.Delete(context.Background(), "ModelClass", "", className); err != nil {
			t.Error(err)
		}
	}()
	agents := controller.NewAgentReconciler(backend.Store, logger)
	reconcile(agents, "source")
	reconcile(agents, "source")
	instances, err := backend.Store.List(ctx, "AgentInstance", store.ListOptions{Namespace: workspace})
	if err != nil || len(instances) != 1 {
		t.Fatalf("controller-created instances: count=%d error=%v", len(instances), err)
	}
	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(instances[0], &instance); err != nil {
		t.Fatal(err)
	}
	driver := &warmSessionDriver{}
	registry := cpi.NewRegistry()
	registry.MustRegister(driver)
	newInstances := func() *controller.AgentInstanceReconciler {
		dispatcher := controller.NewCPIDispatcher(backend.Store, registry, logger)
		reconciler := controller.NewAgentInstanceReconcilerWithDispatcher(backend.Store, dispatcher, logger)
		reconciler.SetRequireAutomaticIdentity(true)
		return reconciler
	}
	instanceController := newInstances()
	for attempt := 0; attempt < 3; attempt++ {
		reconcile(instanceController, instance.ObjectMeta.Name)
	}
	read("AgentInstance", workspace, instance.ObjectMeta.Name, &instance)
	if instance.Status.Phase == v1alpha1.PhaseRunning || instance.Status.SessionID != "" {
		t.Fatal("instance dispatched before automatic identity became Ready")
	}
	identities := controller.NewAgentIdentityReconciler(backend.Store, logger)
	reconcile(identities, "agent-identity-source")
	reconcile(identities, "agent-identity-source")
	reconcile(instanceController, instance.ObjectMeta.Name)
	read("AgentInstance", workspace, instance.ObjectMeta.Name, &instance)
	var identity v1alpha1.AgentIdentity
	read("AgentIdentity", workspace, "agent-identity-source", &identity)
	var agent v1alpha1.Agent
	read("Agent", workspace, "source", &agent)
	if identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.SPIFFEID != "spiffe://kognition.dev/workspace/"+workspace+"/agent/source" || len(identity.ObjectMeta.OwnerReferences) != 1 || identity.ObjectMeta.OwnerReferences[0].UID != agent.ObjectMeta.UID {
		t.Fatal("automatic identity is not Ready and owned by the actual Agent UID")
	}
	if instance.Status.Phase != v1alpha1.PhaseRunning || instance.Status.SessionID == "" || instance.ObjectMeta.UID == "" || driver.calls != 0 {
		t.Fatalf("warm session: phase=%s sessionPresent=%t uidPresent=%t providerCalls=%d", instance.Status.Phase, instance.Status.SessionID != "", instance.ObjectMeta.UID != "", driver.calls)
	}
	original := instance
	restartedBackend, err := gatewaybackend.Open(ctx, config, logger)
	if err != nil {
		t.Fatal(err)
	}
	backend.Close()
	backend = restartedBackend
	reconcile(newInstances(), instance.ObjectMeta.Name)
	read("AgentInstance", workspace, instance.ObjectMeta.Name, &instance)
	if instance.ObjectMeta.UID != original.ObjectMeta.UID || instance.Status.SessionID != original.Status.SessionID || instance.Status.Phase != v1alpha1.PhaseReady || driver.calls != 0 {
		t.Fatalf("restored warm replica: phase=%s uidPreserved=%t sessionPreserved=%t providerCalls=%d", instance.Status.Phase, instance.ObjectMeta.UID == original.ObjectMeta.UID, instance.Status.SessionID == original.Status.SessionID, driver.calls)
	}
	var restoredIdentity v1alpha1.AgentIdentity
	read("AgentIdentity", workspace, identity.ObjectMeta.Name, &restoredIdentity)
	if restoredIdentity.ObjectMeta.UID != identity.ObjectMeta.UID || restoredIdentity.Spec.SPIFFEID != identity.Spec.SPIFFEID || restoredIdentity.Status.Phase != v1alpha1.PhaseReady {
		t.Fatal("automatic identity changed across backend restart")
	}

	t.Run("active completion survives backend restart", func(t *testing.T) {
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := backend.PostgresPool.Exec(cleanupCtx, "DELETE FROM model_dispatches WHERE namespace = $1", workspace); err != nil {
				t.Error(err)
			}
		})
		create("AgentInstance", workspace, "active", v1alpha1.AgentInstance{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentInstance"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "active", Namespace: workspace},
			Spec: v1alpha1.AgentInstanceSpec{
				AgentRef:       v1alpha1.LocalObjectReference{Name: "source"},
				SessionContext: map[string]string{"input": "complete this task"},
			},
		})
		var active v1alpha1.AgentInstance
		activeController := newInstances()
		for attempt := 0; attempt < 4; attempt++ {
			reconcile(activeController, "active")
			read("AgentInstance", workspace, "active", &active)
			if active.Status.Phase == v1alpha1.PhaseRunning {
				break
			}
		}
		if active.Status.Phase != v1alpha1.PhaseRunning || active.ObjectMeta.UID == "" || active.Status.SessionID == "" || driver.calls != 1 {
			t.Fatalf("active dispatch: phase=%s providerCalls=%d", active.Status.Phase, driver.calls)
		}
		originalSession := active.Status.SessionID
		reopened, err := gatewaybackend.Open(ctx, config, logger)
		if err != nil {
			t.Fatal(err)
		}
		backend.Close()
		backend = reopened
		reconcile(newInstances(), "active")
		read("AgentInstance", workspace, "active", &active)
		if active.Status.Phase != v1alpha1.PhaseReady || active.Status.Output != "response" || active.Status.SessionID != originalSession || active.Status.TokenUsage == nil || active.Status.TokenUsage.TotalTokens != 5 || driver.calls != 1 {
			t.Fatalf("restored active completion: phase=%s usage=%v providerCalls=%d", active.Status.Phase, active.Status.TokenUsage, driver.calls)
		}
		dispatcher := controller.NewCPIDispatcher(backend.Store, registry, logger)
		replayedSession, err := dispatcher.Dispatch(ctx, &active)
		if err != nil || replayedSession != originalSession || driver.calls != 1 {
			t.Fatalf("replayed completion: sameSession=%t providerCalls=%d error=%v", replayedSession == originalSession, driver.calls, err)
		}
	})
}
