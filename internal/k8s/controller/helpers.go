// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// isAlreadyExists returns true if the error indicates the resource already exists.
func isAlreadyExists(err error) bool {
	return apierrors.IsAlreadyExists(err)
}

// isConflict returns true if the error indicates a resource version conflict.
func isConflict(err error) bool {
	return apierrors.IsConflict(err)
}
