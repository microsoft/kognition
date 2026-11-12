// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	"github.com/kognition-ai/kognition/providers/pgvector"
)

func TestGovernedPgvectorShadowSamplePreservesLegacyResourcesAndRequiresModelGrants(t *testing.T) {
	content, err := os.ReadFile("../../deploy/examples/knowledge-pgvector/manual-shadow.yaml")
	if err != nil {
		t.Fatal(err)
	}
	documents, err := installation.Documents(content)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeBases := make(map[string]k8s.KnowledgeBase)
	claims := make(map[string]k8s.KnowledgeBaseClaim)
	agents := make(map[string]k8s.Agent)
	grants := make(map[string]map[string]bool)
	configurations, classes, native, autogen := 0, 0, 0, 0
	for _, object := range documents {
		if object.GetKind() != "KnowledgeBaseClass" && object.GetNamespace() != "default" {
			t.Fatal("shadow sample must stay in its consuming namespace")
		}
		body, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		switch object.GetKind() {
		case "PgvectorBackend":
			configurations++
			spec, err := pgvector.DecodeBackend(object)
			if err != nil || spec.EmbeddingDimensions != 1536 || spec.Table != "kognition_knowledge_chunks_v2" {
				t.Fatalf("invalid versioned backend configuration: %v", err)
			}
		case "KnowledgeBaseClass":
			classes++
			var class k8s.KnowledgeBaseClass
			if err := json.Unmarshal(body, &class); err != nil {
				t.Fatal(err)
			}
			if class.Name != "governed-pgvector-v2" || class.Spec.Provisioner != "pgvector" ||
				class.Spec.IndexMode != "dense" || class.Spec.VectorStore != nil || class.Spec.BackendRef == nil ||
				class.Spec.BackendRef.Name != "manual-pgvector-v2" || class.Spec.BackendRef.APIGroup != pgvector.APIGroup ||
				class.Spec.BackendRef.Kind != pgvector.Kind {
				t.Fatal("shadow class must explicitly select the governed provider")
			}
		case "KnowledgeBase":
			var kb k8s.KnowledgeBase
			if err := json.Unmarshal(body, &kb); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(kb.Name, "-v2") || kb.Spec.KnowledgeBaseClassName != "governed-pgvector-v2" ||
				kb.Spec.RefreshPolicy == nil || kb.Spec.RefreshPolicy.Mode != "onDemand" || len(kb.Spec.Sources) == 0 {
				t.Fatal("shadow KB must preserve originals and avoid recurring refresh")
			}
			knowledgeBases[kb.Name] = kb
		case "KnowledgeBaseClaim":
			var claim k8s.KnowledgeBaseClaim
			if err := json.Unmarshal(body, &claim); err != nil {
				t.Fatal(err)
			}
			if claim.Spec.Retrieval == nil || claim.Spec.Retrieval.Mode != "dense" ||
				claim.Spec.Retrieval.TopK == nil || *claim.Spec.Retrieval.TopK != 5 {
				t.Fatal("shadow claims must declare their semantic retrieval intent")
			}
			claims[claim.Name] = claim
		case "Agent":
			var agent k8s.Agent
			if err := json.Unmarshal(body, &agent); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(agent.Name, "-v2") || agent.Spec.ModelRef.Name != "gpt-6-luna" ||
				len(agent.Spec.KnowledgeBaseClaims) != 1 || agent.Spec.Runtime == nil {
				t.Fatal("shadow Agent must reference a reviewed Model and one claim")
			}
			switch agent.Spec.Runtime.Class {
			case "default":
				native++
			case "autogen":
				autogen++
			default:
				t.Fatal("unexpected sample runtime")
			}
			agents[agent.Name] = agent
		case "ModelGrant":
			var grant k8s.ModelGrant
			if err := json.Unmarshal(body, &grant); err != nil {
				t.Fatal(err)
			}
			if grants[grant.Spec.AgentRef.Name] == nil {
				grants[grant.Spec.AgentRef.Name] = make(map[string]bool)
			}
			if grants[grant.Spec.AgentRef.Name][grant.Spec.ModelRef.Name] {
				t.Fatal("duplicate Model grant")
			}
			grants[grant.Spec.AgentRef.Name][grant.Spec.ModelRef.Name] = true
		default:
			t.Fatalf("unexpected shadow resource %s; no Secrets, status, schedules or duplicate explicit index runs belong here", object.GetKind())
		}
	}
	if configurations != 1 || classes != 1 || len(knowledgeBases) != 3 || len(claims) != 3 || native != 3 || autogen != 1 || len(grants) != 4 {
		t.Fatal("the shadow migration must cover three KBs and both runtime profiles")
	}
	for _, claim := range claims {
		if _, found := knowledgeBases[claim.Spec.KnowledgeBaseName]; !found {
			t.Fatal("shadow claim does not reference a shadow KB")
		}
	}
	for name, agent := range agents {
		if _, found := claims[agent.Spec.KnowledgeBaseClaims[0].Name]; !found {
			t.Fatal("Agent claim does not belong to the shadow migration")
		}
		if len(grants[name]) != 2 || !grants[name]["gpt-6-luna"] || !grants[name]["text-embedding-3-small"] {
			t.Fatal("every query Agent requires explicit chat and embedding Model grants")
		}
	}
}
