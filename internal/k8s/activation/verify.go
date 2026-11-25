// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Manifest struct {
	StdioIsolationProfile string `json:",omitempty"`
	Namespace             string
	Controller            string
	Deployment            string
	Guard                 string
	KnowledgeGateway      bool `json:",omitempty"`
	Namespaces            []string
	Objects               []*unstructured.Unstructured
	digest                [sha256.Size]byte
}

func Parse(reader io.Reader) (Manifest, error) {
	var result Manifest
	decoder := yaml.NewYAMLOrJSONDecoder(reader, 4096)
	counts := map[string]int{}
	namespaces := map[string]bool{}
	gatewayNamespace, gatewayName := "", ""
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return result, err
		}
		if len(object.Object) == 0 {
			continue
		}
		component, _, _ := unstructured.NestedString(object.Object, "spec", "template", "metadata", "labels", "app.kubernetes.io/component")
		if object.GetKind() == "Deployment" && component == "knowledge-gateway" {
			account, found, err := unstructured.NestedString(object.Object, "spec", "template", "spec", "serviceAccountName")
			if result.KnowledgeGateway || object.GetAPIVersion() != "apps/v1" || err != nil || !found || account != object.GetName() {
				return result, errors.New("one explicit Knowledge Gateway identity is required")
			}
			result.KnowledgeGateway = true
			gatewayNamespace, gatewayName = object.GetNamespace(), object.GetName()
		}
		if object.GetKind() == "Deployment" && component == "k8s-controller" {
			if object.GetAPIVersion() != "apps/v1" {
				return result, errors.New("unsupported controller Deployment version")
			}
			if result.Deployment != "" {
				return result, errors.New("multiple controller deployments")
			}
			result.Namespace, result.Deployment = object.GetNamespace(), object.GetName()
			account, found, err := unstructured.NestedString(object.Object, "spec", "template", "spec", "serviceAccountName")
			if err != nil || !found || account == "" {
				return result, errors.New("controller requires an explicit service account")
			}
			result.Controller = account
			replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
			if err != nil || !found || replicas != 0 {
				return result, errors.New("expected an admission-only bootstrap manifest with zero controller replicas")
			}
		}
		switch object.GetKind() {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			counts[object.GetKind()]++
			result.Objects = append(result.Objects, object)
		case "Deployment", "Service", "ServiceAccount", "ValidatingWebhookConfiguration", "Role", "RoleBinding":
			if strings.HasSuffix(object.GetName(), "-admission-guard") {
				counts[object.GetKind()]++
				result.Objects = append(result.Objects, object)
				if object.GetKind() == "Deployment" {
					result.Guard = object.GetName()
					result.StdioIsolationProfile, _, _ = unstructured.NestedString(object.Object, "spec", "template", "metadata", "annotations", "kognition.dev/stdio-isolation-profile")
				}
			}
		case "CiliumNetworkPolicy", "CiliumClusterwideNetworkPolicy":
			result.Objects = append(result.Objects, object)
		case "NetworkPolicy":
			if strings.HasSuffix(object.GetName(), "-stdio-baseline") {
				if namespaces[object.GetNamespace()] {
					return result, errors.New("duplicate stdio baseline namespace")
				}
				namespaces[object.GetNamespace()] = true
				result.Namespaces = append(result.Namespaces, object.GetNamespace())
				result.Objects = append(result.Objects, object)
			}
		}
	}
	if result.Namespace == "" || result.Controller == "" || result.Controller == "default" || counts["ValidatingAdmissionPolicy"] != 10 || counts["ValidatingAdmissionPolicyBinding"] != 10 || counts["Deployment"] != 1 || counts["Service"] != 1 || counts["ValidatingWebhookConfiguration"] != 1 || len(result.Namespaces) == 0 {
		return result, errors.New("manifest must contain one controller, one independent guard, ten admission policies and bindings, and dedicated workload baselines")
	}
	if result.KnowledgeGateway && (gatewayNamespace != result.Namespace || gatewayName != strings.TrimSuffix(result.Guard, "-admission-guard")+"-knowledge-gateway") {
		return result, errors.New("Knowledge Gateway must belong to the reviewed installation release")
	}
	if err := result.validate(); err != nil {
		return result, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	result.digest = sha256.Sum256(encoded)
	return result, nil
}

