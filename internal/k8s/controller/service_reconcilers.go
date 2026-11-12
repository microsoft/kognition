// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

type AgentServiceReconcilerAdapter struct {
	internal         *intctrl.AgentServiceReconciler
	logger           *zap.Logger
	events           eventEmitter
	client           client.Client
	runtimeNamespace string
	dispatcher       *dispatcher.K8sDispatcher
}

// SetDispatcher enables dedicated immutable revision workloads for pinned
// AgentServices. It is optional to keep unit construction lightweight.
func (a *AgentServiceReconcilerAdapter) SetDispatcher(d *dispatcher.K8sDispatcher) {
	a.dispatcher = d
}

func NewAgentServiceReconcilerAdapter(s store.Store, logger *zap.Logger, runtimeNamespaces ...string) *AgentServiceReconcilerAdapter {
	adapter := &AgentServiceReconcilerAdapter{logger: logger}
	if len(runtimeNamespaces) > 0 {
		adapter.runtimeNamespace = runtimeNamespaces[0]
	}
	adapter.internal = intctrl.NewAgentServiceReconciler(s, logger, intctrl.WithAgentServiceEndpointPublisher(adapter))
	return adapter
}

func (a *AgentServiceReconcilerAdapter) Ensure(ctx context.Context, service *api.AgentService, agent *api.Agent) (intctrl.AgentServiceEndpoint, error) {
	if a.client == nil {
		return intctrl.AgentServiceEndpoint{}, fmt.Errorf("Kubernetes client is not initialized")
	}
	namespace := service.ObjectMeta.Namespace
	if a.runtimeNamespace != "" {
		namespace = a.runtimeNamespace
	}
	name := agentServiceEndpointName(service, namespace)
	port := service.Spec.Port
	if port <= 0 {
		port = 8080
	}
	selector := map[string]string{
		"app.kubernetes.io/managed-by":  "kognition",
		"kognition.dev/agent-name":      agent.ObjectMeta.Name,
		"kognition.dev/agent-namespace": agent.ObjectMeta.Namespace,
	}
	var revisionWorkload *dispatcher.RevisionWorkload
	if service.Spec.RevisionPin != nil {
		if a.dispatcher == nil {
			return intctrl.AgentServiceEndpoint{}, fmt.Errorf("revision-pinned AgentService requires a dispatcher")
		}
		workload, err := a.dispatcher.EnsureAgentServiceRevisionWorkload(ctx, service.ObjectMeta.Namespace, service.ObjectMeta.Name, *service.Spec.RevisionPin, agent)
		if err != nil {
			return intctrl.AgentServiceEndpoint{}, fmt.Errorf("ensure revision workload: %w", err)
		}
		namespace = workload.Namespace
		selector = workload.Selector
		revisionWorkload = &workload
	}
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kognition",
		"kognition.dev/agent-service":  service.ObjectMeta.Name,
	}
	for key, value := range selector {
		labels[key] = value
	}
	annotations := map[string]string{
		"kognition.dev/owner-agent-service": service.ObjectMeta.Namespace + "/" + service.ObjectMeta.Name,
	}
	desiredPorts := []corev1.ServicePort{{
		Name:       "http",
		Port:       port,
		TargetPort: intstr.FromInt(8080),
		Protocol:   corev1.ProtocolTCP,
	}}
	var existing corev1.Service
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := a.client.Get(ctx, key, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return intctrl.AgentServiceEndpoint{}, fmt.Errorf("get published AgentService %s/%s: %w", namespace, name, err)
		}
		desired := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels, Annotations: annotations},
			Spec:       corev1.ServiceSpec{Selector: selector, Ports: desiredPorts, Type: corev1.ServiceTypeClusterIP},
		}
		if err := a.client.Create(ctx, desired); err != nil {
			return intctrl.AgentServiceEndpoint{}, fmt.Errorf("create published AgentService %s/%s: %w", namespace, name, err)
		}
	} else if existing.Annotations["kognition.dev/owner-agent-service"] != annotations["kognition.dev/owner-agent-service"] {
		return intctrl.AgentServiceEndpoint{}, fmt.Errorf("published AgentService %s/%s has a different owner", namespace, name)
	} else if !reflect.DeepEqual(existing.Spec.Selector, selector) || !reflect.DeepEqual(existing.Spec.Ports, desiredPorts) ||
		!reflect.DeepEqual(existing.Labels, labels) || !reflect.DeepEqual(existing.Annotations, annotations) {
		existing.Spec.Selector = selector
		existing.Spec.Ports = desiredPorts
		existing.Labels = labels
		existing.Annotations = annotations
		if err := a.client.Update(ctx, &existing); err != nil {
			return intctrl.AgentServiceEndpoint{}, fmt.Errorf("update published AgentService %s/%s: %w", namespace, name, err)
		}
	}
	if service.Spec.RevisionPin == nil && a.dispatcher != nil {
		if err := a.dispatcher.CleanupAgentServiceRevisionWorkloads(ctx, service.ObjectMeta.Namespace, service.ObjectMeta.Name, ""); err != nil {
			return intctrl.AgentServiceEndpoint{}, fmt.Errorf("cleanup unpinned revision workloads: %w", err)
		}
	}
	endpoint := intctrl.AgentServiceEndpoint{URL: fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/invoke", name, namespace, port)}
	if revisionWorkload != nil {
		ready := revisionWorkload.ReadyReplicas
		endpoint.ReadyInstances = &ready
	}
	return endpoint, nil
}

