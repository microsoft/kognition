// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ModelInvocationProfileToK8s(in *api.ModelInvocationProfile) *k8s.ModelInvocationProfile {
	out := &k8s.ModelInvocationProfile{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "ModelInvocationProfile"},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
	}
	in.Spec.DeepCopyInto(&out.Spec)
	return out
}

func ModelInvocationProfileFromK8s(in *k8s.ModelInvocationProfile) *api.ModelInvocationProfile {
	out := &api.ModelInvocationProfile{
		TypeMeta:   api.TypeMeta{APIVersion: in.APIVersion, Kind: in.Kind},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
	}
	in.Spec.DeepCopyInto(&out.Spec)
	return out
}
