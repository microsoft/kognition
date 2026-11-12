// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admissionguard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func sandboxFixture() (*corev1.Pod, *batchv1.Job, admission.Request) {
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/managed": "stdio-invocation", invocationLabel: "invocation-one"}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: sandboxPrefix + "one", Namespace: "tenant", UID: "job-uid", Labels: labels}}
	job.Spec.Template = corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(false),
		SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:                   []corev1.Container{{Name: "exec-probe", Image: "sandbox:test", Stdin: true, StdinOnce: true, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sandboxPrefix + "one-pod", Namespace: "tenant", UID: "pod-uid", Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}}, Spec: *job.Spec.Template.Spec.DeepCopy()}
	options, _ := json.Marshal(corev1.PodAttachOptions{Container: "exec-probe", Stdin: true, Stdout: true, Stderr: true})
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Connect, Namespace: "tenant", Name: pod.Name, Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"}, SubResource: "attach", UserInfo: authenticationv1.UserInfo{Username: "controller"}, Object: runtime.RawExtension{Raw: options}}}
	return pod, job, request
}

func sandboxPolicyFixture(job *batchv1.Job) *networkingv1.NetworkPolicy {
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", invocationLabel: job.Labels[invocationLabel]}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: job.Name, Namespace: job.Namespace, UID: "policy-uid", Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
	}
}

func sandboxBaselineFixture() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "stdio-baseline", Namespace: "tenant", UID: "baseline-uid", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "test", "meta.helm.sh/release-namespace": "control"}},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}, MatchExpressions: []metav1.LabelSelectorRequirement{{Key: invocationLabel, Operator: metav1.LabelSelectorOpExists}}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
	}
}

