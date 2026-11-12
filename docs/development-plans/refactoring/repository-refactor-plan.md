# Kognition Repository Refactor Plan

- Status: Proposed architecture and migration plan, revised to a single-repository model (see "Scope Decision"); implementation is not authorized by this document.
- Date: 2026-11-08.
- Review revision: 0.2, reviewed against source and accepted contracts on 2026-11-08.
- Inspected HEAD: `2cf681faa7ae7292e857b502496732a3f9b93df3`.
- Evidence scope: Current source and documentation, including uncommitted changes. This is not a tested release baseline.

## Stabilization Follow-Up

The 2026-11-09 continuation repairs existing core behavior without implementing
the repository split. Verified changes include per-volume pgvector dimensions
with preserved legacy data, guarded retirement of exact completed migration Jobs,
and capability-bound Python Ollama request construction. The local Kubernetes
profile also requires explicit model, Tool and memory-service backend peers;
image availability alone does not establish local execution.

Follow the [installation and recovery contract](../../features/knowledge-provider-execution/installation-recovery-contract.md)
for the migration and network boundaries. Fresh exact-target acceptance and
operator authorization still precede activation. These stabilization results do
not approve extraction, publication, production deployment or full functional
parity.

## Scope Decision: Single Repository

Decision (2026-11-10, owner approval): Kognition stays in **one repository, `kognition`**. No `kognition-azure`, `kognition-aws` or samples repository will be created. The refactor delivers the same boundaries as in-repository modules, and "extraction" in the sections below means moving code into a separately buildable, separately tested module or package directory inside this repository.

What this changes:

- **Boundaries:** the core, each provider integration (Azure, AWS/Bedrock/S3, Anthropic, Mem0) and the samples become independently buildable units with their own module manifests, dependency graphs, charts and test runners. Core must build, test and install with those directories absent.
- **Dependencies:** the core `go.mod` must not require Azure, AWS or Anthropic SDKs. Provider code lives in nested Go modules with their own `go.mod` files, wired to core only through the public CPI/SDK contract. A `go.work` file may help local development but cannot be required by CI or releases.
- **Releases:** one repository version and one immutable release set cover core, integrations and samples. Per-integration chart/image artifacts keep their own digests inside that set. Repository-to-repository compatibility matrices, cross-repository history publication and sibling-checkout tests are not needed.
- **Enforcement:** because nothing prevents an accidental import across a directory boundary except tooling, CI must enumerate every module and run a dependency-boundary check (core imports no provider module; providers import only public contracts, never `internal` packages; core tests and builds never read sample or provider paths).
- **Planning:** a single GitHub Project or board tracks the work. Ownership is expressed through CODEOWNERS paths, not repository permissions.
- **Layout (decided 2026-11-10):** provider integrations live in `integrations/<name>/` (`azure`, `aws`, `anthropic`, `mem0`) and the samples in `samples/`, each a nested module. The existing `providers/pgvector` stays the local reference provider and is reviewed under RP02 for whether it moves under `integrations/` or stays in core.

What does not change: the verification matrix, local capability qualification, data and key preservation, cutover and rollback, activation approval boundaries, and the requirement that no baseline test vanishes when code moves. Where the text below says repository, extracted repository, sibling repository, cross-repository or "published release" for an integration, read it as the in-repository module, its directory boundary and the single release set. Tasks RP10 to RP13 and RP15 are restated for this model in the task table.

This decision does not authorize publication, a release, production deployment or the removal of any provider. The open-source approval covers publishing the single repository; publication still requires reviewed history, notices and restricted-material handling.

## Review Verdict And Readiness

The module-boundary refactor is a reasonable target. The reviewed plan supports starting read-only baseline work and decision preparation. It does **not** establish readiness to move provider code, replace provider execution, publish artifacts, or migrate a running installation.

| Boundary | Current readiness | Required evidence before proceeding |
| --- | --- | --- |
| RP01 baseline inventory | Ready to start | Preserve ongoing changes; inspect only approved sources and isolated test targets |
| RP02 design approval and work allocation | Ready to prepare | Baseline inventory, named owners, accepted boundary decisions, exact supported combinations, and approved budgets |
| Runtime/packaging implementation | Blocked pending RP01/RP02 | Reproducing tests, accepted contracts, and reviewed test/dependency inventory; start with staged modules inside this repository |
| Module extraction | Blocked | Candidate core and staged integration modules pass the prerequisite component, local, compatibility, and ownership gates |
| Public release or deployment cutover | Blocked | Exact release-set, publication, data-restore, target acceptance, and freshly authorized recovery/activation evidence |

Review closed planning gaps in phase ordering, overlooked external memory/embedded UI surfaces, standalone knowledge wiring, module/test enumeration, protocol effect safety, identity/key continuity, and rollback approval semantics. Remaining implementation and qualification work is explicit below. Unknown evidence is a blocker, not an implicit pass.

## Recommendation

Adopt a cloud-independent core, a samples module, and independently installable provider integrations, all inside the single `kognition` repository. Establish and test the module boundaries first, then move code behind them. Moving folders before the contracts exist would preserve the current coupling and only relocate it.

Keep `kognition` as the one canonical repository. Provider integrations live under their own directories as separately buildable modules (for example `integrations/azure` and `integrations/aws`; exact paths are an RP02 decision). One GitHub Project tracks the work.

The target is the existing standalone and Kubernetes platform with real local inference, tool calling, embeddings, and persistence. Source-only repository moves should cause no deployment downtime. Provider routing changes require an explicit, rehearsed maintenance transaction.

### Confirmed scope

| Decision | Rationale |
| --- | --- |
| Core operates without cloud accounts, credentials, or services after setup | Local prerequisites and initial dependency/image/model downloads are acceptable |
| An offline bundle is optional and separately certified | Air-gapped installation is not a prerequisite for the first cloud-independent release |
| Both standalone and Kubernetes modes remain in core | Kubernetes is a deployment mode, not an Azure integration |
| Studio, workflows, gateways, identity, governance, memory, and knowledge remain in core | The refactor must preserve the platform surface |
| Local capability parity uses real execution | Mocked success cannot establish complete functionality |
| Cloud-specific services and model-quality differences remain integration concerns | Capability parity does not mean identical model answers or every vendor-specific feature |

### Terminology

- **Core:** The complete cloud-independent platform. Use this term instead of alternating between core and base.
- **Provider integration:** An optional package implementing backend capabilities and provider-specific configuration. "Plugin" describes this package, not a Go shared-library loader.
- **Local profile:** A documented installation using local inference, persistence, identity, and observability.
- **Release set:** An immutable compatibility manifest identifying core, integration modules, samples, charts, protocols, and image digests for one repository version.
- **GitHub Project:** The work board for this repository's issues and pull requests.

## Findings From The Current Repository

