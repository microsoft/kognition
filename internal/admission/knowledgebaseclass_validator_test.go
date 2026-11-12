// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func kbcReq(t *testing.T, op Operation, kbc v1alpha1.KnowledgeBaseClass) *Request {
	t.Helper()
	data, err := json.Marshal(kbc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &Request{Operation: op, Kind: "KnowledgeBaseClass", Object: data}
}

func validKBC() v1alpha1.KnowledgeBaseClass {
	return v1alpha1.KnowledgeBaseClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBaseClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "kbc1", Namespace: "default"},
		Spec: v1alpha1.KnowledgeBaseClassSpec{
			Provisioner:       "memory",
			IndexMode:         "dense",
			ReclaimPolicy:     v1alpha1.ReclaimDelete,
			ChunkStrategy:     &v1alpha1.KBChunkStrategy{Size: 512, Overlap: 64, Mode: "fixed"},
			EmbeddingModelRef: &v1alpha1.LocalObjectReference{Name: "embed-model"},
		},
	}
}

func TestKnowledgeBaseClassValidator_Handles(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	if !v.Handles("KnowledgeBaseClass", OperationCreate) {
		t.Fatal("expected to handle KnowledgeBaseClass CREATE")
	}
	if !v.Handles("KnowledgeBaseClass", OperationUpdate) {
		t.Fatal("expected to handle KnowledgeBaseClass UPDATE")
	}
	if v.Handles("KnowledgeBaseClass", Operation("DELETE")) {
		t.Fatal("should not handle DELETE")
	}
	if v.Handles("KnowledgeBase", OperationCreate) {
		t.Fatal("should not handle KnowledgeBase")
	}
}

