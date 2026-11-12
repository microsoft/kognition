// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

type foreignJobRaceClient struct {
	client.Client
	foreign *batchv1.Job
}

type faultingKBClient struct {
	client.Client
	getErrors    []error
	createErrors []error
	deleteErrors []error
	listErrors   []error
}

func (c *faultingKBClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if len(c.getErrors) > 0 {
		err := c.getErrors[0]
		c.getErrors = c.getErrors[1:]
		if err != nil {
			return err
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *faultingKBClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if len(c.createErrors) > 0 {
		err := c.createErrors[0]
		c.createErrors = c.createErrors[1:]
		if err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *faultingKBClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if len(c.deleteErrors) > 0 {
		err := c.deleteErrors[0]
		c.deleteErrors = c.deleteErrors[1:]
		if err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *faultingKBClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if len(c.listErrors) > 0 {
		err := c.listErrors[0]
		c.listErrors = c.listErrors[1:]
		if err != nil {
			return err
		}
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *foreignJobRaceClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*batchv1.Job); ok && c.foreign != nil {
		foreign := c.foreign
		c.foreign = nil
		if err := c.Client.Create(ctx, foreign, opts...); err != nil {
			return err
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

func TestEnsureInputSecretRejectsStaleOrForeignSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"}}
	desired := (&KBIngesterLauncher{}).buildInputSecret(kb, "kb-ingest-docs-1", []byte(`{"task":"current"}`), nil)
	stale := desired.DeepCopy()
	stale.Data[ingestInputKey] = []byte(`{"task":"stale"}`)
	launcher := &KBIngesterLauncher{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).Build()}

	err := launcher.ensureInputSecret(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ensure stale Secret error = %v; want mismatch", err)
	}

	foreign := desired.DeepCopy()
	foreign.OwnerReferences[0].UID = "other-kb-uid"
	launcher.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
	err = launcher.ensureInputSecret(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ensure foreign Secret error = %v; want mismatch", err)
	}
}

func TestEnsureInputSecretAcceptsMatchingSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"}}
	desired := (&KBIngesterLauncher{}).buildInputSecret(kb, "kb-ingest-docs-1", []byte(`{"task":"current"}`), nil)
	launcher := &KBIngesterLauncher{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(desired.DeepCopy()).Build()}

	if err := launcher.ensureInputSecret(context.Background(), desired); err != nil {
		t.Fatalf("ensure matching Secret: %v", err)
	}
}

func TestEnsureInputSecretPropagatesExistingSecretReadFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"}}
	desired := (&KBIngesterLauncher{}).buildInputSecret(kb, "kb-ingest-docs-1", []byte(`{"task":"current"}`), nil)
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desired.DeepCopy()).Build()
	boom := errors.New("kubernetes unavailable")
	launcher := &KBIngesterLauncher{client: &faultingKBClient{Client: base, getErrors: []error{boom}}}

	err := launcher.ensureInputSecret(context.Background(), desired)
	if err == nil || !strings.Contains(err.Error(), "get existing secret") || !errors.Is(err, boom) {
		t.Fatalf("ensureInputSecret() error = %v, want wrapped existing Secret read failure", err)
	}
}

func TestEnsureRunningProviderJobDoesNotReconstructMissingInput(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	launcher := &KBIngesterLauncher{client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"}}
	kbc := &api.KnowledgeBaseClass{Spec: api.KnowledgeBaseClassSpec{BackendRef: &api.KnowledgeBackendReference{
		APIGroup: "azuresearch.knowledge.kognition.dev", Kind: "AzureSearchBackend", Name: "shared",
	}}}

	err := launcher.ensureRunningJobInput(context.Background(), kb, kbc, "kb-ingest-docs-1", kbOwnerRef(kb))
	if err == nil || !strings.Contains(err.Error(), "cannot be reconstructed") {
		t.Fatalf("ensureRunningJobInput() error = %v, want immutable provider input failure", err)
	}
}

func TestKBIngesterJobReferencesSecretWithoutExposingValues(t *testing.T) {
	launcher := &KBIngesterLauncher{image: "example/ingester:test"}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"}}
	payload := []byte(`{"vectorStore":{"dsn":"postgres://secret"}}`)
	extraEnv := []corev1.EnvVar{{Name: "AZURE_CLIENT_SECRET", Value: "client-secret-value"}}

	secret := launcher.buildInputSecret(kb, "kb-ingest-docs-1", payload, extraEnv)
	job := launcher.buildJob(kb, "kb-ingest-docs-1", secret.Name, extraEnv)
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "" || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("ingester must not receive a Kubernetes API identity")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("ingester must use a restricted non-root Pod security context")
	}
	security := pod.Containers[0].SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatal("ingester must disable escalation and drop all capabilities")
	}

	if !bytes.Equal(secret.Data[ingestInputKey], payload) {
		t.Fatal("input Secret does not contain the ingestion payload")
	}
	if got := string(secret.Data["AZURE_CLIENT_SECRET"]); got != "client-secret-value" {
		t.Fatalf("Secret AZURE_CLIENT_SECRET = %q, want configured value", got)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != kb.ObjectMeta.Name {
		t.Fatal("input Secret must be owned by the KnowledgeBase")
	}

	container := job.Spec.Template.Spec.Containers[0]
	for _, env := range container.Env {
		if env.Value == string(payload) || env.Value == "client-secret-value" {
			t.Fatalf("Job exposes sensitive value in environment variable %q", env.Name)
		}
	}
	credential := findEnv(container.Env, "AZURE_CLIENT_SECRET")
	if credential == nil || credential.ValueFrom == nil || credential.ValueFrom.SecretKeyRef == nil {
		t.Fatal("AZURE_CLIENT_SECRET must reference the input Secret")
	}
	if credential.ValueFrom.SecretKeyRef.Name != secret.Name {
		t.Fatalf("credential Secret name = %q, want %q", credential.ValueFrom.SecretKeyRef.Name, secret.Name)
	}
	if len(job.Spec.Template.Spec.Volumes) != 2 || job.Spec.Template.Spec.Volumes[0].Secret == nil {
		t.Fatal("Job must project the input Secret as a volume")
	}
	volume := job.Spec.Template.Spec.Volumes[0].Secret
	if volume.SecretName != secret.Name || volume.DefaultMode == nil || *volume.DefaultMode != 0444 {
		t.Fatal("input Secret volume must be mounted from the expected Secret with mode 0444")
	}
	if len(container.VolumeMounts) != 2 || !container.VolumeMounts[0].ReadOnly || container.VolumeMounts[0].MountPath != ingestInputPath {
		t.Fatal("input Secret must be mounted read-only at the configured input path")
	}
}

func TestKBIngesterJobPrivateNetworkOptIn(t *testing.T) {
	launcher := &KBIngesterLauncher{image: "example/ingester:test"}
	defaultKB := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "public-docs"}}
	privateKB := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{
		Namespace: "team-a",
		Name:      "private-docs",
		Annotations: map[string]string{
			"kognition.dev/ingest-allow-private-networks": "true",
		},
	}}

	defaultJob := launcher.buildJob(defaultKB, "public-docs", "public-docs-input", nil)
	if env := findEnv(defaultJob.Spec.Template.Spec.Containers[0].Env, "KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS"); env != nil {
		t.Fatal("private network access must remain disabled by default")
	}

	privateJob := launcher.buildJob(privateKB, "private-docs", "private-docs-input", nil)
	env := findEnv(privateJob.Spec.Template.Spec.Containers[0].Env, "KOGNITION_INGEST_ALLOW_PRIVATE_NETWORKS")
	if env == nil || env.Value != "true" {
		t.Fatal("annotated KnowledgeBase must opt its ingester Job into private network access")
	}
}

