// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/providers/pgvector"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func validBackend() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": pgvector.APIGroup + "/" + pgvector.APIVersion,
		"kind":       pgvector.Kind,
		"metadata":   map[string]interface{}{"name": "shared", "namespace": "tenant-a"},
		"spec": map[string]interface{}{
			"host": "postgres.tenant-a.svc.cluster.local", "port": int64(5432), "database": "knowledge", "table": "chunks",
			"embeddingDimensions":  int64(768),
			"credentialsSecretRef": map[string]interface{}{"name": "database", "usernameKey": "username", "passwordKey": "password"},
			"tls":                  map[string]interface{}{"mode": "verify-full", "caSecretRef": map[string]interface{}{"name": "database-ca", "key": "ca.crt"}},
		},
	}}
}

func TestDecodeBackendAcceptsTypedConfiguration(t *testing.T) {
	configuration := validBackend()
	spec, err := pgvector.DecodeBackend(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Host != "postgres.tenant-a.svc.cluster.local" || spec.Port != 5432 || spec.Database != "knowledge" ||
		spec.Table != "chunks" || spec.EmbeddingDimensions != 768 || spec.TLS.Mode != "verify-full" ||
		spec.CredentialsSecretRef.Name != "database" || spec.TLS.CASecretRef.Name != "database-ca" {
		t.Fatalf("unexpected decoded configuration: %+v", spec)
	}
	spec.TLS.CASecretRef.Name = "changed"
	again, err := pgvector.DecodeBackend(configuration)
	if err != nil || again.TLS.CASecretRef.Name != "database-ca" {
		t.Fatal("decoded configuration aliases its source")
	}
	encoded, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip pgvector.BackendSpec
	if err := json.Unmarshal(encoded, &roundTrip); err != nil || roundTrip.Validate() != nil {
		t.Fatalf("invalid typed round trip: %v", err)
	}
}

func invalidSpecs() map[string]func(map[string]interface{}) {
	return map[string]func(map[string]interface{}){
		"inline credentials": func(spec map[string]interface{}) { spec["password"] = "private-value" },
		"connection string":  func(spec map[string]interface{}) { spec["connectionString"] = "private-value" },
		"adapter endpoint":   func(spec map[string]interface{}) { spec["endpoint"] = "https://private-value" },
		"cross namespace": func(spec map[string]interface{}) {
			spec["credentialsSecretRef"].(map[string]interface{})["namespace"] = "private-value"
		},
		"cross namespace CA": func(spec map[string]interface{}) {
			spec["tls"].(map[string]interface{})["caSecretRef"].(map[string]interface{})["namespace"] = "private-value"
		},
		"host URL":       func(spec map[string]interface{}) { spec["host"] = "postgres://private-value" },
		"host with port": func(spec map[string]interface{}) { spec["host"] = "private-value:5432" },
		"missing host":   func(spec map[string]interface{}) { delete(spec, "host") },
		"port zero":      func(spec map[string]interface{}) { spec["port"] = int64(0) },
		"port overflow":  func(spec map[string]interface{}) { spec["port"] = int64(65536) },
		"port string":    func(spec map[string]interface{}) { spec["port"] = "5432" },
		"SQL table":      func(spec map[string]interface{}) { spec["table"] = "chunks;private-value" },
		"qualified table": func(spec map[string]interface{}) {
			spec["table"] = "other.chunks"
		},
		"database options": func(spec map[string]interface{}) { spec["database"] = "private-value sslmode=disable" },
		"zero dimensions":  func(spec map[string]interface{}) { spec["embeddingDimensions"] = int64(0) },
		"large dimensions": func(spec map[string]interface{}) { spec["embeddingDimensions"] = int64(2001) },
		"missing credentials": func(spec map[string]interface{}) {
			delete(spec, "credentialsSecretRef")
		},
		"empty key": func(spec map[string]interface{}) {
			spec["credentialsSecretRef"].(map[string]interface{})["passwordKey"] = ""
		},
		"duplicate keys": func(spec map[string]interface{}) {
			spec["credentialsSecretRef"].(map[string]interface{})["passwordKey"] = "username"
		},
		"secret path": func(spec map[string]interface{}) {
			spec["credentialsSecretRef"].(map[string]interface{})["name"] = "other/private-value"
		},
		"CA key path": func(spec map[string]interface{}) {
			spec["tls"].(map[string]interface{})["caSecretRef"].(map[string]interface{})["key"] = "../private-value"
		},
		"TLS disabled": func(spec map[string]interface{}) { spec["tls"].(map[string]interface{})["mode"] = "disable" },
		"TLS require":  func(spec map[string]interface{}) { spec["tls"].(map[string]interface{})["mode"] = "require" },
		"TLS omitted":  func(spec map[string]interface{}) { delete(spec, "tls") },
	}
}

func TestDecodeBackendRejectsInvalidConfiguration(t *testing.T) {
	for name, mutate := range invalidSpecs() {
		t.Run(name, func(t *testing.T) {
			configuration := validBackend()
			mutate(configuration.Object["spec"].(map[string]interface{}))
			_, err := pgvector.DecodeBackend(configuration)
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
		func(configuration *unstructured.Unstructured) { configuration.SetAPIVersion(pgvector.APIGroup + "/v2") },
		func(configuration *unstructured.Unstructured) { configuration.SetKind("OtherBackend") },
		func(configuration *unstructured.Unstructured) {
			stamp := metav1.Now()
			configuration.SetDeletionTimestamp(&stamp)
		},
		func(configuration *unstructured.Unstructured) { delete(configuration.Object, "spec") },
	} {
		configuration := validBackend()
		mutate(configuration)
		if _, err := pgvector.DecodeBackend(configuration); err == nil {
			t.Fatal("accepted invalid object")
		}
	}
	if _, err := pgvector.DecodeBackend(nil); err == nil {
		t.Fatal("accepted nil object")
	}
}

func TestDecodeBackendSupportsSystemRootsAndDimensionBounds(t *testing.T) {
	for _, dimensions := range []int64{1, 768, 1536, 2000} {
		configuration := validBackend()
		unstructured.RemoveNestedField(configuration.Object, "spec", "tls", "caSecretRef")
		if err := unstructured.SetNestedField(configuration.Object, dimensions, "spec", "embeddingDimensions"); err != nil {
			t.Fatal(err)
		}
		spec, err := pgvector.DecodeBackend(configuration)
		if err != nil || spec.TLS.CASecretRef != nil || int64(spec.EmbeddingDimensions) != dimensions {
			t.Fatalf("valid configuration rejected: %v", err)
		}
	}
}
