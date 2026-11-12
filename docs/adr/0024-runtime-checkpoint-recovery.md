# ADR-0024: Runtime Checkpoint Recovery

- Status: Accepted by workspace user on 2026-10-06.
- Supplements: [ADR-0019](0019-runtime-adapter-transport.md) and
  [ADR-0023](0023-runtime-control-transport.md).
- Traces: T-0148, US-0042, FR-0600, FR-0602.

## Context

The production AutoGen adapter now owns stable controller sessions and prevents
retry from repeating a paid turn. Its ownership map is process-local. SDK
checkpoints contain sensitive conversation and tool history; automatic durable
recovery needs explicit confidentiality, replay and retention contracts.

## Priorities And Requirements

1. Never repeat an unknown paid model/tool effect after controller or worker
   failure. A missing terminal result cannot authorize another dispatch.
2. Protect retained conversation/tool history and isolate workspace encryption
   authority. Keys must not enter runtime pods, status, checkpoints or logs.
3. Bound sensitive-state retention while preserving replay-denial evidence.
   State expiry must not erase an unresolved execution claim.

## Options

| Option | Replay Safety | Confidentiality | Operations |
| --- | --- | --- | --- |
| Per-workspace key rings and quiescent-only restore | Unknown effects remain blocked | Separate workspace AES-256-GCM authority | More operator key/rotation management |
| Installation key ring with workspace-bound AAD | Can preserve the same replay fence | One key compromise exposes all enrolled workspaces | Simpler key management |
| Automatic replay after unknown outcome | Does not meet no-duplicate-effect priority | Encryption alone does not repair effect ambiguity | Rejected recovery semantics |

## Decision

Use controller-owned encrypted checkpoint persistence in both storage modes.
Operators provision per-workspace AES-256-GCM key rings with a current write key
and retained read keys, mounted read-only only into the controller. Keep them
separate from capability HMAC, artifact signing, approval binding and worker TLS
keys. Never request or record actual key values in agent conversations.

Authenticate ciphertext and binding metadata together: format/domain version,
workspace, Agent UID, AgentInstance UID, stable session ID, operation/restart
generation, runtime identity/version, canonical configuration digest and expiry.
Use a fresh cryptographic nonce per encrypted record. Reject unknown versions,
missing keys, invalid authentication, expired material or any binding mismatch.

Write the durable owned-session/execution claim before an effect-bearing Step.
Keep immutable terminal facts and compare-and-swap ownership so concurrent
controllers cannot execute or restore the same generation independently.
Only authenticated quiescent checkpoints may restore. A claimed turn with no
conclusive outcome remains accounting-uncertain and requires explicit
reconciliation; no automatic redispatch, forced successful settlement or deletion
to obtain a green state. Missing usage must not become zero usage.

Retain terminal encrypted state for seven days by default, operator-configurable
up to thirty days. Expired state cannot restore and may be purged. Keep minimal
UID/session replay-denial evidence until conclusive terminal state and safe
instance retirement. Never age-delete an unresolved execution claim. Retain old
read keys until all supported ciphertext using them is retired; removing a needed
key makes recovery unavailable rather than selecting another key or plain text.

Storage is controller-owned: PostgreSQL ciphertext records in standalone mode,
protected ciphertext records in the control namespace in Kubernetes mode. Do not
place raw tokens in AgentInstance status or mutable workload-accessible resources.
The implementation must bound payloads, queries and reclamation and respect
Kubernetes object-size limits before accepting a checkpoint.

## Consequences

These decisions authorize the recovery contract, not production key provisioning,
schema migration, installed-cluster changes or release certification. Durable
persistence/admission, encryption rotation, checkpoint/claim transaction ordering,
worker restore/session reclamation and both-mode acceptance are not implemented
by this record. Current fail-closed restart behavior remains until they pass.

## Required Verification

Test controller/worker restart, lost responses, concurrent owners, interrupted
checkpoint/terminal persistence and no second provider/tool effect. Test wrong
workspace/UID/generation/configuration, tampered ciphertext/AAD, nonce handling,
key rotation/removal, expiry, bounded retention and safe retirement. Both stores
must reject unauthorized reads/writes and preserve unresolved claims. Verify
decrypted tokens, keys and bearer capabilities never enter status or logs.
