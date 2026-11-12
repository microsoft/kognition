// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert_test

import (
	"reflect"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

func TestKnowledgeBaseClassRoundTrip(t *testing.T) {
	in := &api.KnowledgeBaseClass{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBaseClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "kbc", Namespace: "default"},
		Spec: api.KnowledgeBaseClassSpec{
			Provisioner:       "pgvector",
			IndexMode:         "hybrid",
			ReclaimPolicy:     api.ReclaimRetain,
			EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
			ChunkStrategy:     &api.KBChunkStrategy{Size: 1024, Overlap: 128, Mode: "recursive"},
			VectorStore: &api.KBVectorStore{
				Type: "pgvector",
				Pgvector: &api.KBPgvectorStore{
					ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector-dsn"},
					Table:               "kognition_kb_chunks",
					Dimension:           1536,
				},
			},
			Parameters: map[string]string{"region": "us-east-1"},
		},
	}

	out := convert.KnowledgeBaseClassFromK8s(convert.KnowledgeBaseClassToK8s(in))

	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("spec round-trip mismatch:\n got: %+v\nwant: %+v", out.Spec, in.Spec)
	}
	if out.ObjectMeta.Name != in.ObjectMeta.Name {
		t.Errorf("name: got %q want %q", out.ObjectMeta.Name, in.ObjectMeta.Name)
	}
}

func TestKnowledgeBaseClassBackendReference(t *testing.T) {
	input := &api.KnowledgeBaseClass{Spec: api.KnowledgeBaseClassSpec{
		Provisioner: "pgvector",
		BackendRef:  &api.KnowledgeBackendReference{APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared"},
	}}
	kubernetes := convert.KnowledgeBaseClassToK8s(input)
	output := convert.KnowledgeBaseClassFromK8s(kubernetes)
	if !reflect.DeepEqual(input.Spec, output.Spec) {
		t.Fatalf("backend reference changed in round trip: %+v", output.Spec)
	}
	copy := kubernetes.DeepCopy()
	copy.Spec.BackendRef.Name = "changed"
	kubernetes.Spec.BackendRef.Name = "original-changed"
	if input.Spec.BackendRef.Name != "shared" || output.Spec.BackendRef.Name != "shared" || copy.Spec.BackendRef.Name != "changed" {
		t.Fatal("backend reference aliased across conversion or deep copy")
	}
}

func TestKnowledgeBaseClassRoundTrip_Minimal(t *testing.T) {
	in := &api.KnowledgeBaseClass{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBaseClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "kbc", Namespace: "default"},
		Spec:       api.KnowledgeBaseClassSpec{Provisioner: "memory"},
	}
	out := convert.KnowledgeBaseClassFromK8s(convert.KnowledgeBaseClassToK8s(in))
	if out.Spec.Provisioner != "memory" {
		t.Errorf("provisioner lost: got %q", out.Spec.Provisioner)
	}
	if out.Spec.EmbeddingModelRef != nil {
		t.Errorf("expected nil embedding ref, got %+v", out.Spec.EmbeddingModelRef)
	}
	if out.Spec.ChunkStrategy != nil {
		t.Errorf("expected nil chunk strategy, got %+v", out.Spec.ChunkStrategy)
	}
}