func TestKnowledgeBaseClassValidator_AcceptsValid(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, validKBC()))
	if !resp.Allowed {
		t.Fatalf("expected allow, got reason: %s", resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_AcceptsMinimal(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	kbc := v1alpha1.KnowledgeBaseClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBaseClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "kbc1", Namespace: "default"},
		Spec:       v1alpha1.KnowledgeBaseClassSpec{Provisioner: "memory"},
	}
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
	if !resp.Allowed {
		t.Fatalf("expected allow for minimal spec, got reason: %s", resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_BackendReference(t *testing.T) {
	for _, test := range []struct {
		name      string
		reference string
		extra     string
		allowed   bool
	}{
		{name: "typed reference", reference: `{"apiGroup":"pgvector.knowledge.kognition.dev","kind":"PgvectorBackend","name":"shared"}`, allowed: true},
		{name: "missing group", reference: `{"kind":"PgvectorBackend","name":"shared"}`},
		{name: "invalid group", reference: `{"apiGroup":"https://provider","kind":"PgvectorBackend","name":"shared"}`},
		{name: "missing kind", reference: `{"apiGroup":"pgvector.knowledge.kognition.dev","name":"shared"}`},
		{name: "invalid kind", reference: `{"apiGroup":"pgvector.knowledge.kognition.dev","kind":"../Secret","name":"shared"}`},
		{name: "invalid name", reference: `{"apiGroup":"pgvector.knowledge.kognition.dev","kind":"PgvectorBackend","name":"../other"}`},
		{name: "cross namespace", reference: `{"apiGroup":"pgvector.knowledge.kognition.dev","kind":"PgvectorBackend","name":"shared","namespace":"other"}`},
		{name: "ambiguous backend", reference: `{"apiGroup":"pgvector.knowledge.kognition.dev","kind":"PgvectorBackend","name":"shared"}`, extra: `,"vectorStore":{"type":"pgvector","pgvector":{"connectionSecretRef":{"name":"dsn"}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, operation := range []Operation{OperationCreate, OperationUpdate} {
				request := &Request{Kind: "KnowledgeBaseClass", Operation: operation, Object: []byte(`{"spec":{"provisioner":"pgvector","backendRef":` + test.reference + test.extra + `}}`)}
				response := NewKnowledgeBaseClassValidator().Validate(context.Background(), request)
				if response.Allowed != test.allowed {
					t.Fatalf("allowed=%v, want %v: %s", response.Allowed, test.allowed, response.Reason)
				}
			}
		})
	}
}

func TestKnowledgeBaseClassValidator_RejectsEmptyProvisioner(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	kbc := validKBC()
	kbc.Spec.Provisioner = "   "
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
	if resp.Allowed || !strings.Contains(resp.Reason, "provisioner") {
		t.Fatalf("expected provisioner rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_RejectsInvalidIndexMode(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	kbc := validKBC()
	kbc.Spec.IndexMode = "bogus"
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
	if resp.Allowed || !strings.Contains(resp.Reason, "indexMode") {
		t.Fatalf("expected indexMode rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_RejectsInvalidReclaimPolicy(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	kbc := validKBC()
	kbc.Spec.ReclaimPolicy = "Recycle"
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
	if resp.Allowed || !strings.Contains(resp.Reason, "reclaimPolicy") {
		t.Fatalf("expected reclaimPolicy rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_RejectsBadChunkStrategy(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()

	cases := []struct {
		name     string
		mutate   func(*v1alpha1.KBChunkStrategy)
		fragment string
	}{
		{"negative size", func(c *v1alpha1.KBChunkStrategy) { c.Size = -1 }, "size"},
		{"negative overlap", func(c *v1alpha1.KBChunkStrategy) { c.Overlap = -5 }, "overlap"},
		{"overlap >= size", func(c *v1alpha1.KBChunkStrategy) { c.Size = 100; c.Overlap = 100 }, "must be less than"},
		{"invalid mode", func(c *v1alpha1.KBChunkStrategy) { c.Mode = "weird" }, "mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kbc := validKBC()
			tc.mutate(kbc.Spec.ChunkStrategy)
			resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
			if resp.Allowed || !strings.Contains(resp.Reason, tc.fragment) {
				t.Fatalf("expected rejection containing %q, got allowed=%v reason=%q", tc.fragment, resp.Allowed, resp.Reason)
			}
		})
	}
}

func TestKnowledgeBaseClassValidator_RejectsEmptyEmbeddingRefName(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	kbc := validKBC()
	kbc.Spec.EmbeddingModelRef = &v1alpha1.LocalObjectReference{Name: ""}
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
	if resp.Allowed || !strings.Contains(resp.Reason, "embeddingModelRef.name") {
		t.Fatalf("expected embeddingModelRef.name rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_RejectsInvalidJSON(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "KnowledgeBaseClass", Object: []byte("{not json")})
	if resp.Allowed {
		t.Fatal("expected rejection for invalid JSON")
	}
}

func TestKnowledgeBaseClassValidator_AcceptsPgvectorVectorStore(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()
	kbc := validKBC()
	kbc.Spec.Provisioner = "pgvector"
	kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{
		Type: "pgvector",
		Pgvector: &v1alpha1.KBPgvectorStore{
			ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: "pgvector-dsn"},
			Table:               "kognition_kb_chunks",
			Dimension:           1536,
		},
	}
	resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
	if !resp.Allowed {
		t.Fatalf("expected allow, got reason: %s", resp.Reason)
	}
}

func TestKnowledgeBaseClassValidator_RejectsInvalidPgvectorTable(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()

	for _, table := range []string{
		"chunks; DROP TABLE agents",
		"public.chunks",
		"MixedCase",
		"1chunks",
		strings.Repeat("a", 56),
	} {
		t.Run(table, func(t *testing.T) {
			kbc := validKBC()
			kbc.Spec.VectorStore = &v1alpha1.KBVectorStore{
				Type: "pgvector",
				Pgvector: &v1alpha1.KBPgvectorStore{
					ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: "dsn"},
					Table:               table,
				},
			}
			resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
			if resp.Allowed || !strings.Contains(resp.Reason, "vectorStore.pgvector.table") {
				t.Fatalf("expected table rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
			}
		})
	}
}

func TestKnowledgeBaseClassValidator_RejectsVectorStore(t *testing.T) {
	v := NewKnowledgeBaseClassValidator()

	cases := []struct {
		name     string
		vs       *v1alpha1.KBVectorStore
		fragment string
	}{
		{
			name:     "missing type",
			vs:       &v1alpha1.KBVectorStore{Pgvector: &v1alpha1.KBPgvectorStore{ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: "dsn"}}},
			fragment: "vectorStore.type is required",
		},
		{
			name:     "unknown type",
			vs:       &v1alpha1.KBVectorStore{Type: "milvus"},
			fragment: "invalid",
		},
		{
			name:     "pgvector without block",
			vs:       &v1alpha1.KBVectorStore{Type: "pgvector"},
			fragment: "vectorStore.pgvector is required",
		},
		{
			name: "pgvector missing secret name",
			vs: &v1alpha1.KBVectorStore{
				Type: "pgvector",
				Pgvector: &v1alpha1.KBPgvectorStore{
					ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: ""},
				},
			},
			fragment: "connectionSecretRef.name",
		},
		{
			name: "pgvector negative dimension",
			vs: &v1alpha1.KBVectorStore{
				Type: "pgvector",
				Pgvector: &v1alpha1.KBPgvectorStore{
					ConnectionSecretRef: v1alpha1.LocalObjectReference{Name: "dsn"},
					Dimension:           -1,
				},
			},
			fragment: "dimension",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kbc := validKBC()
			kbc.Spec.VectorStore = tc.vs
			resp := v.Validate(context.Background(), kbcReq(t, OperationCreate, kbc))
			if resp.Allowed || !strings.Contains(resp.Reason, tc.fragment) {
				t.Fatalf("expected rejection containing %q, got allowed=%v reason=%q", tc.fragment, resp.Allowed, resp.Reason)
			}
		})
	}
}
