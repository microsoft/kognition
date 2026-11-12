# ADR-0027: Runtime approval instance configuration

Status: Proposed\
Date: 2026-11-06\
Scope: Supplemental execution-reference contract for
[ADR-0014](0014-runtime-autonomy-gate-completion-and-recovery.md)

## Context

A supervised Tool request received an authenticated approval on the tutorial
cluster. The next poll failed with a resource-version conflict and execution
remained `NotStarted`. Only the AgentInstance reference changed: its UID and
generation remained equal while status reporting advanced `resourceVersion`.
Agent, AgentIdentity, Tool and AutonomyClass versions remained unchanged.

RuntimeApproval must bind the exact operation, configuration, identity,
authorization and session. Observational instance updates must not invalidate
an otherwise unchanged operation.

## Options

| Option | Benefits | Costs and risks |
| --- | --- | --- |
| Retain exact instance resourceVersion binding | Existing format and strict comparison remain unchanged. | Status updates can invalidate approved operations; reporting would need to pause across approval, including concurrent progress and usage. |
| Bind instance execution configuration with a signed digest | Progress/status reporting continues; meaningful configuration or identity changes still invalidate approval. | Adds an optional API/schema field and requires coordinated reader/writer qualification; legacy requests cannot silently acquire new semantics. |

## Proposed decision

Use `agentInstanceRef.configurationDigest`, a canonical SHA-256 digest of the
AgentInstance spec, generation, labels, annotations, owner references, deletion
timestamp and runtime session ID. Include this digest in the keyed action
binding. Preserve the instance's UID, name, namespace and caller session.

For a valid digest-bearing instance reference, canonical signing and immutable
comparison treat its resourceVersion as an observation rather than execution
configuration. The persisted reference retains the original observed version.
All other resource references keep exact version binding. Configuration digests
are rejected outside `agentInstanceRef`.

Legacy references without a digest remain strictly version-bound. Their
existing signatures remain valid; they cannot be silently converted into
digest-bearing approvals. Payload, target, identity, grants, policy, binding
key/version and decision-authorization checks remain unchanged.

## Consequences and qualification

- Tests cover status-only continuation, changed specs, generation, ownership,
  labels, annotations, deletion, runtime/caller sessions, identity, target,
  authorization references and payload.
- The approval stores only the digest, not the execution configuration.
- Kubernetes schema and API conversion must retain the optional field.
  Deploy schemas before compatible writers; older readers must fail closed.
- Pending legacy operations require explicit maintenance/recovery handling
  before a writer change. This decision does not authorize action replay,
  policy relaxation or rewriting retained approvals.
- Repository validation does not certify the deployed repair. The tutorial
  cluster remains unchanged until its preserved source is restored and the
  narrowly scoped build is qualified.

## Change log

| Date | Trigger | Amendment |
| --- | --- | --- |
| 2026-11-06 | failure (validation) | Add instance configuration binding after a real approved continuation failed on status-only version drift. |
