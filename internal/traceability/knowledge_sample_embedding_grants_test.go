// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestKnowledgeSamplesAuthorizeBoundEmbeddingModels(t *testing.T) {
	read := func(t *testing.T, path string) []*unstructured.Unstructured {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "samples", filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		objects, err := installation.Documents(data)
		if err != nil {
			t.Fatal(err)
		}
		return objects
	}
	grants := read(t, "model-grants.yaml")
	for _, sample := range []struct {
		name, agent, class, manifest string
	}{
		{"azure", "azure-docs-agent", "azure-blob-kb", "knowledge-base-blob-azure/manifests.yaml"},
		{"s3", "s3-docs-agent", "s3-blob-kb", "knowledge-base-blob-s3/manifests.yaml"},
		{"documentation", "kognition-docs-agent", "kognition-docs-kb-class", "knowledge-base-kognition-docs/kb-class.yaml"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			embeddingModel := ""
			for _, object := range read(t, sample.manifest) {
				if object.GetKind() == "KnowledgeBaseClass" && object.GetName() == sample.class {
					embeddingModel, _, _ = unstructured.NestedString(object.Object, "spec", "embeddingModelRef", "name")
				}
			}
			if embeddingModel == "" {
				t.Fatal("knowledge sample must declare its embedding Model")
			}
			for _, grant := range grants {
				agent, _, _ := unstructured.NestedString(grant.Object, "spec", "agentRef", "name")
				model, _, _ := unstructured.NestedString(grant.Object, "spec", "modelRef", "name")
				if grant.GetKind() == "ModelGrant" && grant.GetNamespace() == "default" && agent == sample.agent && model == embeddingModel {
					return
				}
			}
			t.Fatalf("native knowledge Agent %q lacks permission to query with bound embedding Model %q", sample.agent, embeddingModel)
		})
	}
}
