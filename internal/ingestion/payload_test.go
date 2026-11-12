// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestPayloadBuilderBuildsCompleteEnvelope(t *testing.T) {
	resources := newResourceStore()
	storeResource(t, resources, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"connectionString":"postgres://vector"}`,
		}},
	})
	storeResource(t, resources, "SecretRef", "team-a", "blob", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"s3","accessKeyId":"id","secretAccessKey":"secret"}`,
		}},
	})
	storeResource(t, resources, "Model", "team-a", "embed", &api.Model{
		Spec: api.ModelSpec{Provider: "deterministic", ModelID: "local-embed"},
	})

	builder := NewPayloadBuilder(resources, func(_ context.Context, namespace, name string) (map[string]string, error) {
		if namespace != "team-a" || name != "docs" {
			t.Fatalf("resolve ConfigMap = %s/%s", namespace, name)
		}
		return map[string]string{"b.md": "beta", "a.md": "alpha"}, nil
	})
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "handbook"},
		Spec: api.KnowledgeBaseSpec{
			IndexConfig: &api.KBIndexConfig{
				EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
				ChunkSize:         256,
				ChunkOverlap:      32,
			},
			Sources: []api.KBSource{
				{Type: "inline", Data: "inline text"},
				{Type: "url", URI: "https://example.test/docs"},
				{Type: "blob", Blob: &api.KBBlobSource{
					Provider: "s3", Bucket: "docs", Prefix: "current/", Region: "test-1",
					Endpoint: "https://s3.example.test", Include: []string{"*.md"}, Exclude: []string{"draft*"},
					CredentialSecretRef: &api.LocalObjectReference{Name: "blob"},
				}},
				{Type: "configMap", ConfigMap: &api.LocalObjectReference{Name: "docs"}},
				{Type: "git", Git: &api.KBGitSource{URL: "https://example.test/repo.git", Path: "docs"}},
			},
		},
	}
	class := &api.KnowledgeBaseClass{
		ObjectMeta: api.ObjectMeta{Name: "pgvector"},
		Spec: api.KnowledgeBaseClassSpec{
			EmbeddingModelRef: &api.LocalObjectReference{Name: "class-embed"},
			ChunkStrategy:     &api.KBChunkStrategy{Size: 512, Overlap: 64, Mode: "fixed"},
			VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
				ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"}, Table: "chunks", Dimension: 8,
			}},
		},
	}

	payload, environment, err := builder.Build(context.Background(), kb, class)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(environment) != 0 {
		t.Fatalf("environment = %v, want none", environment)
	}
	if got := payload["kb"]; !reflect.DeepEqual(got, map[string]any{"namespace": "team-a", "name": "handbook"}) {
		t.Fatalf("kb block = %#v", got)
	}
	vectorStore := payload["vectorStore"].(map[string]any)
	if vectorStore["dsn"] != "postgres://vector" || vectorStore["table"] != "chunks" || vectorStore["dimension"] != 8 {
		t.Fatalf("vector store = %#v", vectorStore)
	}
	chunking := payload["chunking"].(map[string]any)
	if chunking["size"] != 256 || chunking["overlap"] != 32 || chunking["mode"] != "fixed" {
		t.Fatalf("chunking = %#v", chunking)
	}
	embedding := payload["embedding"].(map[string]any)
	if embedding["provider"] != "deterministic" || embedding["model"] != "local-embed" {
		t.Fatalf("embedding = %#v", embedding)
	}
	sources := payload["sources"].([]map[string]any)
	if len(sources) != 6 {
		t.Fatalf("sources = %d, want 6: %#v", len(sources), sources)
	}
	if sources[3]["uri"] != "configmap://docs/a.md" || sources[4]["uri"] != "configmap://docs/b.md" {
		t.Fatalf("ConfigMap sources are not sorted: %#v", sources[3:5])
	}
	if sources[5]["branch"] != "main" {
		t.Fatalf("Git default branch = %#v", sources[5]["branch"])
	}
	credentials := sources[2]["credentials"].(map[string]string)
	if credentials["accessKeyId"] != "id" || credentials["secretAccessKey"] != "secret" {
		t.Fatalf("blob credentials = %#v", credentials)
	}
	if _, exists := credentials["type"]; exists {
		t.Fatalf("blob credential metadata leaked into payload: %#v", credentials)
	}
}

