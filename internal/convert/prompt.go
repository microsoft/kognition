// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func PromptToK8s(in *api.Prompt) *k8s.Prompt {
	if in == nil {
		return nil
	}
	out := &k8s.Prompt{
		TypeMeta:   metav1.TypeMeta{Kind: "Prompt", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.PromptSpec{
			Content:      in.Spec.Content,
			Instructions: in.Spec.Instructions,
			Metadata:     cloneStringsMap(in.Spec.Metadata),
		},
		Status: k8s.PromptStatus{Hash: in.Status.Hash, Version: in.Status.Version},
	}
	if in.Spec.Parameters != nil {
		out.Spec.Parameters = make([]k8s.PromptParameter, len(in.Spec.Parameters))
		for i := range in.Spec.Parameters {
			out.Spec.Parameters[i] = k8s.PromptParameter{
				Name: in.Spec.Parameters[i].Name, Description: in.Spec.Parameters[i].Description,
				Required: in.Spec.Parameters[i].Required, Default: in.Spec.Parameters[i].Default,
			}
		}
	}
	return out
}

func PromptFromK8s(in *k8s.Prompt) *api.Prompt {
	if in == nil {
		return nil
	}
	out := &api.Prompt{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.PromptSpec{
			Content:      in.Spec.Content,
			Instructions: in.Spec.Instructions,
			Metadata:     cloneStringsMap(in.Spec.Metadata),
		},
		Status: api.PromptStatus{Hash: in.Status.Hash, Version: in.Status.Version},
	}
	if in.Spec.Parameters != nil {
		out.Spec.Parameters = make([]api.PromptParameter, len(in.Spec.Parameters))
		for i := range in.Spec.Parameters {
			out.Spec.Parameters[i] = api.PromptParameter{
				Name: in.Spec.Parameters[i].Name, Description: in.Spec.Parameters[i].Description,
				Required: in.Spec.Parameters[i].Required, Default: in.Spec.Parameters[i].Default,
			}
		}
	}
	return out
}
