// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/providers/azuresearch"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func validBackend() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": azuresearch.APIGroup + "/" + azuresearch.APIVersion,
		"kind":       azuresearch.Kind,
		"metadata":   map[string]interface{}{"name": "shared", "namespace": "tenant-a"},
		"spec": map[string]interface{}{
			"serviceName": "kognition-search", "indexName": "knowledge_chunks", "embeddingDimensions": int64(1536),
			"authentication": map[string]interface{}{"mode": "workload-identity"},
		},
	}}
}

func TestDecodeBackendAcceptsTypedConfiguration(t *testing.T) {
	spec, err := azuresearch.DecodeBackend(validBackend())
	if err != nil {
		t.Fatal(err)
	}
	if spec.ServiceName != "kognition-search" || spec.IndexName != "knowledge_chunks" || spec.EmbeddingDimensions != 1536 ||
		spec.Authentication.Mode != "workload-identity" {
		t.Fatalf("unexpected decoded configuration: %+v", spec)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip azuresearch.BackendSpec
	if err := json.Unmarshal(encoded, &roundTrip); err != nil || roundTrip.Validate() != nil {
		t.Fatalf("invalid typed round trip: %v", err)
	}
}

func invalidSpecs() map[string]func(map[string]interface{}) {
	return map[string]func(map[string]interface{}){
		"inline API key":     func(spec map[string]interface{}) { spec["apiKey"] = "private-value" },
		"arbitrary endpoint": func(spec map[string]interface{}) { spec["endpoint"] = "https://private-value" },
		"credential reference": func(spec map[string]interface{}) {
			spec["credentialsSecretRef"] = map[string]interface{}{"name": "private-value"}
		},
		"service URL":         func(spec map[string]interface{}) { spec["serviceName"] = "https://private-value.search.windows.net" },
		"service uppercase":   func(spec map[string]interface{}) { spec["serviceName"] = "PrivateValue" },
		"service second dash": func(spec map[string]interface{}) { spec["serviceName"] = "a-private-value" },
		"service double dash": func(spec map[string]interface{}) { spec["serviceName"] = "private--value" },
		"service too long":    func(spec map[string]interface{}) { spec["serviceName"] = strings.Repeat("a", 61) },
		"missing service":     func(spec map[string]interface{}) { delete(spec, "serviceName") },
		"index uppercase":     func(spec map[string]interface{}) { spec["indexName"] = "PrivateValue" },
		"index leading dash":  func(spec map[string]interface{}) { spec["indexName"] = "-private-value" },
		"index double dash":   func(spec map[string]interface{}) { spec["indexName"] = "private--value" },
		"index double underscore": func(spec map[string]interface{}) {
			spec["indexName"] = "private__value"
		},
		"missing index":    func(spec map[string]interface{}) { delete(spec, "indexName") },
		"zero dimensions":  func(spec map[string]interface{}) { spec["embeddingDimensions"] = int64(0) },
		"large dimensions": func(spec map[string]interface{}) { spec["embeddingDimensions"] = int64(4097) },
		"dimension string": func(spec map[string]interface{}) { spec["embeddingDimensions"] = "1536" },
		"API key mode": func(spec map[string]interface{}) {
			spec["authentication"].(map[string]interface{})["mode"] = "api-key"
		},
		"missing authentication": func(spec map[string]interface{}) { delete(spec, "authentication") },
	}
}

func TestDecodeBackendRejectsInvalidConfiguration(t *testing.T) {
	for name, mutate := range invalidSpecs() {
		t.Run(name, func(t *testing.T) {
			configuration := validBackend()
			mutate(configuration.Object["spec"].(map[string]interface{}))
			_, err := azuresearch.DecodeBackend(configuration)
			if err == nil || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("must reject without exposing input: %v", err)
			}
		})
	}
}

func TestDecodeBackendRequiresNamespacedLiveObject(t *testing.T) {
	for _, mutate := range []func(*unstructured.Unstructured){
		func(configuration *unstructured.Unstructured) { configuration.SetNamespace("") },
		func(configuration *unstructured.Unstructured) { configuration.SetName("") },
		func(configuration *unstructured.Unstructured) {
			configuration.SetAPIVersion(azuresearch.APIGroup + "/v2")
		},
		func(configuration *unstructured.Unstructured) { configuration.SetKind("OtherBackend") },
		func(configuration *unstructured.Unstructured) {
			stamp := metav1.Now()
			configuration.SetDeletionTimestamp(&stamp)
		},
		func(configuration *unstructured.Unstructured) { delete(configuration.Object, "spec") },
	} {
		configuration := validBackend()
		mutate(configuration)
		if _, err := azuresearch.DecodeBackend(configuration); err == nil {
			t.Fatal("accepted invalid object")
		}
	}
	if _, err := azuresearch.DecodeBackend(nil); err == nil {
		t.Fatal("accepted nil object")
	}
}

func TestDecodeBackendSupportsDimensionBounds(t *testing.T) {
	for _, dimensions := range []int64{1, 768, 1536, 3072, 4096} {
		configuration := validBackend()
		if err := unstructured.SetNestedField(configuration.Object, dimensions, "spec", "embeddingDimensions"); err != nil {
			t.Fatal(err)
		}
		spec, err := azuresearch.DecodeBackend(configuration)
		if err != nil || int64(spec.EmbeddingDimensions) != dimensions {
			t.Fatalf("valid configuration rejected: %v", err)
		}
	}
}
