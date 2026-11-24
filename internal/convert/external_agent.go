// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// ExternalAgentToK8s converts an internal ExternalAgent to a K8s ExternalAgent.
func ExternalAgentToK8s(in *api.ExternalAgent) *k8s.ExternalAgent {
	if in == nil {
		return nil
	}
	out := &k8s.ExternalAgent{
		Spec: k8s.ExternalAgentSpec{
			Protocol: k8s.ExternalAgentProtocol(in.Spec.Protocol),
			Endpoint: in.Spec.Endpoint,
			Card: k8s.ExternalAgentCard{
				Digest: in.Spec.Card.Digest,
				Skills: append([]string(nil), in.Spec.Card.Skills...),
			},
			CredentialRef: localRefToK8s(in.Spec.CredentialRef.Name),
		},
		Status: k8s.ExternalAgentStatus{
			ObservedCardDigest: in.Status.ObservedCardDigest,
			ObservedGeneration: in.Status.ObservedGeneration,
			Conditions:         conditionsToK8s(in.Status.Conditions),
		},
	}
	out.TypeMeta = metav1.TypeMeta{Kind: "ExternalAgent", APIVersion: api.APIVersion}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	if in.Spec.Limits != nil {
		out.Spec.Limits = &k8s.ExternalAgentLimits{
			MaxTokensPerTask:   in.Spec.Limits.MaxTokensPerTask,
			TaskTimeout:        durationToString(in.Spec.Limits.TaskTimeout),
			MaxRequestBytes:    in.Spec.Limits.MaxRequestBytes,
			MaxResponseBytes:   in.Spec.Limits.MaxResponseBytes,
			MaxConcurrentTasks: in.Spec.Limits.MaxConcurrentTasks,
		}
	}
	return out
}

// ExternalAgentFromK8s converts a K8s ExternalAgent to an internal ExternalAgent.
func ExternalAgentFromK8s(in *k8s.ExternalAgent) *api.ExternalAgent {
	if in == nil {
		return nil
	}
	out := &api.ExternalAgent{
		TypeMeta:   api.TypeMeta{Kind: "ExternalAgent", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ExternalAgentSpec{
			Protocol: api.ExternalAgentProtocol(in.Spec.Protocol),
			Endpoint: in.Spec.Endpoint,
			Card: api.ExternalAgentCard{
				Digest: in.Spec.Card.Digest,
				Skills: append([]string(nil), in.Spec.Card.Skills...),
			},
			CredentialRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.CredentialRef)},
		},
		Status: api.ExternalAgentStatus{
			ObservedCardDigest: in.Status.ObservedCardDigest,
			ObservedGeneration: in.Status.ObservedGeneration,
			Conditions:         conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Spec.Limits != nil {
		out.Spec.Limits = &api.ExternalAgentLimits{
			MaxTokensPerTask:   in.Spec.Limits.MaxTokensPerTask,
			TaskTimeout:        durationFromString(strings.TrimSpace(in.Spec.Limits.TaskTimeout)),
			MaxRequestBytes:    in.Spec.Limits.MaxRequestBytes,
			MaxResponseBytes:   in.Spec.Limits.MaxResponseBytes,
			MaxConcurrentTasks: in.Spec.Limits.MaxConcurrentTasks,
		}
	}
	return out
}

// ExternalAgentGrantToK8s converts an internal ExternalAgentGrant to a K8s ExternalAgentGrant.
func ExternalAgentGrantToK8s(in *api.ExternalAgentGrant) *k8s.ExternalAgentGrant {
	if in == nil {
		return nil
	}
	out := &k8s.ExternalAgentGrant{
		Spec: k8s.ExternalAgentGrantSpec{
			ExternalAgentRef: localRefToK8s(in.Spec.ExternalAgentRef.Name),
			WorkflowSelector: cloneStringMap(in.Spec.WorkflowSelector),
		},
	}
	out.TypeMeta = metav1.TypeMeta{Kind: "ExternalAgentGrant", APIVersion: api.APIVersion}
	out.ObjectMeta = objectMetaToK8s(in.ObjectMeta)
	if in.Spec.WorkflowRef.Name != "" {
		ref := localRefToK8s(in.Spec.WorkflowRef.Name)
		out.Spec.WorkflowRef = &ref
	}
	if in.Spec.Verbs != nil {
		out.Spec.Verbs = make([]k8s.ExternalAgentGrantVerb, len(in.Spec.Verbs))
		for i, verb := range in.Spec.Verbs {
			out.Spec.Verbs[i] = k8s.ExternalAgentGrantVerb(verb)
		}
	}
	if in.Spec.Conditions != nil {
		out.Spec.Conditions = &k8s.ExternalAgentGrantConditions{
			ValidFrom:  in.Spec.Conditions.ValidFrom,
			ValidUntil: in.Spec.Conditions.ValidUntil,
		}
	}
	return out
}

// ExternalAgentGrantFromK8s converts a K8s ExternalAgentGrant to an internal ExternalAgentGrant.
func ExternalAgentGrantFromK8s(in *k8s.ExternalAgentGrant) *api.ExternalAgentGrant {
	if in == nil {
		return nil
	}
	out := &api.ExternalAgentGrant{
		TypeMeta:   api.TypeMeta{Kind: "ExternalAgentGrant", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ExternalAgentGrantSpec{
			ExternalAgentRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.ExternalAgentRef)},
			WorkflowSelector: cloneStringMap(in.Spec.WorkflowSelector),
		},
	}
	if in.Spec.WorkflowRef != nil {
		out.Spec.WorkflowRef = api.LocalObjectReference{Name: localRefFromK8s(*in.Spec.WorkflowRef)}
	}
	if in.Spec.Verbs != nil {
		out.Spec.Verbs = make([]api.ExternalAgentGrantVerb, len(in.Spec.Verbs))
		for i, verb := range in.Spec.Verbs {
			out.Spec.Verbs[i] = api.ExternalAgentGrantVerb(verb)
		}
	}
	if in.Spec.Conditions != nil {
		out.Spec.Conditions = &api.ExternalAgentGrantConditions{
			ValidFrom:  in.Spec.Conditions.ValidFrom,
			ValidUntil: in.Spec.Conditions.ValidUntil,
		}
	}
	return out
}