func Verify(ctx context.Context, admin client.Client, manifest Manifest) error {
	if err := manifest.validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if manifest.digest == ([sha256.Size]byte{}) || manifest.digest != sha256.Sum256(encoded) {
		return errors.New("bootstrap manifest must be unmodified output from Parse of a trusted render")
	}
	if err := VerifyDrained(ctx, admin, manifest); err != nil {
		return err
	}
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Deployment}, deployment); err != nil {
		return err
	}
	for _, expected := range manifest.Objects {
		if err := verifyObject(ctx, admin, expected); err != nil {
			return err
		}
	}
	guard := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Guard}, guard); err != nil {
		return err
	}
	if guard.Spec.Replicas == nil || *guard.Spec.Replicas < 1 || guard.Status.ObservedGeneration < guard.Generation || guard.Status.UpdatedReplicas != *guard.Spec.Replicas || guard.Status.AvailableReplicas != *guard.Spec.Replicas {
		return errors.New("independent guard rollout is not ready")
	}
	return nil
}

func VerifyDrained(ctx context.Context, admin client.Client, manifest Manifest) error {
	if manifest.Namespace == "" || manifest.Controller == "" || manifest.Controller == "default" || manifest.Deployment == "" || len(manifest.Namespaces) == 0 {
		return errors.New("explicit controller identity and dedicated workload namespaces are required")
	}
	if len(validation.IsDNS1123Label(manifest.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(manifest.Controller)) != 0 || len(validation.IsDNS1123Subdomain(manifest.Deployment)) != 0 {
		return errors.New("invalid installation namespace or controller name")
	}
	seenNamespaces := map[string]bool{}
	for _, namespace := range manifest.Namespaces {
		if len(validation.IsDNS1123Label(namespace)) != 0 || namespace == manifest.Namespace || seenNamespaces[namespace] {
			return errors.New("workload namespaces must be valid, unique and separate from the installation namespace")
		}
		seenNamespaces[namespace] = true
	}
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Deployment}, deployment); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if deployment.Name != "" {
		if deployment.DeletionTimestamp != nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 || deployment.Status.Replicas != 0 || deployment.Status.UpdatedReplicas != 0 || deployment.Status.ReadyReplicas != 0 || deployment.Status.AvailableReplicas != 0 || deployment.Status.ObservedGeneration != deployment.Generation {
			return errors.New("controller has not completed shutdown")
		}
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := admin.List(ctx, bindings); err != nil {
		return err
	}
	for _, binding := range bindings.Items {
		if grantsController(binding.Subjects, binding.Namespace, manifest) {
			return fmt.Errorf("revoke controller RoleBinding %s/%s before bootstrap", binding.Namespace, binding.Name)
		}
	}
	clusterBindings := &rbacv1.ClusterRoleBindingList{}
	if err := admin.List(ctx, clusterBindings); err != nil {
		return err
	}
	for _, binding := range clusterBindings.Items {
		if !grantsController(binding.Subjects, "", manifest) {
			continue
		}
		if binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "ClusterRole" || !slices.Contains([]string{"system:discovery", "system:basic-user", "system:public-info-viewer", "system:service-account-issuer-discovery"}, binding.RoleRef.Name) {
			return fmt.Errorf("revoke controller ClusterRoleBinding %s before bootstrap", binding.Name)
		}
		role := &rbacv1.ClusterRole{}
		if err := admin.Get(ctx, client.ObjectKey{Name: binding.RoleRef.Name}, role); err != nil {
			return err
		}
		if !safeSharedRole(role) {
			return fmt.Errorf("unexpected shared controller grant %s: role rules exceed discovery and self-access review", binding.Name)
		}
	}
	pods := &corev1.PodList{}
	if err := admin.List(ctx, pods); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if pod.Namespace == manifest.Namespace && (pod.Spec.ServiceAccountName == manifest.Controller || pod.Spec.DeprecatedServiceAccount == manifest.Controller) {
			return fmt.Errorf("controller Pod %s still exists", pod.Name)
		}
		for _, namespace := range manifest.Namespaces {
			if pod.Namespace == namespace {
				return fmt.Errorf("drain workload Pod %s/%s before activation", namespace, pod.Name)
			}
		}
	}
	for _, namespace := range append([]string{manifest.Namespace}, manifest.Namespaces...) {
		for _, kind := range []schema.GroupVersionKind{
			{Group: "apps", Version: "v1", Kind: "DeploymentList"}, {Group: "apps", Version: "v1", Kind: "ReplicaSetList"},
			{Group: "apps", Version: "v1", Kind: "StatefulSetList"}, {Group: "apps", Version: "v1", Kind: "DaemonSetList"},
			{Group: "batch", Version: "v1", Kind: "JobList"}, {Group: "batch", Version: "v1", Kind: "CronJobList"},
			{Version: "v1", Kind: "ReplicationControllerList"},
		} {
			objects := &unstructured.UnstructuredList{}
			objects.SetGroupVersionKind(kind)
			if err := admin.List(ctx, objects, client.InNamespace(namespace)); err != nil {
				return err
			}
			controllerListed := false
			for _, object := range objects.Items {
				if namespace == manifest.Namespace {
					if kind.Kind == "DeploymentList" && object.GetName() == manifest.Deployment {
						controllerListed = true
						replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
						if deployment.Name == "" || object.GetUID() != deployment.UID || object.GetGeneration() != deployment.Generation || object.GetDeletionTimestamp() != nil || err != nil || !found || replicas != 0 {
							return errors.New("controller Deployment changed during drain verification")
						}
						continue
					}
					path := []string{"spec", "template", "spec", "serviceAccountName"}
					if kind.Kind == "CronJobList" {
						path = []string{"spec", "jobTemplate", "spec", "template", "spec", "serviceAccountName"}
					}
					account, _, err := unstructured.NestedString(object.Object, path...)
					if err != nil {
						return err
					}
					path[len(path)-1] = "serviceAccount"
					legacyAccount, _, err := unstructured.NestedString(object.Object, path...)
					if err != nil {
						return err
					}
					if account != manifest.Controller && legacyAccount != manifest.Controller {
						continue
					}
					if kind.Kind == "ReplicaSetList" && deployment.UID != "" && object.GetDeletionTimestamp() == nil {
						owner := metav1.GetControllerOf(&object)
						replicas, found, replicasErr := unstructured.NestedInt64(object.Object, "spec", "replicas")
						current, currentFound, currentErr := unstructured.NestedInt64(object.Object, "status", "replicas")
						observed, observedFound, observedErr := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
						stoppedStatus := true
						for _, field := range []string{"readyReplicas", "availableReplicas", "fullyLabeledReplicas", "terminatingReplicas"} {
							count, _, err := unstructured.NestedInt64(object.Object, "status", field)
							if err != nil || count != 0 {
								stoppedStatus = false
							}
						}
						if owner != nil && owner.APIVersion == "apps/v1" && owner.Kind == "Deployment" && owner.Name == deployment.Name && owner.UID == deployment.UID && replicasErr == nil && currentErr == nil && observedErr == nil && found && currentFound && observedFound && replicas == 0 && current == 0 && stoppedStatus && object.GetGeneration() > 0 && observed == object.GetGeneration() {
							continue
						}
					}
				}
				return fmt.Errorf("drain workload producer %s %s/%s before activation", strings.TrimSuffix(kind.Kind, "List"), namespace, object.GetName())
			}
			if namespace == manifest.Namespace && kind.Kind == "DeploymentList" && deployment.Name != "" && !controllerListed {
				return errors.New("controller Deployment disappeared during drain verification")
			}
		}
	}
	return nil
}

