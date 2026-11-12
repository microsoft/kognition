# ADR-0018: Workload Identity Issuer

- Status: Accepted by workspace user on 2026-10-03.
- Traces: OPEN-SEC-2, FR-0401, US-0026, T-0085, T-0086, T-0087.

## Context

Deterministic AgentIdentity URIs and authenticated session capabilities exist,
but they do not implement workload SVID issuance/rotation. The user chose SPIRE.

## Options

| Option | Benefit | Cost or limitation |
| --- | --- | --- |
| SPIRE and SPIFFE Workload API | Proven attestation, short-lived SVID rotation and trust bundles | Requires operator-managed agents, registration and protected socket access |
| Existing enterprise PKI | Uses existing organizational issuer | Requires a separate enrollment, rotation and trust-bundle adapter contract |

## Decision

Integrate the official SPIFFE client and an operator-managed SPIRE issuer.
Kognition must never create an ad-hoc CA or distribute workload private keys
through CRD status, annotations, session requests or reports. Protect the Workload
API socket and validate the expected Agent-incarnation SPIFFE identity before
using the SVID. Certificate lifetime, rotation/readiness and trust-domain binding
must be observable without revealing key material.

New authenticated transports validate peer SPIFFE identity and trust bundles;
SVID presence alone does not bypass live Agent/session/grant/policy checks.
Unavailable, expired or mismatched identity fails closed in the opted-in profile.
The existing capability profile remains supported and is not relabeled as SVID
authentication. Schema, transport, rollout and cross-mode acceptance are required
before claiming FR-0401 complete.

SPIRE installation, enrollment and activation are separate administrator actions.
Do not replace the running cluster or enroll namespaces without explicit plans.

## Verification

Test correct/wrong workload identity, trust domain, expiry, issuer outage,
rotation, peer mismatch and reconnect; use an isolated SPIRE target for deployed
acceptance. Keep informational URI evidence distinct from cryptographic identity.