func TestConfigureProviderJobUsesTrustedProviderAndIsolatesSpool(t *testing.T) {
	launcher := &KBIngesterLauncher{image: "example/ingester:test"}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "docs", UID: "kb-uid"}}
	job := launcher.buildJob(kb, "kb-ingest-docs-1", "kb-ingest-docs-1-input", nil)

	configureProviderJob(job, "kb-ingest-docs-1-input", "custom-provider")

	if security := job.Spec.Template.Spec.SecurityContext; security == nil || security.FSGroup == nil || *security.FSGroup != 1000 {
		t.Fatal("provider credential and spool volumes must be accessible to the ingester group")
	}
	for _, volume := range job.Spec.Template.Spec.Volumes {
		if volume.Name == "provider-config" && (volume.Secret == nil || volume.Secret.DefaultMode == nil || *volume.Secret.DefaultMode != 0440) {
			t.Fatal("provider credentials must be group-readable without world access")
		}
	}
	if job.Annotations[providerAnnotation] != "custom-provider" || job.Annotations[providerInputAnnotation] != "kb-ingest-docs-1-input" {
		t.Fatalf("provider annotations = %#v", job.Annotations)
	}
	container := job.Spec.Template.Spec.Containers[0]
	configFile := findEnv(container.Env, "KOGNITION_KNOWLEDGE_INDEX_CONFIG_FILE")
	spoolFile := findEnv(container.Env, "KOGNITION_KNOWLEDGE_INDEX_SPOOL_FILE")
	if configFile == nil || configFile.Value != providerConfigPath || spoolFile == nil || spoolFile.Value != providerSpoolPath {
		t.Fatalf("provider environment = %#v", container.Env)
	}
	if len(container.VolumeMounts) != 4 || !container.VolumeMounts[2].ReadOnly || container.VolumeMounts[3].ReadOnly {
		t.Fatalf("provider volume mounts = %#v", container.VolumeMounts)
	}
	if len(job.Spec.Template.Spec.Volumes) != 4 || job.Spec.Template.Spec.Volumes[2].Secret == nil || job.Spec.Template.Spec.Volumes[3].EmptyDir == nil {
		t.Fatalf("provider volumes = %#v", job.Spec.Template.Spec.Volumes)
	}
}

func TestBuildSourcesBlockNormalizesSupportedSources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "handbook", Namespace: "team-a"},
		Data:       map[string]string{"z.md": "last", "a.md": "first"},
	}
	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "blob-creds", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","accountName":"docs","accountKey":"secret"}`,
		}},
	})
	launcher := &KBIngesterLauncher{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build(),
		store:  store,
	}
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"},
		Spec: api.KnowledgeBaseSpec{Sources: []api.KBSource{
			{Type: "inline", Data: "inline document"},
			{Type: "url", URI: "https://example.test/docs"},
			{Type: "configMap", ConfigMap: &api.LocalObjectReference{Name: "handbook"}},
			{Type: "git", Git: &api.KBGitSource{URL: "https://example.test/repo.git", Path: "docs"}},
			{Type: "blob", Blob: &api.KBBlobSource{
				Provider: "azure", Bucket: "docs", CredentialSecretRef: &api.LocalObjectReference{Name: "blob-creds"},
			}},
		}},
	}

	sources, err := launcher.buildSourcesBlock(context.Background(), kb)
	if err != nil {
		t.Fatalf("buildSourcesBlock() error = %v", err)
	}
	if len(sources) != 6 {
		t.Fatalf("source count = %d, want 6", len(sources))
	}
	if sources[2]["uri"] != "configmap://handbook/a.md" || sources[3]["uri"] != "configmap://handbook/z.md" {
		t.Fatalf("ConfigMap sources are not sorted: %#v", sources[2:4])
	}
	if sources[4]["branch"] != "main" {
		t.Fatalf("default git branch = %#v, want main", sources[4]["branch"])
	}
	credentials, ok := sources[5]["credentials"].(map[string]string)
	if !ok || credentials["accountKey"] != "secret" {
		t.Fatalf("blob credentials = %#v", sources[5]["credentials"])
	}
	if _, exists := credentials["type"]; exists {
		t.Fatalf("blob credentials expose metadata type: %#v", credentials)
	}
}

func TestBuildSourcesBlockRejectsInvalidSources(t *testing.T) {
	launcher := &KBIngesterLauncher{}
	for _, source := range []api.KBSource{
		{Type: "file", URI: "/tmp/docs.txt"},
		{Type: "blob"},
		{Type: "git"},
		{Type: "configMap"},
		{Type: "unknown"},
	} {
		t.Run(source.Type, func(t *testing.T) {
			kb := &api.KnowledgeBase{Spec: api.KnowledgeBaseSpec{Sources: []api.KBSource{source}}}
			if _, err := launcher.buildSourcesBlock(context.Background(), kb); err == nil {
				t.Fatalf("buildSourcesBlock() error = nil for source %#v", source)
			}
		})
	}
}

func TestBuildEmbeddingBlockResolvesCredentialModes(t *testing.T) {
	store := newFakeStore()
	putStoreResource(t, store, "Model", "team-a", "openai-embed", &api.Model{
		Spec: api.ModelSpec{
			Provider: "openai", ModelID: "text-embedding-3-small",
			CredentialRef: &api.LocalObjectReference{Name: "openai-key"},
		},
	})
	putStoreResource(t, store, "SecretRef", "team-a", "openai-key", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","apiKey":"openai-secret"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "azure-embed", &api.Model{
		Spec: api.ModelSpec{
			Provider: "azure-openai", ModelID: "embedding-deployment", Endpoint: "https://example.openai.azure.com",
			Parameters:    map[string]string{"apiVersion": "2024-06-01"},
			CredentialRef: &api.LocalObjectReference{Name: "azure-identity"},
		},
	})
	putStoreResource(t, store, "SecretRef", "team-a", "azure-identity", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"azure-identity","credentialType":"client-secret","tenantId":"tenant","clientId":"client","clientSecret":"azure-secret"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "legacy-azure-embed", &api.Model{
		Spec: api.ModelSpec{
			Provider: "azure-openai", ModelID: "legacy-deployment",
			CredentialRef: &api.LocalObjectReference{Name: "legacy-azure-identity"},
		},
	})
	putStoreResource(t, store, "SecretRef", "team-a", "legacy-azure-identity", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"azure-identity","tenantId":"tenant","clientId":"client","clientSecret":"legacy-secret"}`,
		}},
	})
	launcher := &KBIngesterLauncher{store: store}

	openAI, env, err := launcher.buildEmbeddingBlock(context.Background(), "team-a", "openai-embed")
	if err != nil {
		t.Fatalf("build OpenAI embedding block: %v", err)
	}
	if openAI["provider"] != "openai" || openAI["apiKey"] != "openai-secret" || len(env) != 0 {
		t.Fatalf("OpenAI embedding = %#v, env = %#v", openAI, env)
	}

	azure, env, err := launcher.buildEmbeddingBlock(context.Background(), "team-a", "azure-embed")
	if err != nil {
		t.Fatalf("build Azure embedding block: %v", err)
	}
	if azure["provider"] != "azure_openai" || azure["apiVersion"] != "2024-06-01" {
		t.Fatalf("Azure embedding = %#v", azure)
	}
	if findEnv(env, "AZURE_CLIENT_SECRET") == nil || findEnv(env, "AZURE_CLIENT_SECRET").Value != "azure-secret" {
		t.Fatalf("Azure credential env = %#v", env)
	}

	_, env, err = launcher.buildEmbeddingBlock(context.Background(), "team-a", "legacy-azure-embed")
	if err != nil {
		t.Fatalf("build legacy Azure embedding block: %v", err)
	}
	if findEnv(env, "AZURE_CREDENTIAL_TYPE") == nil || findEnv(env, "AZURE_CREDENTIAL_TYPE").Value != "client-secret" {
		t.Fatalf("legacy Azure credential env = %#v", env)
	}
}

func TestBuildEmbeddingBlockRejectsIncompleteClientSecret(t *testing.T) {
	store := newFakeStore()
	putStoreResource(t, store, "Model", "team-a", "azure-embed", &api.Model{
		Spec: api.ModelSpec{
			Provider: "azure-openai", ModelID: "embedding-deployment",
			CredentialRef: &api.LocalObjectReference{Name: "incomplete-identity"},
		},
	})
	putStoreResource(t, store, "SecretRef", "team-a", "incomplete-identity", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"azure-identity","credentialType":"client-secret","clientId":"client","clientSecret":"secret"}`,
		}},
	})

	launcher := &KBIngesterLauncher{store: store}
	if _, _, err := launcher.buildEmbeddingBlock(context.Background(), "team-a", "azure-embed"); err == nil || !strings.Contains(err.Error(), "tenantId") {
		t.Fatalf("buildEmbeddingBlock() error = %v, want missing tenantId", err)
	}
}

