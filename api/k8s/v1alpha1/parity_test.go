// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1_test

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestCanonicalKindsHaveNativeKubernetesSurface(t *testing.T) {
	expected := make(map[string]bool, len(api.KindRegistry))
	for _, kind := range api.RegisteredKinds() {
		if api.IsKubernetesNativeKind(kind) {
			expected[kind] = true
		}
	}
	// RuntimeApprovalTombstone is a Kubernetes persistence primitive rather
	// than a user-facing canonical API kind. PostgreSQL provides the equivalent
	// replay fence as a table in Slice F.3.
	expected["RuntimeApprovalTombstone"] = true
	// RuntimeApprovalQuotaTicket is a Kubernetes-only quota ledger primitive;
	// PostgreSQL persists the same accounting in an internal reservations table.
	expected["RuntimeApprovalQuotaTicket"] = true
	// TrustProfileIndex is a Kubernetes-only uniqueness primitive; PostgreSQL
	// enforces the equivalent invariant with a unique index.
	expected["TrustProfileIndex"] = true

	expected["KnowledgeBindingRecord"] = true

	scheme := k8sruntime.NewScheme()
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatalf("register Kubernetes API types: %v", err)
	}
	for kind := range expected {
		for _, registeredKind := range []string{kind, kind + "List"} {
			gvk := k8s.GroupVersion.WithKind(registeredKind)
			if _, err := scheme.New(gvk); err != nil {
				t.Errorf("Kubernetes scheme does not register %s: %v", registeredKind, err)
			}
		}
	}
}

func TestCanonicalKindsHaveKubernetesDeploymentArtifacts(t *testing.T) {
	expected := make(map[string]bool, len(api.KindRegistry))
	for _, kind := range api.RegisteredKinds() {
		if api.IsKubernetesNativeKind(kind) {
			expected[kind] = true
		}
	}
	expected["RuntimeApprovalTombstone"] = true
	expected["RuntimeApprovalQuotaTicket"] = true
	expected["TrustProfileIndex"] = true
	expected["KnowledgeBindingRecord"] = true

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve parity test path")
	}
	crdDirectory := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "deploy", "helm", "kognition", "crds")
	entries, err := os.ReadDir(crdDirectory)
	if err != nil {
		t.Fatalf("read CRD directory: %v", err)
	}
	installed := make(map[string]bool, len(entries))
	expectedRBAC := make(map[string]bool, len(entries)*2)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(crdDirectory, entry.Name()))
		if err != nil {
			t.Fatalf("read CRD %s: %v", entry.Name(), err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatalf("parse CRD %s: %v", entry.Name(), err)
		}
		kind := crd.Spec.Names.Kind
		if kind == "" {
			t.Fatalf("CRD %s has no kind", entry.Name())
		}
		if installed[kind] {
			t.Fatalf("multiple CRDs install kind %s", kind)
		}
		installed[kind] = true
		expectedRBAC[crd.Spec.Names.Plural] = true
		for _, version := range crd.Spec.Versions {
			if version.Served && version.Subresources != nil && version.Subresources.Status != nil {
				expectedRBAC[crd.Spec.Names.Plural+"/status"] = true
				break
			}
		}
	}
	assertKindSetsEqual(t, "Helm CRDs", installed, expected)

	clusterRolePath := filepath.Join(crdDirectory, "..", "templates", "clusterrole.yaml")
	clusterRoleData, err := os.ReadFile(clusterRolePath)
	if err != nil {
		t.Fatalf("read ClusterRole template: %v", err)
	}
	assertKindSetsEqual(t, "ClusterRole resources", kognitionRuleResources(t, string(clusterRoleData)), expectedRBAC)
}

func kognitionRuleResources(t *testing.T, template string) map[string]bool {
	t.Helper()
	resources := make(map[string]bool)
	inRule := false
	inResources := false
	for _, line := range strings.Split(template, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == `- apiGroups: ["kognition.dev"]` {
			inRule = true
			inResources = false
			continue
		}
		if !inRule {
			continue
		}
		if trimmed == "resources:" {
			inResources = true
			continue
		}
		if strings.HasPrefix(trimmed, "verbs:") {
			inRule = false
			inResources = false
			continue
		}
		if strings.HasPrefix(trimmed, "resources: [") {
			value := strings.TrimSuffix(strings.TrimPrefix(trimmed, "resources: ["), "]")
			for _, resource := range strings.Split(value, ",") {
				resources[strings.Trim(strings.TrimSpace(resource), `"`)] = true
			}
			continue
		}
		if inResources && strings.HasPrefix(trimmed, "- ") {
			resources[strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))] = true
		}
	}
	if len(resources) == 0 {
		t.Fatal("ClusterRole template has no kognition.dev resources")
	}
	return resources
}

func assertKindSetsEqual(t *testing.T, surface string, got, want map[string]bool) {
	t.Helper()
	missing := make([]string, 0)
	unexpected := make([]string, 0)
	for kind := range want {
		if !got[kind] {
			missing = append(missing, kind)
		}
	}
	for kind := range got {
		if !want[kind] {
			unexpected = append(unexpected, kind)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)
	if len(missing) != 0 || len(unexpected) != 0 {
		t.Fatalf("%s parity mismatch: missing=%v unexpected=%v", surface, missing, unexpected)
	}
}
