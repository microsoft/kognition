// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admissionguard

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const invocationLabel = "kognition.dev/stdio-invocation"
const sandboxPrefix = "kognition-stdio-invocation-"

type AttachHandler struct {
	Reader                client.Reader
	ControllerUsername    string
	WorkloadNamespaces    map[string]bool
	BaselinePolicyName    string
	ReleaseName           string
	InstallationNamespace string
}

func (handler *AttachHandler) Handle(ctx context.Context, request admission.Request) admission.Response {
	if handler.ControllerUsername == "" || handler.Reader == nil {
		return admission.Denied("attachment guard is not configured")
	}
	if request.Resource.Group == "" && request.Resource.Resource == "pods" && (request.SubResource == "" || request.SubResource == "ephemeralcontainers") &&
		(request.Operation == admissionv1.Create || request.Operation == admissionv1.Update) {
		var pod, oldPod corev1.Pod
		if err := json.Unmarshal(request.Object.Raw, &pod); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if len(request.OldObject.Raw) != 0 {
			if err := json.Unmarshal(request.OldObject.Raw, &oldPod); err != nil {
				return admission.Errored(http.StatusBadRequest, err)
			}
		}
		if strings.HasPrefix(pod.Name, sandboxPrefix) || strings.HasPrefix(pod.GenerateName, sandboxPrefix) || strings.HasPrefix(oldPod.Name, sandboxPrefix) {
			if !handler.WorkloadNamespaces[request.Namespace] || !restrictedSandbox(pod.Spec) || !sandboxNetworkIdentity(pod.Labels) ||
				(len(request.OldObject.Raw) != 0 && pod.Labels[invocationLabel] != oldPod.Labels[invocationLabel]) {
				return admission.Denied("reserved stdio Pod names require a credential-free restricted sandbox")
			}
		}
		return admission.Allowed("sandbox Pod name invariant satisfied")
	}
	if request.UserInfo.Username != handler.ControllerUsername {
		return admission.Allowed("not the guarded controller")
	}
	if request.Operation != admissionv1.Connect || request.Resource.Group != "" || request.Resource.Resource != "pods" ||
		request.SubResource != "attach" || !handler.WorkloadNamespaces[request.Namespace] || !strings.HasPrefix(request.Name, sandboxPrefix) {
		return admission.Denied("controller attachment is limited to enrolled workload namespaces")
	}
	var options corev1.PodAttachOptions
	if err := json.Unmarshal(request.Object.Raw, &options); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if options.Container != "exec-probe" || options.TTY || !options.Stdin || !options.Stdout || !options.Stderr {
		return admission.Denied("controller attachment requires non-interactive stdio sandbox streams")
	}
	var pod corev1.Pod
	if err := handler.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.Name}, &pod); err != nil {
		return admission.Denied("cannot verify attachment Pod")
	}
	owner := metav1.GetControllerOf(&pod)
	if !managedSandbox(&pod) || !sandboxNetworkIdentity(pod.Labels) || owner == nil || owner.APIVersion != "batch/v1" || owner.Kind != "Job" || owner.UID == "" || !restrictedSandbox(pod.Spec) {
		return admission.Denied("attachment target is not a restricted stdio sandbox")
	}
	var job batchv1.Job
	if err := handler.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: owner.Name}, &job); err != nil {
		return admission.Denied("cannot verify attachment Job")
	}
	if job.UID != owner.UID || !strings.HasPrefix(job.Name, sandboxPrefix) || !managedSandbox(&job) || job.Labels[invocationLabel] != pod.Labels[invocationLabel] ||
		job.Spec.Template.Labels[invocationLabel] != pod.Labels[invocationLabel] || !restrictedSandbox(job.Spec.Template.Spec) {
		return admission.Denied("attachment target does not match its current sandbox Job")
	}
	var policy networkingv1.NetworkPolicy
	if err := handler.Reader.Get(ctx, client.ObjectKeyFromObject(&job), &policy); err != nil {
		return admission.Denied("cannot verify attachment NetworkPolicy")
	}
	policyOwner := metav1.GetControllerOf(&policy)
	expectedLabels := map[string]string{"app.kubernetes.io/managed-by": "kognition", invocationLabel: pod.Labels[invocationLabel]}
	if policy.UID == "" || policy.DeletionTimestamp != nil || hasHelmOwnership(&policy) ||
		policy.Labels["app.kubernetes.io/managed-by"] != "kognition" || policy.Labels[invocationLabel] != pod.Labels[invocationLabel] ||
		policyOwner == nil || policyOwner.APIVersion != "batch/v1" || policyOwner.Kind != "Job" || policyOwner.Name != job.Name || policyOwner.UID != job.UID ||
		!equality.Semantic.DeepEqual(policy.Spec.PodSelector, metav1.LabelSelector{MatchLabels: expectedLabels}) ||
		len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(policy.Spec.Egress) != 0 || len(policy.Spec.Ingress) != 0 {
		return admission.Denied("attachment requires its current Job-owned deny-egress NetworkPolicy")
	}
	if handler.BaselinePolicyName == "" || handler.ReleaseName == "" || handler.InstallationNamespace == "" {
		return admission.Denied("attachment isolation baseline is not configured")
	}
	var baseline networkingv1.NetworkPolicy
	if err := handler.Reader.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: handler.BaselinePolicyName}, &baseline); err != nil {
		return admission.Denied("cannot verify attachment isolation baseline")
	}
	expectedSelector := metav1.LabelSelector{
		MatchLabels:      map[string]string{"app.kubernetes.io/managed-by": "kognition"},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: invocationLabel, Operator: metav1.LabelSelectorOpExists}},
	}
	if baseline.UID == "" || baseline.DeletionTimestamp != nil || len(baseline.OwnerReferences) != 0 ||
		baseline.Labels["app.kubernetes.io/managed-by"] != "Helm" || baseline.Annotations["meta.helm.sh/release-name"] != handler.ReleaseName || baseline.Annotations["meta.helm.sh/release-namespace"] != handler.InstallationNamespace ||
		!equality.Semantic.DeepEqual(baseline.Spec.PodSelector, expectedSelector) ||
		len(baseline.Spec.PolicyTypes) != 1 || baseline.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(baseline.Spec.Egress) != 0 || len(baseline.Spec.Ingress) != 0 {
		return admission.Denied("attachment requires an independent deny-egress baseline")
	}
	return admission.Allowed("verified restricted stdio sandbox")
}

