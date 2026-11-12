// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"reflect"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestDeployedCiliumProfile(t *testing.T) {
	const legacy = "quay.io/cilium/cilium:v1.17.8@sha256:6d7ea72ed311eeca4c75a1f17617a3d596fb6038d30d00799090679f82a01636"
	const current = "quay.io/cilium/cilium:v1.20.2@sha256:2939231d0d3e3ebddcd80fffa168b7ddcc78fdf0dc864d1c8c126ff523c54f01"
	const aksCilium = "mcr.microsoft.com/containernetworking/cilium/cilium-distroless:v1.18.15-261022"
	for _, scenario := range []struct {
		name                       string
		profile                    string
		reviewed, isolated, frozen bool
		wantImage                  string
		wantOperator, wantRejected bool
	}{
		{name: "existing default", wantImage: legacy},
		{name: "explicit legacy", profile: "local-cilium-1.17.8", wantImage: legacy},
		{name: "reviewed candidate", profile: "reviewed-cilium-1.20.2", reviewed: true, isolated: true, frozen: true, wantImage: current, wantOperator: true},
		{name: "missing target", profile: "reviewed-cilium-1.20.2", isolated: true, frozen: true, wantRejected: true},
		{name: "missing isolation", profile: "reviewed-cilium-1.20.2", reviewed: true, frozen: true, wantRejected: true},
		{name: "writers active", profile: "reviewed-cilium-1.20.2", reviewed: true, isolated: true, wantRejected: true},
		{name: "unknown profile", profile: "cilium-latest", reviewed: true, isolated: true, frozen: true, wantRejected: true},
		{name: "reviewed AKS candidate", profile: "reviewed-aks-cilium-1.18.15", reviewed: true, isolated: true, frozen: true, wantImage: aksCilium, wantOperator: true},
		{name: "AKS missing target", profile: "reviewed-aks-cilium-1.18.15", isolated: true, frozen: true, wantRejected: true},
		{name: "AKS missing isolation", profile: "reviewed-aks-cilium-1.18.15", reviewed: true, frozen: true, wantRejected: true},
		{name: "AKS writers active", profile: "reviewed-aks-cilium-1.18.15", reviewed: true, isolated: true, wantRejected: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := resolveDeployedCiliumProfile(scenario.profile, scenario.reviewed, scenario.isolated, scenario.frozen)
			if (err != nil) != scenario.wantRejected {
				t.Fatalf("profile rejection = %v, want %v", err, scenario.wantRejected)
			}
			if err == nil && (got.image != scenario.wantImage || got.operatorFixtures != scenario.wantOperator) {
				t.Fatalf("profile = %+v, want image %s operator=%v", got, scenario.wantImage, scenario.wantOperator)
			}
		})
	}
}

func TestDeployedCiliumConfiguration(t *testing.T) {
	tunnel := map[string]string{"enable-policy": "default", "enable-k8s-networkpolicy": "true", "enable-ipv4": "true", "enable-ipv6": "false", "ipam": "kubernetes", "kube-proxy-replacement": "false", "routing-mode": "tunnel", "tunnel-protocol": "vxlan"}
	aks := map[string]string{"enable-policy": "default", "enable-k8s-networkpolicy": "true", "enable-ipv4": "true", "enable-ipv6": "false", "ipam": "delegated-plugin", "kube-proxy-replacement": "true", "routing-mode": "native", "datapath-mode": "veth", "enable-endpoint-routes": "true", "identity-allocation-mode": "crd"}
	with := func(base map[string]string, key, value string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		out[key] = value
		return out
	}
	for _, scenario := range []struct {
		name     string
		profile  string
		data     map[string]string
		rejected bool
	}{
		{name: "legacy tunnel", profile: "local-cilium-1.17.8", data: tunnel},
		{name: "reviewed tunnel", profile: "reviewed-cilium-1.20.2", data: tunnel},
		{name: "tunnel profile rejects native routing", profile: "reviewed-cilium-1.20.2", data: aks, rejected: true},
		{name: "AKS exact", profile: "reviewed-aks-cilium-1.18.15", data: aks},
		{name: "AKS rejects tunnel configuration", profile: "reviewed-aks-cilium-1.18.15", data: tunnel, rejected: true},
		{name: "AKS rejects policy disabled", profile: "reviewed-aks-cilium-1.18.15", data: with(aks, "enable-policy", "never"), rejected: true},
		{name: "AKS rejects missing setting", profile: "reviewed-aks-cilium-1.18.15", data: with(aks, "ipam", ""), rejected: true},
		{name: "AKS rejects IPv6", profile: "reviewed-aks-cilium-1.18.15", data: with(aks, "enable-ipv6", "true"), rejected: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			profile, err := resolveDeployedCiliumProfile(scenario.profile, true, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateDeployedCiliumConfiguration(profile, scenario.data); (err != nil) != scenario.rejected {
				t.Fatalf("configuration rejection = %v, want %v", err, scenario.rejected)
			}
		})
	}
}

