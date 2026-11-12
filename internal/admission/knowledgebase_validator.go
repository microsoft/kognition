// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// KnowledgeBaseValidator validates KnowledgeBase resources on CREATE and UPDATE.
type KnowledgeBaseValidator struct{}

// NewKnowledgeBaseValidator creates a new KnowledgeBaseValidator.
func NewKnowledgeBaseValidator() *KnowledgeBaseValidator {
	return &KnowledgeBaseValidator{}
}

func (v *KnowledgeBaseValidator) Name() string { return "knowledgebase-validator" }

func (v *KnowledgeBaseValidator) Handles(kind string, op Operation) bool {
	return kind == "KnowledgeBase" && (op == OperationCreate || op == OperationUpdate)
}

var validKBSourceTypes = map[string]struct{}{
	"inline":    {},
	"url":       {},
	"file":      {},
	"directory": {},
	"git":       {},
	"configMap": {},
	"blob":      {},
}

var validKBBlobProviders = map[string]struct{}{
	"azure": {},
	"s3":    {},
}

var validKBRefreshModes = map[string]struct{}{
	"periodic": {},
	"onDemand": {},
	"webhook":  {},
}

// maxInlineSourceBytes caps the size of inline `data` per source. Inline KB
// content is forwarded to runtime pods through the KOGNITION_KB_RESOLVED env
// var; oversized values risk exceeding Kubernetes pod-spec size limits.
const maxInlineSourceBytes = 256 * 1024 // 256 KiB per source

func (v *KnowledgeBaseValidator) Validate(_ context.Context, req *Request) *Response {
	var kb v1alpha1.KnowledgeBase
	if err := json.Unmarshal(req.Object, &kb); err != nil {
		return &Response{Allowed: false, Reason: fmt.Sprintf("invalid knowledgeBase JSON: %v", err)}
	}

	var errs []string

	if strings.TrimSpace(kb.Spec.KnowledgeBaseClassName) == "" {
		errs = append(errs, "spec.knowledgeBaseClassName is required")
	}

	if len(kb.Spec.Sources) == 0 {
		errs = append(errs, "spec.sources must contain at least one source")
	}

	for i, src := range kb.Spec.Sources {
		path := fmt.Sprintf("spec.sources[%d]", i)
		if src.Type == "" {
			errs = append(errs, fmt.Sprintf("%s.type is required", path))
			continue
		}
		if _, ok := validKBSourceTypes[src.Type]; !ok {
			errs = append(errs, fmt.Sprintf("%s.type %q is invalid (must be inline, url, file, directory, git, configMap, or blob)", path, src.Type))
			continue
		}

		switch src.Type {
		case "inline":
			if src.Data == "" {
				errs = append(errs, fmt.Sprintf("%s.data is required when type=inline", path))
			} else if len(src.Data) > maxInlineSourceBytes {
				errs = append(errs, fmt.Sprintf("%s.data is %d bytes; inline sources are limited to %d bytes", path, len(src.Data), maxInlineSourceBytes))
			}
		case "url":
			if strings.TrimSpace(src.URI) == "" {
				errs = append(errs, fmt.Sprintf("%s.uri is required when type=url", path))
			} else if !(strings.HasPrefix(src.URI, "http://") || strings.HasPrefix(src.URI, "https://")) {
				errs = append(errs, fmt.Sprintf("%s.uri %q must start with http:// or https://", path, src.URI))
			}
		case "file":
			if strings.TrimSpace(src.URI) == "" {
				errs = append(errs, fmt.Sprintf("%s.uri is required when type=file", path))
			}
		case "directory":
			if strings.TrimSpace(src.Directory) == "" {
				errs = append(errs, fmt.Sprintf("%s.directory is required when type=directory", path))
			}
		case "git":
			if src.Git == nil || strings.TrimSpace(src.Git.URL) == "" {
				errs = append(errs, fmt.Sprintf("%s.git.url is required when type=git", path))
			} else {
				u := src.Git.URL
				if !(strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") ||
					strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git@")) {
					errs = append(errs, fmt.Sprintf("%s.git.url %q must start with https://, http://, ssh://, or git@", path, u))
				}
			}
		case "configMap":
			if src.ConfigMap == nil || strings.TrimSpace(src.ConfigMap.Name) == "" {
				errs = append(errs, fmt.Sprintf("%s.configMap.name is required when type=configMap", path))
			}
		case "blob":
			errs = append(errs, validateKBBlobSource(path, src.Blob)...)
		}
	}

	if ic := kb.Spec.IndexConfig; ic != nil {
		if ic.ChunkSize < 0 {
			errs = append(errs, "spec.indexConfig.chunkSize must be >= 0")
		}
		if ic.ChunkOverlap < 0 {
			errs = append(errs, "spec.indexConfig.chunkOverlap must be >= 0")
		}
		if ic.ChunkSize > 0 && ic.ChunkOverlap >= ic.ChunkSize {
			errs = append(errs, fmt.Sprintf("spec.indexConfig.chunkOverlap (%d) must be less than spec.indexConfig.chunkSize (%d)", ic.ChunkOverlap, ic.ChunkSize))
		}
		if ic.EmbeddingModelRef != nil && strings.TrimSpace(ic.EmbeddingModelRef.Name) == "" {
			errs = append(errs, "spec.indexConfig.embeddingModelRef.name is required when embeddingModelRef is set")
		}
	}

	if rp := kb.Spec.RefreshPolicy; rp != nil {
		if rp.Mode == "" {
			errs = append(errs, "spec.refreshPolicy.mode is required when refreshPolicy is set")
		} else if _, ok := validKBRefreshModes[rp.Mode]; !ok {
			errs = append(errs, fmt.Sprintf("spec.refreshPolicy.mode %q is invalid (must be periodic, onDemand, or webhook)", rp.Mode))
		}
		switch rp.Mode {
		case "periodic":
			if strings.TrimSpace(rp.Interval) == "" {
				errs = append(errs, "spec.refreshPolicy.interval is required when mode=periodic")
			} else if interval, err := time.ParseDuration(rp.Interval); err != nil {
				errs = append(errs, fmt.Sprintf("spec.refreshPolicy.interval %q is not a valid duration: %v", rp.Interval, err))
			} else if interval <= 0 {
				errs = append(errs, "spec.refreshPolicy.interval must be greater than zero when mode=periodic")
			}
		case "webhook":
			if strings.TrimSpace(rp.WebhookPath) == "" {
				errs = append(errs, "spec.refreshPolicy.webhookPath is required when mode=webhook")
			}
		}
	}

	if len(errs) > 0 {
		return &Response{Allowed: false, Reason: strings.Join(errs, "; ")}
	}
	return &Response{Allowed: true}
}

