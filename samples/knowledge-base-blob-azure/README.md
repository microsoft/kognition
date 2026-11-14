# Knowledge base with a local Azurite Blob source

This sample runs a **real, end-to-end** knowledge base whose documents live in
an Azure Blob container — served locally by **[Azurite](https://github.com/Azure/Azurite)**,
the official Azure Storage emulator, deployed in-cluster. Blob storage requires
no external Storage account or Storage RBAC. The supplied embedding and chat
models still use Foundry credentials and network access. The kb-ingester fetches
the uploaded documents, obtains embeddings and writes them into pgvector;
`azure-docs-agent` retrieves them through `knowledge_search`.

## Prerequisites

1. **Reviewed Kubernetes installation and approved activation.** Set
   `kbPgvector.enabled=true`, `kbIngester.enabled=true`, the ingester/runtime images
   and required credentials in reviewed values before bootstrap. Install wrappers
   do not silently enable these components. Follow the
   [installation contract](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md).
2. **Initialized KnowledgeBase database.** The `vector` extension must exist in
   the database referenced by the KnowledgeBaseClass, not merely in platform SQL.
3. **Python with `azure-storage-blob`** for upload. Select the repository `.venv`
   explicitly and verify the import rather than assuming the current interpreter
   has the dependency.
4. **Working configured models.** The supplied manifests require the Foundry chat
   and embedding deployments and their credentials.

Only the storage portion avoids an external Azure service.

## Pipeline

1. `azurite.yaml` — deploys Azurite (Deployment + Service) with
   `--disableProductStyleUrl` so the in-cluster Service DNS name resolves the
   account from the URL path (otherwise Azurite 404s domain-host requests).
2. `apply.ps1` port-forwards Azurite and uploads
   [`samples/docs/*.md`](../docs/README.md) into the `product-docs` container
   under `mark7/`, then creates the `azurite-blob-creds` SecretRef (a
   connection string whose `BlobEndpoint` targets the in-cluster Service).
3. `Model/text-embedding-3-small` — Foundry embedding model.
4. `KnowledgeBaseClass/azure-blob-kb` — `provisioner: pgvector`.
5. `KnowledgeBase/azure-docs-kb` — a `blob` source (`provider: azure`,
   `bucket: product-docs`) with `credentialSecretRef: azurite-blob-creds`.
6. `KnowledgeBaseClaim/azure-docs-claim` → `Agent/azure-docs-agent`.
7. `KnowledgeBaseIndexRun/azure-docs-reindex-now` — triggers ingestion on apply.

## Apply

Installed automatically by `samples-install` (the sample ships an `apply.ps1`):

```powershell
$env:VIRTUAL_ENV = (Resolve-Path ./.venv).Path
& ./.venv/Scripts/python.exe -c "import azure.storage.blob"
.\make.ps1 samples-install SAMPLE=knowledge-base-blob-azure
```

**Bash (Linux/macOS), with PowerShell 7 and `powershell-yaml` installed:**

```bash
source .venv/bin/activate
python -c "import azure.storage.blob"
pwsh -NoProfile -File ./make.ps1 samples-install SAMPLE=knowledge-base-blob-azure
```

Create the environment with `python3 -m venv .venv` if needed and install the
upload SDK there. See [command shells](../../docs/README.md#command-shells)
for the shared PowerShell module prerequisite.

Use the reviewed `KUBECONFIG`. For a nondefault platform, also set
`KOGNITION_PLATFORM_NAMESPACE` and `KOGNITION_PLATFORM_RELEASE` and pass matching
`NAMESPACE`/`RELEASE` arguments to Make.

`apply.ps1` deploys Azurite, uploads the docs, wires the SecretRefs, then
applies the manifests. Require the uploader's actual `Uploaded ... file(s)`
output. It currently returns exit zero on some `SKIP` paths, including a missing
SDK or missing documents; those are not successful uploads. Azurite stores sample
data in `emptyDir`, so a replacement Pod may require re-upload. After fixing a
missing container or database extension, preserve failed index runs and create a
new named `KnowledgeBaseIndexRun`. Watch ingestion:

The watch command is the same in Bash and PowerShell:

```sh
kubectl -n default get knowledgebase azure-docs-kb -o wide -w
```

Once `azure-docs-kb` is `Ready`, ask the agent something specific:

> "What are the Orbital Mark 7 launch regions, and when is Brazil South
> planned?"

It should call `knowledge_search`, hit pgvector, and cite `regions.md`.

## Using a real Azure Storage account instead of Azurite

The `blob`/`provider: azure` source works against real Azure Blob unchanged —
Azurite just stands in for it locally. To point at a real account:

1. Drop `credentialSecretRef` from the KB source and add
   `endpoint: https://<account>.blob.core.windows.net`.
2. Give the ingester's service principal (from `foundry-creds`) **Storage Blob
   Data Contributor** on the account — the ingester then authenticates
   passwordless via `DefaultAzureCredential` (it already receives the SP's
   `AZURE_*` env from the embedding credential).
3. Ensure the account's firewall allows the cluster's egress (or use a private
   endpoint from AKS).

`upload_docs.py` supports both paths: set `KOGNITION_BLOB_CONNECTION_STRING`
for the emulator/shared-key path, or leave it unset to upload passwordless as
the `.env` service principal.

> **Note:** locked-down accounts (public network access disabled, shared-key
> auth disabled) are only reachable over a private endpoint from inside
> Azure — not from a local kind cluster. That's exactly why this sample
> defaults to Azurite.

## Field reference

See [`api/v1alpha1/memory.go`](../../api/v1alpha1/memory.go) for the full
`KBBlobSource` schema. Admission rules are enforced in
[`internal/admission/knowledgebase_validator.go`](../../internal/admission/knowledgebase_validator.go).
