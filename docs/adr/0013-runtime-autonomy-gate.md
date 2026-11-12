# ADR-0013: Durable Runtime Autonomy Gate

- **Status:** Superseded by ADR-0014
- **Date:** 2026-08-31
- **Deciders:** Kognition core team

## Context

Kognition's Tool, Memory, Model, and Agent Communication gateways enforce
identity and live authorization at their execution boundaries. `TrustProfile`
scoring also changes an Agent's `status.currentClass` over time. These pieces do
not yet enforce the third gate described in PROJECT.md: the effective
`AutonomyClass` may require a human decision even after AuthN and AuthZ allow an
action.

The existing approval resources cannot safely fill this role:

- `ApprovalDecision` is bound to a `WorkflowRun`, destination step, and cyclic
  iteration.
- `AutonomyReview` approves a class promotion, not an action.
- The in-memory `ApprovalBroker` loses state across process or replica changes.
- A Tool invocation receipt prevents duplicate execution, but it does not
  represent a human decision and is currently claimed before authorization.

Runtime approval can outlive an HTTP request and a gateway process. It must not
be transferable to changed input, another target, identity, session, resource
revision, or autonomy policy. Demotion must take effect before the next action,
and a retry must neither bypass a pending or rejected decision nor repeat a
possibly completed side effect.

The current `Tool` and `ToolClass` contracts have no trusted side-effect or risk
classification. Therefore, `high-risk-only` cannot safely infer that a Tool is
low risk from its protocol, description, input, or caller-provided metadata.
The Model policy also has no dedicated approval-policy field. This decision
must not invent either classification at runtime.

## Decision Drivers, in Priority Order

1. **Safety and non-bypassability.** No protected side effect occurs without a
   current AuthN, AuthZ, and required approval decision.
2. **Exact binding and replay safety.** A decision applies to one immutable
   action intent and cannot authorize a changed or repeated action.
3. **Durability and cross-mode parity.** Standalone and Kubernetes behavior
   survives process, replica, and controller restarts.
4. **Immediate policy tightening.** Demotion, revocation, or resource drift is
   observed before execution.
5. **Auditability and redaction.** Operators can explain pending, approved,
   rejected, expired, and executed outcomes without storing sensitive payloads.
6. **Incremental delivery.** The first T-0214 slice must fit existing gateway
   boundaries without conflating workflow, promotion, and runtime approvals.

## Decision

Introduce a shared runtime autonomy resolver and a new durable
`RuntimeApproval` resource. Gateways call the resolver after successful AuthN
and AuthZ and before quota mutation, execution receipt claim, write-ahead
execution audit, or target dispatch.

`ApprovalDecision` and `AutonomyReview` retain their existing specialized
semantics. They are not accepted as runtime action approvals.

### Effective Autonomy Policy Resolution

For every protected request, the resolver loads the live Agent and lists
`TrustProfile` resources in the Agent's namespace whose `spec.agentRef.name`
exactly matches the Agent name.

Resolution follows these rules:

1. Exactly one matching profile is the valid configured state. More than one
   is ambiguous and fails closed.
2. The effective class name is, in order:
   `status.currentClass`, `spec.initialClass`, then `supervised`.
3. No matching profile is a cold-start state and uses the built-in supervised
   policy. It does not inherit a more permissive `Agent.spec.autonomyClassRef`.
4. `AutonomyClass` is cluster-scoped. Kubernetes loads it without a namespace;
  standalone uses its reserved `system` scope and never permits a workspace
  object to shadow it. The resolved resource version and stable incarnation
  identity are evidence and part of the approval binding.
5. An unreadable profile, ambiguous profiles, missing class, malformed class,
   unsupported policy value, or unavailable store produces
   `approval.status: unavailable` and blocks the action. It never degrades to
   `never`.
6. If the built-in supervised resource itself is unavailable during cold
   start, the resolver applies an internal fail-safe policy requiring approval
   for every protected action. Approval creation must still be durable; if its
   store is unavailable, the request fails closed.

`AutonomyClass.spec.requiresApproval: true` is an umbrella restriction and
requires approval for every protected action. Otherwise the action-specific
field controls the result:

- `always` requires approval.
- `never` does not require approval under the action-specific rule.
- `high-risk-only` requires approval when trusted control-plane metadata marks
  the action high risk. Unknown risk is high risk; caller input can never lower
  risk.
