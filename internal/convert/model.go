// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- ModelClass ---

func ModelClassToK8s(in *api.ModelClass) *k8s.ModelClass {
	out := &k8s.ModelClass{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ModelClass",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.ModelClassSpec{
			Provisioner:   in.Spec.Provisioner,
			Parameters:    in.Spec.Parameters,
			LatencyClass:  in.Spec.LatencyClass,
			CostTier:      in.Spec.CostTier,
			ContextWindow: in.Spec.ContextWindow,
			Modalities:    in.Spec.Modalities,
			VersionPin:    in.Spec.VersionPin,
		},
		Status: k8s.ModelClassStatus{
			Phase:                phaseToString(in.Status.Phase),
			Conditions:           conditionsToK8s(in.Status.Conditions),
			Provisioned:          in.Status.Provisioned,
			ActiveVersion:        in.Status.ActiveVersion,
			Health:               in.Status.Health,
			LastProbeTime:        in.Status.LastProbeTime,
			ConsecutiveFailures:  in.Status.ConsecutiveFailures,
			ConsecutiveSuccesses: in.Status.ConsecutiveSuccesses,
			FallbackClass:        in.Status.FallbackClass,
		},
	}
	if in.Spec.VersionCandidates != nil {
		out.Spec.VersionCandidates = make(map[string]k8s.ModelVersionBinding, len(in.Spec.VersionCandidates))
		for version, binding := range in.Spec.VersionCandidates {
			out.Spec.VersionCandidates[version] = k8s.ModelVersionBinding(binding)
		}
	}
	if in.Status.ActiveBinding != nil {
		binding := k8s.ModelVersionBinding(*in.Status.ActiveBinding)
		out.Status.ActiveBinding = &binding
	}
	if in.Spec.HealthProbe != nil {
		out.Spec.HealthProbe = &k8s.ModelHealthProbeSpec{
			Endpoint:           in.Spec.HealthProbe.Endpoint,
			Type:               in.Spec.HealthProbe.Type,
			IntervalSeconds:    in.Spec.HealthProbe.IntervalSeconds,
			TimeoutSeconds:     in.Spec.HealthProbe.TimeoutSeconds,
			LatencyThresholdMs: in.Spec.HealthProbe.LatencyThresholdMs,
			ErrorRateThreshold: clonePointer(in.Spec.HealthProbe.ErrorRateThreshold),
			FailureThreshold:   in.Spec.HealthProbe.FailureThreshold,
			SuccessThreshold:   in.Spec.HealthProbe.SuccessThreshold,
		}
	}
	return out
}

func ModelClassFromK8s(in *k8s.ModelClass) *api.ModelClass {
	out := &api.ModelClass{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ModelClassSpec{
			Provisioner:   in.Spec.Provisioner,
			Parameters:    in.Spec.Parameters,
			LatencyClass:  in.Spec.LatencyClass,
			CostTier:      in.Spec.CostTier,
			ContextWindow: in.Spec.ContextWindow,
			Modalities:    in.Spec.Modalities,
			VersionPin:    in.Spec.VersionPin,
		},
		Status: api.ModelClassStatus{
			Phase:                phaseFromString(in.Status.Phase),
			Conditions:           conditionsFromK8s(in.Status.Conditions),
			Provisioned:          in.Status.Provisioned,
			ActiveVersion:        in.Status.ActiveVersion,
			Health:               in.Status.Health,
			LastProbeTime:        in.Status.LastProbeTime,
			ConsecutiveFailures:  in.Status.ConsecutiveFailures,
			ConsecutiveSuccesses: in.Status.ConsecutiveSuccesses,
			FallbackClass:        in.Status.FallbackClass,
		},
	}
	if in.Spec.VersionCandidates != nil {
		out.Spec.VersionCandidates = make(map[string]api.ModelVersionBinding, len(in.Spec.VersionCandidates))
		for version, binding := range in.Spec.VersionCandidates {
			out.Spec.VersionCandidates[version] = api.ModelVersionBinding(binding)
		}
	}
	if in.Status.ActiveBinding != nil {
		binding := api.ModelVersionBinding(*in.Status.ActiveBinding)
		out.Status.ActiveBinding = &binding
	}
	if in.Spec.HealthProbe != nil {
		out.Spec.HealthProbe = &api.HealthProbeSpec{
			Endpoint:           in.Spec.HealthProbe.Endpoint,
			Type:               in.Spec.HealthProbe.Type,
			IntervalSeconds:    in.Spec.HealthProbe.IntervalSeconds,
			TimeoutSeconds:     in.Spec.HealthProbe.TimeoutSeconds,
			LatencyThresholdMs: in.Spec.HealthProbe.LatencyThresholdMs,
			ErrorRateThreshold: clonePointer(in.Spec.HealthProbe.ErrorRateThreshold),
			FailureThreshold:   in.Spec.HealthProbe.FailureThreshold,
			SuccessThreshold:   in.Spec.HealthProbe.SuccessThreshold,
		}
	}
	return out
}

// --- Model ---

