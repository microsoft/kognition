// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

const InputMaxSize = 900 * 1024

type EnvVar struct {
	Name  string
	Value string
}

type ConfigMapResolver func(ctx context.Context, namespace, name string) (map[string]string, error)

type PayloadBuilder struct {
	store            store.Store
	resolveConfigMap ConfigMapResolver
}

func NewPayloadBuilder(st store.Store, resolveConfigMap ConfigMapResolver) *PayloadBuilder {
	return &PayloadBuilder{store: st, resolveConfigMap: resolveConfigMap}
}

func (b *PayloadBuilder) Build(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass) (map[string]any, []EnvVar, error) {
	if kbc.Spec.BackendRef != nil {
		return nil, nil, fmt.Errorf("kbc %q requires a registered knowledge provider; backend references are not supported by the legacy pgvector ingester", kbc.ObjectMeta.Name)
	}
	if kbc.Spec.VectorStore == nil || kbc.Spec.VectorStore.Type != "pgvector" || kbc.Spec.VectorStore.Pgvector == nil {
		return nil, nil, fmt.Errorf("kbc %q vectorStore is not pgvector", kbc.ObjectMeta.Name)
	}
	pg := kbc.Spec.VectorStore.Pgvector
	dsn, err := b.ReadSecretRefKey(ctx, kb.ObjectMeta.Namespace, pg.ConnectionSecretRef.Name, "connectionString")
	if err != nil {
		return nil, nil, fmt.Errorf("resolve pgvector DSN: %w", err)
	}

	dimension := int(pg.Dimension)
	if dimension == 0 {
		dimension = 1536
	}
	table := pg.Table
	if table == "" {
		table = "kognition_kb_chunks"
	}
	payload, environment, err := b.buildPreparation(ctx, kb, kbc, false)
	if err != nil {
		return nil, nil, err
	}
	payload["vectorStore"] = map[string]any{
		"dsn":       dsn,
		"table":     table,
		"dimension": dimension,
	}
	return payload, environment, nil
}

func (b *PayloadBuilder) BuildProvider(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass, dimension int) (map[string]any, []EnvVar, error) {
	if kbc.Spec.BackendRef == nil || kbc.Spec.VectorStore != nil {
		return nil, nil, fmt.Errorf("kbc %q must configure exactly one registered knowledge provider backend", kbc.ObjectMeta.Name)
	}
	if dimension < 1 || dimension > 4096 {
		return nil, nil, fmt.Errorf("knowledge provider embedding dimension must be between 1 and 4096")
	}
	payload, environment, err := b.buildPreparation(ctx, kb, kbc, true)
	if err != nil {
		return nil, nil, err
	}
	payload["providerIndex"] = map[string]any{"dimension": dimension}
	return payload, environment, nil
}

func (b *PayloadBuilder) buildPreparation(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass, captureBinding bool) (map[string]any, []EnvVar, error) {

	chunking := map[string]any{"size": 512, "overlap": 64, "mode": "recursive"}
	if kbc.Spec.ChunkStrategy != nil {
		if kbc.Spec.ChunkStrategy.Size > 0 {
			chunking["size"] = int(kbc.Spec.ChunkStrategy.Size)
		}
		if kbc.Spec.ChunkStrategy.Overlap > 0 {
			chunking["overlap"] = int(kbc.Spec.ChunkStrategy.Overlap)
		}
		if kbc.Spec.ChunkStrategy.Mode != "" {
			chunking["mode"] = kbc.Spec.ChunkStrategy.Mode
		}
	}
	if kb.Spec.IndexConfig != nil {
		if kb.Spec.IndexConfig.ChunkSize > 0 {
			chunking["size"] = int(kb.Spec.IndexConfig.ChunkSize)
		}
		if kb.Spec.IndexConfig.ChunkOverlap > 0 {
			chunking["overlap"] = int(kb.Spec.IndexConfig.ChunkOverlap)
		}
	}

	var modelName string
	if kb.Spec.IndexConfig != nil && kb.Spec.IndexConfig.EmbeddingModelRef != nil {
		modelName = kb.Spec.IndexConfig.EmbeddingModelRef.Name
	} else if kbc.Spec.EmbeddingModelRef != nil {
		modelName = kbc.Spec.EmbeddingModelRef.Name
	}
	if modelName == "" {
		return nil, nil, fmt.Errorf("no embedding model configured on KnowledgeBaseClass %q or KB %q", kbc.ObjectMeta.Name, kb.ObjectMeta.Name)
	}
	embedding, environment, err := b.buildEmbeddingBlock(ctx, kb.ObjectMeta.Namespace, modelName, captureBinding)
	if err != nil {
		return nil, nil, err
	}
	sources, err := b.BuildSourcesBlock(ctx, kb)
	if err != nil {
		return nil, nil, err
	}

	return map[string]any{
		"kb": map[string]any{
			"namespace": kb.ObjectMeta.Namespace,
			"name":      kb.ObjectMeta.Name,
		},
		"chunking":  chunking,
		"embedding": embedding,
		"sources":   sources,
	}, environment, nil
}

