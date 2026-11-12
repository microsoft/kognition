# ADR-0012: ReasoningProbe Canary Execution

- **Status:** Accepted
- **Date:** 2026-08-27
- **Deciders:** Kognition core team

## Context

ReasoningProbe currently treats runtime liveness as semantic health. Reusing
`RuntimeDispatcher.Dispatch` for a periodic canary would mutate the production
AgentInstance lifecycle, derive input and replay identity from production
session state, and reuse ordinary dispatch accounting markers. It would not
provide an isolated, replay-safe semantic check.

The canary must exercise the active Agent instructions and authorized Model
through the CPI provider path. It must not inherit conversation state, invoke
Tools, read or write Memory, or expose prompts and provider output in status,
events, logs, or traces.

## Priorities and Requirements

1. **Prevent unsafe replay and side effects.** A controller retry must not
  repeat a paid call with an uncertain outcome, and a probe must not mutate a
  production session or invoke a Tool.
2. **Measure the deployed reasoning contract.** The check must use the active
  Agent instructions, authorized Model, configured task, and exact expected
  output rather than infer health from runtime liveness.
3. **Bound cost, latency, and evidence.** Every execution needs enforced time
  and token limits, normal quota accounting, bounded output, and redacted
  persisted evidence.
4. **Remain deterministic and implementable.** The boundary needs typed
  requests, results, and failures that focused CI fixtures can exercise
  without a network provider.

## Decision

`ProbeReconciler` uses a dedicated `CanaryExecutor` dependency. The CPI-backed
runtime dispatcher implements this interface alongside `RuntimeDispatcher`:

```go
type CanaryExecutor interface {
    ExecuteCanary(context.Context, CanaryRequest) (CanaryResult, error)
}
```

`CanaryRequest` contains the operation ID and canonical schedule slot; the
running AgentInstance identity and active runtime session ID; the immutable
Agent revision selected by that instance; resolved instruction and Prompt
identities and versions; the authorized Model UID and resource version; the
canary task and exact expected outcome; timeout; and maximum input and output
tokens. `CanaryResult` contains the operation ID, expected and actual byte
counts and SHA-256 digests, reported token usage, provider duration, trace
identifiers, outcome, and a closed failure reason. It never contains raw
provider output. The executor performs the exact comparison while the response
exists in memory, returns typed failures, and never requires `ProbeReconciler`
to parse provider error text.

### Binding and Isolation

- The request binds the Agent namespace, name, UID, and immutable revision
  selected by `AgentInstance.spec.agentRevision`; the AgentInstance namespace,
  name, and UID; and its active runtime session ID. Resolution uses the same
  immutable Agent snapshot rules as ordinary CPI dispatch, not the mutable
  current Agent spec.
- The binding digest includes every execution-affecting dependency: resolved
  inline instructions; each referenced Prompt namespace, name, UID, and
  resource version or immutable revision; and the authorized Model and
  ModelClass namespace, name, UID, and resource version. This satisfies
  ADR-0008's exact Model-version binding.
- The executor rejects a request when any binding is missing or changed. It
  resolves and authorizes the bound Model through the same Model Gateway
  boundary used by ordinary CPI dispatch.
- The CPI request contains the bound system instructions and one user message
  containing `canaryTask`. It contains no production session context,
  conversation history, Tool definitions, Tool credentials, or Memory data.
  Tool calls and side effects are disabled. A provider response that requests
  a Tool is an execution failure.
- Canary execution never changes `AgentInstance.status.sessionID`, phase,
  production dispatch result, or production invocation counters.

### Scheduling and Replay Safety

ReasoningProbe status records the last canonical schedule slot, operation ID,
request digest, resolved versioned resource references, and terminal result. A
slot is deterministic: boundaries are anchored at the AgentInstance creation
time and separated by `intervalSeconds`. Reconcilers never use an arbitrary
local timestamp as an operation identity.

When no operation is in flight, the controller coalesces missed intervals to
the latest boundary not after its observed time; it never executes each missed
slot. It resolves the complete immutable execution binding, then uses the
resource's optimistic version to compare-and-set the slot, derived operation
ID, request digest, and versioned references into status together. The ID is a
hash of the AgentInstance UID, active session ID, immutable Agent revision, and
canonical slot. On conflict the controller reloads status: it joins the stored
operation for the same slot, skips a newer slot, and never substitutes another
ID or binding. A retry reconstructs the request and must match the persisted
digest; unavailable or changed dependencies produce `BindingChanged`, never a
new binding under the stored ID. At most one slot may be in flight per
AgentInstance. A later slot is not acquired until the current operation is
terminal, which bounds catch-up cost to one provider call at a time.