| Finding | Source evidence | Consequence |
| --- | --- | --- |
| The module declares Azure SDK dependencies | [go.mod](../../../go.mod) | Disabling Azure configuration does not produce a provider-independent dependency graph |
| Cloud model drivers are compiled into the API server, both controller assemblies, Model Gateway, and memory embedder | [API server](../../../cmd/kognition-apiserver/main.go), [standalone controller](../../../cmd/kognition-controller-manager/main.go), [Kubernetes controller](../../../cmd/kognition-k8s-controller/main.go), [gateway driver assembly](../../../internal/modelgateway/invocation_transport.go), [memory embedder](../../../internal/memory/embed/embedder.go) | Extraction must change composition and every consumer, not only move the driver directory |
| Core knowledge wiring imports Azure Search configuration and recognizes specific backend kinds | [Kubernetes knowledge assembly](../../../internal/k8s/controller/knowledge_reconciler.go), [Knowledge Gateway](../../../cmd/kognition-knowledge-gateway/main.go) | Typed backend validation and dimension discovery need a provider-neutral boundary |
| A secure knowledge transport already exists; registration remains an in-process allowlist | [knowledge contract](../../knowledge-providers.md), [SDK transport](../../../pkg/pluginsdk/knowledge_transport.go) | Extend existing CPI/SDK contracts; do not introduce a competing plugin framework |
| The model protobuf contract is narrower than the in-process interface | [model protobuf messages](../../../proto/kognition/v1alpha1/cpi.proto), [in-process driver](../../../internal/cpi/driver.go) | Structured tool calls/results, profile binding, embedding usage, and capability negotiation need explicit transport parity |
| Python base dependencies include Azure, Anthropic, and AWS packages | [agent requirements](../../../runtime/agent/requirements.txt), [ingester requirements](../../../runtime/ingester/requirements.txt) | Split dependencies, credential selection, source fetchers, and tests; lazy imports alone do not clean the distribution |
| The legacy Python configuration still defaults to a public OpenAI endpoint; stabilization now accepts capability-bound `ollama` requests through Model Gateway | [runtime configuration](../../../runtime/agent/config.py), [provider factory](../../../runtime/agent/providers.py) | An explicitly configured local profile and real deployed qualification remain required; factory acceptance does not establish full parity |
| Ollama already implements chat and embeddings, but its embedding response omits usage | [Ollama driver](../../../internal/cpi/drivers/ollama/driver.go), [governed embedding executor](../../../internal/modelgateway/embedding.go) | The governed query executor requires reported usage; current Ollama embeddings are not sufficient for that path |
| Signed model-candidate promotion requires exact input token counting; the Ollama driver does not implement that capability | [candidate reconciler](../../../internal/controller/model_class_reconciler.go), [runtime binding contract](../../runtime-binding-profiles.md) | Local promotion needs an exact supported tokenizer/contract or another qualified local backend; estimated or fabricated usage is unacceptable |
| The nominal minimal sample uses Foundry | [simple agent](../../../samples/simple-agent/manifests.yaml), [sample matrix](../../sample-matrix.md) | "Local cluster" currently does not imply "cloud-independent agent" |
| Build wrappers source sample helpers and build cloud binaries; release packaging publishes Azure Search with core | [PowerShell wrapper](../../../make.ps1), [Makefile](../../../Makefile), [Dockerfile](../../../Dockerfile), [release workflow](../../../.github/workflows/release.yml) | Build, install, chart, and release ownership must change together |
| Tests depend on repository-relative samples and checked-in umbrella archives | [sample/archive tests](../../../internal/traceability/sample_hardening_test.go), [build-entrypoint tests](../../../internal/traceability/build_entrypoints_test.go) | Move tests with their owners or replace inputs with explicit pinned fixtures; never replace failures with skips |
| Mem0 is also an external backend and defaults to a hosted service | [memory composition](../../../internal/memory/serviceapp/app.go), [Mem0 adapter](../../../internal/memory/mem0/adapter.go) | Classify and package external memory explicitly; removing Azure/AWS model code alone does not cover every external backend |
| The CLI embeds cloud-backed gallery examples separately from the samples directory | [gallery loader](../../../cmd/kognitionctl/cmd/gallery.go), [starter gallery manifest](../../../cmd/kognitionctl/cmd/gallery_assets/hello-agent.yaml) | Audit embedded examples, import behavior, browser assets and defaults; moving samples does not clean the shipped CLI |
| Standalone controller assembly omits Knowledge Gateway settings/issuer; the gateway still constructs Kubernetes readers and validators | [standalone dispatcher](../../../cmd/kognition-controller-manager/runtime_dispatcher.go), [Knowledge Gateway assembly](../../../cmd/kognition-knowledge-gateway/main.go) | Standalone governed retrieval requires explicit state/configuration/binding/capability wiring; a local pgvector process alone is insufficient |
| Standalone operating notes do not fully describe current dispatcher behavior | [Compose notes](../../standalone-compose.md), [Compose overlay](../../../docker-compose.http-runtime.yml), [external dispatcher](../../../internal/k8s/dispatcher/external.go) | Current source supports provider-backed claims and AutoGen selection, while the documented named-host HTTP runtime conflicts with literal-loopback HTTP enforcement; reproduce and resolve the exact supported profile |
| Azure adapter commands themselves import core internal binding helpers | [Azure Search adapter assembly](../../../cmd/kognition-knowledge-azuresearch/main.go), [authorization](../../../cmd/kognition-knowledge-azuresearch/authorize.go) | Public SDK extraction must cover provider-side binding validation, not only model Driver types |
| SecretRef validation supports internal/env backends; named Vault fields are compatibility examples, not an implemented Vault integration | [SecretRef contract](../../../api/v1alpha1/config.go), [reconciler](../../../internal/controller/secretref_reconciler.go) | Separate implemented behavior from modeled/aspirational providers; schema or Ready status alone is not proof of backend execution |
| Many visible root artifacts are already ignored and untracked | [.gitignore](../../../.gitignore) and tracked-file inventory | Workspace pollution and tracked-repository pollution are different problems |
| Current operational acceptance is incomplete and target-specific | [backlog status](../../../BACKLOG.md), [governed sample follow-up](../features/knowledge-provider-execution/remaining-sample-blockers-plan.md) | Establish a fresh baseline; historical completion markers cannot certify this refactor |

The current canonical registry contains 51 kinds. Kubernetes excludes the two standalone ControlPlaneRole kinds. Generate the migration inventory from [the registry](../../../api/v1alpha1/register.go); the README's historical "43 kinds" wording is not the acceptance inventory.

## Repository Names And Responsibilities

| Recommended repository | Responsibility | Alternatives and tradeoffs |
| --- | --- | --- |
| `kognition` | Canonical core source, CLI/Studio, runtime profiles, contracts/SDK, local persistence, core charts, operational docs, tests | `kognition-core` is clearer in an organization-wide list, but introduces repository and documentation migration. `kognition-base` can suggest reduced functionality; avoid it |
| `kognition-samples` | Provider-neutral tutorials and runnable examples, plus clearly labeled Azure/AWS scenarios using published integration releases | `kognition-examples` is good for code examples; `kognition-samples` better covers tutorial scripts and walkthroughs |
| `kognition-azure` | Foundry/Azure OpenAI, Azure AI Search, Blob sources, Azure identity adapters, AKS/ACR/network infrastructure, install scripts and provider docs | Prefer this broad name to `kognition-foundry` when Search, Storage, and AKS belong here |
| `kognition-aws` | Bedrock, AWS identity, S3 source integration, and future AWS deployment assets | Use `kognition-bedrock` only if the repository will deliberately remain Bedrock-only |
| `kognition-anthropic`, when maintained | The existing direct Anthropic backend, dependencies, compatibility tests, and install package | Do not lose existing functionality while removing cloud code from core |
| Optional `kognition-mem0` package/repository | Existing hosted or self-hosted Mem0 memory integration and its compatibility tests | Approve ownership under RP02; keep it outside the baseline distribution without removing existing Mem0 users' migration path |

Do not create separate `kognition-sdk`, `kognition-ollama`, Kubernetes, Studio, or documentation repositories initially. Keep the small public SDK and the baseline local backend with core. Split further only when independent ownership and release needs justify the overhead.

Changing the repository name does not require simultaneously changing `github.com/kognition-ai/kognition`, API groups, binary names, or resource identities. Preserve those contracts during this migration. Any later module-path change needs its own compatibility decision.

### Compatibility policy

- Preserve accepted manifests, provisioner aliases, CLI flags, configuration keys, resource/status semantics, checkpoint formats, and durable effect receipts within the declared migration window.
- Keep provider-specific legacy API fields readable through explicit compatibility translation. Do not delete existing cloud source enums or rewrite stored objects simply because their implementations move.
- Legacy direct-provider runtime profiles need a tested migration to governed execution or an explicitly versioned compatibility package. Removing their Python imports without addressing installed runtime bindings would break users.
- Existing sample/build convenience commands need thin compatibility dispatch to an explicitly installed, pinned package, or a documented deprecation window. They must not depend on a sibling source checkout or silently download/run unreviewed scripts.
- Publish exact tested release combinations first. The proposed policy supports the current and previous compatible core release; do not imply arbitrary semantic-version ranges, especially for alpha contracts.
- A pre-refactor release without the new provider protocol cannot be declared compatible with a new adapter. Preserve a separately qualified legacy distribution/bridge or require an explicit core-first upgrade; advertise only combinations actually tested.
- Use a feature-gated transition for new routing and preserve the old qualified release set until rollback passes. Removing the transition is a separate reviewed release change.
- Record repository/import/image/chart/URL/command aliases and redirects explicitly. Repository redirects do not preserve OCI tags, chart locations, Go package boundaries, or scripts automatically.

