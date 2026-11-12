// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestStandaloneWaitContainersHaveResourceBudgets(test *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		test.Skip("helm is required for chart rendering")
	}
	for _, component := range []struct {
		name     string
		template string
	}{
		{name: "apiserver", template: "apiserver-deployment.yaml"},
		{name: "controllerManager", template: "controller-manager-deployment.yaml"},
	} {
		for _, scenario := range []string{"defaults", "custom-budgets", "external-postgres"} {
			test.Run(component.name+"/"+scenario, func(test *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				arguments := []string{"template", "resource-test", "../../deploy/helm/kognition", "--kube-version", "1.31.1", "--show-only", "templates/" + component.template}
				expected := []string{"10m", "16Mi", "100m", "32Mi"}
				if scenario == "custom-budgets" {
					expected = []string{"20m", "24Mi", "150m", "48Mi"}
					for index, field := range []string{"requests.cpu", "requests.memory", "limits.cpu", "limits.memory"} {
						arguments = append(arguments, "--set", component.name+".waitForPostgres.resources."+field+"="+expected[index])
					}
				}
				if scenario == "external-postgres" {
					arguments = append(arguments, "--set", "postgres.enabled=false")
				}
				output, err := exec.CommandContext(ctx, helm, arguments...).CombinedOutput()
				if err != nil {
					test.Fatalf("render deployment: %v\n%s", err, output)
				}
				document, err := yaml.ToJSON(output)
				if err != nil {
					test.Fatal(err)
				}
				var deployment appsv1.Deployment
				if err := json.Unmarshal(document, &deployment); err != nil {
					test.Fatal(err)
				}
				if deployment.Kind != "Deployment" {
					test.Fatal("expected deployment")
				}
				initializers := deployment.Spec.Template.Spec.InitContainers
				if scenario == "external-postgres" {
					if len(initializers) != 0 {
						test.Fatal("external PostgreSQL must not create wait containers")
					}
					return
				}
				if len(initializers) != 1 || initializers[0].Name != "wait-for-postgres" {
					test.Fatal("expected one PostgreSQL wait container")
				}
				budgets := initializers[0].Resources
				actual := []resource.Quantity{budgets.Requests[corev1.ResourceCPU], budgets.Requests[corev1.ResourceMemory], budgets.Limits[corev1.ResourceCPU], budgets.Limits[corev1.ResourceMemory]}
				for index, quantity := range actual {
					if quantity.Cmp(resource.MustParse(expected[index])) != 0 {
						test.Errorf("wait container resource %d: got %s, want %s", index, quantity.String(), expected[index])
					}
				}
			})
		}
	}
}
