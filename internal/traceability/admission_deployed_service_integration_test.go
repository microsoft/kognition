// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func deployedServiceProducerAcceptance(test *testing.T, ctx context.Context, adminConfig *rest.Config, admin client.Client, controller client.Client, manifest activation.Manifest, producer *dispatcher.K8sDispatcher, agent *api.Agent, probe *corev1.Pod, created *[]client.Object) {
	test.Helper()
	controllerConfig := rest.CopyConfig(adminConfig)
	controllerConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + manifest.Namespace, "system:authenticated"}}
	manager, err := ctrl.NewManager(controllerConfig, ctrl.Options{
		Scheme: controller.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
		NewClient:  func(*rest.Config, client.Options) (client.Client, error) { return controller, nil },
	})
	if err != nil {
		test.Fatal(err)
	}
	store := k8scontroller.NewStoreAdapterWithControlNamespace(controller, manifest.Namespace)
	adapter := k8scontroller.NewAgentServiceReconcilerAdapter(store, zap.NewNop())
	adapter.SetDispatcher(producer)
	if err := adapter.SetupWithManager(manager); err != nil {
		test.Fatal(err)
	}
	data, err := json.Marshal(agent)
	if err != nil {
		test.Fatal(err)
	}
	spec, err := json.Marshal(agent.Spec)
	if err != nil {
		test.Fatal(err)
	}
	for _, revision := range []int64{1, 2} {
		if err := store.CreateRevision(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, revision, spec, data); err != nil {
			test.Fatal(err)
		}
	}
	service := convert.AgentServiceToK8s(&api.AgentService{
		ObjectMeta: api.ObjectMeta{Name: "sg08-published", Namespace: agent.ObjectMeta.Namespace},
		Spec:       api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: agent.ObjectMeta.Name}, RevisionPin: ptr.To(int64(1))},
	})
	if err := createAdmissionFixture(ctx, admin, service, created); err != nil {
		test.Fatal(err)
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(service)}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		current := &k8sapi.AgentService{}
		if err := admin.Get(cleanup, request.NamespacedName, current); apierrors.IsNotFound(err) {
			return
		} else if err != nil || current.UID != service.UID {
			test.Errorf("service recovery identity: %v", err)
			return
		}
		if err := admin.Delete(cleanup, current, client.Preconditions{UID: &current.UID}); err != nil {
			test.Error(err)
			return
		}
		if _, err := adapter.Reconcile(cleanup, request); err != nil {
			test.Error(err)
		}
	})
	var publishedUID string
	for _, revision := range []int64{1, 2} {
		if revision == 2 {
			service.Spec.RevisionPin = ptr.To(revision)
			if err := admin.Update(ctx, service); err != nil {
				test.Fatal(err)
			}
		}
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
			if _, err := adapter.Reconcile(ctx, request); err != nil {
				return false, err
			}
			if err := admin.Get(ctx, request.NamespacedName, service); err != nil {
				return false, err
			}
			return service.Status.Phase == "Ready" && service.Status.ActiveRevision == revision && service.Status.ReadyInstances == 1, nil
		}); err != nil {
			test.Fatalf("service revision %d failed: %v; status=%+v", revision, err, service.Status)
		}
		published := &corev1.Service{}
		if err := admin.Get(ctx, request.NamespacedName, published); err != nil {
			test.Fatal(err)
		}
		if published.Spec.Selector["kognition.dev/agent-service-revision"] != fmt.Sprint(revision) || published.Annotations["kognition.dev/owner-agent-service"] != service.Namespace+"/"+service.Name {
			test.Fatalf("published Service identity/selector mismatch: %+v", published)
		}
		if publishedUID != "" && publishedUID != string(published.UID) {
			test.Fatal("repin replaced the published Service")
		}
		publishedUID = string(published.UID)
		if !strings.Contains(service.Status.Endpoint, service.Name+"."+service.Namespace+".svc.cluster.local") {
			test.Fatalf("reconciler did not publish Service DNS: %q", service.Status.Endpoint)
		}
		deployedProducerExec(test, ctx, adminConfig, probe, "model", []string{"wget", "-qO-", "-T", "5", strings.TrimSuffix(service.Status.Endpoint, "/invoke") + "/readyz"})
		test.Logf("production AgentService revision=%d published UID=%s endpoint=%s ready=%d; in-cluster Service DNS transport passed", revision, published.UID, service.Status.Endpoint, service.Status.ReadyInstances)
	}
	if err := admin.Delete(ctx, service, client.Preconditions{UID: &service.UID}); err != nil {
		test.Fatal(err)
	}
	if err := admin.Get(ctx, request.NamespacedName, service); err != nil || service.DeletionTimestamp == nil {
		test.Fatalf("service finalizer did not retain deletion: %v", err)
	}
	var cleanupErr error
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		_, cleanupErr = adapter.Reconcile(ctx, request)
		if cleanupErr != nil {
			return false, nil
		}
		err := admin.Get(ctx, request.NamespacedName, &k8sapi.AgentService{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		test.Fatalf("service finalizer did not complete: %v; last reconcile error: %v", err, cleanupErr)
	}
	if err := admin.Get(ctx, request.NamespacedName, &corev1.Service{}); !apierrors.IsNotFound(err) {
		test.Fatalf("published endpoint survived deletion: %v", err)
	}
	test.Logf("production AgentService finalizer %s completed and endpoint UID=%s is absent", intctrl.AgentServiceFinalizer, publishedUID)
}
