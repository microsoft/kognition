# SG-04 Independent Review

- Date: 2026-09-24.
- Mode: Code and recovery-contract review.
- Original verdict: FAILED; SG-05 blocked pending remediation.
- Scope: Activation approval, journal, probes, compensation, recovery, command and local tests.
- Findings: One high-severity integrity issue and one medium-severity recovery issue.

## Findings

### SG04-R01: Uncertain Grant Creation Can Outlive Compensation

- Severity: High.
- Category: Authorization-state integrity.
- Evidence: [grant creation](../../../internal/k8s/activation/transaction.go#L114), [NotFound cleanup](../../../internal/k8s/activation/transaction.go#L336), [inactive stage selection](../../../internal/k8s/activation/transaction.go#L74).

A transport timeout does not establish that the API server has stopped processing
a grant Create. Activation returns the error and compensates. Compensation treats
NotFound as a completed deletion and accepts the first successful drain check.
The journal can therefore become `failed-inactive` while an earlier Create remains
unresolved. If that request subsequently persists, a controller grant remains after
the transaction reported revoked grants. The external writer freeze does not fence
the transaction's own in-flight request.

The [lost-response test](../../../internal/traceability/activation_transaction_integration_test.go#L457)
persists the binding before returning the error. Cleanup immediately sees that
object; this does not exercise late persistence after NotFound.

Required remediation: durably track uncertain grant creation and keep the outcome
`recovery-required` until outstanding writes are resolved or fenced. Recovery must
reconcile exact transaction-owned grants without deleting foreign replacements.
Add a deterministic delayed-Create regression that initially returns NotFound,
then persists after the initial cleanup/drain check. It must never produce a
terminal inactive success while that write remains unresolved. An arbitrary sleep
is not proof of request completion.

### SG04-R02: Interrupted Probes Are Outside Durable Recovery

- Severity: Medium; blocks SG-04 recovery acceptance.
- Category: Recovery completeness and least-privilege cleanup.
- Evidence: [in-memory probe inventory](../../../internal/k8s/activation/behavior.go#L68), [random ownership marker](../../../internal/k8s/activation/probe_create.go#L22), [audit schema](../../../internal/k8s/activation/journal.go#L17), [recovery cleanup](../../../internal/k8s/activation/recovery.go#L122).

Behavioral verification creates temporary policies, policy bindings, fixture
objects, Roles and RoleBindings. Their names, UIDs and ownership markers are held
in the verifier's local slice, without a durable association to the activation
journal. Process death bypasses the deferred cleanup. `Recover` only reconstructs
the permanent candidate bindings and cannot remove those temporary objects.

A remaining probe RoleBinding or suspended workload Job makes drain verification
fail indefinitely across retries until manual cleanup. Earlier interruption with
only protection resources can instead report `recovered-inactive` while leaving
those resources behind. The documented recovery command cannot complete this
portion of its own transaction safely.

The [interrupted recovery fixture](../../../internal/traceability/activation_transaction_integration_test.go#L534)
changes the journal stage to `behavior-verification` without creating interrupted
probe resources. It does not cover this state.

Required remediation: record probe intent and transaction ownership durably before
creation, reconcile returned or uncertain UIDs, and recover probes in the existing
grant/fixture/protection cleanup order. Preserve foreign/replaced objects. Add
interruption cases at each probe stage with live resources and unavailable local
cleanup, including lost Create responses. A fresh executor must complete cleanup
or report precisely owned unresolved resources without claiming completion.

## Validation

- Editor-selected activation, journal, authority and command tests: 29 passed.
- `go test -race -count=1 -timeout=3m ./internal/k8s/activation ./tools/check-admission-activation`: passed; 9.010s and 2.170s.
- `go test -race -tags=integration -count=1 -timeout=5m ./internal/traceability -run 'TestActivation|TestAdmissionBootstrap|TestAdmissionGuard'`: passed; 176.694s.
- Findings above are established by control-flow review and missing interleaving coverage; no process-kill or delayed-persistence reproducer was executed in this review.
- Earlier scoped lint and vulnerability checks passed. Two previously dispositioned broader SG-03 fixture lint findings remain tracked under SG-11; those checks were not rerun here.
- Synthetic approval/readiness fixtures do not satisfy SG-07, SG-08 or SG-09 production acceptance.

## Outcome

Approval identity, immutable consumption claims, original-UID checks and preservation
of foreign grants have focused coverage. Passing tests do not close the two
recovery gaps above. Resolve both findings and add their regression coverage before
reconsidering the SG-05 transition.

Only this separate report was added. Implementation files, the existing feature
security review, index, archives and unrelated user changes were preserved. No
real activation, cloud operation, shared-cluster mutation, commit or push occurred.

## Remediation Review (2026-09-24)

Both findings are resolved for the local SG-04 implementation. The original failed
review above remains the historical record. SG-05 implementation can proceed after
user review; it has not started. Production activation remains blocked by SG-07,
SG-08 and SG-09 and the remaining installation/final-candidate gates.

| Finding | Resolution | Regression evidence |
| --- | --- | --- |
| SG04-R01 | Persist resource intent before Create; unresolved absence blocks compensation and recovery success. Observe exact ownership and persist UID before Delete. Never retry the same Create. | Delayed actual API persistence after failed activation and failed recovery; later exact-owned grant removed by fresh recovery. Journal-write failure, lost update response and collision tests. |
| SG04-R02 | Persist all probe identities under journal ownership; fresh recovery reconstructs and removes grants, fixtures and protections in order. Preserve foreign/replaced resources and retain protections on unresolved cleanup. | Live API interruption at protection, protection binding, fixture, Role and RoleBinding stages with unavailable local cleanup/UID persistence. Fresh clients remove recorded resources. Foreign/replaced and pending-absent protection tests. |

The [resource journal](../../../internal/k8s/activation/resource_journal.go) implements
the shared protocol. [Legacy journal checks](../../../internal/k8s/activation/recovery.go)
reject completion/retirement without version 1; compensation cannot certify those
journals inactive. The [contract](../../features/knowledge-provider-execution/activation-transaction-contract.md)
documents indefinite pending-Create uncertainty, including a crash before send.
Stopping an executor is necessary but does not prove an outstanding API write ended.

### Current Validation

- Full activation and command race suites: passed (16.402s / 7.986s).
- Selected activation/bootstrap/guard API and chart race suite: passed (194.957s), with local Kubernetes 1.30.3 envtest assets.
- Transaction API race fixture after interruption matcher lint repair: passed (158.544s).
- Scoped activation/command lint: zero issues, with unlimited issue reporting and no new suppression.
- Integration lint rerun: only the two existing SG-11 findings described in the [backlog](../features/knowledge-provider-execution/security-gate-backlog.md) remain. Broad traceability lint is not green; neither finding is in the modified transaction fixture.
- Scoped `govulncheck`: no vulnerabilities found.
- Three updated documents pass Markdown lint with the established `MD013` allowance; all 78 relative file links resolve. Explicit editor diagnostics and scoped tracked-diff whitespace checks are clear.
- Governance archive SHA256 remains `541f9336b2aaad1d1d834d068ca7c2b49e656917d17c6b411bce7cfa063bc16f`.
- No additional blocking issue found in the focused recovery-protocol review. This is local remediation revalidation, not a new independent-agent review or full-candidate clearance.

Interruption uses deterministic injected panics with cleanup denied; it is not an
OS process-kill acceptance test. API persistence and RBAC are real; approval and
readiness are synthetic. No production producer, enforcing-CNI/full stdio or trust
ADR acceptance is inferred from these tests. No staging, commit, push, archive
refresh, shared-cluster mutation or cloud operation was performed.