func TestPayloadBuilderAppliesDefaultsAndClassEmbedding(t *testing.T) {
	resources := newResourceStore()
	storeResource(t, resources, "SecretRef", "team-a", "pgvector", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
		"kognition.dev/internal-secret-data": `{"connectionString":"postgres://vector"}`,
	}}})
	storeResource(t, resources, "Model", "team-a", "embed", &api.Model{Spec: api.ModelSpec{Provider: "openai", ModelID: "embedding"}})
	builder := NewPayloadBuilder(resources, nil)
	payload, _, err := builder.Build(context.Background(), &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs"}}, &api.KnowledgeBaseClass{
		ObjectMeta: api.ObjectMeta{Name: "class"},
		Spec: api.KnowledgeBaseClassSpec{
			EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
			VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
				ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := payload["vectorStore"].(map[string]any); got["table"] != "kognition_kb_chunks" || got["dimension"] != 1536 {
		t.Fatalf("vector defaults = %#v", got)
	}
	if got := payload["chunking"].(map[string]any); got["size"] != 512 || got["overlap"] != 64 || got["mode"] != "recursive" {
		t.Fatalf("chunk defaults = %#v", got)
	}
}

func TestPayloadBuilderEmbeddingCredentialModes(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		parameters   map[string]string
		credentials  string
		wantProvider string
		wantEnv      []EnvVar
		wantAPIKey   string
		wantError    string
	}{
		{name: "API key", provider: "openai-compatible", credentials: `{"apiKey":"key"}`, wantProvider: "openai", wantAPIKey: "key"},
		{name: "client secret", provider: "foundry", parameters: map[string]string{"apiVersion": "v1"}, credentials: `{"type":"azure-identity","tenantId":"tenant","clientId":"client","clientSecret":"secret"}`, wantProvider: "azure_openai", wantEnv: []EnvVar{{"AZURE_CREDENTIAL_TYPE", "client-secret"}, {"AZURE_TENANT_ID", "tenant"}, {"AZURE_CLIENT_ID", "client"}, {"AZURE_CLIENT_SECRET", "secret"}}},
		{name: "workload identity", provider: "azure-openai", credentials: `{"credentialType":"workload-identity","tenantId":"tenant","clientId":"client"}`, wantProvider: "azure_openai", wantEnv: []EnvVar{{"AZURE_CREDENTIAL_TYPE", "workload-identity"}, {"AZURE_TENANT_ID", "tenant"}, {"AZURE_CLIENT_ID", "client"}}},
		{name: "managed identity", provider: "foundry", credentials: `{"credentialType":"managed-identity","clientId":"client"}`, wantProvider: "azure_openai", wantEnv: []EnvVar{{"AZURE_CREDENTIAL_TYPE", "managed-identity"}, {"AZURE_CLIENT_ID", "client"}}},
		{name: "default identity", provider: "foundry", credentials: `{"credentialType":"default","tenantId":"tenant","clientId":"client"}`, wantProvider: "azure_openai", wantEnv: []EnvVar{{"AZURE_CREDENTIAL_TYPE", "default"}, {"AZURE_TENANT_ID", "tenant"}, {"AZURE_CLIENT_ID", "client"}}},
		{name: "incomplete client secret", provider: "foundry", credentials: `{"type":"azure-identity","tenantId":"tenant"}`, wantError: "requires tenantId, clientId, and clientSecret"},
		{name: "unsupported identity", provider: "foundry", credentials: `{"credentialType":"certificate"}`, wantError: "unsupported azure-identity credentialType"},
		{name: "missing credential value", provider: "openai", credentials: `{"other":"value"}`, wantError: "neither azure-identity"},
		{name: "unsupported provider", provider: "unknown", credentials: `{"apiKey":"key"}`, wantError: "unsupported model provider"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resources := newResourceStore()
			storeResource(t, resources, "Model", "team-a", "embed", &api.Model{Spec: api.ModelSpec{
				Provider: test.provider, ModelID: "model", Endpoint: "https://example.test", Parameters: test.parameters,
				CredentialRef: &api.LocalObjectReference{Name: "credential"},
			}})
			storeResource(t, resources, "SecretRef", "team-a", "credential", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
				"kognition.dev/internal-secret-data": test.credentials,
			}}})
			block, environment, err := NewPayloadBuilder(resources, nil).BuildEmbeddingBlock(context.Background(), "team-a", "embed")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildEmbeddingBlock: %v", err)
			}
			if block["provider"] != test.wantProvider || block["model"] != "model" || block["baseUrl"] != "https://example.test" {
				t.Fatalf("embedding block = %#v", block)
			}
			if test.parameters["apiVersion"] != "" && block["apiVersion"] != test.parameters["apiVersion"] {
				t.Fatalf("apiVersion = %#v", block["apiVersion"])
			}
			if !reflect.DeepEqual(environment, test.wantEnv) {
				t.Fatalf("environment = %#v, want %#v", environment, test.wantEnv)
			}
			if test.wantAPIKey != "" && block["apiKey"] != test.wantAPIKey {
				t.Fatalf("apiKey = %#v", block["apiKey"])
			}
		})
	}
}

