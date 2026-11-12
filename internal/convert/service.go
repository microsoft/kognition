// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func cloneStringsMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func AgentServiceToK8s(in *api.AgentService) *k8s.AgentService {
	if in == nil {
		return nil
	}
	out := &k8s.AgentService{
		TypeMeta: metav1.TypeMeta{Kind: "AgentService", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec:   k8s.AgentServiceSpec{AgentRef: localRefToK8s(in.Spec.AgentRef.Name), LoadBalancingStrategy: in.Spec.LoadBalancingStrategy, Port: in.Spec.Port},
		Status: k8s.AgentServiceStatus{Phase: string(in.Status.Phase), Endpoint: in.Status.Endpoint, ActiveRevision: in.Status.ActiveRevision, ReadyInstances: in.Status.ReadyInstances, Conditions: conditionsToK8s(in.Status.Conditions)},
	}
	if in.Spec.RevisionPin != nil {
		out.Spec.RevisionPin = new(int64)
		*out.Spec.RevisionPin = *in.Spec.RevisionPin
	}
	out.Spec.Selector = cloneStringsMap(in.Spec.Selector)
	return out
}

func AgentServiceFromK8s(in *k8s.AgentService) *api.AgentService {
	if in == nil {
		return nil
	}
	out := &api.AgentService{
		TypeMeta: api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion}, ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec:   api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: in.Spec.AgentRef.Name}, LoadBalancingStrategy: in.Spec.LoadBalancingStrategy, Port: in.Spec.Port},
		Status: api.AgentServiceStatus{Phase: api.Phase(in.Status.Phase), Endpoint: in.Status.Endpoint, ActiveRevision: in.Status.ActiveRevision, ReadyInstances: in.Status.ReadyInstances, Conditions: conditionsFromK8s(in.Status.Conditions)},
	}
	if in.Spec.RevisionPin != nil {
		out.Spec.RevisionPin = new(int64)
		*out.Spec.RevisionPin = *in.Spec.RevisionPin
	}
	out.Spec.Selector = cloneStringsMap(in.Spec.Selector)
	return out
}

func IntentGatewayToK8s(in *api.IntentGateway) *k8s.IntentGateway {
	if in == nil {
		return nil
	}
	out := &k8s.IntentGateway{TypeMeta: metav1.TypeMeta{Kind: "IntentGateway", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: objectMetaToK8s(in.ObjectMeta), Status: k8s.IntentGatewayStatus{Phase: string(in.Status.Phase), Endpoint: in.Status.Endpoint, Conditions: conditionsToK8s(in.Status.Conditions)}}
	out.Spec.Protocol = in.Spec.Protocol
	if in.Spec.AuthConfig != nil {
		out.Spec.AuthConfig = &k8s.IntentGatewayAuthConfig{Mode: in.Spec.AuthConfig.Mode, Header: in.Spec.AuthConfig.Header}
		if in.Spec.AuthConfig.APIKeySecretRef != nil {
			out.Spec.AuthConfig.APIKeySecretRef = &k8s.LocalObjectReference{Name: in.Spec.AuthConfig.APIKeySecretRef.Name}
		}
	}
	out.Spec.Routes = make([]k8s.IntentRoute, len(in.Spec.Routes))
	for i := range in.Spec.Routes {
		out.Spec.Routes[i] = k8s.IntentRoute{Pattern: in.Spec.Routes[i].Pattern, TargetRef: localRefToK8s(in.Spec.Routes[i].TargetRef.Name), TargetKind: in.Spec.Routes[i].TargetKind}
	}
	return out
}

func IntentGatewayFromK8s(in *k8s.IntentGateway) *api.IntentGateway {
	if in == nil {
		return nil
	}
	out := &api.IntentGateway{TypeMeta: api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion}, ObjectMeta: objectMetaFromK8s(in.ObjectMeta), Status: api.IntentGatewayStatus{Phase: api.Phase(in.Status.Phase), Endpoint: in.Status.Endpoint, Conditions: conditionsFromK8s(in.Status.Conditions)}}
	out.Spec.Protocol = in.Spec.Protocol
	if in.Spec.AuthConfig != nil {
		out.Spec.AuthConfig = &api.IntentGatewayAuthConfig{Mode: in.Spec.AuthConfig.Mode, Header: in.Spec.AuthConfig.Header}
		if in.Spec.AuthConfig.APIKeySecretRef != nil {
			out.Spec.AuthConfig.APIKeySecretRef = &api.LocalObjectReference{Name: in.Spec.AuthConfig.APIKeySecretRef.Name}
		}
	}
	out.Spec.Routes = make([]api.IntentRoute, len(in.Spec.Routes))
	for i := range in.Spec.Routes {
		out.Spec.Routes[i] = api.IntentRoute{Pattern: in.Spec.Routes[i].Pattern, TargetRef: api.LocalObjectReference{Name: in.Spec.Routes[i].TargetRef.Name}, TargetKind: in.Spec.Routes[i].TargetKind}
	}
	return out
}
