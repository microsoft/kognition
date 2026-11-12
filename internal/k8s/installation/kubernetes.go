// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Kubernetes struct {
	Plan       Plan
	Manifest   activation.Manifest
	Admin      client.Client
	Discovery  discovery.DiscoveryInterface
	Render     func(context.Context) error
	Apply      func(context.Context) error
	Journal    *Journal
	recovering bool
}

func (operations *Kubernetes) Preflight(ctx context.Context) error {
	if operations.Admin == nil || operations.Discovery == nil || operations.Render == nil || operations.Apply == nil {
		return errors.New("missing installation dependencies")
	}
	if operations.Plan.ReleaseRevision == 0 && (len(operations.Plan.Controllers) != 0 || len(operations.Plan.Grants) != 0 || len(operations.Plan.LegacyPolicies) != 0 || len(operations.Plan.CompletedMigrationJobs) != 0) {
		return errors.New("fresh installation cannot clean up legacy resources")
	}
	if operations.Plan.ReleaseRevision > 0 && operations.Plan.FailedRelease == nil && len(operations.Plan.Controllers) == 0 {
		return errors.New("upgrade requires an exact old controller inventory")
	}
	if operations.Plan.FailedRelease != nil {
		if err := operations.VerifyDrained(ctx); err != nil {
			return err
		}
	}
	if err := operations.VerifyTarget(ctx); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, controller := range operations.Plan.Controllers {
		ref := controller.Deployment
		if seen[ref.Name] {
			return errors.New("duplicate controller deployment")
		}
		seen[ref.Name] = true
		object, err := operations.reviewed(ctx, ref)
		if err != nil {
			return err
		}
		if err := operations.verifyController(controller, object); err != nil {
			return err
		}
	}
	for _, ref := range operations.Plan.Grants {
		object, err := operations.reviewed(ctx, ref)
		if err != nil {
			return err
		}
		if err := operations.verifyGrant(ref, object); err != nil {
			return err
		}
	}
	for _, ref := range operations.Plan.LegacyPolicies {
		object, err := operations.reviewed(ctx, ref)
		if err != nil {
			return err
		}
		if err := operations.verifyLegacyPolicy(ref, object); err != nil {
			return err
		}
	}
	for _, ref := range operations.Plan.CompletedMigrationJobs {
		object, err := operations.reviewed(ctx, ref)
		if err != nil {
			return err
		}
		if err := operations.verifyCompletedMigration(ctx, ref, object); err != nil {
			return err
		}
	}
	if err := operations.VerifyReleaseObjects(ctx); err != nil {
		return err
	}
	return operations.Render(ctx)
}

func (operations *Kubernetes) verifyController(controller Controller, object *unstructured.Unstructured) error {
	ref := controller.Deployment
	if ref.APIVersion != "apps/v1" || ref.Kind != "Deployment" || ref.Namespace != operations.Manifest.Namespace || controller.ServiceAccount == "" || controller.ServiceAccount == "default" {
		return errors.New("invalid reviewed controller identity")
	}
	account, _, err := unstructured.NestedString(object.Object, "spec", "template", "spec", "serviceAccountName")
	if err != nil || account != controller.ServiceAccount {
		return errors.New("controller service account differs from review")
	}
	return operations.releaseOwner(object)
}