func TestPayloadBuilderRejectsInvalidContracts(t *testing.T) {
	baseKB := api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs"}}
	resolverError := errors.New("ConfigMap unavailable")
	tests := []struct {
		name      string
		source    api.KBSource
		resolver  ConfigMapResolver
		wantError string
	}{
		{name: "blob block", source: api.KBSource{Type: "blob"}, wantError: "requires a blob block"},
		{name: "ConfigMap name", source: api.KBSource{Type: "configMap", ConfigMap: &api.LocalObjectReference{}}, wantError: "requires configMap.name"},
		{name: "ConfigMap resolver", source: api.KBSource{Type: "configMap", ConfigMap: &api.LocalObjectReference{Name: "docs"}}, wantError: "only available with a ConfigMap resolver"},
		{name: "ConfigMap read", source: api.KBSource{Type: "configMap", ConfigMap: &api.LocalObjectReference{Name: "docs"}}, resolver: func(context.Context, string, string) (map[string]string, error) { return nil, resolverError }, wantError: "ConfigMap unavailable"},
		{name: "Git URL", source: api.KBSource{Type: "git", Git: &api.KBGitSource{}}, wantError: "requires git.url"},
		{name: "file", source: api.KBSource{Type: "file"}, wantError: "not yet supported"},
		{name: "directory", source: api.KBSource{Type: "directory"}, wantError: "not yet supported"},
		{name: "unknown", source: api.KBSource{Type: "database"}, wantError: "unknown type"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kb := baseKB
			kb.Spec.Sources = []api.KBSource{test.source}
			_, err := NewPayloadBuilder(newResourceStore(), test.resolver).BuildSourcesBlock(context.Background(), &kb)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}

	resources := newResourceStore()
	builder := NewPayloadBuilder(resources, nil)
	if _, _, err := builder.Build(context.Background(), &baseKB, &api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "invalid"}}); err == nil || !strings.Contains(err.Error(), "not pgvector") {
		t.Fatalf("invalid vector store error = %v", err)
	}
	class := &api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "class"}, Spec: api.KnowledgeBaseClassSpec{VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{ConnectionSecretRef: api.LocalObjectReference{Name: "missing"}}}}}
	if _, _, err := builder.Build(context.Background(), &baseKB, class); err == nil || !strings.Contains(err.Error(), "resolve pgvector DSN") {
		t.Fatalf("missing DSN error = %v", err)
	}
}

func TestPayloadBuilderRejectsBackendReferenceBeforeCredentials(t *testing.T) {
	for _, legacyConfigured := range []bool{false, true} {
		class := &api.KnowledgeBaseClass{
			ObjectMeta: api.ObjectMeta{Name: "provider"},
			Spec: api.KnowledgeBaseClassSpec{BackendRef: &api.KnowledgeBackendReference{
				APIGroup: "pgvector.knowledge.kognition.dev", Kind: "PgvectorBackend", Name: "shared",
			}},
		}
		if legacyConfigured {
			class.Spec.VectorStore = &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
				ConnectionSecretRef: api.LocalObjectReference{Name: "must-not-read"},
			}}
		}
		payload, environment, err := NewPayloadBuilder(nil, nil).Build(context.Background(), &api.KnowledgeBase{}, class)
		if err == nil || !strings.Contains(err.Error(), "registered knowledge provider") || payload != nil || environment != nil {
			t.Fatalf("legacy configured %t: expected rejection before credential access, got %v, %v, %v", legacyConfigured, payload, environment, err)
		}
	}
}