func (b *PayloadBuilder) BuildEmbeddingBlock(ctx context.Context, namespace, modelName string) (map[string]any, []EnvVar, error) {
	return b.buildEmbeddingBlock(ctx, namespace, modelName, false)
}

func (b *PayloadBuilder) buildEmbeddingBlock(ctx context.Context, namespace, modelName string, captureBinding bool) (map[string]any, []EnvVar, error) {
	model, err := getStoreResource[api.Model](ctx, b.store, "Model", namespace, modelName)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve Model %q: %w", modelName, err)
	}
	block := map[string]any{
		"model":   model.Spec.ModelID,
		"baseUrl": model.Spec.Endpoint,
	}
	if captureBinding {
		binding, err := runtimeauth.CaptureModelBinding(ctx, b.store, model)
		if err != nil {
			return nil, nil, fmt.Errorf("capture embedding Model binding: %w", err)
		}
		block["modelBinding"] = binding
	}
	switch model.Spec.Provider {
	case "deterministic":
		block["provider"] = "deterministic"
	case "azure-openai", "foundry":
		block["provider"] = "azure_openai"
		if value := model.Spec.Parameters["apiVersion"]; value != "" {
			block["apiVersion"] = value
		}
	case "openai-compatible", "openai", "":
		block["provider"] = "openai"
	default:
		return nil, nil, fmt.Errorf("unsupported model provider %q for embeddings", model.Spec.Provider)
	}
	if model.Spec.InvocationProfileRef != nil {
		profile, err := modelinvocation.Load(ctx, b.store, model)
		if err != nil {
			return nil, nil, err
		}
		contract, err := openai.InvocationContract(model.Spec.InvocationContract)
		if err != nil || contract.Operation != "embedding" || (block["provider"] != "openai" && block["provider"] != "azure_openai") {
			return nil, nil, fmt.Errorf("embedding backend does not support invocation contract %q", model.Spec.InvocationContract)
		}
		if err := profile.ValidateContract(contract); err != nil {
			return nil, nil, err
		}
		parameters, err := profile.Prepare(contract, model.Spec.Parameters, false, false, 0)
		if err != nil {
			return nil, nil, err
		}
		if value := parameters["dimensions"]; value != "" {
			dimensions, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid resolved embedding dimension: %w", err)
			}
			block["dimensions"] = dimensions
		}
		block["invocationProfileBinding"] = profile.Binding
	}
	if model.Spec.CredentialRef == nil || model.Spec.CredentialRef.Name == "" {
		return block, nil, nil
	}
	values, err := b.ReadSecretRefAll(ctx, namespace, model.Spec.CredentialRef.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve Model %q credentials: %w", modelName, err)
	}
	if values["type"] == "azure-identity" || values["credentialType"] != "" {
		credentialType := values["credentialType"]
		if credentialType == "" {
			credentialType = "client-secret"
		}
		switch credentialType {
		case "client-secret":
			if values["tenantId"] == "" || values["clientId"] == "" || values["clientSecret"] == "" {
				return nil, nil, fmt.Errorf("azure client-secret credential for Model %q requires tenantId, clientId, and clientSecret", modelName)
			}
			return block, []EnvVar{
				{Name: "AZURE_CREDENTIAL_TYPE", Value: "client-secret"},
				{Name: "AZURE_TENANT_ID", Value: values["tenantId"]},
				{Name: "AZURE_CLIENT_ID", Value: values["clientId"]},
				{Name: "AZURE_CLIENT_SECRET", Value: values["clientSecret"]},
			}, nil
		case "workload-identity":
			environment := []EnvVar{{Name: "AZURE_CREDENTIAL_TYPE", Value: "workload-identity"}}
			if value := values["tenantId"]; value != "" {
				environment = append(environment, EnvVar{Name: "AZURE_TENANT_ID", Value: value})
			}
			if value := values["clientId"]; value != "" {
				environment = append(environment, EnvVar{Name: "AZURE_CLIENT_ID", Value: value})
			}
			return block, environment, nil
		case "managed-identity":
			environment := []EnvVar{{Name: "AZURE_CREDENTIAL_TYPE", Value: "managed-identity"}}
			if value := values["clientId"]; value != "" {
				environment = append(environment, EnvVar{Name: "AZURE_CLIENT_ID", Value: value})
			}
			return block, environment, nil
		case "default":
			environment := []EnvVar{{Name: "AZURE_CREDENTIAL_TYPE", Value: "default"}}
			if value := values["tenantId"]; value != "" {
				environment = append(environment, EnvVar{Name: "AZURE_TENANT_ID", Value: value})
			}
			if value := values["clientId"]; value != "" {
				environment = append(environment, EnvVar{Name: "AZURE_CLIENT_ID", Value: value})
			}
			return block, environment, nil
		default:
			return nil, nil, fmt.Errorf("unsupported azure-identity credentialType %q for Model %q", credentialType, modelName)
		}
	}
	if value := values["apiKey"]; value != "" {
		block["apiKey"] = value
		return block, nil, nil
	}
	return nil, nil, fmt.Errorf("SecretRef %q has neither azure-identity (credentialType client-secret/workload-identity/managed-identity/default) nor apiKey", model.Spec.CredentialRef.Name)
}

