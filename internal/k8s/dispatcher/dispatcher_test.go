// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/plugin"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	pluginv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// stubStore implements store.Store for dispatcher tests.
type stubStore struct {
	data      map[string][]byte
	revisions map[string]store.Revision
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func (s *stubStore) Ping(_ context.Context) error { return nil }
func (s *stubStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	key := kind + "/" + namespace + "/" + name
	if d, ok := s.data[key]; ok {
		return d, nil
	}
	return nil, store.ErrNotFound
}
func (s *stubStore) List(_ context.Context, _ string, _ store.ListOptions) ([][]byte, error) {
	return nil, nil
}
func (s *stubStore) Create(_ context.Context, _, _, _ string, _ []byte) error { return nil }
func (s *stubStore) Update(_ context.Context, _, _, _ string, _ []byte, _ int64) error {
	return nil
}
func (s *stubStore) Delete(_ context.Context, _, _, _ string) error { return nil }
func (s *stubStore) Watch(_ context.Context, _ string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, nil
}
func (s *stubStore) CreateRevision(_ context.Context, _, _, _ string, _ int64, _, _ []byte) error {
	return nil
}
func (s *stubStore) GetRevision(_ context.Context, kind, namespace, name string, revision int64) (*store.Revision, error) {
	key := fmt.Sprintf("%s/%s/%s/%d", kind, namespace, name, revision)
	if value, ok := s.revisions[key]; ok {
		return &value, nil
	}
	return nil, store.ErrNotFound
}
func (s *stubStore) ListRevisions(_ context.Context, _, _, _ string) ([]store.Revision, error) {
	return nil, nil
}
func (s *stubStore) AppendEvent(_ context.Context, _, _, _ string, _ store.InstanceEvent) error {
	return nil
}
func (s *stubStore) ListEvents(_ context.Context, _, _, _ string) ([]store.InstanceEvent, error) {
	return nil, nil
}

// --------------------------------------------------------------------------- //
// Test fixtures
// --------------------------------------------------------------------------- //

func newDispatcher(st *stubStore) *K8sDispatcher {
	return &K8sDispatcher{
		store:  st,
		logger: zap.NewNop(),
	}
}

func TestRuntimeReplicasReturnsOnlyRequestedAgentPodsWithExplicitReadiness(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	workloadKey := dispatcher.workloadKey("tenant-a", "researcher")
	ready := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "researcher-ready", Namespace: "runtimes", Labels: map[string]string{
			labelManagedBy: managedByValue, labelAgent: workloadKey, labelAgentName: "researcher",
		}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	unready := ready.DeepCopy()
	unready.Name = "researcher-unready"
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	otherAgent := ready.DeepCopy()
	otherAgent.Name = "writer-ready"
	otherAgent.Labels = map[string]string{labelManagedBy: managedByValue, labelAgent: "other-workload", labelAgentName: "writer"}
	dispatcher.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ready, unready, otherAgent).Build()
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
	}

	replicas, err := dispatcher.RuntimeReplicas(context.Background(), instance)
	if err != nil {
		t.Fatalf("RuntimeReplicas() error = %v", err)
	}
	got := make(map[string]intctrl.RuntimeReplica, len(replicas))
	for _, replica := range replicas {
		got[replica.Name] = replica
	}
	if len(got) != 2 || !got["researcher-ready"].Ready || got["researcher-unready"].Ready {
		t.Fatalf("RuntimeReplicas() = %#v, want ready and unready researcher replicas only", replicas)
	}
	for _, replica := range replicas {
		if replica.AgentName != "researcher" {
			t.Fatalf("replica %q AgentName = %q, want researcher", replica.Name, replica.AgentName)
		}
	}
}

func TestRuntimeInvokeEndpointRevalidatesPreferredReplica(t *testing.T) {
	const (
		namespace   = "runtimes"
		workloadKey = "tenant-a-researcher"
		agentName   = "researcher"
	)
	readyPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "researcher-0", Namespace: namespace, Labels: map[string]string{
				labelManagedBy: managedByValue, labelAgent: workloadKey, labelAgentName: agentName,
			}},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				PodIP:      "10.0.0.8",
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		}
	}
	tests := []struct {
		name      string
		pod       *corev1.Pod
		preferred string
		wantPod   bool
	}{
		{name: "valid ready same-Agent pod", pod: readyPod(), preferred: "researcher-0", wantPod: true},
		{name: "missing pod", preferred: "missing"},
		{name: "unready pod", pod: readyPod(), preferred: "researcher-0"},
		{name: "different Agent label", pod: readyPod(), preferred: "researcher-0"},
		{name: "empty pod IP", pod: readyPod(), preferred: "researcher-0"},
	}
	tests[2].pod.Status.Conditions[0].Status = corev1.ConditionFalse
	tests[3].pod.Labels[labelAgentName] = "writer"
	tests[4].pod.Status.PodIP = ""

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("add core scheme: %v", err)
			}
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if test.pod != nil {
				builder = builder.WithObjects(test.pod)
			}
			dispatcher := NewK8sDispatcher(builder.Build(), nil, zap.NewNop(), Config{})
			got := dispatcher.runtimeInvokeEndpoint(context.Background(), namespace, workloadKey, agentName, test.preferred)
			want := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/invoke", serviceName(workloadKey), namespace, agentPort)
			if test.wantPod {
				want = "http://10.0.0.8:8080/invoke"
			}
			if got != want {
				t.Fatalf("runtimeInvokeEndpoint() = %q, want %q", got, want)
			}
		})
	}
}

type runtimePluginSelectorStub struct {
	connection *plugin.PluginConnection
	capability plugin.CapabilityType
	name       string
}

func (s *runtimePluginSelectorStub) GetReadyPlugin(capability plugin.CapabilityType, name string) (*plugin.PluginConnection, error) {
	s.capability = capability
	s.name = name
	return s.connection, nil
}

type runtimePluginServerStub struct {
	pluginv1alpha1.UnimplementedRuntimeServer
	request *pluginv1alpha1.CreateSessionRequest
}

func (s *runtimePluginServerStub) CreateSession(_ context.Context, request *pluginv1alpha1.CreateSessionRequest) (*pluginv1alpha1.CreateSessionResponse, error) {
	s.request = request
	return &pluginv1alpha1.CreateSessionResponse{SessionId: "plugin-session"}, nil
}

func TestDispatchUsesNamedRuntimePlugin(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	runtimeServer := &runtimePluginServerStub{}
	pluginv1alpha1.RegisterRuntimeServer(grpcServer, runtimeServer)
	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			t.Errorf("serve runtime plugin: %v", err)
		}
	}()
	t.Cleanup(grpcServer.Stop)

	connection, err := grpc.NewClient("passthrough:///runtime-plugin", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial runtime plugin: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4o"},
			Runtime:  &v1alpha1.AgentRuntimeSpec{Class: "runtime-plugin"},
		},
	}
	selector := &runtimePluginSelectorStub{connection: &plugin.PluginConnection{
		Info:  plugin.PluginInfo{Name: "runtime-plugin", Capabilities: []plugin.CapabilityType{plugin.CapabilityRuntime}},
		State: plugin.StateReady,
		Conn:  connection,
	}}
	dispatcher := NewK8sDispatcher(nil, &stubStore{data: map[string][]byte{
		"Agent/tenant-a/researcher": resourceBytes(t, agent),
	}}, zap.NewNop(), Config{})
	dispatcher.SetRuntimePluginSelector(selector)
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "researcher"},
			SessionContext: map[string]string{"input": "investigate"},
		},
	}

	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if sessionID != "plugin-session" {
		t.Fatalf("session ID = %q, want plugin-session", sessionID)
	}
	if selector.capability != plugin.CapabilityRuntime || selector.name != "runtime-plugin" {
		t.Fatalf("plugin selection = (%s, %q), want (runtime, runtime-plugin)", selector.capability, selector.name)
	}
	if runtimeServer.request == nil || runtimeServer.request.AgentName != "researcher" || runtimeServer.request.Namespace != "tenant-a" || runtimeServer.request.Context["input"] != "investigate" {
		t.Fatalf("CreateSession request = %+v", runtimeServer.request)
	}
}

func TestDispatchMaterializesWarmPoolWithoutInvocation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4o"},
		},
	}
	agentData, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	st := &stubStore{data: map[string][]byte{"Agent/tenant-a/researcher": agentData}}
	logCore, logs := observer.New(zap.InfoLevel)
	dispatcher := NewK8sDispatcher(
		fake.NewClientBuilder().WithScheme(scheme).Build(), st, zap.New(logCore), Config{AgentImage: "runtime:v1"},
	)
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a", UID: "instance-uid-1"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"},
		},
		Status: v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhasePending},
	}

	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if sessionID == "" {
		t.Fatal("Dispatch() returned an empty session ID")
	}
	retrySessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("retry Dispatch() error = %v", err)
	}
	if retrySessionID != sessionID {
		t.Fatalf("retry session ID = %q, want stable %q", retrySessionID, sessionID)
	}

	instance.Status.Phase = v1alpha1.PhaseRunning
	restartSessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("restart Dispatch() error = %v", err)
	}
	if restartSessionID == sessionID {
		t.Fatalf("restart session ID = %q, want a new session", restartSessionID)
	}
	restartRetrySessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("restart retry Dispatch() error = %v", err)
	}
	if restartRetrySessionID != restartSessionID {
		t.Fatalf("restart retry session ID = %q, want stable %q", restartRetrySessionID, restartSessionID)
	}
	if got := logs.FilterMessage("dispatch skipping /invoke: empty input").Len(); got != 4 {
		t.Fatalf("empty-input skip log count = %d, want 4", got)
	}

	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: "tenant-a", Name: deploymentName("researcher")}
	if err := dispatcher.client.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "runtime:v1" {
		t.Fatalf("deployment image = %q, want runtime:v1", got)
	}
	var service corev1.Service
	serviceKey := client.ObjectKey{Namespace: "tenant-a", Name: serviceName("researcher")}
	if err := dispatcher.client.Get(context.Background(), serviceKey, &service); err != nil {
		t.Fatalf("get service: %v", err)
	}
}

func TestDispatchUsesPinnedAgentRevisionConfiguration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	currentAgent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "current-model"}, Instructions: v1alpha1.AgentInstructions{Inline: "current instructions"}},
	}
	pinnedRevision := int64(2)
	pinnedAgent := currentAgent
	pinnedAgent.Spec.ModelRef.Name = "pinned-model"
	pinnedAgent.Spec.Instructions.Inline = "pinned instructions"
	currentData, _ := json.Marshal(currentAgent)
	pinnedData, _ := json.Marshal(pinnedAgent)
	st := &stubStore{
		data: map[string][]byte{"Agent/tenant-a/researcher": currentData},
		revisions: map[string]store.Revision{
			"Agent/tenant-a/researcher/2": {Revision: pinnedRevision, Data: pinnedData},
		},
	}
	dispatcher := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).Build(), st, zap.NewNop(), Config{AgentImage: "runtime:v1"})
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "pinned-instance", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}, AgentRevision: &pinnedRevision},
	}
	if _, err := dispatcher.Dispatch(context.Background(), instance); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	var deployment appsv1.Deployment
	if err := dispatcher.client.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: deploymentName("researcher")}, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	env := envToMap(deployment.Spec.Template.Spec.Containers[0].Env)
	if env["KOGNITION_SYSTEM_PROMPT"] != "pinned instructions" || env["KOGNITION_AGENT_NAME"] != "researcher" {
		t.Fatalf("runtime environment = %v, want pinned prompt for researcher identity", env)
	}
}

