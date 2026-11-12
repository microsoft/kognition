//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const authorityProductionRelease = "security-test-kognition"

type gatewayAuthorityDeployment struct {
	bindingKey       []byte
	capabilityKey    []byte
	memoryKey        []byte
	communicationKey []byte
	databaseURL      string
	podName          string
	providerURL      string
	effectCount      func() int32
	effects          func() gatewayAuthorityEffects
	endpoints        []string
	decisionURL      string
	decisionClient   *http.Client
	identityStore    store.Store
	migrations       corev1.ConfigMap
}

type gatewayAuthorityEffects struct {
	Tool          int32 `json:"effects"`
	MemoryStore   int32 `json:"memoryStore"`
	MemoryDelete  int32 `json:"memoryDelete"`
	Communication int32 `json:"communication"`
}

// This non-idempotent provider makes an accidental repeated effect observable.
func TestGatewayAuthorityEffectProvider(test *testing.T) {
	runID := os.Getenv("KOGNITION_AUTHORITY_EFFECT_RUN")
	if runID == "" {
		test.Skip("only runs in the owned acceptance provider Pod")
	}
	parsed, err := uuid.Parse(runID)
	if err != nil || parsed == uuid.Nil || parsed.String() != runID {
		test.Fatal("invalid effect provider run identity")
	}
	var effects, memoryStores, memoryDeletes, communications atomic.Int32
	authorized := func(writer http.ResponseWriter, request *http.Request, environment string) bool {
		token := os.Getenv(environment)
		if token == "" || request.Header.Get("Authorization") != "Bearer "+token {
			http.Error(writer, "synthetic provider authentication denied", http.StatusForbidden)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /effects", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(map[string]any{"runID": runID, "effects": effects.Load(), "memoryStore": memoryStores.Load(), "memoryDelete": memoryDeletes.Load(), "communication": communications.Load()}); err != nil {
			test.Errorf("encode effect evidence: %v", err)
		}
	})
	mux.HandleFunc("GET /v1/memory/health", func(writer http.ResponseWriter, request *http.Request) {
		if authorized(writer, request, "KOGNITION_AUTHORITY_MEMORY_TOKEN") {
			writer.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("POST /v1/memory/store", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(writer, request, "KOGNITION_AUTHORITY_MEMORY_TOKEN") {
			return
		}
		var input struct {
			Volume   string         `json:"volume_handle"`
			Content  string         `json:"content"`
			Metadata map[string]any `json:"metadata"`
		}
		if err := json.NewDecoder(io.LimitReader(request.Body, 64*1024)).Decode(&input); err != nil || input.Volume != authorityMemoryHandle || input.Content != "owned acceptance memory" ||
			request.Header.Get(memory.TrustedWorkspaceHeader) != "tenant" || !strings.HasPrefix(request.Header.Get(memory.TrustedAgentIdentityHeader), "spiffe://kognition.dev/workspace/tenant/agent/authority-http-agent/") {
			http.Error(writer, "invalid synthetic Memory identity or input", http.StatusBadRequest)
			return
		}
		count := memoryStores.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(writer).Encode(map[string]any{"id": fmt.Sprintf("authority-record-%d", count), "content": input.Content, "metadata": input.Metadata}); err != nil {
			test.Errorf("write Memory effect evidence: %v", err)
		}
	})
	mux.HandleFunc("DELETE /v1/memory/delete/"+authorityMemoryHandle+"/authority-record-1", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(writer, request, "KOGNITION_AUTHORITY_MEMORY_TOKEN") {
			return
		}
		memoryDeletes.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(writer, `{"deleted":true}`); err != nil {
			test.Errorf("write Memory deletion evidence: %v", err)
		}
	})
	mux.HandleFunc("POST /invoke", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(writer, request, "KOGNITION_AUTHORITY_RUNTIME_TOKEN") {
			return
		}
		var input struct {
			Session string `json:"session_id"`
			Message string `json:"message"`
		}
		if err := json.NewDecoder(io.LimitReader(request.Body, 64*1024)).Decode(&input); err != nil || !strings.HasPrefix(input.Session, "authority-communication-") || input.Message != "owned acceptance communication" {
			http.Error(writer, "invalid synthetic Communication input", http.StatusBadRequest)
			return
		}
		communications.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		if _, err := io.WriteString(writer, `{"accepted":true}`); err != nil {
			test.Errorf("write Communication effect evidence: %v", err)
		}
	})
	mux.HandleFunc("POST /tool", func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(io.LimitReader(request.Body, 64*1024))
		if err != nil || !json.Valid(body) {
			http.Error(writer, "invalid synthetic tool input", http.StatusBadRequest)
			return
		}
		effects.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(writer, `{"updated":true}`); err != nil {
			test.Errorf("write effect response: %v", err)
		}
	})
	server := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	test.Fatal(server.ListenAndServe())
}

