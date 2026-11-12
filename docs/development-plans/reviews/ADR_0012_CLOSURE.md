# ADR-0012 ReasoningProbe Closure Ledger

- **Status:** Complete
- **Date opened:** 2026-08-28
- **Scope:** T-0125, T-0126, T-0127
- **Contract:** [ADR-0012](../../adr/0012-reasoning-probe-canary-execution.md)
- **Branch baseline:** `main` at `98d5caba`

## Purpose

This ledger controls closure of the known gaps between ADR-0012 and the
ReasoningProbe implementation. Work proceeds one finding at a time in the
order recorded here. A passing broad test suite does not close a finding
without focused evidence for its required behavior and failure paths.

ADR-0012 remains the authority when this ledger and the decision differ. New
scope or an ambiguous contract requires an ADR amendment before implementation.

## Closure Rules

A finding can use only these states: `Open`, `In Progress`, `Blocked`, or
`Closed`. Only one finding may be `In Progress` at a time unless two findings
must change atomically to keep the repository compiling.

A finding becomes `Closed` only when all of these conditions hold:

1. The implementation satisfies every acceptance criterion listed for the
   finding.
2. Focused tests cover the successful path, each material failure branch,
   retry behavior, and relevant concurrency or crash windows.
3. Tests assert durable effects, not only returned values or call counts.
4. Package coverage does not regress, and all changed branches have direct
   behavioral coverage or a recorded justification.
5. Privacy checks prove that raw tasks, expected output, instructions,
   credentials, provider output, and unrestricted arguments do not reach
   status, persistence, audit, usage, logs, or traces.
6. Focused validation and the applicable shared gates pass from a clean test
   process with uncached execution where practical.
7. The implementation diff has been reviewed against the cited ADR clauses.
8. The evidence table contains exact commands, results, and commit hashes.

Tests that pass before remediation are baseline evidence only. They do not
prove that an uncovered requirement is implemented.

## Worktree Controls

The worktree contained no staged changes when this ledger was opened. It also
contained a large set of unrelated tracked changes and generated local output.
The following controls apply throughout remediation:

- Do not create a blanket baseline commit for the current dirty tree.
- Stage explicit files and, where necessary, explicit hunks.
- Do not use destructive cleanup, reset, checkout, or broad restore commands.
- Preserve unrelated changes in `.gitignore`, `BACKLOG.md`, and other files.
- Exclude `node_modules`, `test-results`, reports, rendered manifests,
  binaries, coverage output, credentials, and runtime diagnostics.
- Regenerate checked-in artifacts only from their owning source definitions.
- Inspect `git diff --cached` before every commit.
- Commit directly to `main` only after the corresponding validation gate
  passes.

The committed `.gitignore` already excludes the largest local output groups.
Its current unstaged version removes several of those exclusions, so worktree
cleanliness is not a prerequisite for this effort. Explicit path staging is
the isolation mechanism.

## Finding Summary

| ID | Finding | ADR area | Status | Blocks |
|---|---|---|---|---|
| RP-001 | Failure reasons are not a closed type | Outcome evaluation | Closed | RP-002, RP-003, RP-007, RP-008 |
| RP-002 | Executor-owned lifecycle timeout has incomplete edge-case proof | Time bounds | Closed | RP-003, RP-004 |
| RP-003 | Post-claim pre-call finalization is only partially implemented | Replay and accounting | Closed | RP-004, RP-008 |
| RP-004 | Scheduling conflict and one-in-flight behavior lack proof | Scheduling | Closed | T-0125 closure |
| RP-005 | Claimed canaries do not create a dedicated span | Evidence | Closed | RP-006, RP-008 |
| RP-006 | Usage lacks explicit `reasoning-probe` classification | Accounting | Closed | RP-008 |
| RP-007 | Completed operations lack permanent tombstones | Retention and replay | Closed | RP-008 |
| RP-008 | Internal, Kubernetes, conversion, and CRD schemas diverge | API parity | Closed | RP-009, T-0125 closure |
| RP-009 | Strict-mode trustworthiness readiness access is fragile | Acceptance test | Closed | T-0125 closure |
| RP-010 | Go patch-version intent is unresolved | Toolchain | Closed | Final commit |
| RP-011 | Final traceability and privacy matrix is incomplete | Quality gate | Closed | T-0125 closure |

## RP-001: Closed Failure Reason Model

**Observation:** `CanaryResult.ClosedReason` and persisted audit reason fields
are unrestricted strings. The executor currently emits
`InputTokenLimitExceeded`, which is not in ADR-0012's closed taxonomy.

**Required implementation:**

- Introduce a `CanaryFailureReason` type shared by executor results, durable
  terminal records, status, conversions, and audit serialization.
- Define exactly these failure values: `InvalidConfiguration`,
  `BindingChanged`, `AuthorizationDenied`, `BudgetDenied`, `TimedOut`,
  `ProviderFailed`, `ToolCallRequested`, `InvalidEncoding`,
  `OutputLimitExceeded`, `AccountingUncertain`, and `PersistenceFailed`.
- Keep `OutcomeMismatch` as an outcome, not a failure reason.
- Represent a passing reason canonically without inventing another failure
  value.
- Reject unknown persisted and API values during validation or decoding.
- Map exact-tokenization and input-limit failures to the ADR-defined
  pre-claim result without creating an operation.

**Acceptance criteria:**

- Compile-time types prevent arbitrary failure literals in production paths.
- Table tests cover every allowed reason and reject unknown values.
- Precedence tests prove the ADR-ordered winner for combined response defects.
- No error-string parsing determines the persisted reason.
- Existing terminal records remain decodable or receive an explicit migration.

## RP-002: Executor-Owned Lifecycle Timeout

**Observation:** `ProbeReconciler` creates an outer timeout, while the executor
does not establish one child context that bounds its complete lifecycle.

**Current implementation:** Commit `a7a41444` establishes the configured child
context in `ExecuteCanary` and proves that the provider receives its deadline
and that retry does not repeat a fenced provider call. Commit `223af0e0`
durably classifies a reservation-stage deadline as `TimedOut` without a
provider call, reservation disposition, or usage event. The focused
finalization fixture expires the lifecycle context during settlement, preserves
`CanaryFinalizing`, and completes under a fresh bounded retry without invoking
CPI again.

**Required implementation:**

- Derive one child context in `ExecuteCanary` from the effective configured
  timeout.
- Use it for authorization, reservation, provider execution, result
  persistence, reservation settlement or release, usage, and audit delivery.
- Preserve a caller context as an outer cancellation boundary.
- Distinguish proven pre-call timeout from ambiguous cancellation after
  `ProviderStarted`.
- Permit retry to resume durable finalization after the original execution
  context expires without permitting another provider call.

**Acceptance criteria:**

- A timeout before `ProviderStarted` produces durable `TimedOut` finalization.
- A timeout or cancellation after `ProviderStarted` produces
  `AccountingUncertain` and maximum settlement.
- A timeout during finalization leaves resumable durable intent.
- Retry uses a new bounded context and completes remaining idempotent steps.
- No tested timeout path can invoke CPI more than once.

## RP-003: Durable Pre-Call Failure Finalization

**Observation:** Authorization, budget, and timeout failures after a claim can
return without a replayable terminal operation, reservation release, and audit
completion.

**Current implementation:** Commits `9ac7993f`, `e094a857`, and `223af0e0`
persist replayable terminal results for reservation denial, authorization
denial, and reservation-stage timeout without CPI or usage. Commit `673f8ba7`
persists reservation presence across PostgreSQL reloads so finalization can
release or settle the correct disposition. Commit `dbf2a3c9` proves recovery
after each durable pre-call finalization boundary, including a second executor
recovering a reservation left in `CanaryClaimed`. PostgreSQL reaper coverage
proves that reservations remain protected in `CanaryClaimed`,
`ProviderStarted`, and `ProviderCompleted`.

**Required implementation:**