func TestDispatchUsesPinnedAgentServiceWorkload(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pinnedRevision := int64(2)
	agent := v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "pinned-model"}},
	}
	service := v1alpha1.AgentService{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "research-api", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentServiceSpec{
			AgentRef:    v1alpha1.LocalObjectReference{Name: "researcher"},
			RevisionPin: &pinnedRevision,
		},
		Status: v1alpha1.AgentServiceStatus{
			Phase:          v1alpha1.PhaseReady,
			ReadyInstances: 1,
			Endpoint:       "http://research-api.tenant-a.svc.cluster.local:8080/invoke",
		},
	}
	agentData, _ := json.Marshal(agent)
	serviceData, _ := json.Marshal(service)
	st := &stubStore{
		data: map[string][]byte{
			"Agent/tenant-a/researcher":          agentData,
			"AgentService/tenant-a/research-api": serviceData,
		},
		revisions: map[string]store.Revision{
			"Agent/tenant-a/researcher/2": {Revision: pinnedRevision, Data: agentData},
		},
	}
	dispatcher := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).Build(), st, zap.NewNop(), Config{AgentImage: "runtime:v1"})
	requests := make(chan *http.Request, 2)
	dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests <- request
		return &http.Response{StatusCode: http.StatusAccepted, Body: http.NoBody, Header: make(http.Header), Request: request}, nil
	})
	dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests <- request
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"phase":"completed","output":"done"}`)), Header: make(http.Header), Request: request}, nil
	})
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:        "service-instance",
			Namespace:   "tenant-a",
			Annotations: map[string]string{intctrl.AgentServiceExecutionAnnotation: "research-api"},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "researcher"},
			AgentRevision:  &pinnedRevision,
			SessionContext: map[string]string{"input": "investigate"},
		},
	}
	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	select {
	case request := <-requests:
		wantEndpoint := fmt.Sprintf("http://%s.tenant-a.svc.cluster.local:8080/invoke", agentServiceRevisionWorkloadName("tenant-a", "research-api", pinnedRevision))
		if request.URL.String() != wantEndpoint {
			t.Fatalf("invoke URL = %q, want %q", request.URL.String(), wantEndpoint)
		}
	case <-time.After(time.Second):
		t.Fatal("expected AgentService /invoke request")
	}
	instance.Status.SessionID = sessionID
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if !status.Completed || status.Output != "done" {
		t.Fatalf("Status() = %#v, want completed output", status)
	}
	statusRequest := <-requests
	wantStatusHost := fmt.Sprintf("%s.tenant-a.svc.cluster.local:8080", agentServiceRevisionWorkloadName("tenant-a", "research-api", pinnedRevision))
	if statusRequest.URL.Path != "/status" || statusRequest.URL.Query().Get("session_id") != sessionID || statusRequest.Host != wantStatusHost {
		t.Fatalf("status request = %s, want service /status with session ID", statusRequest.URL.String())
	}
	var shared appsv1.Deployment
	err = dispatcher.client.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: deploymentName("researcher")}, &shared)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("shared Agent deployment error = %v, want not found", err)
	}
}

func TestPinnedAgentServiceExecutionSurvivesServiceRepin(t *testing.T) {
	pinnedRevision := int64(1)
	newRevision := int64(2)
	service := v1alpha1.AgentService{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "research-api", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentServiceSpec{
			AgentRef:    v1alpha1.LocalObjectReference{Name: "researcher"},
			RevisionPin: &newRevision,
		},
		Status: v1alpha1.AgentServiceStatus{Phase: v1alpha1.PhaseReady, ReadyInstances: 1},
	}
	serviceData, _ := json.Marshal(service)
	st := &stubStore{data: map[string][]byte{"AgentService/tenant-a/research-api": serviceData}}
	dispatcher := NewK8sDispatcher(nil, st, zap.NewNop(), Config{})
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "tenant-a", Annotations: map[string]string{intctrl.AgentServiceExecutionAnnotation: "research-api"}},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:      v1alpha1.LocalObjectReference{Name: "researcher"},
			AgentRevision: &pinnedRevision,
		},
	}
	endpoint, routed, err := dispatcher.agentServiceEndpoint(context.Background(), instance)
	if err != nil || !routed {
		t.Fatalf("agentServiceEndpoint() = %q, %t, %v", endpoint, routed, err)
	}
	want := fmt.Sprintf("http://%s.tenant-a.svc.cluster.local:8080/invoke", agentServiceRevisionWorkloadName("tenant-a", "research-api", pinnedRevision))
	if endpoint != want {
		t.Fatalf("endpoint = %q, want %q after service repin", endpoint, want)
	}
}

func TestDispatchInvokesReadyRuntime(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4o"}},
	}
	agentData, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("marshal agent: %v", err)
	}
	st := &stubStore{data: map[string][]byte{"Agent/tenant-a/researcher": agentData}}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.Deployment{}).
		Build()
	dispatcher := NewK8sDispatcher(fakeClient, st, zap.NewNop(), Config{AgentImage: "runtime:v1"})
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "instance-1",
			Namespace: "tenant-a",
			Annotations: map[string]string{
				observability.AnnotationTraceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
			},
		},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef:       v1alpha1.LocalObjectReference{Name: "researcher"},
			SessionContext: map[string]string{"input": "investigate"},
		},
	}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt-4o"}}
	envVars, err := dispatcher.buildEnvVars(context.Background(), "tenant-a", agent, model, instance, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.ensureRuntimeResources(context.Background(), "tenant-a", agent, envVars); err != nil {
		t.Fatalf("prepare runtime resources: %v", err)
	}
	dispatcher.config.AgentImage = "runtime:v2"
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: "tenant-a", Name: deploymentName("researcher")}
	if err := fakeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	deployment.Status = appsv1.DeploymentStatus{
		ObservedGeneration: deployment.Generation,
		Replicas:           1,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	}
	if err := fakeClient.Status().Update(context.Background(), &deployment); err != nil {
		t.Fatalf("mark deployment ready: %v", err)
	}

	type invocation struct {
		body        map[string]string
		traceparent string
		err         error
	}
	requests := make(chan invocation, 1)
	acceptInvocation := make(chan struct{})
	defer func() {
		select {
		case <-acceptInvocation:
		default:
			close(acceptInvocation)
		}
	}()
	dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]string
		err := json.NewDecoder(request.Body).Decode(&body)
		requests <- invocation{body: body, traceparent: request.Header.Get("traceparent"), err: err}
		<-acceptInvocation
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})

	sessionID, err := dispatcher.Dispatch(context.Background(), instance)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	select {
	case got := <-requests:
		if got.err != nil {
			t.Fatalf("decode invocation: %v", got.err)
		}
		if got.body["message"] != "investigate" || got.body["session_id"] != sessionID {
			t.Fatalf("invocation body = %#v, session ID = %q", got.body, sessionID)
		}
		if got.traceparent != instance.ObjectMeta.Annotations[observability.AnnotationTraceparent] {
			t.Fatalf("traceparent header = %q, want %q", got.traceparent, instance.ObjectMeta.Annotations[observability.AnnotationTraceparent])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runtime invocation")
	}
	started := time.Now().Add(-time.Minute)
	instance.Status.SessionID = sessionID
	instance.Status.StartTime = &started
	dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"phase":"Running","uptime_seconds":10}`)),
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Alive || !status.Pending {
		t.Fatalf("queued cold-start Status() = %+v, %v; want alive and pending until acceptance", status, err)
	}
	close(acceptInvocation)
	deadline := time.Now().Add(time.Second)
	for {
		status, err = dispatcher.Status(context.Background(), instance)
		if err != nil || !status.Alive {
			t.Fatalf("accepted cold-start Status() = %+v, %v; initial startup must not be a restart", status, err)
		}
		if !status.Pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted runtime invocation remained pending")
		}
		time.Sleep(time.Millisecond)
	}
	if err := fakeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("get reconciled deployment: %v", err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "runtime:v2" {
		t.Fatalf("reconciled deployment image = %q, want runtime:v2", got)
	}
	t.Run("acknowledged session is still fenced after a genuine runtime restart", func(t *testing.T) {
		time.Sleep(6 * time.Second)
		dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"phase":"Running","uptime_seconds":0.1}`)),
				Header:     make(http.Header),
				Request:    request,
			}, nil
		})
		status, err := dispatcher.Status(context.Background(), instance)
		if err != nil || status.Alive || !strings.Contains(status.Error, "runtime restarted") {
			t.Fatalf("post-acceptance restart Status() = %+v, %v; want unavailable outcome", status, err)
		}
	})
}

func TestDispatchFallsBackToDirectAgentLookup(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	if err := k8sapi.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kognition scheme: %v", err)
	}
	directAgent := &k8sapi.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec:       k8sapi.AgentSpec{ModelRef: k8sapi.LocalObjectReference{Name: "gpt-4o"}},
	}
	dispatcher := NewK8sDispatcher(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(directAgent).Build(),
		&stubStore{data: map[string][]byte{}},
		zap.NewNop(),
		Config{AgentImage: "runtime:v1"},
	)
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"},
		},
	}

	if _, err := dispatcher.Dispatch(context.Background(), instance); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: "tenant-a", Name: deploymentName("researcher")}
	if err := dispatcher.client.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
}

func TestDispatchRejectsMissingAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := k8sapi.AddToScheme(scheme); err != nil {
		t.Fatalf("add Kognition scheme: %v", err)
	}
	dispatcher := NewK8sDispatcher(
		fake.NewClientBuilder().WithScheme(scheme).Build(),
		&stubStore{data: map[string][]byte{}},
		zap.NewNop(),
		Config{},
	)
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentInstanceSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "missing"},
		},
	}

	if _, err := dispatcher.Dispatch(context.Background(), instance); err == nil {
		t.Fatal("Dispatch() error = nil, want missing Agent error")
	}
}

func TestRuntimeImageSelection(t *testing.T) {
	d := &K8sDispatcher{config: Config{
		AgentImage: "native:latest",
		RuntimeImages: map[string]string{
			"autogen": "autogen:latest",
		},
	}}

	for _, tc := range []struct {
		name         string
		runtimeClass string
		wantImage    string
		wantErr      bool
	}{
		{name: "implicit default", wantImage: "native:latest"},
		{name: "native alias", runtimeClass: "native", wantImage: "native:latest"},
		{name: "configured adapter", runtimeClass: "autogen", wantImage: "autogen:latest"},
		{name: "unconfigured adapter", runtimeClass: "crewai", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &v1alpha1.Agent{}
			if tc.runtimeClass != "" {
				agent.Spec.Runtime = &v1alpha1.AgentRuntimeSpec{Class: tc.runtimeClass}
			}
			image, err := d.runtimeImage(agent)
			if (err != nil) != tc.wantErr {
				t.Fatalf("runtimeImage() error = %v, wantErr %v", err, tc.wantErr)
			}
			if image != tc.wantImage {
				t.Fatalf("runtimeImage() = %q, want %q", image, tc.wantImage)
			}
		})
	}
}

func TestGeneratedSDKWorkloadMatchesProtectedConfiguration(test *testing.T) {
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := register(scheme); err != nil {
			test.Fatal(err)
		}
	}
	admin := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fixture-auth", Namespace: "control"}, Data: map[string][]byte{"runtime-control-token": []byte("test-runtime-token")}}).Build()
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "sdk-agent", Namespace: "tenant", UID: "agent-uid"}, Spec: v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}, Runtime: &v1alpha1.AgentRuntimeSpec{Class: "autogen"}}}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o"}}
	storage := &stubStore{data: map[string][]byte{"Model/tenant/model": resourceBytes(test, model)}}
	agent.Annotations = map[string]string{"kognition.dev/kagent-tool-aliases": `{"port-query":"QueryTool"}`, "kognition.dev/agent-delegation-enabled": "false"}
	agent.Spec.Tools = []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "port-query"}, Required: true}}
	storage.data["Tool/tenant/port-query"] = resourceBytes(test, &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "port-query", Namespace: "tenant", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{Protocol: "OpenAPI"}})
	config := externalTestConfig(test)
	config.RuntimeAuthTokenSecretName = "fixture-auth"
	config.ControllerNamespace = "control"
	config.KubeletProbeIngressCIDRs = []string{"192.0.2.0/24"}
	config.AgentImage = "native:v1"
	config.RuntimeImages = map[string]string{"autogen": "autogen:v1"}
	config.RuntimeTLSSecretName, config.RuntimeCAFile = "operator-tls", "/operator/ca.crt"
	config.ToolGatewayURL, config.MemoryPolicyGatewayURL, config.AgentCommunicationGatewayURL = "http://tool", "http://memory", "http://communication"
	nativeOnly := config
	nativeOnly.RuntimeImages = nil
	unregistered := NewK8sDispatcher(admin, storage, zap.NewNop(), nativeOnly)
	if err := unregistered.EnsureAgentRuntime(context.Background(), agent); err == nil || !strings.Contains(err.Error(), "no configured adapter image") {
		test.Fatalf("unregistered AutoGen runtime was not rejected: %v", err)
	}
	var beforeRegistration appsv1.DeploymentList
	if err := admin.List(context.Background(), &beforeRegistration); err != nil || len(beforeRegistration.Items) != 0 {
		test.Fatalf("unregistered runtime created a workload: count=%d error=%v", len(beforeRegistration.Items), err)
	}
	dispatcher := NewK8sDispatcher(admin, storage, zap.NewNop(), config)
	if err := dispatcher.EnsureAgentRuntime(context.Background(), agent); err != nil {
		test.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := admin.Get(context.Background(), client.ObjectKey{Namespace: "tenant", Name: deploymentName(dispatcher.workloadKey("tenant", agent.Name))}, &deployment); err != nil {
		test.Fatal(err)
	}
	loaded := map[string]string{}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.StartupProbe == nil || container.StartupProbe.HTTPGet == nil || container.StartupProbe.HTTPGet.Path != "/healthz" || container.StartupProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS || container.StartupProbe.TimeoutSeconds < 5 || container.StartupProbe.FailureThreshold*container.StartupProbe.PeriodSeconds < 120 {
		test.Fatal("generated AutoGen worker lacks a bounded HTTPS SDK startup fence")
	}
	if container.LivenessProbe.TimeoutSeconds < 5 || container.ReadinessProbe.TimeoutSeconds < 5 {
		test.Fatal("AutoGen worker probes can kill a slow-starting SDK session after a one-second health check")
	}
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		if variable.ValueFrom == nil && !strings.HasPrefix(variable.Name, "KOGNITION_OTEL_") && !strings.HasPrefix(variable.Name, "KOGNITION_CONTROL_TLS_") && variable.Name != "KOGNITION_EXTERNAL_RUNTIME" && variable.Name != "KOGNITION_RUNTIME_AUTH_TOKEN" {
			loaded[variable.Name] = variable.Value
		}
	}
	configuration, err := dispatcher.externalConfiguration(context.Background(), "", "tenant", agent, model)
	if err != nil {
		test.Fatal(err)
	}
	var expected map[string]string
	if err := json.Unmarshal([]byte(configuration), &expected); err != nil {
		test.Fatal(err)
	}
	if loaded["KOGNITION_NAMESPACE"] != "tenant" || !reflect.DeepEqual(loaded, expected) {
		test.Fatalf("generated SDK startup differs from protected binding: loaded=%v expected=%v", loaded, expected)
	}
	if loaded["KOGNITION_AGENT_DELEGATION_ENABLED"] != "false" {
		test.Fatal("explicit reviewed delegation opt-out was not protected in generated configuration")
	}
	var resolved []struct{ Name, RuntimeName string }
	if err := json.Unmarshal([]byte(loaded["KOGNITION_TOOLS_RESOLVED"]), &resolved); err != nil || len(resolved) != 1 || resolved[0].Name != "port-query" || resolved[0].RuntimeName != "QueryTool" {
		test.Fatalf("protected generated tool alias differs: %+v error=%v", resolved, err)
	}
	for _, encoded := range []string{`{`, `{}`, `{"wrong-tool":"QueryTool"}`, `{"port-query":"bad/name"}`, `{"port-query":"QueryTool"} {}`, `{"port-query":"other","port-query":"QueryTool"}`} {
		changed := *agent
		changed.Annotations = map[string]string{"kognition.dev/kagent-tool-aliases": encoded}
		if err := dispatcher.EnsureAgentRuntime(context.Background(), &changed); err == nil {
			test.Fatalf("invalid protected alias mapping accepted: %s", encoded)
		}
	}
}

func TestEnsureDeploymentUsesRuntimeClassImage(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{
		AgentImage:           "native:latest",
		RuntimeTLSSecretName: "operator-worker-tls",
		RuntimeCAFile:        "/operator/ca.crt",
		RuntimeImages: map[string]string{
			"autogen": "autogen:v1",
		},
	})
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Runtime: &v1alpha1.AgentRuntimeSpec{Class: "autogen"},
		},
	}

	if err := d.ensureDeployment(context.Background(), "default", agent, nil); err != nil {
		t.Fatalf("ensureDeployment() create error = %v", err)
	}
	var deployment appsv1.Deployment
	key := client.ObjectKey{Namespace: "default", Name: deploymentName("researcher")}
	if err := fakeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "autogen:v1" {
		t.Fatalf("created deployment image = %q, want autogen:v1", got)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS || container.LivenessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS || len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly || deployment.Spec.Template.Spec.Volumes[0].Secret.SecretName != "operator-worker-tls" {
		t.Fatal("AutoGen workload did not retain protected operator TLS material and HTTPS probes")
	}

	agent.Spec.Runtime.Class = "native"
	if err := d.ensureDeployment(context.Background(), "default", agent, nil); err != nil {
		t.Fatalf("ensureDeployment() update error = %v", err)
	}
	if err := fakeClient.Get(context.Background(), key, &deployment); err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "native:latest" {
		t.Fatalf("updated deployment image = %q, want native:latest", got)
	}
	if len(deployment.Spec.Template.Spec.Volumes) != 0 || deployment.Spec.Template.Spec.Containers[0].ReadinessProbe.HTTPGet.Scheme == corev1.URISchemeHTTPS {
		t.Fatal("native transition retained AutoGen control TLS")
	}
	startup := deployment.Spec.Template.Spec.Containers[0].StartupProbe
	if startup == nil || startup.HTTPGet == nil || startup.HTTPGet.Scheme != corev1.URISchemeHTTP || startup.HTTPGet.Path != "/healthz" || startup.TimeoutSeconds != 5 || startup.PeriodSeconds*startup.FailureThreshold != 120 {
		t.Fatal("native shared worker lacks bounded HTTP startup protection")
	}
}

func TestAgentServiceRevisionWorkloadNameIsBoundedAndDistinct(t *testing.T) {
	name := agentServiceRevisionWorkloadName("tenant-a", strings.Repeat("revision-service-", 8), math.MaxInt64)
	if len(name) > 63 || !strings.HasPrefix(name, "agent-svc-") {
		t.Fatalf("revision workload name = %q (length %d), want a bounded agent-svc name", name, len(name))
	}
	if name != agentServiceRevisionWorkloadName("tenant-a", strings.Repeat("revision-service-", 8), math.MaxInt64) {
		t.Fatalf("revision workload name is not deterministic: %q", name)
	}
	if name == agentServiceRevisionWorkloadName("tenant-a", strings.Repeat("revision-service-", 8), math.MaxInt64-1) {
		t.Fatalf("revision workload name %q does not distinguish revisions", name)
	}
}

func TestEnsureAgentRuntimeRefreshesStaleDeploymentTracingAndCredentials(t *testing.T) {
	t.Setenv("KOGNITION_OTEL_TRACES_ENABLED", "true")
	t.Setenv("KOGNITION_OTEL_TRACES_ENDPOINT", "jaeger-collector.kognition-observability.svc:4317")
	t.Setenv("KOGNITION_OTEL_TRACES_PROTOCOL", "grpc")
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "gpt-4o"},
		},
	}
	model := &v1alpha1.Model{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "gpt-4o", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:      "azure-openai",
			ModelID:       "gpt-4o",
			Endpoint:      "https://example.openai.azure.com",
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "model-creds"},
		},
	}
	secret := &v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "model-creds",
			Namespace: "default",
			Annotations: map[string]string{
				"kognition.dev/internal-secret-data": `{"type":"azure-identity","credentialType":"client-secret","tenantId":"tenant-1","clientId":"client-1","clientSecret":"secret-1"}`,
			},
		},
	}
	modelData, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal model: %v", err)
	}
	secretData, err := json.Marshal(secret)
	if err != nil {
		t.Fatalf("marshal secret: %v", err)
	}
	store := &stubStore{data: map[string][]byte{
		"Model/default/gpt-4o":          modelData,
		"SecretRef/default/model-creds": secretData,
	}}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        deploymentName("researcher"),
			Namespace:   "default",
			Annotations: map[string]string{AnnotationOwnerAgent: "default/researcher"},
		},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "agent",
			Image: "runtime:v1",
			Env:   []corev1.EnvVar{{Name: "KOGNITION_OTEL_TRACES_ENABLED", Value: "false"}},
		}}}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()
	dispatcher := NewK8sDispatcher(fakeClient, store, zap.NewNop(), Config{AgentImage: "runtime:v2"})

	if err := dispatcher.EnsureAgentRuntime(context.Background(), agent); err != nil {
		t.Fatalf("EnsureAgentRuntime() error = %v", err)
	}
	var updated appsv1.Deployment
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &updated); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	container := updated.Spec.Template.Spec.Containers[0]
	if container.Image != "runtime:v2" {
		t.Fatalf("image = %q, want runtime:v2", container.Image)
	}
	env := envToMap(container.Env)
	if env["KOGNITION_OTEL_TRACES_ENABLED"] != "true" {
		t.Fatalf("KOGNITION_OTEL_TRACES_ENABLED = %q, want true", env["KOGNITION_OTEL_TRACES_ENABLED"])
	}
	if env["KOGNITION_OTEL_TRACES_ENDPOINT"] != "jaeger-collector.kognition-observability.svc:4317" {
		t.Fatalf("KOGNITION_OTEL_TRACES_ENDPOINT = %q", env["KOGNITION_OTEL_TRACES_ENDPOINT"])
	}
	if env["AZURE_CREDENTIAL_TYPE"] != "client-secret" || env["AZURE_CLIENT_ID"] != "client-1" || env["AZURE_CLIENT_SECRET"] != "secret-1" {
		t.Fatalf("azure credential env = %#v", env)
	}
	var service corev1.Service
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: serviceName("researcher")}, &service); err != nil {
		t.Fatalf("get ensured service: %v", err)
	}
	if service.Annotations[AnnotationOwnerAgent] != "default/researcher" {
		t.Fatalf("service owner = %q, want default/researcher", service.Annotations[AnnotationOwnerAgent])
	}
}

func TestEnsureRuntimeResourcesRepairsMalformedExistingObjects(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	replicas := int32(3)
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentSpec{Replicas: &replicas},
	}
	ownerRef := "tenant-a/researcher"
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-researcher", Namespace: "tenant-a",
		Annotations: map[string]string{AnnotationOwnerAgent: ownerRef},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent-researcher", Namespace: "tenant-a"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, service).Build()
	dispatcher := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{AgentImage: "native:v2"})
	wantEnv := []corev1.EnvVar{{Name: "MODE", Value: "production"}}

	if err := dispatcher.ensureDeployment(context.Background(), "tenant-a", agent, wantEnv); err != nil {
		t.Fatalf("repair malformed deployment: %v", err)
	}
	var stored appsv1.Deployment
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &stored); err != nil {
		t.Fatalf("get repaired deployment: %v", err)
	}
	if stored.Spec.Replicas == nil || *stored.Spec.Replicas != replicas {
		t.Fatalf("replicas = %v, want %d", stored.Spec.Replicas, replicas)
	}
	if len(stored.Spec.Template.Spec.Containers) != 1 || stored.Spec.Template.Spec.Containers[0].Image != "native:v2" {
		t.Fatalf("containers = %#v, want repaired native runtime", stored.Spec.Template.Spec.Containers)
	}
	if !envVarsEqual(stored.Spec.Template.Spec.Containers[0].Env, wantEnv) {
		t.Fatalf("env = %#v, want %#v", stored.Spec.Template.Spec.Containers[0].Env, wantEnv)
	}

	stored.Spec.Template.Spec.Containers[0].Image = "native:stale"
	stored.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "MODE", Value: "stale"}}
	if err := fakeClient.Update(context.Background(), &stored); err != nil {
		t.Fatalf("seed stale deployment: %v", err)
	}
	if err := dispatcher.ensureDeployment(context.Background(), "tenant-a", agent, wantEnv); err != nil {
		t.Fatalf("repair stale deployment: %v", err)
	}
	if err := dispatcher.ensureService(context.Background(), "tenant-a", agent); err != nil {
		t.Fatalf("repair service ownership: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(service), service); err != nil {
		t.Fatalf("get repaired service: %v", err)
	}
	if got := service.Annotations[AnnotationOwnerAgent]; got != ownerRef {
		t.Fatalf("service owner = %q, want %q", got, ownerRef)
	}
}

func TestEnsureDeploymentPreservesExplicitZeroReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	zeroReplicas := int32(0)
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "paused", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentSpec{Replicas: &zeroReplicas},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	dispatcher := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{AgentImage: "native:v1"})

	if err := dispatcher.ensureDeployment(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatalf("ensure deployment: %v", err)
	}
	var deployment appsv1.Deployment
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "agent-paused"}, &deployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", deployment.Spec.Replicas)
	}
}

func TestEnvVarsEqualTreatsMatchingSecretReferencesAsEqual(t *testing.T) {
	optional := true
	secretReference := &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "model-credentials"},
		Key:                  "api-key",
		Optional:             &optional,
	}}
	a := []corev1.EnvVar{{Name: "MODEL_API_KEY", ValueFrom: secretReference}}
	b := []corev1.EnvVar{{Name: "MODEL_API_KEY", ValueFrom: secretReference.DeepCopy()}}

	if !envVarsEqual(a, b) {
		t.Fatal("matching secret-backed environment variables should be equal")
	}

	b[0].ValueFrom.SecretKeyRef.Key = "other-key"
	if envVarsEqual(a, b) {
		t.Fatal("different secret-backed environment variables should not be equal")
	}
}

func TestEnsureAgentServiceRevisionWorkloadLifecycle(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secrets", Namespace: "control"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	dispatcher := NewK8sDispatcher(k8sClient, &stubStore{data: map[string][]byte{}}, zap.NewNop(), Config{AgentImage: "runtime:v1", RuntimeAuthToken: "controller-token", RuntimeAuthTokenSecretName: "runtime-secrets", RuntimeNetworkPolicyEnabled: true, KubeletProbeIngressCIDRs: []string{"172.16.0.0/12"}, ControllerNamespace: "control", MemoryPolicyGatewayURL: "http://gateway:8091"})
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "pinned-model"},
			Instructions: v1alpha1.AgentInstructions{Inline: "pinned instructions"},
		},
	}

	first, err := dispatcher.EnsureAgentServiceRevisionWorkload(context.Background(), "tenant-a", "research-api", 1, agent)
	if err != nil {
		t.Fatalf("create revision workload: %v", err)
	}
	if first.Namespace != "tenant-a" || first.Name == "" || first.Selector[labelAgentServiceRevision] != "1" {
		t.Fatalf("first revision workload = %#v", first)
	}
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: first.Namespace, Name: first.Name}, &deployment); err != nil {
		t.Fatalf("get created revision Deployment: %v", err)
	}
	startup := deployment.Spec.Template.Spec.Containers[0].StartupProbe
	if startup == nil || startup.HTTPGet == nil || startup.HTTPGet.Scheme != corev1.URISchemeHTTP || startup.HTTPGet.Path != "/healthz" || startup.TimeoutSeconds != 5 || startup.PeriodSeconds*startup.FailureThreshold != 120 {
		t.Fatal("native revision worker lacks bounded HTTP startup protection")
	}
	deployment.Spec.Template.Spec.Containers[0].StartupProbe = nil
	if err := k8sClient.Update(context.Background(), &deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.EnsureAgentServiceRevisionWorkload(context.Background(), "tenant-a", "research-api", 1, agent); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: first.Namespace, Name: first.Name}, &deployment); err != nil || deployment.Spec.Template.Spec.Containers[0].StartupProbe == nil {
		t.Fatal("existing revision workload startup protection was not reconciled")
	}
	annotationOnly := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      "unrelated-deployment",
		Namespace: first.Namespace,
		Annotations: map[string]string{
			annotationOwnerAgentService: "tenant-a/research-api",
		},
	}}
	if err := k8sClient.Create(context.Background(), annotationOnly); err != nil {
		t.Fatalf("create annotation-only Deployment: %v", err)
	}
	env := make(map[string]string)
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		env[variable.Name] = variable.Value
	}
	if deployment.Spec.Template.Spec.Containers[0].Image != "runtime:v1" || env["KOGNITION_SYSTEM_PROMPT"] != "pinned instructions" {
		t.Fatalf("created revision Deployment = %#v", deployment.Spec.Template.Spec.Containers)
	}
	var revisionPolicy networkingv1.NetworkPolicy
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: first.Namespace, Name: first.Name + "-network"}, &revisionPolicy); err != nil {
		t.Fatalf("get created revision NetworkPolicy: %v", err)
	}
	if !reflect.DeepEqual(revisionPolicy.Spec.PodSelector.MatchLabels, first.Selector) || revisionPolicy.Annotations[annotationOwnerAgentService] != "tenant-a/research-api" {
		t.Fatalf("revision NetworkPolicy = %#v", revisionPolicy)
	}
	deployment.Annotations["example.com/external"] = "preserve-me"
	if err := k8sClient.Update(context.Background(), &deployment); err != nil {
		t.Fatalf("add external annotation: %v", err)
	}
	resourceVersion := deployment.ResourceVersion

	if _, err := dispatcher.EnsureAgentServiceRevisionWorkload(context.Background(), "tenant-a", "research-api", 1, agent); err != nil {
		t.Fatalf("reconcile unchanged revision workload: %v", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: first.Namespace, Name: first.Name}, &deployment); err != nil {
		t.Fatalf("get idempotent revision Deployment: %v", err)
	}
	if deployment.ResourceVersion != resourceVersion || deployment.Annotations["example.com/external"] != "preserve-me" {
		t.Fatalf("idempotent revision Deployment changed: resourceVersion=%q annotations=%#v", deployment.ResourceVersion, deployment.Annotations)
	}

	second, err := dispatcher.EnsureAgentServiceRevisionWorkload(context.Background(), "tenant-a", "research-api", 2, agent)
	if err != nil {
		t.Fatalf("migrate to next revision workload: %v", err)
	}
	if second.Name == first.Name || second.Selector[labelAgentServiceRevision] != "2" {
		t.Fatalf("second revision workload = %#v, want distinct revision 2 workload", second)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: first.Namespace, Name: first.Name}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("first revision Deployment after migration = %v, want retained", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: first.Namespace, Name: first.Name}, &corev1.Service{}); err != nil {
		t.Fatalf("first revision Service after migration = %v, want retained", err)
	}
	if err := dispatcher.CleanupAgentServiceRevisionWorkloads(context.Background(), "tenant-a", "research-api", ""); err != nil {
		t.Fatalf("cleanup revision workloads: %v", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: second.Namespace, Name: second.Name}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("second revision Deployment after cleanup = %v, want NotFound", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: second.Namespace, Name: second.Name}, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("second revision Service after cleanup = %v, want NotFound", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: second.Namespace, Name: second.Name + "-network"}, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("second revision NetworkPolicy after cleanup = %v, want NotFound", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(annotationOnly), annotationOnly); err != nil {
		t.Fatalf("annotation-only Deployment after cleanup = %v, want preserved", err)
	}
}

func TestCleanupRevisionWorkloadsPreservesForeignService(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	name := "agent-svc-research-api-12345678"
	labels := map[string]string{labelAgentServiceWorkload: name}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a", Labels: labels, Annotations: map[string]string{annotationOwnerAgentService: "tenant-a/research-api"}}}
	foreignService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a", Labels: labels, Annotations: map[string]string{annotationOwnerAgentService: "tenant-a/other-api"}}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, foreignService).Build()
	d := NewK8sDispatcher(k8sClient, nil, zap.NewNop(), Config{})
	if err := d.CleanupAgentServiceRevisionWorkloads(context.Background(), "tenant-a", "research-api", ""); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned revision Deployment remained: %v", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(foreignService), &corev1.Service{}); err != nil {
		t.Fatalf("foreign revision Service was deleted: %v", err)
	}
}

func TestEnsureRuntimeResourcesPropagatesClientFailures(t *testing.T) {
	wantErr := errors.New("injected Kubernetes API failure")
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"}}
	ownedDeployment := func() client.Object {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: "agent-researcher", Namespace: "tenant-a",
			Annotations: map[string]string{AnnotationOwnerAgent: "tenant-a/researcher"},
		}}
	}
	ownedService := func() client.Object {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "agent-researcher", Namespace: "tenant-a"}}
	}

	for _, tc := range []struct {
		name        string
		objects     []client.Object
		interceptor interceptor.Funcs
		invoke      func(*K8sDispatcher) error
	}{
		{
			name: "deployment get",
			interceptor: interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return wantErr
			}},
			invoke: func(dispatcher *K8sDispatcher) error {
				return dispatcher.ensureDeployment(context.Background(), "tenant-a", agent, nil)
			},
		},
		{
			name: "deployment create",
			interceptor: interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return wantErr
			}},
			invoke: func(dispatcher *K8sDispatcher) error {
				return dispatcher.ensureDeployment(context.Background(), "tenant-a", agent, nil)
			},
		},
		{
			name:    "deployment update",
			objects: []client.Object{ownedDeployment()},
			interceptor: interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return wantErr
			}},
			invoke: func(dispatcher *K8sDispatcher) error {
				return dispatcher.ensureDeployment(context.Background(), "tenant-a", agent, nil)
			},
		},
		{
			name: "service get",
			interceptor: interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return wantErr
			}},
			invoke: func(dispatcher *K8sDispatcher) error {
				return dispatcher.ensureService(context.Background(), "tenant-a", agent)
			},
		},
		{
			name: "service create",
			interceptor: interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return wantErr
			}},
			invoke: func(dispatcher *K8sDispatcher) error {
				return dispatcher.ensureService(context.Background(), "tenant-a", agent)
			},
		},
		{
			name:    "service update",
			objects: []client.Object{ownedService()},
			interceptor: interceptor.Funcs{Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
				return wantErr
			}},
			invoke: func(dispatcher *K8sDispatcher) error {
				return dispatcher.ensureService(context.Background(), "tenant-a", agent)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add apps scheme: %v", err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("add core scheme: %v", err)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).WithInterceptorFuncs(tc.interceptor).Build()
			dispatcher := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{AgentImage: "native:latest"})
			if err := tc.invoke(dispatcher); !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestWorkloadKeyScopesSharedRuntimeNamespace(t *testing.T) {
	shared := &K8sDispatcher{config: Config{AgentNamespace: "runtimes"}}
	first := shared.workloadKey("tenant-a", "researcher")
	second := shared.workloadKey("tenant-b", "researcher")
	if first == second {
		t.Fatalf("shared runtime keys collide: %q", first)
	}
	if left, right := shared.workloadKey("a-b", "c"), shared.workloadKey("a", "b-c"); left == right {
		t.Fatalf("ambiguous identity tuples collide: %q", left)
	}
	if got := (&K8sDispatcher{}).workloadKey("tenant-a", "researcher"); got != "researcher" {
		t.Fatalf("same-namespace workload key = %q, want researcher", got)
	}

	longNamespace := "tenant-abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop"
	longAgent := "agent-abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop"
	firstLong := shared.workloadKey(longNamespace, longAgent)
	secondLong := shared.workloadKey(longNamespace, longAgent)
	if firstLong != secondLong {
		t.Fatalf("long workload key is not deterministic: %q != %q", firstLong, secondLong)
	}
	if len(deploymentName(firstLong)) > 63 {
		t.Fatalf("deployment name exceeds DNS label limit: %q", deploymentName(firstLong))
	}
}

func TestSharedRuntimeResourcesAreIsolated(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	d := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{
		AgentImage:     "native:latest",
		AgentNamespace: "runtimes",
	})

	for _, namespace := range []string{"tenant-a", "tenant-b"} {
		agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: namespace}}
		if err := d.ensureDeployment(context.Background(), "runtimes", agent, nil); err != nil {
			t.Fatalf("ensure deployment for %s: %v", namespace, err)
		}
		if err := d.ensureService(context.Background(), "runtimes", agent); err != nil {
			t.Fatalf("ensure service for %s: %v", namespace, err)
		}
	}

	var deployments appsv1.DeploymentList
	if err := fakeClient.List(context.Background(), &deployments, client.InNamespace("runtimes")); err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deployments.Items) != 2 {
		t.Fatalf("deployment count = %d, want 2", len(deployments.Items))
	}
	if err := d.CleanupAgentResources(context.Background(), "researcher", "tenant-a"); err != nil {
		t.Fatalf("cleanup tenant-a: %v", err)
	}
	deployments.Items = nil
	if err := fakeClient.List(context.Background(), &deployments, client.InNamespace("runtimes")); err != nil {
		t.Fatalf("list deployments after cleanup: %v", err)
	}
	if len(deployments.Items) != 1 || deployments.Items[0].Labels[labelAgentNamespace] != "tenant-b" {
		t.Fatalf("cleanup affected wrong tenant: %#v", deployments.Items)
	}
}

func TestLegacyMigrationDeletesOnlyMatchingOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	matching := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-researcher", Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: "tenant-a/researcher"},
	}}
	other := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-researcher", Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: "tenant-b/researcher"},
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(matching, other).Build()
	d := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"}}

	if err := d.removeOwnedLegacyResources(context.Background(), "runtimes", agent); err != nil {
		t.Fatalf("removeOwnedLegacyResources() error = %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(matching), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("matching legacy deployment was not deleted: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(other), &corev1.Service{}); err != nil {
		t.Fatalf("other tenant legacy service was deleted: %v", err)
	}
}

func TestRuntimeMigrationKeepsLegacyResourcesUntilReplacementExists(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	legacy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-researcher", Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: "tenant-a/researcher"},
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	d := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentSpec{Runtime: &v1alpha1.AgentRuntimeSpec{Class: "unconfigured"}},
	}

	if err := d.ensureRuntimeResources(context.Background(), "runtimes", agent, nil); err == nil {
		t.Fatal("ensureRuntimeResources() error = nil, want unconfigured adapter error")
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(legacy), &appsv1.Deployment{}); err != nil {
		t.Fatalf("legacy deployment was removed before replacement existed: %v", err)
	}
}

func TestRuntimeMigrationResumesAfterServiceCreateFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"}}
	legacyDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-researcher", Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: "tenant-a/researcher"},
	}}
	legacyService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "agent-researcher", Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: "tenant-a/researcher"},
	}}
	serviceCreateFailures := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(legacyDeployment, legacyService).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Service); ok && serviceCreateFailures == 0 {
					serviceCreateFailures++
					return errors.New("injected service create failure")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	d := NewK8sDispatcher(fakeClient, nil, zap.NewNop(), Config{
		AgentImage:     "native:latest",
		AgentNamespace: "runtimes",
	})

	err := d.ensureRuntimeResources(context.Background(), "runtimes", agent, nil)
	if err == nil || !strings.Contains(err.Error(), "ensure service") {
		t.Fatalf("ensureRuntimeResources() error = %v, want service create failure", err)
	}
	workloadKey := d.workloadKey("tenant-a", "researcher")
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "runtimes", Name: deploymentName(workloadKey)}, &appsv1.Deployment{}); err != nil {
		t.Fatalf("replacement deployment missing after partial materialization: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(legacyDeployment), &appsv1.Deployment{}); err != nil {
		t.Fatalf("legacy deployment removed after service creation failed: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(legacyService), &corev1.Service{}); err != nil {
		t.Fatalf("legacy service removed after service creation failed: %v", err)
	}

	if err := d.ensureRuntimeResources(context.Background(), "runtimes", agent, nil); err != nil {
		t.Fatalf("retry ensureRuntimeResources() error = %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "runtimes", Name: serviceName(workloadKey)}, &corev1.Service{}); err != nil {
		t.Fatalf("replacement service missing after retry: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(legacyDeployment), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy deployment remains after successful retry: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(legacyService), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy service remains after successful retry: %v", err)
	}
	if serviceCreateFailures != 1 {
		t.Fatalf("service create failures = %d, want 1", serviceCreateFailures)
	}
}

func TestSharedRuntimeResourcesRejectWrongOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentImage: "native:latest", AgentNamespace: "runtimes"})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a"}}
	name := deploymentName(d.workloadKey("tenant-a", "researcher"))
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "runtimes",
			Annotations: map[string]string{AnnotationOwnerAgent: "tenant-b/researcher"},
		},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "foreign:v1"}},
		}}},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	d.client = fakeClient

	if err := d.ensureDeployment(context.Background(), "runtimes", agent, nil); err == nil {
		t.Fatal("ensureDeployment() error = nil, want owner mismatch")
	}
	if err := d.CleanupAgentResources(context.Background(), "researcher", "tenant-a"); err == nil {
		t.Fatal("CleanupAgentResources() error = nil, want owner mismatch")
	}
	var remaining appsv1.Deployment
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(foreign), &remaining); err != nil {
		t.Fatalf("foreign deployment was deleted: %v", err)
	}
	if got := remaining.Spec.Template.Spec.Containers[0].Image; got != "foreign:v1" {
		t.Fatalf("foreign deployment image = %q, want foreign:v1", got)
	}
}

func TestCleanupAgentResourcesRejectsForeignService(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      serviceName(d.workloadKey("tenant-a", "researcher")),
		Namespace: "runtimes",
		Annotations: map[string]string{
			AnnotationOwnerAgent: "tenant-b/researcher",
		},
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	d.client = fakeClient

	err := d.CleanupAgentResources(context.Background(), "researcher", "tenant-a")
	if err == nil || !strings.Contains(err.Error(), "service") || !strings.Contains(err.Error(), "tenant-b/researcher") {
		t.Fatalf("CleanupAgentResources() error = %v, want service owner mismatch", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.Service{}); err != nil {
		t.Fatalf("foreign service was deleted: %v", err)
	}
}

func TestCleanupAgentResourcesResumesAfterServiceDeleteFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}

	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	ownerRef := "tenant-a/researcher"
	workloadKey := d.workloadKey("tenant-a", "researcher")
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: deploymentName(workloadKey), Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: ownerRef},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: serviceName(workloadKey), Namespace: "runtimes",
		Annotations: map[string]string{AnnotationOwnerAgent: ownerRef},
	}}
	wantErr := errors.New("service deletion unavailable")
	serviceDeleteFailures := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(deployment, service).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Service); ok && serviceDeleteFailures == 0 {
					serviceDeleteFailures++
					return wantErr
				}
				return underlying.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	d.client = fakeClient

	err := d.CleanupAgentResources(context.Background(), "researcher", "tenant-a")
	if !errors.Is(err, wantErr) {
		t.Fatalf("first cleanup error = %v, want %v", err, wantErr)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deployment remained after successful deletion: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(service), &corev1.Service{}); err != nil {
		t.Fatalf("service missing after failed deletion: %v", err)
	}

	if err := d.CleanupAgentResources(context.Background(), "researcher", "tenant-a"); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(service), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Fatalf("service remained after retry: %v", err)
	}
	if serviceDeleteFailures != 1 {
		t.Fatalf("service delete failures = %d, want 1", serviceDeleteFailures)
	}
}

func TestCleanupAgentResourcesDeletesOwnedPolicyAfterRuntimeAuthDisabled(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	workloadKey := d.workloadKey("tenant-a", "researcher")
	policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: deploymentName(workloadKey) + "-network", Namespace: "runtimes", Annotations: map[string]string{AnnotationOwnerAgent: "tenant-a/researcher"}}}
	d.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	if err := d.CleanupAgentResources(context.Background(), "researcher", "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err := d.client.Get(context.Background(), client.ObjectKeyFromObject(policy), &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned policy remained after cleanup with runtime auth disabled: %v", err)
	}
}

func TestEnsureOwnerAnnotationsBackfillsSharedRuntimeResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	workloadKey := d.workloadKey("tenant-a", "planner")
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: "runtimes",
		Name:      deploymentName(workloadKey),
		Annotations: map[string]string{
			"external.example/revision": "7",
		},
	}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: "runtimes",
		Name:      serviceName(workloadKey),
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment, service).Build()
	d.client = fakeClient

	if err := d.EnsureOwnerAnnotations(context.Background(), "planner", "tenant-a"); err != nil {
		t.Fatalf("ensure owner annotations: %v", err)
	}
	if err := d.EnsureOwnerAnnotations(context.Background(), "planner", "tenant-a"); err != nil {
		t.Fatalf("ensure owner annotations idempotently: %v", err)
	}

	var storedDeployment appsv1.Deployment
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), &storedDeployment); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if got := storedDeployment.Annotations[AnnotationOwnerAgent]; got != "tenant-a/planner" {
		t.Fatalf("deployment owner = %q, want tenant-a/planner", got)
	}
	if got := storedDeployment.Annotations["external.example/revision"]; got != "7" {
		t.Fatalf("unrelated deployment annotation = %q, want 7", got)
	}

	var storedService corev1.Service
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(service), &storedService); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if got := storedService.Annotations[AnnotationOwnerAgent]; got != "tenant-a/planner" {
		t.Fatalf("service owner = %q, want tenant-a/planner", got)
	}
}

func TestEnsureOwnerAnnotationsBackfillsServiceWithoutDeployment(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Namespace: "runtimes",
		Name:      serviceName(d.workloadKey("tenant-a", "planner")),
	}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
	d.client = fakeClient

	if err := d.EnsureOwnerAnnotations(context.Background(), "planner", "tenant-a"); err != nil {
		t.Fatalf("ensure owner annotations: %v", err)
	}

	var storedService corev1.Service
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(service), &storedService); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if got := storedService.Annotations[AnnotationOwnerAgent]; got != "tenant-a/planner" {
		t.Fatalf("service owner = %q, want tenant-a/planner", got)
	}
}

func TestEnsureOwnerAnnotationsRejectsForeignOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}

	d := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{AgentNamespace: "runtimes"})
	workloadKey := d.workloadKey("tenant-a", "planner")
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:   "runtimes",
		Name:        deploymentName(workloadKey),
		Annotations: map[string]string{AnnotationOwnerAgent: "tenant-b/planner"},
	}}
	d.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build()

	err := d.EnsureOwnerAnnotations(context.Background(), "planner", "tenant-a")
	if err == nil {
		t.Fatal("expected foreign owner collision")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "tenant-b/planner") {
		t.Fatalf("error = %q, want conflicting owner context", got)
	}
}

func TestQueryAgentStatus(t *testing.T) {
	for _, test := range []struct {
		name          string
		statusCode    int
		body          string
		runtimeToken  string
		wantAlive     bool
		wantCompleted bool
		wantError     bool
	}{
		{
			name:          "completed response",
			statusCode:    http.StatusOK,
			body:          `{"phase":"completed","tool_calls":3,"tool_calls_detail":[{"name":"calculator","args":{"expression":"17 * 23"},"result":"391","duration_ms":12}],"output":"done","token_usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`,
			runtimeToken:  "controller-token",
			wantAlive:     true,
			wantCompleted: true,
		},
		{
			name:       "failed response",
			statusCode: http.StatusOK,
			body:       `{"phase":"FAILED","error":"runtime failed"}`,
		},
		{
			name:       "non success status",
			statusCode: http.StatusServiceUnavailable,
			body:       `unavailable`,
			wantError:  true,
		},
		{
			name:       "invalid response",
			statusCode: http.StatusOK,
			body:       `{`,
			wantError:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if test.runtimeToken != "" && request.Header.Get("Authorization") != "Bearer "+test.runtimeToken {
					t.Errorf("status authorization = %q, want controller bearer token", request.Header.Get("Authorization"))
				}
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{RuntimeAuthToken: test.runtimeToken})
			status, err := dispatcher.queryAgentStatus(context.Background(), server.URL)
			if (err != nil) != test.wantError {
				t.Fatalf("queryAgentStatus() error = %v, wantError %v", err, test.wantError)
			}
			if test.wantError {
				return
			}
			if status.Alive != test.wantAlive || status.Completed != test.wantCompleted {
				t.Fatalf("queryAgentStatus() = %#v", status)
			}
			if test.wantCompleted {
				if status.Output != "done" || status.ToolCalls != 3 {
					t.Fatalf("completed status = %#v", status)
				}
				if status.TokenUsage == nil || status.TokenUsage.TotalTokens != 12 {
					t.Fatalf("completed token usage = %#v", status.TokenUsage)
				}
				if len(status.ToolCallsDetail) != 1 || status.ToolCallsDetail[0].Name != "calculator" || status.ToolCallsDetail[0].Args != `{"expression":"17 * 23"}` || status.ToolCallsDetail[0].Result != "391" || status.ToolCallsDetail[0].DurationMS != 12 {
					t.Fatalf("completed tool call detail = %#v", status.ToolCallsDetail)
				}
			} else if status.Error != "runtime failed" {
				t.Fatalf("failed status error = %q", status.Error)
			}
		})
	}
}

