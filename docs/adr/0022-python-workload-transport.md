# ADR-0022: Python Workload Transport

- Status: Accepted by workspace user on 2026-10-05.
- Supplements: [ADR-0018](0018-workload-identity-issuer.md),
  [ADR-0019](0019-runtime-adapter-transport.md) and
  [ADR-0021](0021-workload-incarnation-binding.md).
- Traces: US-0026, US-0042, FR-0401.

## Context

The real AutoGen 0.7.5 worker requires protobuf 5.29.x. The inspected Python
SPIFFE SDK requires protobuf 6.31.1 or newer. Installing both into this worker
breaks its supported dependency contract. The user selected a Go loopback
transport alongside the Python worker using the official Go Workload API source.

## Priorities And Requirements

1. Preserve the real AutoGen 0.7.5 dependency contract. Its tested worker must
    remain installable without replacing the SDK's required protobuf major.
2. Keep cryptographic identity in the verified Go source. Python must not gain
    private-key handling or replace exact peer and Agent-incarnation checks.

## Options

| Option | SDK Compatibility | Key Isolation | Operational Cost |
| --- | --- | --- | --- |
| Python SDK in the worker | Fails the pinned protobuf requirement | Keys enter Python | Single process and direct TLS |
| Go loopback transport | Independent SDK dependencies preserve the worker | Keys stay in the verified Go source | Colocated process, protected socket and explicit readiness wiring |

## Decision

Keep SPIRE keys and trust bundles in the Go transport. Python sends protected
gateway operations and their existing scoped bearer capabilities to a pinned
literal-loopback HTTP origin. The Go transport uses TLS 1.3 and exact configured
gateway SPIFFE peer identities for outbound requests. This preserves the pinned
AutoGen dependencies and the existing gateway authorization contract.
It meets both priorities; the in-process Python SDK fails each one for this
pinned worker profile.

The transport accepts only the enumerated Model, Tool, Memory and Communication
operations. Reject arbitrary upstream URLs, redirects, query strings and
administrative routes. Bound requests and responses to four MiB. Forward scoped
authorization and selected correlation headers without recording credentials.

Require the Agent namespace, name and immutable UID to derive its enrolled URI.
The worker's SPIRE profile checks transport readiness before initializing its
graph. The default capability profile remains unchanged. An SVID supplements
live Agent, identity, session, grant and policy checks; it does not replace them.

## Consequences

Operators own process colocation, protected Workload API socket access and
pod/service-account registration. Generated Kubernetes sidecar/socket wiring,
resource-status propagation and both-mode deployed rollout acceptance remain
open. This record authorizes no production enrollment or installed-cluster edits.
Fixture selectors granting multiple identities to one test UID are disposable
test scaffolding and are unsuitable production enrollment conventions.

## Verification

The authorized disposable Docker fixture passed actual SPIRE issuance, rotation,
wrong-incarnation denial and a real AutoGen Python turn through the loopback
transport to an exact-peer mTLS gateway fixture. The scoped capability arrived
unchanged and the SDK made exactly one gateway call. Unit checks cover wrong
peers, issuer unavailability, unknown operations and non-loopback configuration.
These receipts establish local transport behavior, not production certification.