func TestAttachGuard(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod, *batchv1.Job, *admission.Request){
		"valid": func(*corev1.Pod, *batchv1.Job, *admission.Request) {},
		"valid-defaulted": func(pod *corev1.Pod, job *batchv1.Job, _ *admission.Request) {
			pod.Spec.ServiceAccountName, job.Spec.Template.Spec.ServiceAccountName = "default", "default"
			pod.Spec.DeprecatedServiceAccount, job.Spec.Template.Spec.DeprecatedServiceAccount = "default", "default"
			pod.Spec.OS = &corev1.PodOS{Name: corev1.Linux}
		},
		"pod-no-uid": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.UID = "" },
		"job-no-uid": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) { job.UID = "" },
		"pod-deleting": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.DeletionTimestamp = ptr.To(metav1.Now())
			pod.Finalizers = []string{"test.kognition.dev/hold"}
		},
		"job-deleting": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) {
			job.DeletionTimestamp = ptr.To(metav1.Now())
			job.Finalizers = []string{"test.kognition.dev/hold"}
		},
		"owner-no-uid": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.OwnerReferences[0].UID = "" },
		"owner-kind": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.OwnerReferences[0].Kind = "Deployment"
		},
		"owner-api": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.OwnerReferences[0].APIVersion = "other/v1"
		},
		"owner-not-controller": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.OwnerReferences[0].Controller = ptr.To(false)
		},
		"job-invocation": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) {
			job.Labels = map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/managed": "stdio-invocation", invocationLabel: "other"}
		},
		"template-invocation": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) {
			job.Spec.Template.Labels = map[string]string{invocationLabel: "other"}
		},
		"malformed-options": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) { request.Object.Raw = []byte(`{`) },
		"tty": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Object.Raw = []byte(`{"container":"exec-probe","stdin":true,"stdout":true,"stderr":true,"tty":true}`)
		},
		"missing-stdin": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Object.Raw = []byte(`{"container":"exec-probe","stdout":true,"stderr":true}`)
		},
		"missing-stdout": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Object.Raw = []byte(`{"container":"exec-probe","stdin":true,"stderr":true}`)
		},
		"missing-stderr": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Object.Raw = []byte(`{"container":"exec-probe","stdin":true,"stdout":true}`)
		},
		"wrong-resource": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Resource.Resource = "services"
		},
		"wrong-group": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) { request.Resource.Group = "other" },
		"outside":     func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) { request.Namespace = "outside" },
		"nonreserved": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) { request.Name = "agent" },
		"missing-token-setting": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.AutomountServiceAccountToken = nil
		},
		"missing-security": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.Spec.SecurityContext = nil },
		"missing-container-security": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].SecurityContext = nil
		},
		"runtime-class": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.RuntimeClassName = ptr.To("other")
		},
		"windows": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.OS = &corev1.PodOS{Name: corev1.Windows}
		},
		"init-container": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.InitContainers = []corev1.Container{{Name: "init"}}
		},
		"ephemeral-container": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}}
		},
		"extra-container": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar"})
		},
		"host-port": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{HostPort: 8080}}
		},
		"capability-add": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		},
		"env-from": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{}}
		},
		"unowned":   func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.OwnerReferences = nil },
		"stale-job": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) { job.UID = "replaced" },
		"unmanaged": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.Labels = nil },
		"pod-helm-namespace": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Annotations = map[string]string{"meta.helm.sh/release-namespace": "other"}
		},
		"job-helm-namespace": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) {
			job.Annotations = map[string]string{"meta.helm.sh/release-namespace": "other"}
		},
		"pod-empty-helm-release": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Annotations = map[string]string{"meta.helm.sh/release-name": ""}
		},
		"installation": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) { request.Namespace = "control" },
		"exec":         func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) { request.SubResource = "exec" },
		"missing-pod": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Name = sandboxPrefix + "missing"
		},
		"missing-job": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) { job.Name = sandboxPrefix + "different" },
		"bad-options": func(_ *corev1.Pod, _ *batchv1.Job, request *admission.Request) {
			request.Object.Raw = []byte(`{"container":"other"}`)
		},
		"privileged": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
		},
		"token": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.AutomountServiceAccountToken = ptr.To(true)
		},
		"host":            func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.Spec.HostNetwork = true },
		"service-account": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) { pod.Spec.ServiceAccountName = "elevated" },
		"volume": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Volumes = []corev1.Volume{{Name: "secret"}}
		},
		"env-secret": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "secret", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{}}}}
		},
		"unconfined": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
		},
		"root": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To(int64(0))
		},
		"unsafe-job": func(_ *corev1.Pod, job *batchv1.Job, _ *admission.Request) { job.Spec.Template.Spec.HostPID = true },
		"legacy-agent-label": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Labels["kognition.dev/agent"] = "agent"
		},
		"legacy-service-label": func(pod *corev1.Pod, _ *batchv1.Job, _ *admission.Request) {
			pod.Labels["kognition.dev/agent-service-workload"] = "service"
		},
	} {
		t.Run(name, func(t *testing.T) {
			pod, job, request := sandboxFixture()
			mutate(pod, job, &request)
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := batchv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := networkingv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			handler := &AttachHandler{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, job, sandboxPolicyFixture(job), sandboxBaselineFixture()).Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}, BaselinePolicyName: "stdio-baseline", ReleaseName: "test", InstallationNamespace: "control"}
			response := handler.Handle(context.Background(), request)
			if response.Allowed != strings.HasPrefix(name, "valid") {
				t.Fatalf("allowed = %v, response = %+v", response.Allowed, response.Result)
			}
		})
	}
}

func TestAttachGuardRequiresIndependentBaseline(test *testing.T) {
	pod, job, request := sandboxFixture()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme} {
		if err := register(scheme); err != nil {
			test.Fatal(err)
		}
	}
	handler := &AttachHandler{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, job, sandboxPolicyFixture(job)).Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}, BaselinePolicyName: "stdio-baseline", ReleaseName: "test", InstallationNamespace: "control"}
	if response := handler.Handle(context.Background(), request); response.Allowed {
		test.Fatal("invocation-owned policy alone cannot guarantee isolation after its deletion")
	}
}

