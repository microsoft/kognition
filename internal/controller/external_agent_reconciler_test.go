// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

const externalAgentReconcilerDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

type stubExternalAgentResolver map[string][]netip.Addr

func (s stubExternalAgentResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := s[host]; ok {
		return addrs, nil
	}
	return nil, errors.New("no such host")
}

type stubCardObserver struct {
	digest string
	err    error
}

func (s stubCardObserver) ObserveCardDigest(context.Context, v1alpha1.ExternalAgent) (string, error) {
	return s.digest, s.err
}

func storeExternalAgent(t *testing.T, st *memStore, endpoint string) {
	t.Helper()
	agent := v1alpha1.ExternalAgent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ExternalAgent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "partner", Namespace: "team-a"},
		Spec: v1alpha1.ExternalAgentSpec{
			Protocol:      v1alpha1.ExternalAgentProtocolA2A,
			Endpoint:      endpoint,
			Card:          v1alpha1.ExternalAgentCard{Digest: externalAgentReconcilerDigest},
			CredentialRef: v1alpha1.LocalObjectReference{Name: "partner-token"},
		},
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(context.Background(), externalAgentKind, "team-a", "partner", data); err != nil {
		t.Fatal(err)
	}
}

func storeExternalAgentCredential(t *testing.T, st *memStore) {
	t.Helper()
	storeSecretRef(t, st, v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a", Name: "partner-token"},
		Spec:       v1alpha1.SecretRefSpec{BackendType: "internal", BackendRef: "secret/partner", Key: "token"},
	})
}

func reconcileExternalAgent(t *testing.T, st *memStore, opts ...ExternalAgentReconcilerOption) v1alpha1.ExternalAgent {
	t.Helper()
	reconciler := NewExternalAgentReconciler(st, zaptest.NewLogger(t), opts...)
	if _, err := reconciler.Reconcile(context.Background(), Request{Namespace: "team-a", Name: "partner", Kind: "ExternalAgent"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	raw, err := st.Get(context.Background(), externalAgentKind, "team-a", "partner")
	if err != nil {
		t.Fatal(err)
	}
	var agent v1alpha1.ExternalAgent
	if err := json.Unmarshal(raw, &agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func externalAgentCondition(t *testing.T, agent v1alpha1.ExternalAgent, conditionType string) v1alpha1.Condition {
	t.Helper()
	for _, c := range agent.Status.Conditions {
		if c.Type == conditionType {
			return c
		}
	}
	t.Fatalf("condition %s missing from %+v", conditionType, agent.Status.Conditions)
	return v1alpha1.Condition{}
}

func requireExternalAgentCondition(t *testing.T, agent v1alpha1.ExternalAgent, conditionType, status, reason string) {
	t.Helper()
	c := externalAgentCondition(t, agent, conditionType)
	if c.Status != status || c.Reason != reason {
		t.Fatalf("%s = %s/%s (%s), want %s/%s", conditionType, c.Status, c.Reason, c.Message, status, reason)
	}
	if c.Reason == "" || c.Message == "" {
		t.Fatalf("%s must carry a reason and message: %+v", conditionType, c)
	}
}

var externalAgentPublic = stubExternalAgentResolver{"agents.example.com": {netip.MustParseAddr("93.184.216.34")}}

// TC-0345: Ready only when the served card digest equals the pinned digest; a mismatch fails closed.
func TestExternalAgentReconcilerPinsTheCard(t *testing.T) {
	allow, err := externalagent.ParseAllowList("agents.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	build := func(observer ExternalAgentCardObserver) []ExternalAgentReconcilerOption {
		return []ExternalAgentReconcilerOption{WithExternalAgentEgressPolicy(allow), WithExternalAgentResolver(externalAgentPublic), WithExternalAgentCardObserver(observer)}
	}

	st := newMemStore()
	storeExternalAgent(t, st, "agents.example.com:443")
	storeExternalAgentCredential(t, st)
	agent := reconcileExternalAgent(t, st, build(stubCardObserver{digest: externalAgentReconcilerDigest})...)
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionEgress, "True", "Allowed")
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionReachable, "True", "CardFetched")
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionCardPinned, "True", "DigestMatches")
	requireExternalAgentCondition(t, agent, "Ready", "True", "Ready")
	if agent.Status.ObservedCardDigest != externalAgentReconcilerDigest {
		t.Fatalf("observedCardDigest = %q", agent.Status.ObservedCardDigest)
	}

	swapped := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	agent = reconcileExternalAgent(t, st, build(stubCardObserver{digest: swapped})...)
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionCardPinned, "False", "DigestMismatch")
	requireExternalAgentCondition(t, agent, "Ready", "False", "CardDigestMismatch")
	if agent.Status.ObservedCardDigest != swapped {
		t.Fatalf("a swapped card must be visible in status, got %q", agent.Status.ObservedCardDigest)
	}

	agent = reconcileExternalAgent(t, st, build(stubCardObserver{err: errors.New("connection refused")})...)
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionReachable, "False", "CardUnavailable")
	requireExternalAgentCondition(t, agent, "Ready", "False", "EndpointUnavailable")
}