func TestBuildEmbeddingBlockResolvesSecretlessAzureModes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		values     string
		wantEnv    map[string]string
		absentEnvs []string
	}{
		{
			name:       "workload identity uses projected IDs",
			values:     `{"type":"azure-identity","credentialType":"workload-identity"}`,
			wantEnv:    map[string]string{"AZURE_CREDENTIAL_TYPE": "workload-identity"},
			absentEnvs: []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID"},
		},
		{
			name:       "managed identity with explicit client",
			values:     `{"type":"azure-identity","credentialType":"managed-identity","clientId":"uami"}`,
			wantEnv:    map[string]string{"AZURE_CREDENTIAL_TYPE": "managed-identity", "AZURE_CLIENT_ID": "uami"},
			absentEnvs: []string{"AZURE_TENANT_ID", "AZURE_CLIENT_SECRET"},
		},
		{
			name:       "default credential without overrides",
			values:     `{"type":"azure-identity","credentialType":"default"}`,
			wantEnv:    map[string]string{"AZURE_CREDENTIAL_TYPE": "default"},
			absentEnvs: []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			putStoreResource(t, store, "Model", "team-a", "azure-embed", &api.Model{
				Spec: api.ModelSpec{
					Provider: "azure-openai", ModelID: "embedding-deployment",
					CredentialRef: &api.LocalObjectReference{Name: "azure-identity"},
				},
			})
			putStoreResource(t, store, "SecretRef", "team-a", "azure-identity", &api.SecretRef{
				ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
					"kognition.dev/internal-secret-data": tc.values,
				}},
			})

			launcher := &KBIngesterLauncher{store: store}
			_, env, err := launcher.buildEmbeddingBlock(context.Background(), "team-a", "azure-embed")
			if err != nil {
				t.Fatalf("buildEmbeddingBlock() error = %v", err)
			}
			for name, want := range tc.wantEnv {
				item := findEnv(env, name)
				if item == nil || item.Value != want {
					t.Fatalf("%s = %#v, want %q", name, item, want)
				}
			}
			for _, name := range tc.absentEnvs {
				if item := findEnv(env, name); item != nil {
					t.Fatalf("%s = %#v, want omitted", name, item)
				}
			}
		})
	}
}

func TestKBIngesterEnsureCreatesInputSecretAndJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","connectionString":"postgres://db/kognition"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "text-embedding-3-small"},
	})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	launcher := NewKBIngesterLauncher(fakeClient, store, zap.NewNop(), "ingester:v1", "ingester", "controllers")
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Spec: api.KnowledgeBaseSpec{
			Sources: []api.KBSource{{Type: "inline", Data: "document"}},
		},
	}
	kbc := &api.KnowledgeBaseClass{
		ObjectMeta: api.ObjectMeta{Name: "pgvector"},
		Spec: api.KnowledgeBaseClassSpec{
			EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
			VectorStore: &api.KBVectorStore{
				Type: "pgvector",
				Pgvector: &api.KBPgvectorStore{
					ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"},
				},
			},
		},
	}

	state, stats, err := launcher.Ensure(context.Background(), kb, kbc)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if state != intctrl.IngestRunning || stats != (intctrl.IngestStats{}) {
		t.Fatalf("Ensure() = (%v, %#v), want running with empty stats", state, stats)
	}
	jobName := ingestJobName("docs", 1)
	var job batchv1.Job
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: jobName}, &job); err != nil {
		t.Fatalf("get ingest Job: %v", err)
	}
	var secret corev1.Secret
	secretName := jobName + "-input"
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: secretName}, &secret); err != nil {
		t.Fatalf("get input Secret: %v", err)
	}
	if len(secret.Data[ingestInputKey]) == 0 {
		t.Fatal("input Secret has empty ingestion payload")
	}
	if job.Spec.Template.Spec.Containers[0].Image != "ingester:v1" {
		t.Fatalf("Job image = %q, want ingester:v1", job.Spec.Template.Spec.Containers[0].Image)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("ingestion root filesystem must be read-only")
	}
	temporaryWritable := false
	for _, mount := range container.VolumeMounts {
		if mount.MountPath == "/tmp" && !mount.ReadOnly {
			for _, volume := range job.Spec.Template.Spec.Volumes {
				if volume.Name == mount.Name && volume.EmptyDir != nil {
					temporaryWritable = true
				}
			}
		}
	}
	if !temporaryWritable {
		t.Fatal("ingestion requires a writable temporary volume with a read-only root")
	}
	if err := fakeClient.Delete(context.Background(), &secret); err != nil {
		t.Fatalf("delete input Secret: %v", err)
	}

	state, _, err = launcher.Ensure(context.Background(), kb, kbc)
	if err != nil || state != intctrl.IngestRunning {
		t.Fatalf("Ensure() existing Job = (%v, %v), want running", state, err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: secretName}, &secret); err != nil {
		t.Fatalf("get restored input Secret: %v", err)
	}
}