func TestTerminatePostsAuthenticatedSessionCancel(t *testing.T) {
	var received map[string]string
	dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{RuntimeAuthToken: "controller-token"})
	dispatcher.http = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "http://agent-researcher.tenant-a.svc.cluster.local:8080/cancel" {
			t.Fatalf("cancel request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer controller-token" {
			t.Fatalf("cancel authorization = %q", request.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("decode cancel request: %v", err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accepted":true,"session_id":"quota-session"}`)), Header: make(http.Header)}, nil
	})}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		Status:     v1alpha1.AgentInstanceStatus{SessionID: "quota-session"},
	}

	if err := dispatcher.Terminate(context.Background(), instance, "token budget exceeded"); err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}
	if received["session_id"] != "quota-session" || received["reason"] != "token budget exceeded" {
		t.Fatalf("cancel payload = %#v", received)
	}
}

func TestTerminateRejectsRuntimeCancelFailure(t *testing.T) {
	dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{})
	dispatcher.http = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
	})}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		Status:     v1alpha1.AgentInstanceStatus{SessionID: "quota-session"},
	}

	if err := dispatcher.Terminate(context.Background(), instance, "token budget exceeded"); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Terminate() error = %v, want runtime failure", err)
	}
}

func TestTerminateRejectsUnacknowledgedRuntimeCancel(t *testing.T) {
	dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{})
	dispatcher.http = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accepted":false,"session_id":"quota-session"}`)), Header: make(http.Header)}, nil
	})}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		Status:     v1alpha1.AgentInstanceStatus{SessionID: "quota-session"},
	}
	if err := dispatcher.Terminate(context.Background(), instance, "token budget exceeded"); err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("Terminate() error = %v, want unacknowledged cancellation", err)
	}
}