func (b *PayloadBuilder) BuildSourcesBlock(ctx context.Context, kb *api.KnowledgeBase) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(kb.Spec.Sources))
	for index, source := range kb.Spec.Sources {
		switch source.Type {
		case "inline":
			result = append(result, map[string]any{"type": "inline", "data": source.Data})
		case "url":
			result = append(result, map[string]any{"type": "url", "uri": source.URI})
		case "blob":
			if source.Blob == nil {
				return nil, fmt.Errorf("kb source %d: type=blob requires a blob block", index)
			}
			entry := map[string]any{
				"type":     "blob",
				"provider": source.Blob.Provider,
				"bucket":   source.Blob.Bucket,
				"prefix":   source.Blob.Prefix,
				"region":   source.Blob.Region,
				"endpoint": source.Blob.Endpoint,
				"include":  source.Blob.Include,
				"exclude":  source.Blob.Exclude,
			}
			if source.Blob.CredentialSecretRef != nil && source.Blob.CredentialSecretRef.Name != "" {
				credentials, err := b.ReadSecretRefAll(ctx, kb.ObjectMeta.Namespace, source.Blob.CredentialSecretRef.Name)
				if err != nil {
					return nil, fmt.Errorf("resolve blob credentials for source %d: %w", index, err)
				}
				delete(credentials, "type")
				entry["credentials"] = credentials
			}
			result = append(result, entry)
		case "configMap":
			if source.ConfigMap == nil || source.ConfigMap.Name == "" {
				return nil, fmt.Errorf("kb source %d: type=configMap requires configMap.name", index)
			}
			if b.resolveConfigMap == nil {
				return nil, fmt.Errorf("kb source %d: type=configMap is only available with a ConfigMap resolver", index)
			}
			data, err := b.resolveConfigMap(ctx, kb.ObjectMeta.Namespace, source.ConfigMap.Name)
			if err != nil {
				return nil, fmt.Errorf("resolve configMap %q for source %d: %w", source.ConfigMap.Name, index, err)
			}
			keys := make([]string, 0, len(data))
			for key := range data {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				result = append(result, map[string]any{
					"type": "inline",
					"data": data[key],
					"uri":  fmt.Sprintf("configmap://%s/%s", source.ConfigMap.Name, key),
				})
			}
		case "git":
			if source.Git == nil || source.Git.URL == "" {
				return nil, fmt.Errorf("kb source %d: type=git requires git.url", index)
			}
			branch := source.Git.Branch
			if branch == "" {
				branch = "main"
			}
			result = append(result, map[string]any{
				"type":   "git",
				"url":    source.Git.URL,
				"branch": branch,
				"path":   source.Git.Path,
			})
		case "file", "directory":
			return nil, fmt.Errorf("kb source %d: type %q is not yet supported by the ingester", index, source.Type)
		default:
			return nil, fmt.Errorf("kb source %d: unknown type %q", index, source.Type)
		}
	}
	return result, nil
}

func (b *PayloadBuilder) ReadSecretRefKey(ctx context.Context, namespace, name, key string) (string, error) {
	values, err := b.ReadSecretRefAll(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	value, ok := values[key]
	if !ok || value == "" {
		return "", fmt.Errorf("secretRef %q has no %q key", name, key)
	}
	return value, nil
}

func (b *PayloadBuilder) ReadSecretRefAll(ctx context.Context, namespace, name string) (map[string]string, error) {
	secretRef, err := getStoreResource[api.SecretRef](ctx, b.store, "SecretRef", namespace, name)
	if err != nil {
		return nil, fmt.Errorf("get SecretRef %s/%s: %w", namespace, name, err)
	}
	if secretRef.Spec.ResolvedBackendType() == "env" {
		environmentName := secretRef.Spec.ResolvedBackendRef()
		raw, ok := os.LookupEnv(environmentName)
		if !ok || raw == "" {
			return nil, fmt.Errorf("SecretRef %s/%s environment variable %q is not set", namespace, name, environmentName)
		}
		values := map[string]string{}
		if json.Unmarshal([]byte(raw), &values) == nil {
			return values, nil
		}
		return map[string]string{secretRef.Spec.ResolvedKey(): raw}, nil
	}
	if secretRef.ObjectMeta.Annotations == nil {
		return nil, fmt.Errorf("SecretRef %s/%s with backend %q has no annotations", namespace, name, secretRef.Spec.ResolvedBackendType())
	}
	raw := secretRef.ObjectMeta.Annotations["kognition.dev/internal-secret-data"]
	if raw == "" {
		return nil, fmt.Errorf("SecretRef %s/%s missing internal-secret-data", namespace, name)
	}
	values := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("parse SecretRef %s/%s data: %w", namespace, name, err)
	}
	return values, nil
}

func getStoreResource[T any](ctx context.Context, st store.Store, kind, namespace, name string) (*T, error) {
	raw, err := st.Get(ctx, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	var resource T
	if err := json.Unmarshal(raw, &resource); err != nil {
		return nil, fmt.Errorf("decode %s %s/%s: %w", kind, namespace, name, err)
	}
	return &resource, nil
}