func TestDeployedCiliumAgentDigest(t *testing.T) {
	const digest = "sha256:c7fd124f9b0c01ac4ae2fec56047e22e2860fc112535dc52aac7cfa2878575db"
	aks, err := resolveDeployedCiliumProfile("reviewed-aks-cilium-1.18.15", true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if aks.agentDigest != digest {
		t.Fatalf("AKS profile must pin the running agent digest, got %q", aks.agentDigest)
	}
	reviewed, err := resolveDeployedCiliumProfile("reviewed-cilium-1.20.2", true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	pod := func(name, imageID string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "azure-iptables-monitor", ImageID: "mcr.microsoft.com/containernetworking/v2/azure-iptables-monitor@sha256:other"},
			{Name: "cilium-agent", ImageID: imageID},
		}}}
	}
	repository := "mcr.microsoft.com/containernetworking/cilium/cilium-distroless@"
	for _, scenario := range []struct {
		name     string
		profile  deployedCiliumProfile
		pods     []corev1.Pod
		rejected bool
	}{
		{name: "AKS every node on pinned digest", profile: aks, pods: []corev1.Pod{pod("a", repository+digest), pod("b", repository+digest)}},
		{name: "AKS one node drifted", profile: aks, pods: []corev1.Pod{pod("a", repository+digest), pod("b", repository+"sha256:drifted")}, rejected: true},
		{name: "AKS no agent pods", profile: aks, rejected: true},
		{name: "AKS agent container missing", profile: aks, pods: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "a"}}}, rejected: true},
		{name: "profiles without a digest pin are unchanged", profile: reviewed, pods: []corev1.Pod{pod("a", "anything")}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if err := validateDeployedCiliumAgentDigest(scenario.profile, scenario.pods); (err != nil) != scenario.rejected {
				t.Fatalf("agent digest rejection = %v, want %v", err, scenario.rejected)
			}
		})
	}
}

func TestDeployedNetworkFixtureScope(t *testing.T) {
	manifest := activation.Manifest{Namespace: "platform-system", Namespaces: []string{"work-one", "work-two"}}
	base := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "operator-probes", UID: types.UID("operator-uid"),
		Labels: map[string]string{
			"kognition.dev/acceptance-operator":  "true",
			"kognition.dev/recording-run":        "reviewed-run",
			"pod-security.kubernetes.io/enforce": "privileged",
		},
	}}
	for _, scenario := range []struct {
		name      string
		mutate    func(*corev1.Namespace)
		uid, run  string
		authority string
		rejected  bool
	}{
		{name: "separate operator", uid: "operator-uid", run: "reviewed-run", authority: "approval-authority"},
		{name: "installation", mutate: func(ns *corev1.Namespace) { ns.Name = manifest.Namespace }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "first workload", mutate: func(ns *corev1.Namespace) { ns.Name = "work-one" }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "second workload", mutate: func(ns *corev1.Namespace) { ns.Name = "work-two" }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "authority", uid: "operator-uid", run: "reviewed-run", authority: base.Name, rejected: true},
		{name: "system namespace", mutate: func(ns *corev1.Namespace) { ns.Name = "kube-system" }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "default namespace", mutate: func(ns *corev1.Namespace) { ns.Name = "default" }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "replaced namespace", uid: "replacement-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "missing uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "missing run", uid: "operator-uid", authority: "approval-authority", rejected: true},
		{name: "missing authority", uid: "operator-uid", run: "reviewed-run", rejected: true},
		{name: "foreign run", uid: "operator-uid", run: "another-run", authority: "approval-authority", rejected: true},
		{name: "no operator label", mutate: func(ns *corev1.Namespace) { delete(ns.Labels, "kognition.dev/acceptance-operator") }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "no explicit host-network scope", mutate: func(ns *corev1.Namespace) { ns.Labels["pod-security.kubernetes.io/enforce"] = "restricted" }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
		{name: "terminating", mutate: func(ns *corev1.Namespace) { now := metav1.Now(); ns.DeletionTimestamp = &now }, uid: "operator-uid", run: "reviewed-run", authority: "approval-authority", rejected: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			namespace := base.DeepCopy()
			if scenario.mutate != nil {
				scenario.mutate(namespace)
			}
			before := namespace.DeepCopy()
			err := validateDeployedNetworkFixtureNamespace(namespace, scenario.uid, scenario.run, scenario.authority, manifest)
			if (err != nil) != scenario.rejected {
				t.Fatalf("scope rejection = %v, want %v", err, scenario.rejected)
			}
			if !reflect.DeepEqual(namespace, before) {
				t.Fatal("fixture-scope validation mutated namespace security configuration")
			}
		})
	}
}
