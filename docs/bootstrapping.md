# Bootstrapping Kognition locally

Kubernetes bootstrap uses reviewed inactive admission followed by separately
approved activation. Automatic `bootstrap.ps1`, `quickstart` and
`fresh-kubernetes-demo` are blocked. Follow the
[installation and recovery contract](features/knowledge-provider-execution/installation-recovery-contract.md)
before using the post-activation sample operations below. Missing security gates
block activation; successful acceptance and approval apply only to their exact
target. This guide itself does not authorize activation or production deployment.

## 1. Prerequisites

| Tool | Notes |
| ---- | ----- |
| Docker | Builds the images. |
| kind | An explicitly authorized isolated local cluster and separate kubeconfig. |
| kubectl | 1.28+. |
| Helm | 3.x. |
| Go | Builds the Go binaries/images. |
| Python 3.11+ | The doc-upload helper for the Azure Blob sample (repo `.venv` has the deps). |
| Azure CLI (`az`) | Optional — only for setting up storage RBAC. |

A running cluster is required. Verify it using its explicit kubeconfig without
changing the global context. Load images only into the named authorized cluster
as a separate preparation step; the installer does not load them.

The 2026-10-02 local rehearsal used Kubernetes 1.31.1, Cilium 1.17.8,
Helm 3.19.0 and Python 3.12. This is a tested local combination, not certification
of every version allowed by the tooling table. Do not infer image availability in
the node from a successful host build or a best-effort `kind load` warning.

If Linux Python image builds fail TLS downloads while the Windows host can verify
the same package source, use host `pip download` for the target Linux/Python ABI
and a hashed wheelhouse for a fresh image's `pip --no-index --find-links` install.
Include Linux-only dependencies such as `uvloop` explicitly when host marker
evaluation omits them, and run `pip check` inside the resulting images. Preserve
source, wheel and image identities; do not disable TLS verification, retag an old
runtime as a rebuild, or treat a failed node import as success.

## 2. Credentials — `.env`

Azure-backed workloads authenticate through a **service principal**, and the same
SP uses Entra authentication for Azure Blob knowledge bases. Copy the template and
fill it in:

```powershell
Copy-Item .env.example .env
```

Required keys:

| Key | Purpose |
| --- | ------- |
| `AZURE_TENANT_ID` / `AZURE_CLIENT_ID` / `AZURE_CLIENT_SECRET` | The service principal. |
| `MS_FOUNDRY_PROJECT_ENDPOINT` / `MS_FOUNDRY_MODEL` / `MS_FOUNDRY_EMBEDDING_MODEL` | Foundry chat + embedding deployments. |

The guarded installer does not read `.env` or inject credentials. Provision only
the credentials required by the reviewed profile in dedicated namespaces.
Credential and sample helpers are separate writers and must remain stopped during
bootstrap/recovery. Local installer acceptance needs no cloud credentials.

### Configure an Azure tenant

The [Azure sample configurator](../deploy/azure-samples/Configure.ps1) uses the
active Azure CLI login. Review the subscription, region, resource names, costs,
and identity permissions before running it. It requires PowerShell 7 and the
`powershell-yaml` module.

```powershell
$azureSamples = @{
    SubscriptionId = '<subscription-id>'
    FoundryResourceGroup = '<resource-group>'
    FoundryAccount = '<account>'
    FoundryProject = '<project>'
    ChatDeployment = '<chat-deployment-name>'
    ChatInvocationContract = 'openai-tool-chat-v1'
    EmbeddingDeployment = 'text-embedding-3-small'
    Location = 'eastus2'
}
.\deploy\azure-samples\Configure.ps1 @azureSamples -ValidateOnly
# After review and explicit approval:
.\deploy\azure-samples\Configure.ps1 @azureSamples
```

The configurator:

- Reuses the selected Foundry deployments and preserves disabled API-key
  authentication. It creates no models, AKS cluster, registry, or Azure database.
- Creates or reuses `kognition-local-samples`, with `Cognitive Services OpenAI User`
  on the Foundry account and `Storage Blob Data Reader` on the demo container.
- Creates Standard LRS Blob Storage with HTTPS. The normal local profile requests
  authenticated public network access; inherited policies can require private
  access. Anonymous Blob and shared-key access stay disabled. Only the signed-in
  operator receives account-scoped Blob upload permission when uploads are enabled.
- Uploads `samples/docs/` to `product-docs/mark7/`.
- Saves the client credential in ignored `.env` and `foundry-creds.yaml`, and
  restricts their file permissions. New credentials expire after 90 days.
  Use `-RenewCredential` to append a replacement without revoking other credentials.
- Generates ignored models, Helm values, and a memory-service identity Secret in
  `.azure/local-samples/`. It does not apply anything to Kubernetes, activate a
  controller, change the global context, or alter unrelated local settings.
- Verifies chat, structured tool calling, exactly 1,536 embedding dimensions, and
  Blob listing/download using the service principal. Repeat these bounded
  data-plane checks with `.\deploy\azure-samples\Test.ps1`; the operator CLI login
  is never replaced with a service-principal login.

