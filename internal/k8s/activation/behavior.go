// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type PolicyProbeResult struct {
	Policy           string    `json:"policy"`
	Namespace        string    `json:"namespace"`
	Name             string    `json:"name"`
	UID              types.UID `json:"uid"`
	Allowed          bool      `json:"allowed"`
	Denied           bool      `json:"denied"`
	CleanupConfirmed bool      `json:"cleanupConfirmed"`
}

type policyProbe struct {
	policy   string
	group    string
	resource string
	object   client.Object
}

func VerifyPolicyBehavior(ctx context.Context, config *rest.Config, manifest Manifest) (results []PolicyProbeResult, resultErr error) {
	return verifyPolicyBehavior(ctx, config, manifest, nil, nil)
}

func verifyPolicyBehavior(ctx context.Context, config *rest.Config, manifest Manifest, transactionClient client.Client, journal *corev1.ConfigMap) (results []PolicyProbeResult, resultErr error) {
	if config == nil {
		return nil, fmt.Errorf("explicit Kubernetes configuration is required")
	}
	bounded := rest.CopyConfig(config)
	bounded.Timeout = 10 * time.Second
	ctx, cancel := context.WithTimeout(ctx, policyBehaviorTimeout(len(manifest.Namespaces)))
	defer cancel()
	admin, err := client.New(bounded, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, err
	}
	if transactionClient != nil {
		admin = transactionClient
	}
	if err := Verify(ctx, admin, manifest); err != nil {
		return nil, err
	}
	bounded.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller,
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + manifest.Namespace, "system:authenticated"},
	}
	controller, err := client.New(bounded, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, err
	}
	var created []client.Object
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var cleanupErr error
		if journal != nil {
			cleanupErr = cleanupJournaledResources(cleanup, admin, journal, true)
		} else {
			cleanupErr = cleanupProbes(cleanup, admin, created)
		}
		for index := range results {
			results[index].CleanupConfirmed = cleanupErr == nil
		}
		resultErr = errors.Join(resultErr, cleanupErr)
		if resultErr == nil {
			resultErr = Verify(ctx, admin, manifest)
		}
	}()
	create := func(object client.Object) error {
		if journal != nil {
			return createJournaledResource(ctx, admin, journal, object, true)
		}
		return createProbe(ctx, admin, object, &created)
	}
	for _, namespace := range manifest.Namespaces {
		probes := policyProbes(manifest, namespace, uuid.NewString())
		protection := probeProtection(probes)
		if err := create(protection); err != nil {
			return results, err
		}
		binding := &admissionv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: protection.Name}, Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: protection.Name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}}}
		if err := create(binding); err != nil {
			return results, err
		}
		roles := map[string]*rbacv1.Role{}
		for _, probe := range probes {
			if err := create(probe.object); err != nil {
				return results, err
			}
			if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				current, ok := probe.object.DeepCopyObject().(client.Object)
				if !ok {
					return false, fmt.Errorf("probe copy does not implement client.Object")
				}
				if err := admin.Get(ctx, client.ObjectKeyFromObject(probe.object), current); err != nil {
					return false, err
				}
				if current.GetUID() != probe.object.GetUID() {
					return false, fmt.Errorf("probe UID changed")
				}
				err := admin.Update(ctx, current)
				if apierrors.IsConflict(err) {
					return false, nil
				}
				if err == nil {
					probe.object.SetResourceVersion(current.GetResourceVersion())
					return false, nil
				}
				if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+protection.Name+"'") && strings.Contains(err.Error(), "binding '"+protection.Name+"'") {
					return true, nil
				}
				return false, err
			}); err != nil {
				return results, fmt.Errorf("dry-run protection for %s/%s: %w", probe.object.GetNamespace(), probe.object.GetName(), err)
			}
			if roles[probe.object.GetNamespace()] == nil {
				roles[probe.object.GetNamespace()] = &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "kognition-admission-probe-" + uuid.NewString(), Namespace: probe.object.GetNamespace()}}
			}
			role := roles[probe.object.GetNamespace()]
			role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{probe.group}, Resources: []string{probe.resource}, ResourceNames: []string{probe.object.GetName()}, Verbs: []string{"update"}})
		}
		for _, namespace := range []string{manifest.Namespace, namespace} {
			role := roles[namespace]
			if err := create(role); err != nil {
				return results, err
			}
			binding := &rbacv1.RoleBinding{ObjectMeta: role.ObjectMeta, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: manifest.Namespace, Name: manifest.Controller}}}
			binding.ResourceVersion, binding.UID = "", ""
			if err := create(binding); err != nil {
				return results, err
			}
		}
		for _, probe := range probes {
			result, err := runPolicyProbe(ctx, admin, controller, probe)
			results = append(results, result)
			if err != nil {
				return results, err
			}
		}
	}
	return results, nil
}