func safeSharedRole(role *rbacv1.ClusterRole) bool {
	if role.AggregationRule != nil {
		return false
	}
	for _, rule := range role.Rules {
		if len(rule.NonResourceURLs) > 0 {
			if !reflect.DeepEqual(rule.Verbs, []string{"get"}) || len(rule.APIGroups) != 0 || len(rule.Resources) != 0 || len(rule.ResourceNames) != 0 {
				return false
			}
			for _, path := range rule.NonResourceURLs {
				if !slices.Contains([]string{"/api", "/api/*", "/apis", "/apis/*", "/healthz", "/livez", "/openapi", "/openapi/*", "/readyz", "/version", "/version/", "/.well-known/openid-configuration", "/.well-known/openid-configuration/", "/openid/v1/jwks", "/openid/v1/jwks/"}, path) {
					return false
				}
			}
			continue
		}
		if !reflect.DeepEqual(rule.Verbs, []string{"create"}) || len(rule.APIGroups) != 1 || len(rule.Resources) == 0 {
			return false
		}
		for _, resource := range rule.Resources {
			if !(rule.APIGroups[0] == "authorization.k8s.io" && slices.Contains([]string{"selfsubjectaccessreviews", "selfsubjectrulesreviews"}, resource)) && !(rule.APIGroups[0] == "authentication.k8s.io" && resource == "selfsubjectreviews") {
				return false
			}
		}
	}
	return true
}

