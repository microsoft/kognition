//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelinvocation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/store"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

type exportedProfileStore struct {
	store.Store
	profiles map[string]api.ModelInvocationProfile
}

func (s *exportedProfileStore) Get(_ context.Context, kind, namespace, name string) ([]byte, error) {
	profile, exists := s.profiles[namespace+"/"+name]
	if kind != "ModelInvocationProfile" || !exists {
		return nil, store.ErrNotFound
	}
	return json.Marshal(profile)
}

func TestAzureSampleExportPinsReviewedProfiles(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("offline exporter integration requires PowerShell 7 and powershell-yaml")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	environment := filepath.Join(directory, "settings.env")
	content := "MS_FOUNDRY_PROJECT_ENDPOINT=https://example.services.ai.azure.com/api/projects/example\nMS_FOUNDRY_INFERENCE_ENDPOINT=https://example.cognitiveservices.azure.com\nMS_FOUNDRY_MODEL=production-chat\nMS_FOUNDRY_EMBEDDING_MODEL=production-embedding\nMS_FOUNDRY_INVOCATION_CONTRACT=openai-tool-chat-v1\n"
	if err := os.WriteFile(environment, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, pwsh, "-NoProfile", "-File", filepath.Join(root, "deploy", "azure-samples", "Export-Models.ps1"), "-EnvironmentFile", environment, "-OutputDirectory", directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("offline export failed: %v\n%s", err, output)
	}
	manifest, err := os.ReadFile(filepath.Join(directory, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	storage := &exportedProfileStore{profiles: map[string]api.ModelInvocationProfile{}}
	var models []api.Model
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var kind struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &kind); err != nil {
			t.Fatal(err)
		}
		switch kind.Kind {
		case "ModelInvocationProfile":
			var profile api.ModelInvocationProfile
			if err := json.Unmarshal(raw, &profile); err != nil {
				t.Fatal(err)
			}
			profile.UID = "offline-test-profile"
			storage.profiles[profile.Namespace+"/"+profile.Name] = profile
		case "Model":
			var model api.Model
			if err := json.Unmarshal(raw, &model); err != nil {
				t.Fatal(err)
			}
			models = append(models, model)
		}
	}
	if len(models) != 12 || len(storage.profiles) != 12 {
		t.Fatalf("export shape: models=%d profiles=%d, want 12 of each outside the Helm-owned namespace", len(models), len(storage.profiles))
	}
	for _, model := range models {
		profile, err := modelinvocation.Load(context.Background(), storage, &model)
		if err != nil {
			t.Fatalf("exported Model %s/%s did not bind its reviewed spec: %v", model.Namespace, model.Name, err)
		}
		contract, err := openai.InvocationContract(model.Spec.InvocationContract)
		if err != nil || profile.ValidateContract(contract) != nil {
			t.Fatalf("invalid exported contract %q: %v", model.Spec.InvocationContract, err)
		}
		if model.Spec.ModelID != "production-chat" && model.Spec.ModelID != "production-embedding" {
			t.Fatalf("opaque deployment alias was changed: %q", model.Spec.ModelID)
		}
		if model.Spec.ModelID == "production-chat" && model.Name != model.Spec.ModelID {
			t.Fatalf("chat Model name %q hides its configured deployment %q", model.Name, model.Spec.ModelID)
		}
	}
	values, err := os.ReadFile(filepath.Join(directory, "helm-multi-agent-values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var helmValues struct {
		Model struct {
			Name    string `json:"name"`
			ModelID string `json:"modelID"`
		} `json:"model"`
	}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(values), 4096).Decode(&helmValues); err != nil {
		t.Fatal(err)
	}
	if helmValues.Model.Name != "production-chat" || helmValues.Model.ModelID != helmValues.Model.Name {
		t.Fatalf("Helm Model name and deployment differ: %+v", helmValues.Model)
	}
}

