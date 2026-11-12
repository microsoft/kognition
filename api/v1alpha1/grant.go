// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// CrossNamespaceGrant allows a resource in a source namespace to be
// referenced by resources in a target namespace. Without a matching grant,
// cross-namespace references are denied by admission control.
type CrossNamespaceGrant struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       CrossNamespaceGrantSpec `json:"spec" yaml:"spec"`
}

// CrossNamespaceGrantSpec defines which resources are shared and with whom.
type CrossNamespaceGrantSpec struct {
	// From is the namespace that owns the resource being shared.
	From string `json:"from" yaml:"from"`

	// To is the namespace allowed to reference the shared resource.
	// Use "*" to grant access to all namespaces.
	To string `json:"to" yaml:"to"`

	// Resources specifies which resources are shared.
	Resources []GrantResource `json:"resources" yaml:"resources"`
}

// GrantResource identifies a specific resource being shared.
type GrantResource struct {
	// Kind is the resource kind (e.g. "Tool", "Memory", "Model").
	Kind string `json:"kind" yaml:"kind"`

	// Name is the resource name. Use "*" to grant access to all resources of this kind.
	Name string `json:"name" yaml:"name"`
}

func (g *CrossNamespaceGrant) GetTypeMeta() TypeMeta      { return g.TypeMeta }
func (g *CrossNamespaceGrant) GetObjectMeta() *ObjectMeta { return &g.ObjectMeta }

// ResourceReference represents a reference to another resource, possibly in
// a different namespace.
type ResourceReference struct {
	// Kind is the resource kind being referenced.
	Kind string `json:"kind" yaml:"kind"`

	// Name is the resource name.
	Name string `json:"name" yaml:"name"`

	// Namespace is the namespace of the referenced resource.
	// If empty, defaults to the same namespace as the referencing resource.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}