// validateKBBlobSource validates the Blob variant of a KBSource. Rules:
//   - blob block is required when type=blob
//   - provider must be "azure" or "s3"
//   - bucket is required
//   - s3 requires region OR endpoint (endpoint covers MinIO/R2/etc.)
//   - endpoint, when set, must be http:// or https://
//   - credentialSecretRef.name is required when the ref is present
//
// A missing credentialSecretRef is allowed at admission time — the
// provisioner may be configured to use workload identity (IRSA, Azure
// Workload Identity, IMDS). Enforcement of "creds required" is a
// provisioner-level policy.
func validateKBBlobSource(path string, blob *v1alpha1.KBBlobSource) []string {
	var errs []string
	if blob == nil {
		return []string{fmt.Sprintf("%s.blob is required when type=blob", path)}
	}
	if strings.TrimSpace(blob.Provider) == "" {
		errs = append(errs, fmt.Sprintf("%s.blob.provider is required (azure or s3)", path))
	} else if _, ok := validKBBlobProviders[blob.Provider]; !ok {
		errs = append(errs, fmt.Sprintf("%s.blob.provider %q is invalid (must be azure or s3)", path, blob.Provider))
	}
	if strings.TrimSpace(blob.Bucket) == "" {
		errs = append(errs, fmt.Sprintf("%s.blob.bucket is required", path))
	}
	if blob.Provider == "s3" && strings.TrimSpace(blob.Region) == "" && strings.TrimSpace(blob.Endpoint) == "" {
		errs = append(errs, fmt.Sprintf("%s.blob.region or %s.blob.endpoint is required for provider=s3", path, path))
	}
	if ep := strings.TrimSpace(blob.Endpoint); ep != "" &&
		!strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://") {
		errs = append(errs, fmt.Sprintf("%s.blob.endpoint %q must start with http:// or https://", path, ep))
	}
	if blob.CredentialSecretRef != nil && strings.TrimSpace(blob.CredentialSecretRef.Name) == "" {
		errs = append(errs, fmt.Sprintf("%s.blob.credentialSecretRef.name is required when credentialSecretRef is set", path))
	}
	return errs
}
