# Remaining Sample Blockers

- Status: Implementation, selected-image qualification and isolated local manual
  acceptance complete. The recovery-enabled candidate is activated through the
  supported protocol; native and AutoGen governed retrieval pass. Authenticated
  real Azure Blob ingestion and cited native retrieval also pass.
- Updated: 2026-11-14. Implementation approved on 2026-10-24.
- Scope: Governed pgvector retrieval, local S3 and real Azure Blob acceptance.
- Security: [design controls](security-review-architecture.md).

## Recommendation

Complete pgvector retrieval through the existing Knowledge Gateway and CPI
provider contract. Resolve query embeddings inside the trusted service boundary
using generic `ModelInvocationProfile` contracts. Replace the unavailable MinIO
fixture with a qualified, digest-pinned SeaweedFS single-node S3 fixture.

This follows [ADR-0015](../../../adr/0015-provider-integration-boundaries.md),
[ADR-0008](../../../adr/0008-model-gateway.md) and
[ADR-0025](../../../adr/0025-model-invocation-contracts.md).
No new Azure model/Search deployment or tenant-wide RBAC expansion is required.
Real Blob acceptance reuses the administrator's existing account with a dedicated
private corpus container and a container-scoped reader assignment.

## Verified Constraints

- Indexing succeeded, but the legacy Python query embedder expects Azure
  credentials absent from Model Gateway-enabled Agent pods. Its error becomes
  an empty result and misleadingly reports no attached knowledge bases.
- The actual Agent NetworkPolicy permits DNS and gateways, not PostgreSQL.
  Supplying an embedding token alone would leave retrieval blocked.
- ADR-0015 prohibits raw backend credentials in Agent pods. Retaining direct
  database queries or adding blanket database egress would not meet that design.
- The existing pgvector package already has typed configuration, verified-TLS
  connection material, a pool factory, a scoped dense querier and an adapter
  command. The chart installs configuration by default; its explicit adapter
  profile now packages the authenticated provider and restricted routes.
- Legacy tables are scoped differently from the provider-owned schema. They
  cannot acquire a committed provider binding through a status edit.
- MinIO's canonical repository is archived and explicitly unmaintained.
  Rebuilding it would transfer maintenance and licensing obligations to us.
- The S3 sample uses CreateBucket, PutObject, ListObjectsV2 and GetObject with
  path-style addressing. It does not require MinIO administration APIs.

## Retrieval Design

1. **Keep the runtime thin.** `knowledge_search` sends query text and its
   session-scoped Knowledge capability to the Knowledge Gateway. Remove database
   DSNs, model provider endpoints and credential material from governed runtime
   bindings. Cover both native Python and AutoGen paths.
2. **Make dense text queries explicit.** Add a validated text-to-vector request
   path to the gateway frontend. Existing explicit-vector and keyword requests
   retain their behavior. Resolve the configured retrieval mode and embedding
   Model server-side; reject conflicting text/vector fields and unsupported
   modes before embedding. Do not call dense search `keyword`.
3. **Reuse the model boundary.** Add a shared server-side embedding execution
   path that uses live ModelGrant/AgentPolicy checks, exact Model/profile binding,
   `cpi.ResolveInvocation`, `PrepareEmbedding` and the registered CPI adapter.
   Derive the embedding Model from the authorized KB/class configuration.
   Do not relax the chat core's current-Model check, reuse a chat capability for
   another Model, or duplicate Foundry parameter rules.
4. **Finish the provider path.** Wire pgvector registration, authenticated CPI
   transport, deployment, namespace-scoped RBAC and restricted routes. Complete
   staged indexing, committed binding publication, Delete/Retain and recovery
   needed by the selected samples. Advertise only implemented capabilities;
   the existing querier supports dense retrieval.
5. **Keep credentials server-side.** Preserve PostgreSQL `verify-full`, provision
   appropriate database TLS and least-privilege roles, and validate peer identity.
   Only the adapter receives database access; only trusted embedding execution
   receives Foundry credentials. Keep Agent egress restricted to gateways.
6. **Expose real failures.** Distinguish authorization, embedding, provider and
   database failures from a valid zero-match search. Preserve sanitized errors,
   request IDs, initiating Agent/session identity, quota decisions and durable
   audit evidence. Do not return a successful empty result on infrastructure
   failure.

Prefer the shared in-process model execution core for the trusted gateway.
A public `/v1/embeddings` facade can be a separate slice if a consumer needs it;
it is not a prerequisite for runtime knowledge search.

