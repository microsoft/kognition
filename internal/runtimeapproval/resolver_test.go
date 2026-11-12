// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestResolverResolveColdStartUsesSyntheticSupervisedPolicy(t *testing.T) {
	st := &resolverStore{agent: resolverAgent(), classErr: store.ErrNotFound}

	resolution, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Synthetic || resolution.TrustProfile != nil {
		t.Fatalf("cold-start resolution = %#v", resolution)
	}
	if resolution.Class.Name != "supervised" || resolution.Class.UID != SyntheticSupervisedUID || !resolution.Class.Spec.RequiresApproval {
		t.Fatalf("synthetic class = %#v", resolution.Class)
	}
}

func TestResolverResolvePreservesStorageFailures(t *testing.T) {
	storageFailure := errors.New("storage unavailable")
	for name, st := range map[string]*resolverStore{
		"Agent":         {agentErr: storageFailure},
		"TrustProfiles": {agent: resolverAgent(), listErr: storageFailure},
		"AutonomyClass": {agent: resolverAgent(), classErr: storageFailure},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
			if !errors.Is(err, ErrPolicyUnavailable) || !errors.Is(err, storageFailure) {
				t.Fatalf("Resolve() error = %v, want policy and storage error identities", err)
			}
		})
	}
}

func TestResolverResolvePreservesMalformedProfileError(t *testing.T) {
	st := &resolverStore{agent: resolverAgent(), rawProfiles: [][]byte{[]byte(`{"metadata":`)}}
	_, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	var syntaxError *json.SyntaxError
	if !errors.Is(err, ErrPolicyUnavailable) || !errors.As(err, &syntaxError) {
		t.Fatalf("Resolve() error = %v, want policy and JSON syntax error identities", err)
	}
}