- Empty or unknown values fail closed as `always`.
- `costThreshold` is not an authorization input in the first T-0214 slice. The
  current API stores an unqualified integer while PROJECT.md describes billing
  currency, and the protected planes do not share a trusted pre-execution
  estimator. Optional or plane-specific interpretation would be unsafe. A
  later contract must define the unit, comparison boundary, trusted estimator,
  overflow behavior, and missing-estimate default before this field can require
  or waive approval. It can never weaken the action-specific rule.

For the first T-0214 slice:

- Tool invocation uses `toolInvocation`. Because no trusted Tool risk field
  exists, `high-risk-only` requires approval for every Tool invocation.
- Memory store and delete use `memoryWrite`; those mutations are high risk.
  Memory reads are not protected by this action policy.
- Exact AgentService capability exchange uses `externalCommunication`; unknown
  communication risk is high risk. Approval occurs before issuing the exact
  executable capability, not after target invocation.
- Model completions remain governed by AuthN, AuthZ, quota, and audit. They are
  not mapped to another policy field. Adding model approval or trusted risk
  metadata requires a separate contract change.

This conservative interpretation makes the shipped assisted tier approve all
Tools until a trusted risk classification is added. It is intentionally safer
than treating every unclassified Tool as low risk.

### RuntimeApproval Contract

A gateway creates or retrieves one `RuntimeApproval`. Canonical name format v1
is `ra-` followed by the first 52 lowercase base32 characters of SHA-256 over
the length-delimited workspace and invocation ID. This is DNS-safe and does not
expose a caller-selected identifier. Hash collisions are detected as binding
conflicts, never treated as a match. Its immutable spec contains only bounded,
redacted binding data:

- workspace and invocation ID;
- plane and operation;
- Agent, AgentIdentity, AgentInstance UID, and session references;
- target kind, scope, name, stable incarnation identity, and resource version;
- effective TrustProfile and AutonomyClass references and resource versions;
- the live authorization evidence references used by Gate 2;
- a versioned canonical action binding tag and binding-key version; and
- server-assigned creation and expiry timestamps.

The binding tag is HMAC-SHA-256 over a domain-separated canonical action
envelope and covers the complete normalized action intent, including payload
or arguments. The resource stores only the tag and key version. Secret values,
raw Tool arguments, prompts, model output, memory content, and communication
messages are never persisted in the approval. Comparisons use constant-time
equality. Binding keys are shared across replicas, restricted to approval
components, and retained for at least the maximum approval and replay-retention
period so rotation cannot make an existing approval unverifiable.

Canonical action format v1 includes the binding version, plane, operation,
workspace, invocation ID, normalized target references, and complete typed
action payload. JSON entry points reject duplicate keys, non-finite numbers,
and unsupported values before deterministic canonical encoding. Map keys and
authorization-reference collections are deduplicated and sorted. The format
has shared standalone, Kubernetes, Go, and Python test vectors; a binding tag
supplied by a caller is never accepted as binding evidence.

Every persisted binding reference includes kind, scope, namespace where
applicable, name, resource version, and a stable incarnation identifier. The
incarnation identifier is the Kubernetes UID in Kubernetes mode and a
store-generated immutable object identity in standalone mode. Missing identity
or version fails closed. A target without authoritative persisted identity is
permitted only when its plane-specific contract names and binds the immutable
external target fields.

The status has two related state machines:

- decision: `Pending` to exactly one of `Approved`, `Rejected`, or `Expired`;
- execution: `NotStarted` to `Claimed`, then `Completed`, `Failed`, or the
  reconcilable state `Uncertain`; reconciliation may move `Uncertain` only to
  `Completed` or `Failed` with an explicit disposition.

Spec fields and terminal decisions are immutable. Decisions are accepted only
through the `RuntimeApproval/decision` Kognition control-plane operation. The
server derives the canonical approver subject from authenticated context and
authorizes the exact `approve` or `reject` verb against the named approval and
workspace. Authentication alone is insufficient. Kubernetes mode performs a
SubjectAccessReview for this approval-specific permission; standalone uses an
equivalent persisted control-plane role binding. `AgentRole` and runtime Agent
credentials do not grant human approval authority. Self-approval by the
requesting Agent, AgentIdentity, or AgentInstance is denied.