### GitHub organization

Use a common approved owner and a Project named **Kognition**. Add fields for component, migration phase, dependencies, compatibility impact, and verification status. Issues live in the repository that owns the change; cross-repository prerequisites must link explicitly.

Keep the authoritative roadmap and architecture in core. Each integration owns its operational documentation and release checklist. A coordinated workspace with sibling clones can simplify development; CI must build each repository without sibling checkouts or unpublished local replacements.

No repository creation, visibility change, history rewrite, push, or cloud provisioning is included in this planning task. New repositories inherit the existing ownership, MIT first-party licensing, CLA, notices, and publication review requirements.

## Source-To-Target Mapping

Paths below describe responsibility; preserve source history and adjust imports deliberately during extraction.

| Current surface | Target | Migration rule |
| --- | --- | --- |
| [canonical API](../../../api/), [schemas](../../../api/schemas/), [protocols](../../../proto/), [public SDK](../../../pkg/pluginsdk/) | Core | Expose only types/helpers that an external adapter needs; generated bindings remain versioned and checked |
| [core services](../../../internal/), [commands](../../../cmd/) excluding provider implementations | Core | Preserve control-plane policy, reconciliation, storage, routing, runtime approvals, identity, quota, and audit authority |
| [Go runtime](../../../internal/runtime/), [Python agent](../../../runtime/agent/), optional AutoGen profile | Core | Preserve supported profiles and transports; provider-specific direct-execution code must leave the base image |
| [Ollama](../../../internal/cpi/drivers/ollama/), generic [OpenAI-compatible transport](../../../internal/cpi/drivers/openai/) | Core | A protocol-compatible local client is acceptable; explicit local configuration must not default to a public cloud endpoint |
| [pgvector provider](../../../providers/pgvector/), [pgvector command](../../../cmd/kognition-knowledge-pgvector/), [memory](../../../internal/memory/), [store](../../../internal/store/) | Core | Pgvector/PostgreSQL are local software prerequisites; keep real durable functionality, not only the in-memory implementation |
| [Mem0 adapter](../../../internal/memory/mem0/) and Mem0 selection in [memory composition](../../../internal/memory/serviceapp/app.go) | Optional Mem0 integration | Expose the necessary memory contract through CPI/SDK; preserve hosted and self-hosted compatibility, credentials, provenance and cleanup |
| [Foundry driver](../../../internal/cpi/drivers/foundry/), [Azure Search provider](../../../providers/azuresearch/), [Azure Search command](../../../cmd/kognition-knowledge-azuresearch/) | Azure | Remove vendor imports from core consumers after external contracts pass; retain provider identifiers and supported manifest behavior |
| [Bedrock driver](../../../internal/cpi/drivers/bedrock/), S3 fetching in [ingester](../../../runtime/ingester/ingest.py) | AWS | Preserve SigV4, existing aliases, source behavior, typed errors, and credential isolation |
| [Anthropic driver](../../../internal/cpi/drivers/anthropic/), direct Python Anthropic integration | Anthropic integration | Extract with its tests or retain temporarily behind an explicitly transitional distribution; never silently drop it |
| Azure credentials and Blob fetching in [Python runtime](../../../runtime/agent/azure_creds.py) and [ingester](../../../runtime/ingester/ingest.py) | Azure | Agent inference uses core gateways; cloud source adapters receive only approved namespace-local credentials |
| Remaining [ingester](../../../runtime/ingester/), [indexer command](../../../cmd/kognition-knowledge-indexer/) | Core | Keep bounded local/inline sources and provider-neutral chunking/embedding orchestration; do not require a cloud SDK |
| [AKS assets](../../../deploy/aks/), [Azure sample setup](../../../deploy/azure-samples/), [Bicep](../../../infra/) | Azure | Integration installation and billable infrastructure provisioning remain separate operations |
| [core chart](../../../deploy/helm/kognition/), [pgvector chart](../../../deploy/helm/kognition-knowledge-pgvector/), [Compose](../../../docker-compose.yml), [observability](../../../deploy/observability/) | Core | Local defaults, local identity/certificates, correct migrations, restricted networking, and opt-in export |
| [Azure Search chart](../../../deploy/helm/kognition-knowledge-azuresearch/) | Azure | Separate immutable chart/image release; install provider CRDs/RBAC without taking ownership of core resources |
| [samples](../../../samples/), [tutorial](../../../TUTORIAL.md), recording/tutorial assets in [tools](../../../tools/recording/) | Samples, except operational fixtures and embedded Studio tutorial | Move narrative demos and their tests; preserve core's minimal quickstart and any fixtures/assets required by shipped Studio |
| [embedded gallery](../../../cmd/kognitionctl/cmd/gallery_assets/), [Studio assets](../../../cmd/kognitionctl/cmd/studio_assets/), [practice router](../../../internal/studiotutorial/) | Core with reviewed local examples; optional sample content from pinned releases | Preserve asset embedding and local availability; practice remains simulated and cannot execute real models or provider installation |
| [.env template](../../../.env.example), [.dockerignore](../../../.dockerignore), [HTTP Compose overlay](../../../docker-compose.http-runtime.yml), [development settings](../../../.vscode/) | Core or owning integration | Split defaults/templates and build contexts; preserve explicit project/volume identity and do not commit machine-specific credentials |
| [build wrappers](../../../Makefile), [PowerShell wrapper](../../../make.ps1), [service scripts](../../../services.ps1), [CI](../../../.github/workflows/ci.yml), [release](../../../.github/workflows/release.yml) | Their owning repositories | Core build/test/install cannot source sample or Azure helpers; each repository gets independent reproducible checks |
| [operational docs](../../), [project specification](../../../PROJECT.md), [backlog](../../../BACKLOG.md), legal/community files | Core or owning integration | Keep core operational authority; migrate provider runbooks and cross-links without discarding historical evidence |
| [Go tests](../../../internal/), [deployment tests](../../../deploy/tests/), [E2E](../../../e2e/), [integration fixtures](../../../tests/) | Contract tests in core; provider tests with providers; tutorial tests with samples | Preserve test intent and ownership, including namespace isolation, real database behavior, and deployed acceptance |

### Core distribution contract

1. No Azure/AWS/Anthropic SDK or provider implementation is required by the core source build, test build, release image, or default installation. Check transitive dependencies and image contents, not just direct imports.
2. No default cloud endpoint, implicit credential discovery, provider resource creation, or external telemetry export occurs in the local profile.
3. All supported core functions remain available without installing a cloud integration. Provider-specific features are advertised separately.
4. PostgreSQL/pgvector, a qualified local inference server, model assets, certificates, and keys are documented prerequisites. "Self-contained" does not mean "dependency-free."
5. Core keeps a small smoke example and deterministic contract fixtures. Removing every example would make independent core validation harder.
6. Installing an integration does not rebuild core binaries/images or require cloning its source. Local and multiple cloud integrations can coexist without changing unrelated workloads.

## Integration Architecture

### Reuse CPI And The Public SDK

The existing [provider-boundary ADR](../../adr/0015-provider-integration-boundaries.md) supports typed, separately installed, out-of-process adapters. The [Model Gateway ADR](../../adr/0008-model-gateway.md) and [invocation contract ADR](../../adr/0025-model-invocation-contracts.md) retain core authorization and accounting.

Recommended boundary:

- Core owns logical resources, current authorization, signatures/bindings, admission, quotas, trusted operation identities, finalizers, and audit.
- Integrations own provider configuration validation, remote API translation, provider identity/credentials, backend execution, and cleanup.
- Agents communicate with governed core gateways. An installed integration must not cause raw backend credentials or database access to reach Agent workloads.
- Add authenticated model execution transport and typed capability metadata to the existing CPI family. Reuse the secure knowledge transport pattern where applicable; local socket discovery is not remote execution authorization.
- Preserve complete tool definitions/calls/results, finish reasons, usage presence and counters, deadlines, cancellation, profile/version binding, limits, and sanitized typed failures across transport.
- A provider advertises only implemented capabilities. Unsupported operations fail before execution; an unavailable Azure backend never becomes pgvector or Ollama automatically.
- Provider validation supplies declared metadata through a reviewed typed contract, allowing core to enforce common constraints without importing vendor-specific configuration decoders.
- Both standalone and Kubernetes need administrator-controlled registration and configuration resolution. Kubernetes CRDs alone cannot complete standalone installation.
- Registration has an explicit owner, provider identity/version, configuration kinds, endpoint/trust binding, namespace policy and generation. Watch changes in every consumer; reject duplicate/spoofed registrations and invalidate stale snapshots without silently redirecting active sessions.
- Keep validation/capability discovery separate from effect-bearing calls. Bind canonical request/configuration digests, request/operation IDs and provider incarnation into execution and retained outcomes. CountInputTokens and invocation-contract discovery need equivalent public support for candidate execution.
- Persist ownership and quota reservations before dispatch. After timeout, cancellation, stream truncation or response loss, a missing terminal outcome remains uncertain; neither a transport reconnect nor an idempotency key alone authorizes another model/tool effect.
- Typed errors distinguish validation, authorization, quota, transient pre-dispatch failure, dispatched-uncertain outcome, and confirmed terminal outcome. Retry only the classes explicitly allowed by the existing execution contract, under bounded deadlines.
- Test concurrent owners, provider restarts/replacements, cancellation acknowledgement, backpressure, malformed terminal metadata, duplicate delivery and credential rotation. Preserve exact output/usage facts and current revocation checks; never turn absent usage into zero.

Go `internal` packages cannot be imported by unrelated external modules. Extract the minimum public CPI/model-binding contract into the existing public API/SDK surface, or make adapters consume generated protocol types. Do not export the entire controller implementation or copy its authorization code into providers.

### Decision And Staging Constraints

- RP02 must accept the provider registration/model/source/memory contract, the public binding-validation surface, and the versioned legacy migration path before dependent implementation.
- [ADR-0005](../../adr/0005-secretless-provider-auth.md) places Azure credential refresh in directly calling runtimes. Moving calls and credentials changes that ownership decision. Record a narrowly scoped superseding ADR for migrated profiles; do not edit the accepted historical record or silently remove supported identity modes.
- Preserve [ADR-0016](../../adr/0016-installation-trust-boundary.md) activation fencing and [ADR-0017](../../adr/0017-artifact-signature-trust.md) operator-pinned signing. Preserve [ADR-0018](../../adr/0018-workload-identity-issuer.md) SPIRE identity and [ADR-0022](../../adr/0022-python-workload-transport.md) Go loopback transport/key isolation; do not introduce an ad-hoc workload CA or incompatible Python SPIFFE dependencies.
- Preserve [ADR-0019](../../adr/0019-runtime-adapter-transport.md) runtime HTTP lifecycle, [ADR-0024](../../adr/0024-runtime-checkpoint-recovery.md) encrypted checkpoint/replay ownership, and [ADR-0025](../../adr/0025-model-invocation-contracts.md) omission and binding semantics. Model CPI extension work is not authorization to replace these runtime transports or add unsupported streaming/reasoning protocols.
- Stage independently buildable provider/source/memory packages and test adapters inside the current repository before changing execution consumers. Use explicit module boundaries and public imports; CI must enumerate every module because root `go test ./...` excludes nested modules.
- Maintain a test ownership manifest mapping each baseline test/fixture/assertion to its destination, runner, supported mode/profile and candidate evidence. No vanished test, new skip or coverage exclusion is accepted merely because its code moved.
- Implement an in-repository candidate-build and compatibility harness before extraction. Reuse it for independent repositories; final publication workflows come later. Component-stage checks use the candidate core and staged packages, not an unavailable future public release.

### Installation And Lifecycle

Each integration release must contain:

- Immutable adapter images, typed configuration schemas/CRDs, a chart, standalone configuration/service packaging, and least-privilege permissions.
- Compatibility metadata for core/protocol/schema versions and its tested platforms.
- PowerShell and shell entrypoints or equivalent packaged commands for preflight, plan, install, verify, upgrade, and uninstall.
- Namespace-local credential/identity setup with separate explicit approval for cloud provisioning and cost-incurring inference.
- A concrete quickstart, troubleshooting, recovery, and data-retention instructions.

Installation follows the existing [installation trust boundary](../../adr/0016-installation-trust-boundary.md) and [recovery contract](../../features/knowledge-provider-execution/installation-recovery-contract.md). Preflight verifies target identity, versions, digests, credentials, certificates, ownership, and networking before mutation. Kubernetes installation remains inactive until its separately approved acceptance and activation steps complete.

Adding another integration must not replace a singleton route globally. Registration must bind each Model/KnowledgeBase to the intended provider identity and namespace-local configuration, including coexistence, revocation, and removal behavior.

Uninstall blocks active bindings by default. Forced removal fails closed and preserves recoverable data/finalizers. Neither Helm cleanup nor a convenience script may cascade-delete core CRDs, shared Secrets, or backend data.

Maintain separate identities/keys for provider mTLS, workload identity, capability HMAC, runtime authentication, artifact signing, approval binding, and per-workspace checkpoint encryption. Inventory mounted key IDs, rotation overlap, expiry and retained read keys without printing material. Preserve existing keys and trust domains across repository moves; installation scripts must not regenerate them on every restart.

Provider configuration/route changes can change signed specifications, configuration digests, session/candidate bindings and installation fingerprints. Obtain new signatures/enrollment/acceptance where required. A repository rename or unchanged model weights does not make old approval hashes reusable.

## Local Capability Qualification

Use Ollama as the first candidate, while retaining the generic local OpenAI-compatible option. Select exact chat and embedding models only after their licenses, hashes, tool support, usage reporting, and supported hardware pass qualification.

The current driver uses `/api/embeddings`; Ollama's current [embedding API](https://docs.ollama.com/api/embed) documents `/api/embed` with batch input and `prompt_eval_count`. Evaluate a versioned contract rather than replacing endpoints without compatibility tests. Preserve request/vector ordering, dimensions, context limits, and explicit truncation behavior.

| Capability family | Required local acceptance |
| --- | --- |
| Agent, AgentInstance, AgentJob, runtime profiles | Real inference through approved bindings; health, scheduling, retries, cancel, checkpoint/resume, restart, and supported native/AutoGen behavior |
| Tools and workflows | Real structured tool call and result, sandbox policy, grants, human approval, conditional/parallel/quorum workflows, retry and replay-safe side effects |
| Memory and knowledge | Durable memory, summarization, actual indexing and dense retrieval with citations, namespace isolation, matching index/query embeddings, refresh, Delete/Retain |
| Identity and governance | Local identity/certificates, RBAC, policy/admission, quota accounting, RuntimeApproval, trust decisions, revocation, redaction, and denied-action evidence |
| Models and autoscaling | Chat/embedding profiles, usage, signature/version binding, exact-accounted candidate promotion where supported, routing, and locally executable scaling scenarios |
| Services, evaluations, Studio, and observability | AgentService/IntentGateway/delegation, datasets and judge execution, visual authoring/round trips, authorized actions, local dashboards and traces |

Expand this family-level checklist into an operation-level matrix derived from the registered resources and supported interfaces. Mark each operation **verified local**, **provider-specific**, or **existing implementation gap** with evidence. Every operation advertised as core functionality must pass locally before claiming complete core functionality.

