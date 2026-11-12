// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestKBPgvectorInitializationIsRestricted(test *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		test.Skip("helm is required for chart rendering")
	}
	for _, scenario := range []string{"defaults", "custom-budgets"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			arguments := []string{"template", "security-test", "../../deploy/helm/kognition", "--kube-version", "1.31.1", "--set", "kbPgvector.enabled=true", "--show-only", "templates/kb-pgvector.yaml"}
			expected := []string{"10m", "32Mi", "100m", "64Mi"}
			if scenario == "custom-budgets" {
				expected = []string{"20m", "48Mi", "200m", "96Mi"}
				for index, field := range []string{"requests.cpu", "requests.memory", "limits.cpu", "limits.memory"} {
					arguments = append(arguments, "--set", "kbPgvector.init.resources."+field+"="+expected[index])
				}
			}
			output, err := exec.CommandContext(ctx, helm, arguments...).CombinedOutput()
			if err != nil {
				test.Fatalf("render pgvector: %v\n%s", err, output)
			}
			decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
			jobs := 0
			for {
				var job batchv1.Job
				if err := decoder.Decode(&job); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					test.Fatal(err)
				}
				if job.Kind != "Job" {
					continue
				}
				jobs++
				pod := job.Spec.Template.Spec
				if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
					test.Error("initialization must not mount a Kubernetes API token")
				}
				security := pod.SecurityContext
				if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.RunAsUser == nil || *security.RunAsUser <= 0 || security.RunAsGroup == nil || *security.RunAsGroup <= 0 || security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					test.Error("initialization must use non-root identity and RuntimeDefault seccomp")
				}
				if len(pod.Containers) != 1 || pod.Containers[0].Name != "init" {
					test.Fatal("expected one initialization container")
				}
				container := pod.Containers[0]
				context := container.SecurityContext
				if context == nil || context.AllowPrivilegeEscalation == nil || *context.AllowPrivilegeEscalation || context.ReadOnlyRootFilesystem == nil || !*context.ReadOnlyRootFilesystem || context.Capabilities == nil || len(context.Capabilities.Drop) != 1 || context.Capabilities.Drop[0] != "ALL" {
					test.Error("initialization must be read-only, drop ALL capabilities, and prohibit escalation")
				}
				budgets := container.Resources
				actual := []resource.Quantity{budgets.Requests[corev1.ResourceCPU], budgets.Requests[corev1.ResourceMemory], budgets.Limits[corev1.ResourceCPU], budgets.Limits[corev1.ResourceMemory]}
				for index, quantity := range actual {
					if quantity.Cmp(resource.MustParse(expected[index])) != 0 {
						test.Errorf("initialization resource %d: got %s, want %s", index, quantity.String(), expected[index])
					}
				}
			}
			if jobs != 1 {
				test.Fatalf("expected one initialization Job, got %d", jobs)
			}
		})
	}
}