The Kubernetes authorization path validates the presented identity through
TokenReview, or through an explicitly configured trusted mapping of canonical
OIDC issuer and subject to Kubernetes username and groups. Display username,
email, and caller-forwarded groups are never authority. The SubjectAccessReview
uses API group `kognition.dev`, resource `runtimeapprovals`, subresource
`decision`, the approval workspace, exact approval name, and distinct custom
verb `approve` or `reject`. The decision service may create
SubjectAccessReviews but may not impersonate subjects. Gateway and runtime
service accounts receive neither decision verb.

Generic create, update, patch, status update, and delete operations cannot set
or alter decision fields. Admission and API authorization enforce that rule,
and gateway and controller service accounts receive only their required
transition permissions. A caller-supplied `approver` value is never authority.
Authorization unavailability fails closed.

Every request receives a bounded expiry. The initial implementation uses a
15-minute default and rejects configured durations over 24 hours. `Pending`
may transition to exactly one of `Approved`, `Rejected`, or `Expired`; those
decision values are immutable. An approved decision is consumable only when
the backend's effective claim time is strictly earlier than `spec.expiresAt`:
database time for PostgreSQL and skew-adjusted trusted host time for Kubernetes.
At the applicable boundary or later it remains historically approved but is
effectively expired and can never be claimed. Expiry after a successful claim does not
revoke the action that already started. An operator must submit a new
invocation ID to try again.

### Store and Evidence-Fence Contract

Both modes implement one shared `RuntimeApprovalStore` with linearizable,
idempotent operations:

1. `CreateOrGet` creates the immutable binding or returns the exact existing
  binding; any mismatch returns conflict. Creation and expiry are assigned by
  the store on first creation, excluded from caller binding comparison, and
  copied from the existing object on an identical retry.
2. `Decide` performs the one allowed `Pending` transition using the expected
  resource version and returns the persisted state.
3. `ExpireDecision` conditionally moves only an expired `Pending` decision to
  `Expired` using backend-authoritative expiry semantics.
4. `ClaimExecution` atomically verifies the complete binding, `Approved`
  decision, decision-evidence fence, backend-authoritative expiry rule, and
  `NotStarted` execution before changing execution to `Claimed`.
5. `FinalizeExecution` performs one allowed `Claimed` transition and
  is idempotent for the same disposition.
6. `ReconcileExecution` conditionally moves only `Uncertain` to `Completed` or
  `Failed`, stores the reconciliation disposition, and uses the separate
  authenticated reconciliation authority and evidence fence.
7. `GetByInvocation` returns the resource or immutable tombstone before current
  policy is allowed to waive approval.

PostgreSQL performs each transition in one database transaction and evaluates
expiry using database time. Kubernetes uses resource-version compare-and-swap
on the status subresource and re-reads after an ambiguous transport result. It
uses the gateway's authenticated host clock with a configured maximum clock
skew and permits a claim only when `now + maximumClockSkew < spec.expiresAt`.
The default skew allowance is 30 seconds; configuration may only increase it.
This deliberately expires Kubernetes approvals early and never claims at or
after the conservative boundary. Clock health outside the configured bound
fails readiness and blocks claims.

A timeout or unknown write result is never interpreted as a successful claim.
Generic read-then-update claim code outside this abstraction is prohibited.
The successful conditional state transition is the execution linearization
point; PostgreSQL provides strict store-time expiry and Kubernetes provides the
documented conservative bounded-skew guarantee.

Each approval transition stores a deterministic, bounded audit outbox payload
and event ID in the same atomic transition. An idempotent dispatcher writes the
Trust Decision event and marks the corresponding evidence-delivered fence by
compare-and-swap. `ClaimExecution` requires the approved-decision fence.
Pending creation follows the same rule: the gateway does not return an
actionable approval reference until the pending event is durable. Duplicate
delivery of the deterministic event ID is an idempotent no-op.

### Request and Retry Semantics

When approval is required, the gateway durably creates the exact-bound pending
resource, records a Trust Decision v1 event with outcome `pending`, reason code
`approval_required`, approval reference, and `execution.status: not_started`,
then returns a non-successful pending result carrying only the invocation and
approval references. HTTP transports use `202 Accepted`.

