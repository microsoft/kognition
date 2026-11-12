# Sample Matrix

Status: local operating notes updated 2026-10-28; dated verification scopes below.

The strict local command is:

```powershell
.\make.ps1 samples-full-install
```

Run this only after approved activation with an explicit kubeconfig and reviewed
namespace enrollment. Pass `NAMESPACE`, `RELEASE` and `AGENT_NAMESPACE` for a
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
Hackathon rehearsal.

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
run. Supported governed sample installation is pending under T-0368 in the
[current backlog](../BACKLOG.md#43-next-work-queue-2026-10-29).

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
| `probes` | Manifest | Runtime health and probe sample. | Included. |
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
- The [Hackathon four-plane workload](../samples/hackathon-trustworthiness/runbook.md)
  is a separate runner in a reviewed, enrolled scope; applying the compatible
  bundle does not execute its scenario.
