// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestDeployedPositiveControlLabels(t *testing.T) {
	got := deployedPositiveControlLabels("abc12345")
	if len(got) != 1 || got["sg07-positive"] != "abc12345" {
		t.Fatalf("control selector must carry only the intended label, got %v", got)
	}
	got["topology.kubernetes.io/zone"] = "0"
	if again := deployedPositiveControlLabels("abc12345"); len(again) != 1 {
		t.Fatalf("callers must not share or mutate the intended labels, got %v", again)
	}
}

func TestDeployedRealizationBudget(t *testing.T) {
	for profile, want := range map[string]time.Duration{
		"":                            15 * time.Second,
		"local-cilium-1.17.8":         15 * time.Second,
		"reviewed-cilium-1.20.2":      15 * time.Second,
		"cilium-latest":               15 * time.Second,
		"reviewed-aks-cilium-1.18.15": 90 * time.Second,
	} {
		if got := deployedRealizationBudgetFor(profile); got != want {
			t.Fatalf("profile %q realization budget = %s, want %s", profile, got, want)
		}
	}
}

func TestDeployedFixturePullPolicy(t *testing.T) {
	for profile, want := range map[string]corev1.PullPolicy{
		"":                            corev1.PullNever,
		"local-cilium-1.17.8":         corev1.PullNever,
		"reviewed-cilium-1.20.2":      corev1.PullNever,
		"cilium-latest":               corev1.PullNever,
		"reviewed-aks-cilium-1.18.15": corev1.PullIfNotPresent,
	} {
		if got := deployedFixturePullPolicyFor(profile); got != want {
			t.Fatalf("profile %q pull policy = %s, want %s", profile, got, want)
		}
	}
}

func TestDeployedTargetEndpoint(t *testing.T) {
	const (
		remote  = "https://kognition-aks.hcp.eastus2.azmk8s.io:443"
		uid     = "a8643441-5498-4c16-9404-e3fd62c87afa"
		aks     = "reviewed-aks-cilium-1.18.15"
		tunnel  = "reviewed-cilium-1.20.2"
		legacy  = "local-cilium-1.17.8"
		managed = "default"
	)
	permitted := []string{"kognition-system", "kognition-work", "kognition-authority", "kognition-providers", "kognition-acceptance"}
	base := deployedTargetEvidence{
		Profile: aks, Host: remote, RemoteEndpoint: remote, DedicatedClusterUID: uid, ClusterUID: uid,
		Namespaces: append([]string{managed, "kube-system", "kube-public", "kube-node-lease"}, permitted...), Permitted: permitted,
	}
	with := func(mutate func(*deployedTargetEvidence)) deployedTargetEvidence {
		evidence := base
		evidence.Namespaces = append([]string(nil), base.Namespaces...)
		mutate(&evidence)
		return evidence
	}
	for _, scenario := range []struct {
		name     string
		evidence deployedTargetEvidence
		rejected bool
	}{
		{name: "reviewed AKS remote target", evidence: base},
		{name: "loopback local target needs no remote evidence", evidence: deployedTargetEvidence{Profile: tunnel, Host: "https://127.0.0.1:6443"}},
		{name: "loopback legacy profile unchanged", evidence: deployedTargetEvidence{Profile: legacy, Host: "https://127.0.0.1:6443"}},
		{name: "remote rejected for the tunnel profile", evidence: with(func(e *deployedTargetEvidence) { e.Profile = tunnel }), rejected: true},
		{name: "remote rejected for the legacy profile", evidence: with(func(e *deployedTargetEvidence) { e.Profile = legacy }), rejected: true},
		{name: "plaintext endpoint", evidence: with(func(e *deployedTargetEvidence) { e.Host = "http://kognition-aks.example:80"; e.RemoteEndpoint = e.Host }), rejected: true},
		{name: "insecure TLS", evidence: with(func(e *deployedTargetEvidence) { e.InsecureTLS = true }), rejected: true},
		{name: "endpoint not pinned", evidence: with(func(e *deployedTargetEvidence) { e.RemoteEndpoint = "" }), rejected: true},
		{name: "endpoint differs from the reviewed plan", evidence: with(func(e *deployedTargetEvidence) { e.RemoteEndpoint = "https://other.hcp.eastus2.azmk8s.io:443" }), rejected: true},
		{name: "no dedicated-cluster attestation", evidence: with(func(e *deployedTargetEvidence) { e.DedicatedClusterUID = "" }), rejected: true},
		{name: "attestation names another cluster", evidence: with(func(e *deployedTargetEvidence) { e.DedicatedClusterUID = "11111111-2222-3333-4444-555555555555" }), rejected: true},
		{name: "missing observed cluster UID", evidence: with(func(e *deployedTargetEvidence) { e.ClusterUID = "" }), rejected: true},
		{name: "foreign workload namespace", evidence: with(func(e *deployedTargetEvidence) { e.Namespaces = append(e.Namespaces, "payments") }), rejected: true},
		{name: "leftover cert-manager namespace", evidence: with(func(e *deployedTargetEvidence) { e.Namespaces = append(e.Namespaces, "cert-manager") }), rejected: true},
		{name: "empty attested namespace set", evidence: with(func(e *deployedTargetEvidence) { e.Namespaces = nil }), rejected: true},
		{name: "loopback lookalike host", evidence: deployedTargetEvidence{Profile: aks, Host: "https://127.0.0.1.evil.example:443"}, rejected: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if err := validateDeployedTarget(scenario.evidence); (err != nil) != scenario.rejected {
				t.Fatalf("target rejection = %v, want %v", err, scenario.rejected)
			}
		})
	}
}

func TestDeployedCiliumProfileRemoteScope(t *testing.T) {
	for profile, want := range map[string]bool{"": false, "local-cilium-1.17.8": false, "reviewed-cilium-1.20.2": false, "reviewed-aks-cilium-1.18.15": true} {
		got, err := resolveDeployedCiliumProfile(profile, true, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if got.remoteDedicated != want {
			t.Fatalf("profile %q remoteDedicated = %v, want %v", profile, got.remoteDedicated, want)
		}
	}
}

func TestDeployedPermittedNamespaces(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		fixed []string
		extra string
		want  []string
	}{
		{name: "no extras", fixed: []string{"system", "authority"}, want: []string{"system", "authority"}},
		{name: "explicit extras", fixed: []string{"system"}, extra: "providers,fixtures", want: []string{"system", "providers", "fixtures"}},
		{name: "whitespace and empties ignored", fixed: []string{"system"}, extra: " providers , ,", want: []string{"system", "providers"}},
		{name: "empty fixed names dropped", fixed: []string{"system", ""}, extra: "providers", want: []string{"system", "providers"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got := permittedDeployedNamespaces(scenario.fixed, scenario.extra)
			if len(got) != len(scenario.want) {
				t.Fatalf("namespaces = %v, want %v", got, scenario.want)
			}
			for index := range got {
				if got[index] != scenario.want[index] {
					t.Fatalf("namespaces = %v, want %v", got, scenario.want)
				}
			}
		})
	}
}