func TestResolverResolveRejectsStaleAgentIncarnation(t *testing.T) {
	st := &resolverStore{agent: resolverAgent(), profiles: []api.TrustProfile{resolverProfile("profile-a", "previous-agent-uid", "assisted")}}

	_, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	if !errors.Is(err, ErrPolicyUnavailable) || !strings.Contains(err.Error(), "different Agent incarnation") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolverResolveRejectsMultipleMatchingLiveProfiles(t *testing.T) {
	st := &resolverStore{agent: resolverAgent(), profiles: []api.TrustProfile{
		resolverProfile("profile-a", "agent-uid", "assisted"),
		resolverProfile("profile-b", "agent-uid", "assisted"),
	}}

	_, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	if !errors.Is(err, ErrPolicyUnavailable) || !strings.Contains(err.Error(), "multiple live TrustProfiles") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolverResolveReturnsLiveProfileAndClass(t *testing.T) {
	profile := resolverProfile("profile-a", "agent-uid", "assisted")
	class := api.AutonomyClass{
		TypeMeta:   api.TypeMeta{Kind: "AutonomyClass", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "assisted", Namespace: "system", UID: "class-uid", ResourceVersion: 9},
		Spec:       api.AutonomyClassSpec{Level: 50},
	}
	st := &resolverStore{agent: resolverAgent(), profiles: []api.TrustProfile{profile}, class: class}

	resolution, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Synthetic || resolution.TrustProfile == nil || resolution.TrustProfile.UID != profile.UID {
		t.Fatalf("profile resolution = %#v", resolution)
	}
	if resolution.Class.Name != "assisted" || resolution.Class.UID != class.UID {
		t.Fatalf("class resolution = %#v", resolution.Class)
	}
	if reference := Reference("AutonomyClass", "Cluster", &resolution.Class); reference.Namespace != "" || reference.ResourceVersion != class.ResourceVersion {
		t.Fatalf("class binding must preserve identity without the internal store namespace: %#v", reference)
	}
}

func TestResolverResolveRejectsInvalidTrustProfileIndex(t *testing.T) {
	profile := resolverProfile("profile-a", "agent-uid", "assisted")
	class := api.AutonomyClass{
		ObjectMeta: api.ObjectMeta{Name: "assisted", Namespace: "system", UID: "class-uid", ResourceVersion: 9},
	}
	st := &indexedResolverStore{
		resolverStore: &resolverStore{agent: resolverAgent(), profiles: []api.TrustProfile{profile}, class: class},
		indexErr:      errors.New("selected profile UID mismatch"),
	}

	_, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	if !errors.Is(err, ErrPolicyUnavailable) || !errors.Is(err, st.indexErr) || !strings.Contains(err.Error(), "TrustProfileIndex") {
		t.Fatalf("Resolve() error = %v, want TrustProfileIndex policy failure", err)
	}
}

func TestResolverResolveValidatesTrustProfileIndexDuringColdStart(t *testing.T) {
	st := &indexedResolverStore{resolverStore: &resolverStore{agent: resolverAgent(), classErr: store.ErrNotFound}}

	resolution, err := NewResolver(st).Resolve(context.Background(), "team-a", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if !st.indexValidated || st.validatedProfile != nil {
		t.Fatalf("index validation = called %t, profile %#v; want called with nil profile", st.indexValidated, st.validatedProfile)
	}
	if !resolution.Synthetic || resolution.TrustProfile != nil {
		t.Fatalf("cold-start resolution = %#v", resolution)
	}
}

func TestEvaluateActionPolicyFailsClosedUnlessExplicitlyNotRequired(t *testing.T) {
	tests := []struct {
		name   string
		class  api.AutonomyClass
		action string
		want   string
	}{
		{name: "class requires approval", class: api.AutonomyClass{Spec: api.AutonomyClassSpec{RequiresApproval: true}}, action: ActionToolInvocation, want: api.RuntimeApprovalDecisionPending},
		{name: "missing policy", class: api.AutonomyClass{}, action: ActionMemoryWrite, want: api.RuntimeApprovalDecisionPending},
		{name: "explicit never", class: classWithApprovalPolicy("never", "always", "always"), action: ActionToolInvocation, want: api.RuntimeApprovalDecisionNotRequired},
		{name: "explicit always", class: classWithApprovalPolicy("never", "always", "never"), action: ActionMemoryWrite, want: api.RuntimeApprovalDecisionPending},
		{name: "high risk only", class: classWithApprovalPolicy("never", "never", "high-risk-only"), action: ActionExternalCommunication, want: api.RuntimeApprovalDecisionPending},
		{name: "empty action policy", class: classWithApprovalPolicy("", "never", "never"), action: ActionToolInvocation, want: api.RuntimeApprovalDecisionPending},
		{name: "unknown action policy", class: classWithApprovalPolicy("unexpected", "never", "never"), action: ActionToolInvocation, want: api.RuntimeApprovalDecisionPending},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EvaluateActionPolicy(test.class, test.action)
			if err != nil || got != test.want {
				t.Fatalf("EvaluateActionPolicy() = %q, %v; want %q, nil", got, err, test.want)
			}
		})
	}

	if got, err := EvaluateActionPolicy(classWithApprovalPolicy("never", "never", "never"), "unknown"); !errors.Is(err, ErrPolicyUnavailable) || got != "" {
		t.Fatalf("unsupported action = %q, %v; want empty decision and ErrPolicyUnavailable", got, err)
	}
}

func classWithApprovalPolicy(tool, memory, communication string) api.AutonomyClass {
	return api.AutonomyClass{Spec: api.AutonomyClassSpec{ApprovalPolicy: &api.ApprovalPolicy{
		ToolInvocation: tool, MemoryWrite: memory, ExternalCommunication: communication,
	}}}
}

type resolverStore struct {
	store.Store
	agent       api.Agent
	profiles    []api.TrustProfile
	class       api.AutonomyClass
	classErr    error
	agentErr    error
	listErr     error
	rawProfiles [][]byte
}

type indexedResolverStore struct {
	*resolverStore
	indexErr         error
	indexValidated   bool
	validatedProfile *api.TrustProfile
}

func (s *indexedResolverStore) ValidateTrustProfileIndex(_ context.Context, _ string, _ api.Agent, profile *api.TrustProfile) error {
	s.indexValidated = true
	s.validatedProfile = profile
	return s.indexErr
}

func (s *resolverStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	switch kind {
	case "Agent":
		if s.agentErr != nil {
			return nil, s.agentErr
		}
		if namespace != "team-a" || name != "agent-a" {
			return nil, store.ErrNotFound
		}
		return json.Marshal(s.agent)
	case "AutonomyClass":
		if s.classErr != nil {
			return nil, s.classErr
		}
		if namespace != "system" || name != s.class.Name {
			return nil, store.ErrNotFound
		}
		return json.Marshal(s.class)
	default:
		return nil, store.ErrNotFound
	}
}

func (s *resolverStore) List(_ context.Context, kind string, options store.ListOptions) ([][]byte, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.rawProfiles != nil {
		return s.rawProfiles, nil
	}
	if kind != "TrustProfile" || options.Namespace != "team-a" {
		return nil, nil
	}
	result := make([][]byte, len(s.profiles))
	for index := range s.profiles {
		result[index], _ = json.Marshal(s.profiles[index])
	}
	return result, nil
}

func resolverAgent() api.Agent {
	return api.Agent{
		TypeMeta:   api.TypeMeta{Kind: "Agent", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "agent-a", Namespace: "team-a", UID: "agent-uid", ResourceVersion: 7},
	}
}

func resolverProfile(name, incarnationUID, class string) api.TrustProfile {
	return api.TrustProfile{
		TypeMeta:   api.TypeMeta{Kind: "TrustProfile", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: name, Namespace: "team-a", UID: name + "-uid", ResourceVersion: 8},
		Spec:       api.TrustProfileSpec{AgentRef: api.LocalObjectReference{Name: "agent-a"}, AgentIncarnationUID: incarnationUID, InitialScore: 50, InitialClass: "supervised"},
		Status:     api.TrustProfileStatus{CurrentClass: class},
	}
}