func TestKBIngesterLifecyclePropagatesCreateFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resourceStore := newFakeStore()
	putStoreResource(t, resourceStore, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","connectionString":"postgres://db/kognition"}`,
		}},
	})
	putStoreResource(t, resourceStore, "Model", "team-a", "embed", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "text-embedding-3-small"},
	})
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Spec:       api.KnowledgeBaseSpec{Sources: []api.KBSource{{Type: "inline", Data: "document"}}},
	}
	kbc := &api.KnowledgeBaseClass{Spec: api.KnowledgeBaseClassSpec{
		EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
		VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
			ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"},
		}},
	}}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual", Namespace: "team-a", UID: "run-uid"}}
	boom := errors.New("kubernetes unavailable")

	tests := []struct {
		name         string
		createErrors []error
		launch       func(*KBIngesterLauncher) error
		want         string
	}{
		{
			name:         "ensure input secret",
			createErrors: []error{boom},
			launch: func(launcher *KBIngesterLauncher) error {
				_, _, err := launcher.Ensure(context.Background(), kb, kbc)
				return err
			},
			want: "create ingest input secret",
		},
		{
			name:         "ensure job",
			createErrors: []error{nil, boom},
			launch: func(launcher *KBIngesterLauncher) error {
				_, _, err := launcher.Ensure(context.Background(), kb, kbc)
				return err
			},
			want: "create ingest job",
		},
		{
			name:         "indexrun input secret",
			createErrors: []error{boom},
			launch: func(launcher *KBIngesterLauncher) error {
				_, _, _, err := launcher.LaunchRun(context.Background(), kb, kbc, run)
				return err
			},
			want: "create indexrun input secret",
		},
		{
			name:         "indexrun job",
			createErrors: []error{nil, boom},
			launch: func(launcher *KBIngesterLauncher) error {
				_, _, _, err := launcher.LaunchRun(context.Background(), kb, kbc, run)
				return err
			},
			want: "create indexrun job",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(scheme).Build()
			faulting := &faultingKBClient{Client: base, createErrors: tc.createErrors}
			launcher := NewKBIngesterLauncher(faulting, resourceStore, zap.NewNop(), "ingester:v1", "ingester", "controllers")

			err := tc.launch(launcher)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, boom) {
				t.Fatalf("lifecycle error = %v, want wrapped %q failure", err, tc.want)
			}
		})
	}
}

func TestKBIngesterLifecyclePropagatesReadFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"}}
	kbc := &api.KnowledgeBaseClass{}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual", Namespace: "team-a", UID: "run-uid"}}
	builder := &KBIngesterLauncher{image: "ingester:v1"}
	ingestJob := builder.buildJob(kb, ingestJobName(kb.ObjectMeta.Name, 1), "input", nil)
	runJob := builder.buildJob(kb, indexRunJobName(run.ObjectMeta.Name), "input", nil)
	runJob.OwnerReferences = []metav1.OwnerReference{indexRunOwnerRef(run)}
	boom := errors.New("kubernetes unavailable")

	tests := []struct {
		name      string
		objects   []client.Object
		getErrors []error
		launch    func(*KBIngesterLauncher) error
		want      string
	}{
		{name: "ensure job", getErrors: []error{boom}, launch: func(l *KBIngesterLauncher) error {
			_, _, err := l.Ensure(context.Background(), kb, kbc)
			return err
		}, want: "get ingest job"},
		{name: "ensure running input", objects: []client.Object{ingestJob}, getErrors: []error{nil, boom}, launch: func(l *KBIngesterLauncher) error {
			_, _, err := l.Ensure(context.Background(), kb, kbc)
			return err
		}, want: "ensure ingest input secret"},
		{name: "indexrun job", getErrors: []error{boom}, launch: func(l *KBIngesterLauncher) error {
			_, _, _, err := l.LaunchRun(context.Background(), kb, kbc, run)
			return err
		}, want: "get indexrun job"},
		{name: "indexrun running input", objects: []client.Object{runJob}, getErrors: []error{nil, boom}, launch: func(l *KBIngesterLauncher) error {
			_, _, _, err := l.LaunchRun(context.Background(), kb, kbc, run)
			return err
		}, want: "ensure indexrun input secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			faulting := &faultingKBClient{Client: base, getErrors: tc.getErrors}
			launcher := NewKBIngesterLauncher(faulting, nil, zap.NewNop(), "ingester:v1", "", "controllers")

			err := tc.launch(launcher)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, boom) {
				t.Fatalf("lifecycle error = %v, want wrapped %q failure", err, tc.want)
			}
		})
	}
}

func TestKBIngesterEnsureRejectsForeignJobCollision(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "kb-ingest-docs-1", Namespace: "team-a"}}
	launcher := &KBIngesterLauncher{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build(),
		image:  "ingester:v1",
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"}}

	state, _, err := launcher.Ensure(context.Background(), kb, &api.KnowledgeBaseClass{})
	if err == nil || state != intctrl.IngestSkipped || !strings.Contains(err.Error(), "not controlled by KnowledgeBase") {
		t.Fatalf("Ensure() foreign Job = (%v, %v), want ownership error", state, err)
	}
}

func TestKBIngesterEnsureRejectsForeignInputSecretForRunningJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"}}
	jobName := ingestJobName(kb.ObjectMeta.Name, 1)
	job := (&KBIngesterLauncher{image: "ingester:v1"}).buildJob(kb, jobName, jobName+"-input", nil)
	foreignSecret := (&KBIngesterLauncher{}).buildInputSecret(kb, jobName, []byte(`{"task":"foreign"}`), nil)
	foreignSecret.OwnerReferences[0].UID = "other-kb-uid"
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, foreignSecret).Build()
	launcher := NewKBIngesterLauncher(fakeClient, nil, zap.NewNop(), "ingester:v1", "", "controllers")

	state, _, err := launcher.Ensure(context.Background(), kb, &api.KnowledgeBaseClass{})
	if err == nil || state != intctrl.IngestSkipped || !strings.Contains(err.Error(), "input secret") || !strings.Contains(err.Error(), "not controlled by KnowledgeBase") {
		t.Fatalf("Ensure() foreign input Secret = (%v, %v), want ownership error", state, err)
	}

	var preserved corev1.Secret
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(foreignSecret), &preserved); err != nil {
		t.Fatalf("get foreign input Secret: %v", err)
	}
	if !bytes.Equal(preserved.Data[ingestInputKey], foreignSecret.Data[ingestInputKey]) || preserved.OwnerReferences[0].UID != "other-kb-uid" {
		t.Fatalf("foreign input Secret was modified: %#v", preserved)
	}
}

func TestKBIngesterEnsureRejectsForeignJobCreationRace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","connectionString":"postgres://db/kognition"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "text-embedding-3-small"},
	})
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	jobName := ingestJobName("docs", 1)
	racingClient := &foreignJobRaceClient{
		Client:  baseClient,
		foreign: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "team-a"}},
	}
	launcher := NewKBIngesterLauncher(racingClient, store, zap.NewNop(), "ingester:v1", "ingester", "controllers")
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Spec:       api.KnowledgeBaseSpec{Sources: []api.KBSource{{Type: "inline", Data: "document"}}},
	}
	kbc := &api.KnowledgeBaseClass{Spec: api.KnowledgeBaseClassSpec{
		EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
		VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
			ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"},
		}},
	}}

	state, _, err := launcher.Ensure(context.Background(), kb, kbc)
	if err == nil || state != intctrl.IngestSkipped || !strings.Contains(err.Error(), "not controlled by KnowledgeBase") {
		t.Fatalf("Ensure() raced foreign Job = (%v, %v), want ownership error", state, err)
	}
}