The caller retries the same action with the same invocation ID after approval.
A fresh short-lived capability may be used only when it identifies the same
AgentInstance UID and active session. Capability expiry does not erase an
approval, but an ended or changed session makes it unusable.

On every retry, the gateway repeats AuthN and live Gate 2 authorization, resolves
Gate 3 again, recomputes the binding tag, and compares all binding references. A
changed request, target revision, identity, session, grant, policy, profile, or
class cannot consume the decision. An existing pending, rejected, expired, or
stale approval for an invocation ID cannot be bypassed because a newer policy
is more permissive; the caller must create a new action intent with a new
invocation ID.

The reads immediately before `ClaimExecution` revalidate Gate 1, Gate 2,
TrustProfile, AutonomyClass, and target incarnation/version against the binding.
Each read is authoritative for its store. The claim is the action-start
boundary for approval replay ownership, but it cannot transact with all policy
resources. Therefore the authorization freshness guarantee is the completion
of the last authoritative Gate 1, Gate 2, TrustProfile, AutonomyClass, and
target read. Changes observed by those reads invalidate the binding; changes
committed after the last read apply to subsequent actions, matching
Kognition's documented in-flight demotion semantics. An implementation must
not claim or dispatch after any unrelated blocking work between that read set
and `ClaimExecution`. A future shared policy-generation fence may narrow this
window but is not implied by a RuntimeApproval resource-version CAS. The
synthetic supervised fallback has a fixed policy identifier, semantic version,
and policy tag in place of a class resource identity; changing that fallback
invalidates unclaimed approvals.

An approved retry atomically changes execution from `NotStarted` to `Claimed`.
Only the claimant may proceed. Competing retries return the durable current
state. A restart while `Pending` or `Approved/NotStarted` is safe because a new
replica can continue from the resource. A restart after `Claimed` never
implicitly re-executes. If completion cannot be proven, the state becomes or
remains `Uncertain` and requires operator reconciliation; availability is
sacrificed rather than risking a duplicate side effect.

`Uncertain` is never retried automatically. Reconciliation is available only
through an authenticated `RuntimeApproval/reconcile` operation and a distinct
permission from approve or reject. The operator supplies a bounded reference
to external evidence. Proven completion transitions `Uncertain` to `Completed`
with disposition `ReconciledCompleted`; proven non-execution transitions it to
`Failed` with disposition `ReconciledNotExecuted`. Inability to prove either
leaves the state uncertain. The invocation ID remains consumed and neither
transition returns execution to `NotStarted`. Reconciliation never dispatches
the target; actor, evidence reference, approval reference, previous state,
disposition, and server timestamp pass through the same audit evidence fence.

Gateway lookup by workspace and invocation ID occurs before current Gate 3
policy may waive approval. Nonterminal approvals are never deleted. Terminal
resources remain queryable for 30 days and are then compacted into immutable
binding tombstones containing workspace, invocation ID, plane, binding version
and tag, stable Agent attribution key, terminal decision and execution state,
and retention deadline. The Agent key participates in per-Agent record and
byte accounting.
Tombstones remain for 90 days and never less than the corresponding receipt,
audit, or capability replay window. During either period, changed binding
conflicts and identical binding returns the durable terminal state.

Both modes enforce configurable total record and byte quotas per workspace and
Agent across live approvals, outbox payloads, and tombstones. Defaults are
10,000 records and 64 MiB per workspace, and 1,000 records and 8 MiB per Agent;
operators may lower but not disable them. Compaction runs before rejecting a
creation. If limits remain exceeded or compaction is unavailable, new approval
creation fails closed. Audit entries keep their independent retention policy;
compaction never deletes audit evidence.

Tool receipt claim moves after Gate 3 approval and uses the same action binding.
The receipt remains the Tool result replay ledger. The RuntimeApproval is the
human-decision and pre-execution claim ledger. Pending approval must never leave
a Tool receipt permanently appearing as an executing Tool.

### Transport Contract

Every approval-eligible transport requires a caller-stable invocation ID of at
most 128 bytes. If live resolution requires approval and the identifier is
absent, the gateway returns a bounded client error without creating an
approval. Tool APIs expose a structured pending result containing invocation
ID, approval reference, state, and expiry; in-process callers use the same
asynchronous contract. Memory store and delete require
`X-Kognition-Invocation-ID` or an equivalently versioned body field.

