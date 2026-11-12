# ADR-0006: Runtime memory policy gateway

- **Status:** Accepted
- **Date:** 2026-08-18
- **Deciders:** Kognition core team

## Context

Kubernetes agent pods call the memory service directly. Those requests carry a
volume handle but no authenticated Agent identity, grant reference, or policy
decision. The memory service therefore cannot enforce `MemoryGrant`,
`AgentRole`, or revocation at request time. The deterministic SPIFFE URI
currently propagated to the runtime is attribution metadata, not a credential.

Kubernetes agent runtime pods require a data-plane memory authorization
contract without relying on user-supplied headers or a shared Kubernetes
default ServiceAccount. Standalone currently uses in-process CPI dispatch and
does not expose the Python runtime memory client; its raw memory service is
restricted to the controller provisioning token.

## Decision

Introduce a policy gateway for runtime memory operations.

1. The AgentInstance dispatch path issues a short-lived signed capability. Its
   claims bind the namespace, Agent, automatic AgentIdentity, AgentInstance,
   session, memory volume, audience, and expiry.
2. The runtime receives the capability only through the dispatch request. It
   sends it as a bearer credential to the memory policy gateway and never to a
   memory backend.
3. The gateway verifies the capability signature, expiry, audience, and
   request binding. It resolves the current controller-owned identity and
   re-evaluates `MemoryGrant` and `AgentRole` for every operation before
   proxying to the memory service.
4. The gateway maps read operations to `ReadOnly` and store/delete operations
   to `ReadWrite`. A revoked or changed grant takes effect on the next request.
5. The gateway stamps every authorized write with the selected `MemoryGrant`
   identity in backend metadata. When a grant is deleted, its controller
   persists a revoked-grant tombstone on the MemoryClaim and marks direct and
   derived `MemoryRecord` provenance. The gateway omits records carrying a
   revoked grant from get, list, and search responses, even when another grant
   still authorizes access to the same claim.
6. The MemoryGrant controller durably records grant evaluation and revocation
   and writes an audit entry before each claim or record cascade mutation. The
   policy gateway durably audits each valid-capability operation before
   forwarding it and records live-policy denials without forwarding upstream.
   Audit-store failure blocks the corresponding side effect.
7. Kubernetes NetworkPolicy permits agent pods to reach the gateway but not
   the memory service directly. The standalone topology applies the same
   gateway request contract without Kubernetes NetworkPolicy.

The first implementation scope protects memory operations. Tool invocation
will reuse the capability format and gateway pattern after memory enforcement
is operating in both modes.

## Consequences

**Positive.**

- Memory operations become authenticated and fail closed at the data plane.
- Grant revocation applies without waiting for a runtime rollout or capability
  expiry.
- Revocation applies to records written under the deleted grant without
   deleting their retained backend data.
- Revoked-operation denial is visible in the durable audit log within the
   five-second MVP-7 bound.
- Kubernetes runtime memory calls enforce the same capability and live-policy
   semantics regardless of the agent namespace.
- Runtime code no longer decides authorization or loads control-plane grants.

**Negative.**

- The gateway adds a network hop and needs a signing-key rotation mechanism.
- Deployments need a gateway endpoint and a secret shared only by dispatchers
  and gateway verifiers.
- Runtime memory access is unavailable until the gateway is ready when policy
  enforcement is enabled.

## Alternatives Considered

1. **Trust the propagated SPIFFE URI header.** Rejected because it is not a
   cryptographic credential and a pod can forge it.
2. **Use the default Kubernetes ServiceAccount with TokenReview.** Rejected as
   the first implementation because one shared ServiceAccount does not bind a
   request to a specific Agent or AgentInstance and has no standalone
   equivalent.
3. **Authorize only at dispatch time.** Rejected because MemoryGrant and
   AgentRole changes, including revocation, must affect subsequent operations.
4. **Add authorization directly to the memory service.** Rejected because
   control-plane policy evaluation and backend data operations should remain
   separated; a gateway can later serve tools with the same capability model.

## Related

- [docs/security.md](../security.md)
- [BACKLOG.md](../../BACKLOG.md), T-0095, T-0100, and T-0108