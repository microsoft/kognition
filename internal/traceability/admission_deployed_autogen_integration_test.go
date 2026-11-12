// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/activation"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func deployedAutoGenProducerAcceptance(test *testing.T, ctx context.Context, restConfig *rest.Config, admin client.Client, controller client.WithWatch, manifest activation.Manifest, image string, created *[]client.Object) {
	test.Helper()
	if os.Getenv("KOGNITION_ADMISSION_TARGET_MANIFEST") != "" || os.Getenv("KOGNITION_ADMISSION_TARGET_RUNTIME_SECRET") != "" {
		test.Fatal("AutoGen credential fixture requires a separate disposable cluster, not an existing installation")
	}
	if !strings.Contains(image, "@sha256:") {
		test.Fatal("AutoGen acceptance requires an immutable runtime image")
	}
	workload := manifest.Namespaces[0]
	create := func(object client.Object) {
		test.Helper()
		if err := createAdmissionFixture(ctx, admin, object, created); err != nil {
			test.Fatal(err)
		}
	}
	var retainedRecovery []*corev1.ConfigMap
	var createdMu sync.Mutex
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		createdMu.Lock()
		retained := append([]*corev1.ConfigMap(nil), retainedRecovery...)
		createdMu.Unlock()
		for _, expected := range retained {
			current := &corev1.ConfigMap{}
			if err := admin.Get(cleanup, client.ObjectKeyFromObject(expected), current); err != nil || current.UID != expected.UID {
				test.Errorf("protected recovery ledger identity changed before namespace teardown: %s error=%v", expected.Name, err)
				continue
			}
			test.Logf("retaining protected recovery ledger %s UID=%s phase=%s for owning disposable namespace teardown", current.Name, current.UID, current.Labels["kognition.dev/recovery-phase"])
		}
	})
	observed := interceptor.NewClient(controller, interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if err := underlying.Create(ctx, object, options...); err != nil {
				return err
			}
			createdMu.Lock()
			if record, ok := object.(*corev1.ConfigMap); ok && record.Namespace == manifest.Namespace && record.Labels["kognition.dev/runtime-recovery"] == "true" {
				// Protected recovery tombstones survive individual resource cleanup.
				retainedRecovery = append(retainedRecovery, record.DeepCopy())
			} else {
				*created = append(*created, object.DeepCopyObject().(client.Object))
			}
			createdMu.Unlock()
			test.Logf("AutoGen dispatcher created %T %s UID=%s", object, client.ObjectKeyFromObject(object), object.GetUID())
			return nil
		},
	})
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: deployedRuntimeSourceName, Namespace: manifest.Namespace}, Data: map[string][]byte{"runtime-control-token": []byte("sg08-autogen-control-token")}}
	create(source)
	if err := controller.Get(ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}); err != nil {
		test.Fatalf("canonical named source read: %v", err)
	}
	modelPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sg08-autogen-model", Namespace: workload}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(1)),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers: []corev1.Container{{Name: "model", Image: "busybox:1.37.0", ImagePullPolicy: corev1.PullNever,
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Command: []string{"sh", "-ec", `mkdir -p /tmp/www/cgi-bin/v1/chat
: > /tmp/model-calls
: > /tmp/model-authorization
cat > /tmp/www/cgi-bin/v1/chat/completions <<'FIXTURE'
#!/bin/sh
printf 'call\n' >> /tmp/model-calls
printf '%s' "$HTTP_AUTHORIZATION" > /tmp/model-authorization
printf 'Content-Type: application/json\r\n\r\n'
printf '%s' '{"id":"sg08-autogen","object":"chat.completion","created":1,"model":"sg08-local","choices":[{"index":0,"message":{"role":"assistant","content":"SG08 AutoGen TLS dispatch completed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}'
FIXTURE
chmod +x /tmp/www/cgi-bin/v1/chat/completions
exec httpd -f -p 8081 -h /tmp/www`},
		}},
	}}
	create(modelPod)
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := admin.Get(ctx, client.ObjectKeyFromObject(modelPod), modelPod); err != nil {
			return false, err
		}
		return len(modelPod.Status.ContainerStatuses) == 1 && modelPod.Status.ContainerStatuses[0].Ready && modelPod.Status.PodIP != "", nil
	}); err != nil {
		test.Fatal(err)
	}
	countCalls := func() int {
		test.Helper()
		output := deployedProducerExec(test, ctx, restConfig, modelPod, "model", []string{"sh", "-ec", "wc -l < /tmp/model-calls"})
		count, err := strconv.Atoi(strings.TrimSpace(output))
		if err != nil {
			test.Fatalf("invalid model effect count %q: %v", output, err)
		}
		return count
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		test.Fatal(err)
	}
	issuer, err := runtimeauth.NewModelSessionCapabilityIssuer(key, 5*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	delegationIssuer, err := runtimeauth.NewAgentDelegationSessionCapabilityIssuer(key, 5*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	keys, err := json.Marshal(map[string]recovery.WorkspaceKeys{workload: {Current: "sg08", Keys: map[string]string{"sg08": base64.StdEncoding.EncodeToString(key)}}})
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	keysFile, caFile := filepath.Join(directory, "recovery.json"), filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(keysFile, keys, 0o600); err != nil {
		test.Fatal(err)
	}
	gateway := "http://" + modelPod.Status.PodIP + ":8081/cgi-bin"
	config := dispatcher.Config{
		ControllerNamespace: manifest.Namespace, RuntimeAuthTokenSecretName: source.Name, RuntimeAuthToken: string(source.Data["runtime-control-token"]),
		RuntimeImages: map[string]string{"autogen": image}, RuntimeTLSSecretName: "sg08-autogen-tls", RuntimeCAFile: caFile,
		RuntimeRecoveryKeysFile: keysFile, RuntimeRecoveryRetention: 7 * 24 * time.Hour,
		ModelGatewayURL: gateway, ModelSessionCapabilityIssuer: issuer,
		ToolGatewayURL: gateway, MemoryPolicyGatewayURL: gateway, AgentCommunicationGatewayURL: gateway, AgentDelegationSessionCapabilityIssuer: delegationIssuer,
	}
	platformStore := k8scontroller.NewStoreAdapterWithControlNamespace(observed, manifest.Namespace)
	persistent, err := k8scost.NewConfigMapStore(observed, manifest.Namespace)
	if err != nil {
		test.Fatal(err)
	}
	newProducer := func(config dispatcher.Config) *dispatcher.K8sDispatcher {
		test.Helper()
		producer := dispatcher.NewK8sDispatcher(observed, platformStore, zap.NewNop(), config)
		if err := producer.ConfigureRuntimeRecoveryFile(persistent, keysFile, 7*24*time.Hour); err != nil {
			test.Fatal(err)
		}
		return producer
	}
	producer := newProducer(config)
	model := convert.ModelToK8s(&api.Model{ObjectMeta: api.ObjectMeta{Name: "sg08-autogen-chat", Namespace: workload}, Spec: api.ModelSpec{Provider: "openai-compatible", ModelID: "sg08-local", Endpoint: gateway}})
	create(model)
	makeAgent := func(name string) *api.Agent {
		test.Helper()
		object := convert.AgentToK8s(&api.Agent{ObjectMeta: api.ObjectMeta{Name: name, Namespace: workload, Annotations: map[string]string{"kognition.dev/agent-delegation-enabled": "false"}}, Spec: api.AgentSpec{
			ModelRef: api.LocalObjectReference{Name: model.Name}, Runtime: &api.AgentRuntimeSpec{Class: "autogen"}, Instructions: api.AgentInstructions{Inline: "Complete the isolated AutoGen TLS acceptance"},
		}})
		create(object)
		agent := convert.AgentFromK8s(object)
		if err := producer.EnsureAgentRuntime(ctx, agent); err != nil {
			test.Fatal(err)
		}
		return agent
	}
	agent := makeAgent("sg08-autogen")
	var services corev1.ServiceList
	if err := admin.List(ctx, &services, client.InNamespace(workload), client.MatchingLabels{"kognition.dev/agent-name": agent.Name}); err != nil || len(services.Items) != 1 {
		test.Fatalf("generated AutoGen Service count=%d error=%v", len(services.Items), err)
	}
	service := &services.Items[0]
	hostname := service.Name + "." + workload + ".svc.cluster.local"
	certificate, privateKey := admissionServingCertificate(test, hostname)
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		test.Fatal(err)
	}
	create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: config.RuntimeTLSSecretName, Namespace: workload}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": certificate, "tls.key": privateKey}})
	wrongAgent := makeAgent("sg08-autogen-wrong-name")
	for _, current := range []*api.Agent{agent, wrongAgent} {
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
			var deployments appsv1.DeploymentList
			if err := admin.List(ctx, &deployments, client.InNamespace(workload), client.MatchingLabels{"kognition.dev/agent-name": current.Name}); err != nil {
				return false, err
			}
			return len(deployments.Items) == 1 && deployments.Items[0].Status.AvailableReplicas == 1, nil
		}); err != nil {
			test.Fatalf("AutoGen worker %s did not become available: %v", current.Name, err)
		}
	}
	var pods corev1.PodList
	if err := admin.List(ctx, &pods, client.InNamespace(workload), client.MatchingLabels{"kognition.dev/agent-name": agent.Name}); err != nil || len(pods.Items) != 1 {
		test.Fatalf("AutoGen Pod count=%d error=%v", len(pods.Items), err)
	}
	pod := &pods.Items[0]
	*created = append(*created, pod.DeepCopy())
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != 1000 || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		test.Fatal("AutoGen worker lacks non-root identity or API-token restriction")
	}
	container := pod.Spec.Containers[0]
	if container.Image != image || len(pod.Status.ContainerStatuses) != 1 || !pod.Status.ContainerStatuses[0].Ready || pod.Status.ContainerStatuses[0].ImageID == "" {
		test.Fatal("AutoGen worker did not resolve the selected immutable image")
	}
	tlsMounted := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == "runtime-control-tls" && mount.MountPath == "/etc/kognition/runtime-control-tls" && mount.ReadOnly {
			tlsMounted = true
		}
	}
	if !tlsMounted || container.StartupProbe == nil || container.StartupProbe.HTTPGet == nil || container.StartupProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS || container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet == nil || container.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
		test.Fatal("generated AutoGen worker lacks read-only TLS delivery or HTTPS probes")
	}
	versions := deployedProducerExec(test, ctx, restConfig, pod, "agent", []string{"python", "-c", `import importlib.metadata as m, importlib.util, json; assert m.version("autogen-agentchat") == "0.7.5"; assert m.version("autogen-ext") == "0.7.5"; assert m.version("pillow") == "13.0.0"; assert importlib.util.find_spec("main").origin == "/app/main.py"; print(json.dumps({"agentchat": m.version("autogen-agentchat"), "ext": m.version("autogen-ext"), "pillow": m.version("pillow"), "source": "/app/main.py"}))`})
	test.Logf("deployed AutoGen Pod UID=%s imageID=%s versions=%s", pod.UID, pod.Status.ContainerStatuses[0].ImageID, strings.TrimSpace(versions))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		test.Fatal("invalid fixture TLS trust")
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		test.Fatal("runtime HTTP transport unavailable")
	}
	transport := baseTransport.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	test.Cleanup(transport.CloseIdleConnections)
	httpClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	endpoint := "https://" + hostname + ":8080"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/healthz", nil)
	if err != nil {
		test.Fatal(err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		test.Fatalf("verified Service-DNS TLS failed: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		test.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 || len(response.TLS.VerifiedChains) == 0 {
		test.Fatal("runtime did not negotiate verified TLS 1.3")
	}
	for _, token := range []string{"", "Bearer sg08-invalid-control-token"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/configuration-check", bytes.NewBufferString("{}"))
		if err != nil {
			test.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", token)
		response, err := httpClient.Do(request)
		if err != nil {
			test.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			test.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized {
			test.Fatalf("unauthorized TLS configuration request returned %d", response.StatusCode)
		}
	}
	plainRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(endpoint, "https://", "http://", 1)+"/healthz", nil)
	if err != nil {
		test.Fatal(err)
	}
	if response, err := httpClient.Do(plainRequest); err == nil {
		if err := response.Body.Close(); err != nil {
			test.Fatal(err)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			test.Fatal("TLS worker accepted plaintext HTTP")
		}
	}
	makeInstance := func(name string, target *api.Agent) *api.AgentInstance {
		test.Helper()
		object := convert.AgentInstanceToK8s(&api.AgentInstance{ObjectMeta: api.ObjectMeta{Name: name, Namespace: workload}, Spec: api.AgentInstanceSpec{
			AgentRef:       api.LocalObjectReference{Name: target.Name},
			SessionContext: map[string]string{"input": "Complete the real AutoGen TLS turn", intctrl.ModelCapabilityIdentityNameContextKey: "sg08-identity", intctrl.AgentDelegationCapabilityIdentityNameContextKey: "sg08-identity"},
		}})
		create(object)
		return convert.AgentInstanceFromK8s(object)
	}
	instance := makeInstance("sg08-autogen-invocation", agent)
	wrongInstance := makeInstance("sg08-autogen-wrong-name", wrongAgent)
	untrusted, _ := admissionServingCertificate(test, "untrusted.test")
	untrustedFile := filepath.Join(directory, "untrusted.pem")
	if err := os.WriteFile(untrustedFile, untrusted, 0o600); err != nil {
		test.Fatal(err)
	}
	wrongCA := config
	wrongCA.RuntimeCAFile = untrustedFile
	for _, negative := range []struct {
		name     string
		config   dispatcher.Config
		instance *api.AgentInstance
	}{{"untrusted CA", wrongCA, instance}, {"wrong Service DNS identity", config, wrongInstance}} {
		if session, err := newProducer(negative.config).Dispatch(ctx, negative.instance); err == nil || session != "" || !strings.Contains(err.Error(), "external runtime configuration check failed") {
			test.Fatalf("%s did not fail closed before SDK execution: session=%q error=%v", negative.name, session, err)
		}
	}
	if count := countCalls(); count != 0 {
		test.Fatalf("TLS/authentication denials triggered %d model effects", count)
	}
	sessionID, err := producer.Dispatch(ctx, instance)
	if err != nil || sessionID == "" {
		test.Fatalf("AutoGen Dispatch session=%q error=%v", sessionID, err)
	}
	instance.Status.SessionID, instance.Status.Phase = sessionID, api.PhaseRunning
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := producer.Terminate(cleanup, instance, "isolated acceptance cleanup"); err != nil {
			test.Errorf("AutoGen session cleanup: %v", err)
		}
	})
	if err := admin.Status().Update(ctx, convert.AgentInstanceToK8s(instance)); err != nil {
		test.Fatal(err)
	}
	var completed intctrl.RuntimeStatus
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		status, err := producer.Status(ctx, instance)
		if err != nil {
			return false, err
		}
		if status.Error != "" {
			return false, fmt.Errorf("AutoGen runtime failed: %s", status.Error)
		}
		completed = status
		return status.Completed, nil
	}); err != nil {
		test.Fatal(err)
	}
	const answer = "SG08 AutoGen TLS dispatch completed"
	if completed.Output != answer || completed.TokenUsage == nil || completed.TokenUsage.PromptTokens != 11 || completed.TokenUsage.CompletionTokens != 7 || completed.TokenUsage.TotalTokens != 18 || countCalls() != 1 {
		test.Fatalf("unexpected AutoGen completion: %+v", completed)
	}
	bearer := deployedProducerExec(test, ctx, restConfig, modelPod, "model", []string{"cat", "/tmp/model-authorization"})
	claims, err := issuer.VerifyClaims(strings.TrimPrefix(bearer, "Bearer "))
	if err != nil || claims.Namespace != workload || claims.AgentName != agent.Name || claims.InstanceUID != instance.UID || claims.SessionID != sessionID || claims.ModelName != model.Name || claims.ModelResourceVersion != convert.ModelFromK8s(model).ResourceVersion {
		test.Fatalf("AutoGen did not forward the exact signed model-session binding: %v", err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		record, err := persistent.GetRuntimeRecovery(ctx, workload, sessionID)
		return record.Phase == store.RuntimeRecoveryTerminal, err
	}); err != nil {
		test.Fatalf("terminal recovery was not persisted by the real Kubernetes backend: %v", err)
	}
	retry := *instance
	retry.Status.Phase = api.PhasePending
	if repeated, err := producer.Dispatch(ctx, &retry); err != nil || repeated != sessionID || countCalls() != 1 {
		test.Fatalf("same-generation AutoGen retry changed session or inference count: original=%q repeated=%q error=%v", sessionID, repeated, err)
	}
	createdMu.Lock()
	retained := append([]*corev1.ConfigMap(nil), retainedRecovery...)
	createdMu.Unlock()
	if len(retained) != 1 {
		test.Fatalf("same-generation retry created %d recovery ledgers, want one", len(retained))
	}
	if err := admin.Delete(ctx, retained[0], client.Preconditions{UID: &retained[0].UID}); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "deletion cannot authorize replay") {
		test.Fatalf("native admission did not protect the terminal recovery ledger from deletion: %v", err)
	}
	for _, current := range []*api.Agent{agent, wrongAgent} {
		if err := producer.CleanupAgentResources(ctx, current.Name, workload); err != nil {
			test.Fatal(err)
		}
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		var workers corev1.PodList
		if err := admin.List(ctx, &workers, client.InNamespace(workload)); err != nil {
			return false, err
		}
		for _, worker := range workers.Items {
			if worker.Labels["kognition.dev/agent-name"] == agent.Name || worker.Labels["kognition.dev/agent-name"] == wrongAgent.Name {
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		test.Fatal(err)
	}
	recovered, err := newProducer(config).Status(ctx, instance)
	if err != nil || !recovered.Completed || recovered.Output != answer || recovered.TokenUsage == nil || recovered.TokenUsage.TotalTokens != 18 || countCalls() != 1 {
		test.Fatalf("AutoGen recovery after dispatcher/worker loss failed: %+v error=%v", recovered, err)
	}
	if err := admin.Delete(ctx, source, client.Preconditions{UID: &source.UID}); err != nil {
		test.Fatal(err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		test.Fatalf("temporary AutoGen source remains: %v", err)
	}
	test.Logf("real Kubernetes AutoGen Service %s UID=%s: verified TLS 1.3; bad CA/name and missing/wrong auth denied before effects; exactly one signed model call; retry and durable terminal recovery after worker cleanup passed; session=%s", hostname, service.UID, sessionID)
}
