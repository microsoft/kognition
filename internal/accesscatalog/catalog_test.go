// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package accesscatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const shippedCRDDir = "../../deploy/helm/kognition/crds"

// expectedClasses is plan section 5 encoded independently of the catalog. Adding
// a kind requires an explicit classification decision here and in the plan.
var expectedClasses = map[string]Class{
	"Agent": ClassConfig, "AgentAutoscaler": ClassConfig, "AgentService": ClassConfig,
	"Evaluation": ClassConfig, "EvaluationDataset": ClassConfig, "IntentGateway": ClassConfig,
	"KnowledgeBaseClaim": ClassConfig, "MemoryClaim": ClassConfig, "Model": ClassConfig,
	"ModelAutoscaler": ClassConfig, "ModelInvocationProfile": ClassConfig, "Prompt": ClassConfig,
	"Solution": ClassConfig, "Tool": ClassConfig, "Workflow": ClassConfig,

	"KnowledgeBase": ClassKnowledge, "KnowledgeBaseIndexRun": ClassKnowledge, "KnowledgeBaseIndexSchedule": ClassKnowledge,

	"AgentJob": ClassSubmit, "EvaluationRun": ClassSubmit, "WorkflowRun": ClassSubmit,

	"AgentInstance": ClassRuntime, "Memory": ClassRuntime, "MemoryVolume": ClassRuntime, "ToolInvocationRun": ClassRuntime,

	"MemoryRecord": ClassData,

	"AgentRole": ClassGrant, "AgentRoleBinding": ClassGrant, "CrossNamespaceGrant": ClassGrant,
	"ExternalAgentGrant": ClassGrant, "MemoryGrant": ClassGrant, "ModelGrant": ClassGrant,
	"SecretRef": ClassGrant, "ToolGrant": ClassGrant,

	"AgentCommunicationPolicy": ClassGovernance, "AgentPolicy": ClassGovernance,
	"CognitiveAdmissionPolicy": ClassGovernance, "CognitiveLimitRange": ClassGovernance,
	"CognitiveQuota": ClassGovernance, "TrustProfile": ClassGovernance,

	"ApprovalDecision": ClassDecision, "AutonomyReview": ClassDecision, "RuntimeApproval": ClassDecision,

	"ExternalAgent": ClassPlatform, "Workspace": ClassPlatform,

	"AutonomyClass": ClassClass, "KnowledgeBaseClass": ClassClass, "MemoryClass": ClassClass,
	"ModelClass": ClassClass, "ToolClass": ClassClass,

	"AgentIdentity": ClassSystem, "KnowledgeBindingRecord": ClassSystem,
	"RuntimeApprovalQuotaTicket": ClassSystem, "RuntimeApprovalTombstone": ClassSystem,
	"TrustProfileIndex": ClassSystem,
}

func loadShippedCRDs(t *testing.T) []ShippedCRD {
	t.Helper()
	crds, err := LoadShippedCRDs(shippedCRDDir)
	if err != nil {
		t.Fatalf("load shipped CRDs: %v", err)
	}
	return crds
}

func TestCatalogClassifiesEveryShippedCRD(t *testing.T) {
	crds := loadShippedCRDs(t)
	catalog := Default()
	if err := catalog.VerifyShippedCRDs(crds); err != nil {
		t.Fatalf("catalog does not match the shipped CRDs: %v", err)
	}
	if len(catalog.Kinds) != len(crds) {
		t.Fatalf("catalog has %d kinds, %d CRDs are shipped", len(catalog.Kinds), len(crds))
	}
}

func TestCatalogMatchesPlanClassification(t *testing.T) {
	catalog := Default()
	seen := map[string]bool{}
	for _, kind := range catalog.Kinds {
		want, ok := expectedClasses[kind.Name]
		if !ok {
			t.Errorf("kind %s is classified as %q but is not in the plan", kind.Name, kind.Class)
			continue
		}
		if kind.Class != want {
			t.Errorf("kind %s is class %q, plan says %q", kind.Name, kind.Class, want)
		}
		seen[kind.Name] = true
	}
	for name := range expectedClasses {
		if !seen[name] {
			t.Errorf("plan kind %s is missing from the catalog", name)
		}
	}
}

func TestClassTotalsMatchPlan(t *testing.T) {
	want := map[Class]int{
		ClassConfig: 15, ClassKnowledge: 3, ClassSubmit: 3, ClassRuntime: 4, ClassData: 1,
		ClassGrant: 8, ClassGovernance: 6, ClassDecision: 3, ClassPlatform: 2, ClassClass: 5, ClassSystem: 5,
	}
	got := map[Class]int{}
	for _, kind := range Default().Kinds {
		got[kind.Class]++
	}
	total := 0
	for class, count := range want {
		if got[class] != count {
			t.Errorf("class %s has %d kinds, plan says %d", class, got[class], count)
		}
		total += got[class]
	}
	if total != 55 {
		t.Errorf("classified %d kinds, plan says 55", total)
	}
}

func TestClusterScopedKindsAreOnlyClassOrSystem(t *testing.T) {
	if len(Default().Kinds) == 0 {
		t.Fatal("catalog has no kinds; the check would pass vacuously")
	}
	for _, kind := range Default().Kinds {
		if kind.Scope == ScopeCluster && kind.Class != ClassClass && kind.Class != ClassSystem {
			t.Errorf("cluster-scoped kind %s is class %q; cluster-scoped kinds must be class or system", kind.Name, kind.Class)
		}
	}
}