func TestAzureSampleReferencesFollowConfiguredChatModel(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("offline sample rendering requires PowerShell 7 and powershell-yaml")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := `
param([string]$Root)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $Root 'samples\azure-settings.ps1')
Import-Module powershell-yaml -ErrorAction Stop
$settings = @{
    MS_FOUNDRY_PROJECT_ENDPOINT = 'https://example.services.ai.azure.com/api/projects/example'
    MS_FOUNDRY_MODEL = 'production-chat'
    MS_FOUNDRY_EMBEDDING_MODEL = 'production-embedding'
    MS_FOUNDRY_INVOCATION_CONTRACT = 'openai-tool-chat-v1'
}
foreach ($relative in @('simple-agent\manifests.yaml', 'model-grants.yaml', 'solution\manifests.yaml',
    'full-stack-agent\manifests.yaml', 'crossnamespace\manifests.yaml', 'workflows\_agents.yaml')) {
    $documents = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $Root "samples\$relative") -Settings $settings) -AllDocuments)
    foreach ($document in $documents) {
        if ($document.kind -eq 'Model' -and $document.spec.provider -eq 'foundry' -and 'chat' -in $document.spec.capabilities) {
            if ($document.metadata.name -ne 'production-chat' -or $document.spec.modelID -ne 'production-chat') { throw 'Rendered chat Model identity differs from its deployment.' }
        }
        if ($document.kind -in @('Agent', 'Solution', 'ModelGrant') -and $document.spec['modelRef']) {
            if ($document.spec.modelRef.name -notin @('production-chat', 'text-embedding-3-small')) { throw 'A standalone reference still uses a stale chat alias.' }
        }
        if ($document.kind -eq 'MemoryClass' -and $document.spec['summarizationConfig'] -and $document.spec.summarizationConfig['modelRef']) {
            if ($document.spec.summarizationConfig.modelRef.name -ne 'production-chat') { throw 'Memory summarization retained the old chat name.' }
        }
        if ($document.kind -eq 'CrossNamespaceGrant') {
            foreach ($resource in $document.spec.resources) {
                if ($resource.kind -eq 'Model' -and $resource.name -ne 'production-chat') { throw 'Cross-namespace Model authorization retained a stale name.' }
            }
        }
        if ($document.kind -eq 'ModelGrant' -and $document.metadata.name -match 'gpt-6-luna|gpt-4-1') { throw 'A grant name still describes the wrong deployment.' }
    }
}
$arguments = @(Get-AzureSampleHelmArguments -Settings $settings)
$index = [Array]::IndexOf($arguments, 'model.name=production-chat')
if ($index -lt 1 -or $arguments[$index - 1] -ne '--set-string') { throw 'Helm does not receive the configured Model resource name.' }
$originalSettings = ${function:Get-AzureSampleSettings}
$script:configuredSettings = $settings
function Get-AzureSampleSettings { return $script:configuredSettings }
$script:operations = [Collections.Generic.List[object]]::new()
function kubectl {
    $documents = @(ConvertFrom-Yaml -Yaml ($input -join [Environment]::NewLine) -AllDocuments)
    foreach ($document in $documents) {
        if ($document.kind -eq 'Model' -and 'chat' -in $document.spec.capabilities -and $document.metadata.name -ne 'production-chat') {
            throw 'Sample lifecycle addressed a different Model name than the installer.'
        }
        if ($document.kind -in @('Agent', 'ModelGrant') -and $document.spec['modelRef'] -and $document.spec.modelRef.name -notin @('production-chat', 'text-embedding-3-small')) {
            throw 'Sample lifecycle retained a stale model reference.'
        }
    }
    $script:operations.Add(@{ verb=$args[0]; count=$documents.Count })
    $global:LASTEXITCODE = 0
}
Invoke-AzureSampleApply -Path (Join-Path $Root 'samples\simple-agent\manifests.yaml')
Invoke-AzureSampleDelete -Path (Join-Path $Root 'samples\simple-agent\manifests.yaml')
Invoke-AzureSampleDelete -Path (Join-Path $Root 'samples\simple-agent')
if ($script:operations.Count -ne 3 -or $script:operations[0].verb -ne 'apply' -or
    @($script:operations | Where-Object { $_.count -lt 4 }).Count -ne 0 -or
    @($script:operations | Where-Object verb -eq 'delete').Count -ne 2) { throw 'Apply and both deletion entry points did not address the rendered resources.' }
Set-Item -Path function:Get-AzureSampleSettings -Value $originalSettings
$settings.MS_FOUNDRY_MODEL = 'invalid/deployment'
$rejected = $false
try { Read-AzureSampleManifest -Path (Join-Path $Root 'samples\workflows\_agents.yaml') -Settings $settings | Out-Null }
catch { $rejected = $_.Exception.Message -match 'Kubernetes' }
if (-not $rejected) { throw 'A deployment that cannot be used as a Kubernetes resource name was not rejected explicitly.' }
Write-Output 'Configured Model identity and all standalone reference surfaces agree.'
`
	path := filepath.Join(t.TempDir(), "verify-model-names.ps1")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, pwsh, "-NoProfile", "-File", path, "-Root", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("offline configured sample identity failed: %v\n%s", err, output)
	}
}
