// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package namespace_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/namespace"
	"github.com/kognition-ai/kognition/internal/store"
)

// Compile-time assertion: CrossNamespaceHook must implement ValidatingHook.
var _ admission.ValidatingHook = (*namespace.CrossNamespaceHook)(nil)

// fakeStore implements store.Store with in-memory data for testing.
type fakeStore struct {
	store.Store
	resources map[string][][]byte // key: kind+namespace
	listErr   error               // if set, List returns this error
}

func newFakeStore() *fakeStore {
	return &fakeStore{resources: make(map[string][][]byte)}
}

func (s *fakeStore) List(_ context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	key := kind + "/" + opts.Namespace
	return s.resources[key], nil
}

func (s *fakeStore) addGrant(grant v1alpha1.CrossNamespaceGrant) {
	data, _ := json.Marshal(grant)
	key := "CrossNamespaceGrant/" + grant.ObjectMeta.Namespace
	s.resources[key] = append(s.resources[key], data)
}

func (s *fakeStore) addRawGrant(ns string, data []byte) {
	key := "CrossNamespaceGrant/" + ns
	s.resources[key] = append(s.resources[key], data)
}

// TC-0015: Cross-namespace reference denied without grant.
func TestCrossNamespace_DeniedWithoutGrant(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "web-search",
		Namespace: "shared-tools",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected denial for cross-namespace reference without grant")
	}
	if !errors.Is(err, namespace.ErrCrossNamespaceDenied) {
		t.Fatalf("expected ErrCrossNamespaceDenied, got: %v", err)
	}
}

// TC-0016: Cross-namespace reference allowed with matching grant.
func TestCrossNamespace_AllowedWithGrant(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-team-a",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From: "shared-tools",
			To:   "team-a",
			Resources: []v1alpha1.GrantResource{
				{Kind: "Tool", Name: "web-search"},
			},
		},
	})

	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "web-search",
		Namespace: "shared-tools",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err != nil {
		t.Fatalf("expected cross-namespace reference to be allowed, got: %v", err)
	}
}

// TC-0159: Cross-namespace KnowledgeBase query is denied without an explicit grant.
// TC-0156: Access is denied when no applicable grant exists for the KnowledgeBase reference.
func TestCrossNamespace_KnowledgeBaseDeniedWithoutGrant(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "KnowledgeBase",
		Name:      "shared-kb",
		Namespace: "knowledge-team",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected denial for cross-namespace KnowledgeBase reference without grant")
	}
	if !errors.Is(err, namespace.ErrCrossNamespaceDenied) {
		t.Fatalf("expected ErrCrossNamespaceDenied, got: %v", err)
	}
}

// TC-0157: KnowledgeBase access is allowed when a matching grant authorizes the reference.
func TestCrossNamespace_KnowledgeBaseAllowedWithGrant(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-team-a-kb",
			Namespace: "knowledge-team",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From: "knowledge-team",
			To:   "team-a",
			Resources: []v1alpha1.GrantResource{
				{Kind: "KnowledgeBase", Name: "shared-kb"},
			},
		},
	})

	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "KnowledgeBase",
		Name:      "shared-kb",
		Namespace: "knowledge-team",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err != nil {
		t.Fatalf("expected KnowledgeBase reference to be allowed by grant, got: %v", err)
	}
}

// Test same-namespace references are always allowed (no grant needed).
func TestCrossNamespace_SameNamespaceAlwaysAllowed(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "local-tool",
		Namespace: "team-a",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err != nil {
		t.Fatalf("same-namespace reference should always be allowed: %v", err)
	}
}

// Test empty namespace in ref defaults to same-namespace.
func TestCrossNamespace_EmptyNamespaceAllowed(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind: "Tool",
		Name: "local-tool",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err != nil {
		t.Fatalf("empty-namespace reference should be allowed: %v", err)
	}
}

// Test wildcard To grants access to any namespace.
func TestCrossNamespace_WildcardToGrant(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-all",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From: "shared-tools",
			To:   "*",
			Resources: []v1alpha1.GrantResource{
				{Kind: "Tool", Name: "web-search"},
			},
		},
	})

	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "web-search",
		Namespace: "shared-tools",
	}

	// Any namespace should be allowed.
	err := resolver.CheckReference(context.Background(), "team-x", ref)
	if err != nil {
		t.Fatalf("wildcard grant should allow any namespace: %v", err)
	}
}

// Test wildcard resource name grants access to all resources of that kind.
func TestCrossNamespace_WildcardResourceName(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-all-tools",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From: "shared-tools",
			To:   "team-a",
			Resources: []v1alpha1.GrantResource{
				{Kind: "Tool", Name: "*"},
			},
		},
	})

	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "any-tool-name",
		Namespace: "shared-tools",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err != nil {
		t.Fatalf("wildcard name grant should allow any resource: %v", err)
	}
}