func TestAttachGuardRejectsInvalidBaseline(test *testing.T) {
	for name, mutate := range map[string]func(*networkingv1.NetworkPolicy){
		"valid":  func(*networkingv1.NetworkPolicy) {},
		"no-uid": func(policy *networkingv1.NetworkPolicy) { policy.UID = "" },
		"deleting": func(policy *networkingv1.NetworkPolicy) {
			policy.DeletionTimestamp = ptr.To(metav1.Now())
			policy.Finalizers = []string{"test.kognition.dev/hold"}
		},
		"job-owned": func(policy *networkingv1.NetworkPolicy) {
			_, job, _ := sandboxFixture()
			policy.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}
		},
		"unmanaged":          func(policy *networkingv1.NetworkPolicy) { policy.Labels = nil },
		"controller-managed": func(policy *networkingv1.NetworkPolicy) { policy.Labels["app.kubernetes.io/managed-by"] = "kognition" },
		"no-release":         func(policy *networkingv1.NetworkPolicy) { delete(policy.Annotations, "meta.helm.sh/release-name") },
		"no-installation":    func(policy *networkingv1.NetworkPolicy) { delete(policy.Annotations, "meta.helm.sh/release-namespace") },
		"other-release":      func(policy *networkingv1.NetworkPolicy) { policy.Annotations["meta.helm.sh/release-name"] = "other" },
		"other-installation": func(policy *networkingv1.NetworkPolicy) {
			policy.Annotations["meta.helm.sh/release-namespace"] = "other"
		},
		"wrong-selector": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PodSelector.MatchLabels["app.kubernetes.io/managed-by"] = "other"
		},
		"missing-invocation": func(policy *networkingv1.NetworkPolicy) { policy.Spec.PodSelector.MatchExpressions = nil },
		"extra-selector":     func(policy *networkingv1.NetworkPolicy) { policy.Spec.PodSelector.MatchLabels["extra"] = "label" },
		"no-policy-type":     func(policy *networkingv1.NetworkPolicy) { policy.Spec.PolicyTypes = nil },
		"ingress-only": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		},
		"extra-policy-type": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
		},
		"allow-egress": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
		},
		"ingress-rule": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
		},
	} {
		test.Run(name, func(test *testing.T) {
			pod, job, request := sandboxFixture()
			baseline := sandboxBaselineFixture()
			mutate(baseline)
			scheme := runtime.NewScheme()
			for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme} {
				if err := register(scheme); err != nil {
					test.Fatal(err)
				}
			}
			handler := &AttachHandler{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, job, sandboxPolicyFixture(job), baseline).Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}, BaselinePolicyName: baseline.Name, ReleaseName: "test", InstallationNamespace: "control"}
			response := handler.Handle(context.Background(), request)
			if response.Allowed != (name == "valid") {
				test.Fatalf("unexpected baseline decision: %+v", response)
			}
			if name != "valid" && (response.Result == nil || response.Result.Code != 403 || response.Result.Message != "attachment requires an independent deny-egress baseline") {
				test.Fatalf("wrong rejection boundary: %+v", response)
			}
		})
	}
}

func TestAttachGuardRequiresNetworkPolicy(test *testing.T) {
	pod, job, request := sandboxFixture()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	handler := &AttachHandler{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, job).Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}}
	if response := handler.Handle(context.Background(), request); response.Allowed {
		test.Fatal("attachment allowed without its sandbox NetworkPolicy")
	}
}

