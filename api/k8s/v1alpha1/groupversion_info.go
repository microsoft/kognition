// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package v1alpha1 contains the Kubernetes-native CRD types for kognition.dev/v1alpha1.
//
// +kubebuilder:object:generate=true
// +groupName=kognition.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version for this package.
	GroupVersion = schema.GroupVersion{Group: "kognition.dev", Version: "v1alpha1"}

	// SchemeBuilder is used to add Go types to the GroupVersionResource scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion} //nolint:staticcheck // Retain controller-runtime API registration compatibility.

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
