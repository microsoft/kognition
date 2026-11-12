// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- Workspace ---

func WorkspaceToK8s(in *api.Workspace) *k8s.Workspace {
	out := &k8s.Workspace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Workspace",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.WorkspaceSpec{
			DisplayName:            in.Spec.DisplayName,
			RequireSignedArtifacts: in.Spec.RequireSignedArtifacts,
		},
		Status: k8s.WorkspaceStatus{
			Phase: phaseToString(in.Status.Phase),
		},
	}
	if in.Spec.Retention != nil {
		out.Spec.Retention = &k8s.RetentionPolicy{
			TraceTTL:      durationToString(in.Spec.Retention.TraceTTL),
			AuditLogTTL:   durationToString(in.Spec.Retention.AuditLogTTL),
			CostRecordTTL: durationToString(in.Spec.Retention.CostRecordTTL),
			EventTTL:      durationToString(in.Spec.Retention.EventTTL),
		}
	}
	return out
}

func WorkspaceFromK8s(in *k8s.Workspace) *api.Workspace {
	out := &api.Workspace{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.WorkspaceSpec{
			DisplayName:            in.Spec.DisplayName,
			RequireSignedArtifacts: in.Spec.RequireSignedArtifacts,
		},
		Status: api.WorkspaceStatus{
			Phase: phaseFromString(in.Status.Phase),
		},
	}
	if in.Spec.Retention != nil {
		out.Spec.Retention = &api.RetentionPolicy{
			TraceTTL:      durationFromString(in.Spec.Retention.TraceTTL),
			AuditLogTTL:   durationFromString(in.Spec.Retention.AuditLogTTL),
			CostRecordTTL: durationFromString(in.Spec.Retention.CostRecordTTL),
			EventTTL:      durationFromString(in.Spec.Retention.EventTTL),
		}
	}
	return out
}

// --- AgentIdentity ---

func AgentIdentityToK8s(in *api.AgentIdentity) *k8s.AgentIdentity {
	out := &k8s.AgentIdentity{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AgentIdentity",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AgentIdentitySpec{
			SPIFFEID: in.Spec.SPIFFEID,
			AgentRef: localRefToK8s(in.Spec.AgentRef.Name),
		},
		Status: k8s.AgentIdentityStatus{
			Phase: phaseToString(in.Status.Phase),
		},
	}
	if in.Status.WorkloadIdentity != nil {
		status := in.Status.WorkloadIdentity
		out.Status.WorkloadIdentity = &k8s.WorkloadIdentityStatus{
			AgentUID: status.AgentUID, SPIFFEID: status.SPIFFEID, TrustDomain: status.TrustDomain,
			NotBefore: metav1.NewTime(status.NotBefore), ExpiresAt: metav1.NewTime(status.ExpiresAt),
			SourceGeneration: status.SourceGeneration, Ready: status.Ready, Reason: status.Reason,
		}
	}
	return out
}

func AgentIdentityFromK8s(in *k8s.AgentIdentity) *api.AgentIdentity {
	out := &api.AgentIdentity{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.AgentIdentitySpec{
			SPIFFEID: in.Spec.SPIFFEID,
			AgentRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)},
		},
		Status: api.AgentIdentityStatus{
			Phase: phaseFromString(in.Status.Phase),
		},
	}
	if in.Status.WorkloadIdentity != nil {
		status := in.Status.WorkloadIdentity
		out.Status.WorkloadIdentity = &api.WorkloadIdentityStatus{
			AgentUID: status.AgentUID, SPIFFEID: status.SPIFFEID, TrustDomain: status.TrustDomain,
			NotBefore: status.NotBefore.Time, ExpiresAt: status.ExpiresAt.Time,
			SourceGeneration: status.SourceGeneration, Ready: status.Ready, Reason: status.Reason,
		}
	}
	return out
}

// --- AgentRole ---

func AgentRoleToK8s(in *api.AgentRole) *k8s.AgentRole {
	out := &k8s.AgentRole{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AgentRole",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
	}
	if len(in.Rules) > 0 {
		out.Rules = make([]k8s.PolicyRule, len(in.Rules))
		for i, r := range in.Rules {
			out.Rules[i] = k8s.PolicyRule{
				Resources: r.Resources,
				Verbs:     r.Verbs,
			}
		}
	}
	return out
}

