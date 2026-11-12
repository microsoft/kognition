// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package lifecycle

// HasFinalizer returns true if the finalizer is present in the slice.
func HasFinalizer(finalizers []string, finalizer string) bool {
	for _, f := range finalizers {
		if f == finalizer {
			return true
		}
	}
	return false
}

// AddFinalizer adds a finalizer to the slice if not already present.
// Returns the updated slice and whether it was added.
func AddFinalizer(finalizers []string, finalizer string) ([]string, bool) {
	if HasFinalizer(finalizers, finalizer) {
		return finalizers, false
	}
	return append(finalizers, finalizer), true
}

// RemoveFinalizer removes a finalizer from the slice.
// Returns the updated slice and whether it was removed.
func RemoveFinalizer(finalizers []string, finalizer string) ([]string, bool) {
	result := make([]string, 0, len(finalizers))
	found := false
	for _, f := range finalizers {
		if f == finalizer {
			found = true
			continue
		}
		result = append(result, f)
	}
	return result, found
}
