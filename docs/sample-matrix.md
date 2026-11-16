# Sample Matrix

Operating instructions updated 2026-11-13. Dated verification scopes below retain
their original dates and targets.

The strict local command is:

```powershell
.\make.ps1 samples-full-install
```

**Bash (Linux/macOS), with PowerShell 7 installed:**

```bash
make samples-full-install
```

Run this only after installation completes and the controller is ready, using an
explicit kubeconfig and enrolled workload namespaces. Existing-release upgrades
must complete their applicable activation procedure first.
Pass `NAMESPACE`, `RELEASE` and `AGENT_NAMESPACE` for a
nondefault installation, and set `KOGNITION_PLATFORM_NAMESPACE` and
`KOGNITION_PLATFORM_RELEASE` for script-based samples. See
[bootstrapping](bootstrapping.md#4-manual-step-by-step).

It applies every compatible local sample, creates required sample
namespaces, injects ignored Foundry credentials from `.env`, installs the
governance and multi-agent Helm profiles, and fails on apply errors or terminal
resource failures. It validates resources marked as part of the compatible
bundle. Agent `Running` is accepted for warm-pool Agents; explicit sample
invocations must become `Ready`.

## Validation Evidence

The following table records the 2026-08-21 run, not a new full-bundle assertion.

| Check | Result | Coverage |
| --- | --- | --- |
| `samples-full-install` | Passed | Compatible bundle with no terminal failed resources. |
| `hello-run-1` | Passed | Foundry Model Gateway invocation completed `Ready` with an output and token usage. |
| `e2e-kubernetes-memory` | Passed | Memory capability authorization, upstream credential isolation, direct-access denial, and grant revocation. |
| Helm rendering | Passed | Governance, multi-agent, and umbrella profiles rendered with Helm. |
| Local blob samples | Passed through strict bundle | Azurite and MinIO scripts uploaded `samples/docs/` before applying their KnowledgeBase resources. |
| Focused Go tests | Passed | Model Gateway, Foundry driver, and Kubernetes dispatcher packages. |

On 2026-10-02, the clean local reinstall applied the compatible bundle but its
strict readiness check failed on index runs created before database initialization
and successful Azurite upload. After repairs, all nine platform Deployments,
39 Agents, four Models and four KnowledgeBases were ready; 13 Workflow templates
were installed. Three original failed index runs were retained and three named
replacement runs succeeded: docs 4 documents/1,307 chunks, S3 6/23, Azurite 6/23.
This is recovered readiness, not an assertion that `samples-full-install` exited
successfully or that every workflow was executed. Counts precede the separate
Demo rehearsal.

On 2026-10-24, a dedicated kind installation completed guarded activation after
fresh required deployed acceptance and exact-target owner approval. Nine platform
Deployments and both databases were ready. The recovered sample installation had
38 ready Agents and identities, four ready Models, seven invocation profiles,
15 bound memory claims and volumes, 13 Workflow templates, and a completed
five-Agent WorkflowRun. Foundry `gpt-6-luna` chat, an actual calculator call
(17 multiplied by 23), and sandbox sample memory store/recall passed through the
installed CLI. These checks do not qualify every Workflow or the Memory Service
capability boundary.

Three KnowledgeBases indexed successfully: inline 3 documents/30 chunks,
Azurite 6/23, and repository documentation 4/1,405. Both explicit and
controller-generated index schedules were suspended. Runtime pgvector retrieval
remained blocked: its legacy embedding client expects Azure credentials that
Model Gateway-enabled Agent pods intentionally do not receive. Successful
indexing and Agent readiness do not establish working retrieval.

S3 was deferred because the pinned MinIO image returned unauthorized/not-found
responses from the official registries; no third-party replacement was installed.
The standalone `kagent-observability` recovery fixture and second-platform
umbrella were excluded. Real Azure Blob remained outside this local profile
pending approved private-network connectivity. This is partial local functional
coverage, not a successful strict full-bundle run or production qualification.

The [October 28 governed sample acceptance](development-plans/features/knowledge-provider-execution/remaining-sample-blockers-plan.md)
supersedes those retrieval, S3 and real-Blob blockers for its separate manual
candidate. Native and AutoGen cited pgvector queries, a qualified SeaweedFS S3
fixture and authenticated real Azure Blob ingestion/retrieval pass there without
raw provider/database credentials in Agent Pods. This does not certify the
legacy direct-vector-store Blob installer or a successful strict full-bundle
run. The explicit governed installer below has focused offline regression
coverage. A successful governed full-bundle deployment and retrieval acceptance
remain pending under T-0368 in the
[current backlog](../BACKLOG.md#43-next-work-queue-2026-10-29).

## Governed Installer Profile

The optional `SAMPLE_PROFILE` selects reviewed namespace, backend, fixture and
budget settings. Without it, existing sample defaults remain unchanged.
Use PowerShell 7 with `powershell-yaml`, Helm, kubectl and a selected Python
environment containing `azure-storage-blob` and `boto3`. No image build, Docker
import, cloud provisioning, platform upgrade or activation occurs in this path.
See [command shells](README.md#command-shells) for installing the shared helper
dependencies. Select the uploader environment with `$env:VIRTUAL_ENV` in
PowerShell or `export VIRTUAL_ENV="$PWD/.venv"` in Bash.

```powershell
.\make.ps1 samples-full-install KUBECONFIG=C:\private\approved.kubeconfig SAMPLE_PROFILE=C:\private\governed-samples.json
.\make.ps1 samples-install SAMPLE=knowledge-base-blob-s3 KUBECONFIG=C:\private\approved.kubeconfig SAMPLE_PROFILE=C:\private\governed-samples.json
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./make.ps1 samples-full-install \
    "KUBECONFIG=$HOME/private/approved.kubeconfig" \
    "SAMPLE_PROFILE=$HOME/private/governed-samples.json"
pwsh -NoProfile -File ./make.ps1 samples-install SAMPLE=knowledge-base-blob-s3 \
    "KUBECONFIG=$HOME/private/approved.kubeconfig" \
    "SAMPLE_PROFILE=$HOME/private/governed-samples.json"
```

These are full-bundle and individual-sample alternatives, not sequential
instructions. Pass profile/target arguments directly to `make.ps1` from Bash;
the GNU Make full-bundle wrapper does not forward these arguments.

Keep the profile outside source control. The following JSON uses fake image
digests; replace them with reviewed, published digests before installation.
All shown fields are required.

```json
{
  "schemaVersion": 1,
  "readinessTimeoutSeconds": 900,
  "namespaceMap": {
    "default": "kognition-work",
    "kognition-demo": "kognition-samples-demo",
    "samples-governance": "kognition-samples-governance",
    "samples-multi-agent": "kognition-samples-multi-agent",
    "samples-azure-blob": "kognition-samples-azure-blob",
    "production": "kognition-samples-production",
    "development": "kognition-samples-development",
    "samples-azure-blob-services": "kognition-providers",
    "samples-s3-blob-services": "kognition-providers",
    "samples-kognition-docs-services": "kognition-providers",
    "samples-tool-sandbox": "kognition-providers"
  },
  "knowledgeBackend": {
    "name": "sample-pgvector-v1",
    "host": "postgres.kognition-providers.svc.cluster.local",
    "port": 5432,
    "database": "sample_knowledge",
    "table": "sample_chunks",
    "embeddingDimensions": 1536,
    "credentialsSecretRef": {
      "name": "sample-db",
      "usernameKey": "username",
      "passwordKey": "password"
    },
    "tls": {
      "mode": "verify-full",
      "caSecretRef": {"name": "sample-ca", "key": "ca.crt"}
    }
  },
  "fixtures": {
    "azuriteImage": "registry.invalid/azurite@sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "s3Image": "registry.invalid/seaweedfs@sha256:2222222222222222222222222222222222222222222222222222222222222222",
    "docsImage": "registry.invalid/docs@sha256:3333333333333333333333333333333333333333333333333333333333333333"
  },
  "budgets": {
    "agents": 64,
    "tools": 32,
    "workflows": 32,
    "maxInputTokens": 1000000,
    "maxOutputTokens": 200000
  }
}
```

### Prerequisites And Safety Contract

- Enroll the seven mapped workload namespaces in the existing controller.
  Fixture namespaces may share `kognition-providers`; workload namespaces must
  remain distinct. The `default` namespace is never implicitly enrolled.
  Additional explicit mappings are supported for namespace references.
- Complete the reviewed governed platform upgrade, activation, Knowledge Gateway
  and Pgvector CPI adapter setup first. All bundled pgvector KBs currently
  consume `kognition-work`, so that is the required adapter namespace.
- Pre-provision `sample-db` and `sample-ca` in `kognition-work`, using the exact
  keys above. The installer checks key presence before writing sample resources.
  The operator must verify the scoped database roles, schema/table and TLS peer
  identity; key presence alone cannot prove these properties.
- Configure complete Foundry chat/embedding settings and Azure credentials in
  ignored `.env`. The embedding deployment must implement the reviewed
  1536-dimensional space. Profile values never contain database passwords.
- Rendered pgvector classes retain their names, claims, chunks and embedding
  references, replace legacy `vectorStore` with a governed `backendRef`, and
  create namespace-local `PgvectorBackend` objects with `verify-full` TLS.
  Inline knowledge remains inline. Agents retain claims without raw database or
  provider credentials. The legacy admin database Secret copy is bypassed.
- Source fixtures must pass bounded rollout and, for Blob/S3, exact inventory
  and content readback before credentials and KBs are applied. Failure prevents
  initial indexing. KB refresh is on demand and explicit index schedules are
  suspended. The Azure Blob, S3 and Kognition-docs samples use automatic initial
  KB indexing; the governed profile excludes their redundant named reindex demos.
  Review the corpus and embedding allowance before installation.
- The compatible governed profile omits `spec.probes.reasoningProbe` from Agents,
  including `monitored-agent`. Only bounded smoke calls are approved; recurring
  paid reasoning canaries remain outside this scope even when a ModelGrant
  authorizes model access. Health and readiness probes retain every setting.
  This rendering-only exclusion adds no profile switch or probe API field;
  unprofiled samples retain their original reasoning probes.
- Quotas use the explicit profile limits with a 24-hour reset. Supported ranges
  are agents 48..512, tools/workflows 32..256, input tokens 100000..10000000 and
  output tokens 32768..2000000. These bounds cover the historical 39-Agent bundle
  without unlimited grants. Existing unrelated quotas and admission safeguards
  remain authoritative and are not bypassed.
- `readinessTimeoutSeconds` is an explicit integer from 60 through 3600. The
  example allows 15 minutes for final full-bundle readiness after applying
  samples, including initial embedding/indexing. Size it for corpus volume,
  embedding deployment throughput and competing work. The legacy unprofiled
  final readiness budget remains five minutes; neither mode extends indefinitely
  or retries a terminal failed index run silently.
- Namespace rewriting includes Namespace names, resource/reference namespaces,
  RBAC subjects, namespace selectors and in-cluster service DNS. Generic names
  such as runtime class `default` remain unchanged. Helm, credentials, labels,
  readiness and scripted installers use the same map.
- The native `tool-sandbox` uses `fixtures.docsImage` in its mapped namespace.
  Full installation applies it before the other Workflow manifests and requires
  bounded rollout readiness before proceeding. Tool endpoint DNS references
  follow the same namespace map; there is no public/unpinned image fallback in
  the governed profile.
- The process-scoped `KOGNITION_SAMPLE_PROFILE_PATH` and explicit kubeconfig
  override are restored after success or failure. A governed reinstall preserves
  `hello-run-1` and its outcome; an unrelated run with that name causes refusal.
- This explicit profile changes rendered sample bindings only. It does not
  migrate/delete old tables or generations, rotate credentials, or perform a
  shadow claim cutover. Use it only after review of those sample identities.
  Profile-driven uninstall is not provided.
- External Azure Blob, the second-platform umbrella, recovery fixture and
  conflicting standalone autonomy Agent retain their existing bundle exclusions.
  Offline tests do not establish deployed retrieval or full-bundle readiness.

### Provider Reindex Demo Exclusions

The compatible governed bundle omits only these named `KnowledgeBaseIndexRun`
resources in the mapped default namespace:

| Sample | Excluded run | Automatically indexed KB |
| --- | --- | --- |
| `knowledge-base-blob-azure` | `azure-docs-reindex-now` | `azure-docs-kb` |
| `knowledge-base-blob-s3` | `s3-docs-reindex-now` | `s3-docs-kb` |
| `knowledge-base-kognition-docs` | `kognition-docs-reindex-now` | `kognition-docs-kb` |

The provider lifecycle seals each namespace, KB UID, generation and backend
configuration to one operation ID. A separate named run duplicates the initial
operation for that immutable generation. Manual provider reindex lifecycle
support is outside this explicitly approved compatible-bundle scope; there is
no new profile option or lifecycle change. Unprofiled installers and manifests
retain their named runs. The inline native `orbital-notes-reindex-now` demo also
remains included.

With `SAMPLE_PROFILE`, `knowledge-base-kognition-docs/indexrun.yaml` renders an
empty string. `Invoke-AzureSampleApply` and `Invoke-AzureSampleDelete` treat empty
rendering as a successful no-op. Callers piping rendered content directly to
`kubectl apply`, `label`, `get` or `delete` must skip empty content; kubectl cannot
operate on an empty document stream. Existing best-effort full-bundle labeling
ignores the absent document. Rendering and installation do not delete or retry
previously created named runs or alter their historical outcomes.

### Required Fixture Images

| Component | Reviewed source and process contract |
| --- | --- |
| `azuriteImage` | Mirror the reviewed Azurite image. Args: `azurite-blob --blobHost 0.0.0.0 --blobPort 10000 --location /data --skipApiVersionCheck --disableProductStyleUrl`. Service `azurite:10000`; account `devstoreaccount1` with a per-installation random key from Secret `azurite-account` (`AZURITE_ACCOUNTS`). |
| `s3Image` | Build [the checksummed SeaweedFS 4.49 recipe](../samples/knowledge-base-blob-s3/Dockerfile). Entrypoint `/weed`; args `mini -dir=/data -admin.ui=false -s3.port.iceberg=0 -s3.port.lance=0 -master.volumeSizeLimitMB=16 -volume.max=4`. `AWS_ACCESS_KEY_ID=minioadmin`, `AWS_SECRET_ACCESS_KEY=minioadmin`, `S3_BUCKET=product-docs`; public sample credentials, authenticated S3. Pod port 8333, compatibility Service `minio:9000`. No obsolete MinIO or unverified upstream image fallback. |
| `docsImage` | Shared reviewed Python 3.12 image for both docs and Tool HTTP servers. `python:3.12-slim@sha256:a6e34c598f2467ed0e9a8d349809fcd8b5c603269512df273a0bb1784edc11b1` is compatible by source/command inspection; neither fixture uses Alpine-specific packages or commands. Docs: `python -m http.server 8080 --directory /docs`, ConfigMap at `/docs`, Service `kognition-docs:80`. Tool sandbox: `python /app/server.py`, ConfigMap at `/app`, Service `tool-sandbox:8080`. Both run as UID 65532 and use only the standard library. |

The three image fields cover four native fixture Deployments using digest
references in the mapped
service namespace; they need no controller enrollment. Preserve manifest
nonroot/read-only/drop-capability settings and bounded fixture storage/resources.
Registry access, architecture compatibility, image review/publication and
platform ingress/egress allowances are operator prerequisites.

### Focused Offline Checks

```powershell
pwsh -NoProfile -File deploy\tests\azure-sample-settings-tests.ps1
pwsh -NoProfile -File deploy\tests\governed-sample-profile-tests.ps1
pwsh -NoProfile -File deploy\tests\azure-blob-provisioning-tests.ps1
python -B -m unittest discover -s samples\knowledge-base-blob-azure -p test_upload_docs.py
python -B -m unittest discover -s samples\knowledge-base-blob-s3 -p test_upload_docs.py
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -File ./deploy/tests/azure-sample-settings-tests.ps1
pwsh -NoProfile -File ./deploy/tests/governed-sample-profile-tests.ps1
pwsh -NoProfile -File ./deploy/tests/azure-blob-provisioning-tests.ps1
python3 -B -m unittest discover -s samples/knowledge-base-blob-azure -p test_upload_docs.py
python3 -B -m unittest discover -s samples/knowledge-base-blob-s3 -p test_upload_docs.py
```

## Compatible Bundle

| Sample | Install path | Primary coverage | Bundle status |
| --- | --- | --- | --- |
| `agentinstance` | PowerShell | Creates and invokes `hello-run-1`. | Included; live invocation verified. |
| `autonomy` | Manifest | Autonomy classes and trust profile. | Standalone Agent excluded because it conflicts on `research-agent`; Tool demos provision the autonomous class and UID-bound trust profiles separately. |
| `crossnamespace` | Manifest | Cross-namespace grants and namespaces. | Included. |
| `evaluation` | Manifest | Evaluation dataset, evaluation, and judge Agent. | Included. |
| `foundry-agent` | Manifest | Foundry-backed Agent configuration. | Included. |
| `full-stack-agent` | Manifest | Models, tools, memory, workflow, quotas, and policies. | Included. |
| `governance` | YAML files | Admission policy, quota, limits, workspace, and RBAC. | Included. |
| `helm-governance` | Helm | Isolated governance chart release. | Included as `sample-governance`. |
| `helm-multi-agent` | Helm | Isolated multi-agent workflow chart release. | Included as `sample-multi-agent`; generated credentials are reused. |
| `knowledge-base` | Manifest and supplemental YAML | Inline KnowledgeBase, claims, index run, and schedule. | Included. |
| `knowledge-base-blob-azure` | PowerShell | In-cluster Azurite Blob ingestion and pgvector retrieval wiring. | Included; verify actual upload counts, not only exit status. |
| `knowledge-base-blob-s3` | PowerShell | In-cluster S3-compatible storage ingestion and pgvector retrieval wiring. | Included; document upload is required to succeed. |
| `knowledge-base-kognition-docs` | PowerShell | Repository documentation served in-cluster and ingested into pgvector. | Included. |
| `memory` | Manifest | Memory classes and claims. | Included. |
| `memory-all-types` | Manifest | All memory claim types and memory-enabled Agents. | Included. |
| `memory-in-action` | Manifest | Memory-backed notes Agent and tools. | Included. |
| `multi-agent-workflow` | Manifest | Researcher, writer, reviewer workflow. | Included. |
| `openai-agent` | Manifest | OpenAI-compatible Agent configuration. | Included. |
| `probes` | Manifest | Runtime health and probe sample. | Included; governed rendering omits the recurring paid reasoning canary and preserves health/readiness probes. |
| `prompt` | Manifest | Prompt resources. | Included. |
| `routing` | Manifest | AgentServices and IntentGateway routing. | Included. |
| `simple-agent` | Manifest | Foundry model, grant, Agent, and explicit invocation. | Included; live invocation verified through Model Gateway. |
| `solution` | Manifest | Solution resources. | Included. |
| `tools` | Manifest | Tool classes and Tool definitions. | Included. |
| `tools-in-action` | Manifest | Tool-using Agent, policy, roles, and bindings. | Included. |
| `workflows` | YAML files | Conditional, parallel, quorum, retry, shared-memory, and HTTP tool workflow definitions. | Included. |

## Explicit Profiles

| Sample | Why it is separate | Install command | Validation status |
| --- | --- | --- | --- |
| `knowledge-base-blob-e2e` | Requires an Azure runner and cluster that can reach the Storage data plane. The profile supports public or private endpoint Storage by validating authenticated data-plane access. | `.\make.ps1 samples-provision-azure` then `.\make.ps1 samples-install SAMPLE=knowledge-base-blob-e2e` | Script parsing and safe preconditions verified locally. Real ingestion requires AKS or another private-network runner in this tenant. |
| `helm-umbrella` | Deploys a second full platform controller and cluster-scoped webhooks. | `.\make.ps1 samples-install SAMPLE=helm-umbrella` in an isolated cluster | Helm rendering verified. |
| `kagent-observability` | Standalone porting/recovery fixture with separate namespaces, platform and images. | Separate reviewed fixture installation. | Excluded from the local manual bundle; do not apply its cluster-scoped bridge objects through generic directory discovery. |

## Supporting Artifacts

| Artifact | Role | Bundle behavior |
| --- | --- | --- |
| `docs` | Documentation-only sample directory. | Excluded. |
| `governance-demo.yaml` | Root governance demonstration manifest. | Included. |
| `k8s-mode-demo.yaml` | Root Kubernetes mode multi-agent demonstration manifest. | Included. |
| `mem0-memory-class.yaml` | Root Mem0 MemoryClass manifest. | Included. |
| `model-grants.yaml` | Grants required model access to included sample Agents. | Included after sample resources. |
| `memory-embedding-values.yaml` | Helm values fragment for memory embedding configuration. | Reference configuration; not an independently applied sample. |

## Operating Notes

- Compatible Foundry samples require `AZURE_TENANT_ID`, `AZURE_CLIENT_ID`, and
  `AZURE_CLIENT_SECRET` in the ignored `.env`. `foundry-creds.yaml` is
  generated from those values and remains ignored.
- Use [the tenant configurator](../deploy/azure-samples/Configure.ps1) to recreate
  scoped inference and Blob-reader permissions after a tenant change. Its
  `-ValidateOnly` mode makes no changes; provisioning generates credentials and
  manifests without touching Kubernetes. See the
  [Azure setup procedure](bootstrapping.md#configure-an-azure-tenant).
- Sample installers read the Foundry deployment settings from `.env`, including
  Helm and script-backed samples. Kubernetes model aliases stay stable even when
  the actual Azure deployment changes. Raw reference YAML is not tenant-configured;
  use the installers or the generated `.azure/local-samples/models.yaml`.
- Profile-backed Azure samples also require an explicit
  `MS_FOUNDRY_INVOCATION_CONTRACT`. Their generic profiles are applied before
  their Models. The [profile guide](model-invocation-profiles.md) describes strict
  defaults, capability checks, migration and exact session/candidate binding.
- The real Azure Blob profile uses the service principal and container-scoped
  read RBAC. Legacy connection-string settings remain accepted by its installer;
  provisioning replaces them with Entra-based settings rather than a seven-day SAS.
- `Configure.ps1 -DeferBlobDataPlane` supports private-only governance without
  overriding network policy. It prepares the account/container/RBAC and marks
  upload and Blob verification pending private connectivity; it does not certify
  the real-Azure ingestion profile. Foundry and local emulator configuration
  remain available.
- Foundry project aliases are converted to Azure Cognitive Services deployment
  completion endpoints by the shared Foundry driver and Model Gateway. The
  gateway owns API-key or Azure-identity authentication so Agent runtime Pods
  do not receive direct provider credentials in gateway mode.
- The local bundle covers Azure Blob semantics with Azurite. It does not claim
  real Azure Storage connectivity from a local kind cluster under a
  private-only Storage policy.
- Initialize platform migrations and the separate KnowledgeBase `vector`
  extension before exercising affected samples. Keep failed runs as evidence and
  use new named index runs after fixing prerequisites; do not force status green.
- Select the repository Python environment explicitly for Azurite uploads. The
  helper can report `SKIP` with exit zero when the SDK is absent. Confirm actual
  uploaded documents; the current sample blob stores are ephemeral.
- The [Demo four-plane workload](../samples/trustworthiness/runbook.md)
  is a separate runner in a reviewed, enrolled scope; applying the compatible
  bundle does not execute its scenario.
