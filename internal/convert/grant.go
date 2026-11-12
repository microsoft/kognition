// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func CrossNamespaceGrantToK8s(in *api.CrossNamespaceGrant) *k8s.CrossNamespaceGrant {
	if in == nil {
		return nil
	}
	out := &k8s.CrossNamespaceGrant{
		TypeMeta:   metav1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec:       k8s.CrossNamespaceGrantSpec{From: in.Spec.From, To: in.Spec.To},
	}
	if in.Spec.Resources != nil {
		out.Spec.Resources = make([]k8s.GrantResource, len(in.Spec.Resources))
		for i, resource := range in.Spec.Resources {
			out.Spec.Resources[i] = k8s.GrantResource{Kind: resource.Kind, Name: resource.Name}
		}
	}
	return out
}

func CrossNamespaceGrantFromK8s(in *k8s.CrossNamespaceGrant) *api.CrossNamespaceGrant {
	if in == nil {
		return nil
	}
	out := &api.CrossNamespaceGrant{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec:       api.CrossNamespaceGrantSpec{From: in.Spec.From, To: in.Spec.To},
	}
	if in.Spec.Resources != nil {
		out.Spec.Resources = make([]api.GrantResource, len(in.Spec.Resources))
		for i, resource := range in.Spec.Resources {
			out.Spec.Resources[i] = api.GrantResource{Kind: resource.Kind, Name: resource.Name}
		}
	}
	return out
}