func TestKBIngesterInterpretJobReadsTerminalStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "ingest-pod", Namespace: "team-a", Labels: map[string]string{"job-name": "kb-ingest-docs-1"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "kb-ingest-docs-1", UID: "job-uid", Controller: ptr.To(true)}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "ingester",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Message: `{"documents":3,"chunks":12,"sourceDigest":"sha256:abc","backendBinding":{"generation":1,"providerName":"azuresearch","classUID":"class-uid","classGeneration":2,"configurationUID":"config-uid","configurationDigest":"config-digest"}}`,
			}},
		}}},
	}
	launcher := &KBIngesterLauncher{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build(),
		logger: zap.NewNop(),
	}
	completed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-ingest-docs-1", Namespace: "team-a", UID: "job-uid"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}

	state, stats, err := launcher.interpretJob(context.Background(), completed)
	if err != nil {
		t.Fatalf("interpret completed Job: %v", err)
	}
	if state != intctrl.IngestSucceeded || stats.Documents != 3 || stats.Chunks != 12 || stats.SourceDigest != "sha256:abc" ||
		stats.BackendBinding == nil || stats.BackendBinding.ProviderName != "azuresearch" || stats.BackendBinding.ConfigurationUID != "config-uid" {
		t.Fatalf("completed Job = (%v, %#v)", state, stats)
	}
	failed := &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "worker failed",
	}}}}
	state, stats, err = launcher.interpretJob(context.Background(), failed)
	if err != nil || state != intctrl.IngestFailed || !strings.Contains(stats.Error, "worker failed") {
		t.Fatalf("failed Job = (%v, %#v, %v)", state, stats, err)
	}
}

func TestKBIngesterProviderCompletionFailsClosed(t *testing.T) {
	configurationKind := schema.GroupVersionKind{Group: "azuresearch.knowledge.kognition.dev", Version: "v1alpha1", Kind: "AzureSearchBackend"}
	configuration := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"embeddingDimensions": int64(1536)},
	}}
	configuration.SetGroupVersionKind(configurationKind)
	configuration.SetNamespace("team-a")
	configuration.SetName("shared")
	configuration.SetUID("config-uid")
	class := &k8s.KnowledgeBaseClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", UID: "class-uid", Generation: 2},
		Spec: k8s.KnowledgeBaseClassSpec{Provisioner: "azuresearch", BackendRef: &k8s.KnowledgeBackendReference{
			APIGroup: configurationKind.Group, Kind: configurationKind.Kind, Name: "shared",
		}},
	}
	trustedKB := &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Spec:       k8s.KnowledgeBaseSpec{KnowledgeBaseClassName: "shared"},
		Status:     k8s.KnowledgeBaseStatus{Generation: 3},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	trustedReader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(trustedKB, class, configuration).Build()
	resolver, err := knowledge.NewBindingResolver(trustedReader, []knowledge.Registration{{
		ProviderName: "azuresearch", ConfigurationKind: configurationKind,
		EmbeddingDimension: func(*unstructured.Unstructured) (int, error) { return 1536, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := resolver.ResolvePending(context.Background(), client.ObjectKey{Namespace: "team-a", Name: "docs"})
	if err != nil {
		t.Fatal(err)
	}
	provider := KBIngesterProviderOptions{Name: "azuresearch", Address: "provider.controllers.svc:9443"}
	transport := providerTransportConfig{
		ProviderName: provider.Name, ProviderAddress: provider.Address, OperationID: "team-a/kb-ingest-docs-4",
		Dimension: 1536, ClassUID: pending.ClassUID, ClassGeneration: pending.ClassGeneration, Scope: pending.Scope,
	}
	transportJSON, err := json.Marshal(transport)
	if err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"}}
	secret := (&KBIngesterLauncher{}).buildInputSecretWithProviderData(kb, "kb-ingest-docs-4", []byte(`{"providerIndex":{"dimension":1536}}`), nil, map[string][]byte{providerConfigKey: transportJSON})
	baseSummary := ingestSummary{
		Documents: 2, Chunks: 5, SourceDigest: "sha256:abc",
		BackendBinding: &api.KnowledgeBackendBinding{
			Generation: pending.Scope.Generation, ProviderName: provider.Name, ClassUID: pending.ClassUID,
			ClassGeneration: pending.ClassGeneration, ConfigurationUID: pending.Scope.ConfigurationUid,
			ConfigurationDigest: pending.Scope.ConfigurationDigest,
		},
		ProviderConfirmation: &providerConfirmation{
			OperationID: transport.OperationID, DocumentCount: 2, IndexedChunks: 5,
			SourceDigest: "sha256:abc", BackendOperationID: "backend-operation",
		},
	}

	tests := []struct {
		name   string
		mutate func(*ingestSummary)
		wantOK bool
	}{
		{name: "exact confirmation", wantOK: true},
		{name: "missing confirmation", mutate: func(summary *ingestSummary) { summary.ProviderConfirmation = nil }},
		{name: "wrong operation", mutate: func(summary *ingestSummary) { summary.ProviderConfirmation.OperationID = "other" }},
		{name: "wrong count", mutate: func(summary *ingestSummary) { summary.ProviderConfirmation.IndexedChunks++ }},
		{name: "wrong digest", mutate: func(summary *ingestSummary) { summary.ProviderConfirmation.SourceDigest = "sha256:other" }},
		{name: "wrong binding", mutate: func(summary *ingestSummary) { summary.BackendBinding.ConfigurationDigest = "other" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summary := baseSummary
			binding := *baseSummary.BackendBinding
			confirmation := *baseSummary.ProviderConfirmation
			summary.BackendBinding = &binding
			summary.ProviderConfirmation = &confirmation
			if tc.mutate != nil {
				tc.mutate(&summary)
			}
			message, err := json.Marshal(summary)
			if err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "ingest-pod", Namespace: "team-a", Labels: map[string]string{"job-name": "kb-ingest-docs-4"},
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "kb-ingest-docs-4", UID: "job-uid", Controller: ptr.To(true)}}},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "ingester", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: string(message)}}}}},
			}
			objects := []client.Object{trustedKB.DeepCopy(), class.DeepCopy(), configuration.DeepCopy(), secret.DeepCopy(), pod}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			localResolver, err := knowledge.NewBindingResolver(reader, []knowledge.Registration{{
				ProviderName: "azuresearch", ConfigurationKind: configurationKind,
				EmbeddingDimension: func(*unstructured.Unstructured) (int, error) { return 1536, nil },
			}})
			if err != nil {
				t.Fatal(err)
			}
			launcher := (&KBIngesterLauncher{client: reader, logger: zap.NewNop()}).WithProvider(provider, localResolver, reader)
			job := launcher.buildJob(kb, "kb-ingest-docs-4", secret.Name, nil)
			job.UID = "job-uid"
			configureProviderJob(job, secret.Name, provider.Name)
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}

			state, stats, err := launcher.interpretJob(context.Background(), job)
			if err != nil {
				t.Fatalf("interpretJob() error = %v", err)
			}
			if tc.wantOK {
				if state != intctrl.IngestSucceeded || stats.Error != "" || stats.BackendBinding == nil || stats.BackendBinding.OperationID != transport.OperationID {
					t.Fatalf("interpretJob() = (%v, %#v), want success", state, stats)
				}
			} else if state != intctrl.IngestFailed || stats.Error == "" {
				t.Fatalf("interpretJob() = (%v, %#v), want fail-closed result", state, stats)
			}
		})
	}
}

func TestKBIngesterInterpretJobToleratesSummaryListFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("kubernetes unavailable")
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	launcher := &KBIngesterLauncher{
		client: &faultingKBClient{Client: base, listErrors: []error{boom}},
		logger: zap.NewNop(),
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "kb-ingest-docs-1", Namespace: "team-a"},
		Status:     batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	}

	state, stats, err := launcher.interpretJob(context.Background(), job)
	if err != nil || state != intctrl.IngestSucceeded || stats != (intctrl.IngestStats{}) {
		t.Fatalf("interpretJob() = (%v, %#v, %v), want success with empty stats", state, stats, err)
	}
}

