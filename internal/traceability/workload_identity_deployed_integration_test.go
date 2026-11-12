// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/postgres"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeployedWorkloadIdentityStatusProjection(test *testing.T) {
	if os.Getenv("KOGNITION_IDENTITY_DEPLOYED_ACCEPTANCE") != "true" {
		test.Skip("explicit isolated identity acceptance required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	configuration, err := rest.InClusterConfig()
	if err != nil {
		test.Fatal(err)
	}
	configuration.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	if err := k8sapi.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	admin, err := client.New(configuration, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	var root corev1.Namespace
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &root); err != nil {
		test.Fatal(err)
	}
	if expected := os.Getenv("KOGNITION_IDENTITY_CLUSTER_UID"); expected == "" || string(root.UID) != expected {
		test.Fatal("isolated cluster UID mismatch")
	}
	var nodes corev1.NodeList
	if err := admin.List(ctx, &nodes); err != nil {
		test.Fatal(err)
	}
	runID := os.Getenv("KOGNITION_IDENTITY_RUN_ID")
	if parsed, err := uuid.Parse(runID); err != nil || parsed == uuid.Nil || parsed.String() != runID || len(nodes.Items) != 1 || nodes.Items[0].Name != "kognition-"+runID+"-control-plane" || nodes.Items[0].Labels["kognition.dev/identity-acceptance-run"] != runID {
		test.Fatal("isolated node ownership mismatch")
	}
	if err := k8sctrl.NewHostClockHealthCheck(30 * time.Second)(); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, os.Getenv("KOGNITION_IDENTITY_POSTGRES_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		test.Fatal(err)
	}
	if os.Getenv("KOGNITION_IDENTITY_MIGRATE") == "true" {
		if err := postgres.Migrate(ctx, pool, "001_initial_schema.sql"); err != nil {
			test.Fatal(err)
		}
		test.Log("normal migration runner applied full schema after verified fixture initial-schema baseline and backup")
		return
	}
	stores := map[string]store.Store{"kubernetes": k8sctrl.NewStoreAdapter(admin), "standalone": postgres.New(pool, zap.NewNop())}
	if selected := os.Getenv("KOGNITION_IDENTITY_RECREATION_MODE"); selected != "" {
		storage, exists := stores[selected]
		if !exists || os.Getenv("KOGNITION_IDENTITY_EXERCISE_RECREATION") != "true" {
			test.Fatal("explicit single-backend recreation required")
		}
		stores = map[string]store.Store{selected: storage}
	}
	if os.Getenv("KOGNITION_IDENTITY_PREPARE") == "true" {
		for mode, storage := range stores {
			namespace := "identity-" + mode
			agent := api.Agent{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"}, ObjectMeta: api.ObjectMeta{Namespace: namespace, Name: "worker"}, Spec: api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "fixture"}, Instructions: api.AgentInstructions{Inline: "identity acceptance only"}}}
			body, err := json.Marshal(agent)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Create(ctx, "Agent", namespace, "worker", body); err != nil {
				test.Fatal(err)
			}
			body, err = storage.Get(ctx, "Agent", namespace, "worker")
			if err != nil {
				test.Fatal(err)
			}
			if err := json.Unmarshal(body, &agent); err != nil {
				test.Fatal(err)
			}
			test.Logf("FIXTURE_AGENT mode=%s namespace=%s name=worker UID=%s", mode, namespace, agent.ObjectMeta.UID)
		}
		return
	}
	var reader *runtimeauth.WorkloadIdentityStatusRegistry
	var closeReader func()
	enrollment, stopEnrollment := context.WithTimeout(ctx, 45*time.Second)
	defer stopEnrollment()
	enrollmentTicker := time.NewTicker(time.Second)
	defer enrollmentTicker.Stop()
	for {
		reader, closeReader, err = runtimeauth.LoadWorkloadIdentityStatusRegistry(ctx, os.Getenv)
		if err == nil && reader != nil {
			break
		}
		select {
		case <-enrollment.Done():
			test.Fatalf("real issuer registry unavailable: %v", err)
		case <-enrollmentTicker.C:
		}
	}
	defer closeReader()
	observers := map[string]func() *api.WorkloadIdentityStatus{}
	for mode, storage := range stores {
		test.Run(mode, func(test *testing.T) {
			namespace := "identity-" + mode
			body, err := storage.Get(ctx, "Agent", namespace, "worker")
			if err != nil {
				test.Fatal(err)
			}
			var agent api.Agent
			if err := json.Unmarshal(body, &agent); err != nil {
				test.Fatal(err)
			}
			if parsed, err := uuid.Parse(agent.ObjectMeta.UID); err != nil || parsed == uuid.Nil || parsed.String() != agent.ObjectMeta.UID {
				test.Fatal("canonical live Agent UID required")
			}
			identity := api.AgentIdentity{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentIdentity"}, ObjectMeta: api.ObjectMeta{Namespace: namespace, Name: "agent-identity-worker", Labels: map[string]string{"kognition.dev/auto-created-for-agent": "worker"}, OwnerReferences: []api.OwnerReference{{APIVersion: api.APIVersion, Kind: "Agent", Name: "worker", UID: agent.ObjectMeta.UID}}}, Spec: api.AgentIdentitySpec{AgentRef: api.LocalObjectReference{Name: "worker"}}}
			if _, err := storage.Get(ctx, "AgentIdentity", namespace, identity.ObjectMeta.Name); errors.Is(err, store.ErrNotFound) {
				body, err := json.Marshal(identity)
				if err != nil {
					test.Fatal(err)
				}
				if err := storage.Create(ctx, "AgentIdentity", namespace, identity.ObjectMeta.Name, body); err != nil {
					test.Fatal(err)
				}
			} else if err != nil {
				test.Fatal(err)
			}
			reconciler := intctrl.NewAgentIdentityReconcilerWithWorkloadIdentity(storage, zap.NewNop(), reader)
			observe := func() *api.WorkloadIdentityStatus {
				test.Helper()
				for attempts := 0; attempts < 2; attempts++ {
					if _, err := reconciler.Reconcile(ctx, intctrl.Request{Namespace: namespace, Name: identity.ObjectMeta.Name}); err != nil {
						test.Fatal(err)
					}
				}
				body, err := storage.Get(ctx, "AgentIdentity", namespace, identity.ObjectMeta.Name)
				if err != nil {
					test.Fatal(err)
				}
				var observed api.AgentIdentity
				if err := json.Unmarshal(body, &observed); err != nil {
					test.Fatal(err)
				}
				if observed.Status.Phase != api.PhaseReady || observed.Status.WorkloadIdentity == nil {
					test.Fatal("legacy or certificate status missing")
				}
				return observed.Status.WorkloadIdentity
			}
			observers[mode] = observe
			initial := observe()
			if os.Getenv("KOGNITION_IDENTITY_EXPECT_UNREADY") == "true" {
				if initial.Ready {
					test.Fatal("unavailable issuer left persisted ready metadata")
				}
				test.Logf("mode=%s UID=%s readiness=false reason=%s", mode, agent.ObjectMeta.UID, initial.Reason)
				return
			}
			expected, err := runtimeauth.AgentIncarnationSPIFFEID("kognition.dev", namespace, "worker", agent.ObjectMeta.UID)
			if err != nil {
				test.Fatal(err)
			}
			if !initial.Ready || initial.AgentUID != agent.ObjectMeta.UID || initial.SPIFFEID != expected || initial.SourceGeneration < 1 || !initial.ExpiresAt.After(initial.NotBefore) {
				test.Fatalf("invalid authenticated projection: %+v", initial)
			}
			if status, err := reader.AgentStatus(namespace, "worker", uuid.NewString()); err == nil || status.Ready {
				test.Fatal("another incarnation received metadata")
			}
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					test.Fatal("live issuer rotation was not projected")
				case <-ticker.C:
				}
				current := observe()
				if current.Ready && current.SourceGeneration > initial.SourceGeneration && current.ExpiresAt.After(initial.ExpiresAt) {
					test.Logf("mode=%s UID=%s generation=%d->%d rotated-validity=%s", mode, agent.ObjectMeta.UID, initial.SourceGeneration, current.SourceGeneration, current.ExpiresAt.UTC().Format(time.RFC3339))
					break
				}
			}
		})
	}
	if os.Getenv("KOGNITION_IDENTITY_EXERCISE_OUTAGE") == "true" {
		name := os.Getenv("KOGNITION_IDENTITY_RUNNER_POD")
		var runner corev1.Pod
		if name == "" {
			test.Fatal("outage runner Pod binding required")
		}
		if err := admin.Get(ctx, client.ObjectKey{Namespace: "identity-acceptance", Name: name}, &runner); err != nil {
			test.Fatal(err)
		}
		if runner.Labels["kognition.dev/identity-acceptance-run"] != runID {
			test.Fatal("outage marker Pod ownership mismatch")
		}
		mark := func(stage string) {
			test.Helper()
			before := runner.DeepCopy()
			if runner.Annotations == nil {
				runner.Annotations = map[string]string{}
			}
			runner.Annotations["kognition.dev/identity-stage"] = stage
			if err := admin.Patch(ctx, &runner, client.MergeFrom(before)); err != nil {
				test.Fatal(err)
			}
		}
		await := func(ready bool) {
			test.Helper()
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				matched := len(observers) == 2
				for _, observe := range observers {
					matched = matched && observe().Ready == ready
				}
				if matched {
					return
				}
				select {
				case <-ctx.Done():
					test.Fatalf("both-mode readiness did not become %v", ready)
				case <-ticker.C:
				}
			}
		}
		mark("awaiting-outage")
		await(false)
		test.Log("both backends cleared certificate readiness during issuer outage")
		mark("outage-observed")
		await(true)
		test.Log("both backends restored certificate readiness after verified issuer reconnect")
		mark("recovered")
	}
	if os.Getenv("KOGNITION_IDENTITY_EXERCISE_RECREATION") == "true" {
		for mode, storage := range stores {
			namespace := "identity-" + mode
			body, err := storage.Get(ctx, "Agent", namespace, "worker")
			if err != nil {
				test.Fatal(err)
			}
			var previous api.Agent
			if err := json.Unmarshal(body, &previous); err != nil {
				test.Fatal(err)
			}
			reconciler := intctrl.NewAgentIdentityReconcilerWithWorkloadIdentity(storage, zap.NewNop(), reader)
			if err := storage.Delete(ctx, "AgentIdentity", namespace, "agent-identity-worker"); err != nil {
				test.Fatal(err)
			}
			ticker := time.NewTicker(250 * time.Millisecond)
			for {
				if _, err := storage.Get(ctx, "AgentIdentity", namespace, "agent-identity-worker"); errors.Is(err, store.ErrNotFound) {
					break
				} else if err != nil {
					test.Fatal(err)
				}
				if _, err := reconciler.Reconcile(ctx, intctrl.Request{Namespace: namespace, Name: "agent-identity-worker"}); err != nil {
					test.Fatal(err)
				}
				select {
				case <-ctx.Done():
					test.Fatal("normal old-identity cleanup did not finish")
				case <-ticker.C:
				}
			}
			ticker.Stop()
			if err := storage.Delete(ctx, "Agent", namespace, "worker"); err != nil {
				test.Fatal(err)
			}
			fresh := api.Agent{TypeMeta: previous.TypeMeta, ObjectMeta: api.ObjectMeta{Name: "worker", Namespace: namespace}, Spec: previous.Spec}
			body, err = json.Marshal(fresh)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Create(ctx, "Agent", namespace, "worker", body); err != nil {
				test.Fatal(err)
			}
			body, err = storage.Get(ctx, "Agent", namespace, "worker")
			if err != nil {
				test.Fatal(err)
			}
			if err := json.Unmarshal(body, &fresh); err != nil {
				test.Fatal(err)
			}
			if fresh.ObjectMeta.UID == "" || fresh.ObjectMeta.UID == previous.ObjectMeta.UID {
				test.Fatal("recreated Agent did not receive a new backend UID")
			}
			identity := api.AgentIdentity{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentIdentity"}, ObjectMeta: api.ObjectMeta{Name: "agent-identity-worker", Namespace: namespace, Labels: map[string]string{"kognition.dev/auto-created-for-agent": "worker"}, OwnerReferences: []api.OwnerReference{{APIVersion: api.APIVersion, Kind: "Agent", Name: "worker", UID: fresh.ObjectMeta.UID}}}, Spec: api.AgentIdentitySpec{AgentRef: api.LocalObjectReference{Name: "worker"}}}
			body, err = json.Marshal(identity)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Create(ctx, "AgentIdentity", namespace, identity.ObjectMeta.Name, body); err != nil {
				test.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := reconciler.Reconcile(ctx, intctrl.Request{Namespace: namespace, Name: identity.ObjectMeta.Name}); err != nil {
					test.Fatal(err)
				}
			}
			body, err = storage.Get(ctx, "AgentIdentity", namespace, identity.ObjectMeta.Name)
			if err != nil {
				test.Fatal(err)
			}
			var observed api.AgentIdentity
			if err := json.Unmarshal(body, &observed); err != nil {
				test.Fatal(err)
			}
			if observed.Status.WorkloadIdentity == nil || observed.Status.WorkloadIdentity.Ready {
				test.Fatal("recreated Agent inherited ready certificate metadata")
			}
			if metadata, err := reader.AgentStatus(namespace, "worker", fresh.ObjectMeta.UID); err == nil || metadata.Ready {
				test.Fatal("old registration authorized a recreated Agent")
			}
			test.Logf("mode=%s recreated-UID=%s old-UID=%s certificate-readiness=false", mode, fresh.ObjectMeta.UID, previous.ObjectMeta.UID)
		}
	}
}
