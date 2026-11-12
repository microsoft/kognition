//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intadmission "github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/controller"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8swebhook "github.com/kognition-ai/kognition/internal/k8s/webhook"
	"github.com/kognition-ai/kognition/internal/opa"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/toolgateway"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	webhookadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type gatewayAuthorityFixture struct {
	context     context.Context
	admin       *kubernetes.Clientset
	config      *rest.Config
	objects     client.Client
	runID       string
	pods        map[string]*corev1.Pod
	gateways    map[string]*kubernetes.Clientset
	credentials map[string]*rest.Config
}

func newGatewayAuthorityFixture(test *testing.T) *gatewayAuthorityFixture {
	return newGatewayAuthorityFixtureWithTimeout(test, 4*time.Minute)
}

func newGatewayAuthorityFixtureWithTimeout(test *testing.T, timeout time.Duration) *gatewayAuthorityFixture {
	test.Helper()
	runID := os.Getenv("KOGNITION_AUTHORITY_RUN_ID")
	if runID == "" {
		test.Skip("requires an explicitly owned disposable authority target")
	}
	parsed, err := uuid.Parse(runID)
	if err != nil || parsed == uuid.Nil || parsed.String() != runID {
		test.Fatal("invalid acceptance run identity")
	}
	config, err := clientcmd.BuildConfigFromFlags("https://127.0.0.1:6443", os.Getenv("KOGNITION_AUTHORITY_KUBECONFIG"))
	if err != nil {
		test.Fatal(err)
	}
	config.Timeout = 10 * time.Second
	admin, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	test.Cleanup(cancel)
	nodes, err := admin.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		test.Fatal(err)
	}
	root, err := admin.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		test.Fatal(err)
	}
	version, err := admin.Discovery().ServerVersion()
	if err != nil {
		test.Fatal(err)
	}
	if os.Getenv("KOGNITION_AUTHORITY_CLUSTER_UID") == "" || string(root.UID) != os.Getenv("KOGNITION_AUTHORITY_CLUSTER_UID") || len(nodes.Items) != 1 ||
		nodes.Items[0].Name != "kog-auth-"+strings.ReplaceAll(runID, "-", "")+"-control-plane" ||
		nodes.Items[0].Labels["kognition.dev/authority-acceptance-run"] != runID || version.GitVersion != os.Getenv("KOGNITION_ADMISSION_EXPECTED_VERSION") {
		test.Fatal("isolated target UID, node identity or Kubernetes version mismatch")
	}
	objects, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	fixture := &gatewayAuthorityFixture{context: ctx, admin: admin, config: config, objects: objects, runID: runID, pods: map[string]*corev1.Pod{}, gateways: map[string]*kubernetes.Clientset{}, credentials: map[string]*rest.Config{}}
	test.Logf("Owned authority run=%s clusterUID=%s server=%s", runID, root.UID, version.GitVersion)
	return fixture
}

func (fixture *gatewayAuthorityFixture) create(test *testing.T, object client.Object) {
	test.Helper()
	if err := fixture.objects.Create(fixture.context, object); err != nil {
		test.Fatal(err)
	}
	test.Logf("Retained %T %s/%s UID=%s", object, object.GetNamespace(), object.GetName(), object.GetUID())
}

func (fixture *gatewayAuthorityFixture) installGatewayLeaseBoundary(test *testing.T) {
	test.Helper()
	for _, namespace := range []string{"control", "tenant", "outside"} {
		fixture.create(test, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}})
	}
	manifest, err := os.ReadFile(os.Getenv("KOGNITION_AUTHORITY_MANIFEST"))
	if err != nil {
		test.Fatal(err)
	}
	test.Logf("Chart manifest SHA256=%x", sha256.Sum256(manifest))
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	accounts, policies := 0, 0
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			test.Fatal(err)
		}
		if object.Object == nil {
			continue
		}
		selected := false
		switch object.GetKind() {
		case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
			for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
				prefix := "security-test-kognition-" + component
				selected = selected || object.GetName() == prefix || strings.HasPrefix(object.GetName(), prefix+"-")
			}
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			selected = object.GetName() == "security-test-kognition-control-gateway-leases"
		}
		if !selected {
			continue
		}
		fixture.create(test, &object)
		if object.GetKind() == "ServiceAccount" {
			accounts++
		}
		if object.GetKind() == "ValidatingAdmissionPolicy" || object.GetKind() == "ValidatingAdmissionPolicyBinding" {
			policies++
		}
	}
	if accounts != 3 || policies != 2 {
		test.Fatalf("incomplete rendered authority boundary: accounts=%d policies=%d", accounts, policies)
	}
	policyName := "security-test-kognition-control-gateway-leases"
	if err := wait.PollUntilContextTimeout(fixture.context, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		policy, err := fixture.admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, policyName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if len(policy.Status.TypeChecking.ExpressionWarnings) != 0 {
			return false, fmt.Errorf("gateway Lease policy typechecking: %v", policy.Status.TypeChecking.ExpressionWarnings)
		}
		return true, nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
		account := "security-test-kognition-" + component
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: component, Namespace: "control", Labels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}, Spec: corev1.PodSpec{ServiceAccountName: account, Containers: []corev1.Container{{Name: component, Image: "registry.k8s.io/pause:3.10", ImagePullPolicy: corev1.PullNever}}}}
		fixture.create(test, pod)
		if err := wait.PollUntilContextTimeout(fixture.context, 200*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
			current, err := fixture.admin.CoreV1().Pods("control").Get(ctx, pod.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if current.UID != pod.UID {
				return false, fmt.Errorf("fixture Pod replaced")
			}
			if len(current.Status.ContainerStatuses) != 1 || current.Status.ContainerStatuses[0].State.Running == nil || current.Status.ContainerStatuses[0].ContainerID == "" {
				return false, nil
			}
			fixture.pods[component] = current
			return true, nil
		}); err != nil {
			test.Fatal(err)
		}
		token, err := fixture.admin.CoreV1().ServiceAccounts("control").CreateToken(fixture.context, account, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(600)), BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
		if err != nil {
			test.Fatal(err)
		}
		bound := rest.AnonymousClientConfig(fixture.config)
		bound.BearerToken = token.Status.Token
		gateway, err := kubernetes.NewForConfig(bound)
		if err != nil {
			test.Fatal(err)
		}
		identity, err := gateway.AuthenticationV1().SelfSubjectReviews().Create(fixture.context, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err != nil {
			test.Fatal(err)
		}
		extra := identity.Status.UserInfo.Extra
		if identity.Status.UserInfo.Username != "system:serviceaccount:control:"+account || len(extra["authentication.kubernetes.io/pod-uid"]) != 1 || extra["authentication.kubernetes.io/pod-uid"][0] != string(pod.UID) || len(extra["authentication.kubernetes.io/pod-name"]) != 1 || extra["authentication.kubernetes.io/pod-name"][0] != pod.Name {
			test.Fatal("real token did not authenticate the expected Pod-bound identity")
		}
		fixture.gateways[component] = gateway
		fixture.credentials[component] = bound
		test.Logf("Verified actual Pod-bound token component=%s PodUID=%s containerID=%s", component, pod.UID, fixture.pods[component].Status.ContainerStatuses[0].ContainerID)
	}
}

