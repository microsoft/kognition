// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestMigrationContainersHaveResourceBudgets(test *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		test.Skip("helm is required for chart rendering")
	}
	for _, scenario := range []struct {
		name         string
		values       []string
		initializers int
	}{
		{name: "standalone", initializers: 1},
		{name: "kubernetes", values: []string{"mode=kubernetes"}, initializers: 1},
		{name: "external-postgres", values: []string{"postgres.enabled=false"}},
		{name: "custom-budgets", values: []string{
			"migrations.resources.requests.cpu=75m",
			"migrations.resources.requests.memory=96Mi",
			"migrations.resources.limits.cpu=400m",
			"migrations.resources.limits.memory=384Mi",
			"migrations.waitForPostgres.resources.requests.cpu=20m",
			"migrations.waitForPostgres.resources.requests.memory=24Mi",
			"migrations.waitForPostgres.resources.limits.cpu=150m",
			"migrations.waitForPostgres.resources.limits.memory=48Mi",
		}, initializers: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			arguments := []string{"template", "resource-test", "../../deploy/helm/kognition", "--kube-version", "1.31.1", "--show-only", "templates/migrations-job.yaml"}
			for _, value := range scenario.values {
				arguments = append(arguments, "--set", value)
			}
			output, err := exec.CommandContext(ctx, helm, arguments...).Output()
			if err != nil {
				test.Fatalf("render migrations: %v", err)
			}
			document, err := yaml.ToJSON(output)
			if err != nil {
				test.Fatal(err)
			}
			var job batchv1.Job
			if err := json.Unmarshal(document, &job); err != nil {
				test.Fatal(err)
			}
			pod := job.Spec.Template.Spec
			if job.Kind != "Job" || len(pod.Containers) != 1 || len(pod.InitContainers) != scenario.initializers {
				test.Fatal("unexpected migration job containers")
			}
			for _, container := range append(pod.InitContainers, pod.Containers...) {
				for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
					request := container.Resources.Requests[name]
					limit := container.Resources.Limits[name]
					if request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
						test.Errorf("%s requires positive %s request <= limit", container.Name, name)
					}
				}
				if scenario.name == "custom-budgets" {
					expected := []string{"75m", "96Mi", "400m", "384Mi"}
					if container.Name == "wait-for-postgres" {
						expected = []string{"20m", "24Mi", "150m", "48Mi"}
					}
					actual := []resource.Quantity{container.Resources.Requests[corev1.ResourceCPU], container.Resources.Requests[corev1.ResourceMemory], container.Resources.Limits[corev1.ResourceCPU], container.Resources.Limits[corev1.ResourceMemory]}
					for index, quantity := range actual {
						if quantity.Cmp(resource.MustParse(expected[index])) != 0 {
							test.Errorf("%s resource %d did not honor override", container.Name, index)
						}
					}
				}
			}
		})
	}
}