The executor uses a `CanaryOperationStore` whose claim operation atomically
creates one immutable record keyed by operation ID and a digest of every bound
request field. A claim either creates `CanaryClaimed` or returns the existing
record. Reusing an ID with a different request digest is
`InvalidConfiguration`. An append-then-list event sequence is not a valid
claim because concurrent replicas could both observe no marker and invoke the
provider.

After authorization and quota reservation, an executor must conditionally
transition `CanaryClaimed` to `ProviderStarted` before calling CPI. A competing
executor that observes `CanaryClaimed` may finish the pre-call work and compete
for that transition, but only its winner may call the provider. Repeated
requests return a stored terminal result without provider output. A
`ProviderStarted` operation with no recoverable provider result is
`AccountingUncertain` and is never sent to the provider again. This deliberately
classifies a crash between the transition and the call as uncertain because it
cannot be distinguished from a lost provider response.

A definitive failure after claim but before `ProviderStarted`, including
authorization, budget, or timeout failure, conditionally changes
`CanaryClaimed` to `CanaryFinalizing`. The record contains the typed terminal
failure, deterministic audit outbox payload, whether a reservation was
created, and the remaining finalization steps. Binding resolution and exact
tokenization happen before slot acquisition and claim; failure there produces
a normal probe configuration/binding failure without creating an operation.

Immediately after a provider response, the owner conditionally changes the
same operation to `ProviderCompleted`. That durable record contains only the
evaluated outcome, closed reason, byte counts, digests, usage, duration, trace
identifiers, bounded redacted evidence, and a deterministic audit payload; it
contains no raw input or output. If this write fails after the call started,
the result is `AccountingUncertain` and the maximum reservation remains
charged. No protocol can recover a response lost before its first durable
write, so this fails closed without under-accounting or replay.

`ProviderCompleted` conditionally advances to `CanaryFinalizing` without a
provider call. Any executor that reads `CanaryFinalizing` resumes its declared
steps. For a provider result it idempotently settles the reservation, applies
the immutable usage event, and delivers the durable audit outbox event. For a
pre-call terminal failure it idempotently releases any reservation and delivers
the audit event, without creating usage. Every step is keyed by operation ID.

The quota contract provides separate `SettleCanaryReservation` and
`ReleaseCanaryReservation` operations backed by a permanent disposition
ledger with `settled` or `released` state and the applied token counts. An
identical retry returns the recorded disposition; an attempt to change the
disposition or counts conflicts. Therefore a crash after quota mutation but
before operation-state advancement is distinguishable from a reservation that
never existed. Once every declared step is durably acknowledged, a conditional
update marks `CanaryCompleted`. This state machine makes crashes between any
terminal-intent write, quota release or settlement, usage application, audit
delivery, and terminal replay recoverable.

Canary reservations are explicitly non-reapable while their operation is
`CanaryClaimed`, `ProviderStarted`, or `ProviderCompleted`; the generic expired
reservation reaper must skip them. When a replay finds `ProviderStarted`
without a recoverable response, finalization irreversibly settles the reserved
input and output ceilings as estimated usage, marks it
`AccountingUncertain`, emits its audit event, and completes the operation. It
never refunds an ambiguous paid call.

Full completed records have a configured retention period and are then
compacted to immutable tombstones containing the operation ID, request digest,
terminal-result digest, outcome, reason, byte counts and digests, usage,
duration, and trace identifiers. This bounded metadata is sufficient to
reconstruct the same `CanaryResult`; only previews and secondary evidence are
discarded. Tombstones are never deleted or recreated. This permanent replay
fence avoids a UID-deletion cleanup race; instance deletion only removes large
evidence and secondary indexes, not operation identity.

### Time and Token Bounds

- `timeoutSeconds` defaults to 60 and bounds model authorization, provider
  execution, result persistence, and accounting through a child context.
- `maxTokens` defaults to 256 and is applied as the CPI completion-token limit.
  The executor rejects non-positive effective values and must not silently
  replace a configured lower limit with a Model default.
- Before claiming, the CPI adapter deterministically serializes the bound
  system instructions and canary message and uses the selected provider/model
  tokenizer to calculate the input-token count. The adapter rejects a request
  when exact tokenization is unavailable or when the serialized input exceeds
  the configured canary input ceiling. That calculated count is
  `maxInputTokens`; the reservation therefore has computable input and output
  bounds.
- The normal namespace `CognitiveQuota` atomically reserves
  `maxInputTokens` and `maxTokens` before provider execution using the operation
  ID as its idempotency key. Agent identity remains usage attribution, not a
  separate quota scope. The finalizer settles reported prompt and completion
  usage. Immutable usage events also use the operation ID as their idempotency
  key. Canary usage is tagged as `reasoning-probe` and correlated by operation
  ID. Missing or unpersisted provider usage is `AccountingUncertain`, not a
  successful probe.