func TestBuildPayloadAppliesKBOverrides(t *testing.T) {
	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","connectionString":"postgres://db/kognition"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "class-model", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "class-embedding"},
	})
	putStoreResource(t, store, "Model", "team-a", "kb-model", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "kb-embedding"},
	})
	launcher := &KBIngesterLauncher{store: store}
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"},
		Spec: api.KnowledgeBaseSpec{
			Sources: []api.KBSource{{Type: "inline", Data: "document"}},
			IndexConfig: &api.KBIndexConfig{
				EmbeddingModelRef: &api.LocalObjectReference{Name: "kb-model"},
				ChunkSize:         1024, ChunkOverlap: 128,
			},
		},
	}
	kbc := &api.KnowledgeBaseClass{
		ObjectMeta: api.ObjectMeta{Name: "pgvector"},
		Spec: api.KnowledgeBaseClassSpec{
			EmbeddingModelRef: &api.LocalObjectReference{Name: "class-model"},
			ChunkStrategy:     &api.KBChunkStrategy{Size: 700, Overlap: 70, Mode: "sentence"},
			VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
				ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"}, Table: "custom_chunks", Dimension: 3072,
			}},
		},
	}

	payload, env, err := launcher.buildPayload(context.Background(), kb, kbc)
	if err != nil {
		t.Fatalf("buildPayload() error = %v", err)
	}
	chunking := payload["chunking"].(map[string]any)
	if chunking["size"] != 1024 || chunking["overlap"] != 128 || chunking["mode"] != "sentence" {
		t.Fatalf("chunking = %#v", chunking)
	}
	embedding := payload["embedding"].(map[string]any)
	if embedding["model"] != "kb-embedding" {
		t.Fatalf("embedding override = %#v", embedding)
	}
	vectorStore := payload["vectorStore"].(map[string]any)
	if vectorStore["table"] != "custom_chunks" || vectorStore["dimension"] != 3072 || len(env) != 0 {
		t.Fatalf("vector store = %#v, env = %#v", vectorStore, env)
	}
}

func TestKBIngesterResetDeletesPendingGenerationJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Status:     api.KnowledgeBaseStatus{Generation: 2},
	}
	job := (&KBIngesterLauncher{}).buildJob(kb, "kb-ingest-docs-3", "kb-ingest-docs-3-input", nil)
	secret := (&KBIngesterLauncher{}).buildInputSecret(kb, job.Name, []byte(`{"task":"stale"}`), nil)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, secret).Build()
	launcher := &KBIngesterLauncher{client: fakeClient, image: "ingester:v1", logger: zap.NewNop()}

	if err := launcher.Reset(context.Background(), kb); err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	var deleted batchv1.Job
	err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: job.Name}, &deleted)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get reset Job error = %v, want not found", err)
	}
	var deletedSecret corev1.Secret
	err = fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: job.Name + "-input"}, &deletedSecret)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("get reset input Secret error = %v, want not found", err)
	}
	if err := launcher.Reset(context.Background(), kb); err != nil {
		t.Fatalf("Reset() missing Job error = %v", err)
	}
}

func TestKBIngesterResetRejectsForeignResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Status:     api.KnowledgeBaseStatus{Generation: 2},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "kb-ingest-docs-3", Namespace: "team-a"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-input", Namespace: "team-a"}}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(job, secret).Build()
	launcher := &KBIngesterLauncher{client: fakeClient, image: "ingester:v1", logger: zap.NewNop()}

	err := launcher.Reset(context.Background(), kb)
	if err == nil || !strings.Contains(err.Error(), "not controlled by KnowledgeBase") {
		t.Fatalf("Reset() error = %v, want ownership error", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("foreign Job was deleted: %v", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
		t.Fatalf("foreign Secret was deleted: %v", err)
	}

	ownedJob := (&KBIngesterLauncher{}).buildJob(kb, job.Name, secret.Name, nil)
	fakeClient = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ownedJob, secret).Build()
	launcher.client = fakeClient
	err = launcher.Reset(context.Background(), kb)
	if err == nil || !strings.Contains(err.Error(), "not controlled by KnowledgeBase") {
		t.Fatalf("Reset() with foreign Secret error = %v, want ownership error", err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKeyFromObject(ownedJob), &batchv1.Job{}); err != nil {
		t.Fatalf("owned Job was deleted before Secret validation: %v", err)
	}
}

func TestKBIngesterResetPropagatesClientFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"}}
	jobName := ingestJobName(kb.ObjectMeta.Name, 1)
	builder := &KBIngesterLauncher{image: "ingester:v1"}
	job := builder.buildJob(kb, jobName, jobName+"-input", nil)
	secret := builder.buildInputSecret(kb, jobName, []byte(`{"task":"ingest"}`), nil)
	boom := errors.New("kubernetes unavailable")

	tests := []struct {
		name         string
		objects      []client.Object
		getErrors    []error
		deleteErrors []error
		want         string
	}{
		{name: "get job", getErrors: []error{boom}, want: "get ingest job for reset"},
		{name: "get secret", getErrors: []error{nil, boom}, want: "get ingest input secret for reset"},
		{name: "delete job", objects: []client.Object{job.DeepCopy(), secret.DeepCopy()}, deleteErrors: []error{boom}, want: "delete ingest job for reset"},
		{name: "delete secret", objects: []client.Object{job.DeepCopy(), secret.DeepCopy()}, deleteErrors: []error{nil, boom}, want: "delete ingest input secret for reset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()
			faulting := &faultingKBClient{Client: base, getErrors: tc.getErrors, deleteErrors: tc.deleteErrors}
			launcher := NewKBIngesterLauncher(faulting, nil, zap.NewNop(), "ingester:v1", "", "controllers")

			err := launcher.Reset(context.Background(), kb)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, boom) {
				t.Fatalf("Reset() error = %v, want wrapped %q failure", err, tc.want)
			}
		})
	}
}