func AgentRoleFromK8s(in *k8s.AgentRole) *api.AgentRole {
	out := &api.AgentRole{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
	}
	if len(in.Rules) > 0 {
		out.Rules = make([]api.PolicyRule, len(in.Rules))
		for i, r := range in.Rules {
			out.Rules[i] = api.PolicyRule{
				Resources: r.Resources,
				Verbs:     r.Verbs,
			}
		}
	}
	return out
}

// --- AgentRoleBinding ---

func AgentRoleBindingToK8s(in *api.AgentRoleBinding) *k8s.AgentRoleBinding {
	out := &k8s.AgentRoleBinding{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AgentRoleBinding",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AgentRoleBindingSpec{
			RoleRef: localRefToK8s(in.Spec.RoleRef.Name),
		},
	}
	if len(in.Spec.Subjects) > 0 {
		out.Spec.Subjects = make([]k8s.Subject, len(in.Spec.Subjects))
		for i, s := range in.Spec.Subjects {
			out.Spec.Subjects[i] = k8s.Subject{
				Kind: s.Kind,
				Name: s.Name,
			}
		}
	}
	return out
}

func AgentRoleBindingFromK8s(in *k8s.AgentRoleBinding) *api.AgentRoleBinding {
	out := &api.AgentRoleBinding{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.AgentRoleBindingSpec{
			RoleRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.RoleRef)},
		},
	}
	if len(in.Spec.Subjects) > 0 {
		out.Spec.Subjects = make([]api.Subject, len(in.Spec.Subjects))
		for i, s := range in.Spec.Subjects {
			out.Spec.Subjects[i] = api.Subject{
				Kind: s.Kind,
				Name: s.Name,
			}
		}
	}
	return out
}

// --- AgentCommunicationPolicy ---

func AgentCommunicationPolicyToK8s(in *api.AgentCommunicationPolicy) *k8s.AgentCommunicationPolicy {
	if in == nil {
		return nil
	}
	out := &k8s.AgentCommunicationPolicy{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentCommunicationPolicy", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AgentCommunicationPolicySpec{
			DefaultDeny: in.Spec.DefaultDeny,
		},
	}
	for _, ref := range in.Spec.AllowFrom {
		out.Spec.AllowFrom = append(out.Spec.AllowFrom, localRefToK8s(ref.Name))
	}
	for _, ref := range in.Spec.AllowTo {
		out.Spec.AllowTo = append(out.Spec.AllowTo, localRefToK8s(ref.Name))
	}
	return out
}

func AgentCommunicationPolicyFromK8s(in *k8s.AgentCommunicationPolicy) *api.AgentCommunicationPolicy {
	if in == nil {
		return nil
	}
	out := &api.AgentCommunicationPolicy{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.AgentCommunicationPolicySpec{
			DefaultDeny: in.Spec.DefaultDeny,
		},
	}
	for _, ref := range in.Spec.AllowFrom {
		out.Spec.AllowFrom = append(out.Spec.AllowFrom, api.LocalObjectReference{Name: localRefFromK8s(ref)})
	}
	for _, ref := range in.Spec.AllowTo {
		out.Spec.AllowTo = append(out.Spec.AllowTo, api.LocalObjectReference{Name: localRefFromK8s(ref)})
	}
	return out
}

// --- MemoryGrant ---

func MemoryGrantToK8s(in *api.MemoryGrant) *k8s.MemoryGrant {
	if in == nil {
		return nil
	}
	return &k8s.MemoryGrant{
		TypeMeta:   metav1.TypeMeta{Kind: "MemoryGrant", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.MemoryGrantSpec{
			AgentRef:       localRefToK8s(in.Spec.AgentRef.Name),
			MemoryClaimRef: localRefToK8s(in.Spec.MemoryClaimRef.Name),
			AccessMode:     in.Spec.AccessMode,
		},
	}
}

func MemoryGrantFromK8s(in *k8s.MemoryGrant) *api.MemoryGrant {
	if in == nil {
		return nil
	}
	return &api.MemoryGrant{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.MemoryGrantSpec{
			AgentRef:       api.LocalObjectReference{Name: localRefFromK8s(in.Spec.AgentRef)},
			MemoryClaimRef: api.LocalObjectReference{Name: localRefFromK8s(in.Spec.MemoryClaimRef)},
			AccessMode:     in.Spec.AccessMode,
		},
	}
}