func (operations *Kubernetes) verifyGrant(ref ObjectRef, object *unstructured.Unstructured) error {
	if ref.APIVersion != rbacv1.SchemeGroupVersion.String() || (ref.Kind != "RoleBinding" && ref.Kind != "ClusterRoleBinding") || (ref.Kind == "ClusterRoleBinding" && ref.Namespace != "") || (ref.Kind == "RoleBinding" && ref.Namespace == "") {
		return errors.New("invalid reviewed grant")
	}
	if err := operations.releaseOwner(object); err != nil {
		_, releaseName := object.GetAnnotations()["meta.helm.sh/release-name"]
		_, releaseNamespace := object.GetAnnotations()["meta.helm.sh/release-namespace"]
		if len(object.GetOwnerReferences()) != 0 || object.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" || releaseName || releaseNamespace {
			return err
		}
		if _, err := operations.cleanupAttestation(ref); err != nil {
			return err
		}
	}
	subjects, found, err := unstructured.NestedSlice(object.Object, "subjects")
	if err != nil || !found || len(subjects) == 0 {
		return errors.New("reviewed grant has no subjects")
	}
	for _, value := range subjects {
		subject, ok := value.(map[string]any)
		if !ok || subject["kind"] != "ServiceAccount" || subject["namespace"] != operations.Manifest.Namespace {
			return errors.New("shared or non-controller grants require explicit external migration")
		}
		name, _ := subject["name"].(string)
		if name == "" || name == "default" || !slices.Contains(operations.accounts(), name) {
			return errors.New("grant also authorizes an unrelated identity")
		}
	}
	return nil
}

func (operations *Kubernetes) cleanupAttestation(ref ObjectRef) (CleanupAttestation, error) {
	var matching []CleanupAttestation
	for _, attestation := range operations.Plan.CleanupAttestations {
		if attestation.ResourceUID == ref.UID {
			matching = append(matching, attestation)
		}
	}
	if len(matching) != 1 || strings.TrimSpace(matching[0].Evidence) == "" {
		return CleanupAttestation{}, errors.New("non-Helm cleanup requires one administrator-reviewed lineage attestation")
	}
	for _, controller := range operations.Plan.Controllers {
		if controller.Deployment.UID == matching[0].ControllerUID && matching[0].ControllerUID != "" {
			return matching[0], nil
		}
	}
	return CleanupAttestation{}, errors.New("cleanup lineage does not identify a reviewed controller UID")
}

func (operations *Kubernetes) verifyLegacyPolicy(ref ObjectRef, object *unstructured.Unstructured) error {
	if ref.APIVersion != "networking.k8s.io/v1" || ref.Kind != "NetworkPolicy" || !slices.Contains(operations.Manifest.Namespaces, ref.Namespace) {
		return errors.New("legacy policy must belong to an enrolled workload namespace")
	}
	for _, expected := range operations.Manifest.Objects {
		if expected.GetKind() == ref.Kind && expected.GetNamespace() == ref.Namespace && expected.GetName() == ref.Name {
			return errors.New("cannot remove the reviewed baseline as a legacy policy")
		}
	}
	attestation, err := operations.cleanupAttestation(ref)
	if err != nil {
		return err
	}
	if attestation.InvocationID == "" || len(object.GetOwnerReferences()) != 0 {
		return errors.New("legacy orphan policy requires explicit invocation lineage and no live owner")
	}
	labels := object.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "kognition" || labels["kognition.dev/stdio-invocation"] != attestation.InvocationID {
		return errors.New("legacy policy invocation identity differs from attestation")
	}
	selector, found, err := unstructured.NestedStringMap(object.Object, "spec", "podSelector", "matchLabels")
	if err != nil || !found || len(selector) != 2 || selector["app.kubernetes.io/managed-by"] != "kognition" || selector["kognition.dev/stdio-invocation"] != attestation.InvocationID {
		return errors.New("legacy policy selector differs from reviewed invocation")
	}
	for _, path := range [][]string{{"spec", "egress"}, {"spec", "ingress"}, {"spec", "podSelector", "matchExpressions"}} {
		values, _, err := unstructured.NestedSlice(object.Object, path...)
		if err != nil || len(values) != 0 {
			return errors.New("legacy policy is not the orphan deny-egress template")
		}
	}
	policyTypes, _, err := unstructured.NestedStringSlice(object.Object, "spec", "policyTypes")
	if err != nil || len(policyTypes) != 1 || policyTypes[0] != "Egress" {
		return errors.New("legacy policy must deny egress only")
	}
	return nil
}

