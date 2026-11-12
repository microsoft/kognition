// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCandidateRejectsZeroValue(test *testing.T) {
	if _, err := (Candidate{}).Snapshot(context.Background(), nil, "", "", "", "", "authority", 1); err == nil {
		test.Fatal("zero candidate accepted")
	}
}

func TestSnapshotDeploymentRevision(test *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]interface{}{"name": "controller", "uid": "uid"}}}
	original, err := snapshotObject(object)
	if err != nil {
		test.Fatal(err)
	}
	object.SetAnnotations(map[string]string{"deployment.kubernetes.io/revision": "2"})
	updated, err := snapshotObject(object)
	if err != nil || updated != original {
		test.Fatalf("revision bookkeeping changed target: %v", err)
	}
	object.SetAnnotations(map[string]string{"reviewed": "changed"})
	updated, err = snapshotObject(object)
	if err != nil || updated == original {
		test.Fatalf("annotation drift hidden: %v", err)
	}
}

func TestCandidateRoleInventory(test *testing.T) {
	for _, scenario := range []string{"complete", "duplicate", "missing-cluster", "missing-workload", "missing-installation", "missing-ledger", "foreign-role"} {
		test.Run(scenario, func(test *testing.T) {
			candidate := Candidate{manifest: Manifest{Namespace: "control", Namespaces: []string{"tenant"}}, controller: &unstructured.Unstructured{}}
			candidate.controller.SetName("release-kognition-k8s-controller")
			for _, item := range []struct{ kind, namespace, name string }{
				{"ClusterRole", "", "release-kognition-controller"},
				{"Role", "control", "release-kognition-controller"},
				{"Role", "tenant", "release-kognition-controller"},
				{"Role", "control", "release-kognition-quota-ledger"},
			} {
				object := &unstructured.Unstructured{}
				object.SetKind(item.kind)
				object.SetNamespace(item.namespace)
				object.SetName(item.name)
				candidate.roles = append(candidate.roles, object)
			}
			switch scenario {
			case "duplicate":
				candidate.roles = append(candidate.roles, candidate.roles[0].DeepCopy())
			case "missing-cluster":
				candidate.roles = candidate.roles[1:]
			case "missing-workload":
				candidate.roles = append(candidate.roles[:2], candidate.roles[3:]...)
			case "missing-installation":
				candidate.roles = append(candidate.roles[:1], candidate.roles[2:]...)
			case "missing-ledger":
				candidate.roles = candidate.roles[:3]
			case "foreign-role":
				candidate.roles[2].SetNamespace("unreviewed")
			}
			if err := candidate.validateRoles(); (err == nil) != (scenario == "complete") {
				test.Fatalf("unexpected inventory result: %v", err)
			}
		})
	}
}