func (a *AgentServiceReconcilerAdapter) Delete(ctx context.Context, service *api.AgentService) error {
	if a.client == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	namespace := service.ObjectMeta.Namespace
	if a.runtimeNamespace != "" {
		namespace = a.runtimeNamespace
	}
	endpoint := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: agentServiceEndpointName(service, namespace), Namespace: namespace,
	}}
	if err := a.client.Get(ctx, client.ObjectKeyFromObject(endpoint), endpoint); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("get published AgentService %s/%s: %w", namespace, endpoint.Name, err)
		}
	} else {
		if endpoint.Annotations["kognition.dev/owner-agent-service"] != service.ObjectMeta.Namespace+"/"+service.ObjectMeta.Name {
			return fmt.Errorf("published AgentService %s/%s has a different owner", namespace, endpoint.Name)
		}
		if err := a.client.Delete(ctx, endpoint, client.Preconditions{UID: &endpoint.UID}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete published AgentService %s/%s: %w", namespace, endpoint.Name, err)
		}
	}
	if a.dispatcher != nil {
		if err := a.dispatcher.CleanupAgentServiceRevisionWorkloads(ctx, service.ObjectMeta.Namespace, service.ObjectMeta.Name, ""); err != nil {
			return fmt.Errorf("cleanup revision workloads: %w", err)
		}
	}
	return nil
}

func agentServiceEndpointName(service *api.AgentService, runtimeNamespace string) string {
	if runtimeNamespace == service.ObjectMeta.Namespace {
		return service.ObjectMeta.Name
	}
	digest := sha256.Sum256([]byte(service.ObjectMeta.Namespace + "\x00" + service.ObjectMeta.Name))
	prefix := strings.ToLower(service.ObjectMeta.Namespace + "-" + service.ObjectMeta.Name)
	prefix = strings.Trim(strings.ReplaceAll(prefix, "_", "-"), "-.")
	const maxPrefix = 53
	if len(prefix) > maxPrefix {
		prefix = prefix[:maxPrefix]
	}
	return fmt.Sprintf("%s-%x", prefix, digest[:4])
}

func (a *AgentServiceReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "AgentService"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *AgentServiceReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "AgentService", "kognition-agentservice-controller")
	a.client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		Named("agentservice").
		For(&k8s.AgentService{}).
		Watches(&k8s.Agent{}, handler.EnqueueRequestsFromMapFunc(a.agentToServices)).
		Watches(&k8s.AgentInstance{}, handler.EnqueueRequestsFromMapFunc(a.dependencyToServices)).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(a.revisionDeploymentToService)).
		Complete(a)
}

func (a *AgentServiceReconcilerAdapter) revisionDeploymentToService(_ context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()["kognition.dev/agent-service-workload"] == "" {
		return nil
	}
	owner := obj.GetAnnotations()["kognition.dev/owner-agent-service"]
	parts := splitOwnerRef(owner)
	if parts == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: parts[0], Name: parts[1]}}}
}

func (a *AgentServiceReconcilerAdapter) agentToServices(ctx context.Context, obj client.Object) []reconcile.Request {
	var services k8s.AgentServiceList
	if err := a.client.List(ctx, &services, client.InNamespace(obj.GetNamespace())); err != nil {
		a.logger.Error("list agent services for dependency watch", zap.Error(err))
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range services.Items {
		if services.Items[i].Spec.AgentRef.Name == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&services.Items[i])})
		}
	}
	return requests
}

func (a *AgentServiceReconcilerAdapter) dependencyToServices(ctx context.Context, obj client.Object) []reconcile.Request {
	return listNamespacedRequests(ctx, a.client, a.logger, obj.GetNamespace(), &k8s.AgentServiceList{}, func(list client.ObjectList) []client.Object {
		serviceList, ok := list.(*k8s.AgentServiceList)
		if !ok || serviceList == nil {
			return nil
		}
		items := serviceList.Items
		objects := make([]client.Object, len(items))
		for i := range items {
			objects[i] = &items[i]
		}
		return objects
	})
}

type IntentGatewayReconcilerAdapter struct {
	internal          *intctrl.IntentGatewayReconciler
	logger            *zap.Logger
	events            eventEmitter
	client            client.Client
	servingNamespace  string
	servingDeployment string
	servingEndpoint   string
}