- Permit `CanaryClaimed -> CanaryFinalizing` with typed terminal result,
  deterministic audit bytes, reservation-presence state, and declared
  finalization steps.
- Release an existing reservation through the permanent disposition ledger.
- Emit no usage event for a provider call that provably did not start.
- Deliver the immutable audit event and mark completion only after all
  declared steps are acknowledged.
- Make identical retries converge and conflicting disposition attempts fail.

**Acceptance criteria:**

- Focused fixtures cover authorization denial, reservation denial, and
  pre-call timeout.
- Crash fixtures stop after reservation, terminal-intent persistence, release,
  and audit delivery, then recover to one identical terminal result.
- Each fixture records at most one `released` disposition and no usage event.
- Another executor can recover `CanaryClaimed` without bypassing authorization
  or invoking CPI after a terminal winner exists.
- Reservation reaping skips all protected nonterminal canary phases.

## RP-004: Canonical Scheduling Conflict Semantics

**Observation:** Canonical slots and operation IDs exist, but a status CAS
conflict currently requeues without proving reload, join, newer-slot, or
one-in-flight behavior.

**Required implementation:**

- Reload AgentInstance status after a claim conflict.
- Join the stored operation when its canonical slot matches.
- Skip execution when the stored slot is newer.
- Preserve the stored operation ID, digest, and resource bindings.
- Refuse a later slot while the current slot has no terminal result.
- Coalesce missed intervals to the latest eligible boundary.

**Acceptance criteria:**

- Deterministic tests cover same-slot conflict, newer persisted slot, changed
  binding under one ID, prolonged downtime, and concurrent reconcilers.
- At most one slot and one provider call are in flight per AgentInstance.
- Conflict recovery never derives a replacement identity for a persisted slot.
- Terminal replay can repair probe status without invoking CPI.

**Closure evidence:** `ProbeReconciler` now reloads AgentInstance status after
a reasoning-slot claim conflict. It joins an unfinished same-slot winner using
the persisted operation ID, request digest, and resource bindings, and skips a
newer winner. An unfinished older slot remains owned until its terminal result
is persisted, preventing later-slot acquisition. Deterministic fixtures prove
same-slot join and status repair, newer-slot skip, dependency-drift rejection,
prolonged-downtime coalescing, and concurrent reconcilers sharing one operation
identity with one provider call. The concurrent fixture passes the race
detector.

## RP-005: Canary Span and Correlation

**Observation:** Result, usage, and audit types can carry trace identifiers,
but successful claims do not create a dedicated canary span.

**Required implementation:**

- Create one canary span for every successfully persisted claim.
- Record bounded operation, Agent, AgentInstance, Model, and revision
  identifiers as attributes.
- Propagate trace and span IDs into terminal result, usage, audit, and
  tombstone records when tracing is available.
- Preserve correlation through replay and crash recovery.
- Never attach sensitive request or response content.

**Acceptance criteria:**

- In-memory exporter tests prove one span per claim and no extra replay span.
- Attribute tests prove required identifiers and absence of prohibited data.
- Result, usage, audit, and compacted replay expose consistent correlation.
- Tracer failure does not weaken replay safety or prevent finalization.

**Closure evidence:** Commit `60b99aee` creates one
`kognition.reasoning.probe` span only after this executor wins a persisted
claim. The span contains bounded operation, Agent, namespace, AgentInstance,
revision, and Model identifiers. It excludes instructions, tasks, expected
outputs, provider output, credentials, and arbitrary provider error text.
Trace and span identifiers are copied into the terminal result before its
provider fence and are preserved in usage, audit, and current-operation replay.
Completed replay emits no additional span. Export and tracer failures are
non-authoritative and cannot prevent durable finalization. Both controller
assemblies inject the configured trace pipeline into the canary executor.

The compacted-replay part of the third acceptance criterion depends on the
permanent tombstone store owned by RP-007. Tombstone trace propagation and
compacted-replay evidence remain required for RP-007; RP-005 does not claim
that nonexistent compaction behavior has been tested.

## RP-006: Reasoning Probe Usage Classification

**Observation:** Canary usage currently uses `model_inference` without an
explicit bounded `reasoning-probe` classification.

**Required implementation:**

- Add a bounded usage-purpose field or the established equivalent to
  `CostRecord` and both cost stores.
- Set the value to `reasoning-probe` for canary usage while preserving model
  inference aggregation compatibility.
- Include operation ID, reservation ID, immutable model binding, estimated
  usage state, and trace correlation.
- Validate and compare immutable duplicate events byte-for-byte or by an
  equivalent canonical record comparison.

**Acceptance criteria:**

- Reported provider usage is classified and settled exactly once.
- Ambiguous execution records estimated ceiling usage with the same class.
- Pre-call terminal failures create no usage.
- PostgreSQL and Kubernetes cost stores preserve equivalent classification.
- Existing cost aggregation and API tests remain compatible.

**Closure evidence:** Commit `007629ac` adds the bounded `purpose` value
`reasoning-probe` and an immutable estimated-usage flag while retaining
`model_inference` as the aggregation operation. Reported provider usage keeps
the legacy JSON shape and sets the stored estimated flag to false. Ambiguous
provider execution records the input count and output ceiling with the flag
set to true. Existing denial and timeout tests continue to prove that
pre-call failures emit no usage.

PostgreSQL migration `017_usage_purpose.sql` adds constrained purpose and
non-null estimated columns; the internal and Helm copies match. PostgreSQL
and Kubernetes tests physically round-trip the classification, reject
unknown purposes, and reject duplicate event identifiers when either purpose
or estimated state changes. Operation ID, reservation ID, immutable model
binding, pricing, token counts, duration, and trace correlation remain part
of each immutable usage record. The repository has no deployed functional
harness that directly observes this internal classification, so closure uses
the controller behavior tests and both production persistence implementations
rather than citing an unrelated end-to-end scenario.

## RP-007: Permanent Tombstones and Retention

**Observation:** Completed operation records have no configured compaction path
or permanent tombstone lookup capable of reconstructing the same result.

**Required implementation:**

- Add immutable tombstone storage keyed by operation ID.
- Store request digest, terminal-result digest, outcome, typed reason, byte
  metadata, usage, duration, and trace identifiers.
- Compact eligible completed records after a configured retention period.
- Discard previews and secondary evidence while retaining permanent operation
  identity.
- Consult tombstones before creating a claim.
- Never delete or recreate a tombstone during instance cleanup.

**Acceptance criteria:**

- Compacted replay reconstructs an identical `CanaryResult` without CPI.
- Reuse with a different request digest returns a conflict.
- Repeated and concurrent compaction is idempotent.
- Retention selects only completed records older than the configured cutoff.
- AgentInstance deletion cannot remove the replay fence.
- Memory and PostgreSQL implementations pass equivalent contract tests.

**Closure evidence:** Commit `35fffc55` adds an immutable PostgreSQL tombstone
keyed by operation ID and preserves the exact serialized `CanaryResult`,
request and result digests, bounded terminal metadata, usage, duration, and
trace identifiers. Claims consult tombstones before creating an operation:
the same request digest replays without provider, settlement, usage, audit, or
release effects, while a changed digest returns a conflict. The in-memory
executor contract test exercises the same replay and conflict behavior.

Compaction rechecks eligibility under transaction and advisory locks, inserts
the tombstone and deletes the active row atomically, and remains idempotent
under repeated and concurrent calls. Only completed rows older than the cutoff
with reservation, audit, and applicable usage finalization acknowledgements
are eligible. Oversized results fail tombstone insertion without deleting the
active replay record. Tombstones exclude secondary usage and audit payloads,
cannot be updated or deleted, and survive AgentInstance deletion.

The leadership-scoped retention runnable uses a 30-day default, supports an
explicit disabled value, rejects invalid or negative configuration, and
retries periodic failures. Both controller modes and Helm deployments receive
the setting, migration 018 is packaged identically, and operator documentation
records the configuration contract.