func TestGatewayAuthorityRuntimeApprovalAdmission(test *testing.T) {
	fixture := newGatewayAuthorityFixture(test)
	ctx := fixture.context
	objects, err := client.New(fixture.config, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	fixture.objects = objects
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
		pod, err := fixture.admin.CoreV1().Pods("control").Get(ctx, component, metav1.GetOptions{})
		if err != nil || pod.Labels["kognition.dev/authority-acceptance-run"] != fixture.runID || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Running == nil {
			test.Fatal("retained gateway Pod unavailable", err)
		}
		fixture.pods[component] = pod
		fixture.credentials[component] = fixture.boundToken(test, pod)
	}
	crdBytes, err := os.ReadFile(os.Getenv("KOGNITION_AUTHORITY_APPROVAL_CRD"))
	if err != nil {
		test.Fatal(err)
	}
	crd, err := decodeAuthorityApprovalCRD(crdBytes)
	if err != nil {
		test.Fatal(err)
	}
	fixture.create(test, crd)
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		var current unstructured.Unstructured
		current.SetAPIVersion(crd.GetAPIVersion())
		current.SetKind(crd.GetKind())
		if err := fixture.objects.Get(ctx, client.ObjectKey{Name: crd.GetName()}, &current); err != nil {
			return false, err
		}
		conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
		for _, condition := range conditions {
			value := condition.(map[string]any)
			if value["type"] == "Established" && value["status"] == "True" {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, account := range []string{"authority-controller", "authority-human"} {
		fixture.create(test, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: "control"}})
		fixture.create(test, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: "tenant"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"kognition.dev"}, Resources: []string{"runtimeapprovals", "runtimeapprovals/status"}, Verbs: []string{"get", "create", "update"}}}})
		fixture.create(test, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: "tenant"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: "control"}}})
		token, err := fixture.admin.CoreV1().ServiceAccounts("control").CreateToken(ctx, account, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(600))}}, metav1.CreateOptions{})
		if err != nil {
			test.Fatal(err)
		}
		configuration := rest.AnonymousClientConfig(fixture.config)
		configuration.BearerToken = token.Status.Token
		fixture.credentials[account] = configuration
	}
	if os.Getenv("KOGNITION_AUTHORITY_GATEWAY_HTTP") == "true" {
		fixture.installGatewayHTTPDefinitions(test)
	}
	backend := k8scontroller.NewStoreAdapterWithControlNamespace(fixture.objects, "control")
	writers := map[string]string{}
	for component, plane := range map[string]string{"tool-gateway": "Tool", "memory-policy-gateway": "Memory", "agent-communication-gateway": "Communication"} {
		writers["system:serviceaccount:control:security-test-kognition-"+component] = plane
	}
	validator := intadmission.NewRuntimeApprovalValidator("system:serviceaccount:control:authority-controller", "system:serviceaccount:control:security-test-kognition-tool-gateway", "system:serviceaccount:control:security-test-kognition-memory-policy-gateway", "system:serviceaccount:control:security-test-kognition-agent-communication-gateway").WithDecisionAuthority("system:serviceaccount:control:authority-human").WithExecutionWriters(writers).WithClaimantReader(backend).WithTerminationReader(backend)
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	server := httptest.NewTLSServer(&webhookadmission.Webhook{Handler: k8swebhook.NewHandler(chain, k8scontroller.SetupScheme(), zap.NewNop())})
	defer server.Close()
	configuration := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "authority-runtime-approval"}, Webhooks: []admissionv1.ValidatingWebhook{{Name: "authority.kognition.test", AdmissionReviewVersions: []string{"v1"}, SideEffects: ptr.To(admissionv1.SideEffectClassNone), FailurePolicy: ptr.To(admissionv1.Fail), TimeoutSeconds: ptr.To(int32(5)), ClientConfig: admissionv1.WebhookClientConfig{URL: ptr.To(server.URL), CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}, NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}, Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{"kognition.dev"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"runtimeapprovals", "runtimeapprovals/status"}, Scope: ptr.To(admissionv1.NamespacedScope)}}}}}}
	fixture.create(test, configuration)
	clients := map[string]client.Client{}
	for name, credential := range fixture.credentials {
		authenticated, err := client.New(credential, client.Options{Scheme: k8scontroller.SetupScheme()})
		if err != nil {
			test.Fatal(err)
		}
		clients[name] = authenticated
	}
	deny := func(err error) {
		test.Helper()
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "authority.kognition.test") {
			test.Fatalf("want production RuntimeApproval webhook denial, got %v", err)
		}
	}
	write := func(writer string, object *k8sapi.RuntimeApproval, status api.RuntimeApprovalStatus) error {
		test.Helper()
		candidate := object.DeepCopy()
		if err := clients[writer].Get(ctx, client.ObjectKeyFromObject(object), candidate); err != nil {
			return err
		}
		encoded, err := json.Marshal(status)
		if err != nil {
			return err
		}
		candidate.Status = k8sapi.RuntimeApprovalStatus{}
		if err := json.Unmarshal(encoded, &candidate.Status); err != nil {
			return err
		}
		if err := clients[writer].Status().Update(ctx, candidate); err != nil {
			return err
		}
		*object = *candidate
		return nil
	}
	newApproval := func(invocation, plane, initial string) (*k8sapi.RuntimeApproval, api.RuntimeApprovalStatus) {
		test.Helper()
		now := time.Now().UTC().Truncate(time.Second)
		ref := func(kind, scope, namespace string) api.RuntimeApprovalReference {
			return api.RuntimeApprovalReference{Kind: kind, Scope: scope, Namespace: namespace, Name: strings.ToLower(kind), UID: kind + "-uid", ResourceVersion: 1}
		}
		target := "Tool"
		if plane == "Memory" {
			target = "MemoryVolume"
		}
		if plane == "Communication" {
			target = "Agent"
		}
		approval := api.RuntimeApproval{ObjectMeta: api.ObjectMeta{Name: runtimeapproval.CanonicalName("tenant", invocation), Namespace: "tenant", Finalizers: []string{k8sapi.RuntimeApprovalCompactionFinalizer}}, Spec: api.RuntimeApprovalSpec{Workspace: "tenant", InvocationID: invocation, InitialDecision: initial, Plane: plane, Operation: "invoke", SessionRef: "session", AgentRef: ref("Agent", "Namespaced", "tenant"), AgentIdentityRef: ref("AgentIdentity", "Namespaced", "tenant"), AgentInstanceRef: ref("AgentInstance", "Namespaced", "tenant"), TargetRef: ref(target, "Namespaced", "tenant"), AutonomyClassRef: ref("AutonomyClass", "Cluster", ""), Binding: api.RuntimeActionBinding{Format: "kognition-runtime-action-v1", Algorithm: "HMAC-SHA-256", Domain: "kognition.dev/runtime-approval/action/v1", KeyVersion: 1, Epoch: 1, Tag: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
		encoded, err := json.Marshal(approval)
		if err != nil {
			test.Fatal(err)
		}
		var object k8sapi.RuntimeApproval
		if err := json.Unmarshal(encoded, &object); err != nil {
			test.Fatal(err)
		}
		object.TypeMeta = metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "RuntimeApproval"}
		status := runtimeapproval.InitialStatus(initial)
		return &object, status
	}
	probe, _ := newApproval("webhook-probe", "Tool", "Pending")
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		err := fixture.objects.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "authority.kognition.test"), nil
	}); err != nil {
		test.Fatal("fail-closed webhook did not become enforced", err)
	}
	var recoveries []struct {
		name string
		run  func(*testing.T)
	}
	for _, scenario := range []struct{ component, plane string }{{"tool-gateway", "Tool"}, {"memory-policy-gateway", "Memory"}, {"agent-communication-gateway", "Communication"}} {
		test.Run(scenario.component, func(test *testing.T) {
			pending, status := newApproval("human-"+scenario.component, scenario.plane, "Pending")
			if err := clients[scenario.component].Create(ctx, pending); err != nil {
				test.Fatal("legitimate approval creation", err)
			}
			if err := write(scenario.component, pending, status); err != nil {
				test.Fatal("legitimate pending initialization", err)
			}
			decided := time.Now().UTC().Truncate(time.Second)
			forged := api.RuntimeApproval{ObjectMeta: api.ObjectMeta{UID: string(pending.UID)}, Spec: api.RuntimeApprovalSpec{Workspace: "tenant", InvocationID: pending.Spec.InvocationID}, Status: status}
			forged.Status.DecisionState = "Approved"
			forged.Status.DecidedAt = &decided
			forged.Status.DecidedByIssuer = "kubernetes.io/tokenreview"
			forged.Status.DecidedBySubject = "human-operator"
			runtimeapproval.PopulateDecisionAudit(&forged)
			deny(write(scenario.component, pending, forged.Status))
			if err := write("authority-human", pending, forged.Status); err != nil {
				test.Fatal("dedicated human decision", err)
			}
			status = forged.Status
			status.DecisionEvidenceDelivered = true
			if err := write(scenario.component, pending, status); err != nil {
				test.Fatal("legitimate decision acknowledgement", err)
			}
			deadline := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
			status.PreparationToken = strings.Repeat("a", 43)
			status.PreparationDeadline = &deadline
			if err := write(scenario.component, pending, status); err != nil {
				test.Fatal("legitimate preparation", err)
			}
			pod := fixture.pods[scenario.component]
			claimed := status
			claimed.ExecutionState = "Claimed"
			claimed.PreparationToken = ""
			claimed.PreparationDeadline = nil
			claimed.FencingToken = runtimeapproval.MinFencingToken
			claimed.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: string(pod.UID), ContainerID: pod.Status.ContainerStatuses[0].ContainerID, ProcessStartNonce: uuid.NewString(), ClaimedAt: decided, LastHeartbeat: decided}
			wrongContainer := claimed
			copyClaim := *claimed.Claimant
			copyClaim.ContainerID = "containerd://retired-container"
			wrongContainer.Claimant = &copyClaim
			deny(write(scenario.component, pending, wrongContainer))
			deny(write("authority-controller", pending, claimed))
			if err := write(scenario.component, pending, claimed); err != nil {
				test.Fatal("legitimate real-container claim", err)
			}
			completed := claimed
			completed.ExecutionState = "Completed"
			completed.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
			foreignPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: scenario.component + "-foreign", Namespace: "control", Labels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}, Spec: *pod.Spec.DeepCopy()}
			fixture.create(test, foreignPod)
			foreign, err := client.New(fixture.boundToken(test, foreignPod), client.Options{Scheme: k8scontroller.SetupScheme()})
			if err != nil {
				test.Fatal(err)
			}
			clients["foreign-"+scenario.component] = foreign
			deny(write("foreign-"+scenario.component, pending, completed))
			changedClaim := completed
			changedOwner := *completed.Claimant
			changedOwner.ProcessStartNonce = uuid.NewString()
			changedClaim.Claimant = &changedOwner
			deny(write(scenario.component, pending, changedClaim))
			for _, other := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway", "authority-controller", "authority-human"} {
				if other != scenario.component {
					deny(write(other, pending, completed))
				}
			}
			uncertain := claimed
			uncertain.ExecutionState = "Uncertain"
			uncertain.TerminationProofRef = "fabricated-proof"
			deny(write(scenario.component, pending, uncertain))
			deny(write("authority-controller", pending, uncertain))
			if err := write(scenario.component, pending, completed); err != nil {
				test.Fatal("legitimate owning-container completion", err)
			}
			deny(write(scenario.component, pending, uncertain))
			test.Logf("Retained approval UID=%s claimant=%s fence=%d; human forgery, cross-plane, foreign-Pod, dead-container, controller finalization/recovery denied", pending.UID, pod.UID, claimed.FencingToken)
			recoveries = append(recoveries, struct {
				name string
				run  func(*testing.T)
			}{name: scenario.component, run: func(test *testing.T) {
				recovery, automatic := newApproval("recovery-"+scenario.component, scenario.plane, "NotRequired")
				if err := clients[scenario.component].Create(ctx, recovery); err != nil {
					test.Fatal("automatic approval create", err)
				}
				automatic.DecidedAt = &decided
				audit := api.RuntimeApproval{ObjectMeta: api.ObjectMeta{UID: string(recovery.UID)}, Spec: api.RuntimeApprovalSpec{Workspace: "tenant", InvocationID: recovery.Spec.InvocationID}, Status: automatic}
				runtimeapproval.PopulateDecisionAudit(&audit)
				automatic = audit.Status
				if err := write(scenario.component, recovery, automatic); err != nil {
					test.Fatal("automatic evidence initialization", err)
				}
				automatic.DecisionEvidenceDelivered = true
				if err := write(scenario.component, recovery, automatic); err != nil {
					test.Fatal(err)
				}
				automatic.PreparationToken = strings.Repeat("b", 43)
				automatic.PreparationDeadline = &deadline
				if err := write(scenario.component, recovery, automatic); err != nil {
					test.Fatal(err)
				}
				automatic.PreparationToken = ""
				automatic.PreparationDeadline = nil
				automatic.ExecutionState = "Claimed"
				automatic.FencingToken = runtimeapproval.MinFencingToken
				automatic.Claimant = claimed.Claimant
				if err := write(scenario.component, recovery, automatic); err != nil {
					test.Fatal("automatic real-container claim", err)
				}
				if proof, err := backend.RuntimeApprovalTerminationProof(ctx, *automatic.Claimant); err == nil || proof != "" {
					test.Fatal("termination proof issued for live claimant")
				}
				if err := fixture.admin.CoreV1().Pods("control").Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}}); err != nil {
					test.Fatal(err)
				}
				var proof string
				if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
					value, err := backend.RuntimeApprovalTerminationProof(ctx, *automatic.Claimant)
					if err != nil {
						return false, nil
					}
					proof = value
					return proof != "", nil
				}); err != nil {
					test.Fatal("authoritative claimant absence unavailable", err)
				}
				recovered := automatic
				recovered.ExecutionState = "Uncertain"
				recovered.TerminationProofRef = proof + "-mismatch"
				deny(write("authority-controller", recovery, recovered))
				recovered.TerminationProofRef = proof
				if err := write("authority-controller", recovery, recovered); err != nil {
					test.Fatal("proof-bound controller uncertainty", err)
				}
				reconciled := recovered
				reconciled.ExecutionState = "Failed"
				reconciled.ExecutionDisposition = runtimeapproval.ExecutionDispositionReconciledNotExecuted
				reconciled.ReconciledByIssuer = "kubernetes.io/tokenreview"
				reconciled.ReconciledBySubject = "human-operator"
				reconciled.ReconciliationEvidenceRef = "fixture://no-provider-operation/" + scenario.component
				deny(write("authority-controller", recovery, reconciled))
				if err := write("authority-human", recovery, reconciled); err != nil {
					test.Fatal("evidence-bound human reconciliation", err)
				}
				test.Logf("Retained recovery UID=%s proof=%s; authoritative uncertainty and human NotExecuted reconciliation passed without provider execution", recovery.UID, proof)
			}})
		})
	}
	if os.Getenv("KOGNITION_AUTHORITY_GATEWAY_HTTP") == "true" {
		test.Run("tool-http-concurrent-execution", func(test *testing.T) { runGatewayAuthorityToolHTTP(test, fixture, fixture.credentials["tool-gateway"]) })
	}
	for _, recovery := range recoveries {
		test.Run(recovery.name+"-recovery", recovery.run)
	}
}