func TestPayloadBuilderBuildsProviderPreparationWithoutBackendDetails(t *testing.T) {
	resources := newResourceStore()
	storeResource(t, resources, "Model", "team-a", "embed", &api.Model{
		ObjectMeta: api.ObjectMeta{Name: "embed", Namespace: "team-a", UID: "embed-uid", ResourceVersion: 1},
		Spec:       api.ModelSpec{Provider: "deterministic", ModelID: "test-embedding"},
	})
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"},
		Spec: api.KnowledgeBaseSpec{
			Sources: []api.KBSource{{Type: "inline", Data: "provider document"}},
		},
	}
	class := &api.KnowledgeBaseClass{
		ObjectMeta: api.ObjectMeta{Name: "provider"},
		Spec: api.KnowledgeBaseClassSpec{
			BackendRef:        &api.KnowledgeBackendReference{APIGroup: "azuresearch.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared"},
			EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
		},
	}

	payload, environment, err := NewPayloadBuilder(resources, nil).BuildProvider(context.Background(), kb, class, 1536)
	if err != nil {
		t.Fatalf("BuildProvider() error = %v", err)
	}
	providerIndex, ok := payload["providerIndex"].(map[string]any)
	if !ok || providerIndex["dimension"] != 1536 || len(environment) != 0 {
		t.Fatalf("provider preparation = %#v, env = %#v", payload, environment)
	}
	if _, found := payload["vectorStore"]; found {
		t.Fatalf("provider preparation leaked vectorStore: %#v", payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"azuresearch", "AzureSearchBackend", "shared"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("provider preparation leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestPayloadBuilderSecretRefErrors(t *testing.T) {
	tests := []struct {
		name      string
		secretRef *api.SecretRef
		envName   string
		envValue  *string
		wantError string
	}{
		{name: "not found", wantError: "get SecretRef"},
		{name: "environment missing", secretRef: &api.SecretRef{Spec: api.SecretRefSpec{BackendType: "env", BackendRef: "MISSING_SECRET", Key: "value"}}, wantError: "is not set"},
		{name: "annotations missing", secretRef: &api.SecretRef{Spec: api.SecretRefSpec{BackendType: "internal"}}, wantError: "has no annotations"},
		{name: "data missing", secretRef: &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{"example.test/other": "value"}}, Spec: api.SecretRefSpec{BackendType: "internal"}}, wantError: "missing internal-secret-data"},
		{name: "malformed data", secretRef: &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": "{"}}, Spec: api.SecretRefSpec{BackendType: "internal"}}, wantError: "parse SecretRef"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resources := newResourceStore()
			if test.secretRef != nil {
				storeResource(t, resources, "SecretRef", "team-a", "credential", test.secretRef)
			}
			if test.envValue != nil {
				t.Setenv(test.envName, *test.envValue)
			}
			_, err := NewPayloadBuilder(resources, nil).ReadSecretRefAll(context.Background(), "team-a", "credential")
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}

	resources := newResourceStore()
	storeResource(t, resources, "SecretRef", "team-a", "credential", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
		"kognition.dev/internal-secret-data": `{"other":"value"}`,
	}}})
	if _, err := NewPayloadBuilder(resources, nil).ReadSecretRefKey(context.Background(), "team-a", "credential", "apiKey"); err == nil || !strings.Contains(err.Error(), `has no "apiKey" key`) {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestPayloadBuilderResolvesEnvironmentSecretRefs(t *testing.T) {
	secretRef := api.SecretRef{
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "pgvector"},
		Spec: api.SecretRefSpec{
			BackendType: "env",
			BackendRef:  "TEST_PGVECTOR_DSN",
			Key:         "connectionString",
		},
	}
	data, err := json.Marshal(secretRef)
	if err != nil {
		t.Fatalf("marshal SecretRef: %v", err)
	}
	var roundTrip api.SecretRef
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal SecretRef fixture: %v", err)
	}
	if got := roundTrip.Spec.ResolvedBackendType(); got != "env" {
		t.Fatalf("fixture backend type = %q; JSON = %s", got, data)
	}
	builder := NewPayloadBuilder(&singleResourceStore{data: data}, nil)

	t.Setenv("TEST_PGVECTOR_DSN", "postgres://database")
	values, err := builder.ReadSecretRefAll(context.Background(), "default", "pgvector")
	if err != nil {
		t.Fatalf("resolve scalar environment SecretRef: %v", err)
	}
	if values["connectionString"] != "postgres://database" {
		t.Fatalf("connectionString = %q", values["connectionString"])
	}

	t.Setenv("TEST_PGVECTOR_DSN", `{"clientId":"id","clientSecret":"secret"}`)
	values, err = builder.ReadSecretRefAll(context.Background(), "default", "pgvector")
	if err != nil {
		t.Fatalf("resolve JSON environment SecretRef: %v", err)
	}
	if values["clientId"] != "id" || values["clientSecret"] != "secret" {
		t.Fatalf("credential keys = %v", values)
	}
}