func TestKnowledgeBaseRoundTrip(t *testing.T) {
	in := &api.KnowledgeBase{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBase", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "kb", Namespace: "default"},
		Spec: api.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "kbc",
			Sources: []api.KBSource{
				{Type: "inline", Data: "hello"},
				{Type: "url", URI: "https://example.com/doc.txt"},
				{Type: "file", URI: "/data/doc.txt"},
				{Type: "directory", Directory: "/data/docs"},
				{Type: "git", Git: &api.KBGitSource{URL: "https://github.com/foo/bar.git", Branch: "main", Path: "docs"}},
				{Type: "configMap", ConfigMap: &api.LocalObjectReference{Name: "cm1"}},
				{Type: "blob", Blob: &api.KBBlobSource{
					Provider:            "azure",
					Bucket:              "docs",
					Prefix:              "mark7/",
					CredentialSecretRef: &api.LocalObjectReference{Name: "azure-creds"},
					Include:             []string{"**/*.md", "**/*.pdf"},
					Exclude:             []string{"drafts/**"},
				}},
				{Type: "blob", Blob: &api.KBBlobSource{
					Provider: "s3",
					Bucket:   "docs",
					Region:   "us-west-2",
					Endpoint: "https://minio.example.com",
				}},
			},
			IndexConfig: &api.KBIndexConfig{
				ChunkSize:         256,
				ChunkOverlap:      32,
				EmbeddingModelRef: &api.LocalObjectReference{Name: "embed-override"},
			},
			RefreshPolicy: &api.KBRefreshPolicy{Mode: "periodic", Interval: "12h"},
		},
		Status: api.KnowledgeBaseStatus{
			Phase:         api.PhaseReady,
			DocumentCount: 7,
			IndexedChunks: 42,
			Generation:    3,
			LastRefresh:   "2025-01-01T00:00:00Z",
		},
	}

	out := convert.KnowledgeBaseFromK8s(convert.KnowledgeBaseToK8s(in))

	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("spec round-trip mismatch:\n got: %+v\nwant: %+v", out.Spec, in.Spec)
	}
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Errorf("status round-trip mismatch:\n got: %+v\nwant: %+v", out.Status, in.Status)
	}
}

func TestKnowledgeBaseRoundTrip_NoOptionals(t *testing.T) {
	in := &api.KnowledgeBase{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBase", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "kb", Namespace: "default"},
		Spec: api.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "kbc",
			Sources:                []api.KBSource{{Type: "inline", Data: "x"}},
		},
	}
	out := convert.KnowledgeBaseFromK8s(convert.KnowledgeBaseToK8s(in))
	if out.Spec.IndexConfig != nil {
		t.Errorf("expected nil IndexConfig, got %+v", out.Spec.IndexConfig)
	}
	if out.Spec.RefreshPolicy != nil {
		t.Errorf("expected nil RefreshPolicy, got %+v", out.Spec.RefreshPolicy)
	}
	if len(out.Spec.Sources) != 1 || out.Spec.Sources[0].Type != "inline" || out.Spec.Sources[0].Data != "x" {
		t.Errorf("inline source not preserved: got %+v", out.Spec.Sources)
	}
}

func TestKnowledgeBaseCommittedBackendBinding(t *testing.T) {
	input := &api.KnowledgeBase{Status: api.KnowledgeBaseStatus{
		Generation: 3,
		BackendBinding: &api.KnowledgeBackendBinding{
			Generation: 3, ProviderName: "pgvector", ClassUID: "class-uid", ClassGeneration: 2,
			ReclaimPolicy: api.ReclaimRetain, ConfigurationUID: "config-uid", ConfigurationDigest: "digest", OperationID: "team-a/index-run-3",
		},
	}}
	kubernetes := convert.KnowledgeBaseToK8s(input)
	output := convert.KnowledgeBaseFromK8s(kubernetes)
	if !reflect.DeepEqual(input.Status, output.Status) {
		t.Fatalf("committed binding changed in round trip: %+v", output.Status)
	}
	copy := kubernetes.DeepCopy()
	copy.Status.BackendBinding.ConfigurationUID = "copy-changed"
	kubernetes.Status.BackendBinding.ConfigurationUID = "original-changed"
	if input.Status.BackendBinding.ConfigurationUID != "config-uid" || output.Status.BackendBinding.ConfigurationUID != "config-uid" || copy.Status.BackendBinding.ConfigurationUID != "copy-changed" {
		t.Fatal("committed binding aliased across conversion or deep copy")
	}
}

