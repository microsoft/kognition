// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func TestManagedWorkloadIdentityDeliveryPinsIncarnationAndReconciles(test *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		test.Fatal(err)
	}
	configuration := &WorkloadIdentityDelivery{ProxyImage: "proxy@sha256:1111111111111111111111111111111111111111111111111111111111111111", TrustDomain: "kognition.dev", ControllerID: "spiffe://kognition.dev/controller/identity", Gateways: map[string]runtimeauth.WorkloadGatewayEndpoint{}}
	for _, plane := range []string{"model", "tool", "memory", "communication"} {
		configuration.Gateways[plane] = runtimeauth.WorkloadGatewayEndpoint{URL: "https://" + plane + ".control:9443", PeerID: "spiffe://kognition.dev/gateway/" + plane}
	}
	d := NewK8sDispatcher(fake.NewClientBuilder().WithScheme(scheme).Build(), nil, zap.NewNop(), Config{AgentImage: "runtime:v1", WorkloadIdentityMode: "spire", WorkloadIdentityDelivery: configuration, RuntimeNetworkPolicyEnabled: true, ControllerNamespace: "control", KubeletProbeIngressCIDRs: []string{"172.16.0.0/12"}})
	agent := &api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "tenant", Name: "worker", UID: "70000000-0000-4000-8000-000000000001"}}
	for _, revision := range []bool{false, true} {
		test.Run(map[bool]string{false: "shared", true: "revision"}[revision], func(test *testing.T) {
			name := deploymentName(d.workloadKey("tenant", "worker"))
			labels := map[string]string{labelManagedBy: managedByValue, labelAgent: "worker"}
			annotations := map[string]string{annotationOwnerAgentService: "tenant/service"}
			if revision {
				name = "revision-worker"
			}
			reconcile := func() {
				test.Helper()
				var err error
				if revision {
					err = d.ensureDedicatedRevisionDeployment(context.Background(), "runtime", name, agent, labels, annotations, nil)
				} else {
					err = d.ensureDeployment(context.Background(), "runtime", agent, nil)
				}
				if err != nil {
					test.Fatal(err)
				}
			}
			reconcile()
			var deployment appsv1.Deployment
			if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: name}, &deployment); err != nil {
				test.Fatal(err)
			}
			if len(deployment.Spec.Template.Spec.Containers) != 2 {
				test.Fatal("proxy missing")
			}
			pod := deployment.Spec.Template.Spec
			proxy := pod.Containers[1]
			if proxy.Name != "workload-identity-proxy" || proxy.SecurityContext == nil || !*proxy.SecurityContext.ReadOnlyRootFilesystem || *proxy.SecurityContext.AllowPrivilegeEscalation || !proxy.VolumeMounts[0].ReadOnly {
				test.Fatal("proxy is not hardened")
			}
			if len(pod.Containers[0].VolumeMounts) != 0 {
				test.Fatal("runtime received protected Workload API")
			}
			if len(pod.Volumes) != 1 || pod.Volumes[0].CSI == nil || pod.Volumes[0].CSI.Driver != "csi.spiffe.io" || !*pod.Volumes[0].CSI.ReadOnly {
				test.Fatal("protected CSI delivery missing")
			}
			version := deployment.ResourceVersion
			reconcile()
			if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: name}, &deployment); err != nil {
				test.Fatal(err)
			}
			if deployment.ResourceVersion != version {
				test.Fatal("unchanged proxy caused rollout")
			}
			stale := deployment.DeepCopy()
			for index := range stale.Spec.Template.Spec.Containers[1].Env {
				if stale.Spec.Template.Spec.Containers[1].Env[index].Name == "KOGNITION_AGENT_UID" {
					stale.Spec.Template.Spec.Containers[1].Env[index].Value = "stale"
				}
			}
			if err := d.client.Update(context.Background(), stale); err != nil {
				test.Fatal(err)
			}
			reconcile()
			if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: name}, &deployment); err != nil {
				test.Fatal(err)
			}
			for _, variable := range deployment.Spec.Template.Spec.Containers[1].Env {
				if variable.Name == "KOGNITION_AGENT_UID" && variable.Value != agent.ObjectMeta.UID {
					test.Fatal("stale incarnation retained")
				}
			}
		})
	}
	if err := d.ensureService(context.Background(), "runtime", agent); err != nil {
		test.Fatal(err)
	}
	var service corev1.Service
	if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: serviceName(d.workloadKey("tenant", "worker"))}, &service); err != nil {
		test.Fatal(err)
	}
	if len(service.Spec.Ports) != 2 || service.Spec.Ports[1].Name != "identity-status" || service.Spec.Ports[1].Port != 8097 {
		test.Fatal("status Service route missing")
	}
	if err := d.ensureRuntimeNetworkPolicy(context.Background(), "runtime", agent); err != nil {
		test.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: deploymentName(d.workloadKey("tenant", "worker")) + "-network"}, &policy); err != nil {
		test.Fatal(err)
	}
	route := policy.Spec.Ingress[1]
	if len(route.From) != 1 || len(route.Ports) != 1 || route.Ports[0].Port.IntVal != 8097 || route.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "control" || route.From[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "k8s-controller" {
		test.Fatal("status route is not controller-only")
	}
	d.config.WorkloadIdentityDelivery = nil
	if err := d.ensureDeployment(context.Background(), "runtime", agent, nil); err == nil {
		test.Fatal("missing protected delivery accepted")
	}
	var existing appsv1.Deployment
	if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: deploymentName(d.workloadKey("tenant", "worker"))}, &existing); err != nil {
		test.Fatal(err)
	}
	existing.Spec.Template.Spec.Containers = append(existing.Spec.Template.Spec.Containers, corev1.Container{Name: "audit", Image: "audit:test", SecurityContext: existing.Spec.Template.Spec.Containers[0].SecurityContext.DeepCopy()})
	if err := d.client.Update(context.Background(), &existing); err != nil {
		test.Fatal(err)
	}
	d.config.WorkloadIdentityMode = ""
	if err := d.ensureDeployment(context.Background(), "runtime", agent, nil); err != nil {
		test.Fatal(err)
	}
	if err := d.client.Get(context.Background(), client.ObjectKeyFromObject(&existing), &existing); err != nil {
		test.Fatal(err)
	}
	if len(existing.Spec.Template.Spec.Containers) != 2 || existing.Spec.Template.Spec.Containers[1].Name != "audit" || len(existing.Spec.Template.Spec.Volumes) != 0 {
		test.Fatal("disablement removed unrelated sidecar or retained CSI socket")
	}
	if err := d.ensureService(context.Background(), "runtime", agent); err != nil {
		test.Fatal(err)
	}
	if err := d.client.Get(context.Background(), client.ObjectKey{Namespace: "runtime", Name: service.Name}, &service); err != nil {
		test.Fatal(err)
	}
	if len(service.Spec.Ports) != 1 {
		test.Fatal("disabled identity status port retained")
	}
}

