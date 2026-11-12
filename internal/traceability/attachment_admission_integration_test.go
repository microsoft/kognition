// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/admissionguard"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestIndependentAttachmentAdmission(test *testing.T) {
	runAttachmentAdmission(test, false)
}

func TestCombinedAttachmentAdmission(test *testing.T) {
	runAttachmentAdmission(test, true)
}

func runAttachmentAdmission(test *testing.T, nativePolicies bool) {
	test.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme, rbacv1.AddToScheme, admissionregistrationv1.AddToScheme} {
		if err := register(scheme); err != nil {
			test.Fatal(err)
		}
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	for _, namespace := range []string{"control", "tenant", "outside"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			test.Fatal(err)
		}
	}
	for _, namespace := range []string{"control", "tenant", "outside"} {
		if err := admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace}}); err != nil {
			test.Fatal(err)
		}
	}
	const controllerName = "system:serviceaccount:control:security-test-kognition-controller"
	const guardName = "system:serviceaccount:control:security-test-kognition-admission-guard"
	impersonated := func(username string) *rest.Config {
		copy := rest.CopyConfig(config)
		copy.Impersonate = rest.ImpersonationConfig{UserName: username, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"}}
		return copy
	}
	guardReader, err := client.New(impersonated(guardName), client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	handler := &admissionguard.AttachHandler{Reader: guardReader, ControllerUsername: controllerName, WorkloadNamespaces: map[string]bool{"tenant": true}, BaselinePolicyName: "security-test-kognition-stdio-baseline", ReleaseName: "security-test", InstallationNamespace: "control"}
	var allowedConnections atomic.Int32
	server := httptest.NewTLSServer(&admission.Webhook{Handler: admission.HandlerFunc(func(ctx context.Context, request admission.Request) admission.Response {
		response := handler.Handle(ctx, request)
		if request.Operation == admissionv1.Connect && response.Allowed {
			allowedConnections.Add(1)
		}
		return response
	})})
	defer server.Close()
	var webhookConfig *admissionregistrationv1.ValidatingWebhookConfiguration
	var attachmentRole *rbacv1.Role
	for _, document := range renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		var metadata metav1.TypeMeta
		if err := json.Unmarshal(document.Raw, &metadata); err != nil {
			test.Fatal(err)
		}
		var object client.Object
		switch metadata.Kind {
		case "Role":
			object = &rbacv1.Role{}
		case "RoleBinding":
			object = &rbacv1.RoleBinding{}
		case "ValidatingAdmissionPolicy":
			if !nativePolicies {
				continue
			}
			object = &admissionregistrationv1.ValidatingAdmissionPolicy{}
		case "ValidatingAdmissionPolicyBinding":
			if !nativePolicies {
				continue
			}
			object = &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		case "ValidatingWebhookConfiguration":
			var candidate admissionregistrationv1.ValidatingWebhookConfiguration
			if err := json.Unmarshal(document.Raw, &candidate); err != nil {
				test.Fatal(err)
			}
			if candidate.Name != "security-test-kognition-admission-guard" {
				continue
			}
			for index := range candidate.Webhooks {
				candidate.Webhooks[index].ClientConfig = admissionregistrationv1.WebhookClientConfig{URL: ptr.To(server.URL + "/guard"), CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}
			}
			webhookConfig = &candidate
			continue
		default:
			continue
		}
		if err := json.Unmarshal(document.Raw, object); err != nil {
			test.Fatal(err)
		}
		if err := admin.Create(ctx, object); err != nil {
			test.Fatal(err)
		}
		if role, ok := object.(*rbacv1.Role); ok && role.Namespace == "tenant" {
			for _, rule := range role.Rules {
				for _, resource := range rule.Resources {
					if resource == "pods/attach" {
						if attachmentRole != nil {
							test.Fatal("multiple attachment roles rendered")
						}
						attachmentRole = role
					}
				}
			}
		}
	}
	if webhookConfig == nil {
		test.Fatal("independent guard configuration missing")
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/managed": "stdio-invocation", "kognition.dev/stdio-invocation": "invocation"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "kognition-stdio-invocation-one", Namespace: "tenant", Labels: labels}, Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), RunAsGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{{Name: "exec-probe", Image: "sandbox:test", Stdin: true, StdinOnce: true, SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true), AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
	}}}}
	if err := admin.Create(ctx, job); err != nil {
		test.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: "tenant", Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	pod.Spec.NodeName = "test-node"
	if err := admin.Create(ctx, pod); err != nil {
		test.Fatal(err)
	}
	unsafe := pod.DeepCopy()
	unsafe.Name, unsafe.ResourceVersion, unsafe.UID = "kognition-stdio-invocation-legacy", "", ""
	unsafe.Spec.HostPID = true
	if err := admin.Create(ctx, unsafe); err != nil {
		test.Fatal(err)
	}
	if err := admin.Create(ctx, webhookConfig); err != nil {
		test.Fatal(err)
	}
	probe := unsafe.DeepCopy()
	probe.Name, probe.ResourceVersion, probe.UID = "kognition-stdio-invocation-probe", "", ""
	var probeError error
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		probeError = admin.Create(ctx, probe.DeepCopy(), client.DryRunAll)
		return apierrors.IsForbidden(probeError) && strings.Contains(probeError.Error(), "sandbox.guard.kognition.dev"), nil
	}); err != nil {
		test.Fatalf("activate reserved-name admission: %v; last response: %v", err, probeError)
	}
	if attachmentRole == nil {
		test.Fatal("controller attachment Role missing")
	}
	if err := admin.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "attachment-fixture-controller", Namespace: "tenant"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: attachmentRole.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "security-test-kognition-controller", Namespace: "control"}},
	}); err != nil {
		test.Fatal(err)
	}
	controller, err := kubernetes.NewForConfig(impersonated(controllerName))
	if err != nil {
		test.Fatal(err)
	}
	if nativePolicies {
		test.Run("native-policy-independently-denies-helm-adoption", func(test *testing.T) {
			candidate := pod.DeepCopy()
			candidate.Annotations = map[string]string{"meta.helm.sh/release-namespace": "other"}
			var lastError error
			if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
				_, lastError = controller.CoreV1().Pods("tenant").Update(ctx, candidate.DeepCopy(), metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
				return isControllerPolicyDenial(lastError), nil
			}); err != nil {
				test.Fatalf("native admission must deny Helm adoption with guard active: %v; last response: %v", err, lastError)
			}
		})
	}
	adminClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	test.Run("other-installations-are-not-guarded", func(test *testing.T) {
		candidate := pod.DeepCopy()
		candidate.Namespace, candidate.ResourceVersion, candidate.UID = "outside", "", ""
		candidate.OwnerReferences = nil
		if err := admin.Create(ctx, candidate, client.DryRunAll); err != nil {
			test.Fatalf("reserved Pod in an unenrolled namespace must not be intercepted: %v", err)
		}
	})
	test.Run("restricted-pod-create-after-activation", func(test *testing.T) {
		candidate := pod.DeepCopy()
		candidate.Name, candidate.ResourceVersion, candidate.UID = "kognition-stdio-invocation-new", "", ""
		if err := admin.Create(ctx, candidate, client.DryRunAll); err != nil {
			test.Fatalf("valid reserved Pod rejected: %v", err)
		}
	})
	test.Run("sandbox-network-labels-cannot-change", func(test *testing.T) {
		for _, label := range []string{"kognition.dev/agent", "kognition.dev/agent-service-workload", "kognition.dev/stdio-invocation", "app.kubernetes.io/managed-by"} {
			candidate := pod.DeepCopy()
			candidate.Labels[label] = "changed"
			if err := admin.Update(ctx, candidate, client.DryRunAll); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "sandbox.guard.kognition.dev") {
				test.Fatalf("network identity %s mutation must be denied by guard: %v", label, err)
			}
		}
	})
	test.Run("ephemeral-container-cannot-change-sandbox", func(test *testing.T) {
		candidate := pod.DeepCopy()
		candidate.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "debug:test"}}}
		_, err := adminClient.CoreV1().Pods("tenant").UpdateEphemeralContainers(ctx, candidate.Name, candidate, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}})
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "sandbox.guard.kognition.dev") {
			test.Fatalf("ephemeral container must be denied by guard: %v", err)
		}
	})
	attach := func(name string) error {
		return controller.CoreV1().RESTClient().Post().Namespace("tenant").Resource("pods").Name(name).SubResource("attach").
			Param("container", "exec-probe").Param("stdin", "true").Param("stdout", "true").Param("stderr", "true").Do(ctx).Error()
	}
	test.Run("legacy-unsafe-target-denied", func(test *testing.T) {
		if err := attach(unsafe.Name); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "attach.guard.kognition.dev") {
			test.Fatalf("expected attachment guard denial, got %v", err)
		}
	})
	test.Run("current-sandbox-authorized", func(test *testing.T) {
		before := allowedConnections.Load()
		if err := attach(pod.Name); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "attach.guard.kognition.dev") || allowedConnections.Load() != before {
			test.Fatalf("attachment without NetworkPolicy must be denied: %v", err)
		}
		policy := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: job.Namespace, Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
			Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/stdio-invocation": "invocation"}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
		}
		if err := admin.Create(ctx, policy); err != nil {
			test.Fatal(err)
		}
		if err := attach(pod.Name); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "attach.guard.kognition.dev") || allowedConnections.Load() != before {
			test.Fatalf("attachment without independent baseline must be denied: %v", err)
		}
		for _, document := range renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
			var baseline networkingv1.NetworkPolicy
			if err := json.Unmarshal(document.Raw, &baseline); err != nil {
				test.Fatal(err)
			}
			if baseline.Kind == "NetworkPolicy" && baseline.Name == handler.BaselinePolicyName {
				if err := admin.Create(ctx, &baseline); err != nil {
					test.Fatal(err)
				}
			}
		}
		err := attach(pod.Name)
		if allowedConnections.Load() != before+1 {
			test.Fatalf("valid sandbox was not authorized: %v", err)
		}
	})
	test.Run("guard-cannot-write-or-read-secrets", func(test *testing.T) {
		if err := guardReader.Delete(ctx, pod); !apierrors.IsForbidden(err) {
			test.Fatalf("guard Pod delete: %v", err)
		}
		if err := guardReader.Get(ctx, client.ObjectKey{Namespace: "control", Name: "guard-tls"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
			test.Fatalf("guard Secret lookup: %v", err)
		}
	})
	test.Run("controller-cannot-alter-guard", func(test *testing.T) {
		if err := controller.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, webhookConfig.Name, metav1.DeleteOptions{}); !apierrors.IsForbidden(err) {
			test.Fatalf("controller guard removal: %v", err)
		}
		if _, err := controller.CoreV1().Secrets("control").Get(ctx, "security-test-kognition-admission-guard-tls", metav1.GetOptions{}); !apierrors.IsForbidden(err) {
			test.Fatalf("controller TLS lookup: %v", err)
		}
	})
	test.Run("outage-fails-closed", func(test *testing.T) {
		server.Close()
		before := allowedConnections.Load()
		if err := attach(pod.Name); err == nil || !strings.Contains(err.Error(), "failed calling webhook") {
			test.Fatalf("guard outage must reject attachment: %v", err)
		}
		if allowedConnections.Load() != before {
			test.Fatal("outage allowed attachment")
		}
	})
}
