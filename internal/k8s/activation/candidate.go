// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Candidate struct {
	manifest          Manifest
	controller        *unstructured.Unstructured
	roles             []*unstructured.Unstructured
	resourceAdmission []*unstructured.Unstructured
	sha256            string
	release           string
}

func ParseCandidate(content []byte, release string) (Candidate, error) {
	manifest, err := Parse(bytes.NewReader(content))
	if err != nil {
		return Candidate{}, err
	}
	if release == "" || len(validation.IsDNS1123Subdomain(release)) != 0 {
		return Candidate{}, errors.New("explicit valid release is required")
	}
	candidate := Candidate{manifest: manifest, sha256: fmt.Sprintf("%x", sha256.Sum256(content)), release: release}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return Candidate{}, err
		}
		if object.GetKind() == "Deployment" && object.GetNamespace() == manifest.Namespace && object.GetName() == manifest.Deployment {
			candidate.controller = object
		}
		if object.GetLabels()["app.kubernetes.io/component"] == "resource-webhooks" || object.GetLabels()["app.kubernetes.io/component"] == "webhook" {
			switch object.GetKind() {
			case "Deployment", "Service", "ServiceAccount", "Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding", "NetworkPolicy", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration":
				candidate.resourceAdmission = append(candidate.resourceAdmission, object)
			}
		}
		if (object.GetKind() != "Role" && object.GetKind() != "ClusterRole") || object.GetLabels()["app.kubernetes.io/component"] != "k8s-controller" {
			continue
		}
		if object.GetAPIVersion() != "rbac.authorization.k8s.io/v1" || object.GetName() == "" || len(object.GetOwnerReferences()) != 0 || object.GetDeletionTimestamp() != nil {
			return Candidate{}, errors.New("invalid controller role")
		}
		if object.GetKind() == "Role" && object.GetNamespace() != manifest.Namespace && !slices.Contains(manifest.Namespaces, object.GetNamespace()) {
			return Candidate{}, errors.New("controller role escapes enrolled namespaces")
		}
		if object.GetKind() == "ClusterRole" {
			if object.GetNamespace() != "" || object.Object["aggregationRule"] != nil {
				return Candidate{}, errors.New("aggregated or namespaced ClusterRole is unsupported")
			}
			rules, found, err := unstructured.NestedSlice(object.Object, "rules")
			if err != nil || !found || len(rules) == 0 {
				return Candidate{}, errors.New("controller ClusterRole rules are required")
			}
			for _, entry := range rules {
				rule, ok := entry.(map[string]interface{})
				if !ok {
					return Candidate{}, errors.New("invalid controller ClusterRole rule")
				}
				groups, found, err := unstructured.NestedStringSlice(rule, "apiGroups")
				if err != nil || !found || len(groups) == 0 || rule["nonResourceURLs"] != nil {
					return Candidate{}, errors.New("controller ClusterRole must only address Kognition resources")
				}
				for _, group := range groups {
					if group != "kognition.dev" && group != "knowledge.kognition.dev" {
						return Candidate{}, errors.New("controller ClusterRole cannot grant Kubernetes administrative access")
					}
				}
			}
		}
		candidate.roles = append(candidate.roles, object)
	}
	if candidate.controller == nil {
		return Candidate{}, errors.New("candidate requires controller Deployment")
	}
	if err := candidate.validateRoles(); err != nil {
		return Candidate{}, err
	}
	return candidate, nil
}

func (candidate Candidate) validateRoles() error {
	clusterName := ""
	for _, role := range candidate.roles {
		if role.GetKind() == "ClusterRole" {
			if clusterName != "" || !strings.HasSuffix(role.GetName(), "-controller") {
				return errors.New("candidate requires exactly one controller ClusterRole")
			}
			clusterName = role.GetName()
		}
	}
	if clusterName == "" {
		return errors.New("candidate requires controller ClusterRole")
	}
	expected := map[string]bool{"ClusterRole//" + clusterName: true, "Role/" + candidate.manifest.Namespace + "/" + strings.TrimSuffix(clusterName, "-controller") + "-quota-ledger": true}
	for _, namespace := range append([]string{candidate.manifest.Namespace}, candidate.manifest.Namespaces...) {
		expected["Role/"+namespace+"/"+clusterName] = true
	}
	for _, role := range candidate.roles {
		key := role.GetKind() + "/" + role.GetNamespace() + "/" + role.GetName()
		if !expected[key] {
			return fmt.Errorf("unexpected or duplicate controller role %s", key)
		}
		delete(expected, key)
	}
	if len(expected) != 0 {
		return errors.New("candidate is missing required controller roles")
	}
	return nil
}