func TestWorkloadIdentityDeliveryRejectsUnsafeOperatorConfiguration(test *testing.T) {
	gateways := map[string]runtimeauth.WorkloadGatewayEndpoint{}
	for _, plane := range []string{"model", "tool", "memory", "communication"} {
		gateways[plane] = runtimeauth.WorkloadGatewayEndpoint{URL: "https://" + plane + ".control:9443", PeerID: "spiffe://kognition.dev/gateway/" + plane}
	}
	body, err := json.Marshal(gateways)
	if err != nil {
		test.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"valid":          func(map[string]string) {},
		"missing-image":  func(values map[string]string) { values["KOGNITION_WORKLOAD_PROXY_IMAGE"] = "" },
		"unpinned-image": func(values map[string]string) { values["KOGNITION_WORKLOAD_PROXY_IMAGE"] = "proxy:latest" },
		"missing-mode":   func(values map[string]string) { values["KOGNITION_WORKLOAD_IDENTITY_MODE"] = "" },
		"unknown-mode":   func(values map[string]string) { values["KOGNITION_WORKLOAD_IDENTITY_MODE"] = "other" },
		"duplicate-plane": func(values map[string]string) {
			values["KOGNITION_SPIFFE_GATEWAY_ENDPOINTS"] = strings.Replace(string(body), "{", "{\"tool\":{},", 1)
		},
		"duplicate-field": func(values map[string]string) {
			values["KOGNITION_SPIFFE_GATEWAY_ENDPOINTS"] = strings.Replace(string(body), "\"url\":", "\"url\":\"https://evil\",\"url\":", 1)
		},
		"null-field": func(values map[string]string) {
			values["KOGNITION_SPIFFE_GATEWAY_ENDPOINTS"] = strings.Replace(string(body), "\"url\":", "\"unknown\":null,\"url\":", 1)
		},
		"trailing": func(values map[string]string) { values["KOGNITION_SPIFFE_GATEWAY_ENDPOINTS"] += " {}" },
		"same-peer": func(values map[string]string) {
			values["KOGNITION_SPIFFE_GATEWAY_ENDPOINTS"] = strings.Replace(string(body), "spiffe://kognition.dev/gateway/tool", "spiffe://kognition.dev/controller/identity", 1)
		},
		"http": func(values map[string]string) {
			values["KOGNITION_SPIFFE_GATEWAY_ENDPOINTS"] = strings.Replace(string(body), "https://", "http://", 1)
		},
	} {
		test.Run(name, func(test *testing.T) {
			values := map[string]string{"KOGNITION_WORKLOAD_IDENTITY_MODE": "spire", "KOGNITION_WORKLOAD_PROXY_IMAGE": "proxy@sha256:" + strings.Repeat("1", 64), "KOGNITION_SPIFFE_TRUST_DOMAIN": "kognition.dev", "KOGNITION_SPIFFE_ID": "spiffe://kognition.dev/controller/identity", "KOGNITION_SPIFFE_GATEWAY_ENDPOINTS": string(body)}
			mutate(values)
			configuration, err := WorkloadIdentityDeliveryFromEnv(func(key string) string { return values[key] })
			if (err == nil) != (name == "valid") {
				test.Fatalf("configuration=%v error=%v", configuration, err)
			}
		})
	}
	configuration, err := WorkloadIdentityDeliveryFromEnv(func(string) string { return "" })
	if err != nil || configuration != nil {
		test.Fatal("legacy defaults enabled identity delivery")
	}
}