- `TimedOut` and `ProviderFailed` apply only when the executor can prove that
  provider execution did not occur or was definitively rejected. Cancellation,
  timeout, connection loss, or an ambiguous provider error after the provider
  call starts is `AccountingUncertain` and remains non-replayable.

### Outcome Evaluation and Evidence

A canary passes only when the provider returns valid UTF-8 output whose bytes
exactly equal the UTF-8 bytes of `expectedOutcome`. Comparison performs no
trimming, case folding, Unicode normalization, substring search, or regular
expression evaluation. `expectedOutcome` and accepted output are limited to
4096 UTF-8 bytes. Output beyond the limit is `OutputLimitExceeded`; truncated
output can never pass.

Raw canary tasks, expected outcomes, Agent instructions, credentials, and
provider output are never persisted. Failure evidence contains the operation
ID, bound resource references, outcome, reason, expected and actual byte
counts, SHA-256 digests, token counts when reported, and trace identifiers. An
optional operator preview is processed by the standard reasoning-content
redactor and then truncated on a UTF-8 boundary to 512 bytes. Evidence fields
use fixed names and bounded values.

The result outcome is one of `Passed`, `OutcomeMismatch`, or
`ExecutionFailed`. `ExecutionFailed` has one of these closed reasons:

- `InvalidConfiguration`
- `BindingChanged`
- `AuthorizationDenied`
- `BudgetDenied`
- `TimedOut`
- `ProviderFailed`
- `ToolCallRequested`
- `InvalidEncoding`
- `OutputLimitExceeded`
- `AccountingUncertain`
- `PersistenceFailed`

Failure selection follows execution order and this total precedence:

1. Before slot acquisition, invalid configuration, missing or changed binding,
  exact tokenization/input-limit failure, and status persistence failure are
  checked in that order. No operation exists, so these are reported through
  probe status without an operation audit event.
2. After slot acquisition, the executor claims the complete persisted binding.
  Claim persistence failure is `PersistenceFailed`. Live authorization,
  reservation, and pre-call timeout are then checked in that order; the first
  observed failure is persisted through `CanaryFinalizing` and wins.
3. After `ProviderStarted`, an ambiguous timeout, cancellation, connection
  loss, or provider error is `AccountingUncertain` and overrides all
  unevaluated response conditions. A definitive rejection known not to have
  executed is `TimedOut` or `ProviderFailed` as applicable.
4. For a definitive response, `ToolCallRequested` takes precedence, followed
  by `OutputLimitExceeded`, `InvalidEncoding`, missing usage
  (`AccountingUncertain`), and exact equality (`Passed` or
  `OutcomeMismatch`).
5. Failure to persist `ProviderCompleted` or finish quota, usage, or audit
  finalization reports `AccountingUncertain` until recovery and temporarily
  overrides the evaluated outcome returned to the reconciler.

`PersistenceFailed` is therefore used only when required durable state fails
before a provider call can start. Probe-status persistence may be retried from
the terminal operation record and cannot cause another provider call.

`OutcomeMismatch` is never represented as a transport or provider failure.
Every successfully claimed operation emits a canary span and a versioned
`ReasoningProbeAuditEventV1`; claim-persistence failure is exempt because no
durable operation or outbox can exist. This is a separate audit payload, not an
extension of ADR-0011's closed Trust Decision v1 schema. Its canonical JSON
schema always requires `version` (`reasoning-probe/v1`), `id`, `operationId`,
`phase` (`completed`), `timestamp`, bound Agent and AgentInstance resource
references, Agent revision, Model UID and resource version, outcome, and
reason. Expected byte count and SHA-256 digest are required for every claimed
operation. Actual byte count/digest are required only when a definitive
provider response supplied output; they are absent for pre-call failures,
ambiguous execution, or responses with no output. Prompt/completion token
counts are required only for reported usage or ceiling settlement and include
an `usageEstimated` boolean. Trace/span IDs are required only when tracing was
created successfully. Strings are UTF-8 and limited to 512 bytes except fixed
64-character lowercase hexadecimal digests; counts are non-negative 64-bit
integers; conditionally absent fields are omitted rather than encoded as zero,
empty string, or null; unknown fields are rejected.

The audit ID is the SHA-256 digest of `operationId + "\x00completed"`. The
terminal operation stores the exact canonical JSON bytes used by the outbox.
Audit stores must compare those bytes when the ID already exists and return a
conflict on different content; silent first-write-wins behavior is not valid
for this event type. Delivery acknowledgement is persisted on the operation
only after the audit store accepts identical bytes. The event carries trace and
span IDs when tracing is available, and the span carries the operation ID and
bound resource identifiers. Neither surface records prompt or raw output
content.

