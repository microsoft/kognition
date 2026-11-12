// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/utils/ptr"
)

func TestControllerWorkloadIdentityCSIDeliveryIsOptIn(test *testing.T) {
	for _, mode := range []string{"kubernetes", "standalone"} {
		test.Run(mode, func(test *testing.T) {
			for _, enabled := range []bool{false, true} {
				arguments := []string{"--set", "mode=" + mode}
				if enabled {
					arguments = append(arguments, "--set", "workloadIdentityStatus.enabled=true", "--set", "workloadIdentityStatus.trustDomain=kognition.dev", "--set", "workloadIdentityStatus.controllerID=spiffe://kognition.dev/controller/identity", "--set", "workloadIdentityStatus.existingRegistryConfigMap=operator-registry", "--set", "workloadIdentityStatus.runtimeNamespace=runtime", "--set", "networkPolicy.enabled=true")
				}
				found := false
				foundRoute := !enabled
				for _, document := range renderControllerChart(test, arguments...) {
					var deployment appsv1.Deployment
					if json.Unmarshal(document.Raw, &deployment) != nil {
						test.Fatal("invalid rendered deployment")
					}
					component := deployment.Labels["app.kubernetes.io/component"]
					if deployment.Kind == "NetworkPolicy" && (component == "k8s-controller" || component == "controller-manager") {
						var policy networkingv1.NetworkPolicy
						if err := json.Unmarshal(document.Raw, &policy); err != nil {
							test.Fatal(err)
						}
						for _, rule := range policy.Spec.Egress {
							for _, port := range rule.Ports {
								if port.Port != nil && port.Port.IntVal == 8097 {
									if !enabled || len(rule.To) != 1 || len(rule.Ports) != 1 || rule.To[0].NamespaceSelector == nil || rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "runtime" || rule.To[0].PodSelector == nil || rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/managed-by"] != "kognition" {
										test.Fatal("status egress is not restricted")
									}
									foundRoute = true
								}
							}
						}
					}
					if deployment.Kind != "Deployment" || (component != "k8s-controller" && component != "controller-manager") {
						continue
					}
					found = true
					count := 0
					for _, volume := range deployment.Spec.Template.Spec.Volumes {
						if volume.CSI != nil {
							count++
							if volume.Name != "workload-api" || volume.CSI.Driver != "csi.spiffe.io" || volume.CSI.ReadOnly == nil || !*volume.CSI.ReadOnly || volume.HostPath != nil {
								test.Fatal("unsafe Workload API volume")
							}
						}
					}
					if (count == 1) != enabled {
						test.Fatal("CSI socket must be opt-in")
					}
					for _, container := range deployment.Spec.Template.Spec.Containers {
						if container.Name != component {
							continue
						}
						seen := false
						for _, variable := range container.Env {
							if variable.Name == "KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE" {
								seen = true
								if variable.Value != "/etc/kognition/workload-status/registry.json" {
									test.Fatal("registry path changed")
								}
							}
						}
						if seen != enabled {
							test.Fatal("status reporting enabled implicitly")
						}
						for _, mount := range container.VolumeMounts {
							if (mount.Name == "workload-api" || mount.Name == "workload-status-registry") && !mount.ReadOnly {
								test.Fatal("operator mount must be read-only")
							}
						}
					}
				}
				if !found {
					test.Fatal("controller deployment missing")
				}
				if !foundRoute {
					test.Fatal("controller status egress missing")
				}
			}
		})
	}
}

