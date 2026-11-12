// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

import "testing"

func TestHasFinalizer(t *testing.T) {
	finalizers := []string{"cleanup.kognition.dev", "other.kognition.dev"}
	if !HasFinalizer(finalizers, "cleanup.kognition.dev") {
		t.Fatal("want true")
	}
	if HasFinalizer(finalizers, "missing") {
		t.Fatal("want false")
	}
}

func TestAddFinalizer(t *testing.T) {
	var finalizers []string
	finalizers, added := AddFinalizer(finalizers, "cleanup.kognition.dev")
	if !added || len(finalizers) != 1 {
		t.Fatalf("want 1, got %d", len(finalizers))
	}

	// Adding duplicate should not add again
	finalizers, added = AddFinalizer(finalizers, "cleanup.kognition.dev")
	if added || len(finalizers) != 1 {
		t.Fatalf("want 1 after duplicate, got %d", len(finalizers))
	}
}

func TestRemoveFinalizer(t *testing.T) {
	finalizers := []string{"a", "b", "c"}
	finalizers, removed := RemoveFinalizer(finalizers, "b")
	if !removed || len(finalizers) != 2 {
		t.Fatalf("want 2, got %d", len(finalizers))
	}
	if HasFinalizer(finalizers, "b") {
		t.Fatal("b should be removed")
	}

	// Removing non-existent is a no-op
	finalizers, removed = RemoveFinalizer(finalizers, "x")
	if removed || len(finalizers) != 2 {
		t.Fatalf("want 2 after no-op remove, got %d", len(finalizers))
	}
}