// TC-0346: an endpoint outside the allow-list or resolving to a forbidden address is never Ready.
func TestExternalAgentReconcilerEnforcesEgressAndAddresses(t *testing.T) {
	allow, err := externalagent.ParseAllowList("agents.example.com:443 metadata.example:443")
	if err != nil {
		t.Fatal(err)
	}
	resolver := stubExternalAgentResolver{"metadata.example": {netip.MustParseAddr("169.254.169.254")}}
	observer := stubCardObserver{digest: externalAgentReconcilerDigest}

	st := newMemStore()
	storeExternalAgent(t, st, "other.example.com:443")
	storeExternalAgentCredential(t, st)
	agent := reconcileExternalAgent(t, st, WithExternalAgentEgressPolicy(allow), WithExternalAgentResolver(externalAgentPublic), WithExternalAgentCardObserver(observer))
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionEgress, "False", "NotAllowListed")
	requireExternalAgentCondition(t, agent, "Ready", "False", "EgressNotAllowed")

	st = newMemStore()
	storeExternalAgent(t, st, "metadata.example:443")
	storeExternalAgentCredential(t, st)
	agent = reconcileExternalAgent(t, st, WithExternalAgentEgressPolicy(allow), WithExternalAgentResolver(resolver), WithExternalAgentCardObserver(observer))
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionEgress, "False", "ForbiddenAddress")
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionReachable, "False", "ForbiddenAddress")
	requireExternalAgentCondition(t, agent, "Ready", "False", "EndpointUnavailable")

	st = newMemStore()
	storeExternalAgent(t, st, "agents.example.com:443")
	storeExternalAgentCredential(t, st)
	agent = reconcileExternalAgent(t, st, WithExternalAgentEgressPolicy(externalagentEmpty(t)), WithExternalAgentResolver(externalAgentPublic), WithExternalAgentCardObserver(observer))
	requireExternalAgentCondition(t, agent, "Ready", "False", "EgressNotAllowed")
}

func externalagentEmpty(t *testing.T) *externalagent.AllowList {
	t.Helper()
	list, err := externalagent.ParseAllowList("")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TC-0347: a missing credential and a disabled feature are reported with reasons, and never Ready.
func TestExternalAgentReconcilerReportsMissingCredentialAndDisabledFeature(t *testing.T) {
	allow, err := externalagent.ParseAllowList("agents.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	storeExternalAgent(t, st, "agents.example.com:443")
	agent := reconcileExternalAgent(t, st, WithExternalAgentEgressPolicy(allow), WithExternalAgentResolver(externalAgentPublic), WithExternalAgentCardObserver(stubCardObserver{digest: externalAgentReconcilerDigest}))
	requireExternalAgentCondition(t, agent, "Ready", "False", "CredentialUnavailable")

	st = newMemStore()
	storeExternalAgent(t, st, "agents.example.com:443")
	agent = reconcileExternalAgent(t, st)
	requireExternalAgentCondition(t, agent, "Ready", "False", "FeatureDisabled")

	st = newMemStore()
	storeExternalAgent(t, st, "agents.example.com:443")
	storeExternalAgentCredential(t, st)
	agent = reconcileExternalAgent(t, st, WithExternalAgentEgressPolicy(allow), WithExternalAgentResolver(externalAgentPublic))
	requireExternalAgentCondition(t, agent, v1alpha1.ExternalAgentConditionCardPinned, "Unknown", "NotObserved")
	requireExternalAgentCondition(t, agent, "Ready", "False", "CardNotVerified")
}

func TestExternalAgentReconcilerIsIdempotent(t *testing.T) {
	allow, err := externalagent.ParseAllowList("agents.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	storeExternalAgent(t, st, "agents.example.com:443")
	storeExternalAgentCredential(t, st)
	opts := []ExternalAgentReconcilerOption{WithExternalAgentEgressPolicy(allow), WithExternalAgentResolver(externalAgentPublic), WithExternalAgentCardObserver(stubCardObserver{digest: externalAgentReconcilerDigest})}
	first := reconcileExternalAgent(t, st, opts...)
	second := reconcileExternalAgent(t, st, opts...)
	if first.ObjectMeta.ResourceVersion != second.ObjectMeta.ResourceVersion {
		t.Fatalf("an unchanged agent must not be rewritten: %d -> %d", first.ObjectMeta.ResourceVersion, second.ObjectMeta.ResourceVersion)
	}
}
