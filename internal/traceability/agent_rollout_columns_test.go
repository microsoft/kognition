// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"os"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource/tableconvertor"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func agentRolloutColumns(t *testing.T) []apiextensionsv1.CustomResourceColumnDefinition {
	t.Helper()
	data, err := os.ReadFile("../../deploy/helm/kognition/crds/kognition.dev_agents.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatal("expected one Agent CRD version")
	}
	return crd.Spec.Versions[0].AdditionalPrinterColumns
}

func TestAgentCRDShowsDeploymentStyleRuntimeColumns(t *testing.T) {
	expected := []apiextensionsv1.CustomResourceColumnDefinition{
		{Name: "Ready", Type: "string", JSONPath: ".status.runtime.ready"},
		{Name: "Up-to-date", Type: "integer", JSONPath: ".status.runtime.updatedReplicas"},
		{Name: "Available", Type: "integer", JSONPath: ".status.runtime.availableReplicas"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase", Priority: 1},
	}
	if columns := agentRolloutColumns(t); !reflect.DeepEqual(columns, expected) {
		t.Fatalf("Agent printer columns = %#v, want %#v", columns, expected)
	}
}

func TestAgentRuntimeTableRendersDeploymentStyleRows(t *testing.T) {
	converter, err := tableconvertor.New(agentRolloutColumns(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, ready := range []string{"0/2", "1/2", "2/2", "0/0"} {
		t.Run(ready, func(t *testing.T) {
			agent := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kognition.dev/v1alpha1", "kind": "Agent",
				"metadata": map[string]any{"name": "worker", "namespace": "work"},
				"status": map[string]any{
					"phase": "Running",
					"runtime": map[string]any{
						"ready": ready, "updatedReplicas": int64(0), "availableReplicas": int64(0),
					},
				},
			}}
			table, err := converter.ConvertToTable(context.Background(), agent, &metav1.TableOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(table.Rows) != 1 || len(table.Rows[0].Cells) != 6 {
				t.Fatalf("unexpected rows: %+v", table.Rows)
			}
			cells := table.Rows[0].Cells
			if !reflect.DeepEqual(cells[:4], []any{"worker", ready, int64(0), int64(0)}) {
				t.Fatalf("wrong runtime cells: %+v", cells)
			}
			var defaultColumns []string
			for _, column := range table.ColumnDefinitions {
				if column.Priority == 0 {
					defaultColumns = append(defaultColumns, column.Name)
				}
			}
			if !reflect.DeepEqual(defaultColumns, []string{"Name", "Ready", "Up-to-date", "Available", "Age"}) {
				t.Fatalf("default columns = %v", defaultColumns)
			}
			last := len(table.ColumnDefinitions) - 1
			if table.ColumnDefinitions[last].Name != "Phase" || table.ColumnDefinitions[last].Priority != 1 || cells[last] != "Running" {
				t.Fatal("wide output lost lifecycle Phase")
			}
		})
	}
}
