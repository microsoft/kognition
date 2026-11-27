// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

func (in *ExternalAgentCard) DeepCopyInto(out *ExternalAgentCard) {
	*out = *in
	if in.Skills != nil {
		out.Skills = make([]string, len(in.Skills))
		copy(out.Skills, in.Skills)
	}
}

func (in *ExternalAgentCard) DeepCopy() *ExternalAgentCard {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentCard)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentLimits) DeepCopyInto(out *ExternalAgentLimits) {
	*out = *in
}

func (in *ExternalAgentLimits) DeepCopy() *ExternalAgentLimits {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentLimits)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentSpec) DeepCopyInto(out *ExternalAgentSpec) {
	*out = *in
	in.Card.DeepCopyInto(&out.Card)
	out.CredentialRef = in.CredentialRef
	if in.Limits != nil {
		out.Limits = in.Limits.DeepCopy()
	}
}

func (in *ExternalAgentSpec) DeepCopy() *ExternalAgentSpec {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentStatus) DeepCopyInto(out *ExternalAgentStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ExternalAgentStatus) DeepCopy() *ExternalAgentStatus {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgent) DeepCopyInto(out *ExternalAgent) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ExternalAgent) DeepCopy() *ExternalAgent {
	if in == nil {
		return nil
	}
	out := new(ExternalAgent)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgent) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ExternalAgentList) DeepCopyInto(out *ExternalAgentList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ExternalAgent, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ExternalAgentList) DeepCopy() *ExternalAgentList {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentList)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ExternalAgentGrantConditions) DeepCopyInto(out *ExternalAgentGrantConditions) {
	*out = *in
}

func (in *ExternalAgentGrantConditions) DeepCopy() *ExternalAgentGrantConditions {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentGrantConditions)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentGrantSpec) DeepCopyInto(out *ExternalAgentGrantSpec) {
	*out = *in
	out.ExternalAgentRef = in.ExternalAgentRef
	if in.WorkflowRef != nil {
		ref := *in.WorkflowRef
		out.WorkflowRef = &ref
	}
	if in.WorkflowSelector != nil {
		out.WorkflowSelector = make(map[string]string, len(in.WorkflowSelector))
		for k, v := range in.WorkflowSelector {
			out.WorkflowSelector[k] = v
		}
	}
	if in.Verbs != nil {
		out.Verbs = make([]ExternalAgentGrantVerb, len(in.Verbs))
		copy(out.Verbs, in.Verbs)
	}
	if in.Conditions != nil {
		out.Conditions = in.Conditions.DeepCopy()
	}
}

func (in *ExternalAgentGrantSpec) DeepCopy() *ExternalAgentGrantSpec {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentGrantSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentGrant) DeepCopyInto(out *ExternalAgentGrant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *ExternalAgentGrant) DeepCopy() *ExternalAgentGrant {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentGrant)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentGrant) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ExternalAgentGrantList) DeepCopyInto(out *ExternalAgentGrantList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ExternalAgentGrant, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ExternalAgentGrantList) DeepCopy() *ExternalAgentGrantList {
	if in == nil {
		return nil
	}
	out := new(ExternalAgentGrantList)
	in.DeepCopyInto(out)
	return out
}

func (in *ExternalAgentGrantList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