func TestControllerWorkloadAPICSIAdmission(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		arguments := []string{"--set", "mode=kubernetes"}
		if enabled {
			arguments = append(arguments, "--set", "workloadIdentityStatus.enabled=true", "--set", "workloadIdentityStatus.trustDomain=kognition.dev", "--set", "workloadIdentityStatus.controllerID=spiffe://kognition.dev/controller/identity", "--set", "workloadIdentityStatus.existingRegistryConfigMap=operator-registry", "--set", "workloadIdentityStatus.runtimeNamespace=runtime")
		}
		var validations []string
		for _, document := range renderControllerChart(test, arguments...) {
			var policy admissionv1.ValidatingAdmissionPolicy
			if err := json.Unmarshal(document.Raw, &policy); err != nil {
				test.Fatal(err)
			}
			if policy.Kind != "ValidatingAdmissionPolicy" || !strings.HasSuffix(policy.Name, "-workload") {
				continue
			}
			for _, validation := range policy.Spec.Validations {
				validations = append(validations, validation.Expression)
			}
		}
		if len(validations) == 0 {
			test.Fatal("workload admission policy missing")
		}
		pod := corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{
			{Name: "agent", SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}},
			{Name: "workload-identity-proxy", SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}, VolumeMounts: []corev1.VolumeMount{{Name: "workload-api", MountPath: "/spiffe-workload-api", ReadOnly: true}}},
		}, Volumes: []corev1.Volume{{Name: "workload-api", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "csi.spiffe.io", ReadOnly: ptr.To(true)}}}}}
		for name, mutate := range map[string]func(*corev1.PodSpec){
			"protected":    func(*corev1.PodSpec) {},
			"other-driver": func(pod *corev1.PodSpec) { pod.Volumes[0].CSI.Driver = "other.csi" },
			"writable":     func(pod *corev1.PodSpec) { pod.Volumes[0].CSI.ReadOnly = ptr.To(false) },
			"attributes": func(pod *corev1.PodSpec) {
				pod.Volumes[0].CSI.VolumeAttributes = map[string]string{"identity": "controller"}
			},
			"secret": func(pod *corev1.PodSpec) {
				pod.Volumes[0].CSI.NodePublishSecretRef = &corev1.LocalObjectReference{Name: "issuer"}
			},
			"agent-mount": func(pod *corev1.PodSpec) { pod.Containers[0].VolumeMounts = pod.Containers[1].VolumeMounts },
			"init-mount":  func(pod *corev1.PodSpec) { pod.InitContainers = []corev1.Container{pod.Containers[1]} },
			"subpath":     func(pod *corev1.PodSpec) { pod.Containers[1].VolumeMounts[0].SubPath = "socket" },
			"hostpath": func(pod *corev1.PodSpec) {
				pod.Volumes[0].CSI = nil
				pod.Volumes[0].HostPath = &corev1.HostPathVolumeSource{Path: "/"}
			},
		} {
			test.Run(name+map[bool]string{false: "/disabled", true: "/enabled"}[enabled], func(test *testing.T) {
				candidate := pod.DeepCopy()
				mutate(candidate)
				body, err := json.Marshal(candidate)
				if err != nil {
					test.Fatal(err)
				}
				var decoded map[string]any
				if err := json.Unmarshal(body, &decoded); err != nil {
					test.Fatal(err)
				}
				object := map[string]any{"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent": "worker"}}}
				environment, err := cel.NewEnv(cel.Variable("variables", cel.DynType), cel.Variable("request", cel.DynType))
				if err != nil {
					test.Fatal(err)
				}
				accepted := true
				for _, expression := range validations {
					ast, issues := environment.Compile(expression)
					if issues.Err() != nil {
						test.Fatal(issues.Err())
					}
					program, err := environment.Program(ast)
					if err != nil {
						test.Fatal(err)
					}
					result, _, err := program.Eval(map[string]any{"variables": map[string]any{"pods": []any{decoded}, "candidates": []any{object}}, "request": map[string]any{"resource": map[string]any{"resource": "pods"}}})
					if err != nil {
						test.Fatal(err)
					}
					accepted = accepted && result == types.True
				}
				if accepted != (enabled && name == "protected") {
					test.Fatalf("accepted=%v for enabled=%v", accepted, enabled)
				}
			})
		}
	}
}

func TestManagedSPIREChartRequiresCompleteOperatorConfiguration(test *testing.T) {
	arguments := []string{"--set", "mode=kubernetes", "--set", "workloadIdentityStatus.enabled=true", "--set", "workloadIdentityStatus.trustDomain=kognition.dev", "--set", "workloadIdentityStatus.controllerID=spiffe://kognition.dev/controller/identity", "--set", "workloadIdentityStatus.existingRegistryConfigMap=operator-registry", "--set", "workloadIdentityStatus.runtimeNamespace=runtime", "--set", "k8sController.agentNamespace=runtime", "--set", "networkPolicy.enabled=true", "--set", "workloadIdentityStatus.managedWorkloads.enabled=true", "--set", "workloadIdentityStatus.managedWorkloads.proxyImage=proxy@sha256:" + strings.Repeat("1", 64)}
	for _, plane := range []string{"model", "tool", "memory", "communication"} {
		arguments = append(arguments, "--set", "workloadIdentityStatus.managedWorkloads.gateways."+plane+".url=https://"+plane+".control:9443", "--set", "workloadIdentityStatus.managedWorkloads.gateways."+plane+".peerID=spiffe://kognition.dev/gateway/"+plane)
	}
	found := false
	for _, document := range renderControllerChart(test, arguments...) {
		var deployment appsv1.Deployment
		if err := json.Unmarshal(document.Raw, &deployment); err != nil {
			test.Fatal(err)
		}
		if deployment.Kind != "Deployment" || deployment.Labels["app.kubernetes.io/component"] != "k8s-controller" {
			continue
		}
		for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
			if variable.Name == "KOGNITION_WORKLOAD_IDENTITY_MODE" {
				found = variable.Value == "spire"
			}
		}
	}
	if !found {
		test.Fatal("managed SPIRE configuration was not passed to dispatcher")
	}
	for _, override := range []string{"mode=standalone", "networkPolicy.enabled=false", "workloadIdentityStatus.enabled=false", "workloadIdentityStatus.managedWorkloads.proxyImage=proxy:latest", "workloadIdentityStatus.managedWorkloads.gateways.tool.url=http://tool.control:9443", "workloadIdentityStatus.managedWorkloads.gateways.tool.peerID=spiffe://other.dev/gateway/tool", "k8sController.agentNamespace=other", "workloadIdentityStatus.existingRegistryConfigMap=", "workloadIdentityStatus.runtimeNamespace="} {
		test.Run(override, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := append([]string{"template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--set", "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12"}, arguments...)
			command = append(command, "--set", override)
			output, err := exec.CommandContext(ctx, "helm", command...).CombinedOutput()
			if err == nil || !strings.Contains(string(output), "Error:") {
				test.Fatalf("unsafe chart override accepted: %s: %v", output, err)
			}
		})
	}
}