func runPolicyProbe(ctx context.Context, admin client.Reader, controller client.Client, probe policyProbe) (PolicyProbeResult, error) {
	result := PolicyProbeResult{Policy: probe.policy, Namespace: probe.object.GetNamespace(), Name: probe.object.GetName(), UID: probe.object.GetUID()}
	currentObject := func(ctx context.Context) (client.Object, error) {
		current, ok := probe.object.DeepCopyObject().(client.Object)
		if !ok {
			return nil, fmt.Errorf("probe copy does not implement client.Object")
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(probe.object), current); err != nil {
			return nil, err
		}
		if current.GetUID() != result.UID {
			return nil, fmt.Errorf("probe UID changed")
		}
		return current, nil
	}
	if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		allowed, err := currentObject(ctx)
		if err != nil {
			return false, err
		}
		annotations := allowed.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["kognition.dev/admission-probe"] = "dry-run"
		allowed.SetAnnotations(annotations)
		err = controller.Update(ctx, allowed, client.DryRunAll)
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		return result, fmt.Errorf("policy %s positive probe: %w", probe.policy, err)
	}
	result.Allowed = true
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		denied, err := currentObject(ctx)
		if err != nil {
			return false, err
		}
		denied.SetLabels(nil)
		if _, job := denied.(*batchv1.Job); job {
			denied.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
		}
		err = controller.Update(ctx, denied, client.DryRunAll)
		if err == nil || apierrors.IsConflict(err) {
			return false, nil
		}
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+probe.policy+"'") || !strings.Contains(err.Error(), "binding '"+probe.policy+"'") {
			return false, fmt.Errorf("policy %s negative probe requires its exact policy and binding denial; got %w", probe.policy, err)
		}
		return true, nil
	})
	if err != nil {
		return result, fmt.Errorf("policy %s enforcement: %w", probe.policy, err)
	}
	result.Denied = true
	return result, nil
}

func policyProbes(manifest Manifest, namespace, suffix string) []policyProbe {
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent": "admission-probe-" + suffix}
	metadata := func(name, target string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name + suffix, Namespace: target, Labels: labels}
	}
	state := &corev1.ConfigMap{ObjectMeta: metadata("kognition-cost-", manifest.Namespace)}
	state.Labels = map[string]string{"kognition.dev/cost-record": "true"}
	job := &batchv1.Job{ObjectMeta: metadata("kognition-admission-probe-", namespace), Spec: batchv1.JobSpec{Suspend: ptr.To(true), Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{{Name: "probe", Image: "registry.k8s.io/pause:3.9", SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
	}}}}
	probes := []policyProbe{
		{policy: "state", resource: "configmaps", object: state},
		{policy: "leases", group: "coordination.k8s.io", resource: "leases", object: &coordinationv1.Lease{ObjectMeta: metadata("kognition-tool-grant-rate-", manifest.Namespace)}},
		{policy: "credentials", resource: "secrets", object: &corev1.Secret{ObjectMeta: metadata("kognition-admission-probe-", namespace), Type: corev1.SecretTypeOpaque}},
		{policy: "workload", group: "batch", resource: "jobs", object: job},
		{policy: "services", resource: "services", object: &corev1.Service{ObjectMeta: metadata("kognition-admission-probe-", namespace), Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: labels, Ports: []corev1.ServicePort{{Port: 8080}}}}},
		{policy: "network", group: "networking.k8s.io", resource: "networkpolicies", object: &networkingv1.NetworkPolicy{ObjectMeta: metadata("kognition-admission-probe-", namespace), Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}}}},
	}
	for index := range probes {
		for _, object := range manifest.Objects {
			if object.GetKind() == "ValidatingAdmissionPolicy" && strings.HasSuffix(object.GetName(), "-"+probes[index].policy) {
				probes[index].policy = object.GetName()
				break
			}
		}
	}
	return probes
}

func probeProtection(probes []policyProbe) *admissionv1.ValidatingAdmissionPolicy {
	var matches []string
	for _, probe := range probes {
		matches = append(matches, "(request.namespace == "+strconv.Quote(probe.object.GetNamespace())+" && request.name == "+strconv.Quote(probe.object.GetName())+" && request.resource.group == "+strconv.Quote(probe.group)+" && request.resource.resource == "+strconv.Quote(probe.resource)+")")
	}
	return &admissionv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "kognition-admission-probe-" + uuid.NewString()},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    ptr.To(admissionv1.Fail),
			MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{Operations: []admissionv1.OperationType{admissionv1.Update}, Rule: admissionv1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*"}, Scope: ptr.To(admissionv1.NamespacedScope)}}}}},
			MatchConditions:  []admissionv1.MatchCondition{{Name: "owned-probes", Expression: strings.Join(matches, " || ")}},
			Validations:      []admissionv1.Validation{{Expression: "request.dryRun == true", Reason: ptr.To(metav1.StatusReasonForbidden), Message: "Admission probes require dry-run updates."}},
		},
	}
}