Record exact deployment mode, runtime, identity profile and backend combinations; resource registration alone does not imply the full Cartesian product is supported. Document intentionally unsupported combinations separately from implementation gaps. Generic core functionality cannot be reclassified as provider-specific merely to pass the matrix.

Keep the embedded Studio practice tutorial isolated and simulated under [ADR-0026](../../adr/0026-studio-tutorial-execution.md). Prove real local functionality with a separate explicit live quickstart/acceptance workflow. Requiring real inference inside practice would violate its accepted no-live-effects contract.

Local model promotion must meet the existing exact-accounting and lifecycle deadline, including warmed and cold/unavailable-model cases. Do not substitute legacy metadata-only promotion, weaken token limits, or extend deadlines without an approved contract change. Qualify local zero-priced accounting explicitly while preserving token/tier quotas and reported usage. Runtime tokenizer/model assets must be present after setup; the optional air-gapped installer is a separate concern.

Existing aspirations and undecided behavior, including Model Gate 3 and autonomy cost-threshold semantics recorded in the backlog, are not automatically implemented by this refactor. Preserve their open status. Exact Ollama tokenization and local scaling/promotion qualification are blockers to any stronger local completeness claim.

## Migration Tasks And Dependencies

These `RP` and `RV` identifiers are local plan identifiers, not allocated backlog task/test IDs. Before implementation, allocate or update stories/tasks/tests using [project traceability conventions](../../../PROJECT.md#05-traceability--identifier-schema). Each implementation task needs one parent story and FR/NFR/test references. Existing related stories below provide anchors; do not reopen completed implementations merely to rename folders.

All tasks are pending. Owners are roles to assign, not assumed staffing. IDs remain stable; table order follows prerequisites rather than numeric order. An early task proves its named component-stage checks; full release-set checks run after packaging and cannot be claimed early.

| ID | Phase / owner | Work and affected surfaces | Prerequisites | Completion evidence |
| --- | --- | --- | --- | --- |
| RP01 | Baseline / maintainer | Capture reviewed source/artifact identities, dirty-work disposition, operation/mode/profile matrix, complete test ownership inventory, current failures/skips, and known gaps from [backlog](../../../BACKLOG.md) and [testing](../../testing.md) | None | Reproducible baseline report; all failures triaged; source/docs drift identified; no borrowed historical green status |
| RP02 | Decisions / maintainer | Approve owner/names, complete integration inventory including Mem0, exact protocol/public-helper/identity decisions, local qualification tier, supported tuples, legacy compatibility, thresholds and superseding ADRs; allocate traceable work items | RP01 | Accepted decisions, assigned owners and test mappings; blocking open decisions resolved before dependent code |
| RP03 | Hygiene / developer tooling | Audit tracked versus ignored outputs; route future artifacts to ignored output directories; decouple [make.ps1](../../../make.ps1), [Makefile](../../../Makefile), and [services.ps1](../../../services.ps1) from sample/Azure helpers | RP01, RP02 | Build helpers run without sample sources; cleanup preserves credentials, reviewed evidence, generated bindings, user work and Compose project/volume names |
| RP04 | Contracts and staging / core | Public CPI/SDK and provider-side binding validation; model/source/memory parity; authenticated registration and namespace-local resolution; staged independent modules/test adapters and module-enumerating candidate CI; related US-0065 | RP02 | Component RV03/RV04/RV21/RV23 pass against candidate core; no external internal imports; every staged module/test is enumerated |
| RP05 | Composition / core | Replace every compiled external driver/decoder dependency in API server, controllers, model/memory/knowledge gateways and ingestion using the staged packages; related US-0065 and US-0087 | RP04 | Go component RV01 plus deterministic RV04/RV07/RV08/RV23 pass; cloud/Python/image-wide independence remains a later gate |
| RP06 | Local models / runtime | Qualify Ollama or another local server; fix usage, tool/profile handling, embeddings and exact candidate counting across [CPI](../../../internal/cpi/), [model execution](../../../internal/modelgateway/) and [runtime factory](../../../runtime/agent/providers.py); related US-0021 | RP04, RP05 | RV05/RV07 and real runtime component RV06 pass; cold/warm canary and zero-priced accounting qualified; remaining installed-mode cases tracked |
| RP07 | Local persistence / memory and knowledge | Wire local ingestion/query/memory through approved gateways, credentials, TLS, migrations and index bindings; related US-0013 and US-0014 | RP05, RP06 | Real-database component RV08/RV11 pass; known-corpus citations and data restart preserved |
| RP18 | Standalone parity / core and runtime | Make [Knowledge Gateway](../../../cmd/kognition-knowledge-gateway/main.go) binding/configuration validation mode-aware; add standalone knowledge issuer/routes, provider registration and thin runtime access in [dispatcher assembly](../../../cmd/kognition-controller-manager/runtime_dispatcher.go) | RP04, RP05, RP06, RP07 | RV22 passes using PostgreSQL/local providers with no Kubernetes API dependency; unsupported combinations fail explicitly without exposing backend credentials |
| RP08 | Runtime packaging / runtime | Split [Python requirements](../../../runtime/agent/requirements.txt), [ingester dependencies](../../../runtime/ingester/requirements.txt), direct providers, fetchers, tests and image profiles; retain supported native/AutoGen behavior | RP05, RP06 | Python/image component RV01/RV06/RV12/RV21 pass from fresh images; full source independence is not yet claimed |
| RP09 | Local installation / deployment | Complete Compose and core/pgvector chart profiles, credential/key/certificate preservation, migrations, HTTPS/loopback runtime wiring, networking and live local smoke fixture; related US-0073 and US-0099 | RP03, RP06, RP07, RP18, RP08 | RV02/RV09/RV10/RV11/RV22/RV24 pass on clean isolated installations; documented HTTP endpoint behavior matches actual source |
| RP10 | Azure module / Azure maintainer | Move staged driver, Search, Blob, identities, charts, [AKS](../../../deploy/aks/), [Bicep](../../../infra/), scripts and owning tests into a nested Azure module with its own `go.mod`, chart and runner; retain a qualified legacy route until replacement passes | RP04, RP05, RP08, RP09 | Azure module builds and passes RV13 and its scoped RV15/RV18/RV24 with core built from the same checkout; full cross-provider combinations remain RP13 |
| RP11 | AWS and other modules / integration maintainers | Move staged Bedrock/S3, direct Anthropic and external Mem0 into owned nested modules, preserving identifiers and supported behavior | RP04, RP05, RP08, RP09 | Owning modules pass RV14/RV25 and scoped RV15/RV18/RV24; no provider disappears without an approved deprecation |
| RP12 | Samples module / sample maintainer | Move examples/tutorials and narrative recording assets into a samples module; rewrite [relative fixtures](../../../internal/traceability/sample_hardening_test.go), cloud defaults, archive handling and CLI embedded gallery; keep isolated practice and minimal core fixtures | RP09, RP10, RP11 | RV16/RV17/RV25 pass using only the declared module interfaces, with core passing when the samples directory is absent; repeat under RP13 |
| RP17 | Core independence / core and release engineering | Remove remaining transitional source/dependency/test/image leakage after all adapters and sample consumers have moved; reconcile every baseline test against its owner and restore independent build entrypoints | RP04, RP05, RP08, RP09, RP10, RP11, RP12 | Full RV01/RV02/RV21/RV25 pass; zero missing baseline test mappings; core builds and tests with provider and sample directories removed and the dependency-boundary check green |
| RP13 | Release coordination / release engineering | Finalize CI that enumerates every module, immutable charts/images, version/alias policy, public generation/compatibility, the single release-set inventory, platform matrix and per-distribution notices | RP17 | Full RV01/RV03/RV12/RV15/RV16/RV18/RV21/RV24/RV26 pass across the exact release set; partial publication never advances the approved manifest |
| RP14 | Optional offline bundle / release engineering | Package verified images/charts, model assets or approved import procedure, tokenizer data, dependency caches, license notices, and checksums | RP06, RP09, RP13 | RV19 passes without network installation/runtime fetches; failure does not falsely certify offline support |
| RP15 | Cutover rehearsal / operations | Inventory actual state, run shadow comparisons, rehearse provider swap, failure recovery and freshly approved return to prior compatible routing; preserve pending approvals and unknown-effect receipts | RP13 | RV11/RV18/RV20/RV24 pass on the reviewed rehearsal target with measured approval-gated recovery time and zero data loss |
| RP16 | Release and migration / maintainer and operations | Execute ordered cutover below, update core/provider quickstarts and migration guide, monitor, then retire transitional paths | RP15 | All mandatory release gates pass; deployment-specific acceptance/approval remains separate from source release |

Keep RP10 and RP11 independent once their prerequisites pass. RP14 is optional and is not a dependency of ordinary cloud-free release.

Component-stage and full release-set evidence must be labeled separately. RP04 creates the harness; RP10/RP11 validate their own candidate packages; RP12 consumes those immutable candidates; RP17 proves full core independence; RP13 reruns supported combined/published combinations. This ordering avoids requiring a future artifact or final dependency graph to complete an earlier task.

## Verification Matrix

Every result identifies source SHA, image/chart/model digests, configuration, test command, toolchain, hardware, duration, skips, and report hash. Record results per mode and runtime profile. A required environmental test skipped for missing infrastructure is incomplete evidence.

RP01 supplies the baseline test/operation inventory. Each applicable row below needs a reproducing test selector, expected output, owner, exact supported tuple and result location before its implementation starts. For tests spanning repositories, the caller-owned compatibility job references immutable provider/sample artifacts and cannot silently disappear when paths move.

Differential tests may normalize documented per-request generated IDs/timestamps only when their relationships remain intact. Namespace/resource UIDs, provider identity, configuration/profile digests, committed generations, durable receipt identities and authorization bindings must match exactly. Real-model quality tests use predeclared task/corpus criteria rather than byte-identical generated prose.

| ID | Scenario / input | Required result and verification |
| --- | --- | --- |
| RV01 | Fresh independent core checkout and release images | Build and test dependency closures contain no cloud integrations/SDKs; inspect Go dependencies, Python installed packages, Docker contexts/layers, image inventory, and default manifests |
| RV02 | No cloud credentials/CLIs; runtime external egress denied, local services allowed | Install/run the local profile; real inference, tools, memory, retrieval, workflows, identity, and Studio pass; zero public/cloud connection attempts after preparation |
| RV03 | Independently compiled adapter, current and previous supported core release, unknown protocol/schema | Declared supported combinations pass; incompatible combinations fail before deployment mutation or backend execution; Buf generation/breaking checks and public SDK consumer build pass |
| RV04 | In-process baseline versus external adapter on identical scripted requests | Equal stable payloads/results, finish reasons, usage presence/counters, profile bindings, embeddings/order, errors and cancellation; provider-supplied tool IDs are preserved and synthesized IDs retain uniqueness and exact call/result linkage; oversized/malformed responses and restart are bounded and explicit |
| RV05 | Real pinned local chat and embedding models | Actual tool-call round trip, generation limits, reported usage, finite vectors of exact configured dimension, ordered batch results, and correct oversized-input behavior |
| RV06 | Supported Go/native Python/AutoGen transports with gateway-only Agent networking | Real chat/tool/memory/knowledge flow passes; no direct backend credentials or bypass; checkpoint/resume preserves conversation/tool bindings and never repeats completed effects |
| RV07 | Revoked/stale ModelGrant, changed profile/version/UID/session, missing usage/audit, quota exhaustion | Deny before dispatch when authorization/accounting is missing; persist explicit failures otherwise; exact-accounted local candidate promotion and rollback obey current bindings |
| RV08 | Fixed corpus with expected document/chunk counts and citation identities | Real index/query embedding contract matches; expected document appears in top five for each predeclared query; cross-namespace retrieval returns no foreign rows; refresh failure retains valid committed data |
| RV09 | Installation preflight fails; installer interrupted at each mutation boundary | No activation, unrelated mutation, credential leak, or false success; journal-driven compensation/retry recovers; both shell entrypoints honor exact target and ownership |
| RV10 | Core/provider chart, schema conversion, admission, RBAC, and enforcing-CNI deployment | Supported resource schemas/status survive round trips; direct backend access and unauthorized writes fail; local identity and namespace boundaries remain enforced |
| RV11 | Populated prior-version databases, pending work/approvals, checkpoints, receipts, indexes | Upgrade and process restart preserve counts/checksums, resource UIDs, committed generations, and no-replay guarantees; Delete/Retain/finalizers remain correct |
| RV12 | Fresh supported release images/platforms and runtime dependency profiles | Independent build plus actual process execution for Linux AMD64/ARM64; Windows CLI/shell and supported local orchestration checks pass; mounted TLS, read-only images, migrations and image contents match release metadata |
| RV13 | Existing Azure configuration and independently installed Azure package | Foundry chat/tool/embedding, Azure identity modes, Blob read/ingest/citations, Search lifecycle/query and AKS packaging pass their declared supported scope; live gates need an approved disposable target and budget |
| RV14 | Existing Bedrock/S3/Anthropic configuration and their packages | Preserve supported auth, chat/tools, source fetching, timeouts, usage, errors and cleanup; never advertise an unsupported embedding or lifecycle capability; qualify live providers separately |
| RV15 | Local, Azure, and AWS registrations together; duplicate/spoofed/removing provider | Correct per-resource routing, isolated credentials, immediate fail-closed revocation, blocked uninstall with active bindings, recovery on reinstall; unrelated providers stay healthy |
| RV16 | Each sample from its own checkout with published core/integration releases | Documented commands complete and produce required outputs; local category needs no cloud; cloud category declares prerequisites/costs; validate upload/count/readiness, not exit zero alone |
| RV17 | Studio desktop/mobile, dashboard, embedded tutorial and sample documentation | Existing authoring/catalog/round-trip/authorization browser tests pass; resource field coverage survives; all moved links/assets/paths resolve |
| RV18 | Core-only upgrade, provider-only upgrade, failed upgrade, previous supported release set | Compatible independent upgrades pass; incompatible ones stop before mutation; supported rollback preserves bindings/data; release workflows cannot publish mixed or missing artifacts as a complete release set |
| RV19 | Optional bundle imported on a clean network-isolated machine | Install and all advertised local operations complete without fetching images, wheels, modules, tokenizer data, or models; verify model licensing/import requirements and corruption checks |
| RV20 | Same hardware, workload, backend/model, concurrency and corpus before/after | Meets the quantified regression budgets below; rehearse abrupt adapter loss and cutover recovery while preserving data and pending effects |
| RV21 | Nested modules, extracted tests and shared fixtures | CI enumerates every module/profile; each baseline test/assertion has a destination and result; generation/public consumer builds and coverage gates fail for missing inputs rather than silently excluding them |
| RV22 | Standalone governed knowledge, provider registration and actual runtime entrypoint | No Kubernetes reader/kubeconfig required; real local index/query/citations and namespace revocation pass; HTTP overlay endpoints obey TLS/literal-loopback rules; supported native/AutoGen combinations run and declared exclusions remain explicit |
| RV23 | Concurrent owners, duplicate delivery, credential/provider replacement, response loss after effect, uncertain cancellation | Exactly one effect-bearing dispatch; immutable bound terminal facts; no guessed refund/zero usage or retry after unknown outcome; stale registration/session fails closed and documented recovery works |
| RV24 | Provider/workload identity rotation, checkpoint read keys, changed signatures/digests and failed activation | Preserve distinct key purposes and retained ciphertext recovery; invalid/stale hashes, trust roots or bindings fail; no reuse of consumed approvals or restoration of old grants; recovery requires fresh exact-target authorization |
| RV25 | Mem0 hosted/self-hosted configuration, embedded gallery imports and isolated Studio practice | Owning integration preserves memory CRUD/search/provenance/cleanup; baseline core has no hosted Mem0 dependency/default; local gallery works without cloud credentials; practice remains simulated with zero live backend/model calls |
| RV26 | Manifest/API round trips, legacy CLI/env/URL/image/chart aliases and interrupted multi-repository publication | Explicit omissions, zero/false values, defaults, scope, signatures and bindings preserve their contracts; aliases follow the declared migration policy; immutable complete inventory is approved only after every required artifact/check succeeds |

### Existing runners to preserve

Use [the testing guide](../../testing.md) and [change-based gates](../../development-gates.md) for setup and selection. The current CI includes an authored Go coverage floor of 80%, race tests, tagged integration/envtest, Python/native/AutoGen tests, dependency/configuration scans, traceability, copyright, and generated-protobuf checks.

Examples for later implementation validation, run from the owning checkout with its documented database/envtest prerequisites:

```powershell
# Small affected slices first; add the extracted provider's owning suite separately.
go test -count=1 -timeout=3m .\internal\cpi\... .\internal\modelgateway\... .\internal\memory\embed\... .\pkg\pluginsdk\...
go test -count=1 -timeout=3m .\internal\controller\... .\internal\k8s\dispatcher\... .\internal\traceability\...

# Full release baseline in the prepared CI-equivalent environment.
go test -p=1 -count=1 -timeout=10m -race "-coverpkg=github.com/kognition-ai/kognition/..." "-coverprofile=coverage.out" ./...
go test -tags integration -count=1 -timeout=15m ./...
go build ./cmd/...
buf lint

.\.venv\Scripts\python.exe -m pytest -q runtime\agent runtime\ingester
helm lint deploy\helm\kognition
& .\deploy\helm\kognition\tests\helm-regression-tests.ps1
npm run test:dashboard:browser
npm run test:studio:browser
npm run test:studio:tutorial
```

The full module commands retain Go's package-pattern syntax. Install test dependencies only in the documented preparation phase; run actual AutoGen dependencies and enforce zero unexpected skips. Preserve the coverage checker and generated-binding consistency check from CI.

Cross-repository moves also invalidate repository-path expectations in traceability, security/change classifiers, legal inventories, Docker build contexts, umbrella archives and tutorial recording tests. Replace those assumptions with owner-local fixtures or digest-pinned release inputs without weakening the tested contract. Reclassify moved inputs in [the change planner](../../../tools/securitygate/impact.go); fingerprints and scanner/activation receipts cannot be carried forward by renaming their paths.

The commands above describe the original root module only. After RP04, use the module-enumerating harness for all staged/extracted modules. Run the ordinary and real-SDK Python profiles in separately prepared environments to prevent one optional dependency set from masking another. Run each Helm provider/sample suite with its owner, and refresh only the intended chart archive while preserving separately owned archive hashes.

### Proposed measurable release budgets

These budgets require approval under RP02 and measurement under RP01/RV20; they are not existing guarantees.

- Preserve the 80% authored Go coverage floor in core and require explicit coverage gates for extracted authored code. Also require no regression in the migrated contract assertions; a changed denominator cannot conceal lost tests.
- Run performance comparisons three times on identical hardware and configuration with 100 warm-up requests and 1,000 measured requests at a fixed concurrency. Record inference and platform overhead separately.
- Allow at most 10% growth in platform p95 overhead, at most 5% throughput loss, and at most 10% growth in steady-state core RSS relative to the same-backend baseline. Record absolute values; CPU-local versus cloud inference is a separate quality/performance comparison.
- Require zero unexpected application errors in the deterministic 1,000-request comparison and a 60-minute mixed-operation soak. Intended denial/fault cases must produce their specified errors.
- Source extraction target: zero service downtime. Deployed routing cutover target: at most five minutes of paused new work in the qualified reference environment.
- Rollback target: restore prior supported routing/service health within ten minutes in the qualified environment. Failure of the rehearsal blocks rollout; larger installations require their own approved measured budget.
- Data loss and duplicate irreversible effects: zero. Required corpus/count/checksum/binding comparisons must all match, apart from explicitly expected new audit/migration records.
- Local-model hardware minima, cold-start bounds, and acceptable model-task scores must be measured during RP06 before the local profile is advertised.
- RP02 must pin the tested Go/Python/Kubernetes/Helm/CNI/PostgreSQL/pgvector/runtime/model combinations from baseline evidence. Existing documentation minimum versions and `latest` tags do not define an accepted environment.
- Measure performance with the same telemetry, authorization, TLS, accounting and storage settings. A faster run obtained by disabling gates is not comparable. Fault/denial tests remain separate from the zero-unexpected-error workload.

## State Preservation, Cutover, And Rollback

### Data policy

Repository extraction itself requires no data migration. Do not change database schema, resource names/UIDs, API groups, vector dimensions, or embeddings merely to reorganize source.

Before any deployed routing or schema change, inventory actual database size, rows per table, resources by kind/namespace, index documents/chunks, model/profile/configuration digests, active bindings, credentials, pending approvals/work, and retained dispatch receipts. Volumes are deployment-specific and unmeasured in this plan.

Preserve Compose project/volume names, database roles/schemas and handles, Kubernetes release/namespace/resource ownership, capability key IDs, workload trust domains, checkpoint key rings, and installation-authority records. Do not recreate named resources to make repository names line up; UID replacement invalidates existing bindings. Restoration must include the protected encryption keys and authority-state continuity, not only database rows.

Take an access-controlled backup and verify restore on an isolated target. Under a writer freeze, compare row/resource counts and canonical content checksums of protected state, excluding documented volatile telemetry. Compare index counts, citation/source digests, embedding binding and committed generation separately.

Use additive schema/configuration changes and keep prior readers compatible throughout the rollback window. Preserve migration bytes/hashes and approval evidence. Do not regenerate executed SQL to move files.

If a local embedding model replaces a cloud model, create a separate reviewed shadow index, reindex with a known budget, validate corpus retrieval, and then switch claims. Equal dimensions do not make embedding spaces compatible. Keep the previous index and bindings until rollback qualification and retention policy allow retirement.

### Ordered cutover

1. **Freeze the candidate:** Record reviewed commits, exact artifacts, compatibility set, and baseline failures. Confirm existing uncommitted work has an owner-approved disposition.
2. **Qualify independently:** Run all mandatory RV gates in isolated environments, including real local inference and applicable approved cloud conformance. Complete data-restore and rollback rehearsals.
3. **Prepare the target:** Inventory/backup, pin old and new artifacts, verify certificates/credentials/registrations, and stop new work through the supported maintenance workflow. Drain work where safe; preserve pending approvals and unknown-effect claims.
4. **Install inactive:** Apply the reviewed additive provider/configuration transaction under the installation contract. Verify ownership and journal checkpoints; do not activate on installation success alone.
5. **Compare without effects:** Validate representative resources, known-corpus retrieval, bindings, schema/count/checksum equality, and read-only shadow routing. Never dual-dispatch a side-effecting Tool or rerun an uncertain inference.
6. **Activate deliberately:** Complete fresh target-specific acceptance, required signatures/bindings and a new approval transaction, switch the reviewed routes/claims, resume new work, and verify real gateway-mediated operations. Old consumed or expired approvals remain invalid.
7. **Monitor and retire:** Run the 60-minute soak and scheduled/restart scenarios. Keep the previous release set, data, and configs through a proposed minimum seven-day rollback window; retire transitional code/routes only after sign-off. Active sessions, retained checkpoint ciphertext and unresolved effects can require longer retention; the window never authorizes deleting their keys, receipts or recovery artifacts.

### Rollback

Trigger rollback for an authorization/isolation regression, incorrect routing, data/checksum mismatch, duplicate effect, unsupported manifest/schema change, breached cutover budget, or failed operational health checks.

1. Pause new admissions/dispatch and preserve the installation journal, effect receipts, current state, and failure evidence.
2. Recover/compensate the failed transaction using its retained journal and exact trusted executable. Select the pinned previous compatible release set as a new reviewed recovery target; do not downgrade binaries against an incompatible schema or restore previous grants/activation automatically.
3. Restore previous valid model/index bindings through supported controllers/configuration. Do not replay pending Tool effects, unknown model outcomes, or stale RuntimeApprovals.
4. Verify row/resource/index counts, protected-state checksums and inactive identities/bindings. Complete fresh exact-target acceptance and a new authorized activation transaction before restoring dispatch privileges or resuming; then verify readiness and a real gateway request.
5. Use the rehearsed backup restore only if necessary, with an explicit writer freeze and reconciliation of any accepted writes. Snapshot-only restoration after new writes would lose data and is not an automatic rollback.

The ten-minute reference recovery target is an approval-gated rehearsal goal. It never overrides writer fencing, acceptance freshness or approval lifetime under [the activation contract](../../features/knowledge-provider-execution/activation-transaction-contract.md). Include those steps in measured recovery time or revise the proposed budget under RP02. Escalate while dispatch remains safely paused if evidence cannot be obtained. Repository-history operations are not runtime rollback. Any irreversible schema/data transformation requires a separate plan and cannot enter this rollout implicitly.

## Risks And Controls

| Risk | Control / stop condition |
| --- | --- |
| Many repositories increase release, governance, and contributor overhead | Start with the smallest useful split; public contracts and release-set CI precede extraction |
| Extraction leaves hidden vendor coupling or requires core recompilation | Independent SDK consumer builds, complete dependency/image inventory, and install-from-release tests |
| Local readiness succeeds while real inference/retrieval/accounting fails | Actual model/tool/embedding/corpus tests; exact usage/counting gates; no simulated success or silent fallback |
| New adapters weaken authority, namespace isolation, or egress restrictions | Keep core enforcement, authenticated execution, immutable scope/binding, and deployed enforcing-CNI tests |
| Azure/AWS installation unexpectedly provisions or bills resources | Separate provision/install/activate; explicit target/cost approval and bounded live tests |
| Provider removal or vector-model changes orphan or corrupt data | Bound uninstall, finalizers, confirmed cleanup, shadow reindex, backups and exercised rollback |
| Examples move and core tests/builds become dependent on a sibling repository | Owner-local fixtures, immutable published inputs, link/path tests, and clean standalone CI |
| Cleanup removes local credentials, accepted evidence, or generated bindings | Inventory tracked/ignored paths; clean only named disposable outputs; preserve generated protocol sources and evidence |
| Fresh repository history publishes restricted or unlicensed material | Complete applicable publication reviews, preserve first-party/third-party notices, and approve history strategy before publishing |
| Existing unrelated failures get hidden by refactor churn | Baseline each failure and owner; fix refactor-caused/tightly coupled defects; never reduce checks to manufacture green |

## Final Release Gates And Next Step

Do not claim complete local core until:

- Every advertised core operation passes the local capability matrix in both deployment modes and each supported runtime profile.
- Fresh core source, tests, images and default installation are provider-independent, with no runtime cloud connection attempts.
- Existing supported cloud configurations pass through separately installable integrations without rebuilding core.
- Independent compatible upgrades, coexistence, uninstall/reinstall, restart, backup restore, and rollback pass.
- Mandatory tests have no unexplained failures/skips; performance, integrity, isolation and publication gates meet approved thresholds.
- Quickstarts, samples, legal notices, compatibility metadata and migration instructions identify the exact tested release set.
- Every baseline test/operation has a reviewed destination and outcome; all staged/extracted modules are covered and no unsupported combination is presented as verified parity.
- Rollback preserves keys, signatures, authority journals and no-replay claims and passes a freshly authorized recovery/activation rehearsal.

An optional offline-bundle claim additionally requires RV19. Source publication, release certification, installation, and activation retain separate approval scopes. Approve an immutable release-set manifest only when the complete named artifact inventory and compatibility results exist. Mutable `latest` tags and partial image publication are not a complete release set. Tests provide bounded, reproducible evidence; no finite suite establishes a 100% absence of regressions.

**Start with RP01 and RP02, then the public contract and dependency/composition work.** Do not begin by moving directories before the module boundaries and contracts exist. Confirm the module names and paths and the local backend qualification before moving code; preserve current ongoing work and backlog priorities.

## Planning Validation And References

This task changes documentation only. Source inspection supports the findings above; runtime suites, live providers, cloud provisioning, release builds, deployed acceptance, and migration rehearsals were not run for this plan. Their results remain pending under RP/RV tasks.

Before delivery, check Markdown structure/whitespace, relative references, task/test identifier uniqueness, prerequisite order, and coverage of source ownership, data preservation, cutover, rollback, and confirmed scope.

Documentation validation on 2026-11-08 passed: 18 task definitions, 26 verification definitions, 44 dependency edges, 144 relative links and one heading anchor. Checks require complete row enumeration, valid prerequisite order/acyclicity, defined references, independent optional offline work, owner/work/evidence fields, required sections, balanced fences, ASCII and clean whitespace. Phase-exit artifact availability and the accepted ADR constraints were reviewed directly. These results do not supply any pending runtime, release or deployment evidence.

Review found that a raw multiline task-row regex ending in `$` failed to enumerate CRLF rows, so the earlier prerequisite-order assertion did not establish edge coverage. Validation now parses individual lines, requires the expected nonzero task/test counts, checks every edge/reference, and verifies acyclicity and phase-appropriate exits. This corrects documentation-validation evidence, not runtime assurance.

### Review Disposition

| Review gap | Planning correction | Remaining execution evidence |
| --- | --- | --- |
| Early tasks required later full packaging/releases | Staged modules/harness, scoped phase exits, RP17 final independence, and explicit candidate versus published inputs | RP04 through RP13 component and release gates |
| External memory, embedded gallery and default templates were insufficiently explicit | Expanded source mapping, Mem0 ownership decision and RV25 | Inventory and package/UI regressions |
| Standalone governed knowledge/runtime configuration was under-specified | Source-grounded RP18 and RV22; document/source drift is a baseline issue | Real isolated standalone retrieval and supported runtime entrypoint acceptance |
| New transport lacked explicit uncertain-effect and registration continuity checks | Contract constraints, public binding helpers and RV23 | Concurrency, lost-response, rotation and restart regressions |
| Identity/signature/key continuity and rollback could be misread as old-approval restoration | Accepted ADR constraints, preserved key/state inventory, fresh recovery/activation and RV24 | Actual approval-gated recovery rehearsal |
| Repository/module/test/release ownership could lose verification silently | Baseline ownership manifest, module enumeration, RV21/RV26 and complete immutable release-set inventory | Fresh independently reproduced suites and published-artifact verification |
| Static prerequisite check did not parse CRLF task rows | Line-based parsing with expected counts and full graph/reference checks | Documentation checker results; runtime evidence remains pending |

- [GitHub Projects](https://docs.github.com/en/issues/planning-and-tracking-with-projects/learning-about-projects/about-projects).
- [Provider integration boundaries](../../adr/0015-provider-integration-boundaries.md).
- [Current knowledge provider contract](../../knowledge-providers.md).
- [Model invocation profiles](../../model-invocation-profiles.md).
- [Testing strategy](../../testing.md) and [development gates](../../development-gates.md).
- [Current backlog](../../../BACKLOG.md).

### Evolution log

| Version | Date | Change | Trigger | Author |
| --- | --- | --- | --- | --- |
| 0.1 | 2026-11-08 | Source-grounded repository split, confirmed local scope, migration tasks, conformance matrix, cutover and rollback proposal | new work | Repository planning session |
| 0.3 | 2026-11-10 | Adopt a single-repository model: in-repository nested modules, one release set, dependency-boundary enforcement; restate RP10 to RP13 | new work | Owner decision |
| 0.2 | 2026-11-08 | Correct phase-exit ordering and add omitted backend/UI/standalone/identity/recovery boundaries, explicit readiness and stronger conformance/evidence checks | failure (spec) | Repository plan review |
| 0.2 | 2026-11-08 | Replace empty CRLF task parsing with required-count and complete dependency/reference validation | failure (validation) | Repository plan review |