For a tenant that enforces private Storage, use `-DeferBlobDataPlane` after review.
This creates the private account, container, and container-read role through ARM,
generates configuration, and verifies Foundry. It does not upload documents or
claim Blob connectivity. The environment records
`KOGNITION_AZURE_BLOB_DATA_PLANE_STATUS=pending-private-connectivity`.
Inherited policy discovery includes management-group assignments through
`atScope()`; the configurator never changes or exempts governance controls.

Run `Test.ps1 -SkipBlob` to repeat only the Foundry checks. After arranging an
approved private-network runner and connectivity, rerun provisioning without the
deferral and run the complete verification. Local Azurite and S3 emulator samples
continue to use their in-cluster storage and the configured Foundry identity.

The PowerShell sample installers, standalone manifest helper, and Makefile sample
target read `.env` to bind Foundry `Model` resources to these deployments. The chat
Model name and `spec.modelID` both use `MS_FOUNDRY_MODEL`, currently
`gpt-6-luna`. Agent, Solution, grant and summarization references use the same
name, including documents that do not declare a Model. This setting must be a
valid Kubernetes resource name. Use the sample installers rather than applying the
environment-specific reference manifests directly. Helm users can include the
generated `helm-multi-agent-values.yaml`.

Changing the chat Model name requires creating the new resource and its grants
before switching consumers; Kubernetes does not rename an existing resource.
Keep embedding Models and committed knowledge bindings unchanged. Retire the
old chat Model only after current references and real invocations use the new
name. Preserve immutable historical revisions and failed-session evidence.
Select `MS_FOUNDRY_INVOCATION_CONTRACT` explicitly; a deployment name does not
identify an invocation protocol. The installers generate generic namespace-local
profiles and bind the Models to the selected adapter contract. See
[Model Invocation Profiles](model-invocation-profiles.md). Use
`.\deploy\azure-samples\Export-Models.ps1` to refresh nonsecret model/profile
manifests and Helm values without provisioning or changing a running installation.
The generated model snapshot excludes `samples-multi-agent`, where Helm owns the
Model resource. Its values reuse the shared Foundry class and generated credentials
to avoid taking ownership of objects created by another installer.
Generated documents use JSON-compatible YAML with explicit document markers so
date-like API versions remain strings across YAML parsers.

GPT-5/6 reasoning deployments use `max_completion_tokens` and model-default
sampling. The Foundry driver, Model Gateway, and Python provider adapt those fields;
older deployments retain their existing behavior. Rebuild the affected gateway
and runtime images before using the updated code in a local installation.
The GPT-6 Chat Completions profile also defaults `reasoning_effort` to `none`,
which the deployed model requires for function tools on that API.

For memory embeddings, pre-provision the generated `identity-secret.yaml` in the
reviewed platform namespace and include `memory-embedding-values.yaml` in the
complete reviewed values. This uses a local client credential, not AKS workload
federation or a Foundry API key. Platform changes still require the guarded installer.

The `knowledge-base-blob-azure` sample continues to use in-cluster Azurite and needs
no Azure Storage account. The real-account profile is
[`knowledge-base-blob-e2e`](../samples/knowledge-base-blob-e2e/README.md).

## 3. Reviewed Bootstrap

Prepare images, CRDs, certificates, dedicated namespaces and a protected authority
namespace. Freeze writers; review the exact plan/artifact hashes and live inventory.
For upgrades, finalize or explicitly migrate work before stopping the old controller
and revoking grants. The installer verifies this order and ends inactive.

```powershell
.\make.ps1 deploy-kubernetes INSTALL_PLAN=./reviewed/install.json INSTALL_WRITERS_FROZEN=true
```

Use the same arguments with `make` on POSIX. Direct CLI and recovery commands are
in the contract. No helper automatically activates or resumes samples.

