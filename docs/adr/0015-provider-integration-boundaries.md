# ADR-0015: Provider Integration Boundaries

- **Status:** Accepted
- **Date:** 2026-09-19
- **Scope:** Package boundaries and incremental migration; wire APIs remain to be implemented.
- **Related:** [ADR-0001](0001-knowledge-base-claims.md), [ADR-0005](0005-secretless-provider-auth.md), [ADR-0007](0007-agent-gateway-fabric.md)

## Context

Core KnowledgeBaseClass embeds a pgvector-specific vectorStore union. Adding
Azure AI Search there would make core validation and releases depend on another
vendor. CPI already has identity, lifecycle, health and typed model/tool/memory
extensions, but lacks a typed knowledge extension. Plugin discovery also lacks a
knowledge capability. The experimental Helm plugin host uses sidecars.

The user approved independent provider packages with typed configuration, starting
with pgvector compatibility and Azure AI Search. AKS remains a CPU-only demo with
optional Foundry inference; installing an integration must not provision billable
cloud services.

## Priorities and Requirements

1. Preserve governed, isolated knowledge access. Claims, namespace boundaries,
   revocation, quotas, policy and audit must remain enforceable regardless of vendor.
2. Decouple provider installation and releases from core. A Search integration must
   not require Foundry, and neither may add vendor unions to core CRDs.
3. Preserve working pgvector manifests. Migration must use the same backend contract
   and cannot report synthetic indexing success for a configured backend.

## Options

| Option | Priority 1 | Priority 2 | Priority 3 |
| --- | --- | --- | --- |
| Extend the core vendor union | Central validation remains straightforward | Fails: each vendor changes core schema and release | Meets: existing pgvector fields remain |
| Typed, separately installed provider packages | Meets with core authorization and authenticated adapter calls | Meets: versioned CRDs, adapters and charts ship independently | Meets with an explicit legacy pgvector translation |

## Decision

Adopt provider packages, extending CPI and existing discovery rather than creating
a second plugin framework. Keep KnowledgeBase, KnowledgeBaseClass and
KnowledgeBaseClaim in core. Keep the built-in memory provider from ADR-0001;
independent installation is required for external integrations, not for that
zero-dependency implementation.

### Ownership and Configuration

- Core owns claim authorization, lifecycle state, committed generations, quotas,
  audit, scheduling and namespace isolation. Providers own backend configuration,
  validation, SDKs, indexing, retrieval and data cleanup.
- Each package ships a versioned provider API group, typed configuration CRDs,
  adapter image, chart, least-privilege RBAC and conformance results. Names such as
  `AzureSearchBackend` are illustrative until the API change is implemented.
- Add a validated core backend configuration reference with API group, kind and
  name. Its type must match a registered provider's supported configuration kinds.
  Do not expose arbitrary JSON parameter bags or caller-supplied adapter URLs.
- A cluster-scoped class selects a configuration name resolved in the consuming
  KnowledgeBase's namespace. Provider configuration and credential references stay
  in that namespace; no arbitrary cross-namespace secret references. This gives
  cluster administrators reusable classes without granting tenant credential access.
- Reject ambiguous legacy vectorStore plus new backend-reference configuration.
  Preserve old pgvector manifests through explicit compatibility translation;
  do not silently rewrite stored objects or switch providers.

### Discovery and Execution

- Extend existing plugin discovery with knowledge capability and CPI with a typed
  knowledge extension. Negotiate protocol version and explicit keyword, dense,
  hybrid, filter, citation, refresh and deletion capabilities. Unsupported modes
  fail validation before any ingestion starts.
- Use independently deployed, out-of-process adapters with authenticated service
  identity and restricted network access. Sidecar packaging is not mandatory.
  Registration is administrator-controlled; health alone does not authorize a
  provider. Pin provider identity and endpoint for each operation.
- Keep lifecycle/ingestion calls separate from governed query traffic. Core checks
  current claims and revocation on queries, then supplies trusted namespace, KB UID
  and committed generation. Providers enforce this scope for every data operation;
  callers cannot replace it through filters. Raw backend credentials never reach
  agent pods, status, logs or CPI discovery responses.
- Require idempotency keys, deadlines, bounded retries and typed errors. Operation
  identities include namespace, KB UID, target generation and configuration digest.
  Responses include backend operation IDs and normalized source/chunk citations;
  vendor scores must not be represented as universally comparable confidence.

### Lifecycle and Removal

- Stage a new generation, check every batch-item result, validate it, then promote.
  Failed refresh preserves the prior committed generation. Model, dimensions,
  chunking or backend configuration changes require reindexing.
- Missing, incompatible or unhealthy providers cannot create Ready status or advance
  generation. Conditions must explain recovery, and reconciliation must recover
  when the provider returns. Never substitute pgvector for failed Search.
- Delete requires confirmed, idempotent cleanup before removing the finalizer.
  Retain removes the binding but preserves data and records recoverable ownership.
  Reject Archive unless the provider explicitly implements it.
- Provider removal with active bindings is blocked by the supported uninstall
  workflow. Forced removal fails closed and leaves data/finalizers for recovery;
  Helm must not cascade-delete provider CRDs, credentials or backend data.

## Implementation and Verification

1. Completed prerequisite: reject missing/skipped/invalid ingestion providers for
   configured backends; preserve counts and generation and test automatic recovery.
   This does not establish the new transport or provider health checks for Ready KBs.
2. Add core references, provider CRDs, capability negotiation and typed CPI messages
   together with conversions, deepcopy, admission and checked-in schema changes.
3. Route pgvector ingestion and governed retrieval through the contract. Test old
   manifests, real known-corpus retrieval and citations before enabling a new vendor.
4. Implement the Search package: real ingest/query, keyword/dense/hybrid support,
   per-item failure handling, refresh, tenant isolation and Delete/Retain tests.
5. Test incompatible versions, unavailable providers, revoked claims, cross-namespace
   attempts, partial ingestion, restart/retry, config changes and uninstall/reinstall.
6. Add optional cloud modules and identity/networking, then provision a disposable
   AKS environment only after resource/cost approval. Live Search and Foundry tests
   are separate opt-in gates; pricing and model availability are not yet verified.

## Consequences

Typed provider packages meet isolation and independent-release priorities while
keeping the existing core resource model. They add protocol compatibility,
registration, packaging and conformance maintenance. Pgvector migration must cover
both ingestion and query traffic; a new CRD alone does not complete integration.
Existing simulated backend-free bookkeeping and deletion are not proof of actual
backend readiness or cleanup and must not be used as provider conformance evidence.