func TestAttachGuardRejectsInvalidNetworkPolicy(test *testing.T) {
	for name, mutate := range map[string]func(*networkingv1.NetworkPolicy){
		"unowned":              func(policy *networkingv1.NetworkPolicy) { policy.OwnerReferences = nil },
		"stale-owner":          func(policy *networkingv1.NetworkPolicy) { policy.OwnerReferences[0].UID = "previous-job" },
		"wrong-owner-name":     func(policy *networkingv1.NetworkPolicy) { policy.OwnerReferences[0].Name = "other-job" },
		"wrong-owner-kind":     func(policy *networkingv1.NetworkPolicy) { policy.OwnerReferences[0].Kind = "Deployment" },
		"wrong-owner-api":      func(policy *networkingv1.NetworkPolicy) { policy.OwnerReferences[0].APIVersion = "other/v1" },
		"non-controller-owner": func(policy *networkingv1.NetworkPolicy) { policy.OwnerReferences[0].Controller = ptr.To(false) },
		"no-uid":               func(policy *networkingv1.NetworkPolicy) { policy.UID = "" },
		"deleting": func(policy *networkingv1.NetworkPolicy) {
			policy.DeletionTimestamp = ptr.To(metav1.Now())
			policy.Finalizers = []string{"test.kognition.dev/hold"}
		},
		"helm-owned": func(policy *networkingv1.NetworkPolicy) {
			policy.Annotations = map[string]string{"meta.helm.sh/release-name": "other"}
		},
		"helm-namespace": func(policy *networkingv1.NetworkPolicy) {
			policy.Annotations = map[string]string{"meta.helm.sh/release-namespace": "other"}
		},
		"empty-helm-release": func(policy *networkingv1.NetworkPolicy) {
			policy.Annotations = map[string]string{"meta.helm.sh/release-name": ""}
		},
		"unmanaged": func(policy *networkingv1.NetworkPolicy) { policy.Labels = nil },
		"different-invocation": func(policy *networkingv1.NetworkPolicy) {
			policy.Labels = map[string]string{"app.kubernetes.io/managed-by": "kognition", invocationLabel: "other"}
		},
		"broad-selector": func(policy *networkingv1.NetworkPolicy) { policy.Spec.PodSelector = metav1.LabelSelector{} },
		"nonmatching-selector": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PodSelector.MatchLabels = map[string]string{"app.kubernetes.io/managed-by": "kognition", invocationLabel: "other"}
		},
		"extra-selector": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "other", Operator: metav1.LabelSelectorOpExists}}
		},
		"no-policy-types": func(policy *networkingv1.NetworkPolicy) { policy.Spec.PolicyTypes = nil },
		"ingress-only": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		},
		"extra-policy-type": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.PolicyTypes = append(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress)
		},
		"egress-allowed": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
		},
		"ingress-rule": func(policy *networkingv1.NetworkPolicy) {
			policy.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}}
		},
	} {
		test.Run(name, func(test *testing.T) {
			pod, job, request := sandboxFixture()
			policy := sandboxPolicyFixture(job)
			mutate(policy)
			scheme := runtime.NewScheme()
			for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme} {
				if err := register(scheme); err != nil {
					test.Fatal(err)
				}
			}
			handler := &AttachHandler{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, job, policy).Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}}
			response := handler.Handle(context.Background(), request)
			if response.Allowed || response.Result == nil || response.Result.Code != 403 || response.Result.Message != "attachment requires its current Job-owned deny-egress NetworkPolicy" {
				test.Fatalf("invalid policy must deny attachment after lookup: %+v", response)
			}
		})
	}
}