func (fixture *gatewayAuthorityFixture) boundToken(test *testing.T, pod *corev1.Pod) *rest.Config {
	test.Helper()
	token, err := fixture.admin.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(fixture.context, pod.Spec.ServiceAccountName, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(600)), BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}, metav1.CreateOptions{})
	if err != nil {
		test.Fatal(err)
	}
	configuration := rest.AnonymousClientConfig(fixture.config)
	configuration.BearerToken = token.Status.Token
	return configuration
}

func decodeAuthorityApprovalCRD(data []byte) (*unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object.Object); err != nil {
			return nil, fmt.Errorf("read RuntimeApproval CRD: %w", err)
		}
		if object.Object == nil {
			continue
		}
		if object.GetKind() != "CustomResourceDefinition" || object.GetName() != "runtimeapprovals.kognition.dev" {
			return nil, fmt.Errorf("unexpected RuntimeApproval CRD")
		}
		return &object, nil
	}
}

func TestGatewayAuthorityApprovalCRDDocument(test *testing.T) {
	data, err := os.ReadFile("../../deploy/helm/kognition/crds/kognition.dev_runtimeapprovals.yaml")
	if err != nil {
		test.Fatal(err)
	}
	object, err := decodeAuthorityApprovalCRD(data)
	if err != nil {
		test.Fatal(err)
	}
	if object.GetAPIVersion() != "apiextensions.k8s.io/v1" || object.GetName() != "runtimeapprovals.kognition.dev" {
		test.Fatal("wrong chart CRD selected")
	}
}

