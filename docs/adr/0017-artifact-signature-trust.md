# ADR-0017: Artifact Signature Trust

- Status: Accepted by workspace user on 2026-10-03.
- Traces: OPEN-K8S-10, OPEN-SEC-1, FR-0305, US-0022, T-0074, T-0075.

## Context

The signed-artifact admission hook accepts any nonempty signature. The user chose
operator-pinned signing keys instead of keyless issuer/subject trust. Existing
unsigned workspaces remain opt-in; required-signature workspaces must fail closed.

## Options

| Option | Benefit | Cost or limitation |
| --- | --- | --- |
| Operator-pinned public keys | Explicit offline trust, no admission dependency on transparency/OIDC services | Administrator owns provisioning, rotation, signer scope and removal |
| Sigstore keyless identity | Issuer/subject policy and transparency evidence | Requires verified certificate chains, time and transparency contracts; not this slice |

## Decision

Use the Sigstore verifier library with pinned administrator-configured public
keys. Trust configuration belongs to the admission process, not Tool/Model or
tenant Workspace content. Missing, malformed or unsupported trust fails closed
when signatures are required. Do not accept a signer key from the artifact.

An artifact signature binds a versioned payload to resource kind, namespace,
name and normalized executable specification with the signature field excluded.
ResourceVersion, status and server-owned metadata are not signing inputs. Tool
and Model signature envelopes identify an approved key and detached signature;
altered specification, scope, key or signature must fail verification.

Manifest verification proves that a trusted signer approved the specification.
It does not prove a mutable remote endpoint's bytes. Existing digest/version
pinning remains required wherever immutable artifact identity is promised; OCI
registry provenance and keyless transparency verification are separate contracts.

Public keys may overlap during rotation; removal rejects subsequent writes signed
only by a removed key. No private keys or signature bodies are logged. Existing
required-signature workspaces must migrate old presence-only strings to real
signatures before the corrected admission hook accepts writes.

## Verification

Require real-key Tool and Model acceptance plus forged/wrong-key, changed
scope/specification, malformed/unknown envelope, missing trust, cancellation and
production-chain tests. Validate both-mode shared admission and controlled
configuration before deployment; do not alter the running installation here.
