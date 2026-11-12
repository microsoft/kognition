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

func kbReq(t *testing.T, op Operation, kb v1alpha1.KnowledgeBase) *Request {
	t.Helper()
	data, err := json.Marshal(kb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &Request{Operation: op, Kind: "KnowledgeBase", Object: data}
}

func validKB() v1alpha1.KnowledgeBase {
	return v1alpha1.KnowledgeBase{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "KnowledgeBase"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "kb1", Namespace: "default"},
		Spec: v1alpha1.KnowledgeBaseSpec{
			KnowledgeBaseClassName: "kbc1",
			Sources: []v1alpha1.KBSource{
				{Type: "inline", Data: "Hello world."},
			},
		},
	}
}

func TestKnowledgeBaseValidator_Handles(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	if !v.Handles("KnowledgeBase", OperationCreate) {
		t.Fatal("expected to handle CREATE")
	}
	if !v.Handles("KnowledgeBase", OperationUpdate) {
		t.Fatal("expected to handle UPDATE")
	}
	if v.Handles("KnowledgeBase", Operation("DELETE")) {
		t.Fatal("should not handle DELETE")
	}
	if v.Handles("KnowledgeBaseClass", OperationCreate) {
		t.Fatal("should not handle KnowledgeBaseClass")
	}
}

func TestKnowledgeBaseValidator_AcceptsValid(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	resp := v.Validate(context.Background(), kbReq(t, OperationCreate, validKB()))
	if !resp.Allowed {
		t.Fatalf("expected allow, got reason: %s", resp.Reason)
	}
}

func TestKnowledgeBaseValidator_AcceptsAllSourceTypes(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	kb := validKB()
	kb.Spec.Sources = []v1alpha1.KBSource{
		{Type: "inline", Data: "x"},
		{Type: "url", URI: "https://example.com/doc.txt"},
		{Type: "file", URI: "/data/doc.txt"},
		{Type: "directory", Directory: "/data/docs"},
		{Type: "git", Git: &v1alpha1.KBGitSource{URL: "https://github.com/foo/bar.git"}},
		{Type: "git", Git: &v1alpha1.KBGitSource{URL: "git@github.com:foo/bar.git"}},
		{Type: "configMap", ConfigMap: &v1alpha1.LocalObjectReference{Name: "cm1"}},
		{Type: "blob", Blob: &v1alpha1.KBBlobSource{
			Provider: "azure",
			Bucket:   "docs",
		}},
		{Type: "blob", Blob: &v1alpha1.KBBlobSource{
			Provider: "s3",
			Bucket:   "docs",
			Region:   "us-west-2",
		}},
		{Type: "blob", Blob: &v1alpha1.KBBlobSource{
			Provider:            "s3",
			Bucket:              "docs",
			Endpoint:            "https://minio.internal:9000",
			CredentialSecretRef: &v1alpha1.LocalObjectReference{Name: "s3-creds"},
			Include:             []string{"**/*.md"},
			Exclude:             []string{"drafts/**"},
		}},
	}
	resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
	if !resp.Allowed {
		t.Fatalf("expected allow, got reason: %s", resp.Reason)
	}
}