func TestGatewayAuthorityToolHTTPConcurrentExecution(test *testing.T) {
	fixture := newGatewayAuthorityFixture(test)
	configuration := os.Getenv("KOGNITION_AUTHORITY_HTTP_KUBECONFIG")
	if configuration == "" {
		test.Fatal("explicit gateway credential kubeconfig required")
	}
	credential, err := clientcmd.BuildConfigFromFlags("", configuration)
	if err != nil {
		test.Fatal(err)
	}
	runGatewayAuthorityToolHTTP(test, fixture, credential)
}

func runGatewayAuthorityToolHTTP(test *testing.T, fixture *gatewayAuthorityFixture, credential *rest.Config) {
	runGatewayAuthorityToolHTTPWithDeployment(test, fixture, credential, nil)
}

func runGatewayAuthorityToolHTTPWithDeployment(test *testing.T, fixture *gatewayAuthorityFixture, credential *rest.Config, deployment *gatewayAuthorityDeployment) {
	test.Helper()
	ctx := fixture.context
	adminClient, err := client.New(fixture.config, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	adminStore := k8scontroller.NewStoreAdapterWithControlNamespace(adminClient, "control")
	gatewayClient, err := client.New(credential, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	storage := k8scontroller.NewStoreAdapterWithControlNamespace(gatewayClient, "control")
	if err := storage.ConfigureRuntimeApprovalClock(30*time.Second, k8scontroller.NewHostClockHealthCheck(30*time.Second)); err != nil {
		test.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		test.Fatal(err)
	}
	if deployment != nil {
		key = deployment.bindingKey
	}
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{Version: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: key}})
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
		test.Fatal(err)
	}
	podName := "tool-gateway"
	if deployment != nil {
		podName = deployment.podName
	}
	pod, err := fixture.admin.CoreV1().Pods("control").Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		test.Fatal(err)
	}
	claimant, err := k8scontroller.ResolveRuntimeApprovalClaimant(ctx, gatewayClient, "control", pod.Name, "tool-gateway", string(pod.UID))
	if err != nil {
		test.Fatal(err)
	}
	claimant.ProcessStartNonce = uuid.NewString()
	capabilityKey := key
	if deployment != nil {
		capabilityKey = deployment.capabilityKey
	}
	sessionIssuer, err := runtimeauth.NewToolSessionCapabilityIssuer(capabilityKey, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	toolIssuer, err := runtimeauth.NewToolCapabilityIssuer(capabilityKey, time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	var effects atomic.Int32
	providerURL := ""
	effectCount := effects.Load
	if deployment == nil {
		provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			effects.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"updated":true}`)
		}))
		defer provider.Close()
		providerURL = provider.URL
	} else {
		providerURL, effectCount = deployment.providerURL, deployment.effectCount
	}
	seed := func(kind, name string, value any) {
		test.Helper()
		var seedStore store.Store = adminStore
		if deployment != nil && kind == "AgentIdentity" {
			seedStore = deployment.identityStore
		}
		seedGatewayAuthorityResource(test, ctx, seedStore, kind, "tenant", name, value)
	}
	metadata := func(name string) api.ObjectMeta {
		return api.ObjectMeta{Name: name, Namespace: "tenant", Labels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}
	}
	seed("Agent", "authority-http-agent", api.Agent{ObjectMeta: metadata("authority-http-agent"), Spec: api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "no-inference-fixture"}, Instructions: api.AgentInstructions{Inline: "deterministic authority fixture"}}})
	var agent api.Agent
	encoded, err := adminStore.Get(ctx, "Agent", "tenant", "authority-http-agent")
	if err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &agent); err != nil {
		test.Fatal(err)
	}
	identityMetadata := metadata("agent-identity-authority-http-agent")
	identityMetadata.Labels["kognition.dev/auto-created-for-agent"] = agent.Name
	identityMetadata.OwnerReferences = []api.OwnerReference{{APIVersion: api.APIVersion, Kind: "Agent", Name: agent.Name, UID: agent.UID, Controller: true}}
	seed("AgentIdentity", identityMetadata.Name, api.AgentIdentity{ObjectMeta: identityMetadata, Spec: api.AgentIdentitySpec{AgentRef: api.LocalObjectReference{Name: agent.Name}, SPIFFEID: "spiffe://kognition.dev/workspace/tenant/agent/" + agent.Name + "/incarnation/" + agent.UID}, Status: api.AgentIdentityStatus{Phase: api.PhaseReady}})
	seed("AgentInstance", "authority-http-instance", api.AgentInstance{ObjectMeta: metadata("authority-http-instance"), Spec: api.AgentInstanceSpec{AgentRef: api.LocalObjectReference{Name: agent.Name}}, Status: api.AgentInstanceStatus{Phase: api.PhaseRunning, SessionID: "authority-http-session"}})
	seed("Tool", "authority-http-tool", api.Tool{ObjectMeta: metadata("authority-http-tool"), Spec: api.ToolSpec{ClassName: "sandbox-openapi", Protocol: "OpenAPI", Endpoint: providerURL, Metadata: map[string]string{"allowPrivateNetwork": "true"}, InputSchema: map[string]any{"type": "object"}}, Status: api.ToolStatus{Phase: api.PhaseReady}})
	seed("ToolGrant", "authority-http-grant", api.ToolGrant{ObjectMeta: metadata("authority-http-grant"), Spec: api.ToolGrantSpec{AgentRef: api.LocalObjectReference{Name: agent.Name}, ToolRef: api.LocalObjectReference{Name: "authority-http-tool"}, Verbs: []api.ToolGrantVerb{"invoke"}}})
	seed("AgentRole", "authority-http-role", api.AgentRole{ObjectMeta: metadata("authority-http-role"), Rules: []api.PolicyRule{{Resources: []string{"Tool"}, Verbs: []string{"get", "invoke"}}}})
	seed("AgentRoleBinding", "authority-http-role", api.AgentRoleBinding{ObjectMeta: metadata("authority-http-role"), Spec: api.AgentRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: "authority-http-role"}, Subjects: []api.Subject{{Kind: "AgentIdentity", Name: identityMetadata.Name}}}})
	var tool api.Tool
	body, err := storage.Get(ctx, "Tool", "tenant", "authority-http-tool")
	if err != nil {
		test.Fatal("prepared HTTP Tool fixture unavailable", err)
	}
	if err := json.Unmarshal(body, &tool); err != nil {
		test.Fatal(err)
	}
	tool.Spec.Endpoint = providerURL
	body, err = json.Marshal(tool)
	if err != nil {
		test.Fatal(err)
	}
	if err := adminStore.Update(ctx, "Tool", "tenant", tool.Name, body, tool.ResourceVersion); err != nil {
		test.Fatal(err)
	}
	body, err = storage.Get(ctx, "Tool", "tenant", tool.Name)
	if err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(body, &tool); err != nil {
		test.Fatal(err)
	}
	endpoints := make([]string, 0, 2)
	if deployment == nil {
		for replicaIndex := range 2 {
			replicaCredential := credential
			replicaClaimant := claimant
			if replicaIndex == 1 {
				replicaPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "tool-gateway-replica", Namespace: "control", Labels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}, Spec: *pod.Spec.DeepCopy()}
				fixture.create(test, replicaPod)
				if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
					current, err := fixture.admin.CoreV1().Pods("control").Get(ctx, replicaPod.Name, metav1.GetOptions{})
					if err != nil {
						return false, err
					}
					if current.UID != replicaPod.UID {
						return false, fmt.Errorf("gateway replica Pod replaced")
					}
					return len(current.Status.ContainerStatuses) == 1 && current.Status.ContainerStatuses[0].State.Running != nil, nil
				}); err != nil {
					test.Fatal(err)
				}
				replicaCredential = fixture.boundToken(test, replicaPod)
			}
			replicaClient, err := client.New(replicaCredential, client.Options{Scheme: k8scontroller.SetupScheme()})
			if err != nil {
				test.Fatal(err)
			}
			if replicaIndex == 1 {
				replicaClaimant, err = k8scontroller.ResolveRuntimeApprovalClaimant(ctx, replicaClient, "control", "tool-gateway-replica", "tool-gateway", string(mustGatewayAuthorityPod(test, fixture, "tool-gateway-replica").UID))
				if err != nil {
					test.Fatal(err)
				}
				replicaClaimant.ProcessStartNonce = uuid.NewString()
			}
			replicaStore := k8scontroller.NewStoreAdapterWithControlNamespace(replicaClient, "control")
			if err := replicaStore.ConfigureToolInvocationReceiptWriter("tool-gateway"); err != nil {
				test.Fatal(err)
			}
			if err := replicaStore.ConfigureRuntimeApprovalClock(30*time.Second, k8scontroller.NewHostClockHealthCheck(30*time.Second)); err != nil {
				test.Fatal(err)
			}
			if err := replicaStore.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
				test.Fatal(err)
			}
			coordinator, err := runtimeapproval.NewGate3Coordinator(runtimeapproval.Gate3Config{Store: replicaStore, Resolver: runtimeapproval.NewResolver(replicaStore), Keys: keys, TTL: time.Minute})
			if err != nil {
				test.Fatal(err)
			}
			policies := controller.NewInvocationPolicyEngine(controller.NewToolGrantEngine(replicaStore, zap.NewNop()), controller.NewAgentPolicyReconciler(replicaStore, opa.NewEngine(), zap.NewNop())).WithAgentRBAC(replicaStore, controller.NewRBACEngine(replicaStore, zap.NewNop()))
			if _, err := controller.NewToolGatewayAuthorizer(policies).AuthorizeToolInvocation(ctx, "tenant", agent.Name, tool.Name, map[string]any{"ticket": "42"}); err != nil {
				test.Fatalf("live Gate 2 fixture authorization: %v", err)
			}
			executor := toolexec.New(nil)
			core, err := toolgateway.New(replicaStore, controller.NewToolGatewayAuthorizer(policies), controller.NewQuotaEnforcer(replicaStore, controller.NewCognitiveQuotaReconciler(replicaStore, zap.NewNop()), zap.NewNop()), toolgateway.NewInvocationRunExecutor(replicaStore, runtime.NewToolDispatcher(executor, executor)), zap.NewNop(), toolgateway.WithLedger(toolgateway.NewStoreLedger(replicaStore)), toolgateway.WithGate3Coordinator(coordinator), toolgateway.WithGate3RequestBuilder(toolgateway.NewGate3RequestBuilder(replicaStore)), toolgateway.WithRuntimeApprovalExecutionStore(replicaStore), toolgateway.WithClaimant(replicaClaimant))
			if err != nil {
				test.Fatal(err)
			}
			handler, err := toolgateway.NewServer(replicaStore, sessionIssuer, toolIssuer, core, zaptest.NewLogger(test))
			if err != nil {
				test.Fatal(err)
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			endpoints = append(endpoints, server.URL)
		}
	} else {
		endpoints = deployment.endpoints
	}
	var instance api.AgentInstance
	body, err = storage.Get(ctx, "AgentInstance", "tenant", "authority-http-instance")
	if err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(body, &instance); err != nil {
		test.Fatal(err)
	}
	session, err := sessionIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{Namespace: "tenant", AgentName: "authority-http-agent", AgentIdentityName: identityMetadata.Name, InstanceName: instance.Name, InstanceUID: instance.UID, SessionID: instance.Status.SessionID})
	if err != nil {
		test.Fatal(err)
	}
	invocation := "authority-http-" + uuid.NewString()
	payload, err := json.Marshal(map[string]any{"tool_name": tool.Name, "tool_resource_version": tool.ResourceVersion, "invocation_id": invocation, "input": map[string]any{"ticket": "42"}})
	if err != nil {
		test.Fatal(err)
	}
	post := func(endpoint, path, token string) (int, []byte, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		return response.StatusCode, body, err
	}
	code, body, err := post(endpoints[0], "/v1/tools/capabilities", session)
	if err != nil || code != http.StatusOK {
		test.Fatalf("real capability exchange: status=%d body=%s err=%v", code, body, err)
	}
	var exchange struct {
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal(body, &exchange); err != nil || exchange.Capability == "" {
		test.Fatal("capability exchange result unavailable", err)
	}
	code, body, err = post(endpoints[0], "/v1/tools/invoke", exchange.Capability)
	if err != nil || code != http.StatusAccepted || effectCount() != 0 {
		test.Fatalf("pending action reached provider: status=%d effects=%d body=%s err=%v", code, effectCount(), body, err)
	}
	approvalBytes, err := storage.Get(ctx, "RuntimeApproval", "tenant", runtimeapproval.CanonicalName("tenant", invocation))
	if err != nil {
		test.Fatal(err)
	}
	var approval api.RuntimeApproval
	if err := json.Unmarshal(approvalBytes, &approval); err != nil {
		test.Fatal(err)
	}
	decisionURL := ""
	var decisionClient *http.Client
	if deployment == nil {
		humanClient, err := client.New(fixture.credentials["authority-human"], client.Options{Scheme: k8scontroller.SetupScheme()})
		if err != nil {
			test.Fatal(err)
		}
		humanStore := k8scontroller.NewStoreAdapterWithControlNamespace(humanClient, "control")
		humanStore.ConfigureRuntimeApprovalBindingKeys(keys)
		if err := humanStore.ConfigureRuntimeApprovalClock(30*time.Second, k8scontroller.NewHostClockHealthCheck(30*time.Second)); err != nil {
			test.Fatal(err)
		}
		reviews, err := kubernetes.NewForConfig(fixture.credentials["authority-human"])
		if err != nil {
			test.Fatal(err)
		}
		decisionServer := httptest.NewTLSServer(k8swebhook.NewRuntimeApprovalDecisionHandler(reviews.AuthenticationV1().TokenReviews(), reviews.AuthorizationV1().SubjectAccessReviews(), humanStore, "system:serviceaccount:control:security-test-kognition-tool-gateway"))
		defer decisionServer.Close()
		decisionURL, decisionClient = decisionServer.URL, decisionServer.Client()
	} else {
		decisionURL, decisionClient = deployment.decisionURL, deployment.decisionClient
	}
	decisionPayload, err := json.Marshal(map[string]any{"namespace": "tenant", "name": approval.Name, "invocationID": invocation, "decision": "approve", "expectedResourceVersion": approval.ResourceVersion})
	if err != nil {
		test.Fatal(err)
	}
	decide := func(token string) int {
		test.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, decisionURL+k8swebhook.RuntimeApprovalDecisionPath, bytes.NewReader(decisionPayload))
		if err != nil {
			test.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := decisionClient.Do(request)
		if err != nil {
			test.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			test.Fatal(err)
		}
		if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusForbidden {
			test.Fatalf("decision status=%d body=%s", response.StatusCode, body)
		}
		return response.StatusCode
	}
	if decide(credential.BearerToken) != http.StatusForbidden || effectCount() != 0 {
		test.Fatal("gateway self-approved or reached provider")
	}
	if decide(fixture.credentials["authority-operator"].BearerToken) != http.StatusOK {
		test.Fatal("authorized operator decision denied")
	}
	var group sync.WaitGroup
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, endpoint := range endpoints {
		group.Add(1)
		go func(endpoint string) {
			defer group.Done()
			<-start
			code, _, err := post(endpoint, "/v1/tools/invoke", exchange.Capability)
			if err != nil {
				results <- 0
				return
			}
			results <- code
		}(endpoint)
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for code := range results {
		if code == http.StatusOK {
			successes++
		} else if code != http.StatusConflict && code != http.StatusServiceUnavailable {
			test.Fatalf("concurrent invocation status=%d providerEffects=%d", code, effectCount())
		}
	}
	if successes == 0 || effectCount() != 1 {
		test.Fatalf("concurrent provider effects=%d successes=%d", effectCount(), successes)
	}
	code, _, err = post(endpoints[1], "/v1/tools/invoke", exchange.Capability)
	if err != nil || code != http.StatusOK || effectCount() != 1 {
		test.Fatalf("durable replay: status=%d effects=%d err=%v", code, effectCount(), err)
	}
	test.Logf("HTTP capability exchange, two concurrent gateway instances and durable replay: invocation=%s providerEffects=%d", invocation, effectCount())
	retained, err := adminStore.Get(ctx, "RuntimeApproval", "tenant", runtimeapproval.CanonicalName("tenant", invocation))
	if err != nil {
		test.Fatal("durable execution approval unavailable", err)
	}
	var completed api.RuntimeApproval
	if err := json.Unmarshal(retained, &completed); err != nil {
		test.Fatal(err)
	}
	if completed.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted || completed.Status.FencingToken < runtimeapproval.MinFencingToken || completed.Status.Claimant == nil {
		test.Fatalf("provider effect lacks a retained terminal owner/fence: %+v", completed.Status)
	}
}

func (fixture *gatewayAuthorityFixture) installGatewayHTTPDefinitions(test *testing.T) {
	test.Helper()
	directory := os.Getenv("KOGNITION_AUTHORITY_CRD_DIRECTORY")
	if directory == "" {
		test.Fatal("explicit HTTP fixture CRD directory required")
	}
	for _, plural := range []string{"agents", "agentidentities", "agentinstances", "agentpolicies", "agentroles", "agentrolebindings", "tools", "toolgrants", "toolinvocationruns", "cognitivequotas", "trustprofiles", "trustprofileindices", "autonomyclasses", "runtimeapprovaltombstones", "runtimeapprovalquotatickets"} {
		data, err := os.ReadFile(filepath.Join(directory, "kognition.dev_"+plural+".yaml"))
		if err != nil {
			test.Fatal(err)
		}
		decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var object unstructured.Unstructured
			if err := decoder.Decode(&object.Object); err != nil {
				test.Fatal(err)
			}
			if object.Object == nil {
				continue
			}
			if object.GetKind() != "CustomResourceDefinition" || object.GetName() != plural+".kognition.dev" {
				test.Fatal("unexpected HTTP fixture CRD")
			}
			fixture.create(test, &object)
			break
		}
	}
	fixture.create(test, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "authority-human-reviews"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}}, {APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}}}})
	fixture.create(test, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "authority-human-reviews"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "authority-human-reviews"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "authority-human", Namespace: "control"}}})
	fixture.create(test, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "authority-human-binding-reader", Namespace: "control"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{"kognition-runtime-approval-binding-keys"}, Verbs: []string{"get"}}}})
	fixture.create(test, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "authority-human-binding-reader", Namespace: "control"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "authority-human-binding-reader"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "authority-human", Namespace: "control"}}})
	fixture.create(test, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "authority-operator", Namespace: "control"}})
	fixture.create(test, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "authority-operator", Namespace: "tenant"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"kognition.dev"}, Resources: []string{"runtimeapprovals/decision"}, Verbs: []string{"approve"}}}})
	fixture.create(test, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "authority-operator", Namespace: "tenant"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "authority-operator"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "authority-operator", Namespace: "control"}}})
	token, err := fixture.admin.CoreV1().ServiceAccounts("control").CreateToken(fixture.context, "authority-operator", &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: ptr.To(int64(600))}}, metav1.CreateOptions{})
	if err != nil {
		test.Fatal(err)
	}
	configuration := rest.AnonymousClientConfig(fixture.config)
	configuration.BearerToken = token.Status.Token
	fixture.credentials["authority-operator"] = configuration
}

func mustGatewayAuthorityPod(test *testing.T, fixture *gatewayAuthorityFixture, name string) *corev1.Pod {
	test.Helper()
	pod, err := fixture.admin.CoreV1().Pods("control").Get(fixture.context, name, metav1.GetOptions{})
	if err != nil {
		test.Fatal(err)
	}
	return pod
}

func TestGatewayAuthorityHTTPReadOnlyDiagnostics(test *testing.T) {
	fixture := newGatewayAuthorityFixture(test)
	pod := mustGatewayAuthorityPod(test, fixture, "tool-gateway-replica")
	credential := fixture.boundToken(test, pod)
	reader, err := client.New(credential, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	backend := k8scontroller.NewStoreAdapterWithControlNamespace(reader, "control")
	if err := backend.ConfigureRuntimeApprovalClock(30*time.Second, k8scontroller.NewHostClockHealthCheck(30*time.Second)); err != nil {
		test.Fatal(err)
	}
	if err := backend.RuntimeApprovalClockReady(); err != nil {
		test.Errorf("real host clock prerequisite: %v", err)
	} else {
		test.Log("real host clock prerequisite passed")
	}
	if _, err := runtimeapproval.NewResolver(backend).Resolve(fixture.context, "tenant", "authority-http-agent"); err != nil {
		test.Errorf("live autonomy policy prerequisite: %v", err)
	} else {
		test.Log("live autonomy policy prerequisite passed")
	}
	cluster, err := kubernetes.NewForConfig(credential)
	if err != nil {
		test.Fatal(err)
	}
	for _, resource := range []struct{ group, name, verb, namespace string }{{"kognition.dev", "runtimeapprovals", "create", "tenant"}, {"kognition.dev", "runtimeapprovalquotatickets", "create", "tenant"}, {"coordination.k8s.io", "leases", "create", "tenant"}, {"kognition.dev", "trustprofileindices", "get", "tenant"}, {"", "configmaps", "create", "control"}} {
		review, err := cluster.AuthorizationV1().SelfSubjectAccessReviews().Create(fixture.context, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Group: resource.group, Resource: resource.name, Verb: resource.verb, Namespace: resource.namespace}}}, metav1.CreateOptions{})
		if err != nil {
			test.Fatal(err)
		}
		test.Logf("rendered RBAC %s %s/%s namespace=%s allowed=%t", resource.verb, resource.group, resource.name, resource.namespace, review.Status.Allowed)
	}
	var approvals k8sapi.RuntimeApprovalList
	admin, err := client.New(fixture.config, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	if err := admin.List(fixture.context, &approvals, client.InNamespace("tenant")); err != nil {
		test.Fatal(err)
	}
	for _, approval := range approvals.Items {
		if strings.HasPrefix(approval.Spec.InvocationID, "authority-http-") {
			test.Logf("retained HTTP approval UID=%s decision=%s execution=%s claimantPresent=%t", approval.UID, approval.Status.DecisionState, approval.Status.ExecutionState, approval.Status.Claimant != nil)
		}
	}
}

func TestGatewayAuthorityBoundTokenLeaseAdmission(test *testing.T) {
	fixture := newGatewayAuthorityFixture(test)
	fixture.installGatewayLeaseBoundary(test)
	ctx := fixture.context
	owned := func(namespace, name string) *coordinationv1.Lease {
		return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}}
	}
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
		test.Run(component, func(test *testing.T) {
			gateway := fixture.gateways[component]
			assertPolicyDenial := func(err error) {
				test.Helper()
				if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "security-test-kognition-control-gateway-leases") {
					test.Fatalf("want independent gateway Lease admission denial, got %v", err)
				}
			}
			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
				_, err := gateway.CoordinationV1().Leases("control").Create(ctx, owned("control", "unreserved-probe"), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
				return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "security-test-kognition-control-gateway-leases"), nil
			}); err != nil {
				test.Fatal("native policy did not become enforced", err)
			}
			leases := gateway.CoordinationV1().Leases("tenant")
			lease, err := leases.Get(ctx, "kognition-runtime-approval-records", metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				lease, err = leases.Create(ctx, owned("tenant", "kognition-runtime-approval-records"), metav1.CreateOptions{})
			}
			if err != nil {
				test.Fatal("legitimate tenant lock", err)
			}
			lease.Spec.HolderIdentity = ptr.To(component)
			if _, err = leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
				test.Fatal("legitimate tenant lock renewal", err)
			}
			for _, namespace := range []string{"control", "tenant", "outside"} {
				attributes := &authorizationv1.ResourceAttributes{Namespace: namespace, Group: "coordination.k8s.io", Resource: "leases", Verb: "delete", Name: lease.Name}
				review, err := gateway.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attributes}}, metav1.CreateOptions{})
				if err != nil || review.Status.Allowed {
					test.Fatalf("gateway Lease delete authority: review=%+v err=%v", review, err)
				}
			}
			for _, verb := range []string{"approve", "reject", "reconcile"} {
				review, err := gateway.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "tenant", Group: "kognition.dev", Resource: "runtimeapprovals", Verb: verb}}}, metav1.CreateOptions{})
				if err != nil || review.Status.Allowed {
					test.Fatalf("gateway human %s authority: review=%+v err=%v", verb, review, err)
				}
			}
			for _, candidate := range []*coordinationv1.Lease{owned("outside", "kognition-runtime-approval-records"), owned("tenant", "kognition-runtime-approval-binding-keys"), owned("tenant", "foreign-leader")} {
				_, err := gateway.CoordinationV1().Leases(candidate.Namespace).Create(ctx, candidate, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
				assertPolicyDenial(err)
			}
			for _, scenario := range []string{"unowned", "helm"} {
				seed := owned("control", "kognition-usage-lock-"+scenario+"-"+component)
				if scenario == "unowned" {
					seed.Labels = nil
				} else {
					seed.Annotations = map[string]string{"meta.helm.sh/release-name": "foreign"}
				}
				fixture.create(test, seed)
				seed.Labels = map[string]string{"app.kubernetes.io/managed-by": "kognition"}
				seed.Annotations = nil
				seed.Spec.HolderIdentity = ptr.To(component)
				_, err := gateway.CoordinationV1().Leases("control").Update(ctx, seed, metav1.UpdateOptions{})
				assertPolicyDenial(err)
				retained, err := fixture.admin.CoordinationV1().Leases("control").Get(ctx, seed.Name, metav1.GetOptions{})
				if err != nil || retained.UID != seed.UID || retained.Spec.HolderIdentity != nil {
					test.Fatal("denied takeover changed retained Lease", err)
				}
			}
			_, err = gateway.CoordinationV1().Leases("control").Create(ctx, owned("control", "kognition-tool-grant-rate-"+component), metav1.CreateOptions{})
			if err != nil {
				test.Fatal("legitimate control-plane coordination", err)
			}
			err = leases.Delete(ctx, lease.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(types.UID(lease.UID))}})
			if !apierrors.IsForbidden(err) {
				test.Fatal("gateway deleted coordination Lease", err)
			}
			released, err := leases.Get(ctx, lease.Name, metav1.GetOptions{})
			if err != nil || released.UID != lease.UID || released.Spec.HolderIdentity == nil || *released.Spec.HolderIdentity != component {
				test.Fatal("test coordination holder changed before release", err)
			}
			released.Spec.HolderIdentity = ptr.To("")
			if _, err := leases.Update(ctx, released, metav1.UpdateOptions{}); err != nil {
				test.Fatal("release owned test coordination holder", err)
			}
			test.Log("Real Pod-bound gateway credential: legitimate locks passed; human verbs, foreign scope, adoption and deletion denied")
		})
	}
	var policy admissionv1.ValidatingAdmissionPolicy
	if err := fixture.objects.Get(ctx, client.ObjectKey{Name: "security-test-kognition-control-gateway-leases"}, &policy); err != nil {
		test.Fatal(err)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionv1.Fail {
		test.Fatal("gateway Lease policy is not fail-closed")
	}
}