Communication approval binds the complete message before an executable exact
capability is issued. Capability exchange therefore carries the message
transiently to the gateway, which computes the binding tag and stores no raw
message. The exact capability binds the RuntimeApproval UID, invocation ID,
binding version, and binding tag. Invocation recomputes the tag from the
received message and rejects a mismatch before receipt claim or target
contact. No legacy helper that cannot represent pending state may bypass Gate
3.

### Gate and Side-Effect Ordering

The mandatory order is:

1. validate request shape and exact capability binding;
2. Gate 1: authenticate current identity and active session;
3. Gate 2: evaluate live role, grant, and enforced policy;
4. Gate 3: resolve live trust/class policy and obtain an exact approval when
   required;
5. atomically claim approved execution;
6. reserve quota or budget and claim the plane-specific replay receipt;
7. persist the ADR-0011 write-ahead allowed/approved decision;
8. dispatch the target side effect;
9. persist terminal receipt, approval execution state, usage, and audit data.

Read-only checks may occur before Gate 3, but quota consumption, executable
capability issuance, receipt ownership that denotes execution, and target
contact may not. Failure to persist pending, approved, rejected, expired, or
write-ahead execution evidence blocks the corresponding transition or side
effect.

A Gate 2 denial never creates an approval, because approval cannot grant a
permission. A policy tightening found on retry invalidates the old binding. A
policy loosening never revives or bypasses a terminal or pending decision.

### Trust Decision Evidence

Runtime autonomy events reuse ADR-0011 and its closed redaction contract:

- pending: `pending` / `approval_required` / `not_started`;
- approved before execution: `approved` / `approval_approved` / `not_started`;
- rejected: `rejected` / `approval_rejected` / `blocked`;
- expired: `rejected` / `approval_rejected` / `blocked`, with bounded reason
  `approval expired`;
- a post-claim completion gap: `failed` / `target_failed` /
  `execution.status: unavailable`, with `targetReached` omitted and bounded
  reason `target completion unavailable after execution claim`;
- reconciled proven completion: `completed` / `target_completed` / `completed`,
  with `targetReached: true`;
- reconciled proven non-execution: `failed` / `target_failed` / `failed`, with
  `targetReached: false` and a bounded reconciliation reason;
- allowed execution and terminal target outcomes continue to use the existing
  plane-specific events and reference the RuntimeApproval.

The binding tag is approval-store evidence, not an unrestricted extension to
Trust Decision v1. ADR-0011 remains closed and contains only the decision
resource reference. RuntimeApproval remains the authoritative recovery record
for `Claimed` and `Uncertain`, which ADR-0011 cannot represent directly.

## Security Review

The decision was reviewed against STRIDE:

- **Spoofing:** approval authority comes from authenticated control-plane
  context and an approval role, never a submitted approver string.
- **Tampering:** immutable spec, canonical binding tag, resource revisions, server
  timestamps, and optimistic concurrency bind the decision to one action.
- **Repudiation:** pending and terminal decisions are durably audited before
  their effects, with approver and approval references.
- **Information disclosure:** only bounded references and a keyed tag are stored;
  sensitive action bodies are excluded from approval and audit payloads.
- **Denial of service:** expiry and bounded fields limit abandoned approvals;
  implementations may rate-limit creation but cannot bypass approval under
  load.
- **Elevation of privilege:** Gate 3 follows Gate 2, unknown state fails closed,
  approval cannot override AuthZ, and live drift is rechecked before execution.

Residual risk remains for a crash after a non-idempotent target completed but
before completion was persisted. The safe state is `Uncertain`; automatic
replay is prohibited. Target-native idempotency can improve recovery but is not
assumed by this decision.

## Options Considered

