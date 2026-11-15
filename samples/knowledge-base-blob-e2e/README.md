# Knowledge base — end-to-end blob → pgvector → agent

## Verified governed manual profile

The existing local governed environment passed real Azure Blob acceptance on
2026-10-28. The resource names below belong to that recorded environment, not
every installation. Only on that prepared target, use `azure-live-docs-agent`
for real Storage and keep `azure-docs-agent-v2` for the separate Azurite example.
Select the operator-provided kubeconfig and context explicitly:

```powershell
$env:KUBECONFIG = (Resolve-Path ./reviewed/target.kubeconfig).Path
.\bin\kognitionctl.exe --mode kubernetes --kube-context '<reviewed-context>' chat azure-live-docs-agent `
  "Use knowledge_search to quote the Mark 7 prices and cite the source."
```

**Bash (Linux/macOS):**

```bash
export KUBECONFIG="$PWD/reviewed/target.kubeconfig"
./bin/kognitionctl --mode kubernetes --kube-context '<reviewed-context>' chat azure-live-docs-agent \
  "Use knowledge_search to quote the Mark 7 prices and cite the source."
```

The additive profile uses `azure-live-docs-kb`, `azure-live-docs-claim`, the
existing `governed-pgvector-v2` class and explicit chat/embedding ModelGrants.
Its six documents come from `sakognitiondatasource/product-docs/mark7/` through
Entra authentication and container-scoped Storage Blob Data Reader. Exactly one
ingestion generation produced 23 verified 1536-dimensional vectors; actual
Agent retrieval returned both prices with the real Blob HTTPS citation.
Anonymous access and shared-key authentication remain disabled. The existing
user container and all emulator/repository/S3 indexes were preserved.

The manifest files and `apply.ps1` in this directory retain the legacy
direct-vector-store profile described below. Do not run that installer against
the current governed manual deployment: it copies database credentials and
creates legacy indexing/scheduling resources. The separately rendered six-resource
manual profile reuses the already installed provider, database TLS and Models;
it declares no recurring schedule or additional IndexRun.

## Legacy direct-vector-store profile

This sample chains every piece of the KB pipeline into a single
`kubectl apply` recipe:

```
Azure Blob container ─► KB ingester Job ─► pgvector store ─► agent runtime
                                                 ▲                  │
                                                 └── knowledge_search tool
