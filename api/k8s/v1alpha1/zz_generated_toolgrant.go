// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import runtime "k8s.io/apimachinery/pkg/runtime"

func (in *ToolGrant) DeepCopyInto(out *ToolGrant) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *ToolGrant) DeepCopy() *ToolGrant {
	if in == nil {
		return nil
	}
	out := new(ToolGrant)
	in.DeepCopyInto(out)
	return out
}

func (in *ToolGrant) DeepCopyObject() runtime.Object {
	if copied := in.DeepCopy(); copied != nil {
		return copied
	}
	return nil
}

func (in *ToolGrantList) DeepCopyInto(out *ToolGrantList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ToolGrant, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ToolGrantList) DeepCopy() *ToolGrantList {
	if in == nil {
		return nil
	}
	out := new(ToolGrantList)
	in.DeepCopyInto(out)
	return out
}

func (in *ToolGrantList) DeepCopyObject() runtime.Object {
	if copied := in.DeepCopy(); copied != nil {
		return copied
	}
	return nil
}

func (in *ToolGrantSpec) DeepCopyInto(out *ToolGrantSpec) {
	*out = *in
	if in.AgentSelector != nil {
		out.AgentSelector = make(map[string]string, len(in.AgentSelector))
		for key, value := range in.AgentSelector {
			out.AgentSelector[key] = value
		}
	}
	if in.Verbs != nil {
		out.Verbs = append([]ToolGrantVerb(nil), in.Verbs...)
	}
	if in.ArgumentConstraints != nil {
		out.ArgumentConstraints = runtime.DeepCopyJSON(in.ArgumentConstraints)
	}
	if in.RateLimits != nil {
		out.RateLimits = new(ToolGrantRateLimits)
		*out.RateLimits = *in.RateLimits
	}
	if in.Conditions != nil {
		out.Conditions = new(ToolGrantConditions)
		in.Conditions.DeepCopyInto(out.Conditions)
	}
}

func (in *ToolGrantConditions) DeepCopyInto(out *ToolGrantConditions) {
	*out = *in
	if in.RequiredAgentLabels != nil {
		out.RequiredAgentLabels = make(map[string]string, len(in.RequiredAgentLabels))
		for key, value := range in.RequiredAgentLabels {
			out.RequiredAgentLabels[key] = value
		}
	}
}
