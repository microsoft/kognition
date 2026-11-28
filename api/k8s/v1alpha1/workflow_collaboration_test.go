// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestCollaborationCRDsRequireUserContentLimit(t *testing.T) {
	for _, resource := range []string{"workflows", "workflowruns"} {
		t.Run(resource, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "helm", "kognition", "crds", "kognition.dev_"+resource+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.Unmarshal(raw, &crd); err != nil {
				t.Fatal(err)
			}
			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			if resource == "workflowruns" {
				schema = crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["snapshotSpec"]
			}
			steps := schema.Properties["steps"]
			if steps.Items == nil || steps.Items.Schema == nil {
				t.Fatal("workflow step schema is missing")
			}
			limits := steps.Items.Schema.Properties["collaboration"].Properties["limits"]
			content := limits.Properties["maxContentBytes"]
			if content.Type != "integer" || content.Minimum == nil || *content.Minimum != 1 ||
				content.Maximum != nil || content.Default != nil {
				t.Fatalf("content limit must be positive, user-defined and without a default: %+v", content)
			}
			required := false
			for _, field := range limits.Required {
				required = required || field == "maxContentBytes"
			}
			if !required {
				t.Fatal("maxContentBytes is not required by the CRD")
			}
		})
	}
}