```

You will end up with:

- A `KnowledgeBaseClass` that binds a shared pgvector store and an
  OpenAI-compatible embedding model.
- A `KnowledgeBase` whose sole source is an Azure Blob prefix.
- An `Agent` that reaches the KB through a `KnowledgeBaseClaim`. The
  runtime automatically attaches a `knowledge_search` tool the LLM can
  call.

Unlike the inline [`samples/knowledge-base/`](../knowledge-base/README.md),
this sample runs the real ingester Job, populates a pgvector table, and lets
the runtime issue nearest-neighbor queries directly. Both samples use the
built-in `knowledge_search` tool.

## Automated Azure Integration Profile

`samples-full-install` intentionally excludes this profile. Its local Azure
Blob coverage uses Azurite through
[`samples/knowledge-base-blob-azure/`](../knowledge-base-blob-azure/README.md).

First configure the tenant using
[`Configure.ps1`](../../deploy/azure-samples/Configure.ps1) as described in the
[Azure setup procedure](../../docs/bootstrapping.md#configure-an-azure-tenant).
Run this profile from a runner that can reach the Storage account data plane:

```powershell
.\make.ps1 samples-provision-azure
.\make.ps1 samples-install SAMPLE=knowledge-base-blob-e2e
```

**Bash (Linux/macOS), with PowerShell 7 installed:**

```bash
pwsh -NoProfile -File ./make.ps1 samples-provision-azure
pwsh -NoProfile -File ./make.ps1 samples-install SAMPLE=knowledge-base-blob-e2e
```

The provisioning target creates an isolated test Storage account, uploads the
repository sample documents, and records the account, container, and prefix in
ignored `.env`. The configured service principal receives container-scoped
`Storage Blob Data Reader` and reads Blobs through Entra authentication. The
embedding-model credential supplies the same identity to the ingester; no Storage
key or expiring SAS is required. Existing connection-string configurations remain
accepted by the installer. No Azure credential is committed to the repository.

If inherited policy disables public Storage networking and no approved private
network path is available, configure with `-DeferBlobDataPlane`. The account,
container, and reader role are prepared through ARM, but upload, ingestion, and
Blob read verification remain explicitly pending. This does not make the real
Azure profile runnable from an unconnected local cluster. The compatible sample
bundle can still use Azurite and the local SeaweedFS S3 fixture.

The installer validates authenticated Blob data-plane access before it applies
Kubernetes resources. It supports Storage accounts reached through public
networks or private endpoints. For private-only Storage, run both the installer
and the ingester pods from AKS or another private-endpoint/VNet-connected
environment; a local kind cluster cannot reach that Blob data plane.

## Prerequisites

1. **Chart installed with both feature flags on.** The chart ships an
   optional pgvector deployment and an optional `kbIngester` image
   reference the k8s controller reads at startup.

   Include `kbPgvector.enabled=true`, `kbIngester.enabled=true` and the exact
   `kbIngester.image.repository` / `kbIngester.image.tag` in the complete reviewed
   values. Prepare images and use the
   [guarded installer](../../docs/features/knowledge-provider-execution/installation-recovery-contract.md)
   for platform changes; direct Helm upgrade/install is unsupported. Sample
   execution requires separately approved activation and dedicated enrolled
   namespaces. Credential/sample helpers must use an explicitly reviewed target
   and must not run during an installation or recovery writer freeze.

   The chart materialises a Secret named `<release>-kb-pgvector` in
   `kognition-system` with two keys: `password` and `connectionString`
   (a libpq DSN). We reference the DSN Secret from the KBClass below.

2. **Foundry credentials and Models installed.** Run
  `samples-full-install` first on the target cluster. The profile copies its
  ready `text-embedding-3-small` Model into the profile namespace and creates
  secret references from the generated Azure settings.

3. **Network path to Azure Blob Storage.** The runner and ingester pods must
  reach the Storage data plane. The preflight performs an authenticated
  data-plane check from the runner; private-only accounts require private
  endpoint or VNet connectivity for both the runner and the cluster.

4. **A working chat `Model` for the agent.** The Kubernetes alias is
   `gpt-6-luna`; sample installers map its provider deployment from
   `MS_FOUNDRY_MODEL` without renaming Agent or Workflow references.

5. **Azure Storage blobs.** `samples-provision-azure` uploads the repository
  sample documents under the `mark7/` prefix.

## Apply

Run the secret-safe preflight before provisioning or applying the profile:

```powershell
.\e2e\azure-blob-preflight.ps1 -StorageAccount '<account-name>'
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./e2e/azure-blob-preflight.ps1 -StorageAccount '<account-name>'
```

Then install the profile. It creates the namespace-local SecretRefs, copies the
ready embedding Model, and applies the KnowledgeBase, Agent, and reindex
resources:

```powershell
.\make.ps1 samples-install SAMPLE=knowledge-base-blob-e2e
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./make.ps1 samples-install SAMPLE=knowledge-base-blob-e2e
```

Wait for the pipeline to finish:

The following inspection commands work in Bash and PowerShell (`jq` is required
for the formatted JSON view):

```sh
# 1. Ingester Job runs to completion.
kubectl -n samples-azure-blob get jobs -l kognition.dev/kb=azure-docs-kb --watch

# 2. KB status reports counts + digest from the Job's termination log.
kubectl -n samples-azure-blob get knowledgebase azure-docs-kb -o jsonpath='{.status}' | jq

# 3. Agent Deployment picks up KOGNITION_KB_RESOLVED.
kubectl -n samples-azure-blob rollout status deployment/agent-azure-docs-agent
```

If ingestion fails, the summary appears both in the Job's Pod log and on
the KB's `status.conditions`:

```sh
kubectl -n samples-azure-blob logs job/kb-ingest-azure-docs-kb-1
kubectl -n samples-azure-blob describe knowledgebase azure-docs-kb
```

## Ask the agent something

```powershell
.\bin\kognitionctl.exe chat azure-docs-agent `
  "Summarise the Mark 7 launch checklist. Cite the source blobs." `
  --mode kubernetes
```

**Bash (Linux/macOS):**

```bash
./bin/kognitionctl chat azure-docs-agent \
  "Summarise the Mark 7 launch checklist. Cite the source blobs." \
  --mode kubernetes
```

The runtime logs each tool call. The `knowledge_search` invocation
appears with the query it sent to pgvector:

```powershell
kubectl -n default logs deployment/agent-azure-docs-agent --tail=80 |
  Select-String "knowledge_search"
```

**Bash (Linux/macOS):**

```bash
kubectl -n default logs deployment/agent-azure-docs-agent --tail=80 |
  grep -F 'knowledge_search'