func TestPayloadBuilderSupportsDeterministicEmbeddingModel(t *testing.T) {
	model := api.Model{
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "local-embedding"},
		Spec: api.ModelSpec{
			Provider: "deterministic",
			ModelID:  "local",
		},
	}
	data, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("marshal Model: %v", err)
	}
	builder := NewPayloadBuilder(&singleResourceStore{data: data}, nil)
	block, environment, err := builder.BuildEmbeddingBlock(context.Background(), "default", "local-embedding")
	if err != nil {
		t.Fatalf("BuildEmbeddingBlock: %v", err)
	}
	if block["provider"] != "deterministic" || block["model"] != "local" {
		t.Fatalf("embedding block = %v", block)
	}
	if len(environment) != 0 {
		t.Fatalf("environment = %v, want empty", environment)
	}
}

type singleResourceStore struct{ data []byte }

func (s *singleResourceStore) Ping(context.Context) error { return nil }
func (s *singleResourceStore) Get(context.Context, string, string, string) ([]byte, error) {
	return s.data, nil
}
func (s *singleResourceStore) List(context.Context, string, store.ListOptions) ([][]byte, error) {
	return nil, nil
}
func (s *singleResourceStore) Create(context.Context, string, string, string, []byte) error {
	return nil
}
func (s *singleResourceStore) Update(context.Context, string, string, string, []byte, int64) error {
	return nil
}
func (s *singleResourceStore) Delete(context.Context, string, string, string) error { return nil }
func (s *singleResourceStore) Watch(context.Context, string, store.ListOptions) (<-chan store.WatchEvent, error) {
	return make(chan store.WatchEvent), nil
}
func (s *singleResourceStore) CreateRevision(context.Context, string, string, string, int64, []byte, []byte) error {
	return nil
}
func (s *singleResourceStore) ListRevisions(context.Context, string, string, string) ([]store.Revision, error) {
	return nil, nil
}
func (s *singleResourceStore) GetRevision(context.Context, string, string, string, int64) (*store.Revision, error) {
	return nil, store.ErrNotFound
}
func (s *singleResourceStore) AppendEvent(context.Context, string, string, string, store.InstanceEvent) error {
	return nil
}
func (s *singleResourceStore) ListEvents(context.Context, string, string, string) ([]store.InstanceEvent, error) {
	return nil, nil
}

type resourceStore struct {
	singleResourceStore
	resources map[string][]byte
}

func newResourceStore() *resourceStore {
	return &resourceStore{resources: map[string][]byte{}}
}

func (s *resourceStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	data, ok := s.resources[kind+"/"+namespace+"/"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func storeResource(t *testing.T, resources *resourceStore, kind, namespace, name string, object any) {
	t.Helper()
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal %s %s/%s: %v", kind, namespace, name, err)
	}
	resources.resources[kind+"/"+namespace+"/"+name] = data
}
