// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const deployedRuntimeSourceName = "sg08-controller-credential"

func deployedAgentProducerAcceptance(test *testing.T, ctx context.Context, restConfig *rest.Config, admin client.Client, controller client.WithWatch, manifest activation.Manifest, roles []*unstructured.Unstructured, image string, created *[]client.Object) {
	test.Helper()
	var sourceName string
	wantedSource := deployedRuntimeSourceName
	targetSource := os.Getenv("KOGNITION_ADMISSION_TARGET_RUNTIME_SECRET")
	preserveSource := targetSource != ""
	if preserveSource {
		if os.Getenv("KOGNITION_ADMISSION_TARGET_MANIFEST") == "" || os.Getenv("KOGNITION_ADMISSION_WRITERS_FROZEN") != "true" {
			test.Fatal("existing runtime source requires reviewed inactive target and frozen writers")
		}
		wantedSource = targetSource
	}
	for _, role := range roles {
		if role.GetKind() != "Role" || role.GetNamespace() != manifest.Namespace {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
		for _, item := range rules {
			rule, ok := item.(map[string]any)
			if !ok {
				continue
			}
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			if len(resources) != 1 || resources[0] != "secrets" {
				continue
			}
			names, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
			for _, name := range names {
				if name == wantedSource {
					sourceName = name
				}
			}
		}
	}
	if sourceName == "" {
		test.Fatal("rendered installation Role has no named runtime source Secret")
	}
	workload := manifest.Namespaces[0]
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sourceName, Namespace: manifest.Namespace}}
	if !preserveSource {
		source.Data = map[string][]byte{"runtime-control-token": []byte("sg08-local-token-one")}
		if err := createAdmissionFixture(ctx, admin, source, created); err != nil {
			test.Fatal(err)
		}
	} else {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
			test.Fatal(err)
		}
		original := source.DeepCopy()
		if source.UID == "" || source.DeletionTimestamp != nil {
			test.Fatal("reviewed runtime source has no stable live identity")
		}
		test.Cleanup(func() {
			recovery, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			current := &corev1.Secret{}
			if err := admin.Get(recovery, client.ObjectKeyFromObject(original), current); err != nil {
				test.Errorf("reviewed runtime source could not be verified unchanged: %v", err)
				return
			}
			if current.UID != original.UID || current.ResourceVersion != original.ResourceVersion || !reflect.DeepEqual(current.Data, original.Data) {
				test.Error("reviewed runtime source changed during acceptance; no automatic restoration was attempted")
			}
		})
	}
	runtimeToken := string(source.Data["runtime-control-token"])
	if runtimeToken == "" {
		test.Fatal("runtime source requires a nonempty runtime-control-token")
	}
	for _, namespace := range []string{manifest.Namespace, workload} {
		if err := controller.List(ctx, &corev1.SecretList{}, client.InNamespace(namespace)); !apierrors.IsForbidden(err) {
			test.Fatalf("Secret list in %s: %v, want Forbidden", namespace, err)
		}
		watcher, err := controller.Watch(ctx, &corev1.SecretList{}, client.InNamespace(namespace))
		if watcher != nil {
			watcher.Stop()
		}
		if !apierrors.IsForbidden(err) {
			test.Fatalf("Secret watch in %s: %v, want Forbidden", namespace, err)
		}
	}
	if err := controller.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: "sg08-unrelated"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
		test.Fatalf("unrelated installation Secret GET: %v, want Forbidden", err)
	}
	observed := interceptor.NewClient(controller, interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := underlying.Create(ctx, object, options...); err != nil {
				return err
			}
			*created = append(*created, object.DeepCopyObject().(client.Object))
			test.Logf("production dispatcher created %T %s UID=%s", object, client.ObjectKeyFromObject(object), object.GetUID())
			return nil
		},
	})
	config := dispatcher.Config{AgentImage: image, ControllerNamespace: manifest.Namespace, RuntimeAuthToken: runtimeToken, RuntimeAuthTokenSecretName: sourceName}
	store := k8scontroller.NewStoreAdapterWithControlNamespace(observed, manifest.Namespace)
	producer := dispatcher.NewK8sDispatcher(observed, store, zap.NewNop(), config)
	trackingStart := len(*created)
	invalid := &api.Agent{ObjectMeta: api.ObjectMeta{Name: "sg08-invalid", Namespace: manifest.Namespace}}
	if err := producer.EnsureAgentRuntime(ctx, invalid); err == nil || !strings.Contains(err.Error(), "installation namespace") {
		test.Fatalf("Agent installation namespace rejection: %v", err)
	}
	if _, err := producer.EnsureAgentServiceRevisionWorkload(ctx, manifest.Namespace, "sg08-invalid", 1, invalid); err == nil || !strings.Contains(err.Error(), "installation namespace") {
		test.Fatalf("revision installation namespace rejection: %v", err)
	}
	if len(*created) != trackingStart {
		test.Fatal("installation namespace rejection occurred after provisioning")
	}
	streamImage := os.Getenv("KOGNITION_ADMISSION_STREAM_IMAGE")
	if streamImage == "" {
		test.Fatal("reviewed stream image required for local model fixture")
	}
	fixture := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sg08-model", Namespace: workload}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(1)),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers: []corev1.Container{{Name: "model", Image: streamImage, ImagePullPolicy: corev1.PullNever,
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Command: []string{"sh", "-ec", `mkdir -p /tmp/www/cgi-bin/chat
cat > /tmp/www/cgi-bin/chat/completions <<'FIXTURE'
#!/bin/sh
printf 'Content-Type: application/json\r\n\r\n'
printf '%s' '{"id":"sg08-local","object":"chat.completion","created":1,"model":"sg08-local","choices":[{"index":0,"message":{"role":"assistant","content":"SG08 local dispatch completed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}'
FIXTURE
chmod +x /tmp/www/cgi-bin/chat/completions
exec httpd -f -p 8081 -h /tmp/www`},
		}},
	}}
	if err := createAdmissionFixture(ctx, admin, fixture, created); err != nil {
		test.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(fixture), fixture); err != nil {
			return false, err
		}
		return len(fixture.Status.ContainerStatuses) == 1 && fixture.Status.ContainerStatuses[0].Ready && fixture.Status.PodIP != "", nil
	}); err != nil {
		test.Fatal(err)
	}
	model := convert.ModelToK8s(&api.Model{ObjectMeta: api.ObjectMeta{Name: "sg08-chat", Namespace: workload}, Spec: api.ModelSpec{Provider: "ollama", ModelID: "sg08-local", Endpoint: "http://" + fixture.Status.PodIP + ":8081/cgi-bin"}})
	if err := createAdmissionFixture(ctx, admin, model, created); err != nil {
		test.Fatal(err)
	}
	agent := &api.Agent{ObjectMeta: api.ObjectMeta{Name: "sg08-agent", Namespace: workload}, Spec: api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "sg08-chat"}, Instructions: api.AgentInstructions{Inline: "Local SG08 acceptance"}}}
	if err := createAdmissionFixture(ctx, admin, convert.AgentToK8s(agent), created); err != nil {
		test.Fatal(err)
	}
	if err := producer.EnsureAgentRuntime(ctx, agent); err != nil {
		test.Fatal(err)
	}
	localToken := &corev1.Secret{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: sourceName}, localToken); err != nil {
		test.Fatal(err)
	}
	if string(localToken.Data["runtime-control-token"]) != runtimeToken {
		test.Fatal("runtime source token was not materialized")
	}
	first, err := producer.EnsureAgentServiceRevisionWorkload(ctx, workload, "sg08-service", 1, agent)
	if err != nil {
		test.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: first.Name}, deployment); err != nil {
			return false, err
		}
		return deployment.Status.AvailableReplicas == 1, nil
	}); err != nil {
		test.Fatalf("revision workload did not become available: %v; status=%+v", err, deployment.Status)
	}
	var pods corev1.PodList
	if err := admin.List(ctx, &pods, client.InNamespace(workload), client.MatchingLabels(first.Selector)); err != nil || len(pods.Items) != 1 {
		test.Fatalf("revision Pod count=%d error=%v", len(pods.Items), err)
	}
	pod := &pods.Items[0]
	*created = append(*created, pod.DeepCopy())
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "ReplicaSet" {
		test.Fatal("revision Pod missing ReplicaSet owner")
	}
	var replicaSet appsv1.ReplicaSet
	if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: owner.Name}, &replicaSet); err != nil || replicaSet.UID != owner.UID {
		test.Fatalf("revision Pod owner mismatch: %v", err)
	}
	deploymentOwner := metav1.GetControllerOf(&replicaSet)
	if deploymentOwner == nil || deploymentOwner.UID != deployment.UID {
		test.Fatal("revision ReplicaSet belongs to another Deployment")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 1000 || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		test.Fatal("revision Pod missing UID 1000 or token automount restriction")
	}
	if len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready || pod.Status.ContainerStatuses[0].ImageID == "" {
		test.Fatal("revision runtime is not ready with a resolved image identity")
	}
	test.Logf("revision Deployment UID=%s Pod UID=%s imageID=%s", deployment.UID, pod.UID, pod.Status.ContainerStatuses[0].ImageID)
	service := &corev1.Service{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: workload, Name: first.Name}, service); err != nil {
		test.Fatal(err)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Selector["kognition.dev/agent-service-revision"] != "1" {
		test.Fatalf("unexpected published revision Service: %+v", service.Spec)
	}
	endpoint := deployedAgentForward(test, ctx, restConfig, pod)
	routedService := convert.AgentServiceToK8s(&api.AgentService{
		ObjectMeta: api.ObjectMeta{Name: "sg08-route", Namespace: workload},
		Spec:       api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: agent.ObjectMeta.Name}},
	})
	if err := createAdmissionFixture(ctx, admin, routedService, created); err != nil {
		test.Fatal(err)
	}
	routedService.Status.Phase = "Ready"
	routedService.Status.ReadyInstances = 1
	routedService.Status.Endpoint = "http://" + endpoint + "/invoke"
	if err := admin.Status().Update(ctx, routedService); err != nil {
		test.Fatal(err)
	}
	instance := &api.AgentInstance{ObjectMeta: api.ObjectMeta{Name: "sg08-invocation", Namespace: workload, Annotations: map[string]string{intctrl.AgentServiceExecutionAnnotation: routedService.Name}}, Spec: api.AgentInstanceSpec{AgentRef: api.LocalObjectReference{Name: agent.ObjectMeta.Name}, SessionContext: map[string]string{"input": "Complete the local SG08 acceptance"}}}
	sessionID, err := producer.Dispatch(ctx, instance)
	if err != nil || sessionID == "" {
		test.Fatalf("dispatch session=%q error=%v", sessionID, err)
	}
	instance.Status.SessionID = sessionID
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		status, err := producer.Status(ctx, instance)
		if err != nil {
			return false, err
		}
		if status.Error != "" {
			return false, fmt.Errorf("runtime failed: %s", status.Error)
		}
		if status.Completed && status.Output != "SG08 local dispatch completed" {
			return false, fmt.Errorf("unexpected output: %q", status.Output)
		}
		return status.Completed, nil
	}); err != nil {
		test.Fatalf("real dispatch did not complete: %v", err)
	}
	test.Logf("real production Dispatch/Status completed session=%s through authenticated runtime and local model fixture", sessionID)
	deployedServiceProducerAcceptance(test, ctx, restConfig, admin, observed, manifest, producer, agent, fixture, created)
	if !preserveSource {
		source.Data["runtime-control-token"] = []byte("sg08-local-token-two")
		if err := admin.Update(ctx, source); err != nil {
			test.Fatal(err)
		}
		config.RuntimeAuthToken = "sg08-local-token-two"
		producer = dispatcher.NewK8sDispatcher(observed, store, zap.NewNop(), config)
		if err := producer.EnsureAgentRuntime(ctx, agent); err != nil {
			test.Fatal(err)
		}
		rotated := &corev1.Secret{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(localToken), rotated); err != nil || rotated.UID != localToken.UID || string(rotated.Data["runtime-control-token"]) != "sg08-local-token-two" {
			test.Fatalf("source rotation not preserved after dispatcher restart: %v", err)
		}
		if err := admin.Delete(ctx, source, client.Preconditions{UID: &source.UID}); err != nil {
			test.Fatal(err)
		}
		beforeRejectedRevision := len(*created)
		if _, err := producer.EnsureAgentServiceRevisionWorkload(ctx, workload, "sg08-service", 2, agent); err == nil || !apierrors.IsNotFound(err) {
			test.Fatalf("deleted source did not stop new revision provisioning: %v", err)
		}
		if len(*created) != beforeRejectedRevision {
			test.Fatal("deleted source allowed partial revision provisioning")
		}
		retained := &corev1.Secret{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(localToken), retained); err != nil || retained.UID != localToken.UID || string(retained.Data["runtime-control-token"]) != "sg08-local-token-two" {
			test.Fatalf("unexpected materialized token state after source deletion: %v", err)
		}
	}
	var cleanupErr error
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		cleanupErr = producer.CleanupAgentServiceRevisionWorkloads(ctx, workload, "sg08-service", "")
		return cleanupErr == nil, nil
	}); err != nil {
		test.Fatalf("revision cleanup did not settle: %v; last error: %v", err, cleanupErr)
	}
	if err := producer.CleanupAgentResources(ctx, agent.ObjectMeta.Name, workload); err != nil {
		test.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		for _, expected := range (*created)[trackingStart:] {
			switch expected.(type) {
			case *appsv1.Deployment, *corev1.Service:
			default:
				continue
			}
			current := expected.DeepCopyObject().(client.Object)
			if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), current); apierrors.IsNotFound(err) {
				continue
			} else if err != nil {
				return false, err
			}
			if current.GetUID() != expected.GetUID() {
				return false, fmt.Errorf("Agent cleanup target %s was replaced", client.ObjectKeyFromObject(expected))
			}
			return false, nil
		}
		var pods corev1.PodList
		if err := admin.List(ctx, &pods, client.InNamespace(workload)); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			for _, container := range pod.Spec.Containers {
				if container.Name == "agent" {
					return false, nil
				}
			}
		}
		return true, nil
	}); err != nil {
		test.Fatal(fmt.Errorf("Agent cleanup did not settle: %w", err))
	}
	if preserveSource {
		test.Logf("production Agent dispatch, service revisions and cleanup settled using unchanged reviewed source=%s/%s; rotation/deletion remain separate mutable-fixture coverage", manifest.Namespace, sourceName)
	} else {
		test.Logf("production Agent resources created and revision Service available; source rotation updates copied Secret; source deletion blocks new revisions but retains copied credentials; Deployment/Service/Pod cleanup settled; source=%s/%s", manifest.Namespace, sourceName)
	}
}

func deployedAgentForward(test *testing.T, ctx context.Context, config *rest.Config, pod *corev1.Pod) string {
	return deployedPodForward(test, ctx, config, pod, 8080)
}

func deployedPodForward(test *testing.T, ctx context.Context, config *rest.Config, pod *corev1.Pod, port int) string {
	test.Helper()
	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		test.Fatal(err)
	}
	endpoint, err := url.Parse(config.Host + "/api/v1/namespaces/" + pod.Namespace + "/pods/" + pod.Name + "/portforward")
	if err != nil {
		test.Fatal(err)
	}
	stop, ready, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	forward, err := portforward.NewOnAddresses(spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, endpoint), []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		test.Fatal(err)
	}
	go func() { done <- forward.ForwardPorts() }()
	test.Cleanup(func() {
		close(stop)
		select {
		case err := <-done:
			if err != nil {
				test.Errorf("port-forward shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			test.Error("port-forward did not settle")
		}
	})
	select {
	case <-ready:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	ports, err := forward.GetPorts()
	if err != nil || len(ports) != 1 {
		test.Fatalf("port-forward ports=%v error=%v", ports, err)
	}
	return net.JoinHostPort("127.0.0.1", fmt.Sprint(ports[0].Local))
}