func (manifest Manifest) validate() error {
	if manifest.Namespace == "" || manifest.Controller == "" || manifest.Controller == "default" || manifest.Deployment == "" || manifest.Guard == "" || manifest.Controller == manifest.Guard || len(manifest.Namespaces) == 0 {
		return errors.New("incomplete bootstrap manifest")
	}
	counts := map[string]int{}
	baselines := map[string]bool{}
	denies := map[string]bool{}
	policies := map[string]bool{}
	bindings := map[string]bool{}
	versions := map[string]string{
		"CiliumNetworkPolicy":            "cilium.io/v2",
		"CiliumClusterwideNetworkPolicy": "cilium.io/v2",
		"ValidatingAdmissionPolicy":      "admissionregistration.k8s.io/v1", "ValidatingAdmissionPolicyBinding": "admissionregistration.k8s.io/v1", "ValidatingWebhookConfiguration": "admissionregistration.k8s.io/v1",
		"Deployment": "apps/v1", "Service": "v1", "ServiceAccount": "v1", "NetworkPolicy": "networking.k8s.io/v1", "Role": "rbac.authorization.k8s.io/v1", "RoleBinding": "rbac.authorization.k8s.io/v1",
	}
	seen := map[string]bool{}
	for _, namespace := range manifest.Namespaces {
		if namespace == "" || namespace == manifest.Namespace || seen[namespace] {
			return errors.New("workload namespaces must be unique and separate from installation")
		}
		seen[namespace] = true
	}
	seen = map[string]bool{}
	for _, object := range manifest.Objects {
		if object == nil {
			return errors.New("nil bootstrap object")
		}
		if version, supported := versions[object.GetKind()]; !supported || object.GetAPIVersion() != version {
			return fmt.Errorf("unsupported bootstrap GVK %s", object.GroupVersionKind())
		}
		if object.GetDeletionTimestamp() != nil || len(object.GetOwnerReferences()) != 0 {
			return errors.New("bootstrap objects must be independent and not deleting")
		}
		key := object.GetKind() + "/" + object.GetNamespace() + "/" + object.GetName()
		if object.GetName() == "" || seen[key] {
			return errors.New("unnamed or duplicate bootstrap object")
		}
		seen[key] = true
		counts[object.GetKind()]++
		switch object.GetKind() {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "ValidatingWebhookConfiguration":
			if object.GetAPIVersion() != "admissionregistration.k8s.io/v1" || object.GetNamespace() != "" {
				return errors.New("invalid admission object scope or version")
			}
		case "Deployment", "Service", "ServiceAccount":
			if object.GetNamespace() != manifest.Namespace || object.GetName() != manifest.Guard {
				return errors.New("guard identity mismatch")
			}
		case "CiliumNetworkPolicy":
			if !manifest.isCiliumAPIEgress(object) && manifest.probeComponent(object) == "" && !slices.Contains(manifest.Namespaces, object.GetNamespace()) {
				return errors.New("unexpected workload namespace")
			}
		case "CiliumClusterwideNetworkPolicy":
			if !manifest.isRuntimeProbe(object) {
				return errors.New("unexpected cluster-wide Cilium policy")
			}
		case "NetworkPolicy", "Role", "RoleBinding":
			if !slices.Contains(manifest.Namespaces, object.GetNamespace()) {
				return errors.New("unexpected workload namespace")
			}
		default:
			return fmt.Errorf("unexpected bootstrap kind %s", object.GetKind())
		}
		if err := manifest.validateObject(object); err != nil {
			return fmt.Errorf("%s %s: %w", object.GetKind(), object.GetName(), err)
		}
		switch object.GetKind() {
		case "CiliumNetworkPolicy":
			if manifest.isCiliumAPIEgress(object) || manifest.probeComponent(object) != "" {
				continue
			}
			if denies[object.GetNamespace()] || !strings.HasSuffix(object.GetName(), "-stdio-deny") {
				return errors.New("invalid stdio deny policy identity")
			}
			denies[object.GetNamespace()] = true
		case "NetworkPolicy":
			if baselines[object.GetNamespace()] {
				return errors.New("duplicate stdio baseline namespace")
			}
			baselines[object.GetNamespace()] = true
		case "ValidatingAdmissionPolicy":
			policies[object.GetName()] = true
		case "ValidatingAdmissionPolicyBinding":
			policy, _, _ := unstructured.NestedString(object.Object, "spec", "policyName")
			if policy == "" || policy != object.GetName() || bindings[policy] {
				return errors.New("invalid policy-to-binding pairing")
			}
			bindings[policy] = true
		}
	}
	if counts["ValidatingAdmissionPolicy"] != 10 || counts["ValidatingAdmissionPolicyBinding"] != 10 || counts["Deployment"] != 1 || counts["Service"] != 1 || counts["ServiceAccount"] != 1 || counts["ValidatingWebhookConfiguration"] != 1 || counts["NetworkPolicy"] != len(manifest.Namespaces) || counts["Role"] != len(manifest.Namespaces) || counts["RoleBinding"] != len(manifest.Namespaces) {
		return errors.New("incomplete bootstrap policy and guard inventory")
	}
	switch manifest.StdioIsolationProfile {
	case "":
		if len(denies) != 0 {
			return errors.New("Cilium deny policies require the cilium isolation profile")
		}
	case "cilium":
		if !reflect.DeepEqual(denies, baselines) {
			return errors.New("cilium isolation requires a deny policy in every workload namespace")
		}
	default:
		return errors.New("unsupported stdio isolation profile")
	}
	if !reflect.DeepEqual(policies, bindings) {
		return errors.New("invalid policy-to-binding pairing")
	}
	gatewayLeasePolicy := strings.TrimSuffix(manifest.Deployment, "-k8s-controller") + "-" + manifest.Namespace + "-gateway-leases"
	if !policies[gatewayLeasePolicy] || !bindings[gatewayLeasePolicy] {
		return errors.New("gateway Lease enforcement is required for activation")
	}
	statePolicy := strings.TrimSuffix(manifest.Deployment, "-k8s-controller") + "-" + manifest.Namespace + "-state"
	if !policies[statePolicy] || !bindings[statePolicy] {
		return errors.New("installation state enforcement is required for activation")
	}
	for _, namespace := range manifest.Namespaces {
		if !baselines[namespace] || !seen["Role/"+namespace+"/"+manifest.Guard] || !seen["RoleBinding/"+namespace+"/"+manifest.Guard] {
			return errors.New("missing guard permissions in workload namespace")
		}
	}
	return nil
}

