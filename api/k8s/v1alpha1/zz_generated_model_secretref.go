// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Hand-written deepcopy methods for Model and SecretRef types.
// controller-gen v0.21.0 panics on this package; these are equivalent to
// what controller-gen would produce. Keep in sync with the *Spec/*Status
// structs in model_types.go and secretref_types.go.

package v1alpha1

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *Model) DeepCopyInto(out *Model) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy.
func (in *Model) DeepCopy() *Model {
	if in == nil {
		return nil
	}
	out := new(Model)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *Model) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelSpec) DeepCopyInto(out *ModelSpec) {
	*out = *in
	if in.Capabilities != nil {
		out.Capabilities = make([]string, len(in.Capabilities))
		copy(out.Capabilities, in.Capabilities)
	}
	if in.CredentialRef != nil {
		out.CredentialRef = new(LocalObjectReference)
		*out.CredentialRef = *in.CredentialRef
	}
	if in.InvocationProfileRef != nil {
		out.InvocationProfileRef = new(ModelInvocationProfileReference)
		*out.InvocationProfileRef = *in.InvocationProfileRef
	}
	if in.RateLimit != nil {
		out.RateLimit = new(int64)
		*out.RateLimit = *in.RateLimit
	}
	if in.Parameters != nil {
		out.Parameters = make(map[string]string, len(in.Parameters))
		for k, v := range in.Parameters {
			out.Parameters[k] = v
		}
	}
}

// DeepCopy returns a deep copy.
func (in *ModelSpec) DeepCopy() *ModelSpec {
	if in == nil {
		return nil
	}
	out := new(ModelSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelStatus) DeepCopyInto(out *ModelStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
	if in.Capabilities != nil {
		out.Capabilities = make([]string, len(in.Capabilities))
		copy(out.Capabilities, in.Capabilities)
	}
}

// DeepCopy returns a deep copy.
func (in *ModelStatus) DeepCopy() *ModelStatus {
	if in == nil {
		return nil
	}
	out := new(ModelStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *ModelList) DeepCopyInto(out *ModelList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Model, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *ModelList) DeepCopy() *ModelList {
	if in == nil {
		return nil
	}
	out := new(ModelList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *ModelList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *SecretRef) DeepCopyInto(out *SecretRef) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy.
func (in *SecretRef) DeepCopy() *SecretRef {
	if in == nil {
		return nil
	}
	out := new(SecretRef)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *SecretRef) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *SecretRefSpec) DeepCopyInto(out *SecretRefSpec) {
	*out = *in
}

// DeepCopy returns a deep copy.
func (in *SecretRefSpec) DeepCopy() *SecretRefSpec {
	if in == nil {
		return nil
	}
	out := new(SecretRefSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *SecretRefStatus) DeepCopyInto(out *SecretRefStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *SecretRefStatus) DeepCopy() *SecretRefStatus {
	if in == nil {
		return nil
	}
	out := new(SecretRefStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out. in must be non-nil.
func (in *SecretRefList) DeepCopyInto(out *SecretRefList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SecretRef, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy.
func (in *SecretRefList) DeepCopy() *SecretRefList {
	if in == nil {
		return nil
	}
	out := new(SecretRefList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a deep copy as runtime.Object.
func (in *SecretRefList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
