# ADR-0023: Runtime Control Transport

- Status: Accepted by workspace user on 2026-10-06.
- Supplements: [ADR-0019](0019-runtime-adapter-transport.md).
- Traces: T-0148, US-0042, FR-0602.

## Context

Generated Kubernetes workers previously exposed plain HTTP Service endpoints.
The public AutoGen client requires HTTPS except literal loopback. The user
selected operator-provisioned worker HTTPS certificates and controller CA trust
instead of adding a controller-local runtime bridge.

## Priorities And Requirements

1. Preserve verified transport and exact Agent/instance/configuration binding.
   Service DNS must not become an exception to the client's HTTPS requirement.
2. Preserve operator ownership of certificate delivery. This implementation must
   not mint keys, enroll workloads or mutate the installed cluster.

## Options

| Option | Verified Transport | Operator Ownership | Cost |
| --- | --- | --- | --- |
| Operator worker HTTPS | Meets the existing public client contract | Existing worker TLS and CA Secrets remain operator-provisioned | Requires worker SAN coverage and explicit trust configuration |
| Controller-local bridge | Keeps the public client on loopback | Requires a new bridge transport/identity contract | Adds another ownership and failure boundary |

## Decision

Use explicit operator TLS certificate/key material for generated AutoGen worker
control and an explicit CA source for the controller. Require TLS 1.3 in the
controller client, standard hostname verification and redirect refusal. Missing
or invalid trust fails closed; never fall back to HTTP or insecure verification.

The runtime namespace contains the existing worker Secret with `tls.crt` and
`tls.key`; the controller namespace contains its existing CA Secret with `ca.crt`.
Mount these read-only. The certificate must cover the actual worker Service DNS.
HTTPS probes and mounts apply only to the AutoGen profile. Native defaults remain
unchanged. Current stateful workers require a single pinned replica.

## Consequences

Helm exposes `k8sController.runtimeControlTLS.workerSecretName` and
`existingCASecret` when an AutoGen image is selected. This configures resources;
it neither creates those Secrets nor approves rollout. SPIRE gateway transport
remains a separate outbound identity boundary.

The public adapter now has stable controller session/instance binding and a
single-turn replay fence. A lost in-memory owner is accounting-uncertain and must
not trigger automatic paid redispatch. Automatic durable checkpoint recovery,
state retention/reclamation and deployed runtimeClass-only parity remain open.

## Verification

Public lifecycle tests cover contract selection, activation, exact capability
binding, usage, retry and cancellation. A disposable actual SDK container passes
through the production configured dispatcher without repeating inference. TLS
tests reject untrusted/missing CA material and accept the explicit trust source;
generated workload tests verify read-only mounts, HTTPS probes and native cleanup.
These are local receipts, not an installed Kubernetes or release certification.