| Option | Safety | Exact binding and replay | Durability and parity | Policy tightening | Auditability | Incremental fit |
|---|---|---|---|---|---|---|
| New `RuntimeApproval` plus shared resolver | Strong: fail-closed boundary before effects | Strong: keyed tag, revisions, invocation, and atomic claim | Strong: store-backed in both modes | Strong: live re-evaluation invalidates drift | Strong: dedicated reference and lifecycle | Moderate implementation cost; clean gateway insertion |
| Extend workflow `ApprovalDecision` | Weak: workflow coordinates do not identify every runtime action | Weak: no target revision, session, or full action binding | Durable, but semantics are coupled to WorkflowRun | Weak outside workflows | Confusing: workflow vote and runtime execution share a type | Low initial schema cost, high coupling and migration cost |
| Reuse `AutonomyReview` | Weak: a promotion approval could be mistaken for action authority | Weak: binds class transition, not invocation | Durable in both stores | Incorrect: approving promotion is not approving an action | Misleading operator evidence | Low code reuse but violates resource purpose |
| Keep the in-memory `ApprovalBroker` | Process-local only; restart can bypass or strand state | Weak across retries and replicas | Fails restart and Kubernetes replica parity | Process-dependent | Volatile and incomplete | Lowest initial effort, unacceptable correctness |
| Block every action synchronously until an approver responds | Safe only while one process remains healthy | Connection identity can bind a request but not durable replay | Fails restart and timeout behavior | Cannot reliably re-evaluate after disconnect | Partial | Simple prototype, unsuitable for gateways |

The selected option is the only one that satisfies the first three priorities
without overloading an existing resource with incompatible identity and
lifecycle semantics.

## Consequences

**Positive.**

- Dynamic trust tier changes affect the next consequential action.
- Human authority is exact, durable, redacted, and non-transferable.
- Pending decisions and gateway restarts cannot silently execute or duplicate
  a side effect.
- Standalone and Kubernetes share one resolver and lifecycle contract.
- Existing workflow and promotion approvals remain understandable.

**Negative.**

- T-0214 requires a new resource, authenticated decision operation, admission
  protection, store/controller integration, and gateway changes.
- Conservative unknown-risk handling makes `high-risk-only` equivalent to
  `always` until trusted risk metadata is designed.
- Approval can expire while a runtime capability or session changes, requiring
  a new action intent.
- Crash uncertainty may require operator reconciliation and can reduce
  availability for non-idempotent targets.

## Implementation Constraints for T-0214

T-0214 must include unit and cross-mode tests for:

- configured, cold-start, missing, ambiguous, malformed, and custom classes;
- `always`, `never`, `high-risk-only`, umbrella approval, and proof that the
  deferred `costThreshold` neither weakens nor unpredictably changes them;
- pending, approved, rejected, expired, stale, conflicting, and unavailable
  decisions;
- process restart, replica retry, concurrent claim, and post-claim uncertainty;
- exact request, target revision, identity, instance UID, session, Gate 2
  evidence, TrustProfile, and AutonomyClass binding;
- Tool receipt ordering and no side effect before durable approval and audit;
- Memory write/delete and communication capability issuance boundaries;
- approval-role enforcement, approver derivation, redaction, and bounded data;
- Trust Decision v1 evidence in PostgreSQL and Kubernetes ConfigMap modes.

T-0214 must also enforce at most one TrustProfile per Agent through admission,
maintain an admission-controlled Agent-incarnation lookup key for efficient
cross-mode resolution, bound pending-approval creation per workspace and
Agent, and fail closed when those limits or indexes are unavailable. It must
register `RuntimeApproval` in the standalone type registry and add Kubernetes
types, conversion, CRD status subresource, admission, and store-adapter cases.

The implementation must not add model approval, trust caller-supplied risk,
reuse workflow decisions, or auto-replay an uncertain action as part of this
slice.

## Related

- [ADR-0014: Runtime autonomy gate completion and recovery](0014-runtime-autonomy-gate-completion-and-recovery.md) supersedes this decision after post-acceptance implementation review.

- **OPEN items closed:** None. `OPEN-TOOL-3` was already resolved for
  workflow-authored human approval; this ADR preserves that mechanism and
  separately decides the previously task-scoped T-0358 runtime gate.

- [ADR-0006: Runtime memory policy gateway](0006-runtime-memory-policy-gateway.md)
- [ADR-0007: Agent Gateway fabric](0007-agent-gateway-fabric.md)
- [ADR-0008: Model Gateway invocation boundary](0008-model-gateway.md)
- [ADR-0010: Stdio Tool invocation sandbox](0010-stdio-tool-invocation-sandbox.md)
- [ADR-0011: Trust Decision contract](0011-trust-decision-contract.md)
- [PROJECT.md](../../PROJECT.md), section 10B
- [BACKLOG.md](../../BACKLOG.md), T-0358 and T-0214