## RP-008: API, Conversion, and CRD Parity

**Observation:** The internal API carries canary input bounds and durable status
metadata that the Kubernetes Agent and AgentInstance schemas do not fully
represent.

**Required implementation:**

- Add `maxInputTokens` to the Kubernetes ReasoningProbe spec.
- Add canonical slot, operation ID, request digest, immutable bindings, and
  metadata-only terminal result to Kubernetes probe status.
- Use the closed outcome and reason types on both API surfaces.
- Update internal and Kubernetes conversions and deepcopy generation.
- Regenerate checked-in CRDs from owning Go markers.
- Verify all resources embedding the shared definitions remain valid.

**Acceptance criteria:**

- Internal to Kubernetes to internal round trips preserve every canary field.
- Deepcopy tests prove no aliasing for slices and nested terminal metadata.
- Generated CRDs expose matching bounds, required fields, and closed enums.
- Helm rendering and Kubernetes resource parity checks pass.
- No manual-only CRD delta remains after regeneration.

**Closure evidence:** Commit `c46adea4` adds `maxInputTokens`, durable
reasoning operation identity, immutable resource bindings, and metadata-only
terminal results to the Kubernetes API. Bidirectional conversion preserves the
complete internal contract. Generated-equivalent deepcopy logic prevents
aliasing of binding revisions and terminal results. Kubernetes terminal
outcomes and reasons use closed types with strict JSON decoding, and commit
`219f1a3b` proves that programmatically malformed terminal values are discarded
without dropping the surrounding probe status.

The owning kubebuilder markers and checked-in AgentInstance CRD expose the same
required fields, defaults, and closed enum values. This repository has no
pinned `controller-gen` dependency or generation target, so the
generated-equivalent deepcopy and CRD files were synchronized manually in
controller-tools format. Focused marker-to-CRD tests protect that parity and
leave no schema rule defined only in the checked-in CRD.

Focused conversion, API, deepcopy, strict-enum, and CRD integration tests pass.
The native surface check and live Kubernetes parity exercise passed for all 17
resources, and the disposable namespace and cluster-scoped fixtures were
deleted. Helm lint and all 226 chart regression assertions passed. The affected
packages passed the race detector, the full repository suite passed, and
repository-authored coverage remained 84.3% against the 80% minimum.

**Post-closure audit:** Commit `b010d5b8` closes two defects found during an
independent review of the completed RP-008 work. Bidirectional AgentInstance
conversion now clones schedule-slot and resource-revision pointers instead of
retaining caller-owned state. Internal and Kubernetes terminal results now
reject impossible outcome and reason combinations during encoding, decoding,
and conversion. The AgentInstance CRD enforces the same presence invariant at
admission, and marker parity tests verify exact enum cardinality and the CEL
rule. Regression tests cover both conversion directions, malformed internal
and Kubernetes values, valid terminal round trips, and pointer ownership.

The post-closure source passed focused tests, affected-package race tests,
tagged API integration, the full race-enabled repository suite, repository
integration, Helm lint, all 226 chart assertions, and the 84.3% authored-code
coverage gate. Server-side CRD dry-run succeeded. Live Kubernetes parity passed
for all 17 resources on Docker Desktop, and all disposable resources were
removed.

## RP-009: Strict-Mode Acceptance Readiness

**Observation:** The trustworthiness E2E script directly accesses nested
readiness properties that may be absent under PowerShell strict mode.

**Required implementation:**

- Inspect property existence before reading optional readiness data.
- Preserve the existing control-plane readiness assertion and actionable
  failure evidence.
- Keep cleanup unconditional and scoped to the run identifier.

**Acceptance criteria:**

- Missing, null, false, and true readiness shapes have deterministic tests.
- Strict mode produces an assertion failure rather than a property exception.
- The live Docker Desktop acceptance run passes and leaves no namespace or
  labeled resources behind.

**Closure evidence:** Commit `4af75e75` routes optional EndpointSlice
conditions and readiness through the existing presence-aware property helper.
Absent, null, and false readiness now reach the existing actionable
`Service/<name> has no ready EndpointSlice addresses` assertion. True readiness
returns the ready endpoint count.

The focused strict-mode regression parsed and invoked the production helper
functions and passed all four required readiness shapes. The live Docker
Desktop acceptance report passed all 11 assertions with no failure and reported
successful cleanup. An independent post-run scan confirmed that the disposable
namespace was absent and no run-labeled resources remained across 75
namespaced, list-capable API resources.

## RP-010: Go Toolchain Intent

**Observation:** The worktree changes the module directive from Go 1.26.5 to
1.26.6 while also adding tokenizer dependencies. The patch-version change has
not been attributed to T-0125.

**Required implementation:**

- Compare the module directive with CI, Dockerfiles, bootstrap scripts, and the
  supported developer toolchain.
- Keep 1.26.6 only when those sources establish it as the repository target.
- Separate an intentional toolchain update from tokenizer dependency changes.

**Acceptance criteria:**

- All declared build environments agree on a supported Go version policy.
- `go mod tidy` produces no unexplained dependency churn.
- Tokenizer dependencies are committed with the feature.
- Any toolchain-only change has independent validation and commit history.

**Closure evidence:** Repository history establishes Go 1.26.5 as the
intentional module target in commit `944bd689`. CI resolves its exact version
from `go.mod`, and the supported local toolchain reports Go 1.26.5. No 1.26.6
change exists in the closing worktree or reachable `go.mod` history, so RP-010
retains 1.26.5 and rejects the unattributed patch-version premise. The Docker
builder is pinned from the floating 1.26 patch line to
`golang:1.26.5-alpine` so all supported build paths use the same patch version.

The tokenizer production import and JSON Schema test imports are now direct
module requirements, separate from the toolchain decision. `go mod tidy -diff`
produces no changes. Focused dependency-consuming packages pass, the uncached
full repository suite passes on Go 1.26.5, and the pinned Docker builder
successfully compiles every shipped Go binary.

## RP-011: Traceability, Privacy, and Final Quality Gate

**Observation:** Existing focused and broad tests are green, but no single
matrix proves every ADR-0012 clause across execution, persistence, schema,
replay, privacy, and production wiring.

**Required implementation:**

- Map every normative ADR-0012 requirement to code and executable evidence.
- Verify both controller-manager wiring paths directly.
- Scan all serialized and emitted surfaces for prohibited content.
- Record focused coverage and package coverage before and after remediation.
- Run broad, race-sensitive, schema, Helm, PostgreSQL, and live Kubernetes
  gates applicable to the changed behavior.

**Acceptance criteria:**

- The traceability matrix has no requirement without implementation and test
  evidence.
- Every changed branch has a focused test or documented unreachable rationale.
- Package coverage does not regress from the recorded pre-change baseline.
- Full Go tests and acceptance tests pass without relying on cached results.
- The final diff contains no unrelated files, generated drift, secrets, raw
  canary content, or unresolved review finding.

**Closure evidence:** The final review mapped every normative ADR-0012 clause
to its owning implementation and focused executable evidence. Both production
assembly paths register retention through tested configuration boundaries.
`TestCanaryReservationsInProtectedOperationPhasesSurviveReaper` directly
proves that the generic reaper preserves reservations in `CanaryClaimed`,
`ProviderStarted`, and `ProviderCompleted`. Helm regression covers default,
override, and disabled retention values for both controller deployments.

The privacy review covered terminal results, operation records, tombstones,
usage events, audit JSON, status conversion, and span attributes. These
surfaces contain bounded metadata, digests, counts, resource references, and
correlation identifiers without raw tasks, expected output, Agent
instructions, credentials, unrestricted arguments, provider output, or
provider error text. ADR-0012 permits an optional redacted operator preview;
the implementation intentionally emits no preview, which provides the
strictest allowed disclosure posture. Canonical audit bytes are immutable on
duplicate IDs, conflicting bytes are rejected, and usage is classified as
`reasoning-probe` with explicit estimated state.

