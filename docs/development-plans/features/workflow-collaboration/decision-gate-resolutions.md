# Workflow Collaboration Decision Gate Resolutions

Status: **approved by the feature owner on 2026-11-17**, as written. The
[plan](plan.md) and ADR-0029 reference this document. Changing a resolution
needs a recorded amendment here before dependent code relies on it. Independent
security-owner approval is still required before production activation.

Date: 2026-11-17. Baseline: `origin/main` at `9aa2c7b`.

## How To Read This Document

Each gate lists the recommended resolution, the reason, the rejected
alternative, and the test or review that proves it. Gates marked **contested**
are the ones where a reasonable reviewer could choose differently and should
receive explicit attention.

| Gate | Topic | Risk | Contested |
| --- | --- | --- | --- |
| 1 | Schema and strategy names | Low | No |
| 2 | Initial dispatch authority | High | Yes |
| 3 | Outer boundary and data release | High | Yes |
| 4 | Peer completion | Medium | No |
| 5 | Parallel joins | Low | No |
| 6 | Handoff transfer | High | No |
| 7 | Member proposal contract | Medium | No |
| 8 | Service selection | Medium | No |
| 9 | Persistence and protection | High | No |
| 10 | Cross-namespace policy migration | High | Yes |
| 11 | Stop and owner fencing | High | No |
| 12 | Accounting ownership | Medium | No |
| 13 | Protocol and SDK | Medium | Yes |
| 14 | Limits and retention | Medium | No |
| 15 | Cancellation projection and retry | Medium | No |
| 16 | Runtime release matrix | Medium | Yes |

## Source Facts Used

These were verified in the repository or upstream on 2026-11-17.

- `WorkflowStepType` has only `agent` and `tool`; an empty type is Agent
  ([`api/v1alpha1/workflow.go`](../../../../api/v1alpha1/workflow.go)).
  `WorkflowStep` uses `LocalObjectReference` for `agentRef` and `toolRef`.
- The communication gateway applies a fixed 25 second target timeout
  (`targetInvokeTimeout` in [`internal/agentgateway/server.go`](../../../../internal/agentgateway/server.go)).
  It is a request bound, not a collaboration deadline.
- The repository has no A2A dependency. `go.mod` contains gRPC, protobuf, and the
  OpenTelemetry gRPC exporter only.
- Two store backends exist: PostgreSQL (`internal/store/postgres`) and the
  Kubernetes-backed store (`internal/k8s/controller`).
- Two Agent runtimes exist: the native Go runtime (`internal/runtime/native`) and
  the Python runtime (`runtime/agent`, with an AutoGen graph option).
- The upstream A2A 1.0 specification is released. The official Go SDK
  `github.com/a2aproject/a2a-go/v2` advertises v1.0 support, gRPC, REST and
  JSON-RPC bindings, and requires Go 1.26 or newer. The official Python SDK
  also has a 1.0 line.

## Gate 1: Exact Schema And Strategy Names

**Resolution.** Adopt the plan's example schema with these exact names.

```yaml
- name: investigate
  type: collaboration
  collaboration:
    strategy: Supervisor          # Supervisor | Parallel | Handoff | Peer
    protocol: A2A                 # single enum value initially
    members:                      # 2..8 entries, unique alias
      - name: lead                # DNS-label alias, 1..32 chars
        agentRef: {name: planner, namespace: planning}
    allowedInteractions:
      - {from: lead, to: analyst}
    limits: {maxTurns: 20, maxMessages: 100, maxDelegations: 20,
             maxDelegationDepth: 3, maxParallelTasks: 2, maxTokens: 16000}
    supervisor: {coordinator: lead}
    # parallel:  {branches: [...], synthesizer: <alias, optional>}
    # handoff:   {entryMember: <alias>, maxHandoffs: <int>}
    # peer:      {finalizer: <alias>}
```

Rules:

- New type `AgentReference{name, namespace}` for collaboration members.
  `LocalObjectReference` is unchanged. Empty namespace means the Workflow
  namespace.
- Member count ceiling is **8**. The same Agent may appear under two aliases only
  if the strategy declares it explicitly; the default is a duplicate-Agent
  rejection, which keeps ownership and accounting unambiguous.
- Exactly one strategy block must match `strategy`. Any other block is rejected.
- Unknown fields, member-supplied URLs, credentials and protocol names are
  rejected by structural schema pruning plus admission.
- `collaboration` is additive to the step discriminator, so `v1alpha1` stays
  compatible. Older controllers must not receive it; see Gate 16 for the
  guarded-upgrade requirement.

**Why.** Exclusive typed blocks keep validation total and avoid free-form maps.
Eight members bounds fan-out, ledger size and status size.

**Rejected.** A generic `config` map per strategy (unvalidatable) and a separate
AgentPool resource (explicitly out of scope).

