# Remaining Sample Blockers: Design Security

- Mode: Architectural.
- Date: 2026-10-24.
- Scope: [proposed blocker resolution](remaining-sample-blockers-plan.md).
- Verdict: APPROVED_WITH_CONTROLS for planning; no implementation or deployment
  clearance is issued.

The design preserves the accepted model and knowledge execution boundaries.
The controls below are implementation acceptance gates, not findings from an
exploitable-vulnerability audit or a production security qualification.

## Attack Surface And Trust Boundaries

| Boundary | Permitted data/access | Required restriction |
| --- | --- | --- |
| Agent to Knowledge Gateway | Query text and session capability | Current identity, instance, claims and permitted KBs |
| Trusted query service to model execution | Authorized embedding Model and query | Live model policy/grant, immutable invocation binding, quota/audit |
| Knowledge Gateway to pgvector adapter | Typed, scoped CPI query | Mutual TLS and exact registered peer/endpoint |
| Adapter to PostgreSQL | Scoped data and namespace-local credentials | Verified database TLS, least-privilege role and SQL predicates |
| Uploader/ingester to local S3 | Sample objects and development credentials | Isolated fixture, authenticated S3 and exact uploaded corpus |

Agent Pods must receive neither Azure/model credentials nor database DSNs.
Adding provider/database egress to repair the legacy runtime path is excluded.
Provider configuration and credentials remain namespace-local. No new Azure
resource, role assignment or public endpoint is proposed.

## Required Controls

| ID | STRIDE concern | Control and test |
| --- | --- | --- |
| SC-01 | Spoofing and elevation | Validate live Agent/instance/session/claim identity before deriving the embedding Model. An embedding grant cannot become arbitrary Model selection or weaken chat assignment checks. |
| SC-02 | Tampering and isolation | Bind current backend configuration UID/digest and committed KB generation; parameterize scoped SQL. Reject staged, foreign and stale data even when it has a better similarity score. |
| SC-03 | Information disclosure | Keep secrets and provider routing out of runtime configuration, errors, logs and citations. Verify denied Agent network paths and permitted gateway paths. |
| SC-04 | Spoofing in transport | Preserve CPI mutual TLS and PostgreSQL verify-full; test wrong peer, hostname, trust and expired certificates. No plaintext fallback. |
| SC-05 | Denial of service and repudiation | Bound query/input/vector/result sizes, timeouts and connection pools. Apply quotas before paid work and persist correlated authorization/success/failure evidence. |
| SC-06 | Supply chain and fixture exposure | Pin a qualified upstream image and platform, retain provenance/SBOM/scan evidence, run without privilege, and expose only authenticated S3. A maintained repository is not proof of a safe image. |
| SC-07 | Lifecycle integrity | Publish bindings only after real index confirmation; preserve prior generations on failure. Confirm scoped cleanup before finalizer removal and retain old data through migration. |

Implement explicit failure semantics. A failed embedding or database call must
not become a valid zero-match search. No automatic fallback may redirect queries
to another provider, Model, index or image.

## Reasoning

### Decisions

| Decision | Applied principle | Alternatives considered | Justification | Confidence |
| --- | --- | --- | --- | --- |
| Complete the governed pgvector query path | ADR-0015 | Runtime credentials; direct database allow; embeddings-only patch | Live egress and credential boundaries make the smaller patch incomplete | High |
| Reuse CPI and generic invocation profiles server-side | ADR-0025 | Foundry-specific embedding client/settings in Python | Authorization and parameter translation remain at their existing owners | High |
| Qualify maintained SeaweedFS before fixture replacement | Supply-chain ownership | Archived MinIO build; unverified mirror; RustFS alternative | Upstream source/release and amd64 publication are available; runtime qualification remains required | Medium |

### Assumptions

- Generic S3 compatibility is sufficient; no MinIO-specific API is required.
- The current target is isolated local testing, not production clearance.
- Existing Foundry inference permissions remain sufficient; new role grants are
  not an assumed solution to the observed runtime authentication failure.

### Missing Information

- SeaweedFS hardening, resource use and exact SDK/ingester interoperability need
  real tests before accepting the candidate image.
- Database TLS, deployed provider registration/rotation and migration need
  target-specific evidence.
- The documented installer drain limitation needs feasibility verification
  before an upgrade can be scheduled.
- Rebuilt images need fresh advisory evidence and their own disposition.

The implementation cannot proceed to deployment until these controls have
evidence and the original guarded installation/activation protocol is satisfied.