// IntentGatewayServingConfig identifies the dedicated Kubernetes workload that
// serves HTTP goal submissions and the Service URL it publishes.
type IntentGatewayServingConfig struct {
	Namespace  string
	Deployment string
	Endpoint   string
}

func NewIntentGatewayReconcilerAdapter(s store.Store, logger *zap.Logger, configs ...IntentGatewayServingConfig) *IntentGatewayReconcilerAdapter {
	adapter := &IntentGatewayReconcilerAdapter{logger: logger}
	if len(configs) > 0 {
		adapter.servingNamespace = configs[0].Namespace
		adapter.servingDeployment = configs[0].Deployment
		adapter.servingEndpoint = strings.TrimRight(configs[0].Endpoint, "/")
	}
	adapter.internal = intctrl.NewIntentGatewayReconciler(s, logger, intctrl.WithIntentGatewayServingEndpointProvider(adapter.endpointForGateway))
	return adapter
}

func (a *IntentGatewayReconcilerAdapter) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx = requestid.NewContext(ctx, requestid.Generate())
	result, err := a.internal.Reconcile(ctx, intctrl.Request{Namespace: req.Namespace, Name: req.Name, Kind: "IntentGateway"})
	a.events.emit(ctx, req, err)
	return ctrl.Result{Requeue: result.Requeue, RequeueAfter: result.RequeueAfter}, err
}

func (a *IntentGatewayReconcilerAdapter) SetupWithManager(mgr ctrl.Manager) error {
	a.events.setup(mgr, "IntentGateway", "kognition-intentgateway-controller")
	a.client = mgr.GetClient()
	builder := ctrl.NewControllerManagedBy(mgr).
		Named("intentgateway").
		For(&k8s.IntentGateway{}).
		Watches(&k8s.Agent{}, handler.EnqueueRequestsFromMapFunc(a.dependencyToGateways)).
		Watches(&k8s.AgentService{}, handler.EnqueueRequestsFromMapFunc(a.dependencyToGateways)).
		Watches(&k8s.Workflow{}, handler.EnqueueRequestsFromMapFunc(a.dependencyToGateways))
	if a.servingDeployment != "" && a.servingNamespace != "" {
		builder = builder.Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(a.gatewayDeploymentToGateways))
	}
	return builder.Complete(a)
}

func (a *IntentGatewayReconcilerAdapter) dependencyToGateways(ctx context.Context, obj client.Object) []reconcile.Request {
	return listNamespacedRequests(ctx, a.client, a.logger, obj.GetNamespace(), &k8s.IntentGatewayList{}, func(list client.ObjectList) []client.Object {
		gatewayList, ok := list.(*k8s.IntentGatewayList)
		if !ok || gatewayList == nil {
			return nil
		}
		items := gatewayList.Items
		objects := make([]client.Object, len(items))
		for i := range items {
			objects[i] = &items[i]
		}
		return objects
	})
}

func (a *IntentGatewayReconcilerAdapter) endpointForGateway(ctx context.Context, gateway *api.IntentGateway) (string, bool) {
	if a.client == nil || a.servingNamespace == "" || a.servingDeployment == "" || a.servingEndpoint == "" {
		return "", false
	}
	deployment := &appsv1.Deployment{}
	if err := a.client.Get(ctx, client.ObjectKey{Namespace: a.servingNamespace, Name: a.servingDeployment}, deployment); err != nil || !deploymentAvailable(deployment) {
		return "", false
	}
	return a.servingEndpoint + intctrl.IntentGatewayGoalPath(gateway.ObjectMeta.Namespace, gateway.ObjectMeta.Name), true
}

func (a *IntentGatewayReconcilerAdapter) gatewayDeploymentToGateways(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != a.servingNamespace || obj.GetName() != a.servingDeployment {
		return nil
	}
	gateways := &k8s.IntentGatewayList{}
	if err := a.client.List(ctx, gateways); err != nil {
		a.logger.Error("list intent gateways for serving deployment event", zap.Error(err))
		return nil
	}
	requests := make([]reconcile.Request, 0, len(gateways.Items))
	for index := range gateways.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&gateways.Items[index])})
	}
	return requests
}

func deploymentAvailable(deployment *appsv1.Deployment) bool {
	if deployment.Generation != deployment.Status.ObservedGeneration {
		return false
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func listNamespacedRequests(ctx context.Context, k8sClient client.Client, logger *zap.Logger, namespace string, list client.ObjectList, objects func(client.ObjectList) []client.Object) []reconcile.Request {
	if err := k8sClient.List(ctx, list, client.InNamespace(namespace)); err != nil {
		logger.Error("list resources for dependency watch", zap.Error(err))
		return nil
	}
	items := objects(list)
	requests := make([]reconcile.Request, 0, len(items))
	for _, item := range items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(item)})
	}
	return requests
}