func TestGatewayAuthorityProductionToolExecution(test *testing.T) {
	fixture := newGatewayAuthorityFixtureWithTimeout(test, 9*time.Minute)
	deployment := fixture.installProductionAssembly(test, false)
	runGatewayAuthorityProductionTool(test, fixture, deployment)
}

func runGatewayAuthorityProductionTool(test *testing.T, fixture *gatewayAuthorityFixture, deployment *gatewayAuthorityDeployment) {
	test.Helper()
	pods := fixture.productionPods(test, "tool-gateway", 2)
	deployment.podName = pods[0].Name
	for _, pod := range pods {
		deployment.endpoints = append(deployment.endpoints, "http://"+net.JoinHostPort(pod.Status.PodIP, "8092"))
		test.Logf("Actual Tool process Pod=%s UID=%s containerID=%s imageID=%s", pod.Name, pod.UID, pod.Status.ContainerStatuses[0].ContainerID, pod.Status.ContainerStatuses[0].ImageID)
	}
	credential := fixture.boundToken(test, &pods[0])
	runGatewayAuthorityToolHTTPWithDeployment(test, fixture, credential, deployment)
	objects, err := client.New(fixture.config, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	var approvals k8sapi.RuntimeApprovalList
	if err := objects.List(fixture.context, &approvals, client.InNamespace("tenant")); err != nil {
		test.Fatal(err)
	}
	if len(approvals.Items) != 1 {
		test.Fatalf("expected one retained production Tool approval, got %d", len(approvals.Items))
	}
	approval := approvals.Items[0]
	if approval.Status.Claimant == nil {
		test.Fatal("production effect has no retained claimant")
	}
	matched := false
	for _, pod := range fixture.productionPods(test, "tool-gateway", 2) {
		for _, started := range pods {
			if pod.UID == started.UID && pod.Status.ContainerStatuses[0].ContainerID == started.Status.ContainerStatuses[0].ContainerID &&
				approval.Status.Claimant.WorkloadUID == string(pod.UID) && approval.Status.Claimant.ContainerID == pod.Status.ContainerStatuses[0].ContainerID {
				matched = true
			}
		}
	}
	if !matched {
		test.Fatal("retained claim does not identify an unchanged actual gateway container")
	}
	test.Logf("Production Tool receipt approval=%s UID=%s claimantPodUID=%s fence=%d state=%s effects=%d", approval.Name, approval.UID, approval.Status.Claimant.WorkloadUID, approval.Status.FencingToken, approval.Status.ExecutionState, deployment.effectCount())
}

func (fixture *gatewayAuthorityFixture) installProductionAssembly(test *testing.T, full bool) *gatewayAuthorityDeployment {
	test.Helper()
	ctx := fixture.context
	for _, namespace := range []string{"control", "tenant", "outside"} {
		fixture.create(test, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"kognition.dev/authority-acceptance-run": fixture.runID}}})
	}
	manifest, err := os.ReadFile(os.Getenv("KOGNITION_AUTHORITY_MANIFEST"))
	if err != nil {
		test.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var resources []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			test.Fatal(err)
		}
		if len(object.Object) != 0 {
			resources = append(resources, object)
		}
	}
	inventory, err := os.ReadFile(filepath.Join(filepath.Dir(os.Getenv("KOGNITION_AUTHORITY_MANIFEST")), "authority-crd-inventory.json"))
	if err != nil {
		test.Fatal(err)
	}
	var expectedDefinitions []string
	if err := json.Unmarshal(inventory, &expectedDefinitions); err != nil || len(expectedDefinitions) == 0 {
		test.Fatal("explicit chart CRD inventory required", err)
	}
	expectedCRDs := make(map[string]bool, len(expectedDefinitions))
	for _, name := range expectedDefinitions {
		if expectedCRDs[name] {
			test.Fatal("duplicate expected CRD", name)
		}
		expectedCRDs[name] = true
	}
	crds := 0
	for _, object := range resources {
		if object.GetKind() == "CustomResourceDefinition" {
			if !expectedCRDs[object.GetName()] {
				test.Fatal("unexpected or duplicate chart CRD", object.GetName())
			}
			delete(expectedCRDs, object.GetName())
			fixture.create(test, object)
			crds++
		}
	}
	if crds != len(expectedDefinitions) || len(expectedCRDs) != 0 {
		test.Fatalf("incomplete chart CRDs: got=%d expected=%d missing=%v", crds, len(expectedDefinitions), expectedCRDs)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		test.Fatal(err)
	}
	configuration, err := json.Marshal([]map[string]any{{"version": 1, "epoch": 1, "algorithm": runtimeapproval.BindingAlgorithmV1, "domain": runtimeapproval.BindingDomainV1, "status": runtimeapproval.BindingKeyActive, "material": base64.StdEncoding.EncodeToString(key)}})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := runtimeapproval.BindingKeyStoreFromJSON(string(configuration)); err != nil {
		test.Fatal("production binding-key configuration", err)
	}
	fixture.create(test, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "authority-runtime-auth", Namespace: "control"}, Data: map[string][]byte{"runtime-approval-binding-keys.json": configuration, "runtime-control-token": []byte(uuid.NewString() + uuid.NewString())}})
	ca, certificate, privateKey := productionAuthorityTLS(test)
	fixture.create(test, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authorityProductionRelease + "-webhook-tls", Namespace: "control"}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: certificate, corev1.TLSPrivateKeyKey: privateKey}})
	fixture.create(test, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authorityProductionRelease + "-admission-guard-tls", Namespace: "control"}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: certificate, corev1.TLSPrivateKeyKey: privateKey}})
	var deployments, webhooks []*unstructured.Unstructured
	var migrations corev1.ConfigMap
	policies, bindings := 0, 0
	for _, object := range resources {
		selected := false
		switch object.GetKind() {
		case "ConfigMap":
			if object.GetName() == authorityProductionRelease+"-migrations" {
				encoded, err := json.Marshal(object.Object)
				if err != nil {
					test.Fatal(err)
				}
				if err := json.Unmarshal(encoded, &migrations); err != nil {
					test.Fatal(err)
				}
			}
		case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "Secret", "Service", "NetworkPolicy":
			for _, component := range []string{"webhook", "controller", "admission-guard", "tool-gateway", "memory-policy-gateway", "agent-communication-gateway", "model-gateway"} {
				prefix := authorityProductionRelease + "-" + component
				selected = selected || object.GetName() == prefix || strings.HasPrefix(object.GetName(), prefix+"-")
			}
		case "ValidatingAdmissionPolicy":
			selected = true
			policies++
		case "ValidatingAdmissionPolicyBinding":
			selected = true
			bindings++
		case "Deployment":
			if object.GetName() == authorityProductionRelease+"-webhook" || object.GetName() == authorityProductionRelease+"-admission-guard" || object.GetName() == authorityProductionRelease+"-tool-gateway" ||
				full && (object.GetName() == authorityProductionRelease+"-memory-policy-gateway" || object.GetName() == authorityProductionRelease+"-agent-communication-gateway") {
				deployments = append(deployments, object)
			}
		case "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration":
			webhooks = append(webhooks, object)
		}
		if selected {
			fixture.create(test, object)
		}
	}
	expectedDeployments := 3
	if full {
		expectedDeployments = 5
	}
	if policies != 9 || bindings != 9 || len(deployments) != expectedDeployments || len(webhooks) != 3 {
		test.Fatalf("incomplete production assembly: policies=%d bindings=%d deployments=%d webhooks=%d", policies, bindings, len(deployments), len(webhooks))
	}
	for _, object := range resources {
		if object.GetKind() != "ValidatingAdmissionPolicy" {
			continue
		}
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 45*time.Second, true, func(ctx context.Context) (bool, error) {
			policy, err := fixture.admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, object.GetName(), metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			if policy.Status.TypeChecking == nil || policy.Status.ObservedGeneration != policy.Generation {
				return false, nil
			}
			if len(policy.Status.TypeChecking.ExpressionWarnings) != 0 {
				return false, fmt.Errorf("production policy %s has typechecking warnings", policy.Name)
			}
			return true, nil
		}); err != nil {
			test.Fatal(err)
		}
	}
	databaseURL := ""
	if full {
		databaseURL = fixture.installProductionDatabase(test)
	}
	providerURL, effects := fixture.installProductionEffectProvider(test)
	if full {
		fixture.create(test, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: authorityProductionRelease + "-memory-service", Namespace: "control"}, Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/component": "authority-effect-provider", "kognition.dev/authority-acceptance-run": fixture.runID},
			Ports:    []corev1.ServicePort{{Port: 8090, TargetPort: intstr.FromInt32(8080)}},
		}})
	}
	for _, object := range deployments {
		fixture.create(test, object)
	}
	fixture.productionPods(test, "resource-webhooks", 2)
	fixture.productionPods(test, "admission-guard", 2)
	fixture.productionPods(test, "tool-gateway", 2)
	if full {
		fixture.productionPods(test, "memory-policy-gateway", 2)
		fixture.productionPods(test, "agent-communication-gateway", 2)
	}
	// Provision local trust in the same caBundle fields populated by a CA injector.
	for _, object := range webhooks {
		items, found, err := unstructured.NestedSlice(object.Object, "webhooks")
		if err != nil || !found {
			test.Fatal("production webhook configuration is missing", err)
		}
		for _, item := range items {
			hook, ok := item.(map[string]any)
			if !ok || hook["failurePolicy"] != "Fail" {
				test.Fatal("production acceptance requires every resource webhook to fail closed")
			}
			if err := unstructured.SetNestedField(hook, base64.StdEncoding.EncodeToString(ca), "clientConfig", "caBundle"); err != nil {
				test.Fatal(err)
			}
		}
		if err := unstructured.SetNestedSlice(object.Object, items, "webhooks"); err != nil {
			test.Fatal(err)
		}
		fixture.create(test, object)
	}
	fixture.installProductionOperator(test)
	// Seed trusted identities without activating the controller or granting its workload permissions.
	fixture.create(test, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "authority-fixture-identity", Namespace: "tenant"}, Rules: []rbacv1.PolicyRule{{
		APIGroups: []string{"kognition.dev"}, Resources: []string{"agentidentities", "agentidentities/status"}, Verbs: []string{"get", "create", "update", "patch"},
	}}})
	fixture.create(test, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "authority-fixture-identity", Namespace: "tenant"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "authority-fixture-identity"},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "control", Name: authorityProductionRelease + "-controller"}},
	})
	for _, resource := range []string{"pods", "secrets", "leases"} {
		group := ""
		if resource == "leases" {
			group = "coordination.k8s.io"
		}
		review, err := fixture.admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User: "system:serviceaccount:control:" + authorityProductionRelease + "-controller", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "tenant", Verb: "create", Group: group, Resource: resource},
		}}, metav1.CreateOptions{})
		if err != nil || review.Status.Allowed {
			test.Fatalf("fixture identity provisioning must not activate controller %s authority: %v", resource, err)
		}
	}
	identityConfig := rest.CopyConfig(fixture.config)
	identityConfig.Impersonate.UserName = "system:serviceaccount:control:" + authorityProductionRelease + "-controller"
	identityConfig.Impersonate.Groups = []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"}
	identityClient, err := client.New(identityConfig, client.Options{Scheme: k8scontroller.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	secret, err := fixture.admin.CoreV1().Secrets("control").Get(ctx, authorityProductionRelease+"-tool-gateway", metav1.GetOptions{})
	if err != nil {
		test.Fatal(err)
	}
	if string(secret.Data["capability-hmac-key-id"]) != "active" || len(secret.Data["capability-hmac-key"]) < 32 {
		test.Fatal("production Tool capability key configuration is not usable")
	}
	webhookService, err := fixture.admin.CoreV1().Services("control").Get(ctx, authorityProductionRelease+"-webhook", metav1.GetOptions{})
	if err != nil {
		test.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		test.Fatal("invalid local acceptance CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: authorityProductionRelease + "-webhook.control.svc", MinVersion: tls.VersionTLS12}}
	test.Cleanup(transport.CloseIdleConnections)
	deployment := &gatewayAuthorityDeployment{
		bindingKey: key, capabilityKey: secret.Data["capability-hmac-key"], providerURL: providerURL, effectCount: func() int32 { return effects().Tool }, effects: effects, databaseURL: databaseURL, migrations: migrations,
		identityStore:  k8scontroller.NewStoreAdapterWithControlNamespace(identityClient, "control"),
		decisionURL:    "https://" + net.JoinHostPort(webhookService.Spec.ClusterIP, "443"),
		decisionClient: &http.Client{Transport: transport, Timeout: 20 * time.Second},
	}
	if full {
		for _, component := range []string{"memory-policy-gateway", "agent-communication-gateway"} {
			current, err := fixture.admin.CoreV1().Secrets("control").Get(ctx, authorityProductionRelease+"-"+component, metav1.GetOptions{})
			if err != nil {
				test.Fatal(err)
			}
			if string(current.Data["capability-hmac-key-id"]) != "active" || len(current.Data["capability-hmac-key"]) < 32 {
				test.Fatal("production capability key unavailable", component)
			}
			if component == "memory-policy-gateway" {
				deployment.memoryKey = current.Data["capability-hmac-key"]
			} else {
				deployment.communicationKey = current.Data["capability-hmac-key"]
			}
		}
	}
	return deployment
}

func (fixture *gatewayAuthorityFixture) productionPods(test *testing.T, component string, count int) []corev1.Pod {
	test.Helper()
	var ready []corev1.Pod
	if err := wait.PollUntilContextTimeout(fixture.context, 500*time.Millisecond, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		pods, err := fixture.admin.CoreV1().Pods("control").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=" + component})
		if err != nil {
			return false, err
		}
		ready = nil
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" || len(pod.Status.ContainerStatuses) != 1 {
				continue
			}
			status := pod.Status.ContainerStatuses[0]
			if status.Ready && status.State.Running != nil && status.ContainerID != "" {
				ready = append(ready, pod)
			}
		}
		return len(pods.Items) == count && len(ready) == count, nil
	}); err != nil {
		diagnosticContext, cancel := context.WithTimeout(context.WithoutCancel(fixture.context), 10*time.Second)
		defer cancel()
		pods, listErr := fixture.admin.CoreV1().Pods("control").List(diagnosticContext, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=" + component})
		if listErr != nil {
			test.Logf("Read %s readiness diagnostics: %v", component, listErr)
		} else {
			for _, pod := range pods.Items {
				test.Logf("%s Pod=%s phase=%s status=%+v", component, pod.Name, pod.Status.Phase, pod.Status.ContainerStatuses)
				log, logErr := fixture.admin.CoreV1().Pods("control").GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: ptr.To(int64(30))}).DoRaw(diagnosticContext)
				if logErr != nil {
					test.Logf("Read %s logs: %v", pod.Name, logErr)
				} else {
					test.Logf("%s logs: %s", pod.Name, log)
				}
			}
		}
		test.Fatalf("production %s replicas not ready: %v", component, err)
	}
	return ready
}