**Proof.** CRD schema tests, admission table tests, a round-trip test between
canonical and native types, and a compatibility test that existing fixtures
decode unchanged.

## Gate 2: Initial Dispatch Authority (contested)

**Resolution.** Adopt ADR-0029's operator-bound executor.

- An operator-owned, namespaced `WorkflowExecutionBinding` (protected policy,
  not authorable by Workflow authors) pins Workflow UID, revision digest, an
  executor identity, and a finite allow-list of target Agents and operations.
- A submitter needs permission to *use* the approved revision. The submitter's
  identity is recorded as initiator but its credentials are never retained.
- The controller starts members through a distinct exact initiation route with
  its own capability type. It never fabricates a member AgentInstance identity.
- Live destination authority and executor validity are rechecked at every new
  initiation, after approval or input waits, and after recovery. Revocation
  stops new dispatch immediately and moves running children to reconciliation.

**Why.** It keeps authoring, submitting and executing as three separate
authorities, which is the core finding of the security review.

**Rejected.** Letting the controller act as a super-identity (confused deputy),
and requiring a human-in-the-loop credential per run (breaks automation).

**Proof.** Authorization matrix tests for each identity pair, a revocation
test mid-run, and a test that an author without binding rights cannot select an
executor.

## Gate 3: Outer Boundary And Data Release (contested)

**Resolution.**

- Collaboration-bearing Workflows must declare explicit `inputs` and `results`
  bindings. No implicit access to prior step output or run parameters.
- Every released value carries a lineage set. Combining values unions their
  restrictions. A finalizer or synthesizer cannot lower restrictions that a
  worker's content carried.
- Templates are restricted to a reviewed function set over declared bindings.
- Result publication to the outer Workflow, status, events, and logs goes
  through one protected payload path. Status holds bounded summaries and
  content references only.

**Why.** Collaboration multiplies mixing points; conservative lineage is the
only rule that cannot be bypassed by an agent summarizing restricted content.

**Rejected.** Per-field declassification by agents (unreviewable) and
whole-run single-label tagging (blocks legitimate mixed steps).

**Proof.** Cross-surface content isolation tests (status, events, logs,
exports) and a lineage-propagation test through each strategy.

## Gate 4: Peer Completion

**Resolution.** A designated `finalizer` member. Completion is a single
conditional write by the finalizer after all *required* tasks are settled.
No voting. Quiescence (no runnable task, no waiting input, finalizer has not
completed) fails the step with reason `PeerQuiescence`.

**Proof.** Concurrent finalizer-completion race test; deadlock fixture.

## Gate 5: Parallel Joins And Partial Results

**Resolution.** All required branches must succeed. Optional branches are
declared `optional: true`, recorded in the result manifest, and their failure
does not fail the step. A synthesizer is optional. Any-success and quorum are
deferred and rejected by validation.

**Proof.** Table tests for each branch outcome combination.

## Gate 6: Handoff Transfer Contract

**Resolution.** Exactly one owner. Transfer is a persisted proposal, acknowledged
by the recipient, which advances a monotonically increasing **owner epoch**
before the recipient acts. The recipient activates only after the previous
owner's required outcomes are known. Every owner request carries its epoch; a
stale epoch is denied. Context is filtered by the handoff's declared fields.

**Proof.** Late old-owner request denial test; crash between proposal and ack;
lost acknowledgement preserves uncertainty without replay.

## Gate 7: Member Proposal Contract

**Resolution.** Versioned structured operations (`delegate`, `handoff`,
`finalize`, `complete`) with `schemaVersion`, and a trusted actor binding taken
from the capability, never from payload. Mapping onto A2A: operations travel as
a typed `DataPart` under a Kognition media type, and are declared through an
A2A extension URI in the Agent Card. They are explicitly Kognition-specific
and not claimed as core A2A.

**Proof.** Schema fuzz tests, a spoofed-actor test, and an unknown
`schemaVersion` rejection test.

## Gate 8: Service Selection

**Resolution.** Resolve each member Agent to exactly one ready, authorized
`AgentService`. Zero or more than one candidate blocks dispatch with reasons
`NoServingBinding` or `AmbiguousServingBinding`. An optional
`serviceRef` on the member disambiguates. Record resolved UIDs and revision;
replacement during a run is a changed target.

**Proof.** Ambiguity, replacement and revocation tests.

## Gate 9: Persistence And Protection

**Resolution.** One logical store contract with five logically separate
record kinds (definition, step execution, child task ledger, messages and
artifacts, accounting/evidence references). Large content goes to protected
backend records. Status carries bounded counters and references only.
Both backends implement the shared contract suite in
`internal/store/workflowactivationtest`, extended per record kind.
Schema migration ships before writers; a disabled feature rejects new
collaboration definitions yet recovers accepted runs.