func (candidate Candidate) Snapshot(ctx context.Context, admin client.Client, endpoint, serverVersion, chartSHA256, toolVersion, authorityNamespace string, replicas int32) (ActivationTarget, error) {
	if candidate.controller == nil {
		return ActivationTarget{}, errors.New("candidate must be parsed before preparing activation")
	}
	target := ActivationTarget{
		Endpoint: endpoint, ServerVersion: serverVersion, Release: candidate.release,
		Namespace: candidate.manifest.Namespace, Namespaces: slices.Clone(candidate.manifest.Namespaces),
		Controller: candidate.manifest.Controller, Guard: candidate.manifest.Guard,
		Deployment: candidate.manifest.Deployment, AuthorityNamespace: authorityNamespace,
		ManifestSHA256: candidate.sha256, ChartSHA256: chartSHA256, ToolVersion: toolVersion,
		Replicas: replicas, Images: map[string]string{},
	}
	if authorityNamespace == "" || authorityNamespace == target.Namespace || slices.Contains(target.Namespaces, authorityNamespace) || len(validation.IsDNS1123Label(authorityNamespace)) != 0 {
		return target, errors.New("approval namespace must be separate from installation and workload namespaces")
	}
	if err := Verify(ctx, admin, candidate.manifest); err != nil {
		return target, err
	}
	objects := append([]*unstructured.Unstructured{}, candidate.manifest.Objects...)
	objects = append(objects, candidate.controller)
	objects = append(objects, candidate.roles...)
	for _, reviewed := range candidate.resourceAdmission {
		expected := reviewed
		if reviewed.GetKind() == "NetworkPolicy" {
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(reviewed.GroupVersionKind())
			if err := admin.Get(ctx, client.ObjectKeyFromObject(reviewed), live); err != nil {
				return target, err
			}
			annotations := live.GetAnnotations()
			_, releaseName := annotations["meta.helm.sh/release-name"]
			_, releaseNamespace := annotations["meta.helm.sh/release-namespace"]
			if releaseName || releaseNamespace {
				ownershipError := fmt.Errorf("NetworkPolicy %s/%s baseline ownership drift", reviewed.GetNamespace(), reviewed.GetName())
				if reviewed.GetLabels()["app.kubernetes.io/managed-by"] != "Helm" || annotations["meta.helm.sh/release-name"] != candidate.release || annotations["meta.helm.sh/release-namespace"] != candidate.manifest.Namespace {
					return target, ownershipError
				}
				// Helm adds these two ownership annotations after rendering.
				// Keep every other metadata and policy check exact.
				expected = reviewed.DeepCopy()
				wanted := expected.GetAnnotations()
				if wanted == nil {
					wanted = map[string]string{}
				}
				for key, value := range map[string]string{"meta.helm.sh/release-name": candidate.release, "meta.helm.sh/release-namespace": candidate.manifest.Namespace} {
					if configured, found := wanted[key]; found && configured != value {
						return target, ownershipError
					}
					wanted[key] = value
				}
				expected.SetAnnotations(wanted)
			}
		}
		if err := verifyObject(ctx, admin, expected); err != nil {
			return target, err
		}
	}
	objects = append(objects, candidate.resourceAdmission...)
	objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]interface{}{"name": target.Controller, "namespace": target.Namespace}}})
	for _, namespace := range append([]string{"kube-system", target.Namespace, authorityNamespace}, target.Namespaces...) {
		objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": namespace}}})
	}
	seen := map[string]bool{}
	for _, expected := range objects {
		key := expected.GetAPIVersion() + "/" + expected.GetKind() + "/" + expected.GetNamespace() + "/" + expected.GetName()
		if seen[key] {
			return target, errors.New("duplicate activation inventory object")
		}
		seen[key] = true
		if expected.GetKind() == "Deployment" || expected.GetKind() == "Role" || expected.GetKind() == "ClusterRole" {
			if err := verifyObject(ctx, admin, expected); err != nil {
				return target, err
			}
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(expected.GroupVersionKind())
		if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
			return target, err
		}
		if live.GetUID() == "" || live.GetDeletionTimestamp() != nil {
			return target, fmt.Errorf("missing UID or deleting activation object %s", key)
		}
		if expected.GetKind() == "Namespace" && expected.GetName() == "kube-system" {
			target.ClusterUID = string(live.GetUID())
		}
		if expected.GetKind() == "Deployment" {
			for _, field := range []string{"containers", "initContainers"} {
				containers, _, err := unstructured.NestedSlice(live.Object, "spec", "template", "spec", field)
				if err != nil {
					return target, err
				}
				for _, entry := range containers {
					container, ok := entry.(map[string]interface{})
					if !ok {
						return target, errors.New("invalid activation container")
					}
					image, _, _ := unstructured.NestedString(container, "image")
					name, _, _ := unstructured.NestedString(container, "name")
					_, digest, pinned := strings.Cut(image, "@sha256:")
					if !pinned || name == "" || !validSHA256(digest) {
						return target, errors.New("controller and guard images must be digest-pinned")
					}
					target.Images[expected.GetName()+"/"+field+"/"+name] = digest
				}
			}
		}
		evidence, err := snapshotObject(live)
		if err != nil {
			return target, err
		}
		target.Objects = append(target.Objects, evidence)
	}
	return target, validateTarget(target)
}

func snapshotObject(object *unstructured.Unstructured) (ObjectEvidence, error) {
	evidence := ObjectEvidence{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), Generation: object.GetGeneration()}
	copy := object.DeepCopy()
	delete(copy.Object, "status")
	copy.SetResourceVersion("")
	copy.SetManagedFields(nil)
	if copy.GetAPIVersion() == "apps/v1" && copy.GetKind() == "Deployment" {
		annotations := copy.GetAnnotations()
		delete(annotations, "deployment.kubernetes.io/revision")
		if len(annotations) == 0 {
			annotations = nil
		}
		copy.SetAnnotations(annotations)
	}
	encoded, err := json.Marshal(copy.Object)
	if err != nil {
		return evidence, err
	}
	evidence.SHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return evidence, nil
}