func (fixture *gatewayAuthorityFixture) installProductionOperator(test *testing.T) {
	test.Helper()
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

func (fixture *gatewayAuthorityFixture) installProductionEffectProvider(test *testing.T) (string, func() gatewayAuthorityEffects) {
	test.Helper()
	name := "authority-effect-provider"
	labels := map[string]string{"app.kubernetes.io/component": name, "kognition.dev/authority-acceptance-run": fixture.runID}
	fixture.create(test, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "control"}, Spec: appsv1.DeploymentSpec{
		Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				AutomountServiceAccountToken: ptr.To(false),
				Volumes:                      []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
				Containers: []corev1.Container{{
					Name: name, Image: "local/kognition-authority-provider:" + fixture.runID, ImagePullPolicy: corev1.PullNever,
					Args: []string{"-test.run=^TestGatewayAuthorityEffectProvider$", "-test.timeout=15m", "-test.v"},
					Env: []corev1.EnvVar{
						{Name: "KOGNITION_AUTHORITY_EFFECT_RUN", Value: fixture.runID},
						{Name: "KOGNITION_AUTHORITY_RUNTIME_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "authority-runtime-auth"}, Key: "runtime-control-token"}}},
						{Name: "KOGNITION_AUTHORITY_MEMORY_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: authorityProductionRelease + "-memory-policy-gateway"}, Key: "upstream-token"}}},
					},
					Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
					VolumeMounts:   []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}}, PeriodSeconds: 1},
				}},
			},
		},
	}})
	fixture.create(test, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "control"}, Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 8080}}}})
	pods := fixture.productionPods(test, name, 1)
	endpoint := "http://" + net.JoinHostPort(pods[0].Status.PodIP, "8080")
	effects := func() gatewayAuthorityEffects {
		test.Helper()
		request, err := http.NewRequestWithContext(fixture.context, http.MethodGet, endpoint+"/effects", nil)
		if err != nil {
			test.Fatal(err)
		}
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			test.Fatal("read provider evidence", err)
		}
		defer func() { _ = response.Body.Close() }()
		var evidence struct {
			RunID string `json:"runID"`
			gatewayAuthorityEffects
		}
		if response.StatusCode != http.StatusOK {
			test.Fatalf("provider evidence status=%d", response.StatusCode)
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&evidence); err != nil || evidence.RunID != fixture.runID {
			test.Fatal("provider evidence identity mismatch", err)
		}
		current := fixture.productionPods(test, name, 1)
		if current[0].UID != pods[0].UID || current[0].Status.ContainerStatuses[0].ContainerID != pods[0].Status.ContainerStatuses[0].ContainerID {
			test.Fatal("provider restarted; its in-memory effect count is not authoritative")
		}
		return evidence.gatewayAuthorityEffects
	}
	if effects() != (gatewayAuthorityEffects{}) {
		test.Fatal("new production fixture already has effects")
	}
	test.Logf("Non-idempotent provider PodUID=%s containerID=%s", pods[0].UID, pods[0].Status.ContainerStatuses[0].ContainerID)
	return "http://" + name + ".control.svc:8080/tool", effects
}