func (operations *Kubernetes) releaseDocuments(content []byte) ([]*unstructured.Unstructured, error) {
	objects, err := Documents(content)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, object := range objects {
		mapping, err := operations.Admin.RESTMapper().RESTMapping(object.GroupVersionKind().GroupKind(), object.GroupVersionKind().Version)
		if err != nil {
			return nil, err
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			if object.GetNamespace() == "" {
				object.SetNamespace(operations.Manifest.Namespace)
			}
		} else if object.GetNamespace() != "" {
			return nil, errors.New("cluster resource has namespace")
		}
		identity := releaseIdentity(object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName())
		if seen[identity] {
			return nil, fmt.Errorf("duplicate normalized release object: %s", identity)
		}
		seen[identity] = true
	}
	return objects, nil
}

func releaseIdentity(version, kind, namespace, name string) string {
	groupVersion, _ := schema.ParseGroupVersion(version)
	return groupVersion.Group + "/" + kind + "/" + namespace + "/" + name
}

func (operations *Kubernetes) VerifyReleaseObjects(ctx context.Context) error {
	if err := operations.VerifyFailedRelease(ctx); err != nil {
		return err
	}
	expected, err := ReadArtifact(operations.Plan.Manifest)
	if err != nil {
		return err
	}
	next, err := operations.releaseDocuments(expected)
	if err != nil {
		return err
	}
	previous := map[string]*unstructured.Unstructured{}
	for _, artifact := range []*Artifact{operations.Plan.PreviousDeployedManifest, operations.Plan.PreviousManifest} {
		if artifact == nil {
			continue
		}
		content, err := ReadArtifact(*artifact)
		if err != nil {
			return err
		}
		objects, err := operations.releaseDocuments(content)
		if err != nil {
			return err
		}
		for _, object := range objects {
			identity := releaseIdentity(object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName())
			if old := previous[identity]; old != nil && old.GetAPIVersion() != object.GetAPIVersion() {
				return errors.New("failed and deployed release manifests disagree on API version")
			}
			previous[identity] = object
		}
	}
	absent := map[string]bool{}
	for _, ref := range operations.Plan.AbsentObjects {
		identity := releaseIdentity(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
		object := previous[identity]
		if operations.Plan.FailedRelease == nil || object == nil || object.GetAPIVersion() != ref.APIVersion {
			return errors.New("absent inventory must identify a reviewed failed or deployed release object")
		}
		if err := operations.Admin.Get(ctx, client.ObjectKeyFromObject(object), object.DeepCopy()); !apierrors.IsNotFound(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("reviewed absent release object now exists: %s", identity)
		}
		absent[identity] = true
	}
	cleanup := map[string]bool{}
	references := append(append([]ObjectRef{}, operations.Plan.Grants...), operations.Plan.LegacyPolicies...)
	references = append(references, operations.Plan.CompletedMigrationJobs...)
	for _, controller := range operations.Plan.Controllers {
		references = append(references, controller.Deployment)
	}
	for _, ref := range references {
		cleanup[releaseIdentity(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)] = true
	}
	references = append(references, operations.Plan.RetainedObjects...)
	inventory := map[string]ObjectRef{}
	for _, ref := range references {
		identity := releaseIdentity(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
		if _, exists := inventory[identity]; exists {
			return fmt.Errorf("duplicate release inventory: %s", identity)
		}
		inventory[identity] = ref
	}
	retained := map[string]bool{}
	for _, object := range next {
		identity := releaseIdentity(object.GetAPIVersion(), object.GetKind(), object.GetNamespace(), object.GetName())
		retained[identity] = true
		if previous[identity] != nil {
			continue
		}
		live := object.DeepCopy()
		err := operations.Admin.Get(ctx, client.ObjectKeyFromObject(object), live)
		if !apierrors.IsNotFound(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("new release object already exists; external migration required: %s", identity)
		}
	}
	for identity, object := range previous {
		if absent[identity] {
			if _, exists := inventory[identity]; exists {
				return errors.New("release object cannot be both absent and live")
			}
			continue
		}
		ref, exists := inventory[identity]
		if !exists || (!retained[identity] && !cleanup[identity]) {
			return fmt.Errorf("previous release object lacks exact retained or cleanup review: %s", identity)
		}
		if object.GetAPIVersion() != ref.APIVersion {
			return errors.New("release inventory API version differs from previous manifest")
		}
		live, err := operations.reviewed(ctx, ref)
		if err != nil && operations.Journal != nil {
			if apierrors.IsNotFound(err) && cleanup[identity] {
				_, found, intentErr := operations.Journal.mutation("delete", ref)
				if intentErr != nil {
					return intentErr
				}
				if found {
					continue
				}
			}
			intent, found, intentErr := operations.Journal.mutation("stop", ref)
			if intentErr != nil {
				return intentErr
			}
			if found {
				ref.SHA256 = intent.ResultSHA256
				live, err = operations.reviewed(ctx, ref)
			}
		}
		if err != nil {
			return err
		}
		if err := operations.releaseOwner(live); err != nil {
			return err
		}
	}
	for _, ref := range operations.Plan.RetainedObjects {
		identity := releaseIdentity(ref.APIVersion, ref.Kind, ref.Namespace, ref.Name)
		if previous[identity] == nil || !retained[identity] {
			return fmt.Errorf("retained inventory object not retained by release: %s", identity)
		}
	}
	return nil
}

func (operations *Kubernetes) VerifyTarget(ctx context.Context) error {
	if operations.Manifest.Namespace == "" || operations.Manifest.Namespace == "kube-system" || len(operations.Plan.WorkloadNamespaceUIDs) != len(operations.Manifest.Namespaces) {
		return errors.New("installation and workload namespace identity inventory is invalid")
	}
	namespaces := map[string]string{"kube-system": operations.Plan.ClusterUID, operations.Manifest.Namespace: operations.Plan.NamespaceUID}
	if operations.Plan.AuthorityNamespace == "" || operations.Plan.AuthorityNamespace == "kube-system" || operations.Plan.AuthorityNamespace == operations.Manifest.Namespace || slices.Contains(operations.Manifest.Namespaces, operations.Plan.AuthorityNamespace) {
		return errors.New("authority namespace must be separate from installation and workload namespaces")
	}
	namespaces[operations.Plan.AuthorityNamespace] = operations.Plan.AuthorityNamespaceUID
	for _, namespace := range operations.Manifest.Namespaces {
		uid := operations.Plan.WorkloadNamespaceUIDs[namespace]
		if uid == "" || namespace == "kube-system" || namespace == operations.Manifest.Namespace {
			return errors.New("missing or overlapping workload namespace UID")
		}
		namespaces[namespace] = uid
	}
	for name, uid := range namespaces {
		namespace := &corev1.Namespace{}
		if err := operations.Admin.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
			return err
		}
		if uid == "" || string(namespace.UID) != uid || namespace.DeletionTimestamp != nil {
			return fmt.Errorf("namespace identity changed: %s", name)
		}
	}
	names := []string{"", JournalKey(operations.Plan).Name}
	roles := &rbacv1.RoleList{}
	if err := operations.Admin.List(ctx, roles, client.InNamespace(operations.Plan.AuthorityNamespace)); err != nil {
		return err
	}
	clusterRoles := &rbacv1.ClusterRoleList{}
	if err := operations.Admin.List(ctx, clusterRoles); err != nil {
		return err
	}
	var rules []rbacv1.PolicyRule
	for _, role := range roles.Items {
		rules = append(rules, role.Rules...)
	}
	for _, role := range clusterRoles.Items {
		rules = append(rules, role.Rules...)
	}
	for _, rule := range rules {
		if (slices.Contains(rule.APIGroups, "") || slices.Contains(rule.APIGroups, "*")) && (slices.Contains(rule.Resources, "configmaps") || slices.Contains(rule.Resources, "*")) {
			for _, name := range rule.ResourceNames {
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	for _, account := range operations.accounts() {
		for _, resource := range []string{"configmaps", "namespaces"} {
			resourceNames := names
			if resource == "namespaces" {
				resourceNames = []string{"", operations.Plan.AuthorityNamespace, "kube-system"}
			}
			for _, name := range resourceNames {
				for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
					attributes := &authv1.ResourceAttributes{Namespace: operations.Plan.AuthorityNamespace, Resource: resource, Name: name, Verb: verb}
					if resource == "namespaces" {
						attributes.Namespace = ""
					}
					review := &authv1.SubjectAccessReview{Spec: authv1.SubjectAccessReviewSpec{User: "system:serviceaccount:" + operations.Manifest.Namespace + ":" + account, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + operations.Manifest.Namespace, "system:authenticated"}, ResourceAttributes: attributes}}
					if err := operations.Admin.Create(ctx, review); err != nil {
						return err
					}
					if review.Status.Allowed || review.Status.EvaluationError != "" {
						return errors.New("controller can mutate installation authority or authorization is indeterminate")
					}
				}
			}
		}
	}
	return nil
}

func (operations *Kubernetes) reviewed(ctx context.Context, ref ObjectRef) (*unstructured.Unstructured, error) {
	if ref.UID == "" || ref.Name == "" || len(ref.SHA256) != 64 {
		return nil, errors.New("reviewed object requires exact name, UID and digest")
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(ref.APIVersion)
	object.SetKind(ref.Kind)
	if err := operations.Admin.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, object); err != nil {
		return nil, err
	}
	digest, err := ObjectDigest(object)
	if err != nil {
		return nil, err
	}
	if string(object.GetUID()) != ref.UID || digest != ref.SHA256 || object.GetDeletionTimestamp() != nil || len(object.GetFinalizers()) != 0 {
		return nil, fmt.Errorf("reviewed object changed, deleting or has finalizers: %s %s/%s", ref.Kind, ref.Namespace, ref.Name)
	}
	return object, nil
}

func (operations *Kubernetes) releaseOwner(object *unstructured.Unstructured) error {
	annotations := object.GetAnnotations()
	if annotations["meta.helm.sh/release-name"] != operations.Plan.Release || annotations["meta.helm.sh/release-namespace"] != operations.Manifest.Namespace || object.GetLabels()["app.kubernetes.io/managed-by"] != "Helm" || len(object.GetOwnerReferences()) != 0 {
		return fmt.Errorf("object is not independently owned by the reviewed Helm release: %s %s", object.GetKind(), object.GetName())
	}
	return nil
}

func (operations *Kubernetes) accounts() []string {
	accounts := []string{operations.Manifest.Controller}
	for _, controller := range operations.Plan.Controllers {
		accounts = append(accounts, controller.ServiceAccount)
	}
	return accounts
}

func (operations *Kubernetes) WorkloadsDrained(ctx context.Context) error {
	groups, err := operations.Discovery.ServerPreferredResources()
	if err != nil {
		return err
	}
	for _, group := range groups {
		version, err := schema.ParseGroupVersion(group.GroupVersion)
		if err != nil {
			return err
		}
		if !strings.Contains(version.Group, "kognition") {
			continue
		}
		for _, resource := range group.APIResources {
			if strings.Contains(resource.Name, "/") || !slices.Contains(resource.Verbs, "list") {
				continue
			}
			objects := &unstructured.UnstructuredList{}
			objects.SetGroupVersionKind(version.WithKind(resource.Kind + "List"))
			if err := operations.Admin.List(ctx, objects); err != nil {
				return err
			}
			for _, object := range objects.Items {
				if object.GetNamespace() == "" || object.GetNamespace() == operations.Manifest.Namespace || slices.Contains(operations.Manifest.Namespaces, object.GetNamespace()) {
					retained, err := operations.retainedDefaultMemoryClass(ctx, &object)
					if err != nil {
						return err
					}
					if retained {
						continue
					}
					return fmt.Errorf("complete controlled drain/finalization or external migration before stopping the controller: %s %s/%s", resource.Kind, object.GetNamespace(), object.GetName())
				}
			}
		}
	}
	for _, namespace := range append([]string{operations.Manifest.Namespace}, operations.Manifest.Namespaces...) {
		for _, kind := range []schema.GroupVersionKind{
			{Version: "v1", Kind: "PodList"}, {Version: "v1", Kind: "ReplicationControllerList"},
			{Group: "apps", Version: "v1", Kind: "DeploymentList"}, {Group: "apps", Version: "v1", Kind: "ReplicaSetList"},
			{Group: "apps", Version: "v1", Kind: "StatefulSetList"}, {Group: "apps", Version: "v1", Kind: "DaemonSetList"},
			{Group: "batch", Version: "v1", Kind: "JobList"}, {Group: "batch", Version: "v1", Kind: "CronJobList"},
		} {
			objects := &unstructured.UnstructuredList{}
			objects.SetGroupVersionKind(kind)
			if err := operations.Admin.List(ctx, objects, client.InNamespace(namespace)); err != nil {
				return err
			}
			for _, object := range objects.Items {
				if namespace != operations.Manifest.Namespace {
					return fmt.Errorf("workload producer remains: %s %s/%s", kind.Kind, namespace, object.GetName())
				}
				if len(object.GetFinalizers()) != 0 && object.GetDeletionTimestamp() != nil {
					return fmt.Errorf("installation workload still finalizing: %s", object.GetName())
				}
				path := []string{"spec", "template", "spec", "serviceAccountName"}
				if kind.Kind == "PodList" {
					path = []string{"spec", "serviceAccountName"}
				}
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
				if slices.Contains(operations.accounts(), account) || slices.Contains(operations.accounts(), legacyAccount) {
					if err := operations.controllerWorkload(ctx, &object); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (operations *Kubernetes) retainedDefaultMemoryClass(ctx context.Context, object *unstructured.Unstructured) (bool, error) {
	if object.GetAPIVersion() != "kognition.dev/v1alpha1" || object.GetKind() != "MemoryClass" || object.GetNamespace() != "" || object.GetName() != "default" ||
		operations.Plan.ReleaseRevision < 1 || operations.Plan.PreviousManifest == nil {
		return false, nil
	}
	var reference *ObjectRef
	for index := range operations.Plan.RetainedObjects {
		candidate := &operations.Plan.RetainedObjects[index]
		if candidate.APIVersion == object.GetAPIVersion() && candidate.Kind == object.GetKind() && candidate.Namespace == "" && candidate.Name == "default" {
			if reference != nil {
				return false, errors.New("duplicate retained default MemoryClass review")
			}
			reference = candidate
		}
	}
	if reference == nil {
		return false, nil
	}
	live, err := operations.reviewed(ctx, *reference)
	if err != nil {
		return false, err
	}
	if live.GetUID() != object.GetUID() || operations.releaseOwner(live) != nil {
		return false, errors.New("retained default MemoryClass differs from the reviewed release identity")
	}
	for _, artifact := range []Artifact{*operations.Plan.PreviousManifest, operations.Plan.Manifest} {
		content, err := ReadArtifact(artifact)
		if err != nil {
			return false, err
		}
		documents, err := Documents(content)
		if err != nil {
			return false, err
		}
		found := 0
		for _, expected := range documents {
			if expected.GetAPIVersion() == live.GetAPIVersion() && expected.GetKind() == live.GetKind() && expected.GetNamespace() == "" && expected.GetName() == "default" {
				found++
				if !reflect.DeepEqual(expected.Object["spec"], live.Object["spec"]) {
					return false, errors.New("retained default MemoryClass configuration changes require explicit migration")
				}
			}
		}
		if found != 1 {
			return false, errors.New("retained default MemoryClass must exist unchanged in both reviewed renders")
		}
	}
	return true, nil
}

func (operations *Kubernetes) controllerWorkload(ctx context.Context, object *unstructured.Unstructured) error {
	if object.GetKind() == "Deployment" {
		for _, controller := range operations.Plan.Controllers {
			ref := controller.Deployment
			if object.GetAPIVersion() == ref.APIVersion && object.GetName() == ref.Name && object.GetNamespace() == ref.Namespace && string(object.GetUID()) == ref.UID && object.GetDeletionTimestamp() == nil {
				return nil
			}
		}
	}
	owner := metav1.GetControllerOf(object)
	if owner == nil || owner.APIVersion != "apps/v1" || !((object.GetKind() == "Pod" && owner.Kind == "ReplicaSet") || (object.GetKind() == "ReplicaSet" && owner.Kind == "Deployment")) {
		return fmt.Errorf("migrate controller-identity producer before shutdown: %s %s/%s", object.GetKind(), object.GetNamespace(), object.GetName())
	}
	parent := &unstructured.Unstructured{}
	parent.SetAPIVersion(owner.APIVersion)
	parent.SetKind(owner.Kind)
	if err := operations.Admin.Get(ctx, client.ObjectKey{Namespace: object.GetNamespace(), Name: owner.Name}, parent); err != nil {
		return err
	}
	if parent.GetUID() != owner.UID || parent.GetDeletionTimestamp() != nil {
		return errors.New("controller workload owner replaced or deleting")
	}
	return operations.controllerWorkload(ctx, parent)
}

func (operations *Kubernetes) StopControllers(ctx context.Context) error {
	for _, controller := range operations.Plan.Controllers {
		object, err := operations.reviewed(ctx, controller.Deployment)
		if err != nil {
			if operations.recovering && operations.Journal != nil {
				intent, found, intentErr := operations.Journal.mutation("stop", controller.Deployment)
				if intentErr != nil {
					return intentErr
				}
				if found {
					stopped := controller.Deployment
					stopped.SHA256 = intent.ResultSHA256
					object, err = operations.reviewed(ctx, stopped)
				}
			}
			if err != nil {
				return err
			}
		}
		if err := operations.verifyController(controller, object); err != nil {
			return err
		}
		before := object.DeepCopy()
		replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
		if err != nil || !found {
			return errors.New("reviewed controller lacks explicit replica count")
		}
		if replicas == 0 {
			continue
		}
		if err := unstructured.SetNestedField(object.Object, int64(0), "spec", "replicas"); err != nil {
			return err
		}
		if operations.Journal != nil {
			result := object.DeepCopy()
			result.SetGeneration(before.GetGeneration() + 1)
			digest, err := ObjectDigest(result)
			if err != nil {
				return err
			}
			if err := operations.Journal.prepareMutation("stop", controller.Deployment, digest); err != nil {
				return err
			}
		}
		if err := operations.Admin.Patch(ctx, object, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func (operations *Kubernetes) ControllersStopped(ctx context.Context) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		for _, controller := range operations.Plan.Controllers {
			deployment := &appsv1.Deployment{}
			if err := operations.Admin.Get(ctx, client.ObjectKey{Namespace: controller.Deployment.Namespace, Name: controller.Deployment.Name}, deployment); err != nil {
				return false, err
			}
			if string(deployment.UID) != controller.Deployment.UID || deployment.DeletionTimestamp != nil {
				return false, errors.New("controller replaced or deleting during shutdown")
			}
			if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 || deployment.Status.ObservedGeneration != deployment.Generation || deployment.Status.Replicas != 0 || deployment.Status.ReadyReplicas != 0 || deployment.Status.AvailableReplicas != 0 || deployment.Status.UpdatedReplicas != 0 {
				return false, nil
			}
		}
		pods := &corev1.PodList{}
		if err := operations.Admin.List(ctx, pods, client.InNamespace(operations.Manifest.Namespace)); err != nil {
			return false, err
		}
		for _, pod := range pods.Items {
			if slices.Contains(operations.accounts(), pod.Spec.ServiceAccountName) || slices.Contains(operations.accounts(), pod.Spec.DeprecatedServiceAccount) {
				return false, nil
			}
		}
		return true, nil
	})
}

func (operations *Kubernetes) remove(ctx context.Context, references []ObjectRef, validate func(ObjectRef, *unstructured.Unstructured) error) error {
	for _, ref := range references {
		object, err := operations.reviewed(ctx, ref)
		if err != nil {
			if operations.recovering && operations.Journal != nil && apierrors.IsNotFound(err) {
				_, found, intentErr := operations.Journal.mutation("delete", ref)
				if intentErr != nil {
					return intentErr
				}
				if found {
					continue
				}
			}
			return err
		}
		if err := validate(ref, object); err != nil {
			return err
		}
		if operations.Journal != nil {
			if err := operations.Journal.prepareMutation("delete", ref, ""); err != nil {
				return err
			}
		}
		uid, version := object.GetUID(), object.GetResourceVersion()
		if err := operations.Admin.Delete(ctx, object, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil {
			return err
		}
		err = wait.PollUntilContextTimeout(ctx, time.Second, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			live := object.DeepCopy()
			err := operations.Admin.Get(ctx, client.ObjectKeyFromObject(object), live)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if live.GetUID() != types.UID(ref.UID) {
				return false, errors.New("resource replaced during deletion; replacement was not deleted")
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (operations *Kubernetes) RevokeGrants(ctx context.Context) error {
	return operations.remove(ctx, operations.Plan.Grants, operations.verifyGrant)
}
func (operations *Kubernetes) RemoveLegacyPolicies(ctx context.Context) error {
	return operations.remove(ctx, operations.Plan.LegacyPolicies, operations.verifyLegacyPolicy)
}

func (operations *Kubernetes) RecoverCleanup(ctx context.Context, journal *Journal) error {
	if journal == nil || journal.BeforeMutation() || journal.AdmissionStarted() {
		return errors.New("cleanup recovery requires a pre-admission mutation journal")
	}
	if err := operations.VerifyTarget(ctx); err != nil {
		return err
	}
	operations.Journal, operations.recovering = journal, true
	defer func() { operations.recovering = false }()
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"workloads-drained", operations.WorkloadsDrained},
		{"stop-controllers", operations.StopControllers},
		{"controllers-stopped", operations.ControllersStopped},
		{"revoke-grants", operations.RevokeGrants},
		{"verify-drained", operations.VerifyDrained},
		{"remove-legacy-policies", operations.RemoveLegacyPolicies},
		{"remove-completed-migrations", operations.RemoveCompletedMigrations},
	} {
		if err := journal.Record("recovery-" + step.name + ":pending"); err != nil {
			return err
		}
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("recovery %s: %w", step.name, err)
		}
		if err := journal.Record("recovery-" + step.name + ":complete"); err != nil {
			return err
		}
	}
	return nil
}

func (operations *Kubernetes) VerifyDrained(ctx context.Context) error {
	manifests := []activation.Manifest{operations.Manifest}
	for _, controller := range operations.Plan.Controllers {
		manifest := operations.Manifest
		manifest.Controller, manifest.Deployment = controller.ServiceAccount, controller.Deployment.Name
		manifests = append(manifests, manifest)
	}
	for _, manifest := range manifests {
		if err := activation.VerifyDrained(ctx, operations.Admin, manifest); err != nil {
			return err
		}
	}
	return nil
}

func (operations *Kubernetes) Bootstrap(ctx context.Context) error { return operations.Apply(ctx) }
func (operations *Kubernetes) VerifyBootstrapNow(ctx context.Context) error {
	if err := operations.VerifyDrained(ctx); err != nil {
		return err
	}
	return activation.Verify(ctx, operations.Admin, operations.Manifest)
}

func (operations *Kubernetes) VerifyBootstrap(ctx context.Context) error {
	return waitForBootstrap(ctx, 2*time.Second, 2*time.Minute, operations.VerifyDrained, func(ctx context.Context) error {
		return activation.Verify(ctx, operations.Admin, operations.Manifest)
	})
}

func waitForBootstrap(ctx context.Context, interval, timeout time.Duration, drained, verify func(context.Context) error) error {
	var last error
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		if err := drained(ctx); err != nil {
			return false, err
		}
		last = verify(ctx)
		return last == nil, nil
	})
	if err != nil {
		return errors.Join(err, last)
	}
	return nil
}
