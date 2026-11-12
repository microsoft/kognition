// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// KnowledgeBaseClassValidator validates KnowledgeBaseClass resources on CREATE and UPDATE.
type KnowledgeBaseClassValidator struct{}

// NewKnowledgeBaseClassValidator creates a new KnowledgeBaseClassValidator.
func NewKnowledgeBaseClassValidator() *KnowledgeBaseClassValidator {
	return &KnowledgeBaseClassValidator{}
}

func (v *KnowledgeBaseClassValidator) Name() string { return "knowledgebaseclass-validator" }

func (v *KnowledgeBaseClassValidator) Handles(kind string, op Operation) bool {
	return kind == "KnowledgeBaseClass" && (op == OperationCreate || op == OperationUpdate)
}

var validKBIndexModes = map[string]struct{}{
	"dense":  {},
	"sparse": {},
	"hybrid": {},
	"inline": {},
}

var validKBChunkModes = map[string]struct{}{
	"fixed":     {},
	"sentence":  {},
	"recursive": {},
}

var validKBReclaimPolicies = map[v1alpha1.ReclaimPolicy]struct{}{
	v1alpha1.ReclaimDelete:  {},
	v1alpha1.ReclaimRetain:  {},
	v1alpha1.ReclaimArchive: {},
}

var validPgvectorTableName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,54}$`)
var validKnowledgeBackendKind = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{0,62}$`)

func (v *KnowledgeBaseClassValidator) Validate(_ context.Context, req *Request) *Response {
	var kbc v1alpha1.KnowledgeBaseClass
	if err := json.Unmarshal(req.Object, &kbc); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid knowledgeBaseClass JSON: %v", err)}
	}

	var errs []string
	if reference := kbc.Spec.BackendRef; reference != nil {
		var raw struct {
			Spec struct {
				BackendRef json.RawMessage `json:"backendRef"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(req.Object, &raw); err != nil {
			return &Response{Allowed: false, Reason: "invalid knowledgeBaseClass JSON"}
		}
		decoder := json.NewDecoder(bytes.NewReader(raw.Spec.BackendRef))
		decoder.DisallowUnknownFields()
		var checked v1alpha1.KnowledgeBackendReference
		if err := decoder.Decode(&checked); err != nil {
			errs = append(errs, "spec.backendRef permits only apiGroup, kind, and name; configuration must be in the consuming KnowledgeBase namespace")
		}
		if len(validation.IsDNS1123Subdomain(reference.APIGroup)) != 0 {
			errs = append(errs, "spec.backendRef.apiGroup must be a DNS subdomain")
		}
		if !validKnowledgeBackendKind.MatchString(reference.Kind) {
			errs = append(errs, "spec.backendRef.kind must be an alphanumeric kind starting with an uppercase letter (maximum 63 characters)")
		}
		if len(validation.IsDNS1123Subdomain(reference.Name)) != 0 {
			errs = append(errs, "spec.backendRef.name must be a DNS subdomain")
		}
		if kbc.Spec.VectorStore != nil {
			errs = append(errs, "spec.backendRef and spec.vectorStore are mutually exclusive")
		}
	}

	if strings.TrimSpace(kbc.Spec.Provisioner) == "" {
		errs = append(errs, "spec.provisioner is required")
	}

	if kbc.Spec.IndexMode != "" {
		if _, ok := validKBIndexModes[kbc.Spec.IndexMode]; !ok {
			errs = append(errs, fmt.Sprintf("spec.indexMode %q is invalid (must be one of dense, sparse, hybrid, inline)", kbc.Spec.IndexMode))
		}
	}

	if kbc.Spec.ReclaimPolicy != "" {
		if _, ok := validKBReclaimPolicies[kbc.Spec.ReclaimPolicy]; !ok {
			errs = append(errs, fmt.Sprintf("spec.reclaimPolicy %q is invalid (must be Delete, Retain, or Archive)", kbc.Spec.ReclaimPolicy))
		}
	}

	if cs := kbc.Spec.ChunkStrategy; cs != nil {
		if cs.Size < 0 {
			errs = append(errs, "spec.chunkStrategy.size must be >= 0")
		}
		if cs.Overlap < 0 {
			errs = append(errs, "spec.chunkStrategy.overlap must be >= 0")
		}
		if cs.Size > 0 && cs.Overlap >= cs.Size {
			errs = append(errs, fmt.Sprintf("spec.chunkStrategy.overlap (%d) must be less than spec.chunkStrategy.size (%d)", cs.Overlap, cs.Size))
		}
		if cs.Mode != "" {
			if _, ok := validKBChunkModes[cs.Mode]; !ok {
				errs = append(errs, fmt.Sprintf("spec.chunkStrategy.mode %q is invalid (must be fixed, sentence, or recursive)", cs.Mode))
			}
		}
	}

	if kbc.Spec.EmbeddingModelRef != nil && strings.TrimSpace(kbc.Spec.EmbeddingModelRef.Name) == "" {
		errs = append(errs, "spec.embeddingModelRef.name is required when embeddingModelRef is set")
	}

	if vs := kbc.Spec.VectorStore; vs != nil {
		switch strings.TrimSpace(vs.Type) {
		case "":
			errs = append(errs, "spec.vectorStore.type is required")
		case "pgvector":
			if vs.Pgvector == nil {
				errs = append(errs, "spec.vectorStore.pgvector is required when spec.vectorStore.type is \"pgvector\"")
			} else {
				if strings.TrimSpace(vs.Pgvector.ConnectionSecretRef.Name) == "" {
					errs = append(errs, "spec.vectorStore.pgvector.connectionSecretRef.name is required")
				}
				if vs.Pgvector.Dimension < 0 {
					errs = append(errs, "spec.vectorStore.pgvector.dimension must be >= 0")
				}
				if table := vs.Pgvector.Table; table != "" && !validPgvectorTableName.MatchString(table) {
					errs = append(errs, "spec.vectorStore.pgvector.table must match [a-z_][a-z0-9_]{0,54}")
				}
			}
		default:
			errs = append(errs, fmt.Sprintf("spec.vectorStore.type %q is invalid (only \"pgvector\" is currently supported)", vs.Type))
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}
	return &Response{Allowed: true}
}