func TestKBIngesterLaunchRunCreatesRunOwnedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","connectionString":"postgres://db/kognition"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "text-embedding-3-small"},
	})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	launcher := NewKBIngesterLauncher(fakeClient, store, zap.NewNop(), "ingester:v1", "ingester", "controllers")
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Spec:       api.KnowledgeBaseSpec{Sources: []api.KBSource{{Type: "inline", Data: "document"}}},
	}
	kbc := &api.KnowledgeBaseClass{
		Spec: api.KnowledgeBaseClassSpec{
			EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
			VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
				ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"},
			}},
		},
	}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual-run", UID: "run-uid"}}

	state, _, jobName, err := launcher.LaunchRun(context.Background(), kb, kbc, run)
	if err != nil || state != intctrl.IngestRunning || jobName != "kb-indexrun-manual-run" {
		t.Fatalf("LaunchRun() = (%v, %q, %v)", state, jobName, err)
	}
	var job batchv1.Job
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: jobName}, &job); err != nil {
		t.Fatalf("get IndexRun Job: %v", err)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "KnowledgeBaseIndexRun" || job.OwnerReferences[0].UID != "run-uid" {
		t.Fatalf("IndexRun Job owner = %#v", job.OwnerReferences)
	}
	if job.Labels["kognition.dev/indexrun"] != "manual-run" || job.Spec.Template.Labels["kognition.dev/indexrun"] != "manual-run" {
		t.Fatalf("IndexRun labels = %#v, pod labels = %#v", job.Labels, job.Spec.Template.Labels)
	}
	var secret corev1.Secret
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: jobName + "-input"}, &secret); err != nil {
		t.Fatalf("get IndexRun input Secret: %v", err)
	}
	if len(secret.OwnerReferences) != 1 ||
		secret.OwnerReferences[0].Kind != "KnowledgeBaseIndexRun" ||
		secret.OwnerReferences[0].Name != "manual-run" ||
		secret.OwnerReferences[0].UID != "run-uid" ||
		secret.OwnerReferences[0].Controller == nil || !*secret.OwnerReferences[0].Controller ||
		secret.OwnerReferences[0].BlockOwnerDeletion == nil || !*secret.OwnerReferences[0].BlockOwnerDeletion {
		t.Fatalf("IndexRun Secret owner = %#v", secret.OwnerReferences)
	}
	if err := fakeClient.Delete(context.Background(), &secret); err != nil {
		t.Fatalf("delete IndexRun input Secret: %v", err)
	}

	state, _, existingName, err := launcher.LaunchRun(context.Background(), kb, kbc, run)
	if err != nil || state != intctrl.IngestRunning || existingName != jobName {
		t.Fatalf("LaunchRun() existing Job = (%v, %q, %v)", state, existingName, err)
	}
	if err := fakeClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: jobName + "-input"}, &secret); err != nil {
		t.Fatalf("get restored IndexRun input Secret: %v", err)
	}
	if !hasControllerOwner(secret.OwnerReferences, "KnowledgeBaseIndexRun", "manual-run", "run-uid") {
		t.Fatalf("restored IndexRun Secret owner = %#v", secret.OwnerReferences)
	}
}

func TestKBIngesterLaunchRunRejectsForeignJobCollision(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "kb-indexrun-manual-run", Namespace: "team-a"}}
	launcher := &KBIngesterLauncher{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build(),
		image:  "ingester:v1",
	}
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"}}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual-run", UID: "run-uid"}}

	state, _, jobName, err := launcher.LaunchRun(context.Background(), kb, &api.KnowledgeBaseClass{}, run)
	if err == nil || state != intctrl.IngestSkipped || jobName != foreign.Name || !strings.Contains(err.Error(), "not controlled by KnowledgeBaseIndexRun") {
		t.Fatalf("LaunchRun() foreign Job = (%v, %q, %v), want ownership error", state, jobName, err)
	}
}

func TestKBIngesterLaunchRunRejectsForeignJobCreationRace(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "pgvector", &api.SecretRef{
		ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal","connectionString":"postgres://db/kognition"}`,
		}},
	})
	putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{
		Spec: api.ModelSpec{Provider: "openai", ModelID: "text-embedding-3-small"},
	})
	baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	jobName := indexRunJobName("manual-run")
	racingClient := &foreignJobRaceClient{
		Client:  baseClient,
		foreign: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: "team-a"}},
	}
	launcher := NewKBIngesterLauncher(racingClient, store, zap.NewNop(), "ingester:v1", "ingester", "controllers")
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a", UID: "kb-uid"},
		Spec:       api.KnowledgeBaseSpec{Sources: []api.KBSource{{Type: "inline", Data: "document"}}},
	}
	kbc := &api.KnowledgeBaseClass{Spec: api.KnowledgeBaseClassSpec{
		EmbeddingModelRef: &api.LocalObjectReference{Name: "embed"},
		VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{
			ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"},
		}},
	}}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual-run", UID: "run-uid"}}

	state, _, returnedJobName, err := launcher.LaunchRun(context.Background(), kb, kbc, run)
	if err == nil || state != intctrl.IngestSkipped || returnedJobName != jobName || !strings.Contains(err.Error(), "not controlled by KnowledgeBaseIndexRun") {
		t.Fatalf("LaunchRun() raced foreign Job = (%v, %q, %v), want ownership error", state, returnedJobName, err)
	}
}

func TestKBIngesterWithoutImageSkipsLifecycleOperations(t *testing.T) {
	launcher := NewKBIngesterLauncher(nil, nil, zap.NewNop(), "", "", "controllers")
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"}}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual-run"}}

	state, stats, err := launcher.Ensure(context.Background(), kb, &api.KnowledgeBaseClass{})
	if err != nil || state != intctrl.IngestSkipped || stats != (intctrl.IngestStats{}) {
		t.Fatalf("Ensure() = (%v, %#v, %v), want skipped", state, stats, err)
	}
	if err := launcher.Reset(context.Background(), kb); err != nil {
		t.Fatalf("Reset() error = %v, want nil", err)
	}
	state, stats, jobName, err := launcher.LaunchRun(context.Background(), kb, &api.KnowledgeBaseClass{}, run)
	if err != nil || state != intctrl.IngestSkipped || stats != (intctrl.IngestStats{}) || jobName != "" {
		t.Fatalf("LaunchRun() = (%v, %#v, %q, %v), want skipped", state, stats, jobName, err)
	}
}