All final gates passed from uncached processes. The serialized race-enabled
repository suite passed in 16 minutes 59 seconds. The full coverage suite and
repository checker reported 84.3% authored statement coverage against the 80%
minimum, matching the pre-closure 84.3% baseline. The uncached full suite
passed in 1 minute 26 seconds. Tagged integration, tagged Kubernetes API
integration, Helm lint, all 226 Helm assertions, and server-side CRD dry-run
also passed. The corrective live run `rp011-final-20260831-d264` passed every
recorded assertion with `failure: null`, produced 20 durable timeline entries,
and cleaned its namespace. An independent scan checked 75 namespaced,
list-capable resource types with zero list errors and zero survivors.

### RP-011 ADR Clause Matrix

| ADR-0012 clause | Implementation | Executable evidence |
|---|---|---|
| Immutable binding and CPI isolation | `internal/controller/probe_reconciler.go`; `internal/controller/cpi_canary_executor.go`; `internal/modelgateway/core.go` | Binding, dependency-drift, request-digest, CPI request isolation, Tool-request, and production-session non-mutation tests in `probe_reconciler_test.go` and `cpi_canary_executor_test.go` |
| Canonical scheduling and one in-flight slot | `internal/controller/probe_reconciler.go` | Canonical-slot, same-slot join, newer-slot skip, downtime coalescing, older-slot completion, concurrent reconciler, and race tests |
| Immutable claim and provider replay fence | `internal/store/postgres/store.go`; `internal/controller/cpi_canary_executor.go` | Claim conflict, one-provider-call concurrency, provider-started uncertainty, terminal replay, changed-digest rejection, and PostgreSQL integration tests |
| Durable pre-call and post-call finalization | `internal/controller/cpi_canary_executor.go`; `internal/store/postgres/store.go` | Authorization, budget, pre-call timeout, post-reservation crash, every finalization crash window, quota disposition, usage, audit, and replay tests |
| Protected reservations and conservative accounting | `internal/store/postgres/store.go`; `internal/controller/cpi_canary_executor.go` | `TestCanaryReservationsInProtectedOperationPhasesSurviveReaper`; ambiguous execution ceiling settlement; immutable disposition; reported and estimated usage tests |
| Permanent tombstone retention | `internal/store/postgres/store.go`; `internal/controller/canary_operation_retention.go`; both controller entry points; Helm values and templates | Compaction, oversized rollback, permanent replay, retention cutoff/retry/default/disable, both assembly-path tests, migration parity, and 226 Helm assertions |
| Lifecycle, input, and output bounds | `internal/cpi/tokenizer`; `internal/controller/cpi_canary_executor.go` | Exact tokenization, input ceiling, configured lower output limit, lifecycle timeout, output limit, invalid UTF-8, and ambiguous timeout tests |
| Exact outcome and closed failure model | `api/v1alpha1/probe.go`; `internal/controller/cpi_canary_executor.go` | Exact byte equality, mismatch, precedence, all eleven failure reasons, malformed terminal-state rejection, and JSON closed-set tests |
| API, conversion, and CRD parity | `api/v1alpha1/probe.go`; `api/k8s/v1alpha1/agent_types.go`; `internal/convert/agent.go`; generated deepcopy and CRD | Round-trip, pointer ownership, malformed-state, marker parity, tagged Kubernetes API integration, live 17-resource parity, and server-side CRD dry-run |
| Metadata-only evidence and privacy | `internal/controller/cpi_canary_executor.go`; `internal/observability/spans.go`; operation, usage, and audit stores | Terminal-payload privacy, provider-error exclusion, bounded schema, no extra replay span, optional preview omission, cross-store persistence, and manual serialized-surface review |
| Versioned immutable audit and trace correlation | `reasoningProbeAuditEventV1`; deterministic outbox construction; audit stores | Audit schema, canonical ID, identical duplicate acceptance, conflicting-byte rejection, acknowledgement ordering, trace/span correlation, and exporter-failure isolation tests |
| Deterministic fixtures and production wiring | In-process fake executor/provider fixtures; `cmd/kognition-controller-manager`; `cmd/kognition-k8s-controller` | TC-0098/TC-0099, focused assembly tests, tagged integration, Helm delivery checks, full race suite, and live trustworthiness acceptance |

## Execution Order

Work follows this dependency order:

1. RP-001, closed failure reasons.
2. RP-002, lifecycle timeout.
3. RP-003, durable pre-call finalization.
4. RP-004, scheduling conflict semantics.
5. RP-005, canary span.
6. RP-006, usage classification.
7. RP-007, tombstones and retention.
8. RP-008, schema parity and generation.
9. RP-009, strict-mode E2E hardening.
10. RP-010, toolchain intent.
11. RP-011, final traceability and quality gate.

After RP-011 closes, T-0125 receives broad validation and its implementation
commit. T-0126 then receives its focused outcome-evaluation closure and commit.
T-0127 follows with evidence, usage, and audit closure. `BACKLOG.md` is updated
only after all required gates for the corresponding task pass.

## Validation Gates

### Per-Finding Gate

Run the narrowest tests that can falsify the finding first. Record the exact
command and uncached result in the evidence table. Include race or PostgreSQL
variants when the finding changes concurrency or durable behavior.

### T-0125 Gate

The minimum gate is:

```powershell
go test ./internal/cpi/tokenizer -count=1 -cover
go test ./internal/controller -run 'Canary|ReasoningProbe' -count=1 -cover
go test ./internal/store/... -run 'Canary|Reservation|Cost' -count=1 -cover
go test ./internal/convert ./api/v1alpha1 ./api/k8s/v1alpha1 -count=1
go test ./cmd/kognition-controller-manager ./cmd/kognition-k8s-controller -count=1
go test ./... -count=1
```

Run targeted race tests on supported packages. Run PostgreSQL integration,
generated schema, Helm rendering, and Kubernetes parity commands using the
repository's documented tooling. Record any environment prerequisite or skip;
an unexplained skip does not pass the gate.

### T-0126 Gate

Run table tests for exact byte equality, invalid UTF-8, output limits, Tool
requests, absent or malformed usage, nil responses, ambiguous provider errors,
and mismatch degradation thresholds. Verify precedence follows ADR-0012.

### T-0127 Gate

Run audit schema, immutable duplicate, usage classification, conservative
accounting, redaction, bounded evidence, trace correlation, and cross-store
parity tests. Search persisted and emitted payloads for prohibited content.

### Final Live Gate

Run the trustworthiness primary path with a unique run ID against the selected
Kubernetes context. Record report assertions and independently verify that the
namespace and all run-labeled resources were removed.

## Commit Plan

| Order | Commit intent | Required evidence |
|---|---|---|
| 1 | `feat(reasoning-probe): add replay-safe canary execution (T-0125)` | RP-001 through RP-011 applicable to T-0125 and the T-0125 gate |
| 2 | `feat(reasoning-probe): evaluate bounded canary outcomes (T-0126)` | T-0126 focused and regression gates |
| 3 | `feat(reasoning-probe): emit correlated canary evidence (T-0127)` | T-0127 focused, privacy, and regression gates |
| 4 | `docs(backlog): close reasoning probe tasks` | Final traceability review and exact backlog hunks |

If a finding needs an independently reviewable prerequisite commit, that
commit must compile, pass its focused gate, avoid claiming task completion,
and be recorded below. Shared files may require hunk-level staging to preserve
the semantic task order.

## Evidence Log

Add one row for every finding transition and validation gate. Do not replace
failed evidence; append the corrective run so the history remains visible.