**Proof.** Shared contract tests against PostgreSQL (real database) and the
Kubernetes store (fake plus a real API-server qualification).

## Gate 10: Cross-Namespace Policy Migration (contested)

**Resolution.**

- Communication policy callers become namespace-qualified. An entry without a
  namespace keeps legacy same-namespace meaning and can never authorize a
  foreign-namespace caller.
- Reference grants and communication authority stay separate checks, both
  evaluated live.
- Existing policies are untouched. Collaboration with cross-namespace members
  requires new qualified entries; a reviewed legacy-compatibility mode is not
  added.
- Rollout order: ship qualified-caller reader, then writer, then enable.

**Why.** Avoids silently widening any existing policy.

**Proof.** Regression test that unqualified entries still deny foreign
callers, plus a policy-engine golden suite for old and new forms.

## Gate 11: Cross-Plane Stop And Owner Fencing

**Resolution.** A single-gate **CommitDispatch** performed after the plane claim:
a dispatch commits only if the run is not stopping and the owner epoch matches,
atomically in the gate store. Stop sets the gate and then drains. A claimed
operation that loses CommitDispatch makes zero target calls. Maintenance
drain is a separate, explicit operation.

This builds on the existing activation-gate foundation. Required additions:
RBAC/admission protection of the gate, a trusted clock for the Kubernetes
adapter, and plane-claim verification.

**Proof.** Barrier-forced stop-versus-dispatch races on PostgreSQL and on a
real Kubernetes API server; pending approval completing after stop does not
execute.

## Gate 12: Accounting Ownership

**Resolution.** One provider cost record per actual provider call, charged
under the existing workspace policy. The run aggregates logical usage. A
separate named policy allowance may cap by origin, but never creates a second
cost record. Child allowances are reserved from the parent before dispatch.
Unknown usage is charged conservatively until reconciled.

**Proof.** No-double-charge test across namespaces; reservation exhaustion test.

## Gate 13: Protocol And SDK (contested)

**Resolution.**

- Profile: A2A **1.0**, one binding initially (**gRPC**, since the repo already
  carries gRPC; JSON-RPC/REST deferred and rejected by validation).
- SDK: `github.com/a2aproject/a2a-go/v2`, pinned. Adopt only if the
  compatibility spike passes the criteria below; otherwise stop and return to
  review rather than hand-writing protocol models.
- Polling is the recovery baseline. Streaming is optional and off by default.
  Push notifications are disabled.
- The 25 second gateway timeout stays for submit/status requests; collaboration
  deadlines are persisted separately.

Spike pass criteria: task and context identity semantics, error mapping,
serialization stability, Agent Card signing support, extension negotiation,
idempotency behavior for retried sends, list/access scoping, and a clean
`govulncheck` plus license review. Go 1.26 or newer is required and already
satisfied.

**Rejected.** Python SDK on the controller side (wrong language plane) and a
custom protocol (violates "prefer supported libraries").

## Gate 14: Limits And Retention

**Resolution.** Required finite values with a reviewed ceiling table
(initial ceilings, all lower-boundable by policy):

| Limit | Ceiling |
| --- | --- |
| members | 8 |
| maxTurns | 100 |
| maxMessages | 500 |
| maxDelegations | 100 |
| maxDelegationDepth | 5 |
| maxParallelTasks | 8 |
| step deadline | 24h |
| retained message/artifact content | 7d after run end |
| replay-denial and exact-effect evidence | 90d |

No defaults: omission is a validation error until reviewed defaults exist.

## Gate 15: Cancellation Projection And Retry

**Resolution.** Internal states per the plan. Outer mapping: Succeeded to
`Ready`, Failed to `Failed`; waiting and uncertain stay nonterminal with
conditions. Cancelled maps to `Terminated`/`Cancelled` pending the
public-consumer review. Retry only effect-safe attempts, with a new attempt
number; uncertain submissions are never resubmitted.

## Gate 16: Runtime Release Matrix (contested)

**Resolution.**

| Dimension | Supported at release | Explicitly unsupported |
| --- | --- | --- |
| Store | PostgreSQL, Kubernetes | any other |
| Agent runtime | native Go runtime | Python/AutoGen until its A2A server conformance passes |
| Protocol | A2A 1.0 over gRPC, polling | streaming, push, REST, JSON-RPC |
| Strategies | all four, or none | any subset |

Unsupported combinations fail validation or dispatch with a named
`UnsupportedCapability` reason. Rollback is a guarded drain or a
supported-reader upgrade; the feature stays disabled until the full matrix
qualifies.

## Implementation Entry Checklist

1. User approves or amends the resolutions above.
2. ADR-0029 is updated and accepted; plan gates marked resolved.
3. Feature branch is created from `origin/main`; the existing uncommitted
   activation-foundation work is moved onto it with consent.
4. First implementation commit: characterization tests only.