func TestKBIngesterInterpretJobHandlesNonTerminalAndFallbackSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "attempt-0-invalid", Namespace: "team-a", Labels: map[string]string{"job-name": "ingest"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: "not-json"}},
			}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "attempt-1-failed", Namespace: "team-a", Labels: map[string]string{"job-name": "ingest"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Message:  `{"documents":99,"chunks":999}`,
				}},
			}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "attempt-2-completed", Namespace: "team-a", Labels: map[string]string{"job-name": "ingest"}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"documents":2,"chunks":5}`}},
			}}},
		},
	}
	for _, object := range pods {
		pod := object.(*corev1.Pod)
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "ingest", UID: "job-uid", Controller: ptr.To(true)}}
		pod.Status.ContainerStatuses[0].Name = "ingester"
	}
	launcher := &KBIngesterLauncher{
		client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(pods...).Build(),
		logger: zap.NewNop(),
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "ingest", Namespace: "team-a", UID: "job-uid"},
		Status: batchv1.JobStatus{
			Succeeded:  1,
			Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionFalse}},
		},
	}

	state, stats, err := launcher.interpretJob(context.Background(), job)
	if err != nil || state != intctrl.IngestSucceeded || stats.Documents != 2 || stats.Chunks != 5 {
		t.Fatalf("interpretJob() = (%v, %#v, %v), want fallback success summary", state, stats, err)
	}
	job.Status.Succeeded = 0
	state, stats, err = launcher.interpretJob(context.Background(), job)
	if err != nil || state != intctrl.IngestRunning || stats != (intctrl.IngestStats{}) {
		t.Fatalf("interpretJob() = (%v, %#v, %v), want running", state, stats, err)
	}
}

func TestKBIngesterRejectsInvalidPayloadDependencies(t *testing.T) {
	ctx := context.Background()
	kb := &api.KnowledgeBase{
		ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"},
		Spec:       api.KnowledgeBaseSpec{Sources: []api.KBSource{{Type: "inline", Data: "document"}}},
	}

	t.Run("vector store", func(t *testing.T) {
		launcher := &KBIngesterLauncher{}
		if _, _, err := launcher.buildPayload(ctx, kb, &api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "invalid"}}); err == nil || !strings.Contains(err.Error(), "not pgvector") {
			t.Fatalf("buildPayload() error = %v, want vector store error", err)
		}
	})

	t.Run("missing embedding model", func(t *testing.T) {
		store := newFakeStore()
		putStoreResource(t, store, "SecretRef", "team-a", "pgvector", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"connectionString":"postgres://db/kognition"}`,
		}}})
		launcher := &KBIngesterLauncher{store: store}
		kbc := &api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "pgvector"}, Spec: api.KnowledgeBaseClassSpec{
			VectorStore: &api.KBVectorStore{Type: "pgvector", Pgvector: &api.KBPgvectorStore{ConnectionSecretRef: api.LocalObjectReference{Name: "pgvector"}}},
		}}
		if _, _, err := launcher.buildPayload(ctx, kb, kbc); err == nil || !strings.Contains(err.Error(), "no embedding model") {
			t.Fatalf("buildPayload() error = %v, want embedding model error", err)
		}
	})

	t.Run("unsupported embedding provider", func(t *testing.T) {
		store := newFakeStore()
		putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{Spec: api.ModelSpec{Provider: "bedrock", ModelID: "embed"}})
		launcher := &KBIngesterLauncher{store: store}
		if _, _, err := launcher.buildEmbeddingBlock(ctx, "team-a", "embed"); err == nil || !strings.Contains(err.Error(), "unsupported model provider") {
			t.Fatalf("buildEmbeddingBlock() error = %v, want provider error", err)
		}
	})

	t.Run("unsupported credential type", func(t *testing.T) {
		store := newFakeStore()
		putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{Spec: api.ModelSpec{
			Provider: "foundry", ModelID: "embed", CredentialRef: &api.LocalObjectReference{Name: "identity"},
		}})
		putStoreResource(t, store, "SecretRef", "team-a", "identity", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"azure-identity","credentialType":"certificate"}`,
		}}})
		launcher := &KBIngesterLauncher{store: store}
		if _, _, err := launcher.buildEmbeddingBlock(ctx, "team-a", "embed"); err == nil || !strings.Contains(err.Error(), "unsupported azure-identity credentialType") {
			t.Fatalf("buildEmbeddingBlock() error = %v, want credential type error", err)
		}
	})

	t.Run("missing api key", func(t *testing.T) {
		store := newFakeStore()
		putStoreResource(t, store, "Model", "team-a", "embed", &api.Model{Spec: api.ModelSpec{
			Provider: "openai", ModelID: "embed", CredentialRef: &api.LocalObjectReference{Name: "empty"},
		}})
		putStoreResource(t, store, "SecretRef", "team-a", "empty", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
			"kognition.dev/internal-secret-data": `{"type":"internal"}`,
		}}})
		launcher := &KBIngesterLauncher{store: store}
		if _, _, err := launcher.buildEmbeddingBlock(ctx, "team-a", "embed"); err == nil || !strings.Contains(err.Error(), "nor apiKey") {
			t.Fatalf("buildEmbeddingBlock() error = %v, want apiKey error", err)
		}
	})

	t.Run("missing source dependencies", func(t *testing.T) {
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		launcher := &KBIngesterLauncher{client: fake.NewClientBuilder().WithScheme(scheme).Build(), store: newFakeStore()}
		for _, source := range []api.KBSource{
			{Type: "configMap", ConfigMap: &api.LocalObjectReference{Name: "missing"}},
			{Type: "blob", Blob: &api.KBBlobSource{CredentialSecretRef: &api.LocalObjectReference{Name: "missing"}}},
		} {
			candidate := *kb
			candidate.Spec.Sources = []api.KBSource{source}
			if _, err := launcher.buildSourcesBlock(ctx, &candidate); err == nil {
				t.Fatalf("buildSourcesBlock() error = nil for source %#v", source)
			}
		}
	})
}

func TestKBIngesterLifecycleReportsInvalidPayloadAsFailed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	launcher := NewKBIngesterLauncher(fake.NewClientBuilder().WithScheme(scheme).Build(), newFakeStore(), zap.NewNop(), "ingester:v1", "ingester", "controllers")
	kb := &api.KnowledgeBase{ObjectMeta: api.ObjectMeta{Name: "docs", Namespace: "team-a"}}
	kbc := &api.KnowledgeBaseClass{ObjectMeta: api.ObjectMeta{Name: "invalid"}}

	state, stats, err := launcher.Ensure(context.Background(), kb, kbc)
	if err != nil || state != intctrl.IngestFailed || !strings.Contains(stats.Error, "not pgvector") {
		t.Fatalf("Ensure() = (%v, %#v, %v), want failed vector-store validation", state, stats, err)
	}
	run := &api.KnowledgeBaseIndexRun{ObjectMeta: api.ObjectMeta{Name: "manual-run"}}
	state, stats, jobName, err := launcher.LaunchRun(context.Background(), kb, kbc, run)
	if err != nil || state != intctrl.IngestFailed || jobName != "kb-indexrun-manual-run" || !strings.Contains(stats.Error, "not pgvector") {
		t.Fatalf("LaunchRun() = (%v, %#v, %q, %v), want failed vector-store validation", state, stats, jobName, err)
	}
}

func TestKBIngesterSecretRefValidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ref  *api.SecretRef
		want string
	}{
		{name: "no annotations", ref: &api.SecretRef{}, want: "has no annotations"},
		{name: "missing data", ref: &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{"owner": "test"}}}, want: "missing internal-secret-data"},
		{name: "malformed data", ref: &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{"kognition.dev/internal-secret-data": "{"}}}, want: "parse SecretRef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			putStoreResource(t, store, "SecretRef", "team-a", "credential", tc.ref)
			launcher := &KBIngesterLauncher{store: store}
			if _, err := launcher.readSecretRefAll(ctx, "team-a", "credential"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("readSecretRefAll() error = %v, want %q", err, tc.want)
			}
		})
	}

	store := newFakeStore()
	putStoreResource(t, store, "SecretRef", "team-a", "credential", &api.SecretRef{ObjectMeta: api.ObjectMeta{Annotations: map[string]string{
		"kognition.dev/internal-secret-data": `{"apiKey":""}`,
	}}})
	launcher := &KBIngesterLauncher{store: store}
	if _, err := launcher.readSecretRefKey(ctx, "team-a", "credential", "apiKey"); err == nil || !strings.Contains(err.Error(), `has no "apiKey" key`) {
		t.Fatalf("readSecretRefKey() error = %v, want missing key", err)
	}
}

func TestKBIngesterResourceNamesStayWithinDNSLimit(t *testing.T) {
	longName := strings.Repeat("a", 80)
	for name, wantPrefix := range map[string]string{
		ingestJobName(longName, 42): "kb-ingest-",
		indexRunJobName(longName):   "kb-indexrun-",
	} {
		if len(name) != 63 || !strings.HasPrefix(name, wantPrefix) {
			t.Fatalf("resource name = %q (length %d), want 63-character %q prefix", name, len(name), wantPrefix)
		}
	}
	if indexRunJobName(strings.Repeat("a", 70)+"x") == indexRunJobName(strings.Repeat("a", 70)+"y") {
		t.Fatal("long IndexRun names with different suffixes must not collide")
	}
}

func putStoreResource(t *testing.T, store *fakeStore, kind, namespace, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s %s/%s: %v", kind, namespace, name, err)
	}
	store.getData[kind+"/"+namespace+"/"+name] = data
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}
	return nil
}