func sandboxNetworkIdentity(labels map[string]string) bool {
	if labels["app.kubernetes.io/managed-by"] != "kognition" || labels[invocationLabel] == "" {
		return false
	}
	for _, key := range []string{"kognition.dev/agent", "kognition.dev/agent-service-workload"} {
		if _, exists := labels[key]; exists {
			return false
		}
	}
	return true
}

func managedSandbox(object metav1.Object) bool {
	labels := object.GetLabels()
	return object.GetDeletionTimestamp() == nil && object.GetUID() != "" &&
		labels["app.kubernetes.io/managed-by"] == "kognition" && labels["kognition.dev/managed"] == "stdio-invocation" &&
		labels[invocationLabel] != "" && !hasHelmOwnership(object)
}

func hasHelmOwnership(object metav1.Object) bool {
	_, release := object.GetAnnotations()["meta.helm.sh/release-name"]
	_, namespace := object.GetAnnotations()["meta.helm.sh/release-namespace"]
	return release || namespace
}

func restrictedSandbox(spec corev1.PodSpec) bool {
	if spec.HostNetwork || spec.HostPID || spec.HostIPC || spec.RuntimeClassName != nil ||
		(spec.OS != nil && spec.OS.Name != corev1.Linux) ||
		(spec.ServiceAccountName != "" && spec.ServiceAccountName != "default") ||
		(spec.DeprecatedServiceAccount != "" && spec.DeprecatedServiceAccount != "default") ||
		spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken ||
		len(spec.Volumes) != 0 || len(spec.InitContainers) != 0 || len(spec.EphemeralContainers) != 0 || len(spec.Containers) != 1 {
		return false
	}
	podSecurity := spec.SecurityContext.DeepCopy()
	if podSecurity == nil || podSecurity.RunAsNonRoot == nil || !*podSecurity.RunAsNonRoot ||
		podSecurity.RunAsUser == nil || *podSecurity.RunAsUser <= 0 ||
		podSecurity.SeccompProfile == nil || podSecurity.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || podSecurity.SeccompProfile.LocalhostProfile != nil {
		return false
	}
	if podSecurity.RunAsGroup != nil && *podSecurity.RunAsGroup <= 0 {
		return false
	}
	podSecurity.RunAsNonRoot, podSecurity.RunAsUser, podSecurity.RunAsGroup, podSecurity.SeccompProfile = nil, nil, nil, nil
	if !equality.Semantic.DeepEqual(*podSecurity, corev1.PodSecurityContext{}) {
		return false
	}
	container := spec.Containers[0]
	if container.Name != "exec-probe" || !container.Stdin || !container.StdinOnce || container.TTY ||
		len(container.Env) != 0 || len(container.EnvFrom) != 0 || len(container.VolumeMounts) != 0 || len(container.VolumeDevices) != 0 {
		return false
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 {
			return false
		}
	}
	security := container.SecurityContext.DeepCopy()
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
		security.Capabilities == nil || len(security.Capabilities.Add) != 0 || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		return false
	}
	if security.Privileged != nil && *security.Privileged {
		return false
	}
	security.AllowPrivilegeEscalation, security.ReadOnlyRootFilesystem, security.Capabilities, security.Privileged = nil, nil, nil, nil
	return equality.Semantic.DeepEqual(*security, corev1.SecurityContext{})
}