### Deterministic Acceptance Fixtures

TC-0098 and TC-0099 use an in-process fake `CanaryExecutor`; they do not call a
network provider or reuse `RuntimeDispatcher.Status`. The fake returns an
already evaluated `CanaryResult` with byte counts and digests, never raw
output. Separate `CanaryExecutor` tests use an in-process fake CPI provider so
exact comparison can be tested inside the component that is allowed to see
provider output.

- TC-0098 configures `canaryTask` as
  `Return exactly KOGNITION_CANARY_OK` and `expectedOutcome` as
  `KOGNITION_CANARY_OK`. The fake records the full request and returns a passed
  result, reported usage, byte metadata, and fixed trace identifiers. The test
  verifies one invocation, all bindings and bounds, a reset failure count, and
  correlated bounded evidence.
- TC-0099 uses the same request but returns a pre-evaluated `OutcomeMismatch`
  result for the actual bytes `KOGNITION_CANARY_BAD`. The test verifies no
  transport-failure classification, redacted evidence no larger than 512
  bytes, operation and trace correlation, and degradation at the configured
  consecutive-failure threshold.
- Executor fixtures verify exact comparison, invalid UTF-8, output limits, and
  that CPI requests contain no Tool or Memory context. Timeout, Tool-request,
  and uncertain-accounting table cases verify the closed failure reasons.
- Replay fixtures concurrently execute the same request from two executor
  instances sharing a store and assert one claim, one quota reservation, one
  provider invocation, one usage event, and identical metadata-only results.
  A changed request under the same operation ID is rejected.
- Pre-call fixtures prove each post-claim definitive failure becomes a
  replayable terminal result and releases any reservation without usage.
  They stop after reservation creation, `CanaryFinalizing` persistence, quota
  release, and audit delivery in turn, then prove retry reaches one terminal
  result with one `released` ledger entry and no usage event. Crash-window
  fixtures prove another executor can recover `CanaryClaimed`, but that
  `ProviderStarted` without a result settles the non-reapable ceiling and
  returns `AccountingUncertain` without provider invocation. Completed replay
  is also provider-free.
- Finalization crash fixtures stop after each durable step and prove retry
  settles quota, usage, and audit exactly once. Cleanup fixtures prove compacted
  metadata returns an identical result and permanently rejects replay. A
  scheduling fixture coalesces prolonged downtime and permits only one
  in-flight slot.

## Consequences

**Positive.** ReasoningProbe measures the configured Agent and Model through a
real authorized CPI path without contaminating production sessions. Exact
matching and deterministic fixtures make semantic health reproducible in CI.
Durable operation state prevents unsafe paid-call replay and correlates probe,
usage, audit, and trace evidence.

**Negative.** The controller requires additional persisted probe-operation
state and a CPI canary adapter. A real canary consumes provider quota and cost.
Providers that cannot disable Tool requests fail closed. Exact matching
requires canary prompts and expected outputs designed for deterministic
responses.

## Alternatives Considered

| Option | Replay and isolation | Semantic fidelity | Bounds and evidence | Deterministic implementation |
|---|---|---|---|---|
| Dedicated `CanaryExecutor` | Meets: owns durable operation state and prohibits session and Tool side effects | Meets: executes the active Agent and Model with the configured task | Meets: typed request and result carry enforced bounds and accounting | Meets: a narrow fake records one request and returns typed outcomes |
| Temporary AgentInstance through `RuntimeDispatcher.Dispatch` | Does not meet: creates lifecycle and cleanup races and can collide with ordinary dispatch markers | Meets: can execute the active Agent and Model | Partial: ordinary dispatch accounts usage but lacks probe evidence limits | Partial: tests must model temporary resource lifecycle and cleanup |
| Add canary parameters to `RuntimeDispatcher` | Partial: isolation can be added, but unrelated runtime implementations inherit probe replay semantics | Meets: can execute the active Agent and Model | Meets if every implementation adopts the new contract | Partial: all dispatcher fakes must implement a probe-only operation |
| Evaluate `RuntimeDispatcher.Status.Output` | Does not meet: reads production session content | Does not meet: no configured canary executes | Does not meet: production output has different bounds and disclosure risk | Meets: status fakes are simple, but they test the wrong behavior |

The dedicated `CanaryExecutor` is selected because it is the only option that
meets all four priorities without changing production AgentInstance lifecycle
semantics. Exact byte equality is part of this option because substring or
regular expression matching would weaken semantic fidelity and deterministic
CI behavior.

## Related

- [ADR-0008: Model Gateway invocation boundary](0008-model-gateway.md)
- [ADR-0011: Trust Decision contract](0011-trust-decision-contract.md)
- [BACKLOG.md](../../BACKLOG.md)