| Date | Finding or gate | State | Command or review | Result | Commit |
|---|---|---|---|---|---|
| 2026-08-28 | Ledger creation | Active | ADR-0012 and worktree review | Eleven findings recorded; implementation not started from this ledger | Pending |
| 2026-08-28 | RP-001 API model | Passed | `go test ./api/v1alpha1 -count=1 -cover` | Package passed at 98.4%; failure-reason conversion methods covered at 100% | `756a4d1d` |
| 2026-08-28 | RP-001 controller behavior | Passed | `go test ./internal/controller -run 'Canary\|ReasoningProbe' -count=1 -cover` | Typed mapping, response precedence, status normalization, and replay tests passed; package coverage 84.4% | `756a4d1d` |
| 2026-08-28 | RP-001 privacy | Passed | `go test ./internal/controller -run '^TestCPICanaryExecutor_TerminalPayloadsExcludeSensitiveContent$' -count=1` | Passed; result, usage, and audit payloads excluded task, expected output, instructions, credential, and provider output markers | `756a4d1d` |
| 2026-08-28 | RP-001 repository regression | Passed | `go test ./... -count=1` | All Go packages passed with exit code 0 | `756a4d1d` |
| 2026-08-28 | RP-001 ADR and privacy review | Passed | Manual review against ADR-0012 outcome evaluation and persisted payload schemas | Exact eleven-value taxonomy and canonical wire strings; no error-text classification or prohibited raw payload persistence | `756a4d1d` |
| 2026-08-30 | RP-002 partial implementation | In Progress | `go test ./internal/controller -run TestCPICanaryExecutor_OwnsConfiguredLifecycleTimeout -count=1` | Passed; executor propagated the configured lifecycle deadline and retry completed the fenced uncertain result without another provider call | `a7a41444` |
| 2026-08-30 | RP-003 reservation-denial path | Open | `go test ./internal/controller -run TestCPICanaryExecutor_DurablyFinalizesReservationDenial -count=1` | Passed; reservation denial completed durably with one audit event, no provider call, no usage, and an identical side-effect-free replay | `9ac7993f` |
| 2026-08-30 | RP-003 reservation-presence persistence | Open | `go test ./internal/store/postgres -run TestCanaryOperationPersistsPreCallFinalizationWithoutUsage -count=1` | Passed; PostgreSQL preserved reservation presence and absence of usage across reload | `673f8ba7` |
| 2026-08-30 | RP-003 authorization-denial path | Open | `go test ./internal/controller -run TestCPICanaryExecutor_DurablyFinalizesAuthorizationDenial -count=1` | Passed; authorization denial completed durably with one audit event, no reservation, provider call, or usage, and an identical replay | `e094a857` |
| 2026-08-30 | RP-002 pre-call timeout path | In Progress | `go test ./internal/controller -run TestCPICanaryExecutor_DurablyFinalizesPreCallTimeout -count=1` | Passed; reservation-stage deadline completed durably as `TimedOut` with no reservation disposition, provider call, or usage, and an identical replay | `223af0e0` |
| 2026-08-30 | RP-002 finalization timeout and resume | Closed | `go test ./internal/controller -run TestCPICanaryExecutor_ResumesFinalizationAfterLifecycleTimeout -count=1` | Passed; settlement expired under the original lifecycle context, durable finalization intent survived, and a fresh bounded retry completed all effects without another provider call | `b6b13728` |
| 2026-08-30 | RP-003 pre-call crash recovery | Closed | `go test ./internal/controller -run 'TestCPICanaryExecutor_(AnotherExecutorReleasesReservationAfterPreCallCrash|RecoversPreCallFinalizationCrashWindows|DurablyFinalizesAuthorizationDenial)' -count=1` | Passed; another executor recovered a post-reservation `CanaryClaimed` crash, and every durable finalization boundary converged to one result, one release effect, one audit event, no usage, and no CPI call | `dbf2a3c9` |
| 2026-08-30 | RP-003 protected reservation reaping | Closed | `go test ./internal/store/postgres -run TestCanaryReservationsInProtectedOperationPhasesSurviveReaper -count=1` | Passed; expired canary reservations in `CanaryClaimed`, `ProviderStarted`, and `ProviderCompleted` survived generic reaping and remained releasable | `dbf2a3c9` |
| 2026-08-30 | RP-003 affected packages and coverage | Closed | `go test ./internal/controller ./internal/modelgateway ./internal/store/postgres -count=1 -cover` | Passed; coverage was 84.7%, 74.1%, and 78.9%, respectively | `dbf2a3c9` |
| 2026-08-30 | RP-004 scheduling conflict semantics | Closed | `go test ./internal/controller -run 'TestProbeReconciler_(ReasoningProbeRunsOncePerCanonicalSlot\|ReasoningProbeConflictDoesNotInvokeProvider\|ReasoningProbeConflictJoinsSameSlotWinner\|ReasoningProbeConflictSkipsNewerSlot\|ReasoningProbeFinishesOlderSlotBeforeAcquiringLatest\|ConcurrentReasoningProbesShareOneSlotAndProviderCall)\|TestCPICanaryExecutor_(RejectsDependencyDriftBeforeSideEffects\|RejectsRequestDigestTamperingBeforeSideEffects)' -count=1` | Passed; conflict reload joins the exact persisted same-slot identity, skips a newer winner, preserves one in-flight slot, coalesces downtime, and rejects drift before side effects | `367d2dcd` |
| 2026-08-30 | RP-004 concurrent reconciler race check | Closed | `go test -race ./internal/controller -run '^TestProbeReconciler_ConcurrentReasoningProbesShareOneSlotAndProviderCall$' -count=1` | Passed; two replica-like reconcilers shared one operation identity and admitted one provider call with no data race | `367d2dcd` |
| 2026-08-30 | RP-004 affected packages and coverage | Closed | `go test ./internal/controller ./internal/modelgateway ./internal/store/postgres -count=1 -cover` | Passed; coverage was 84.7%, 74.1%, and 78.8%, respectively | `367d2dcd` |
| 2026-08-30 | RP-004 terminal replay regression | Passed | `go test ./internal/controller -run '^TestCPICanaryExecutor_ReturnsStoredResultWithoutProviderCall$' -count=1` | Passed; an identical completed-operation replay returned the stored result and preserved exactly one provider, settlement, usage, and audit effect | `b1a35b9a` |
| 2026-08-30 | Repository authored-code coverage | Passed | `bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80` | Passed at 84.4% against the 80% minimum; the raw profile total was 79.3% because it includes generated and non-authored code excluded by the repository checker | `b1a35b9a` |
| 2026-08-30 | Final live primary gate | Passed | Three sequential invocations of `pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-e2e.ps1 -DemoRunId <trust-audit-a/b/c-20260830> -KubernetesContext docker-desktop` | All three consecutive runs passed all 11 assertions with `failure: null`; independent `kubectl` checks confirmed all three namespaces absent and no matching resources remained | `b1a35b9a` |
| 2026-08-30 | Fallback and storyboard technical traversal | Passed | `pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-fallback-e2e.ps1`; `pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-storyboard-e2e.ps1` | TC-0313 passed. The automated TC-0316 traversal kept fallback claims explicit, finished on Trust Focus, and cleaned up fixture processes; spoken narration was not attested and TC-0316 remained open. | `b1a35b9a` |
| 2026-08-30 | Dashboard browser acceptance | Passed | `npm run test:dashboard:browser` | All 10 Playwright tests passed in 10.9 seconds with no failures or skips | `b1a35b9a` |
| 2026-08-30 | RP-005 span, correlation, privacy, and failure isolation | Closed | `go test ./internal/controller -run 'TestCPICanaryExecutor_(EmitsOneCorrelatedMetadataOnlySpanPerClaim\|DoesNotExportProviderErrorDetails\|SpanExportFailureDoesNotBlockFinalization)$' -count=1` | Passed; one metadata-only span was emitted for the persisted claim winner, replay emitted no second span, result, usage, and audit identifiers matched, provider error details were absent, and exporter failure did not block finalization | `60b99aee` |
| 2026-08-30 | RP-005 executor regression | Passed | `go test ./internal/controller -run 'TestCPICanaryExecutor_' -count=1 -timeout 10m` | All canary executor tests passed with uncached execution | `60b99aee` |
| 2026-08-30 | RP-005 affected packages and race | Passed | `go test ./internal/controller ./internal/observability -race -count=1 -timeout 15m`; `go test ./cmd/kognition-controller-manager ./cmd/kognition-k8s-controller -count=1` | Controller and observability race tests passed; both production assembly packages passed | `60b99aee` |
| 2026-08-30 | RP-005 repository regression and coverage | Passed | `go test ./... -count=1 -timeout 20m`; `go test ./internal/controller ./internal/observability -count=1 -cover`; `go test ./... -count=1 -coverpkg=github.com/kognition-ai/kognition/... -coverprofile=coverage.out -timeout 20m`; `bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80` | All Go packages passed; controller coverage was 84.8%, observability coverage was 84.5%, and repository-authored coverage passed at 84.3% against the 80% minimum | `60b99aee` |
| 2026-08-30 | RP-006 controller classification | Closed | `go test ./internal/controller -run 'TestCPICanaryExecutor_(EmitsOneCorrelatedMetadataOnlySpanPerClaim\|DoesNotReplayProviderStarted\|DurablyFinalizesReservationDenial\|DurablyFinalizesAuthorizationDenial\|DurablyFinalizesPreCallTimeout)$' -count=1` | Passed; reported usage was classified as non-estimated `reasoning-probe`, ambiguous execution used estimated ceiling usage, and pre-call terminal paths emitted no usage | `007629ac` |
| 2026-08-30 | RP-006 persistence and compatibility | Passed | `go test ./internal/k8s/cost -run 'TestConfigMapStoreAppliesUsageEventAndRepairsProjectionIdempotently$' -count=1`; `go test ./internal/store/postgres -run 'TestApplyUsageEventAtomicallyProjectsCostAndModelTierOnce$' -count=1`; `go test ./internal/observability -run '^TestCostRecordUsageEstimatedJSONCompatibility$' -count=1` | Passed; both production stores round-tripped estimated `reasoning-probe` usage, PostgreSQL retained model-inference aggregation, and false estimated state preserved the legacy JSON fingerprint shape | `007629ac` |
| 2026-08-30 | RP-006 immutable classification and migration | Passed | `go test ./internal/k8s/cost -run 'TestConfigMapStoreRejectsInvalidAndConflictingUsageEvents$' -count=1`; `go test ./internal/store/postgres -run 'TestApplyUsageEventRejectsInvalidAndConflictingEvents\|TestUsagePurposeMigrationIsIdempotentAndMatchesHelmBundle' -count=1` | Passed; unknown purposes were rejected, purpose and estimated-state changes conflicted under duplicate event IDs, and migration 017 was idempotent with matching internal and Helm content | `007629ac` |
| 2026-08-30 | RP-006 affected-package race | Passed | `go test ./internal/controller ./internal/observability ./internal/k8s/cost ./internal/store/postgres -race -count=1 -timeout 20m` | All four affected packages passed the race detector from the final source | `007629ac` |
| 2026-08-30 | RP-006 repository regression and coverage | Passed | `go test ./... -count=1 -timeout 20m`; `go test ./... -count=1 '-coverpkg=github.com/kognition-ai/kognition/...' -coverprofile=coverage.out -timeout 20m`; `bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80` | Both full repository runs exited 0; repository-authored coverage passed at 84.3% against the 80% minimum | `007629ac` |
| 2026-08-30 | RP-007 durable replay and compaction | Closed | `go test ./internal/store/postgres -run 'TestCanaryOperation(CompactionPreservesPermanentReplayFence\|CompactionSkipsIneligibleRows\|TombstoneMigrationIsIdempotentAndMatchesHelmBundle)$' -count=1 -timeout 10m` | Passed in 24.687 seconds; exact result replay, changed-digest conflict, repeated and concurrent idempotence, finalization eligibility, AgentInstance deletion survival, immutability, and migration parity were proved | `35fffc55` |
| 2026-08-30 | RP-007 oversized tombstone rollback | Passed | `TestCanaryOperationCompactionPreservesActiveRowWhenTombstoneExceedsLimit`, executed by the PostgreSQL package, race, and full-repository gates | Passed; a result exceeding the bounded tombstone payload caused compaction to fail while preserving the active operation for replay | `35fffc55` |
| 2026-08-30 | RP-007 executor replay contract | Passed | `go test ./internal/controller -run 'Test(CPICanaryExecutor_ReplaysCompactedTombstoneWithoutEffects\|CanaryOperationRetention)' -count=1` | Passed in 3.702 seconds; compacted replay returned the exact result without repeating side effects, and retention default, disabled, invalid, cutoff, and retry behavior passed | `35fffc55` |
| 2026-08-30 | RP-007 standalone assembly | Passed | `go test ./cmd/kognition-controller-manager -run 'TestNewControllerManager(RegistersCanaryRetention\|DisablesCanaryRetention)$' -count=1` | Passed in 5.900 seconds; enabled retention registered the leadership-scoped runnable and disabled retention omitted it | `35fffc55` |
| 2026-08-30 | RP-007 affected-package race | Passed | `go test -race ./internal/store/postgres ./internal/controller ./cmd/kognition-controller-manager ./cmd/kognition-k8s-controller -count=1 -timeout 10m` | All four affected packages passed the race detector; PostgreSQL completed in 55.828 seconds, controller in 12.950 seconds, standalone assembly in 8.385 seconds, and Kubernetes assembly in 2.883 seconds | `35fffc55` |
| 2026-08-30 | RP-007 integration and Helm | Passed | `.\make.ps1 integration`; `helm lint deploy/helm/kognition`; `.\deploy\helm\kognition\tests\helm-regression-tests.ps1` | Tagged integration passed in 6.637 seconds; Helm lint reported one chart linted and zero failures; chart regression passed 226 assertions with zero failures | `35fffc55` |
| 2026-08-30 | RP-007 repository regression and coverage | Passed | `go test ./... -count=1 -timeout 20m`; `go test ./... -count=1 '-coverpkg=github.com/kognition-ai/kognition/...' -coverprofile=coverage.out -timeout 20m`; `bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80` | The uncached suite passed 57 tested packages and 10 packages without tests, with zero failures; repository-authored coverage passed at 84.3% against the 80% minimum | `35fffc55` |
| 2026-08-30 | RP-007 ADR and privacy review | Passed | Manual review against ADR-0012 retention, replay, finalization, and privacy clauses | Exact terminal result bytes and trace correlation survive compaction; secondary usage and audit payloads are discarded; tombstones are bounded, immutable, permanent across instance cleanup, and consulted before claim creation | `35fffc55` |
| 2026-08-30 | RP-008 API, conversion, deepcopy, and CRD contract | Closed | `go test ./internal/convert ./api/v1alpha1 ./api/k8s/v1alpha1 -count=1`; `go test -tags=integration ./api/k8s/v1alpha1 -count=1` | Focused round trips, closed JSON values, nested anti-aliasing, and marker-to-CRD parity passed; malformed programmatic terminal values also passed a focused conversion test and race run | `c46adea4`, `219f1a3b` |
| 2026-08-30 | RP-008 Kubernetes resource parity | Passed | `go test ./api/k8s/v1alpha1 -run '^TestCanonicalKindsHaveNativeKubernetesSurface$' -count=1`; `pwsh -NoProfile -File .\e2e\kubernetes-resource-parity.ps1` | Native surface validation passed; live create, read, update, and list parity passed for all 17 resources, and namespace `kognition-parity-1788131404` plus cluster-scoped fixtures were deleted | `c46adea4` |
| 2026-08-30 | RP-008 race, Helm, and repository regression | Passed | `go test -race ./internal/convert ./api/v1alpha1 ./api/k8s/v1alpha1 -count=1`; `helm lint deploy/helm/kognition`; `.\deploy\helm\kognition\tests\helm-regression-tests.ps1`; `go test ./... -count=1 -timeout 20m` | Affected packages passed the race detector; Helm lint reported one chart and zero failures; all 226 chart assertions and the full Go suite passed | `c46adea4`, `219f1a3b` |
| 2026-08-30 | RP-008 authored coverage | Passed | `go test ./... -count=1 '-coverpkg=github.com/kognition-ai/kognition/...' -coverprofile=coverage.out -timeout 20m`; `bash .github/scripts/check-go-coverage.sh --profile coverage.out --min 80` | Repository-authored statement coverage passed at 84.3% against the 80% minimum | `c46adea4` |
| 2026-08-30 | RP-008 post-closure semantic and ownership audit | Closed | Focused internal API, Kubernetes API, conversion, CRD parity, malformed-value, and anti-alias regression tests | Both conversion directions own nested pointers; JSON and conversion boundaries reject impossible terminal states; exact enums and the CRD CEL presence invariant match the owning Go markers | `b010d5b8` |
| 2026-08-30 | RP-008 post-closure repository gates | Passed | `go test ./... -p=1 -count=1 -timeout=20m -race -cover`; `.\make.ps1 integration`; `helm lint deploy/helm/kognition`; `.\deploy\helm\kognition\tests\helm-regression-tests.ps1`; authored coverage check | Full race-enabled repository and tagged integration suites passed; Helm lint had zero failures; all 226 chart assertions passed; authored coverage was 84.3% against the 80% minimum | `b010d5b8` |
| 2026-08-30 | RP-008 post-closure Kubernetes admission and parity | Passed | `kubectl apply --dry-run=server -f deploy/helm/kognition/crds/kognition.dev_agentinstances.yaml`; `pwsh -NoProfile -File .\e2e\kubernetes-resource-parity.ps1` | Server accepted the CRD CEL rule; live parity passed for all 17 native resource kinds on `docker-desktop`; disposable resources and namespace were removed | `b010d5b8` |
| 2026-08-30 | RP-009 strict-mode EndpointSlice readiness | Closed | `pwsh -NoProfile -File .\e2e\hackathon-trustworthiness-readiness-test.ps1` | Passed all four missing, null, false, and true readiness cases by invoking helpers parsed from the production acceptance script; non-ready shapes produced the exact actionable service assertion | `4af75e75` |
| 2026-08-30 | RP-009 live acceptance and cleanup | Passed | Live `e2e/hackathon-trustworthiness-e2e.ps1` run on `docker-desktop`; independent namespace and run-label cleanup scan | The report passed all 11 assertions with no failure and successful cleanup; the namespace was absent and no labeled resources remained across 75 namespaced, list-capable API resources | `4af75e75` |
| 2026-08-30 | RP-010 toolchain and module intent | Closed | `go version`; repository history and source review; `go mod tidy -diff`; `docker build --check -f Dockerfile .` | Go 1.26.5 is the intentional module, CI, local, and Docker target; no 1.26.6 change exists; module metadata is canonical; Docker static validation passed without warnings | `c0b2c6ed` |
| 2026-08-30 | RP-010 build and repository regression | Passed | `go test ./internal/cpi/tokenizer ./internal/memory/policygateway ./internal/observability ./internal/toolgateway -count=1`; `go test ./... -count=1 -timeout 20m`; `docker build --target builder -f Dockerfile .` | Four dependency-consuming packages passed; the uncached repository suite passed in 2 minutes 31 seconds; the pinned Go 1.26.5 builder compiled every shipped Go binary in 3 minutes 23 seconds | `c0b2c6ed` |
| 2026-08-31 | RP-011 Kubernetes retention assembly | Closed | `go test ./cmd/kognition-k8s-controller -run '^TestConfigureCanaryOperationRetention$' -count=1`; existing standalone assembly tests | Enabled and disabled Kubernetes retention, invalid configuration, registration failure, and standalone registration paths passed | `680e5848` |
| 2026-08-31 | RP-011 integration, schema, and Helm | Passed | `.\make.ps1 integration`; `go test -tags=integration ./api/k8s/v1alpha1 -count=1`; `helm lint deploy/helm/kognition`; Helm regression script; server-side AgentInstance CRD dry-run | Tagged integration and API integration passed; Helm lint had zero failures; all 226 chart assertions passed; the API server accepted the CRD | `680e5848` |
| 2026-08-31 | RP-011 first live attempt | Failed and cleaned | `hackathon-trustworthiness-e2e.ps1 -DemoRunId rp011-final-20260831-a831 -KubernetesContext docker-desktop`; independent cleanup | The detached attempt produced no valid completion report and initially left its namespace; corrective namespace deletion succeeded, and a 75-resource scan found zero survivors | `680e5848` |
| 2026-08-31 | RP-011 interrupted live attempt | Failed and cleaned | `hackathon-trustworthiness-e2e.ps1 -DemoRunId rp011-final-20260831-b942 -KubernetesContext docker-desktop` | The runner interrupted the parent during the Memory phase; the report correctly remained failed with missing downstream assertions, while harness cleanup passed. A focused fresh Memory Boundary run then passed with exit code 0 | `680e5848` |
| 2026-08-31 | RP-011 corrective live acceptance | Passed | `hackathon-trustworthiness-e2e.ps1 -DemoRunId rp011-final-20260831-d264 -KubernetesContext docker-desktop`; independent all-resource cleanup scan | Exit code 0 after 5 minutes 33 seconds; every recorded assertion passed, `failure` was null, 20 timeline entries were collected, cleanup passed, and 75 resource types contained zero survivors or list errors | `680e5848` |
| 2026-08-31 | RP-011 final race and coverage | Passed | `go test ./... -p=1 -count=1 -timeout=20m -race -cover`; full coverprofile run; authored coverage checker | Serialized race suite passed in 16 minutes 59 seconds; coverage generation passed in 1 minute 51 seconds; authored coverage remained 84.3% against the 80% minimum | `680e5848` |
| 2026-08-31 | RP-011 final uncached regression | Closed | `go test ./... -count=1 -timeout 20m`; clause-level ADR and serialized-surface review | Full suite passed in 1 minute 26 seconds; no ADR clause, privacy leak, generated drift, secret, unrelated diff, or unresolved finding remained | `680e5848` |