func productionAuthorityTLS(test *testing.T) ([]byte, []byte, []byte) {
	test.Helper()
	caPublic, caPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "owned authority acceptance CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(6 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	ca, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		test.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		test.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: caTemplate.NotBefore, NotAfter: caTemplate.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{
		authorityProductionRelease + "-webhook.control.svc", authorityProductionRelease + "-webhook.control.svc.cluster.local",
		authorityProductionRelease + "-admission-guard.control.svc", authorityProductionRelease + "-admission-guard.control.svc.cluster.local",
	}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, caTemplate, public, caPrivate)
	if err != nil {
		test.Fatal(err)
	}
	encodedKey, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		test.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encodedKey})
}

func TestGatewayAuthorityProductionTLS(test *testing.T) {
	ca, certificate, key := productionAuthorityTLS(test)
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		test.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		test.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		test.Fatal("acceptance CA is invalid")
	}
	options := x509.VerifyOptions{Roots: roots, DNSName: authorityProductionRelease + "-webhook.control.svc"}
	if _, err := leaf.Verify(options); err != nil {
		test.Fatal("production Service certificate is not trusted", err)
	}
	options.DNSName = authorityProductionRelease + "-webhook.outside.svc"
	if _, err := leaf.Verify(options); err == nil {
		test.Fatal("acceptance certificate trusts a different namespace")
	}
}