## Data And Compatibility

- Preserve legacy stored manifests and their existing explicit legacy mode.
  Supply a reviewed translation/migration path rather than silently rewriting
  a class or switching its backend.
- Introduce versioned provider-backed classes, namespace-local PgvectorBackend
  configurations and shadow KBs for the affected samples. Keep original indexes,
  runs and evidence until the replacement query path passes.
- The provider schema must enforce namespace, KB UID, committed generation and
  configuration identity/digest. Normalize citations to the existing CPI URI
  contract, including S3 sources; never return signed URLs or credentials.
- Prefer a controlled, named reindex into the new scoped schema over an ad hoc
  backfill that cannot prove ownership. Review its embedding budget first.
  Do not rerun the entire sample installer or overwrite original index runs.
- Match the index and query embedding contract, Model/profile binding and vector
  dimension. Fail explicitly when incompatible; never mix vector spaces merely
  because their dimensions match. Switch existing claims only after validation.

## Compatible Bundle Indexing Scope

The approved governed bundle relies on automatic initial indexing for Azure
Blob, S3 and Kognition-docs KBs. It excludes only their bundled named reindex
demos: `azure-docs-reindex-now`, `s3-docs-reindex-now` and
`kognition-docs-reindex-now` in the mapped default namespace. The provider
lifecycle seals each namespace, KB UID, generation and backend configuration to
one operation ID; these additional runs would duplicate the operation for the
same immutable generation. Implementing manual provider reindex lifecycle
support is outside this compatible-bundle scope.