func grantsController(subjects []rbacv1.Subject, namespace string, manifest Manifest) bool {
	username := "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller
	for _, subject := range subjects {
		switch subject.Kind {
		case "ServiceAccount":
			subjectNamespace := subject.Namespace
			if subjectNamespace == "" {
				subjectNamespace = namespace
			}
			if subjectNamespace == manifest.Namespace && subject.Name == manifest.Controller {
				return true
			}
		case "User":
			if subject.Name == username {
				return true
			}
		case "Group":
			if subject.Name == "system:authenticated" || subject.Name == "system:serviceaccounts" || subject.Name == "system:serviceaccounts:"+manifest.Namespace {
				return true
			}
		}
	}
	return false
}

func verifyObject(ctx context.Context, admin client.Client, expected *unstructured.Unstructured) error {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(expected.GroupVersionKind())
	if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
		return err
	}
	identity := expected.GetKind() + " " + expected.GetNamespace() + "/" + expected.GetName()
	if live.GetDeletionTimestamp() != nil {
		return fmt.Errorf("%s is deleting", identity)
	}
	normalized := expected.DeepCopy()
	normalized.SetResourceVersion(live.GetResourceVersion())
	normalized.SetUID(live.GetUID())
	if expected.GetKind() == "Service" {
		for _, field := range []string{"clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy"} {
			if _, configured, err := unstructured.NestedFieldNoCopy(expected.Object, "spec", field); err != nil {
				return err
			} else if configured {
				continue
			}
			value, found, err := unstructured.NestedFieldCopy(live.Object, "spec", field)
			if err != nil {
				return err
			}
			if found {
				if err := unstructured.SetNestedField(normalized.Object, value, "spec", field); err != nil {
					return err
				}
			}
		}
	}
	if expected.GetKind() == "ValidatingWebhookConfiguration" || expected.GetKind() == "MutatingWebhookConfiguration" {
		webhooks, _, _ := unstructured.NestedSlice(normalized.Object, "webhooks")
		current, _, _ := unstructured.NestedSlice(live.Object, "webhooks")
		if len(webhooks) == 0 || len(current) != len(webhooks) {
			return fmt.Errorf("%s requires the exact reviewed webhooks", identity)
		}
		for index := range webhooks {
			wanted, valid := webhooks[index].(map[string]interface{})
			actual, currentValid := current[index].(map[string]interface{})
			if !valid || !currentValid || wanted["name"] != actual["name"] {
				return fmt.Errorf("%s webhook identity mismatch", identity)
			}
			bundle, found, err := unstructured.NestedString(actual, "clientConfig", "caBundle")
			decoded, decodeErr := base64.StdEncoding.DecodeString(bundle)
			if err != nil || !found || decodeErr != nil || !x509.NewCertPool().AppendCertsFromPEM(decoded) {
				return fmt.Errorf("%s has no valid serving CA", identity)
			}
			if _, configured, err := unstructured.NestedFieldNoCopy(wanted, "clientConfig", "caBundle"); err != nil {
				return err
			} else if !configured {
				if err := unstructured.SetNestedField(wanted, bundle, "clientConfig", "caBundle"); err != nil {
					return err
				}
			}
		}
		if err := unstructured.SetNestedSlice(normalized.Object, webhooks, "webhooks"); err != nil {
			return err
		}
	}
	if err := admin.Update(ctx, normalized, client.DryRunAll); err != nil {
		return fmt.Errorf("normalize %s: %w", identity, err)
	}
	for _, field := range []string{"spec", "specs", "webhooks", "rules", "subjects", "roleRef", "automountServiceAccountToken", "imagePullSecrets", "secrets"} {
		if !reflect.DeepEqual(normalized.Object[field], live.Object[field]) {
			return fmt.Errorf("%s differs from the intended bootstrap manifest (%s)", identity, field)
		}
	}
	if len(live.GetOwnerReferences()) != 0 {
		return fmt.Errorf("%s ownership drift", identity)
	}
	if expected.GetKind() == "NetworkPolicy" || expected.GetKind() == "CiliumNetworkPolicy" {
		if !reflect.DeepEqual(expected.GetLabels(), live.GetLabels()) || !reflect.DeepEqual(expected.GetAnnotations(), live.GetAnnotations()) {
			return fmt.Errorf("%s baseline ownership drift", identity)
		}
	}
	return verifyPolicyStatus(live)
}