func TestVerifyShippedCRDsRejectsDrift(t *testing.T) {
	base := Catalog{Kinds: []Kind{{Name: "Agent", Resource: "agents", Scope: ScopeNamespaced, Class: ClassConfig}}}
	shipped := ShippedCRD{Group: APIGroup, Kind: "Agent", Resource: "agents", Scope: ScopeNamespaced}

	tests := []struct {
		name    string
		catalog Catalog
		crds    []ShippedCRD
		wantErr string
	}{
		{name: "exact match", catalog: base, crds: []ShippedCRD{shipped}},
		{
			name: "unclassified kind", catalog: base,
			crds:    []ShippedCRD{shipped, {Group: APIGroup, Kind: "Widget", Resource: "widgets", Scope: ScopeNamespaced}},
			wantErr: "Widget",
		},
		{
			name: "stale catalog kind", catalog: base, crds: nil,
			wantErr: "Agent",
		},
		{
			name: "resource mismatch", catalog: base,
			crds:    []ShippedCRD{{Group: APIGroup, Kind: "Agent", Resource: "robots", Scope: ScopeNamespaced}},
			wantErr: "robots",
		},
		{
			name: "scope mismatch", catalog: base,
			crds:    []ShippedCRD{{Group: APIGroup, Kind: "Agent", Resource: "agents", Scope: ScopeCluster}},
			wantErr: "scope",
		},
		{
			name: "foreign group", catalog: base,
			crds:    []ShippedCRD{{Group: "other.example", Kind: "Agent", Resource: "agents", Scope: ScopeNamespaced}},
			wantErr: "other.example",
		},
		{
			name:    "duplicate classification",
			catalog: Catalog{Kinds: append(append([]Kind{}, base.Kinds...), base.Kinds[0])},
			crds:    []ShippedCRD{shipped}, wantErr: "more than once",
		},
		{
			name:    "empty class",
			catalog: Catalog{Kinds: []Kind{{Name: "Agent", Resource: "agents", Scope: ScopeNamespaced}}},
			crds:    []ShippedCRD{shipped}, wantErr: "class",
		},
		{
			name:    "unknown class",
			catalog: Catalog{Kinds: []Kind{{Name: "Agent", Resource: "agents", Scope: ScopeNamespaced, Class: "mystery"}}},
			crds:    []ShippedCRD{shipped}, wantErr: "mystery",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.catalog.VerifyShippedCRDs(test.crds)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not mention %q", err, test.wantErr)
			}
		})
	}
}

func TestVerifyShippedCRDsReportsEveryProblem(t *testing.T) {
	catalog := Catalog{Kinds: []Kind{{Name: "Agent", Resource: "agents", Scope: ScopeNamespaced, Class: ClassConfig}}}
	crds := []ShippedCRD{
		{Group: APIGroup, Kind: "Widget", Resource: "widgets", Scope: ScopeNamespaced},
		{Group: APIGroup, Kind: "Gadget", Resource: "gadgets", Scope: ScopeNamespaced},
	}
	err := catalog.VerifyShippedCRDs(crds)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"Widget", "Gadget", "Agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should report %s so every problem is fixed in one pass", err, want)
		}
	}
}

func TestLoadShippedCRDsReadsTheChartManifests(t *testing.T) {
	crds := loadShippedCRDs(t)
	if len(crds) == 0 {
		t.Fatal("no CRDs loaded")
	}
	byKind := map[string]ShippedCRD{}
	for _, crd := range crds {
		if crd.Group != APIGroup {
			t.Errorf("%s is in group %q, want %q", crd.Kind, crd.Group, APIGroup)
		}
		byKind[crd.Kind] = crd
	}
	if got := byKind["Agent"]; got.Resource != "agents" || got.Scope != ScopeNamespaced || !got.HasStatus {
		t.Errorf("Agent loaded as %+v", got)
	}
	if got := byKind["ModelClass"]; got.Scope != ScopeCluster {
		t.Errorf("ModelClass scope = %q, want Cluster", got.Scope)
	}
	if got := byKind["ToolGrant"]; got.Resource != "toolgrants" {
		t.Errorf("ToolGrant resource = %q", got.Resource)
	}
}

func TestLoadShippedCRDsRejectsBadInput(t *testing.T) {
	t.Run("missing directory content", func(t *testing.T) {
		if _, err := LoadShippedCRDs(t.TempDir()); err == nil {
			t.Fatal("expected an error for a directory without manifests")
		}
	})
	t.Run("malformed manifest", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("spec: [unterminated"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadShippedCRDs(dir); err == nil {
			t.Fatal("expected a parse error")
		}
	})
	t.Run("incomplete CRD", func(t *testing.T) {
		dir := t.TempDir()
		manifest := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: x\nspec:\n  group: kognition.dev\n"
		if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadShippedCRDs(dir); err == nil {
			t.Fatal("expected an error for a CRD without names")
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		dir := t.TempDir()
		manifest := "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nsurprise: true\n"
		if err := os.WriteFile(filepath.Join(dir, "x.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadShippedCRDs(dir); err == nil {
			t.Fatal("expected strict parsing to reject an unknown field")
		}
	})
}