```

You can also confirm rows landed in pgvector directly:

```powershell
$dsn = kubectl -n default get secret kognition-kb-pgvector `
  -o jsonpath='{.data.connectionString}' | ForEach-Object { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($_)) }
psql "$dsn" -c "SELECT kb_namespace, kb_name, source_uri, chunk_idx FROM kognition_kb_chunks WHERE kb_name = 'azure-docs-kb' LIMIT 20;"
```

**Bash (Linux/macOS), for this legacy direct-database profile only:**

```bash
set -o pipefail
dsn="$(kubectl -n default get secret kognition-kb-pgvector \
  -o jsonpath='{.data.connectionString}' |
  python3 -c 'import base64,sys; print(base64.b64decode(sys.stdin.read()).decode())')" || exit 1
psql "$dsn" -c "SELECT kb_namespace, kb_name, source_uri, chunk_idx FROM kognition_kb_chunks WHERE kb_name = 'azure-docs-kb' LIMIT 20;"
unset dsn
```

Keep shell tracing disabled and do not print the DSN. This legacy inspection is
not the governed provider path; do not copy database credentials into Agent Pods.

## What each file does

| File | Kind | Purpose |
| --- | --- | --- |
| `kb-class.yaml` | `KnowledgeBaseClass` | Binds provisioner `pgvector`, points at `kognition-kb-pgvector` Secret, references the embedding Model. |
| `secretref-azure.yaml` | `SecretRef` | Wraps the Azure Storage credentials the ingester uses to list/download blobs. |
| `kb.yaml` | `KnowledgeBase` | Sole source is `type: blob, provider: azure` under container `product-docs`, prefix `mark7/`. |
| `model.yaml` | `Model` + `SecretRef` | The embedding model and its API key. |
| `agent.yaml` | `Agent` + `KnowledgeBaseClaim` | The claim binds `azure-docs-kb`; the agent references the claim. |
| `indexrun.yaml` | `KnowledgeBaseIndexRun` | One-shot ad-hoc reindex of the KB. Job-analog. Optional. |
| `indexschedule.yaml` | `KnowledgeBaseIndexSchedule` | Recurring cron-scheduled reindex. CronJob-analog. Optional. |

## Reindex on demand or on a schedule

Two CRDs provide explicit reindex history, scheduling, and RBAC separation:

- Ad hoc: `kubectl apply -f indexrun.yaml` creates a
  `KnowledgeBaseIndexRun` that launches one ingest Job and reports
  status back on the run object. Delete the run (or let its
  `ttlSecondsAfterFinished` expire) to garbage-collect it.
- On a schedule: `kubectl apply -f indexschedule.yaml` creates a
  `KnowledgeBaseIndexSchedule` that materialises a new IndexRun on each
  cron tick. `concurrencyPolicy: Forbid` skips overlapping runs;
  `successfulRunsHistoryLimit` / `failedRunsHistoryLimit` cap retained
  runs.

Watch progress with:

Use either shell:

```sh
kubectl get kbrun -n default -w
kubectl get kbsched -n default
```

## Provider mapping

The `knowledge_search` tool works over any `blob` source whose credentials
resolve. To switch to S3, replace the `blob` block in `kb.yaml`:

```yaml
sources:
  - type: blob
    blob:
      provider: s3
      bucket: my-corpus
      prefix: docs/
      region: us-east-1
      credentialSecretRef:
        name: s3-blob-creds
      include: ["**/*.md", "**/*.txt", "**/*.pdf"]
```

and swap `secretref-azure.yaml` for a SecretRef exposing the keys
documented in [`samples/knowledge-base-blob-s3/`](../knowledge-base-blob-s3/README.md).
Everything else (KBClass, KB, Agent, runtime wiring) stays the same.

## Caveats

- **DSN in a Secret means DSN in-cluster.** The dispatcher inlines the
  DSN into the agent Deployment's env so the runtime can connect
  without a network hop to the control plane. Rotate the pgvector
  password by re-templating the chart and letting reconciliation roll
  the agent Deployments.
- **Dimension must match.** The chart defaults to 1536 to match
  `text-embedding-3-small`. If you switch to `text-embedding-3-large`
  (3072), update `KnowledgeBaseClass.spec.vectorStore.pgvector.dimension`
  **and** re-provision the pgvector store — the column type is fixed at
  creation time.
- **First-query latency.** The first `knowledge_search` call in a fresh
  pod pays a lazy `import psycopg` and one embedding round-trip. Subsequent
  calls reuse the connection.
