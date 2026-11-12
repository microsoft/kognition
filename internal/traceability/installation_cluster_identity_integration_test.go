// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestInstallationClusterAcceptsExplicitFreshNode(test *testing.T) {
	name := "kognition-acceptance-fresh-control-plane"
	nodes := []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: name}}}
	if err := validateInstallationNode(nodes, name); err != nil {
		test.Fatalf("explicit fresh node must be accepted: %v", err)
	}
}

func TestInstallationClusterIdentity(test *testing.T) {
	const runID = "550e8400-e29b-41d4-a716-446655440000"
	const name = "kognition-" + runID + "-control-plane"
	const oversizedName = "kognition-acceptance-" + runID + "-control-plane"
	const clusterUID = "3d594650-3436-4bf4-985f-2bd081d51d8a"
	if errors := validation.IsDNS1123Label(name); len(errors) != 0 {
		test.Fatalf("acceptance node must fit a single DNS hostname label: %v", errors)
	}
	for _, scenario := range []struct {
		name        string
		expected    string
		actual      string
		run         string
		label       string
		uid         string
		namespace   string
		nodeCount   int
		missingRoot bool
		wantError   bool
	}{
		{"fresh owned cluster", name, name, runID, runID, clusterUID, "kube-system", 1, false, false},
		{"oversized legacy prefix", oversizedName, oversizedName, runID, runID, clusterUID, "kube-system", 1, false, true},
		{"missing expected node", "", name, runID, runID, clusterUID, "kube-system", 1, false, true},
		{"wrong node", name, "docker-desktop", runID, runID, clusterUID, "kube-system", 1, false, true},
		{"shared node explicitly selected", "docker-desktop", "docker-desktop", runID, runID, clusterUID, "kube-system", 1, false, true},
		{"historical node", "kognition-admission-security-20260923-control-plane", "kognition-admission-security-20260923-control-plane", runID, runID, clusterUID, "kube-system", 1, false, true},
		{"missing run", name, name, "", runID, clusterUID, "kube-system", 1, false, true},
		{"invalid run", name, name, "fresh", "fresh", clusterUID, "kube-system", 1, false, true},
		{"zero run", name, name, "00000000-0000-0000-0000-000000000000", runID, clusterUID, "kube-system", 1, false, true},
		{"missing ownership", name, name, runID, "", clusterUID, "kube-system", 1, false, true},
		{"different ownership", name, name, runID, "another-run", clusterUID, "kube-system", 1, false, true},
		{"missing expected UID", name, name, runID, runID, "", "kube-system", 1, false, true},
		{"different UID", name, name, runID, runID, "different-cluster", "kube-system", 1, false, true},
		{"wrong namespace", name, name, runID, runID, clusterUID, "default", 1, false, true},
		{"no nodes", name, name, runID, runID, clusterUID, "kube-system", 0, false, true},
		{"multiple nodes", name, name, runID, runID, clusterUID, "kube-system", 2, false, true},
		{"missing root", name, name, runID, runID, clusterUID, "kube-system", 1, true, true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			nodes := make([]corev1.Node, scenario.nodeCount)
			for index := range nodes {
				nodes[index].ObjectMeta = metav1.ObjectMeta{Name: scenario.actual, Labels: map[string]string{"kognition.dev/acceptance-run": scenario.label}}
			}
			root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: scenario.namespace, UID: types.UID(clusterUID)}}
			if scenario.missingRoot {
				root = nil
			}
			err := validateInstallationCluster(nodes, scenario.expected, root, scenario.uid, scenario.run)
			if (err != nil) != scenario.wantError {
				test.Fatalf("identity check error = %v, want rejection = %v", err, scenario.wantError)
			}
		})
	}
}
