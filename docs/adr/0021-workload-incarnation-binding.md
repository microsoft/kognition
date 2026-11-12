# ADR-0021: Workload Incarnation Binding

- Status: Accepted by workspace user on 2026-10-05.
- Supplements: [ADR-0018](0018-workload-identity-issuer.md).
- Traces: US-0026, T-0085, T-0086, T-0087, FR-0401.

## Context

The existing informational Agent identity uses workspace and Agent name.
Deletion and recreation can reuse these names. The user selected an immutable
Agent UID in the cryptographic workload identity, with operator-managed
registration tied to the Agent's dedicated pod/service account.

## Options

| Option | Benefit | Limitation |
| --- | --- | --- |
| Agent UID binding | Distinct incarnations | Needs protected enrollment |
| Operator convention | Reuses deployment | Needs URI and selector contract |

## Decision

Use the configured SPIFFE trust domain with this canonical workload URI:

```text
spiffe://<trust-domain>/workspace/<namespace>/agent/<name>/incarnation/<agent-uid>
```

Agent UID denotes the Agent resource incarnation, not an AgentInstance session.
Validate canonical namespace/name/UID components before constructing the URI.
SPIRE registration selectors and socket access remain operator-controlled. A
caller-supplied label or URI must not establish enrollment authority.

Use the official Workload API client and verify the expected SVID, certificate
chain, matching private key, trust domain, lifetime and exact peer identity.
Issuer watch errors make the opted-in source unavailable until a verified
update arrives. Do not persist keys or certificate material in resource status.

Keep existing informational URIs and capability authorization unchanged until
transport/schema compatibility is implemented. A valid SVID does not replace
live Agent, session, grant or policy checks.

## Consequences

The UID convention is resolved. The tested Workload API source is a foundation;
transport factories, readiness/status wiring, protected pod/service-account
enrollment, both-mode integration and isolated deployed acceptance remain open.
No installation, socket mount or SPIRE enrollment is authorized by this record.

## Verification

Test Agent recreation, wrong UID/domain, certificate/key mismatch, duplicate
identities, expiry, issuer outage/reconnect, rotation and peer mismatch. Confirm
status contains only identity, lifetime, generation and readiness metadata.