## Traceability Matrix

Populate code and test references as findings close.

| ADR area | Finding | Implementation | Focused tests | Broader evidence |
|---|---|---|---|---|
| Closed failure taxonomy and precedence | RP-001 | `api/v1alpha1/probe.go`; `internal/controller/cpi_canary_executor.go`; `internal/controller/probe_reconciler.go` | `api/v1alpha1/probe_result_test.go`; `internal/controller/cpi_canary_executor_test.go`; `internal/controller/probe_reconciler_test.go` | API 98.4%; controller 84.4%; `go test ./... -count=1` passed |
| Full lifecycle timeout | RP-002 | `internal/controller/cpi_canary_executor.go` | `TestCPICanaryExecutor_OwnsConfiguredLifecycleTimeout`; `TestCPICanaryExecutor_DurablyFinalizesPreCallTimeout`; `TestCPICanaryExecutor_ResumesFinalizationAfterLifecycleTimeout` | Provider ambiguity, durable pre-call timeout, finalization expiry, fresh-context resume, maximum settlement, and at-most-once CPI proof pass |
| Durable pre-call finalization | RP-003 | `internal/controller/cpi_canary_executor.go`; `internal/modelgateway/core.go`; `internal/store/postgres/store.go` | `TestCPICanaryExecutor_DurablyFinalizesReservationDenial`; `TestCPICanaryExecutor_DurablyFinalizesAuthorizationDenial`; `TestCPICanaryExecutor_DurablyFinalizesPreCallTimeout`; `TestCPICanaryExecutor_AnotherExecutorReleasesReservationAfterPreCallCrash`; `TestCPICanaryExecutor_RecoversPreCallFinalizationCrashWindows`; `TestCanaryOperationPersistsPreCallFinalizationWithoutUsage`; `TestCanaryReservationsInProtectedOperationPhasesSurviveReaper` | All named failures, post-reservation recovery by another executor, durable-step crash recovery, exactly-once effects, provider-free replay, and protected-phase reaper behavior pass |
| Canonical scheduling and concurrency | RP-004 | `internal/controller/probe_reconciler.go` | `TestProbeReconciler_ReasoningProbeRunsOncePerCanonicalSlot`; `TestProbeReconciler_ReasoningProbeConflictDoesNotInvokeProvider`; `TestProbeReconciler_ReasoningProbeConflictJoinsSameSlotWinner`; `TestProbeReconciler_ReasoningProbeConflictSkipsNewerSlot`; `TestProbeReconciler_ReasoningProbeFinishesOlderSlotBeforeAcquiringLatest`; `TestProbeReconciler_ConcurrentReasoningProbesShareOneSlotAndProviderCall`; `TestCPICanaryExecutor_RejectsDependencyDriftBeforeSideEffects`; `TestCPICanaryExecutor_RejectsRequestDigestTamperingBeforeSideEffects`; `TestCPICanaryExecutor_ReturnsStoredResultWithoutProviderCall` | Same/newer conflict classification, immutable persisted identity, one in-flight slot, downtime coalescing, provider-free terminal replay, at-most-once CPI, race, and affected-package checks pass |
| Canary span and correlation | RP-005 | `internal/controller/cpi_canary_executor.go`; `internal/observability/spans.go`; `cmd/kognition-controller-manager/main.go`; `cmd/kognition-k8s-controller/main.go` | `TestCPICanaryExecutor_EmitsOneCorrelatedMetadataOnlySpanPerClaim`; `TestCPICanaryExecutor_DoesNotExportProviderErrorDetails`; `TestCPICanaryExecutor_SpanExportFailureDoesNotBlockFinalization` | One span per persisted claim winner; bounded attributes; result, usage, audit, and current-operation replay correlation; no extra replay span; privacy, exporter-failure, race, assembly, full-repository, and 84.3% authored-coverage gates pass. Tombstone propagation and compacted replay remain RP-007 evidence. |
| Quota and usage classification | RP-006 | `internal/observability/cost.go`; `internal/controller/cpi_canary_executor.go`; `internal/store/postgres/cost.go`; `internal/store/postgres/migrations/017_usage_purpose.sql`; `deploy/helm/kognition/migrations/017_usage_purpose.sql`; `internal/k8s/cost/configmap_store.go` | `TestCPICanaryExecutor_EmitsOneCorrelatedMetadataOnlySpanPerClaim`; `TestCPICanaryExecutor_DoesNotReplayProviderStarted`; pre-call denial and timeout tests; `TestCostRecordUsageEstimatedJSONCompatibility`; PostgreSQL and Kubernetes apply, validation, immutable-conflict, and migration parity tests | Reported and estimated paths, no pre-call usage, physical cross-store persistence, bounded validation, immutable duplicate identity, legacy fingerprint compatibility, affected-package race, full repository regression, and 84.3% authored-coverage gates pass. No deployed functional harness directly exposes this internal classification. |
| Retention and permanent replay fence | RP-007 | `internal/store/interface.go`; `internal/store/postgres/store.go`; `internal/store/postgres/migrations/018_canary_operation_tombstones.sql`; `deploy/helm/kognition/migrations/018_canary_operation_tombstones.sql`; `internal/controller/canary_operation_retention.go`; `internal/controller/manager.go`; `cmd/kognition-controller-manager/main.go`; `cmd/kognition-k8s-controller/main.go`; Helm values, deployment templates, and regression tests; `docs/bootstrapping.md` | `TestCanaryOperationCompactionPreservesPermanentReplayFence`; `TestCanaryOperationCompactionSkipsIneligibleRows`; `TestCanaryOperationCompactionPreservesActiveRowWhenTombstoneExceedsLimit`; `TestCanaryOperationTombstoneMigrationIsIdempotentAndMatchesHelmBundle`; `TestCPICanaryExecutor_ReplaysCompactedTombstoneWithoutEffects`; retention configuration, cutoff, and retry tests; standalone assembly registration tests | PostgreSQL, controller, and assembly focused tests; affected-package race; tagged integration; Helm lint; 226 chart regression assertions; 67-package repository suite; 84.3% authored coverage; manual ADR and privacy review all pass |
| API and generated schema parity | RP-008 | `api/v1alpha1/probe.go`; `api/k8s/v1alpha1/agent_types.go`; `internal/convert/agent.go`; `api/k8s/v1alpha1/zz_generated.deepcopy.go`; `deploy/helm/kognition/crds/kognition.dev_agentinstances.yaml` | `TestAgentProbesRoundTrip`; `TestAgentInstanceProbeStatusRoundTrip`; `TestAgentInstanceProbeStatusRejectsMalformedTerminalResult`; `TestAgentInstanceProbeStatusRejectsMalformedInternalTerminalResult`; `TestAgentInstanceProbeStatusConversionDoesNotAliasCanaryPointers`; `TestAgentInstanceDeepCopy`; `TestCanaryOutcomeJSONClosedSet`; `TestCanaryFailureReasonJSONClosedSet`; `TestReasoningCanaryResultRejectsInvalidOutcomeReasonCombination`; `TestReasoningProbeCRDParity` | Focused and tagged API tests; affected-package race; server-side CRD dry-run; 17-resource native and live parity; Helm lint; 226 chart assertions; full race-enabled repository suite; tagged integration; 84.3% authored coverage all pass |
| Acceptance harness robustness | RP-009 | `e2e/hackathon-trustworthiness-e2e.ps1` | `e2e/hackathon-trustworthiness-readiness-test.ps1`; four strict-mode readiness shapes | Live Docker Desktop acceptance passed all 11 assertions; cleanup passed; independent scan found no namespace or labeled resources across 75 namespaced API resources |
| Reproducible toolchain | RP-010 | `go.mod`; `Dockerfile`; `.github/workflows/ci.yml` consumes `go.mod` | `go mod tidy -diff`; four dependency-consuming packages | Go 1.26.5 retained and Docker pinned to it; uncached full repository suite and complete builder target passed |
| Privacy and production quality | RP-011 | Executor, operation/tombstone stores, audit outbox, usage stores, spans, both controller entry points, Helm delivery, and the internal/Kubernetes/schema conversion chain | Clause matrix above; privacy, immutable audit conflict, usage classification, protected reservation, both retention assembly, focused outcome, conversion, schema, and deterministic fixture tests | Full serialized race, coverage, uncached regression, integration, API integration, Helm lint and 226 assertions, server-side CRD dry-run, corrective live acceptance, and independent 75-resource cleanup scan passed |

## Completion Record

- **Completed:** 2026-08-31
- **Implementation commits:** RP-001 through RP-010 evidence records the
  finding-specific commits; final Kubernetes retention assembly is `680e5848`.
  The commit containing this record closes the ledger and backlog.
- **Coverage:** Authored statement coverage is 84.3% against an 84.3%
  pre-closure baseline and an 80% required minimum.
- **Broad Go validation:** Serialized race-enabled and uncached full repository
  suites passed. Focused PostgreSQL reservation, finalization, audit, usage,
  compaction, tombstone, migration, and replay tests passed.
- **Delivery validation:** Tagged integration, tagged Kubernetes API
  integration, Helm lint, all 226 chart assertions, and server-side CRD
  validation passed.
- **Live acceptance:** Report
  `test-results/hackathon-trustworthiness-rp011-final-20260831-d264.json`
  records `passed: true`, `failure: null`, all assertions passing, 20 durable
  timeline entries, and successful cleanup. Independent verification found no
  namespace or labeled survivor across 75 namespaced resource types.
- **Final review:** The closure diff is limited to this ledger and the exact
  T-0125 through T-0127 backlog closure. Generated coverage, reports,
  credentials, runtime output, and unrelated worktree content are excluded.