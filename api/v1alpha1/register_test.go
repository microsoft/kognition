// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

func TestKindRegistryConstructsEveryRegisteredResource(t *testing.T) {
	kinds := RegisteredKinds()
	if len(kinds) != len(KindRegistry) {
		t.Fatalf("RegisteredKinds() returned %d kinds, registry contains %d", len(kinds), len(KindRegistry))
	}

	seen := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		if seen[kind] {
			t.Fatalf("RegisteredKinds() contains duplicate %q", kind)
		}
		seen[kind] = true

		first, err := NewObject(kind)
		if err != nil {
			t.Fatalf("NewObject(%q) error = %v", kind, err)
		}
		second, err := NewObject(kind)
		if err != nil {
			t.Fatalf("second NewObject(%q) error = %v", kind, err)
		}
		if first == nil || second == nil {
			t.Fatalf("NewObject(%q) returned nil", kind)
		}
		if reflect.TypeOf(first).Elem().Name() != kind {
			t.Fatalf("NewObject(%q) returned %T", kind, first)
		}
		if reflect.ValueOf(first).Pointer() == reflect.ValueOf(second).Pointer() {
			t.Fatalf("NewObject(%q) reused an instance", kind)
		}
	}

	for kind := range KindRegistry {
		if !seen[kind] {
			t.Fatalf("RegisteredKinds() omitted %q", kind)
		}
	}
}

func TestKindRegistryMetadataAccessorsExposeEmbeddedMetadata(t *testing.T) {
	for kind, construct := range KindRegistry {
		t.Run(kind, func(t *testing.T) {
			object := construct()
			value := reflect.ValueOf(object).Elem()
			value.FieldByName("TypeMeta").Set(reflect.ValueOf(TypeMeta{APIVersion: APIVersion, Kind: kind}))
			value.FieldByName("ObjectMeta").Set(reflect.ValueOf(ObjectMeta{Name: "registered", Namespace: "testing"}))

			if got := object.GetTypeMeta(); got.APIVersion != APIVersion || got.Kind != kind {
				t.Fatalf("GetTypeMeta() = %+v, want apiVersion %q and kind %q", got, APIVersion, kind)
			}
			metadata := object.GetObjectMeta()
			if metadata.Name != "registered" || metadata.Namespace != "testing" {
				t.Fatalf("GetObjectMeta() = %+v, want seeded metadata", metadata)
			}
			metadata.Name = "mutated"
			if got := value.FieldByName("ObjectMeta").Interface().(ObjectMeta).Name; got != "mutated" {
				t.Fatalf("GetObjectMeta() returned detached metadata; embedded name = %q", got)
			}
		})
	}
}

func TestNewObjectRejectsUnknownKind(t *testing.T) {
	object, err := NewObject("Unknown")
	if object != nil || err == nil || !strings.Contains(err.Error(), "unknown kind: Unknown") {
		t.Fatalf("NewObject(Unknown) = (%T, %v), want explicit unknown-kind error", object, err)
	}
}