// Test admission hook integration.
func TestCrossNamespaceHook_DeniesWithoutGrant(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	hook.RegisterExtractor("Agent", func(obj json.RawMessage) ([]v1alpha1.ResourceReference, error) {
		var spec struct {
			ToolRefs []v1alpha1.ResourceReference `json:"toolRefs"`
		}
		if err := json.Unmarshal(obj, &spec); err != nil {
			return nil, err
		}
		return spec.ToolRefs, nil
	})

	agent := map[string]interface{}{
		"toolRefs": []v1alpha1.ResourceReference{
			{Kind: "Tool", Name: "web-search", Namespace: "shared-tools"},
		},
	}
	body, _ := json.Marshal(agent)

	req := &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      "Agent",
		Namespace: "team-a",
		Name:      "my-agent",
		Object:    body,
	}

	resp := hook.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected hook to deny cross-namespace reference without grant")
	}
}

func TestCrossNamespaceHook_AllowsWithGrant(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-team-a",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From: "shared-tools",
			To:   "team-a",
			Resources: []v1alpha1.GrantResource{
				{Kind: "Tool", Name: "web-search"},
			},
		},
	})

	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	hook.RegisterExtractor("Agent", func(obj json.RawMessage) ([]v1alpha1.ResourceReference, error) {
		var spec struct {
			ToolRefs []v1alpha1.ResourceReference `json:"toolRefs"`
		}
		if err := json.Unmarshal(obj, &spec); err != nil {
			return nil, err
		}
		return spec.ToolRefs, nil
	})

	agent := map[string]interface{}{
		"toolRefs": []v1alpha1.ResourceReference{
			{Kind: "Tool", Name: "web-search", Namespace: "shared-tools"},
		},
	}
	body, _ := json.Marshal(agent)

	req := &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      "Agent",
		Namespace: "team-a",
		Name:      "my-agent",
		Object:    body,
	}

	resp := hook.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected hook to allow reference with grant, got: %s", resp.Reason)
	}
}

// Test hook skips unregistered kinds.
func TestCrossNamespaceHook_SkipsUnregisteredKind(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	req := &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      "UnknownKind",
		Namespace: "team-a",
		Name:      "test",
		Object:    json.RawMessage(`{}`),
	}

	resp := hook.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatal("hook should allow unregistered kinds")
	}
}

// --- Additional coverage tests ---

// Test store.List error is propagated by the resolver.
func TestCrossNamespace_StoreErrorPropagated(t *testing.T) {
	s := newFakeStore()
	s.listErr = fmt.Errorf("connection refused")
	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "web-search",
		Namespace: "shared-tools",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected error when store fails")
	}
	// Should NOT be ErrCrossNamespaceDenied since it's an infrastructure error.
	if errors.Is(err, namespace.ErrCrossNamespaceDenied) {
		t.Fatal("store error should not be classified as denied")
	}
}

// Test malformed JSON grants in store are skipped (continue path).
func TestCrossNamespace_MalformedGrantSkipped(t *testing.T) {
	s := newFakeStore()
	// Add malformed JSON that won't unmarshal.
	s.addRawGrant("shared-tools", []byte(`{not valid json`))
	// Without a valid grant, cross-namespace should be denied.
	resolver := namespace.NewResolver(s)

	ref := v1alpha1.ResourceReference{
		Kind:      "Tool",
		Name:      "web-search",
		Namespace: "shared-tools",
	}

	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected denial when only malformed grants exist")
	}
	if !errors.Is(err, namespace.ErrCrossNamespaceDenied) {
		t.Fatalf("expected ErrCrossNamespaceDenied, got: %v", err)
	}
}

// Test grant with From mismatch is not matched.
func TestCrossNamespace_GrantFromMismatch(t *testing.T) {
	s := newFakeStore()
	// Grant lives in "shared-tools" namespace, but From says "other-ns".
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "wrong-from",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From:      "other-ns", // Doesn't match "shared-tools"
			To:        "team-a",
			Resources: []v1alpha1.GrantResource{{Kind: "Tool", Name: "web-search"}},
		},
	})

	resolver := namespace.NewResolver(s)
	ref := v1alpha1.ResourceReference{Kind: "Tool", Name: "web-search", Namespace: "shared-tools"}
	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected denial when grant From doesn't match")
	}
}

// Test grant with To mismatch (non-wildcard, wrong target namespace).
func TestCrossNamespace_GrantToMismatch(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-team-b-only",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From:      "shared-tools",
			To:        "team-b", // Only allows team-b
			Resources: []v1alpha1.GrantResource{{Kind: "Tool", Name: "web-search"}},
		},
	})

	resolver := namespace.NewResolver(s)
	ref := v1alpha1.ResourceReference{Kind: "Tool", Name: "web-search", Namespace: "shared-tools"}
	// team-a tries to access, but grant is for team-b
	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected denial when grant To doesn't match requesting namespace")
	}
}