func TestKnowledgeBaseRoundTrip_EmptySources(t *testing.T) {
	in := &api.KnowledgeBase{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBase", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: api.ObjectMeta{Name: "kb", Namespace: "default"},
		Spec:       api.KnowledgeBaseSpec{KnowledgeBaseClassName: "kbc"},
	}
	out := convert.KnowledgeBaseFromK8s(convert.KnowledgeBaseToK8s(in))
	if len(out.Spec.Sources) != 0 {
		t.Errorf("expected empty sources, got %+v", out.Spec.Sources)
	}
}

func TestKnowledgeBaseClaimRoundTrip(t *testing.T) {
	topK := int32(12)
	maxQueriesPerMinute := int32(90)
	maxTokensReturned := int32(4096)
	transitionTime := time.Date(2026, time.August, 2, 11, 30, 0, 0, time.UTC)
	in := &api.KnowledgeBaseClaim{
		TypeMeta: api.TypeMeta{Kind: "KnowledgeBaseClaim", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "support-corpus-reader",
			Namespace:   "production",
			Labels:      map[string]string{"team": "support"},
			Annotations: map[string]string{"purpose": "grounding"},
			Finalizers:  []string{"kognition.dev/knowledgebaseclaim-cleanup"},
		},
		Spec: api.KnowledgeBaseClaimSpec{
			KnowledgeBaseName: "support-corpus",
			AccessMode:        "ReadOnly",
			Retrieval: &api.KBRetrievalOverride{
				TopK:     &topK,
				MinScore: "0.72",
				Filter:   map[string]string{"locale": "en-US", "state": "published"},
				HybridWeights: &api.KBHybridWeights{
					Keyword: "0.35",
					Vector:  "0.65",
				},
			},
			Usage: &api.KBUsageQuota{
				MaxQueriesPerMinute: &maxQueriesPerMinute,
				MaxTokensReturned:   &maxTokensReturned,
			},
		},
		Status: api.KnowledgeBaseClaimStatus{
			Phase:              api.PhaseBound,
			BoundKnowledgeBase: "support-corpus",
			ObservedGeneration: 7,
			Conditions: []api.Condition{{
				Type:               "Ready",
				Status:             "True",
				LastTransitionTime: transitionTime,
				Reason:             "Bound",
				Message:            "knowledge base is ready for retrieval",
			}},
		},
	}

	out := convert.KnowledgeBaseClaimFromK8s(convert.KnowledgeBaseClaimToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("claim identity metadata changed: got %#v, want %#v", out, in)
	}
	if !reflect.DeepEqual(out.ObjectMeta.Labels, in.ObjectMeta.Labels) || !reflect.DeepEqual(out.ObjectMeta.Annotations, in.ObjectMeta.Annotations) || !reflect.DeepEqual(out.ObjectMeta.Finalizers, in.ObjectMeta.Finalizers) {
		t.Errorf("claim object metadata changed: got %#v, want %#v", out.ObjectMeta, in.ObjectMeta)
	}
	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("claim retrieval policy changed:\n got: %#v\nwant: %#v", out.Spec, in.Spec)
	}
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Errorf("claim binding status changed:\n got: %#v\nwant: %#v", out.Status, in.Status)
	}
}

func TestKnowledgeBaseClaimRoundTrip_NoOverrides(t *testing.T) {
	in := &api.KnowledgeBaseClaim{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBaseClaim", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "default-reader", Namespace: "production"},
		Spec:       api.KnowledgeBaseClaimSpec{KnowledgeBaseName: "support-corpus", AccessMode: "ReadOnly"},
	}

	out := convert.KnowledgeBaseClaimFromK8s(convert.KnowledgeBaseClaimToK8s(in))

	if out.Spec.Retrieval != nil {
		t.Errorf("expected retrieval override to remain absent, got %#v", out.Spec.Retrieval)
	}
	if out.Spec.Usage != nil {
		t.Errorf("expected usage quota to remain absent, got %#v", out.Spec.Usage)
	}
}