func TestTerminateRejectsCancellationAcknowledgmentForDifferentSession(t *testing.T) {
	dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{})
	dispatcher.http = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"accepted":true,"session_id":"replacement-session"}`)), Header: make(http.Header)}, nil
	})}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		Status:     v1alpha1.AgentInstanceStatus{SessionID: "quota-session"},
	}
	if err := dispatcher.Terminate(context.Background(), instance, "token budget exceeded"); err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("Terminate() error = %v, want mismatched-session cancellation", err)
	}
}

func TestTerminateCancelsQueuedInvocationBeforeRuntimeAcceptance(t *testing.T) {
	requestReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/cancel" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accepted":true,"session_id":"queued-session"}`))
			return
		}
		select {
		case requestReceived <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer server.Close()
	dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{})
	dispatcher.invokeHTTP = server.Client()
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatcher.invokeEndpoint(server.URL, "researcher", "instance-1", "queued-session", "hello", "", "", "", "", "", "")
	}()
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("queued invocation did not reach runtime")
	}
	instance := &v1alpha1.AgentInstance{
		Spec:   v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		Status: v1alpha1.AgentInstanceStatus{SessionID: "queued-session"},
	}
	dispatcher.http = server.Client()
	dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		request.URL.Scheme = "http"
		request.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return http.DefaultTransport.RoundTrip(request)
	})
	if err := dispatcher.Terminate(context.Background(), instance, "session timeout exceeded"); err != nil {
		t.Fatalf("Terminate() error = %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued invocation did not stop after termination")
	}
}

func TestStatusReflectsDeploymentAndRuntimeState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		Status:     v1alpha1.AgentInstanceStatus{SessionID: "session with spaces"},
	}

	t.Run("missing deployment is not alive", func(t *testing.T) {
		dispatcher := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, zap.NewNop(), Config{})
		status, err := dispatcher.Status(context.Background(), instance)
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if status.Alive {
			t.Fatalf("Status() = %#v, want not alive", status)
		}
	})

	t.Run("unavailable deployment is alive but not complete", func(t *testing.T) {
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: deploymentName("researcher"), Namespace: "tenant-a",
		}}
		dispatcher := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build(), nil, zap.NewNop(), Config{})
		status, err := dispatcher.Status(context.Background(), instance)
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if !status.Alive || status.Completed || !status.Pending {
			t.Fatalf("Status() = %#v, want alive and incomplete", status)
		}
	})

	t.Run("available deployment returns session status", func(t *testing.T) {
		config := Config{AgentNamespace: "runtimes"}
		workloadKey := (&K8sDispatcher{config: config}).workloadKey(instance.Namespace, instance.Spec.AgentRef.Name)
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: deploymentName(workloadKey), Namespace: "runtimes"},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		}
		dispatcher := NewK8sDispatcher(
			fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build(), nil, zap.NewNop(),
			config,
		)
		requestCount := 0
		dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestCount++
			if got := request.URL.Query().Get("session_id"); got != instance.Status.SessionID {
				t.Fatalf("session_id = %q, want %q", got, instance.Status.SessionID)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       http.NoBody,
				Header:     make(http.Header),
				Request:    request,
			}, nil
		})
		status, err := dispatcher.Status(context.Background(), instance)
		if err != nil {
			t.Fatalf("Status() error = %v", err)
		}
		if !status.Alive || !status.Pending {
			t.Fatalf("Status() = %#v, want endpoint failure fallback to alive", status)
		}
		if requestCount != 1 {
			t.Fatalf("status endpoint requests = %d, want 1", requestCount)
		}
	})
}