func TestAttachGuardReaderFailures(test *testing.T) {
	const privateDetail = "private API backend detail"
	for _, resource := range []string{"pods", "jobs", "networkpolicies", "baseline"} {
		for name, failure := range map[string]error{
			"forbidden": apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "sandbox", errors.New(privateDetail)),
			"timeout":   apierrors.NewTimeoutError(privateDetail, 1),
			"canceled":  context.Canceled,
		} {
			test.Run(resource+"/"+name, func(test *testing.T) {
				pod, job, request := sandboxFixture()
				scheme := runtime.NewScheme()
				if err := corev1.AddToScheme(scheme); err != nil {
					test.Fatal(err)
				}
				if err := batchv1.AddToScheme(scheme); err != nil {
					test.Fatal(err)
				}
				if err := networkingv1.AddToScheme(scheme); err != nil {
					test.Fatal(err)
				}
				failedReads := 0
				reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, job, sandboxPolicyFixture(job), sandboxBaselineFixture()).WithInterceptorFuncs(interceptor.Funcs{
					Get: func(ctx context.Context, reader client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
						_, isPod := object.(*corev1.Pod)
						_, isJob := object.(*batchv1.Job)
						_, isPolicy := object.(*networkingv1.NetworkPolicy)
						if (resource == "pods" && isPod) || (resource == "jobs" && isJob) || (resource == "networkpolicies" && isPolicy) || (resource == "baseline" && isPolicy && key.Name == "stdio-baseline") {
							failedReads++
							return failure
						}
						return reader.Get(ctx, key, object, options...)
					},
				}).Build()
				handler := &AttachHandler{Reader: reader, ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}, BaselinePolicyName: "stdio-baseline", ReleaseName: "test", InstallationNamespace: "control"}
				response := handler.Handle(context.Background(), request)
				if failedReads != 1 || response.Allowed || response.Result == nil || response.Result.Code != 403 {
					test.Fatalf("read failure must deny attachment: reads=%d response=%+v", failedReads, response)
				}
				if strings.Contains(response.Result.Message, privateDetail) {
					test.Fatal("admission response exposes backend error details")
				}
			})
		}
	}
}

func TestReservedPodNetworkIdentity(test *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"missing-invocation": func(pod *corev1.Pod) { delete(pod.Labels, invocationLabel) },
		"changed-invocation": func(pod *corev1.Pod) { pod.Labels[invocationLabel] = "different" },
		"agent-selector":     func(pod *corev1.Pod) { pod.Labels["kognition.dev/agent"] = "other" },
		"service-selector":   func(pod *corev1.Pod) { pod.Labels["kognition.dev/agent-service-workload"] = "other" },
		"unmanaged":          func(pod *corev1.Pod) { delete(pod.Labels, "app.kubernetes.io/managed-by") },
		"unchanged":          func(*corev1.Pod) {},
		"job-labels": func(pod *corev1.Pod) {
			pod.Labels["batch.kubernetes.io/controller-uid"] = "job-uid"
			pod.Labels["batch.kubernetes.io/job-name"] = sandboxPrefix + "one"
		},
	} {
		test.Run(name, func(test *testing.T) {
			for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
				pod, _, request := sandboxFixture()
				if operation == admissionv1.Update {
					request.OldObject.Raw, _ = json.Marshal(pod)
				}
				mutate(pod)
				request.Operation, request.SubResource, request.UserInfo.Username = operation, "", "another-identity"
				request.Object.Raw, _ = json.Marshal(pod)
				handler := &AttachHandler{Reader: fake.NewClientBuilder().Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}}
				allowed := name == "unchanged" || name == "job-labels" || (name == "changed-invocation" && operation == admissionv1.Create)
				if response := handler.Handle(context.Background(), request); response.Allowed != allowed {
					test.Fatalf("operation=%s allowed=%v response=%+v", operation, allowed, response)
				}
			}
		})
	}
}

func TestReservedPodNamesCannotBeReplacedWithUnsafeTargets(t *testing.T) {
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		for _, unsafe := range []bool{false, true} {
			pod, _, request := sandboxFixture()
			pod.Spec.HostPID = unsafe
			request.Operation, request.SubResource, request.UserInfo.Username = operation, "", "another-identity"
			request.Object.Raw, _ = json.Marshal(pod)
			handler := &AttachHandler{Reader: fake.NewClientBuilder().Build(), ControllerUsername: "controller", WorkloadNamespaces: map[string]bool{"tenant": true}}
			if response := handler.Handle(context.Background(), request); response.Allowed == unsafe {
				t.Fatalf("operation=%s unsafe=%v response=%+v", operation, unsafe, response)
			}
		}
	}
}