func verifyPolicyStatus(live *unstructured.Unstructured) error {
	identity := live.GetKind() + " " + live.GetName()
	if live.GetKind() == "ValidatingAdmissionPolicy" {
		generation, found, err := unstructured.NestedInt64(live.Object, "status", "observedGeneration")
		if err != nil || !found || generation != live.GetGeneration() || generation < 1 {
			return fmt.Errorf("%s has not passed current-generation type checking", identity)
		}
		typeChecking, found, err := unstructured.NestedMap(live.Object, "status", "typeChecking")
		if err != nil || !found || typeChecking == nil {
			return fmt.Errorf("%s has missing or malformed type checking", identity)
		}
		warnings, found, err := unstructured.NestedSlice(typeChecking, "expressionWarnings")
		if err != nil || (found && warnings == nil) || len(warnings) != 0 {
			return fmt.Errorf("%s has malformed type checking or expression warnings", identity)
		}
		conditions, found, err := unstructured.NestedSlice(live.Object, "status", "conditions")
		if err != nil || (found && conditions == nil) {
			return fmt.Errorf("%s has malformed conditions", identity)
		}
		seen := map[string]bool{}
		for _, condition := range conditions {
			encoded, err := json.Marshal(condition)
			if err != nil {
				return err
			}
			var status metav1.Condition
			if err := json.Unmarshal(encoded, &status); err != nil {
				return err
			}
			if status.Type == "" || seen[status.Type] || status.Status != metav1.ConditionTrue || status.ObservedGeneration != generation || status.LastTransitionTime.IsZero() || status.Reason == "" {
				return fmt.Errorf("%s reports malformed, stale or unsuccessful condition %s", identity, status.Type)
			}
			seen[status.Type] = true
		}
	}
	return nil
}