Unprofiled behavior and the inline native reindex demo remain unchanged.
The [sample matrix](../../../sample-matrix.md#provider-reindex-demo-exclusions)
documents the exact run/KB pairs and the empty rendering of the docs-only
`indexrun.yaml`. Shared apply/delete helpers skip empty streams successfully;
direct kubectl consumers must also skip them. Historical duplicate-run failures
remain preserved; this source fix does not retry or remove them.

Focused offline rendering and installer regressions pass. They do not establish
current deployed full-bundle acceptance or change the platform/provider lifecycle.

## S3 Fixture

The [explicit governed sample profile](../../../sample-matrix.md#governed-installer-profile)
now renders namespace-local backend references, scoped Secret/TLS references,
reviewed fixture digests and bounded quotas through the supported installer.
Focused offline checks cover profile rejection, namespace integrity, source
ordering, upload verification and legacy compatibility. This software coverage
does not establish a successful deployed full-bundle run or retrieval acceptance.

Use the qualified **SeaweedFS 4.49 source build** with upstream `weed mini` in a
single Pod. Keep the Kognition source provider as `s3`; no SeaweedFS-specific core
API is needed.
Retain the existing Service address as a documented compatibility alias where
useful; use a generic fixture manifest name and expose only the S3 port.

The initial published 4.48 candidate was rejected after scanning found six High
vulnerabilities. The [source-build recipe](../../../../samples/knowledge-base-blob-s3/Dockerfile)
pins the official 4.49 commit and archive checksum, uses a patched Go toolchain
and a scratch runtime, and retains license and linked-package evidence.
Its exact linux/amd64 image passed authenticated six-document upload/readback,
three-page listing, invalid/anonymous credential rejection and repeat-upload
idempotency within the one-CPU/512-MiB budget.

The fresh scan found no rated vulnerabilities in this replacement S3 image.
The deprecated OpenPGP module advisory remains visible with explicit
not-affected analysis: the build rejects imports of the affected packages and
records the actual linked dependency graph. This qualification does not grant
production clearance or accept risks in the other candidate images.

Require nonroot execution, read-only root filesystem, dropped capabilities,
bounded writable data/tmp mounts, authenticated S3, and no exposed admin/control
ports. Test the current one-CPU/512-MiB fixture budget before accepting it.
Record source/release identity, digest, platform, license, SBOM and scan evidence.
If qualification fails, reassess explicitly; RustFS is an available maintained
alternative, not an automatic fallback.

Harden the existing upload/install flow:

- Require an explicit kubeconfig/cluster; never select the first kind cluster.
- Separate image availability from bounded workload readiness. Avoid importing
  absent architectures from a partial multi-platform image archive.
- Bind upload forwarding to loopback, verify the owned forward is responsive,
  and clean up its exact PID.
- Fail on missing dependencies, missing inputs, failed uploads or unexpected
  bucket errors. Verify the exact expected key set and retrieved content before
  creating the index run; a zero exit code alone is insufficient.
- Suspend explicit and controller-generated schedules before they can produce
  recurring work.

## Implementation Order

| Work package | Deliverable | Prerequisites |
| --- | --- | --- |
| A | Reproducing retrieval tests, compatibility mapping and upgrade/drain feasibility | Existing evidence |
| B | Pgvector lifecycle/schema, verified TLS and deployable provider package | A |
| C | Governed text-to-dense query, generic embedding execution and runtime bindings | A; integrate with B |
| D | SeaweedFS qualification and strict S3 upload/install flow | Independent of B/C |
| E | Shadow-index migration, claim cutover and three-KB retrieval acceptance | B, C; S3 also needs D |
| F | Frozen candidate, guarded deployment and regression checks | B through E |

Use [the existing provider components](../../../../providers/pgvector/),
[adapter command](../../../../cmd/kognition-knowledge-pgvector/),
[gateway](../../../../internal/knowledgegateway/),
[shared CPI resolution](../../../../internal/cpi/invocation.go) and
[S3 fetcher](../../../../runtime/ingester/ingest.py).
Update charts, sample rendering, CLI/error presentation and documentation with
their owning changes.

## Acceptance Gates

- A real Agent query against each of the Azure-emulator, repository-docs and S3
  KBs returns an expected passage and valid citation. Verify tool results,
  not just Agent Ready status. Query vectors contain exactly 1,536 finite values.
- Negative tests cover revoked claims/grants, stale or recreated configuration,
  profile replacement, wrong dimensions, cross-namespace/KB/generation data,
  invalid TLS peers, unsupported modes and provider unavailability.
  Rejected requests make zero embedding/provider calls where rejection precedes
  execution. Failed refresh retains the prior committed generation.
- Agent Pods contain neither Azure/model nor database credentials and cannot
  reach provider/database endpoints directly. Current namespace/model quotas and
  audit requirements apply to server-side embedding and retrieval work.
- Real S3 checks cover authentication, bucket operations, pagination, exact
  source-file inventory/content, prefix include/exclude rules and successful
  indexing. Repeating upload does not create unintended duplicate objects.
- Native and AutoGen retrieval, inline knowledge, chat, calculator, sample memory,
  existing profiles and the representative workflow retain behavior.

Run targeted Go/Python/provider-database and Helm tests first, then bounded,
opt-in live Foundry and deployed acceptance. Record provider requests and token
usage. Do not expand live coverage by replaying all historical workflows.

## Deployment And Recovery

Verify upgrade feasibility before stopping the working environment. The
[installer contract](../../../features/knowledge-provider-execution/installation-recovery-contract.md) records a retained
Helm-owned default MemoryClass drain limitation and the implemented
exact-UID/content/ownership retention rule. An isolated candidate cluster
preserves the original working cluster and indexes. Do not strip finalizers
or bypass drain.

Back up data and binding identities, freeze writers, stage immutable images and
reviewed schemas/certificates, and follow the supported inactive-upgrade workflow.
Rebuild only affected images, including shared controller/guard/webhook consumers
and both Python runtimes when their code changes. Package the Knowledge Gateway,
pgvector adapter and S3 fixture explicitly.

Require fresh scans, deployed gates and
[exact-target approval](../../../features/knowledge-provider-execution/activation-transaction-contract.md). Existing approval
and local vulnerability exceptions do not authorize different image digests.
After activation, apply only the reviewed sample/migration delta, verify claim
cutover and keep recurring work paused. Recover through the original transaction
journal/artifacts on failure; do not use direct Helm rollback or replay consumed
approval records.

## Explicit Shadow Migration

The [manual shadow bundle](../../../../deploy/examples/knowledge-pgvector/manual-shadow.yaml)
is outside automatic sample installation and removal. It declares a versioned
PgvectorBackend/class, three shadow KBs and claims, three native Agents, one
AutoGen Agent, and explicit chat plus embedding Model grants. It leaves original
resources unchanged and declares no recurring schedules or extra IndexRuns.

Before applying it, provision the reviewed provider/database TLS, administrator
schema migration and least-privilege database role; configure Knowledge Gateway
registration; prepare the referenced namespace-local Secrets and Models; and
verify the Azurite, repository-document and S3 fixtures. The `gpt-6-luna` Model
must resolve to the reviewed deployment with the same resource name and
invocation profile. Creating the three KBs starts paid initial indexing.
Review the source inventory and embedding budget first.

Configure `runtimeRecovery.existingSecret` before installing AutoGen workloads.
Its immutable, namespace-specific encryption keys are separate from runtime
control and approval-binding keys. The manual retrieval Agents declare a
180-second execution budget. The complete combined sample bundle also needs
sufficient declared Agent quota and exact MemoryGrant/identity-bound Memory
roles for builtin claim-backed access; sandbox Tool grants do not authorize it.

The permanent contract test and candidate API-server dry-run accepted all
20 resources. Dry-run persisted no KBs or Agents and performed no indexing.
Actual indexing and cited retrieval now pass in the isolated candidate:

| Corpus | Documents | Chunks | Installed Agent acceptance |
|---|---:|---:|---|
| Azure Blob emulator | 6 | 23 | Native, cited launch regions and unavailable Brazil South |
| Repository documentation | 4 | 1405 | Native and AutoGen, quoted manifest thesis |
| S3 fixture | 6 | 23 | Native, cited introductory and subsequent pricing |
| Real Azure Blob Storage | 6 | 23 | Native, both prices quoted with the real Blob HTTPS citation |

Claims bind to their new scoped generations. Original-cluster KBs and indexes
remain unchanged. The separate `azure-live-docs-kb` and `azure-live-docs-agent`
use the existing governed class and new account-only source reference.
`azure-docs-agent-v2` remains the Azurite example.

On 2026-10-28, the administrator-enabled `sakognitiondatasource` account passed
real public-endpoint acceptance with HTTPS/TLS 1.2, anonymous access disabled
and shared-key access disabled. The older `kognsamples76a292ed` account remains
private-only and unchanged. A dedicated private `product-docs` container holds
the six verified source files under `mark7/`; the existing user container was
not modified. The Kognition service principal has container-scoped Storage Blob
Data Reader. Its six downloads matched source SHA-256 values, access to the
unrelated container was denied, and anonymous Blob access returned
`PublicAccessNotPermitted`. The temporary container-only uploader role was
removed.

Exactly one ingestion Job produced the new generation and 23 verified
1536-dimensional database vectors. The actual Agent completed one
`knowledge_search`, returned five in-scope chunks and cited the real pricing
Blob. That invocation reported 2157 tokens. The Agent has no raw Azure/database
credentials and cannot connect directly to Blob Storage or PostgreSQL.
Existing local configuration now selects the verified account, with a protected
backup of its previous values. The legacy real-Blob sample installer still
targets the direct-vector-store profile; it was not run against this governed
manual deployment.

## Sources And Qualification Status

- [Knowledge provider contract](../../../knowledge-providers.md) and the current
  provider/command source establish reusable components and remaining wiring.
- [MinIO upstream](https://github.com/minio/minio) explicitly marks itself
  unmaintained; its canonical repository is archived.
- [SeaweedFS upstream](https://github.com/seaweedfs/seaweedfs),
  [4.49 release](https://github.com/seaweedfs/seaweedfs/releases/tag/4.49) and
  [release command source](https://github.com/seaweedfs/seaweedfs/blob/4.49/weed/command/mini.go)
  establish the maintained fixture.
- [Azure embedding guidance](https://learn.microsoft.com/en-us/azure/ai-foundry/openai/how-to/embeddings)
  documents Entra-authenticated embeddings and compatible dimensions.

Verified coverage includes the full Go suite, affected race tests, actual
PostgreSQL lifecycle and verified-TLS tests, and 505 Helm regression assertions.
The remediated images passed 175 native tests with one expected optional-SDK
skip, 204 AutoGen tests and 76 ingestion tests. Complete installed SDK versions
remain unchanged; both system and certifi trust stores exclude the affected
CA roots. All four actual provider database/TLS suites pass against PostgreSQL
16.15 and pgvector 0.8.7 without skips.
A bounded real Foundry/CPI/PostgreSQL tracer bullet returned a known cited passage
and denied revoked access without another provider call, using two requests and
37 tokens. Its authorization state was a local fixture, not deployed acceptance.

The initial Debian runtime and old database candidates were rejected.
Remediation qualified 15 exact application, database, S3 and helper images with
zero Critical, High, Medium or Low findings, without scanner suppression.
The remaining deprecated OpenPGP module advisory is explicitly not affected:
actual compiled dependency graphs exclude its packages. The Go producer and
ingestion transport executables remained byte-identical across base-only changes.
The later KnowledgeBaseClaim webhook correction intentionally rebuilt the shared
controller/guard/webhook executable and passed handler, conversion, admission,
image and deployed checks.

The local Python alternative uses pinned Alpine CPython 3.12.15 with 111
hash-verified offline wheel artifacts and the previously qualified SDK versions.
Default repository Python build recipes remain Debian-based; this local
qualification does not grant those different images production clearance.
The database preserves PostgreSQL major 16, UID/GID 999 and its existing
features; its bundled gosu helper is rebuilt with patched Go and dependency
versions, including passing upstream parser tests.

Frozen charts render deterministically and pass the supported installer's
post-render guard. The inactive core/provider/shadow resources previously passed
strict server dry-run without persistence. Candidate image import verifies the
copied archive, original OCI descriptor and independent CRI configuration
identity. The supported inactive bootstrap completed, all 34 primary database
migrations completed, and the actual provider password, database TLS and denied
unsafe privileges passed live verification. Required deployed admission,
attachment, stdio and ingestion checks pass against this candidate. The ingestion
fixture uses an owned PGDATA child directory so qualification does not depend on
running PostgreSQL as root.

The current core release is revision 3 with encrypted durable AutoGen recovery.
Fresh deployed gates and an exact-target operator review preceded activation.
Each approval and transaction remains separate; consumed approvals were not
replayed. Completed installation claims were retired through supported
settled-request recovery while inactive.

Local acceptance also passed inline knowledge, chat, the actual calculator
result 391, sandbox memory, builtin claim-backed memory write/readback and a
five-Agent workflow with five successful child jobs and `MVP1_COMPLETE`.
Live embedding-grant revocation failed explicitly and authorized retrieval
worked after restoration. Actual Agent Pods have no provider/database credentials,
cannot connect directly to PostgreSQL or the provider, and have no Kubernetes
service-account token. API-server negative cases cover retrieval thresholds,
profile limits and namespace-local claim resolution.

The candidate uses an explicit 64-Agent capacity for the aggregated examples;
token budgets and admission enforcement remain enabled. Twenty exact
Agent/declared-claim MemoryGrants and identity-bound Memory get/update roles
authorize builtin memory access. A separately recorded candidate-only migration
widens opaque memory identifiers to text; it does not add a framework migration
or change the count of 34 primary migrations.

Before the recovery configuration upgrade, a real PostgreSQL archive preserved
the first 1451 vectors. Their committed binding pinned Delete, so a later class
Retain edit could not change cleanup. All original vector rows were restored and
verified in a separate private preservation database, without resurrecting active
provider state or editing status/finalizers. One earlier short-budget AutoGen
diagnostic retains its uncertain usage reservation and was not replayed;
distinct longer-budget and governed-retrieval invocations passed.

The tested Go 1.27.3 Windows CLI and kubectl plugin are installed with previous
binary backups. The dedicated governed launcher selects the candidate for one
terminal without changing the persistent global context or original launcher.
Recurring knowledge and evaluation schedules remain paused. This is local manual
qualification, not production, AKS or native Azure AI Search clearance.

## Sample Model Name Alignment

The current sample chat Model is named `gpt-6-luna`, matching its deployment.
The manual cluster uses that name in all three chat Model namespaces and all
42 Agents, both Solutions, ModelGrants, memory summarization and cross-namespace
authorization. The old active alias and temporary migration grants are removed.
Static examples, generated local Models, Helm values and sample apply/delete
paths use the same configured resource identity. The embedding Model and all
five committed knowledge bindings are unchanged; no reindex was requested.

The deployed controller could not update existing compiled Solution children
without submitting obsolete compiled UIDs. The two unused examples had no memory
records or work-driven execution history and were normally redeployed with their
new desired Model reference. Other example resources and stored data were
preserved. The source fix now retains actual child metadata, finalizers and
status during Solution updates, with a red/green Kubernetes-adapter regression
and both affected controller packages passing. This source fix is not yet in
the running controller image; no platform image or guarded installation changed.

Renamed native chat and real-Blob cited retrieval passed. A distinct AutoGen
query completed and returned scoped, cited repository content; it correctly
reported that the requested specific passage was absent from its results.
An earlier AutoGen check retained its protected uncertain reservation without
replay. A restarted tool gateway also encountered live claimant validation failure;
replacing only the exact idle serving Pod restored actual calculator execution
for a distinct `19 * 23 = 437` request. Original unavailable invocation records
and enforcement remain intact.