func TestStatusDetectsSessionLostAfterRuntimeRestart(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName("researcher"), Namespace: "tenant-a"},
		Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
	}
	dispatcher := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build(), nil, zap.NewNop(), Config{})
	dispatcher.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"phase":"Running","session_id":"old-session","uptime_seconds":60}`)), Header: make(http.Header), Request: request}, nil
	})
	started := time.Now().Add(-2 * time.Hour)
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "old-instance", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}, SessionContext: map[string]string{"input": "original request"}},
		Status:     v1alpha1.AgentInstanceStatus{SessionID: "old-session", StartTime: &started},
	}
	status, err := dispatcher.Status(context.Background(), instance)
	if err != nil || status.Alive || status.Error == "" {
		t.Fatalf("lost session Status() = %#v, %v; want inactive with reason", status, err)
	}
	started = time.Now().Add(-30 * time.Second)
	status, err = dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Alive {
		t.Fatalf("recent session Status() = %#v, %v; want alive", status, err)
	}
	started = time.Now().Add(-2 * time.Hour)
	instance.Spec.SessionContext = nil
	status, err = dispatcher.Status(context.Background(), instance)
	if err != nil || !status.Alive {
		t.Fatalf("idle warm replica Status() = %#v, %v; no invocation exists to lose", status, err)
	}
}

func TestDeploymentRolloutComplete(t *testing.T) {
	replicas := int32(2)
	complete := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 4,
			Replicas:           2,
			UpdatedReplicas:    2,
			AvailableReplicas:  2,
		},
	}

	for _, test := range []struct {
		name   string
		mutate func(*appsv1.Deployment)
		want   bool
	}{
		{name: "complete", mutate: func(*appsv1.Deployment) {}, want: true},
		{name: "generation not observed", mutate: func(deployment *appsv1.Deployment) { deployment.Status.ObservedGeneration = 3 }},
		{name: "replicas not updated", mutate: func(deployment *appsv1.Deployment) { deployment.Status.UpdatedReplicas = 1 }},
		{name: "replicas not available", mutate: func(deployment *appsv1.Deployment) { deployment.Status.AvailableReplicas = 1 }},
		{name: "old replicas remain", mutate: func(deployment *appsv1.Deployment) { deployment.Status.Replicas = 3 }},
		{name: "implicit single replica", mutate: func(deployment *appsv1.Deployment) {
			deployment.Spec.Replicas = nil
			deployment.Status.Replicas = 1
			deployment.Status.UpdatedReplicas = 1
			deployment.Status.AvailableReplicas = 1
		}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			deployment := complete.DeepCopy()
			test.mutate(deployment)
			if got := deploymentRolloutComplete(deployment); got != test.want {
				t.Fatalf("deploymentRolloutComplete() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestInvokeWhenReadyPostsSessionInput(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       deploymentName("tenant-a-researcher"),
			Namespace:  "runtimes",
			Generation: 1,
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
	activeInstance := v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-1", Namespace: "runtimes"},
		Status:     v1alpha1.AgentInstanceStatus{Phase: v1alpha1.PhaseRunning, SessionID: "session-1"},
	}
	dispatcher := NewK8sDispatcher(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build(), &stubStore{data: map[string][]byte{
			"AgentInstance/runtimes/instance-1": resourceBytes(t, activeInstance),
		}}, zap.NewNop(), Config{},
	)
	requestCount := 0
	dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if request.Method != http.MethodPost {
			t.Fatalf("request method = %q, want POST", request.Method)
		}
		if got, want := request.URL.String(), "http://agent-tenant-a-researcher.runtimes.svc.cluster.local:8080/invoke"; got != want {
			t.Fatalf("request URL = %q, want %q", got, want)
		}
		if got := request.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer runtime-token" {
			t.Fatalf("Authorization = %q, want runtime bearer token", got)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if body["message"] != "investigate" || body["session_id"] != "session-1" || body["conversation_id"] != "conversation-7" || body["spiffe_id"] != "spiffe://kognition.dev/workspace/tenant-a/agent/researcher" || body["memory_capability"] != "memory-capability" {
			t.Fatalf("request body = %#v", body)
		}
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})

	dispatcher.config.RuntimeAuthToken = "runtime-token"
	dispatcher.invokeWhenReady("runtimes", "tenant-a-researcher", "researcher", "instance-1", "session-1", "investigate", "", "", "spiffe://kognition.dev/workspace/tenant-a/agent/researcher", "memory-capability", "", "", "", "conversation-7")

	if requestCount != 1 {
		t.Fatalf("invoke endpoint requests = %d, want 1", requestCount)
	}
}

func TestInvokeWhenReadyRetriesBusyRuntime(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploymentName("researcher"), Namespace: "tenant-a", Generation: 1},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Replicas:           1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}
	dispatcher := NewK8sDispatcher(
		fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build(), nil, zap.NewNop(), Config{},
	)
	requestCount := 0
	dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount++
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body on attempt %d: %v", requestCount, err)
		}
		if body["message"] != "investigate" || body["session_id"] != "stable-session" {
			t.Fatalf("request body on attempt %d = %#v", requestCount, body)
		}
		status := http.StatusAccepted
		if requestCount == 1 {
			status = http.StatusConflict
		}
		return &http.Response{
			StatusCode: status,
			Body:       http.NoBody,
			Header:     make(http.Header),
			Request:    request,
		}, nil
	})

	dispatcher.invokeWhenReady("tenant-a", "researcher", "researcher", "instance-1", "stable-session", "investigate", "", "")

	if requestCount != 2 {
		t.Fatalf("invoke endpoint requests = %d, want 2", requestCount)
	}
}

func TestInvokeWhenReadyDoesNotRetryRuntimeFailures(t *testing.T) {
	for _, test := range []struct {
		name      string
		transport roundTripFunc
	}{
		{
			name: "transport error",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			},
		},
		{
			name: "server error",
			transport: func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Body:       io.NopCloser(strings.NewReader("runtime unavailable")),
					Header:     make(http.Header),
					Request:    request,
				}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatalf("add apps scheme: %v", err)
			}
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: deploymentName("researcher"), Namespace: "tenant-a", Generation: 1},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 1,
					Replicas:           1,
					UpdatedReplicas:    1,
					AvailableReplicas:  1,
				},
			}
			dispatcher := NewK8sDispatcher(
				fake.NewClientBuilder().WithScheme(scheme).WithObjects(deployment).Build(), nil, zap.NewNop(), Config{},
			)
			requestCount := 0
			dispatcher.invokeHTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				requestCount++
				return test.transport(request)
			})

			dispatcher.invokeWhenReady("tenant-a", "researcher", "researcher", "instance-1", "session-1", "investigate", "", "")

			if requestCount != 1 {
				t.Fatalf("invoke endpoint requests = %d, want 1", requestCount)
			}
		})
	}
}

// modelOpts customises a Model fixture.
type modelOpts struct {
	provider   string
	apiVersion string
	credName   string
	modelID    string
	endpoint   string
}

func modelBytes(t *testing.T, name string, opts modelOpts) []byte {
	t.Helper()
	if opts.modelID == "" {
		opts.modelID = "text-embedding-3-small"
	}
	if opts.endpoint == "" {
		opts.endpoint = "https://example.openai.azure.com/"
	}
	m := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:  opts.provider,
			ClassName: "azure-openai-embedding",
			ModelID:   opts.modelID,
			Endpoint:  opts.endpoint,
		},
	}
	if opts.apiVersion != "" {
		m.Spec.Parameters = map[string]string{"apiVersion": opts.apiVersion}
	}
	if opts.credName != "" {
		m.Spec.CredentialRef = &v1alpha1.LocalObjectReference{Name: opts.credName}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal model: %v", err)
	}
	return b
}

func secretRefBytes(t *testing.T, name string, values map[string]string) []byte {
	t.Helper()
	rawValues, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal secret values: %v", err)
	}
	sr := v1alpha1.SecretRef{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				"kognition.dev/internal-secret-data": string(rawValues),
			},
		},
		Spec: v1alpha1.SecretRefSpec{BackendType: "internal"},
	}
	b, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("marshal secretRef: %v", err)
	}
	return b
}

func resourceBytes(t *testing.T, resource any) []byte {
	t.Helper()
	b, err := json.Marshal(resource)
	if err != nil {
		t.Fatalf("marshal resource: %v", err)
	}
	return b
}

func newKB(embModelRefName string) *v1alpha1.KnowledgeBase {
	kb := &v1alpha1.KnowledgeBase{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBase"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "kognition-docs", Namespace: "default"},
		Spec: v1alpha1.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "pgvector-docs",
		},
	}
	if embModelRefName != "" {
		kb.Spec.IndexConfig = &v1alpha1.KBIndexConfig{
			EmbeddingModelRef: &v1alpha1.LocalObjectReference{Name: embModelRefName},
		}
	}
	return kb
}

func newKBC(embModelRefName string) *v1alpha1.KnowledgeBaseClass {
	kbc := &v1alpha1.KnowledgeBaseClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBaseClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "pgvector-docs"},
		Spec: v1alpha1.KnowledgeBaseClassSpec{
			Provisioner: "pgvector",
		},
	}
	if embModelRefName != "" {
		kbc.Spec.EmbeddingModelRef = &v1alpha1.LocalObjectReference{Name: embModelRefName}
	}
	return kbc
}

// --------------------------------------------------------------------------- //
// Agent tool and knowledge-base resolution
// --------------------------------------------------------------------------- //

func TestResolveAgentTools_UsesSchemaAndInputSchemaFallback(t *testing.T) {
	primarySchema := map[string]any{"type": "object", "required": []any{"query"}}
	legacySchema := map[string]any{"type": "string"}
	st := &stubStore{data: map[string][]byte{
		"Tool/default/search": resourceBytes(t, v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "search", Namespace: "default"},
			Spec: v1alpha1.ToolSpec{
				Protocol:    v1alpha1.ToolProtocolOpenAPI,
				ClassName:   "http",
				Description: "Search documents",
				Endpoint:    "https://tools.example/search",
				Schema:      primarySchema,
				InputSchema: legacySchema,
			},
		}),
		"Tool/default/legacy": resourceBytes(t, v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "legacy", Namespace: "default"},
			Spec: v1alpha1.ToolSpec{
				Protocol:    v1alpha1.ToolProtocolMCP,
				InputSchema: legacySchema,
			},
		}),
		"Tool/default/null": []byte("null"),
	}}
	d := newDispatcher(st)

	got := d.resolveAgentTools(context.Background(), "default", []v1alpha1.AgentToolRef{
		{Ref: v1alpha1.LocalObjectReference{Name: "search"}},
		{Ref: v1alpha1.LocalObjectReference{Name: "missing"}},
		{Ref: v1alpha1.LocalObjectReference{Name: "null"}},
		{Ref: v1alpha1.LocalObjectReference{Name: "legacy"}},
	}, true)

	if len(got) != 2 {
		t.Fatalf("resolved tools = %#v, want 2 entries", got)
	}
	if got[0].Name != "search" || got[0].Protocol != "OpenAPI" || got[0].ClassName != "http" {
		t.Errorf("primary tool metadata = %#v", got[0])
	}
	if got[0].Description != "Search documents" || got[0].Endpoint != "https://tools.example/search" {
		t.Errorf("primary tool runtime fields = %#v", got[0])
	}
	if got[0].InputSchema["type"] != "object" {
		t.Errorf("primary schema = %#v, want Schema to take precedence", got[0].InputSchema)
	}
	if got[1].Name != "legacy" || got[1].InputSchema["type"] != "string" {
		t.Errorf("legacy tool = %#v, want InputSchema fallback", got[1])
	}
}

func TestResolveAgentKBs_BuildsRuntimeContract(t *testing.T) {
	topK := int32(7)
	claim := v1alpha1.KnowledgeBaseClaim{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "default", Generation: 1},
		Spec: v1alpha1.KnowledgeBaseClaimSpec{
			KnowledgeBaseName: "stale-spec-name",
			Retrieval: &v1alpha1.KBRetrievalOverride{
				TopK:     &topK,
				MinScore: "0.42",
				Filter:   map[string]string{"language": "en"},
			},
		},
		Status: v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "bound-docs", ObservedGeneration: 1},
	}
	kb := v1alpha1.KnowledgeBase{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bound-docs", Namespace: "default"},
		Spec: v1alpha1.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "pgvector-docs",
			Sources: []v1alpha1.KBSource{
				{Type: "inline", Data: "Kognition documentation"},
				{Type: "url", URI: "https://example.test/docs"},
			},
		},
	}
	kbc := v1alpha1.KnowledgeBaseClass{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "pgvector-docs"},
		Spec: v1alpha1.KnowledgeBaseClassSpec{
			EmbeddingModelRef: &v1alpha1.LocalObjectReference{Name: "embedding"},
			VectorStore: &v1alpha1.KBVectorStore{
				Type: "pgvector",
				Pgvector: &v1alpha1.KBPgvectorStore{
					ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: "pg-creds"},
				},
			},
		},
	}
	st := &stubStore{data: map[string][]byte{
		"KnowledgeBaseClaim/default/docs":   resourceBytes(t, claim),
		"KnowledgeBase/default/bound-docs":  resourceBytes(t, kb),
		"KnowledgeBaseClass//pgvector-docs": resourceBytes(t, kbc),
		"SecretRef/default/pg-creds":        secretRefBytes(t, "pg-creds", map[string]string{"connectionString": "postgres://kb"}),
		"Model/default/embedding":           modelBytes(t, "embedding", modelOpts{provider: "foundry", apiVersion: "2024-12-01-preview", credName: "embedding-creds"}),
		"SecretRef/default/embedding-creds": secretRefBytes(t, "embedding-creds", map[string]string{"apiKey": "embedding-key"}),
	}}
	d := newDispatcher(st)

	got, err := d.resolveAgentKBs(context.Background(), "default", []v1alpha1.KnowledgeBaseClaimRef{
		{Name: "missing"},
		{Name: "docs"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 {
		t.Fatalf("resolved knowledge bases = %#v, want one entry", got)
	}
	record := got[0]
	if record.Name != "bound-docs" || record.Namespace != "default" || record.ClassName != "pgvector-docs" {
		t.Errorf("knowledge base identity = %#v", record)
	}
	if len(record.Sources) != 2 || record.Sources[0].Data != "Kognition documentation" || record.Sources[1].URI != "https://example.test/docs" {
		t.Errorf("sources = %#v", record.Sources)
	}
	if record.VectorStore["provider"] != "pgvector" || record.VectorStore["dsn"] != "postgres://kb" {
		t.Errorf("vector store = %#v", record.VectorStore)
	}
	if record.VectorStore["table"] != "kognition_kb_chunks" || record.VectorStore["dimension"] != 1536 {
		t.Errorf("vector store defaults = %#v", record.VectorStore)
	}
	if record.Embedding["provider"] != "azure_openai" || record.Embedding["apiKey"] != "embedding-key" {
		t.Errorf("embedding = %#v", record.Embedding)
	}
	if record.Retrieval["topK"] != 7 || record.Retrieval["minScore"] != 0.42 {
		t.Errorf("retrieval = %#v", record.Retrieval)
	}
	if filter, ok := record.Retrieval["filter"].(map[string]string); !ok || filter["language"] != "en" {
		t.Errorf("retrieval filter = %#v", record.Retrieval["filter"])
	}
}

func TestResolveAgentKBs_RejectsClaimsNotBoundAtCurrentGeneration(t *testing.T) {
	tests := []struct {
		name   string
		status v1alpha1.KnowledgeBaseClaimStatus
	}{
		{name: "pending", status: v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhasePending, ObservedGeneration: 2}},
		{name: "lost", status: v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseLost, BoundKnowledgeBase: "docs", ObservedGeneration: 2}},
		{name: "stale generation", status: v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "docs", ObservedGeneration: 1}},
		{name: "empty binding", status: v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, ObservedGeneration: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := v1alpha1.KnowledgeBaseClaim{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "default", Generation: 2},
				Status:     test.status,
			}
			d := newDispatcher(&stubStore{data: map[string][]byte{
				"KnowledgeBaseClaim/default/docs": resourceBytes(t, claim),
			}})
			got, err := d.resolveAgentKBs(context.Background(), "default", []v1alpha1.KnowledgeBaseClaimRef{{Name: "docs"}})
			if err == nil || !strings.Contains(err.Error(), "not bound at its current generation") || len(got) != 0 {
				t.Fatalf("resolveAgentKBs() = %#v, %v; want current-binding rejection", got, err)
			}
		})
	}
}

func TestResolveAgentKBs_GracefullySkipsUnavailableBindings(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"KnowledgeBaseClaim/default/null": []byte("null"),
		"KnowledgeBaseClaim/default/unbound": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "unbound", Namespace: "default"},
		}),
		"KnowledgeBaseClaim/default/deleted-kb": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "deleted-kb", Namespace: "default", Generation: 1},
			Spec:       v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "missing"},
			Status:     v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "missing", ObservedGeneration: 1},
		}),
		"KnowledgeBaseClaim/default/null-kb": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "null-kb", Namespace: "default", Generation: 1},
			Spec:       v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "null"},
			Status:     v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "null", ObservedGeneration: 1},
		}),
		"KnowledgeBase/default/null": []byte("null"),
		"KnowledgeBaseClaim/default/fallback": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "fallback", Namespace: "default", Generation: 1},
			Spec:       v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "fallback-kb"},
			Status:     v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "fallback-kb", ObservedGeneration: 1},
		}),
		"KnowledgeBase/default/fallback-kb": resourceBytes(t, v1alpha1.KnowledgeBase{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "fallback-kb", Namespace: "default"},
			Spec:       v1alpha1.KnowledgeBaseSpec{KnowledgeBaseClassName: "missing-class"},
		}),
	}}
	d := newDispatcher(st)

	got, err := d.resolveAgentKBs(context.Background(), "default", []v1alpha1.KnowledgeBaseClaimRef{
		{Name: "null"},
		{Name: "deleted-kb"},
		{Name: "null-kb"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 0 {
		t.Fatalf("resolved knowledge bases = %#v, want none", got)
	}
	got, err = d.resolveAgentKBs(context.Background(), "default", []v1alpha1.KnowledgeBaseClaimRef{{Name: "unbound"}})
	if err == nil || len(got) != 0 {
		t.Fatalf("unbound claim must fail closed, got %#v, %v", got, err)
	}
	got, err = d.resolveAgentKBs(context.Background(), "default", []v1alpha1.KnowledgeBaseClaimRef{{Name: "fallback"}})
	if err == nil || len(got) != 0 {
		t.Fatalf("missing class must fail closed, got %#v, %v", got, err)
	}
}

func TestKnowledgeBackendReferenceBlocksRuntimeWithoutProvider(t *testing.T) {
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "tenant-a", UID: "agent-uid"},
		Spec:       v1alpha1.AgentSpec{KnowledgeBaseClaims: []v1alpha1.KnowledgeBaseClaimRef{{Name: "docs"}}},
	}
	storage := &stubStore{data: map[string][]byte{
		"Agent/tenant-a/researcher": resourceBytes(t, agent),
		"AgentIdentity/tenant-a/agent-identity-researcher": resourceBytes(t, v1alpha1.AgentIdentity{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-identity-researcher", Namespace: "tenant-a", UID: "identity-uid"},
			Spec:       v1alpha1.AgentIdentitySpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
		}),
		"KnowledgeBaseClaim/tenant-a/docs": resourceBytes(t, v1alpha1.KnowledgeBaseClaim{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "tenant-a", Generation: 1},
			Spec:       v1alpha1.KnowledgeBaseClaimSpec{KnowledgeBaseName: "docs"},
			Status:     v1alpha1.KnowledgeBaseClaimStatus{Phase: v1alpha1.PhaseBound, BoundKnowledgeBase: "docs", ObservedGeneration: 1},
		}),
		"KnowledgeBase/tenant-a/docs": resourceBytes(t, v1alpha1.KnowledgeBase{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "docs", Namespace: "tenant-a"},
			Spec:       v1alpha1.KnowledgeBaseSpec{KnowledgeBaseClassName: "provider", Sources: []v1alpha1.KBSource{{Type: "inline", Data: "must not leak through inline fallback"}}},
		}),
		"KnowledgeBaseClass//provider": resourceBytes(t, v1alpha1.KnowledgeBaseClass{
			Spec: v1alpha1.KnowledgeBaseClassSpec{BackendRef: &v1alpha1.KnowledgeBackendReference{APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared"}},
		}),
	}}
	dispatcher := newDispatcher(storage)
	if err := dispatcher.EnsureAgentRuntime(context.Background(), agent); err == nil || !strings.Contains(err.Error(), "Knowledge Gateway") {
		t.Fatalf("runtime creation must reject unsupported backend: %v", err)
	}
	session, err := dispatcher.Dispatch(context.Background(), &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant-a"},
		Spec:       v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
	})
	if err == nil || !strings.Contains(err.Error(), "Knowledge Gateway") || session != "" {
		t.Fatalf("dispatch must reject unsupported backend without a session: %q, %v", session, err)
	}

	issuer, err := runtimeauth.NewKnowledgeSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.config.KnowledgeGatewayURL = "http://knowledge-gateway:8095"
	dispatcher.config.KnowledgeSessionCapabilityIssuer = issuer
	records, err := dispatcher.resolveAgentKBs(context.Background(), "tenant-a", agent.Spec.KnowledgeBaseClaims)
	if err != nil || len(records) != 1 || !records[0].ProviderBacked || len(records[0].Sources) != 0 || records[0].VectorStore != nil {
		t.Fatalf("provider-backed runtime record = %#v, %v", records, err)
	}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance", Namespace: "tenant-a", UID: "instance-uid"},
		Spec:       v1alpha1.AgentInstanceSpec{SessionContext: map[string]string{intctrl.KnowledgeCapabilityIdentityNameContextKey: "agent-identity-researcher"}},
	}
	capability, err := dispatcher.issueKnowledgeSessionCapability(context.Background(), "tenant-a", agent, instance, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(capability)
	if err != nil || !reflect.DeepEqual(claims.KnowledgeBases, []string{"docs"}) || claims.AgentUID != "agent-uid" || claims.AgentIdentityName != "agent-identity-researcher" || claims.AgentIdentityUID != "identity-uid" {
		t.Fatalf("knowledge capability claims = %#v, %v", claims, err)
	}
}

func TestBuildKBVectorStoreBlock_RejectsUnusableConfiguration(t *testing.T) {
	d := newDispatcher(&stubStore{data: map[string][]byte{}})
	kb := newKB("")
	cases := []struct {
		name string
		kbc  *v1alpha1.KnowledgeBaseClass
	}{
		{name: "no vector store", kbc: newKBC("")},
		{name: "unsupported provider", kbc: &v1alpha1.KnowledgeBaseClass{Spec: v1alpha1.KnowledgeBaseClassSpec{VectorStore: &v1alpha1.KBVectorStore{Type: "other"}}}},
		{name: "missing pgvector block", kbc: &v1alpha1.KnowledgeBaseClass{Spec: v1alpha1.KnowledgeBaseClassSpec{VectorStore: &v1alpha1.KBVectorStore{Type: "pgvector"}}}},
		{name: "missing secret name", kbc: &v1alpha1.KnowledgeBaseClass{Spec: v1alpha1.KnowledgeBaseClassSpec{VectorStore: &v1alpha1.KBVectorStore{Type: "pgvector", Pgvector: &v1alpha1.KBPgvectorStore{}}}}},
		{name: "missing secret", kbc: &v1alpha1.KnowledgeBaseClass{Spec: v1alpha1.KnowledgeBaseClassSpec{VectorStore: &v1alpha1.KBVectorStore{Type: "pgvector", Pgvector: &v1alpha1.KBPgvectorStore{ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: "missing"}}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.buildKBVectorStoreBlock(context.Background(), kb, tc.kbc); got != nil {
				t.Fatalf("vector store block = %#v, want nil", got)
			}
		})
	}
}

func TestBuildKBRetrievalBlock_OmitsEmptyAndInvalidValues(t *testing.T) {
	if got := buildKBRetrievalBlock(&v1alpha1.KBRetrievalOverride{MinScore: "not-a-number"}); got != nil {
		t.Fatalf("invalid-only retrieval block = %#v, want nil", got)
	}
	if got := buildKBRetrievalBlock(&v1alpha1.KBRetrievalOverride{}); got != nil {
		t.Fatalf("empty retrieval block = %#v, want nil", got)
	}
}

// --------------------------------------------------------------------------- //
// buildKBEmbeddingBlock — provider mapping
// --------------------------------------------------------------------------- //

func TestBuildKBEmbeddingBlock_ProviderMapping(t *testing.T) {
	cases := []struct {
		name           string
		provider       string
		apiVersion     string
		wantProvider   string
		wantAPIVersion any // nil = key must be absent
		expectNilBlock bool
	}{
		// Regression: "foundry" must be accepted (previously fell into default
		// arm and returned nil, breaking KB retrieval for Foundry embedders).
		{"foundry maps to azure_openai", "foundry", "2024-02-01", "azure_openai", "2024-02-01", false},
		{"azure-openai maps to azure_openai", "azure-openai", "2024-02-01", "azure_openai", "2024-02-01", false},
		{"azure-openai without apiVersion omits key", "azure-openai", "", "azure_openai", nil, false},
		{"foundry without apiVersion omits key", "foundry", "", "azure_openai", nil, false},
		{"openai maps to openai", "openai", "", "openai", nil, false},
		{"openai-compatible maps to openai", "openai-compatible", "", "openai", nil, false},
		{"empty provider defaults to openai", "", "", "openai", nil, false},
		{"openai ignores apiVersion", "openai", "2024-02-01", "openai", nil, false},
		{"unknown provider returns nil", "anthropic", "", "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stubStore{data: map[string][]byte{
				"Model/default/emb-model": modelBytes(t, "emb-model", modelOpts{
					provider:   tc.provider,
					apiVersion: tc.apiVersion,
				}),
			}}
			d := newDispatcher(st)
			block := d.buildKBEmbeddingBlock(context.Background(), newKB("emb-model"), newKBC(""))

			if tc.expectNilBlock {
				if block != nil {
					t.Fatalf("expected nil block for provider %q, got %#v", tc.provider, block)
				}
				return
			}
			if block == nil {
				t.Fatalf("expected block for provider %q, got nil", tc.provider)
			}
			if got := block["provider"]; got != tc.wantProvider {
				t.Errorf("provider = %v, want %v", got, tc.wantProvider)
			}
			if tc.wantAPIVersion == nil {
				if _, ok := block["apiVersion"]; ok {
					t.Errorf("apiVersion should be omitted, got %v", block["apiVersion"])
				}
			} else if got := block["apiVersion"]; got != tc.wantAPIVersion {
				t.Errorf("apiVersion = %v, want %v", got, tc.wantAPIVersion)
			}
		})
	}
}

func TestBuildKBEmbeddingBlock_IncludesModelIDAndEndpoint(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Model/default/emb-model": modelBytes(t, "emb-model", modelOpts{
			provider: "foundry",
			modelID:  "text-embedding-3-large",
			endpoint: "https://foundry.example/",
		}),
	}}
	d := newDispatcher(st)
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("emb-model"), newKBC(""))

	if block == nil {
		t.Fatal("expected non-nil block")
	}
	if got := block["model"]; got != "text-embedding-3-large" {
		t.Errorf("model = %v, want text-embedding-3-large", got)
	}
	if got := block["baseUrl"]; got != "https://foundry.example/" {
		t.Errorf("baseUrl = %v, want https://foundry.example/", got)
	}
}

// --------------------------------------------------------------------------- //
// buildKBEmbeddingBlock — ref resolution
// --------------------------------------------------------------------------- //

func TestBuildKBEmbeddingBlock_ReturnsNilWhenNoModelRef(t *testing.T) {
	d := newDispatcher(&stubStore{data: map[string][]byte{}})
	block := d.buildKBEmbeddingBlock(context.Background(), newKB(""), newKBC(""))
	if block != nil {
		t.Fatalf("expected nil block when no embeddingModelRef is set, got %#v", block)
	}
}

func TestBuildKBEmbeddingBlock_FallsBackToClassEmbeddingRef(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Model/default/class-emb": modelBytes(t, "class-emb", modelOpts{provider: "foundry"}),
	}}
	d := newDispatcher(st)
	// KB has no IndexConfig; class provides the ref.
	block := d.buildKBEmbeddingBlock(context.Background(), newKB(""), newKBC("class-emb"))
	if block == nil {
		t.Fatal("expected block from class-level EmbeddingModelRef")
	}
	if block["provider"] != "azure_openai" {
		t.Errorf("provider = %v, want azure_openai", block["provider"])
	}
}

func TestBuildKBEmbeddingBlock_KBIndexConfigOverridesClass(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Model/default/kb-emb":    modelBytes(t, "kb-emb", modelOpts{provider: "openai"}),
		"Model/default/class-emb": modelBytes(t, "class-emb", modelOpts{provider: "foundry"}),
	}}
	d := newDispatcher(st)
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("kb-emb"), newKBC("class-emb"))
	if block == nil {
		t.Fatal("expected non-nil block")
	}
	if block["provider"] != "openai" {
		t.Errorf("provider = %v, want openai (kb override)", block["provider"])
	}
}

func TestBuildKBEmbeddingBlock_ReturnsNilWhenModelMissing(t *testing.T) {
	d := newDispatcher(&stubStore{data: map[string][]byte{}})
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("missing-model"), newKBC(""))
	if block != nil {
		t.Fatalf("expected nil block when Model does not exist, got %#v", block)
	}
}

// --------------------------------------------------------------------------- //
// buildKBEmbeddingBlock — credential resolution
// --------------------------------------------------------------------------- //

func TestBuildKBEmbeddingBlock_ResolvesAPIKeyFromCredentialRef(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Model/default/emb-model": modelBytes(t, "emb-model", modelOpts{
			provider: "foundry",
			credName: "emb-creds",
		}),
		"SecretRef/default/emb-creds": secretRefBytes(t, "emb-creds", map[string]string{
			"apiKey": "sk-test-123",
		}),
	}}
	d := newDispatcher(st)
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("emb-model"), newKBC(""))
	if block == nil {
		t.Fatal("expected non-nil block")
	}
	if got := block["apiKey"]; got != "sk-test-123" {
		t.Errorf("apiKey = %v, want sk-test-123", got)
	}
}

func TestBuildKBEmbeddingBlock_AcceptsAzureIdentityCredentialRefWithoutAPIKey(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Model/default/emb-model": modelBytes(t, "emb-model", modelOpts{
			provider: "foundry",
			credName: "foundry-creds",
		}),
		"SecretRef/default/foundry-creds": secretRefBytes(t, "foundry-creds", map[string]string{
			"type":           "azure-identity",
			"credentialType": "client-secret",
			"tenantId":       "tenant",
			"clientId":       "client",
			"clientSecret":   "secret",
		}),
	}}
	d := newDispatcher(st)
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("emb-model"), newKBC(""))
	if block == nil {
		t.Fatal("expected non-nil block")
	}
	if _, ok := block["apiKey"]; ok {
		t.Errorf("apiKey should be absent for azure-identity credentials, got %v", block["apiKey"])
	}
	if block["provider"] != "azure_openai" {
		t.Errorf("provider = %v, want azure_openai", block["provider"])
	}
}

func TestResolveCredentialEnv_AzureModes(t *testing.T) {
	model := &v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "m", Namespace: "default"},
		Spec:       v1alpha1.ModelSpec{CredentialRef: &v1alpha1.LocalObjectReference{Name: "creds"}},
	}
	cases := []struct {
		name     string
		values   map[string]string
		wantType string
		wantEnv  map[string]string
		absent   []string
	}{
		{
			name:     "client-secret",
			values:   map[string]string{"type": "azure-identity", "credentialType": "client-secret", "tenantId": "t", "clientId": "c", "clientSecret": "s"},
			wantType: "client-secret",
			wantEnv:  map[string]string{"AZURE_TENANT_ID": "t", "AZURE_CLIENT_ID": "c", "AZURE_CLIENT_SECRET": "s"},
		},
		{
			name:     "managed-identity",
			values:   map[string]string{"type": "azure-identity", "credentialType": "managed-identity", "clientId": "uami"},
			wantType: "managed-identity",
			wantEnv:  map[string]string{"AZURE_CLIENT_ID": "uami"},
			absent:   []string{"AZURE_CLIENT_SECRET"},
		},
		{
			name:     "workload-identity",
			values:   map[string]string{"type": "azure-identity", "credentialType": "workload-identity", "tenantId": "t", "clientId": "c"},
			wantType: "workload-identity",
			wantEnv:  map[string]string{"AZURE_TENANT_ID": "t", "AZURE_CLIENT_ID": "c"},
			absent:   []string{"AZURE_CLIENT_SECRET"},
		},
		{
			name:     "default",
			values:   map[string]string{"type": "azure-identity", "credentialType": "default"},
			wantType: "default",
			absent:   []string{"AZURE_CLIENT_SECRET"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &stubStore{data: map[string][]byte{
				"SecretRef/default/creds": secretRefBytes(t, "creds", tc.values),
			}}
			d := newDispatcher(st)
			got := map[string]string{}
			for _, e := range d.resolveCredentialEnv(context.Background(), "default", model) {
				got[e.Name] = e.Value
			}
			if got["AZURE_CREDENTIAL_TYPE"] != tc.wantType {
				t.Errorf("AZURE_CREDENTIAL_TYPE = %q, want %q", got["AZURE_CREDENTIAL_TYPE"], tc.wantType)
			}
			for k, v := range tc.wantEnv {
				if got[k] != v {
					t.Errorf("%s = %q, want %q", k, got[k], v)
				}
			}
			for _, k := range tc.absent {
				if _, ok := got[k]; ok {
					t.Errorf("%s should be absent for %s", k, tc.name)
				}
			}
		})
	}
}

func TestResolveCredentialEnv_DefaultsAPIKeysAndInvalidSecrets(t *testing.T) {
	credentialModel := func() *v1alpha1.Model {
		return &v1alpha1.Model{Spec: v1alpha1.ModelSpec{
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "creds"},
		}}
	}
	secret := func(values map[string]string) map[string][]byte {
		return map[string][]byte{
			"SecretRef/default/creds": secretRefBytes(t, "creds", values),
		}
	}
	tests := []struct {
		name    string
		model   *v1alpha1.Model
		data    map[string][]byte
		wantEnv map[string]string
	}{
		{
			name:  "azure identity defaults to client secret",
			model: credentialModel(),
			data:  secret(map[string]string{"type": "azure-identity", "tenantId": "tenant", "clientId": "client", "clientSecret": "secret"}),
			wantEnv: map[string]string{
				"AZURE_CREDENTIAL_TYPE": "client-secret",
				"AZURE_TENANT_ID":       "tenant",
				"AZURE_CLIENT_ID":       "client",
				"AZURE_CLIENT_SECRET":   "secret",
			},
		},
		{
			name:  "managed identity omits empty client ID",
			model: credentialModel(),
			data:  secret(map[string]string{"type": "azure-identity", "credentialType": "managed-identity"}),
			wantEnv: map[string]string{
				"AZURE_CREDENTIAL_TYPE": "managed-identity",
			},
		},
		{
			name:  "default identity includes optional selectors",
			model: credentialModel(),
			data:  secret(map[string]string{"type": "azure-identity", "credentialType": "default", "tenantId": "tenant", "clientId": "client"}),
			wantEnv: map[string]string{
				"AZURE_CREDENTIAL_TYPE": "default",
				"AZURE_TENANT_ID":       "tenant",
				"AZURE_CLIENT_ID":       "client",
			},
		},
		{
			name:    "explicit API key",
			model:   credentialModel(),
			data:    secret(map[string]string{"type": "api-key", "apiKey": "primary"}),
			wantEnv: map[string]string{"KOGNITION_API_KEY": "primary"},
		},
		{
			name:    "provider-named API key fallback",
			model:   credentialModel(),
			data:    secret(map[string]string{"type": "api-key", "token": "fallback"}),
			wantEnv: map[string]string{"KOGNITION_API_KEY": "fallback"},
		},
		{name: "nil model", data: map[string][]byte{}},
		{name: "missing credential reference", model: &v1alpha1.Model{}, data: map[string][]byte{}},
		{name: "missing SecretRef", model: credentialModel(), data: map[string][]byte{}},
		{name: "unsupported azure mode", model: credentialModel(), data: secret(map[string]string{"type": "azure-identity", "credentialType": "certificate"})},
		{name: "empty API key", model: credentialModel(), data: secret(map[string]string{"type": "api-key"})},
		{name: "unsupported credential type", model: credentialModel(), data: secret(map[string]string{"type": "oauth"})},
		{
			name:  "SecretRef without annotations",
			model: credentialModel(),
			data: map[string][]byte{
				"SecretRef/default/creds": resourceBytes(t, v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{Name: "creds"}}),
			},
		},
		{
			name:  "malformed secret data",
			model: credentialModel(),
			data: map[string][]byte{
				"SecretRef/default/creds": resourceBytes(t, v1alpha1.SecretRef{ObjectMeta: v1alpha1.ObjectMeta{
					Name: "creds",
					Annotations: map[string]string{
						"kognition.dev/internal-secret-data": "{",
					},
				}}),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d := newDispatcher(&stubStore{data: test.data})
			got := envToMap(d.resolveCredentialEnv(context.Background(), "default", test.model))
			if len(got) != len(test.wantEnv) {
				t.Fatalf("environment = %#v, want %#v", got, test.wantEnv)
			}
			for name, value := range test.wantEnv {
				if got[name] != value {
					t.Errorf("%s = %q, want %q", name, got[name], value)
				}
			}
		})
	}
}

func TestBuildKBEmbeddingBlock_MissingSecretRefStillReturnsBlockWithoutAPIKey(t *testing.T) {
	// Documents current behavior: secret read failure only logs; the block
	// is still returned so the runtime surfaces the auth error itself.
	st := &stubStore{data: map[string][]byte{
		"Model/default/emb-model": modelBytes(t, "emb-model", modelOpts{
			provider: "foundry",
			credName: "missing-secret",
		}),
	}}
	d := newDispatcher(st)
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("emb-model"), newKBC(""))
	if block == nil {
		t.Fatal("expected non-nil block even when secretRef is missing")
	}
	if _, ok := block["apiKey"]; ok {
		t.Errorf("apiKey should be absent when secret read fails, got %v", block["apiKey"])
	}
	// Provider must still be mapped correctly.
	if block["provider"] != "azure_openai" {
		t.Errorf("provider = %v, want azure_openai", block["provider"])
	}
}

func TestBuildKBEmbeddingBlock_SecretRefMissingRequestedKey(t *testing.T) {
	st := &stubStore{data: map[string][]byte{
		"Model/default/emb-model": modelBytes(t, "emb-model", modelOpts{
			provider: "foundry",
			credName: "emb-creds",
		}),
		"SecretRef/default/emb-creds": secretRefBytes(t, "emb-creds", map[string]string{
			"otherKey": "ignored",
		}),
	}}
	d := newDispatcher(st)
	block := d.buildKBEmbeddingBlock(context.Background(), newKB("emb-model"), newKBC(""))
	if block == nil {
		t.Fatal("expected non-nil block when apiKey is missing from secret")
	}
	if _, ok := block["apiKey"]; ok {
		t.Errorf("apiKey should be absent when key missing, got %v", block["apiKey"])
	}
}

// --------------------------------------------------------------------------- //
// buildEnvVars — memory service wiring
// --------------------------------------------------------------------------- //

func envToMap(envs []corev1.EnvVar, errors ...error) map[string]string {
	for _, err := range errors {
		if err != nil {
			panic(err)
		}
	}
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}

func TestMergeEnvVars_CredentialOverridesEmptyAPIKeyWithoutDuplicate(t *testing.T) {
	merged := mergeEnvVars(
		[]corev1.EnvVar{{Name: "KOGNITION_MODEL_ID", Value: "mock"}, {Name: "KOGNITION_API_KEY", Value: ""}},
		[]corev1.EnvVar{{Name: "KOGNITION_API_KEY", Value: "mock-key"}},
	)
	if len(merged) != 2 || merged[1].Name != "KOGNITION_API_KEY" || merged[1].Value != "mock-key" {
		t.Fatalf("merged environment = %#v", merged)
	}
}

func memAgent(withClaim bool) *v1alpha1.Agent {
	a := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "mem-agent", Namespace: "default"},
	}
	if withClaim {
		a.Spec.Memory = &v1alpha1.AgentMemorySpec{
			Claims: []v1alpha1.LocalObjectReference{{Name: "notes"}},
		}
	}
	return a
}

func TestBuildEnvVars_SharedDeploymentRuntimeContract(t *testing.T) {
	maxTokens := int64(4096)
	tool := v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Protocol:    v1alpha1.ToolProtocolOpenAPI,
			ClassName:   "http",
			Description: "Search documents",
			Endpoint:    "https://tools.example/search",
			Schema:      map[string]any{"type": "object"},
		},
	}
	claim := v1alpha1.MemoryClaim{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "shared-notes", Namespace: "default"},
		Spec:       v1alpha1.MemoryClaimSpec{Type: v1alpha1.MemoryTypeShared},
		Status:     v1alpha1.MemoryClaimStatus{BoundVolumeName: "shared-volume"},
	}
	d := &K8sDispatcher{
		store: &stubStore{data: map[string][]byte{
			"Tool/default/search":              resourceBytes(t, tool),
			"MemoryClaim/default/shared-notes": resourceBytes(t, claim),
			"MemoryVolume/default/shared-volume": resourceBytes(t, v1alpha1.MemoryVolume{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "shared-volume", Namespace: "default"},
				Spec:       v1alpha1.MemoryVolumeSpec{BackendHandle: "backend-shared"},
				Status:     v1alpha1.MemoryVolumeStatus{Phase: v1alpha1.PhaseBound},
			}),
		}},
		logger: zap.NewNop(),
		config: Config{MemoryServiceURL: "http://memory.default:8090"},
	}
	agent := &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{Inline: "Use cited sources."},
			Runtime:      &v1alpha1.AgentRuntimeSpec{MaxTokens: &maxTokens, Timeout: "45"},
			Tools: []v1alpha1.AgentToolRef{
				{Ref: v1alpha1.LocalObjectReference{Name: "search"}, Required: true},
			},
			Memory: &v1alpha1.AgentMemorySpec{
				Claims: []v1alpha1.LocalObjectReference{{Name: "shared-notes"}},
			},
		},
	}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{
		ModelID:    "gpt-4o",
		Endpoint:   "https://models.example",
		Parameters: map[string]string{"apiVersion": "2025-01-01"},
	}}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "instance-private"},
		Spec: v1alpha1.AgentInstanceSpec{
			SessionContext: map[string]string{"secret-session-key": "private-value"},
		},
	}

	got := envToMap(d.buildEnvVars(context.Background(), "default", agent, model, instance, "azure-openai", "api-key"))
	want := map[string]string{
		"KOGNITION_AGENT_NAME":         "researcher",
		"KOGNITION_MODEL_PROVIDER":     "azure-openai",
		"KOGNITION_MODEL_ID":           "gpt-4o",
		"KOGNITION_MODEL_ENDPOINT":     "https://models.example",
		"KOGNITION_API_KEY":            "api-key",
		"KOGNITION_API_VERSION":        "2025-01-01",
		"KOGNITION_SYSTEM_PROMPT":      "Use cited sources.",
		"KOGNITION_MAX_TOKENS":         "4096",
		"KOGNITION_TIMEOUT_SECONDS":    "45",
		"KOGNITION_MEMORY_ENABLED":     "true",
		"KOGNITION_MEMORY_VOLUME":      "backend-shared",
		"KOGNITION_MEMORY_SERVICE_URL": "http://memory.default:8090",
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}

	var rawTools []v1alpha1.AgentToolRef
	if err := json.Unmarshal([]byte(got["KOGNITION_TOOLS_JSON"]), &rawTools); err != nil {
		t.Fatalf("decode KOGNITION_TOOLS_JSON: %v", err)
	}
	if len(rawTools) != 1 || rawTools[0].Ref.Name != "search" || !rawTools[0].Required {
		t.Errorf("KOGNITION_TOOLS_JSON = %#v", rawTools)
	}

	var resolvedTools []resolvedTool
	if err := json.Unmarshal([]byte(got["KOGNITION_TOOLS_RESOLVED"]), &resolvedTools); err != nil {
		t.Fatalf("decode KOGNITION_TOOLS_RESOLVED: %v", err)
	}
	if len(resolvedTools) != 1 || resolvedTools[0].Name != "search" || resolvedTools[0].Endpoint != tool.Spec.Endpoint {
		t.Errorf("KOGNITION_TOOLS_RESOLVED = %#v", resolvedTools)
	}

	d.config.ToolGatewayURL = "http://tool-gateway.default:8092"
	got = envToMap(d.buildEnvVars(context.Background(), "default", agent, model, instance, "azure-openai", "api-key"))
	if got["KOGNITION_TOOL_GATEWAY_URL"] != d.config.ToolGatewayURL {
		t.Fatalf("KOGNITION_TOOL_GATEWAY_URL = %q, want %q", got["KOGNITION_TOOL_GATEWAY_URL"], d.config.ToolGatewayURL)
	}
	resolvedTools = nil
	if err := json.Unmarshal([]byte(got["KOGNITION_TOOLS_RESOLVED"]), &resolvedTools); err != nil {
		t.Fatalf("decode gateway KOGNITION_TOOLS_RESOLVED: %v", err)
	}
	if len(resolvedTools) != 1 || resolvedTools[0].Endpoint != "" || resolvedTools[0].ResourceVersion != tool.ObjectMeta.ResourceVersion {
		t.Errorf("gateway KOGNITION_TOOLS_RESOLVED = %#v, want endpoint redacted with stable version", resolvedTools)
	}

	for name, value := range got {
		if strings.Contains(value, "instance-private") || strings.Contains(value, "private-value") || strings.Contains(value, "secret-session-key") {
			t.Errorf("instance-specific data leaked through %s", name)
		}
	}
}

// TC-0300: Gateway runtime templates retain no direct provider configuration.
func TestBuildEnvVars_ModelGatewayRedactsDirectProviderConfiguration(t *testing.T) {
	d := &K8sDispatcher{config: Config{ModelGatewayURL: "http://model-gateway.kognition-system:8093/v1"}, logger: zap.NewNop()}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "default"}}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt-4o", Endpoint: "https://provider.example/v1", Parameters: map[string]string{"apiVersion": "2025-01-01"}}}
	envs := envToMap(d.buildEnvVars(context.Background(), "default", agent, model, nil, "openai-compatible", "provider-key"))
	if envs["KOGNITION_MODEL_GATEWAY_URL"] != d.config.ModelGatewayURL {
		t.Fatalf("KOGNITION_MODEL_GATEWAY_URL = %q", envs["KOGNITION_MODEL_GATEWAY_URL"])
	}
	for _, name := range []string{"KOGNITION_MODEL_ENDPOINT", "KOGNITION_API_KEY", "KOGNITION_API_VERSION"} {
		if _, found := envs[name]; found {
			t.Errorf("gateway environment must not project %s", name)
		}
	}
	if envs["KOGNITION_MODEL_PROVIDER"] != "openai-compatible" || envs["KOGNITION_MODEL_ID"] != "gpt-4o" {
		t.Errorf("gateway model environment = %#v", envs)
	}
}

// TC-0294: Runtime Pods use gateway-only model and Tool egress under enforcing NetworkPolicies.
func TestRuntimeNetworkEgress_TC0294_ModelGatewayDisablesPublicHTTPS(t *testing.T) {
	rules := runtimeNetworkEgress("", "", 0, "", "", 0, "http://model-gateway:8093", "kognition-system", 8093, "", "", 0, "", "", 0, false)
	for _, rule := range rules {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				t.Fatal("Model Gateway runtime egress must not allow arbitrary public HTTPS")
			}
		}
	}
	if len(rules) != 2 || rules[1].Ports[0].Port.IntVal != 8093 {
		t.Fatalf("gateway egress rules = %#v", rules)
	}
}

func TestBuildEnvVars_ExposesOnlyAgentCommunicationGatewayURL(t *testing.T) {
	d := &K8sDispatcher{config: Config{AgentCommunicationGatewayURL: "http://agent-communication-gateway.kognition-system:8094"}, logger: zap.NewNop()}
	envs := envToMap(d.buildEnvVars(context.Background(), "default", &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "default"}}, &v1alpha1.Model{}, nil, "openai", ""))
	if envs["KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL"] != d.config.AgentCommunicationGatewayURL {
		t.Fatalf("KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL = %q", envs["KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL"])
	}
	for name, value := range envs {
		if strings.Contains(name, "DELEGATION_CAPABILITY") || strings.Contains(value, "capability-hmac") {
			t.Fatalf("delegation signing material leaked through runtime environment: %s=%q", name, value)
		}
	}
}

func TestUsesAgentCommunicationGateway(t *testing.T) {
	if (*K8sDispatcher)(nil).UsesAgentCommunicationGateway() {
		t.Fatal("nil dispatcher reports Agent Communication Gateway enabled")
	}
	d := &K8sDispatcher{}
	if d.UsesAgentCommunicationGateway() {
		t.Fatal("dispatcher reports Agent Communication Gateway enabled without a URL")
	}
	d.config.AgentCommunicationGatewayURL = "http://agent-communication-gateway:8094"
	if !d.UsesAgentCommunicationGateway() {
		t.Fatal("dispatcher did not report configured Agent Communication Gateway")
	}
}

func TestBuildEnvVarsInheritsOpenTelemetryConfig(t *testing.T) {
	t.Setenv("KOGNITION_OTEL_TRACES_ENABLED", "true")
	t.Setenv("KOGNITION_OTEL_TRACES_ENDPOINT", "jaeger-collector.kognition-observability.svc:4317")
	t.Setenv("KOGNITION_OTEL_TRACES_PROTOCOL", "grpc")
	t.Setenv("KOGNITION_OTEL_TRACES_SAMPLE_RATE", "1")
	t.Setenv("KOGNITION_OTEL_TRACES_SERVICE_NAME", "kognition-controller")
	t.Setenv("KOGNITION_OTEL_TRACES_ENVIRONMENT", "kind")

	d := &K8sDispatcher{logger: zap.NewNop()}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher", Namespace: "default"}}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt-4o"}}

	got := envToMap(d.buildEnvVars(context.Background(), "default", agent, model, nil, "openai", "key"))
	if got["KOGNITION_OTEL_TRACES_ENABLED"] != "true" {
		t.Errorf("KOGNITION_OTEL_TRACES_ENABLED = %q, want true", got["KOGNITION_OTEL_TRACES_ENABLED"])
	}
	if got["KOGNITION_OTEL_TRACES_ENDPOINT"] != "jaeger-collector.kognition-observability.svc:4317" {
		t.Errorf("KOGNITION_OTEL_TRACES_ENDPOINT = %q", got["KOGNITION_OTEL_TRACES_ENDPOINT"])
	}
	if got["KOGNITION_OTEL_TRACES_SERVICE_NAME"] != "kognition-controller" {
		t.Errorf("KOGNITION_OTEL_TRACES_SERVICE_NAME = %q", got["KOGNITION_OTEL_TRACES_SERVICE_NAME"])
	}
	if _, ok := got["KOGNITION_OTEL_TRACES_HEADERS"]; ok {
		t.Errorf("empty KOGNITION_OTEL_TRACES_HEADERS should not be inherited")
	}
}

func TestBuildEnvVars_InjectsMemoryServiceURL(t *testing.T) {
	d := &K8sDispatcher{
		store:  &stubStore{data: map[string][]byte{}},
		logger: zap.NewNop(),
		config: Config{MemoryServiceURL: "http://mem:8090"},
	}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt", Endpoint: "http://x"}}

	got := envToMap(d.buildEnvVars(context.Background(), "default", memAgent(true), model, nil, "openai", "key"))
	if got["KOGNITION_MEMORY_ENABLED"] != "true" {
		t.Fatalf("KOGNITION_MEMORY_ENABLED = %q, want true", got["KOGNITION_MEMORY_ENABLED"])
	}
	if got["KOGNITION_MEMORY_SERVICE_URL"] != "http://mem:8090" {
		t.Fatalf("KOGNITION_MEMORY_SERVICE_URL = %q, want http://mem:8090", got["KOGNITION_MEMORY_SERVICE_URL"])
	}
}

func TestBuildEnvVars_PrefersMemoryPolicyGatewayURL(t *testing.T) {
	d := &K8sDispatcher{
		store:  &stubStore{data: map[string][]byte{}},
		logger: zap.NewNop(),
		config: Config{MemoryServiceURL: "http://memory:8090", MemoryPolicyGatewayURL: "http://memory-policy:8091"},
	}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt", Endpoint: "http://x"}}
	got := envToMap(d.buildEnvVars(context.Background(), "default", memAgent(true), model, nil, "openai", "key"))
	if got["KOGNITION_MEMORY_SERVICE_URL"] != "http://memory-policy:8091" {
		t.Fatalf("KOGNITION_MEMORY_SERVICE_URL = %q, want policy gateway URL", got["KOGNITION_MEMORY_SERVICE_URL"])
	}
}

func TestBuildEnvVars_InjectsRuntimeAuthFromSecret(t *testing.T) {
	d := &K8sDispatcher{
		store:  &stubStore{data: map[string][]byte{}},
		logger: zap.NewNop(),
		config: Config{RuntimeAuthToken: "controller-token", RuntimeAuthTokenSecretName: "runtime-secrets", RuntimeAuthTokenSecretKey: "control-token"},
	}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt", Endpoint: "http://x"}}
	envs, err := d.buildEnvVars(context.Background(), "default", memAgent(false), model, nil, "openai", "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range envs {
		if env.Name == "KOGNITION_RUNTIME_AUTH_TOKEN" {
			if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Name != "runtime-secrets" || env.ValueFrom.SecretKeyRef.Key != "control-token" {
				t.Fatalf("runtime auth env = %#v", env)
			}
			return
		}
	}
	t.Fatal("runtime auth token environment variable was not injected")
}

func TestEnsureRuntimeResourcesCreatesNamespaceLocalIsolationPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secrets", Namespace: "kognition-system"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	d := NewK8sDispatcher(client, nil, zap.NewNop(), Config{AgentImage: "runtime:test", RuntimeAuthToken: "controller-token", RuntimeAuthTokenSecretName: "runtime-secrets", RuntimeNetworkPolicyEnabled: true, KubeletProbeIngressCIDRs: []string{"172.16.0.0/12"}, MemoryPolicyGatewayURL: "http://gateway:8091", MemoryPolicyGatewayPort: 18091, ControllerNamespace: "kognition-system", MemoryPolicyGatewayNamespace: "kognition-system"})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatal(err)
	}
	name := deploymentName(d.workloadKey("tenant-a", "planner")) + "-network"
	var policy networkingv1.NetworkPolicy
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: name}, &policy); err != nil {
		t.Fatalf("get runtime NetworkPolicy: %v", err)
	}
	if policy.Spec.PodSelector.MatchLabels[labelAgent] != d.workloadKey("tenant-a", "planner") || len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 2 {
		t.Fatalf("runtime NetworkPolicy = %#v", policy.Spec)
	}
	if policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "k8s-controller" {
		t.Fatalf("runtime ingress = %#v", policy.Spec.Ingress)
	}
	if policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kognition-system" {
		t.Fatalf("runtime ingress namespace selector = %#v", policy.Spec.Ingress)
	}
	if policy.Spec.Ingress[0].From[1].IPBlock == nil || policy.Spec.Ingress[0].From[1].IPBlock.CIDR != "172.16.0.0/12" {
		t.Fatalf("runtime kubelet probe ingress = %#v", policy.Spec.Ingress)
	}
	foundGateway := false
	for _, egress := range policy.Spec.Egress {
		for _, peer := range egress.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == "memory-policy-gateway" {
				foundGateway = true
				if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kognition-system" {
					t.Fatalf("gateway egress namespace selector = %#v", peer.NamespaceSelector)
				}
			}
		}
	}
	if !foundGateway {
		t.Fatalf("runtime NetworkPolicy egress = %#v, want memory policy gateway", policy.Spec.Egress)
	}
	var mirrored corev1.Secret
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "runtime-secrets"}, &mirrored); err != nil || string(mirrored.Data["runtime-control-token"]) != "controller-token" {
		t.Fatalf("runtime token Secret mirror = %#v, err = %v", mirrored, err)
	}
	foundGatewayPort := false
	for _, egress := range policy.Spec.Egress {
		for _, port := range egress.Ports {
			if port.Port != nil && port.Port.IntValue() == 18091 {
				foundGatewayPort = true
			}
		}
	}
	if !foundGatewayPort {
		t.Fatalf("runtime NetworkPolicy egress = %#v, want configured gateway port", policy.Spec.Egress)
	}
}

func TestEnsureRuntimeResourcesAllowsOnlyConfiguredAgentCommunicationGatewayIngress(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secrets", Namespace: "kognition-system"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	d := NewK8sDispatcher(client, nil, zap.NewNop(), Config{
		AgentImage: "runtime:test", RuntimeAuthToken: "controller-token", RuntimeAuthTokenSecretName: "runtime-secrets",
		RuntimeNetworkPolicyEnabled: true, KubeletProbeIngressCIDRs: []string{"172.16.0.0/12"},
		ControllerNamespace: "kognition-system", AgentCommunicationGatewayURL: "http://agent-communication-gateway:8094",
		AgentCommunicationGatewayNamespace: "kognition-system", AgentCommunicationGatewayPort: 8094,
	})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	name := deploymentName(d.workloadKey("tenant-a", "planner")) + "-network"
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: name}, &policy); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, peer := range policy.Spec.Ingress[0].From {
		if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == "agent-communication-gateway" {
			found = peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kognition-system"
		}
	}
	if !found {
		t.Fatalf("runtime ingress = %#v, want configured Agent Communication Gateway peer", policy.Spec.Ingress)
	}
	d.config.AgentCommunicationGatewayNamespace = ""
	if err := d.ensureRuntimeWorkloadNetworkPolicy(context.Background(), "tenant-a", deploymentName(d.workloadKey("tenant-a", "planner")), map[string]string{labelManagedBy: managedByValue, labelAgent: d.workloadKey("tenant-a", "planner")}, map[string]string{AnnotationOwnerAgent: "tenant-a/planner"}, agent); err == nil || !strings.Contains(err.Error(), "requires a namespace") {
		t.Fatalf("missing gateway namespace error = %v", err)
	}
}

func TestEnsureRuntimeResourcesAllowsCiliumHostProbeModeWithoutCIDRs(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secrets", Namespace: "kognition-system"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	d := NewK8sDispatcher(client, nil, zap.NewNop(), Config{AgentImage: "runtime:test", RuntimeAuthToken: "controller-token", RuntimeAuthTokenSecretName: "runtime-secrets", RuntimeNetworkPolicyEnabled: true, CiliumHostProbeIngress: true, ControllerNamespace: "kognition-system"})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatalf("ensure runtime resources with Cilium host probe mode: %v", err)
	}
	name := deploymentName(d.workloadKey("tenant-a", "planner")) + "-network"
	var policy networkingv1.NetworkPolicy
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: name}, &policy); err != nil {
		t.Fatal(err)
	}
	for _, peer := range policy.Spec.Ingress[0].From {
		if peer.IPBlock != nil {
			t.Fatalf("native runtime policy unexpectedly opened CIDR %q in Cilium host probe mode", peer.IPBlock.CIDR)
		}
	}
}

func TestEnsureRuntimeResourcesRejectsUninjectableRuntimeAuthToken(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	d := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, zap.NewNop(), Config{RuntimeAuthToken: "controller-token", ControllerNamespace: "kognition-system"})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err == nil || !strings.Contains(err.Error(), "RuntimeAuthTokenSecretName") {
		t.Fatalf("ensureRuntimeResources() error = %v, want runtime token injection validation", err)
	}
}

func TestEnsureRuntimeResourcesPropagatesOnlyKnowledgeGatewayCA(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "knowledge-runtime-tls", Namespace: "kognition-system"},
		Data: map[string][]byte{
			"ca.crt":  []byte("ca-v1"),
			"tls.key": []byte("must-not-propagate"),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	d := NewK8sDispatcher(k8sClient, nil, zap.NewNop(), Config{
		AgentImage:                   "runtime:test",
		KnowledgeGatewayURL:          "https://knowledge-gateway.kognition-system.svc.cluster.local:8095",
		KnowledgeGatewayNamespace:    "kognition-system",
		KnowledgeGatewayCASecretName: "knowledge-runtime-tls",
		KnowledgeGatewayCASecretKey:  "ca.crt",
	})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}

	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatalf("ensure runtime resources: %v", err)
	}
	var trust corev1.ConfigMap
	trustKey := client.ObjectKey{Namespace: "tenant-a", Name: knowledgeGatewayCAConfigMap}
	if err := k8sClient.Get(context.Background(), trustKey, &trust); err != nil {
		t.Fatalf("get propagated gateway CA: %v", err)
	}
	if len(trust.Data) != 0 || len(trust.BinaryData) != 1 || string(trust.BinaryData["ca.crt"]) != "ca-v1" {
		t.Fatalf("propagated trust data = %#v/%#v, want only ca.crt", trust.Data, trust.BinaryData)
	}
	var deployment appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "agent-planner"}, &deployment); err != nil {
		t.Fatalf("get runtime deployment: %v", err)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if len(deployment.Spec.Template.Spec.Volumes) != 1 || len(container.VolumeMounts) != 1 || !container.VolumeMounts[0].ReadOnly {
		t.Fatalf("gateway trust volume/mount = %#v/%#v", deployment.Spec.Template.Spec.Volumes, container.VolumeMounts)
	}

	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal(err)
	}
	source.Data["ca.crt"] = []byte("ca-v2")
	if err := k8sClient.Update(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatalf("reconcile rotated gateway CA: %v", err)
	}
	if err := k8sClient.Get(context.Background(), trustKey, &trust); err != nil {
		t.Fatal(err)
	}
	if got := string(trust.BinaryData["ca.crt"]); got != "ca-v2" {
		t.Fatalf("rotated gateway CA = %q, want ca-v2", got)
	}
}

func TestEnsureRuntimeResourcesRemovesPolicyWhenRuntimeNetworkPolicyIsDisabled(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-secrets", Namespace: "kognition-system"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	d := NewK8sDispatcher(k8sClient, nil, zap.NewNop(), Config{RuntimeAuthToken: "controller-token", RuntimeAuthTokenSecretName: "runtime-secrets", RuntimeNetworkPolicyEnabled: true, KubeletProbeIngressCIDRs: []string{"172.16.0.0/12"}, ControllerNamespace: "kognition-system"})
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatal(err)
	}
	policyKey := client.ObjectKey{Namespace: "tenant-a", Name: deploymentName(d.workloadKey("tenant-a", "planner")) + "-network"}
	if err := k8sClient.Get(context.Background(), policyKey, &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("get enabled runtime policy: %v", err)
	}
	d.config.RuntimeNetworkPolicyEnabled = false
	if err := d.ensureRuntimeResources(context.Background(), "tenant-a", agent, nil); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(context.Background(), policyKey, &networkingv1.NetworkPolicy{}); !apierrors.IsNotFound(err) {
		t.Fatalf("runtime policy remained after NetworkPolicy disabled: %v", err)
	}
}

func TestIssueMemoryCapabilityBindsManagedIdentityAndBackendHandle(t *testing.T) {
	issuer, err := runtimeauth.NewMemoryCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claim := v1alpha1.MemoryClaim{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "notes", Namespace: "tenant-a"},
		Status:     v1alpha1.MemoryClaimStatus{BoundVolumeName: "notes-volume"},
	}
	volume := v1alpha1.MemoryVolume{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "notes-volume", Namespace: "tenant-a"},
		Spec:       v1alpha1.MemoryVolumeSpec{BackendHandle: "backend-notes"},
		Status:     v1alpha1.MemoryVolumeStatus{Phase: v1alpha1.PhaseBound},
	}
	d := &K8sDispatcher{
		store: &stubStore{data: map[string][]byte{
			"MemoryClaim/tenant-a/notes":         resourceBytes(t, claim),
			"MemoryVolume/tenant-a/notes-volume": resourceBytes(t, volume),
		}},
		logger: zap.NewNop(),
		config: Config{MemoryPolicyGatewayURL: "http://memory-policy:8091", MemoryCapabilityIssuer: issuer},
	}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}, Spec: v1alpha1.AgentSpec{Memory: &v1alpha1.AgentMemorySpec{Claims: []v1alpha1.LocalObjectReference{{Name: "notes"}}}}}
	instance := &v1alpha1.AgentInstance{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "tenant-a", UID: "instance-uid"},
		Spec: v1alpha1.AgentInstance{Spec: v1alpha1.AgentInstanceSpec{SessionContext: map[string]string{
			intctrl.MemoryCapabilityIdentityNameContextKey: "agent-identity-planner",
		}}}.Spec,
	}
	capability, err := d.issueMemoryCapability(context.Background(), "tenant-a", agent, instance, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.Verify(capability, runtimeauth.MemoryCapabilityBinding{
		Namespace: "tenant-a", AgentName: "planner", AgentIdentityName: "agent-identity-planner",
		InstanceName: "planner-r1", InstanceUID: instance.ObjectMeta.UID, SessionID: "session-1", VolumeHandle: "backend-notes",
	})
	if err != nil || claims.VolumeHandle != "backend-notes" {
		t.Fatalf("capability claims = %#v, err = %v", claims, err)
	}
	delete(instance.Spec.SessionContext, intctrl.MemoryCapabilityIdentityNameContextKey)
	if _, err := d.issueMemoryCapability(context.Background(), "tenant-a", agent, instance, "session-1"); err == nil {
		t.Fatal("gateway dispatch without managed identity binding was allowed")
	}
}

func TestIssueModelSessionCapabilityBindsManagedIdentityAndModelRevision(t *testing.T) {
	issuer, err := runtimeauth.NewModelSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d := &K8sDispatcher{logger: zap.NewNop(), config: Config{ModelGatewayURL: "http://model-gateway:8093", ModelSessionCapabilityIssuer: issuer}}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	model := &v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "gpt-4o", Namespace: "tenant-a", ResourceVersion: 9}}
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "tenant-a", UID: "instance-uid", Annotations: map[string]string{observability.AnnotationWorkflowName: "research-flow", observability.AnnotationStepName: "plan"}}, Spec: v1alpha1.AgentInstanceSpec{SessionContext: map[string]string{intctrl.ModelCapabilityIdentityNameContextKey: "agent-identity-planner"}}}

	capability, err := d.issueModelSessionCapability(context.Background(), agent, model, instance, "session-1", "research-solution", 12)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(capability)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ModelName != model.ObjectMeta.Name || claims.ModelResourceVersion != model.ObjectMeta.ResourceVersion || claims.AgentIdentityName != "agent-identity-planner" || claims.WorkflowName != "research-flow" || claims.StepName != "plan" || claims.Solution != "research-solution" || claims.SolutionRev != 12 {
		t.Fatalf("model capability claims = %#v", claims)
	}
	model.ObjectMeta.ResourceVersion++
	if claims.ModelResourceVersion == model.ObjectMeta.ResourceVersion {
		t.Fatal("model capability did not bind the original Model revision")
	}
	delete(instance.Spec.SessionContext, intctrl.ModelCapabilityIdentityNameContextKey)
	if _, err := d.issueModelSessionCapability(context.Background(), agent, model, instance, "session-1", "", 0); err == nil {
		t.Fatal("Model Gateway session capability without automatic identity was allowed")
	}
	d.config.ModelGatewayURL = ""
	if capability, err := d.issueModelSessionCapability(context.Background(), agent, model, instance, "session-1", "", 0); err != nil || capability != "" {
		t.Fatalf("disabled Model Gateway capability = %q, %v", capability, err)
	}
}

func TestIssueAgentDelegationSessionCapabilityBindsManagedIdentity(t *testing.T) {
	issuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d := &K8sDispatcher{logger: zap.NewNop(), config: Config{AgentCommunicationGatewayURL: "http://agent-communication-gateway:8094", AgentDelegationSessionCapabilityIssuer: issuer}}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a"}}
	instance := &v1alpha1.AgentInstance{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner-r1", Namespace: "tenant-a", UID: "instance-uid"}, Spec: v1alpha1.AgentInstanceSpec{SessionContext: map[string]string{intctrl.AgentDelegationCapabilityIdentityNameContextKey: "agent-identity-planner"}}}

	capability, err := d.issueAgentDelegationSessionCapability(agent, instance, "session-1", "research-solution", 12)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := issuer.VerifyClaims(capability)
	if err != nil {
		t.Fatal(err)
	}
	if claims.AgentName != "planner" || claims.AgentIdentityName != "agent-identity-planner" || claims.InstanceUID != "instance-uid" || claims.SessionID != "session-1" || claims.Solution != "research-solution" || claims.SolutionRev != 12 {
		t.Fatalf("delegation claims = %#v", claims)
	}
	delete(instance.Spec.SessionContext, intctrl.AgentDelegationCapabilityIdentityNameContextKey)
	if _, err := d.issueAgentDelegationSessionCapability(agent, instance, "session-1", "", 0); err == nil {
		t.Fatal("delegation capability without automatic identity was allowed")
	}
	instance.ObjectMeta.Annotations = map[string]string{intctrl.CollaborationDispatchAnnotation: "d1"}
	if capability, err := d.issueAgentDelegationSessionCapability(agent, instance, "session-1", "", 0); err != nil || capability != "" {
		t.Fatalf("collaboration member capability = %q, %v", capability, err)
	}
	instance.ObjectMeta.Annotations = nil
	d.config.AgentCommunicationGatewayURL = ""
	if capability, err := d.issueAgentDelegationSessionCapability(agent, instance, "session-1", "", 0); err != nil || capability != "" {
		t.Fatalf("disabled Agent Communication Gateway capability = %q, %v", capability, err)
	}
}

func TestSolutionAttributionSnapshotsActiveRevision(t *testing.T) {
	solution := v1alpha1.Solution{ObjectMeta: v1alpha1.ObjectMeta{Name: "research", Namespace: "tenant-a"}, Status: v1alpha1.SolutionStatus{ActiveRevision: 8}}
	d := &K8sDispatcher{store: &stubStore{data: map[string][]byte{"Solution/tenant-a/research": resourceBytes(t, solution)}}}
	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "planner", Namespace: "tenant-a", Labels: map[string]string{"kognition.dev/solution": "research"}}}
	name, revision, err := d.solutionAttribution(context.Background(), "tenant-a", agent)
	if err != nil || name != "research" || revision != 8 {
		t.Fatalf("solutionAttribution() = (%q, %d, %v)", name, revision, err)
	}
	name, revision, err = d.solutionAttribution(context.Background(), "tenant-a", &v1alpha1.Agent{})
	if err != nil || name != "" || revision != 0 {
		t.Fatalf("unmanaged solutionAttribution() = (%q, %d, %v)", name, revision, err)
	}
}

func TestModelGatewayRejectsInvocationTimeoutBeyondCapabilityLifetime(t *testing.T) {
	issuer, err := runtimeauth.NewModelSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d := &K8sDispatcher{config: Config{ModelGatewayURL: "http://model-gateway:8093", ModelSessionCapabilityIssuer: issuer}}
	agent := &v1alpha1.Agent{Spec: v1alpha1.AgentSpec{Runtime: &v1alpha1.AgentRuntimeSpec{Timeout: "2m"}}}
	if err := d.validateModelGatewayInvocationLifetime(agent); err == nil {
		t.Fatal("Model Gateway accepted an invocation timeout beyond the capability lifetime")
	}
	agent.Spec.Runtime.Timeout = "1m"
	if err := d.validateModelGatewayInvocationLifetime(agent); err != nil {
		t.Fatalf("Model Gateway rejected a timeout equal to capability lifetime: %v", err)
	}
}

func TestAgentDelegationGatewayRejectsInvocationTimeoutBeyondCapabilityLifetime(t *testing.T) {
	issuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuer([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	d := &K8sDispatcher{config: Config{AgentCommunicationGatewayURL: "http://agent-communication-gateway:8094", AgentDelegationSessionCapabilityIssuer: issuer}}
	agent := &v1alpha1.Agent{Spec: v1alpha1.AgentSpec{Runtime: &v1alpha1.AgentRuntimeSpec{Timeout: "2m"}}}
	if err := d.validateAgentDelegationGatewayInvocationLifetime(agent); err == nil {
		t.Fatal("Agent Communication Gateway accepted an invocation timeout beyond the capability lifetime")
	}
	agent.Spec.Runtime.Timeout = "1m"
	if err := d.validateAgentDelegationGatewayInvocationLifetime(agent); err != nil {
		t.Fatalf("Agent Communication Gateway rejected matching capability lifetime: %v", err)
	}
	d.config.AgentDelegationSessionCapabilityIssuer = nil
	if err := d.validateAgentDelegationGatewayInvocationLifetime(agent); err == nil {
		t.Fatal("Agent Communication Gateway accepted a missing capability issuer")
	}
}

func TestBuildEnvVars_OmitsMemoryURLWhenUnconfigured(t *testing.T) {
	d := &K8sDispatcher{store: &stubStore{data: map[string][]byte{}}, logger: zap.NewNop()}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt"}}

	got := envToMap(d.buildEnvVars(context.Background(), "default", memAgent(true), model, nil, "openai", "key"))
	if got["KOGNITION_MEMORY_ENABLED"] != "true" {
		t.Fatalf("memory should be enabled")
	}
	if _, ok := got["KOGNITION_MEMORY_SERVICE_URL"]; ok {
		t.Fatalf("URL should be omitted when unconfigured")
	}
}

func TestBuildEnvVars_NoMemoryWhenNoClaims(t *testing.T) {
	d := &K8sDispatcher{
		store:  &stubStore{data: map[string][]byte{}},
		logger: zap.NewNop(),
		config: Config{MemoryServiceURL: "http://mem:8090"},
	}
	model := &v1alpha1.Model{Spec: v1alpha1.ModelSpec{ModelID: "gpt"}}

	got := envToMap(d.buildEnvVars(context.Background(), "default", memAgent(false), model, nil, "openai", "key"))
	if got["KOGNITION_MEMORY_ENABLED"] != "false" {
		t.Fatalf("memory should be disabled without claims")
	}
	if _, ok := got["KOGNITION_MEMORY_SERVICE_URL"]; ok {
		t.Fatalf("URL should not be injected when memory disabled")
	}
}