func ModelToK8s(in *api.Model) *k8s.Model {
	out := &k8s.Model{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Model",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.ModelSpec{
			Provider:           in.Spec.Provider,
			ClassName:          in.Spec.ClassName,
			ModelID:            in.Spec.ModelID,
			Endpoint:           in.Spec.Endpoint,
			Capabilities:       in.Spec.Capabilities,
			RateLimit:          in.Spec.RateLimit,
			Parameters:         in.Spec.Parameters,
			Signature:          in.Spec.Signature,
			InvocationContract: in.Spec.InvocationContract,
		},
		Status: k8s.ModelStatus{
			Phase:        phaseToString(in.Status.Phase),
			Conditions:   conditionsToK8s(in.Status.Conditions),
			Available:    in.Status.Available,
			Latency:      in.Status.Latency,
			LastCheck:    in.Status.LastCheck,
			Capabilities: in.Status.Capabilities,
		},
	}
	if in.Spec.CredentialRef != nil {
		ref := localRefToK8s(in.Spec.CredentialRef.Name)
		out.Spec.CredentialRef = &ref
	}
	if in.Spec.InvocationProfileRef != nil {
		out.Spec.InvocationProfileRef = &k8s.ModelInvocationProfileReference{Name: in.Spec.InvocationProfileRef.Name, SpecDigest: in.Spec.InvocationProfileRef.SpecDigest}
	}
	return out
}

func ModelFromK8s(in *k8s.Model) *api.Model {
	out := &api.Model{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ModelSpec{
			Provider:           in.Spec.Provider,
			ClassName:          in.Spec.ClassName,
			ModelID:            in.Spec.ModelID,
			Endpoint:           in.Spec.Endpoint,
			Capabilities:       in.Spec.Capabilities,
			RateLimit:          in.Spec.RateLimit,
			Parameters:         in.Spec.Parameters,
			Signature:          in.Spec.Signature,
			InvocationContract: in.Spec.InvocationContract,
		},
		Status: api.ModelStatus{
			Phase:        phaseFromString(in.Status.Phase),
			Conditions:   conditionsFromK8s(in.Status.Conditions),
			Available:    in.Status.Available,
			Latency:      in.Status.Latency,
			LastCheck:    in.Status.LastCheck,
			Capabilities: in.Status.Capabilities,
		},
	}
	if in.Spec.CredentialRef != nil {
		out.Spec.CredentialRef = &api.LocalObjectReference{Name: localRefFromK8s(*in.Spec.CredentialRef)}
	}
	if in.Spec.InvocationProfileRef != nil {
		out.Spec.InvocationProfileRef = &api.ModelInvocationProfileReference{Name: in.Spec.InvocationProfileRef.Name, SpecDigest: in.Spec.InvocationProfileRef.SpecDigest}
	}
	return out
}

// --- ModelGrant ---

func ModelGrantToK8s(in *api.ModelGrant) *k8s.ModelGrant {
	if in == nil {
		return nil
	}
	return &k8s.ModelGrant{
		TypeMeta:   metav1.TypeMeta{Kind: "ModelGrant", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.ModelGrantSpec{
			AgentRef: localRefToK8s(in.Spec.AgentRef.Name),
			ModelRef: localRefToK8s(in.Spec.ModelRef.Name),
		},
	}
}

func ModelGrantFromK8s(in *k8s.ModelGrant) *api.ModelGrant {
	if in == nil {
		return nil
	}
	return &api.ModelGrant{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ModelGrantSpec{
			AgentRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)},
			ModelRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.ModelRef)},
		},
	}
}

// --- SecretRef ---

func SecretRefToK8s(in *api.SecretRef) *k8s.SecretRef {
	out := &k8s.SecretRef{
		TypeMeta: metav1.TypeMeta{
			Kind:       "SecretRef",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.SecretRefSpec{
			BackendType: in.Spec.BackendType,
			BackendRef:  in.Spec.BackendRef,
			Key:         in.Spec.Key,
			Provider:    in.Spec.Provider,
			Path:        in.Spec.Path,
			SecretKey:   in.Spec.LegacyKey,
		},
		Status: k8s.SecretRefStatus{
			Phase:      string(in.Status.Phase),
			Message:    in.Status.Message,
			Synced:     in.Status.Synced,
			Conditions: conditionsToK8s(in.Status.Conditions),
		},
	}
	if in.Status.LastSyncTime != nil {
		out.Status.LastSyncTime = in.Status.LastSyncTime.UTC().Format(time.RFC3339)
	}
	return out
}

func SecretRefFromK8s(in *k8s.SecretRef) *api.SecretRef {
	out := &api.SecretRef{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.SecretRefSpec{
			BackendType: in.Spec.BackendType,
			BackendRef:  in.Spec.BackendRef,
			Key:         in.Spec.Key,
			Provider:    in.Spec.Provider,
			Path:        in.Spec.Path,
			LegacyKey:   in.Spec.SecretKey,
		},
		Status: api.SecretRefStatus{
			Phase:      api.Phase(in.Status.Phase),
			Message:    in.Status.Message,
			Synced:     in.Status.Synced,
			Conditions: conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Status.LastSyncTime != "" {
		if t, err := time.Parse(time.RFC3339, in.Status.LastSyncTime); err == nil {
			out.Status.LastSyncTime = &t
		}
	}
	return out
}