// Test grant with kind mismatch in resources.
func TestCrossNamespace_KindMismatch(t *testing.T) {
	// TC-0161: invalid grant target kind/verb combinations are rejected by access checks.
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-memory-only",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From:      "shared-tools",
			To:        "team-a",
			Resources: []v1alpha1.GrantResource{{Kind: "Memory", Name: "*"}}, // Only Memory, not Tool
		},
	})

	resolver := namespace.NewResolver(s)
	ref := v1alpha1.ResourceReference{Kind: "Tool", Name: "web-search", Namespace: "shared-tools"}
	err := resolver.CheckReference(context.Background(), "team-a", ref)
	if err == nil {
		t.Fatal("expected denial when grant kind doesn't match reference kind")
	}
}

// Test CheckReferences stops at first denial.
func TestCrossNamespace_CheckReferencesFirstDenied(t *testing.T) {
	s := newFakeStore()
	s.addGrant(v1alpha1.CrossNamespaceGrant{
		TypeMeta: v1alpha1.TypeMeta{Kind: "CrossNamespaceGrant", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "allow-one-tool",
			Namespace: "shared-tools",
		},
		Spec: v1alpha1.CrossNamespaceGrantSpec{
			From:      "shared-tools",
			To:        "team-a",
			Resources: []v1alpha1.GrantResource{{Kind: "Tool", Name: "allowed-tool"}},
		},
	})

	resolver := namespace.NewResolver(s)
	refs := []v1alpha1.ResourceReference{
		{Kind: "Tool", Name: "allowed-tool", Namespace: "shared-tools"}, // allowed
		{Kind: "Tool", Name: "denied-tool", Namespace: "shared-tools"},  // denied
	}
	err := resolver.CheckReferences(context.Background(), "team-a", refs)
	if err == nil {
		t.Fatal("expected denial for second reference")
	}
	if !errors.Is(err, namespace.ErrCrossNamespaceDenied) {
		t.Fatalf("expected ErrCrossNamespaceDenied, got: %v", err)
	}
}

// Test hook Name() method.
func TestCrossNamespaceHook_Name(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	if name := hook.Name(); name != "cross-namespace-deny" {
		t.Fatalf("expected name %q, got %q", "cross-namespace-deny", name)
	}
}

// Test hook Handles() returns true for registered and false for unregistered kinds.
func TestCrossNamespaceHook_Handles(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	hook.RegisterExtractor("Agent", func(obj json.RawMessage) ([]v1alpha1.ResourceReference, error) {
		return nil, nil
	})

	if !hook.Handles("Agent", admission.OperationCreate) {
		t.Fatal("expected Handles to return true for registered kind")
	}
	if !hook.Handles("Agent", admission.OperationUpdate) {
		t.Fatal("expected Handles to return true regardless of operation")
	}
	if hook.Handles("Tool", admission.OperationCreate) {
		t.Fatal("expected Handles to return false for unregistered kind")
	}
}

// Test hook denies when extractor returns an error.
func TestCrossNamespaceHook_ExtractorError(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	hook.RegisterExtractor("Agent", func(obj json.RawMessage) ([]v1alpha1.ResourceReference, error) {
		return nil, fmt.Errorf("parse failure")
	})

	req := &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      "Agent",
		Namespace: "team-a",
		Name:      "bad-agent",
		Object:    json.RawMessage(`{"broken"}`),
	}

	resp := hook.Validate(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial when extractor returns an error")
	}
	if resp.Reason == "" {
		t.Fatal("expected denial reason to be set")
	}
}

// Test hook allows when extractor returns no references.
func TestCrossNamespaceHook_NoRefsAllowed(t *testing.T) {
	s := newFakeStore()
	resolver := namespace.NewResolver(s)
	hook := namespace.NewCrossNamespaceHook(resolver)

	hook.RegisterExtractor("Agent", func(obj json.RawMessage) ([]v1alpha1.ResourceReference, error) {
		return nil, nil // No refs
	})

	req := &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      "Agent",
		Namespace: "team-a",
		Name:      "local-agent",
		Object:    json.RawMessage(`{}`),
	}

	resp := hook.Validate(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allow when no references, got: %s", resp.Reason)
	}
}

func TestProductionCrossNamespaceHook_DeniesEvaluationReferencesWithoutGrant(t *testing.T) {
	s := newFakeStore()
	hook := namespace.NewProductionCrossNamespaceHook(s)
	evaluation := v1alpha1.Evaluation{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "quality", Namespace: "team-a"},
		Spec: v1alpha1.EvaluationSpec{
			TargetRef:  v1alpha1.EvaluationTargetRef{Kind: "Agent", Name: "reviewer", Namespace: "shared"},
			DatasetRef: v1alpha1.EvaluationDatasetRef{Name: "cases", Namespace: "team-a"},
		},
	}
	data, err := json.Marshal(evaluation)
	if err != nil {
		t.Fatal(err)
	}

	if !hook.Handles("Evaluation", admission.OperationCreate) {
		t.Fatal("production hook must handle Evaluation")
	}
	resp := hook.Validate(context.Background(), &admission.Request{
		Operation: admission.OperationCreate,
		Kind:      "Evaluation",
		Namespace: "team-a",
		Name:      "quality",
		Object:    data,
	})
	if resp.Allowed {
		t.Fatal("expected cross-namespace Evaluation target to be denied without a grant")
	}
}
