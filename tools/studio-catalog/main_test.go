// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestCatalogScalarTypesMatchJSONEncoding(t *testing.T) {
	for _, tc := range []struct {
		value    any
		typeName string
	}{
		{time.Second, "integer"},
		{time.Now(), "string"},
		{api.Duration{Duration: time.Second}, "string"},
		{[]byte("opaque"), "string"},
	} {
		if got := typeSchema(reflect.TypeOf(tc.value), map[reflect.Type]bool{})["type"]; got != tc.typeName {
			t.Fatalf("%T: schema type %v, want %s", tc.value, got, tc.typeName)
		}
	}
}

func TestCatalogMatchesRegistryAndShippedAsset(t *testing.T) {
	root := filepath.Join("..", "..")
	actual, err := catalog(root)
	if err != nil {
		t.Fatal(err)
	}
	var entries []resource
	if err := json.Unmarshal(actual, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(api.KindRegistry) {
		t.Fatalf("catalog has %d kinds, registry has %d", len(entries), len(api.KindRegistry))
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.Kind] || api.KindRegistry[entry.Kind] == nil {
			t.Fatalf("duplicate or unknown kind %s", entry.Kind)
		}
		seen[entry.Kind] = true
		switch entry.Kind {
		case "RuntimeApproval", "ControlPlaneRole", "ControlPlaneRoleBinding", "ModelAutoscaler":
			if entry.Treatment != "unavailable" || entry.Reason == "" {
				t.Fatalf("protected/deferred kind is authorable: %+v", entry)
			}
		case "AgentRole":
			standalone := entry.Schema["properties"].(map[string]any)
			kubernetes := entry.KubernetesSchema["properties"].(map[string]any)
			if standalone["rules"] == nil || standalone["spec"] != nil || kubernetes["rules"] == nil || kubernetes["spec"] != nil {
				t.Fatal("AgentRole rules belong at the resource root, not in an invented spec wrapper")
			}
		}
	}
	expected, err := os.ReadFile(filepath.Join(root, "cmd", "kognitionctl", "cmd", "studio_assets", "resource-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("Studio catalog is stale; run go run .\\tools\\studio-catalog")
	}
}