Prepare deterministic values with controller replicas zero and pre-provisioned
gateway Secrets as described in the contract. Resource admission runs separately
from reconciliation, so its webhook must be ready before controller activation.
Guarded installation skips Helm migration hooks: initialize the platform SQL
schema and the separate KnowledgeBase `vector` extension explicitly under the
[database prerequisites](features/knowledge-provider-execution/installation-recovery-contract.md#database-prerequisites).

## 4. Manual step-by-step

After separately approved activation, these are post-activation operations:

```powershell
$env:KUBECONFIG = (Resolve-Path ./reviewed/target.kubeconfig).Path
$context = 'reviewed-context'
$release = 'reviewed-release'
$systemNamespace = 'reviewed-system'
$env:KOGNITION_PLATFORM_NAMESPACE = $systemNamespace
$env:KOGNITION_PLATFORM_RELEASE = $release
$env:VIRTUAL_ENV = (Resolve-Path ./.venv).Path
& ./.venv/Scripts/python.exe -c "import azure.storage.blob"
.\make.ps1 samples-full-install "NAMESPACE=$systemNamespace" "RELEASE=$release" AGENT_NAMESPACE=default
go build -o ./bin/kognitionctl.exe ./cmd/kognitionctl
./bin/kognitionctl.exe studio --mode kubernetes --kube-context $context --namespace default --kube-namespace $systemNamespace
```

Replace the target values with the reviewed installation. Use a dedicated shell
or restore its environment afterward; do not switch the global kubeconfig context.
The sample scripts inherit `KUBECONFIG`; confirm its current context matches
`$context` before running them. See the [sample matrix](sample-matrix.md) for
incompatible and cloud-dependent exclusions. In a separate shell with the same
explicit kubeconfig, launch `kognitionctl dashboard` with `--namespace default`,
`--kube-namespace $systemNamespace`, and `--audit-namespace $systemNamespace`.

Include required `kbPgvector`, `kbIngester`, runtime image and credential settings
in the reviewed values before bootstrap. No install wrapper silently enables
components, substitutes images or enrolls extra namespaces.

### Canary operation retention

Controllers retain completed canary operation records for 30 days, then
replace them with permanent immutable tombstones. Tombstones preserve replay
and conflict detection while discarding usage and audit payloads.

Set `KOGNITION_CANARY_OPERATION_RETENTION` to a positive Go duration such as
`72h` to change the retention period. Set it to `0` to disable compaction.
Negative or invalid durations stop controller startup. Helm installs expose
the same setting as `canaryOperationRetention` in `values.yaml`.

## 5. What you get

| Sample | Agent | Knowledge base |
| ------ | ----- | -------------- |
| `knowledge-base` | `orbital-docs-agent` | Inline "Orbital Mark 7" notes. |
| `knowledge-base-kognition-docs` | `kognition-docs-agent` | This repo's docs, served in-cluster → pgvector (real). |
| `knowledge-base-blob-azure` | `azure-docs-agent` | `samples/docs/` uploaded to in-cluster **Azurite** (Azure Storage emulator) → pgvector (real, fully local). |
| Other samples | See each sample's README. | Review its dependencies before installation. |

The Azure Blob sample's `apply.ps1` invokes an uploader for
[`samples/docs/`](../samples/docs/README.md). Confirm the selected interpreter has
`azure-storage-blob` and the output reports the files uploaded to `product-docs`.
The helper currently returns success for some `SKIP` paths, including a missing
SDK; exit code zero alone does not prove the container was populated. Local blob
storage does not make Foundry chat or embedding inference local.

## 6. Test it

```powershell
./bin/kognitionctl.exe studio --mode kubernetes --kube-context $context --namespace default --kube-namespace $systemNamespace
```

In Studio (kubernetes mode), open an agent and expand **Knowledge Base
Claims** — you'll see its bound claim. Chat with:

- `kognition-docs-agent` — "What autonomy classes does Kognition define?"
- `orbital-docs-agent` / `azure-docs-agent` — "What are the Orbital Mark 7
  launch regions, and when is Brazil South planned?"

A grounded answer that cites a source file means retrieval is working end to
end (agent → `knowledge_search` → pgvector).

## 7. Troubleshooting

| Symptom | Cause / fix |
| ------- | ----------- |
| Studio's **Knowledge Base** box is empty | Stale `kognitionctl` on your PATH. Use `.\make.ps1 studio` (runs a freshly built binary) instead of a `go install`ed one. |
| Studio/dashboard has no resources or returns missing-CRD errors | Check the process's explicit kubeconfig, context and resource namespace. The system/audit namespace is not necessarily where Agents reside. |
| Memory requests return `memory mutation replay unavailable` | Inspect gateway logs for missing `memory_mutation_receipts` (`42P01`); run reviewed platform migrations. Gateway readiness alone does not verify schema. |
| Ingester reports `vector type not found in the database` | Initialize the `vector` extension in the separate KnowledgeBase database, then submit a new index run. |
| Azurite indexing reports `ContainerNotFound` after installation | Check for uploader `SKIP` output or lost ephemeral emulator data. Select the verified interpreter and re-upload documents before a new index run. |
| A KnowledgeBase is stuck `Failed` | Inspect its conditions and ingestion logs; correct the reported cause before a resource-specific retry. Do not force status to claim readiness or bypass bootstrap gates. |
| Azure Blob KB ingest fails against a **real** account with `AuthorizationFailure` | The SP lacks **Storage Blob Data Contributor**, or the account firewall/public-network-access blocks your host. The default sample uses in-cluster Azurite and avoids this entirely. |
| Agent 503 "requires KOGNITION_API_KEY or AZURE_*" | Review the credential configuration and intended namespace/target. Run credential helpers only against that explicit target, outside any bootstrap/recovery writer freeze; deployment does not inject credentials. |
| Ingest logs | `kubectl -n <ns> logs job/kb-ingest-<kb>-<generation>` (or the pod). |

## 8. Uninstall

Freeze new work, complete data retention/deletion and finalization with the old
controller still authorized, then review exact cleanup before removing the
platform. These destructive commands are not installer recovery and must never
be used to clear claims or bypass a failed upgrade:

```powershell
.\make.ps1 samples-uninstall    # remove all samples (manifest + script-based)
.\make.ps1 uninstall            # remove the platform, CRDs, and namespace
```
