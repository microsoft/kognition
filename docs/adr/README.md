# Architecture Decision Records (ADRs)

This directory holds Architecture Decision Records for the Kognition platform.

## Convention

- One file per decision, named `NNNN-short-title.md` where `NNNN` is a zero-padded
  sequential number (e.g. `0001-use-apiserver-storage-abstraction.md`).
- Use the [MADR](https://adr.github.io/madr/) template or an equivalent structure:
  **Context**, **Decision**, **Status**, **Consequences**, and **Alternatives Considered**.
- Status values: `Proposed`, `Accepted`, `Deprecated`, `Superseded by <ADR>`.
- Once accepted, ADRs are immutable. Subsequent changes are captured by a new ADR
  that supersedes the previous one.

## Index

- [ADR-0001 — KnowledgeBaseClaim + built-in in-memory KB provisioner](0001-knowledge-base-claims.md) — Accepted, 2026-07-03
- [ADR-0002 — Default `KnowledgeBaseClass` via annotation](0002-knowledge-base-default-class.md) — Accepted, 2026-07-03
- [ADR-0003 — API server authentication (OIDC) and TLS](0003-apiserver-authentication.md) — Accepted, 2026-07-12
- [ADR-0004 — Multi-replica watch fan-out via PostgreSQL NOTIFY](0004-watch-fanout-pg-notify.md) — Accepted, 2026-07-12
- [ADR-0005 — Secretless outbound provider auth (Azure managed / workload identity)](0005-secretless-provider-auth.md) — Accepted, 2026-07-12
- [ADR-0006: Runtime memory policy gateway](0006-runtime-memory-policy-gateway.md) - Accepted, 2026-08-18
- [ADR-0007: Agent Gateway fabric](0007-agent-gateway-fabric.md) - Accepted, 2026-08-19
- [ADR-0008: Model Gateway invocation boundary](0008-model-gateway.md) - Accepted, 2026-08-20
- [ADR-0009: Tool exec probe sandbox](0009-tool-exec-probe-sandbox.md) - Accepted, 2026-08-23
- [ADR-0010: Stdio Tool invocation sandbox](0010-stdio-tool-invocation-sandbox.md) - Accepted, 2026-08-24
- [ADR-0011: Trust Decision contract](0011-trust-decision-contract.md) - Accepted, 2026-08-27
- [ADR-0012: ReasoningProbe canary execution](0012-reasoning-probe-canary-execution.md) - Accepted, 2026-08-27
- [ADR-0013: Durable runtime autonomy gate](0013-runtime-autonomy-gate.md) - Superseded by ADR-0014, 2026-08-31
- [ADR-0014: Runtime autonomy gate completion and recovery](0014-runtime-autonomy-gate-completion-and-recovery.md) - Accepted, 2026-08-31
- [ADR-0015: Provider Integration Boundaries](0015-provider-integration-boundaries.md) - Accepted, 2026-09-19
- [ADR-0016: Installation trust boundary](0016-installation-trust-boundary.md) - Accepted, 2026-09-24
- [ADR-0017: Artifact signature trust](0017-artifact-signature-trust.md) - Accepted, 2026-10-03
- [ADR-0018: Workload identity issuer](0018-workload-identity-issuer.md) - Accepted, 2026-10-03
- [ADR-0019: Runtime adapter transport](0019-runtime-adapter-transport.md) - Accepted, 2026-10-03
- [ADR-0020: Model version binding](0020-model-version-binding.md) - Binding choice accepted, 2026-10-05; integration contract remains open
- [ADR-0021: Workload incarnation binding](0021-workload-incarnation-binding.md) - Accepted, 2026-10-05
- [ADR-0022: Python workload transport](0022-python-workload-transport.md) - Accepted, 2026-10-05
- [ADR-0023: Runtime control transport](0023-runtime-control-transport.md) - Accepted, 2026-10-06
- [ADR-0024: Runtime checkpoint recovery](0024-runtime-checkpoint-recovery.md) - Accepted, 2026-10-06; durable implementation remains open
- [ADR-0025: Model invocation contracts](0025-model-invocation-contracts.md), accepted 2026-10-22.
- [ADR-0026: Studio tutorial execution](0026-studio-tutorial-execution.md), Accepted for isolated implementation, 2026-10-29.
- [ADR-0027: Runtime approval instance configuration](0027-runtime-approval-instance-configuration.md), Proposed, 2026-11-06.

## Related

- [docs/security.md](../security.md) — operational guide that applies these decisions.
- [PROJECT.md](../../PROJECT.md) — references this directory for architectural decisions.
- [BACKLOG.md](../../BACKLOG.md) — functional requirements that drive ADR work.