func TestKnowledgeBaseValidator_RejectsEmptyClassName(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	kb := validKB()
	kb.Spec.KnowledgeBaseClassName = ""
	resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
	if resp.Allowed || !strings.Contains(resp.Reason, "knowledgeBaseClassName") {
		t.Fatalf("expected rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
	}
}

func TestKnowledgeBaseValidator_RejectsEmptySources(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	kb := validKB()
	kb.Spec.Sources = nil
	resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
	if resp.Allowed || !strings.Contains(resp.Reason, "sources") {
		t.Fatalf("expected rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
	}
}

func TestKnowledgeBaseValidator_RejectsSourceTypeIssues(t *testing.T) {
	v := NewKnowledgeBaseValidator()

	cases := []struct {
		name     string
		src      v1alpha1.KBSource
		fragment string
	}{
		{"empty type", v1alpha1.KBSource{Type: ""}, "type is required"},
		{"unknown type", v1alpha1.KBSource{Type: "weird"}, "is invalid"},
		{"inline missing data", v1alpha1.KBSource{Type: "inline"}, "data is required"},
		{
			"inline too big",
			v1alpha1.KBSource{Type: "inline", Data: strings.Repeat("a", maxInlineSourceBytes+1)},
			"limited to",
		},
		{"url missing", v1alpha1.KBSource{Type: "url", URI: "   "}, "uri is required"},
		{"url bad scheme", v1alpha1.KBSource{Type: "url", URI: "ftp://example.com"}, "must start with http"},
		{"file missing", v1alpha1.KBSource{Type: "file"}, "uri is required"},
		{"directory missing", v1alpha1.KBSource{Type: "directory"}, "directory is required"},
		{"git missing", v1alpha1.KBSource{Type: "git"}, "git.url is required"},
		{
			"git bad scheme",
			v1alpha1.KBSource{Type: "git", Git: &v1alpha1.KBGitSource{URL: "ftp://github.com/foo/bar.git"}},
			"must start with",
		},
		{"configMap missing", v1alpha1.KBSource{Type: "configMap"}, "configMap.name is required"},
		{"blob missing", v1alpha1.KBSource{Type: "blob"}, "blob is required"},
		{
			"blob missing provider",
			v1alpha1.KBSource{Type: "blob", Blob: &v1alpha1.KBBlobSource{Bucket: "docs"}},
			"blob.provider is required",
		},
		{
			"blob bad provider",
			v1alpha1.KBSource{Type: "blob", Blob: &v1alpha1.KBBlobSource{Provider: "gcs", Bucket: "docs"}},
			"is invalid",
		},
		{
			"blob missing bucket",
			v1alpha1.KBSource{Type: "blob", Blob: &v1alpha1.KBBlobSource{Provider: "azure"}},
			"blob.bucket is required",
		},
		{
			"s3 missing region and endpoint",
			v1alpha1.KBSource{Type: "blob", Blob: &v1alpha1.KBBlobSource{Provider: "s3", Bucket: "docs"}},
			"region or",
		},
		{
			"blob bad endpoint scheme",
			v1alpha1.KBSource{Type: "blob", Blob: &v1alpha1.KBBlobSource{
				Provider: "s3", Bucket: "docs", Endpoint: "minio.example.com",
			}},
			"must start with http",
		},
		{
			"blob empty credential ref name",
			v1alpha1.KBSource{Type: "blob", Blob: &v1alpha1.KBBlobSource{
				Provider: "azure", Bucket: "docs",
				CredentialSecretRef: &v1alpha1.LocalObjectReference{Name: ""},
			}},
			"credentialSecretRef.name is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kb := validKB()
			kb.Spec.Sources = []v1alpha1.KBSource{tc.src}
			resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
			if resp.Allowed || !strings.Contains(resp.Reason, tc.fragment) {
				t.Fatalf("expected rejection containing %q, got allowed=%v reason=%q", tc.fragment, resp.Allowed, resp.Reason)
			}
		})
	}
}

func TestKnowledgeBaseValidator_RejectsBadIndexConfig(t *testing.T) {
	v := NewKnowledgeBaseValidator()

	cases := []struct {
		name     string
		mutate   func(*v1alpha1.KBIndexConfig)
		fragment string
	}{
		{"negative chunk size", func(c *v1alpha1.KBIndexConfig) { c.ChunkSize = -1 }, "chunkSize"},
		{"negative overlap", func(c *v1alpha1.KBIndexConfig) { c.ChunkOverlap = -1 }, "chunkOverlap"},
		{"overlap >= size", func(c *v1alpha1.KBIndexConfig) { c.ChunkSize = 100; c.ChunkOverlap = 100 }, "must be less than"},
		{
			"empty embedding ref name",
			func(c *v1alpha1.KBIndexConfig) {
				c.EmbeddingModelRef = &v1alpha1.LocalObjectReference{Name: ""}
			},
			"embeddingModelRef.name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kb := validKB()
			kb.Spec.IndexConfig = &v1alpha1.KBIndexConfig{}
			tc.mutate(kb.Spec.IndexConfig)
			resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
			if resp.Allowed || !strings.Contains(resp.Reason, tc.fragment) {
				t.Fatalf("expected rejection containing %q, got allowed=%v reason=%q", tc.fragment, resp.Allowed, resp.Reason)
			}
		})
	}
}

func TestKnowledgeBaseValidator_RefreshPolicy(t *testing.T) {
	v := NewKnowledgeBaseValidator()

	t.Run("missing mode", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if resp.Allowed || !strings.Contains(resp.Reason, "refreshPolicy.mode is required") {
			t.Fatalf("expected mode rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
		}
	})

	t.Run("invalid mode", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "manual"}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if resp.Allowed || !strings.Contains(resp.Reason, "is invalid") {
			t.Fatalf("expected invalid mode rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
		}
	})

	t.Run("periodic missing interval", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic"}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if resp.Allowed || !strings.Contains(resp.Reason, "interval is required") {
			t.Fatalf("expected interval rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
		}
	})

	t.Run("periodic bad interval", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "weekly"}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if resp.Allowed || !strings.Contains(resp.Reason, "valid duration") {
			t.Fatalf("expected duration rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
		}
	})

	t.Run("periodic ok", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: "6h"}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if !resp.Allowed {
			t.Fatalf("expected allow, got reason: %s", resp.Reason)
		}
	})

	for _, interval := range []string{"0s", "-5m"} {
		t.Run("periodic rejects non-positive interval "+interval, func(t *testing.T) {
			kb := validKB()
			kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "periodic", Interval: interval}
			resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
			if resp.Allowed || !strings.Contains(resp.Reason, "must be greater than zero") {
				t.Fatalf("expected non-positive interval rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
			}
		})
	}

	t.Run("webhook missing path", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "webhook"}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if resp.Allowed || !strings.Contains(resp.Reason, "webhookPath is required") {
			t.Fatalf("expected webhookPath rejection, got allowed=%v reason=%q", resp.Allowed, resp.Reason)
		}
	})

	t.Run("onDemand ok", func(t *testing.T) {
		kb := validKB()
		kb.Spec.RefreshPolicy = &v1alpha1.KBRefreshPolicy{Mode: "onDemand"}
		resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
		if !resp.Allowed {
			t.Fatalf("expected allow, got reason: %s", resp.Reason)
		}
	})
}

func TestKnowledgeBaseValidator_RejectsInvalidJSON(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	resp := v.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "KnowledgeBase", Object: []byte("{not json")})
	if resp.Allowed {
		t.Fatal("expected rejection for invalid JSON")
	}
}

func TestKnowledgeBaseValidator_InlineAtMaxBoundaryAllowed(t *testing.T) {
	v := NewKnowledgeBaseValidator()
	kb := validKB()
	kb.Spec.Sources = []v1alpha1.KBSource{{Type: "inline", Data: strings.Repeat("a", maxInlineSourceBytes)}}
	resp := v.Validate(context.Background(), kbReq(t, OperationCreate, kb))
	if !resp.Allowed {
		t.Fatalf("expected allow at exact max size, got reason: %s", resp.Reason)
	}
}
