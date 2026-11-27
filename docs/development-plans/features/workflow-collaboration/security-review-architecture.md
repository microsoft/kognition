# Workflow Collaboration Readiness And Architectural Security Review

> **Historical.** This review covers the receiver and task-plane design, which
> was removed in favor of AgentJob-based member turns. The shipped design needs a
> fresh review; see the [workflow collaboration guide](../../../workflow-collaboration.md).

- Date: 2026-11-16.
- Scope: final review of the proposed Workflow collaboration implementation plan.
- Mode: source-grounded architecture and implementation-readiness review.
- Verdict: BLOCKED for dependent feature implementation. Contract and baseline
  work can begin while the applicable decision gates and contracts are resolved.
- Evidence boundary: document and representative source inspection. Application
  tests, live A2A interoperability, deployed acceptance, and automated secret
  scanning are not established by this review.

## Resolution Update (2026-11-17)

The feature owner approved all sixteen
[decision gate resolutions](decision-gate-resolutions.md) and accepted
[ADR-0029](../../../adr/0029-workflow-execution-boundaries.md). The verdict
above is unchanged as a record of the 2026-11-16 review. Dependent implementation
may now proceed behind a disabled feature in delivery order. This is not
independent security-owner approval. Each remedy still needs executable
acceptance evidence, and production activation needs that approval.

## Governance Design Elaboration (2026-11-18)

The accepted decisions stand. The expanded
[execution contract](execution-contract.md#governance-and-trust-enforcement)
details per-operation identity, authority, content release, effective Agent
autonomy, exact-action review, and evidence without defining a shared team
principal or pooled trust score. Specialization uses separately authorized
service operations and does not transfer a specialist's capabilities to its
coordinator. This is design elaboration, not independent security-owner approval.

The [strategy contract](execution-contract.md#strategy-safety-and-runtime-state)
distinguishes child completion from activation finalization, preserves optional
branch uncertainty, requires passive recipient preparation and atomic handoff,
and defines durable progress/capacity waits. Native Go and the approved gRPC
polling profile remain the release baseline. The SDK compatibility spike still
precedes transport implementation.

Source cross-checks identify these concrete implementation obligations:

| Boundary | Required implementation proof |
| --- | --- |
| Non-Agent initiation review | The current Gate3 request and Agent-specific approval references cannot authenticate an executor by substitution. Review a typed executor-action representation without changing legacy Agent meanings or skipping review. |
| Handoff lifecycle | Opt-in proposal, acknowledgement, guarded owner-transfer, and cancellation-precedence primitives are now implemented in the activation store. Authenticate their callers, implement real passive recipient preparation, wire owner eligibility across effect paths, and qualify cancellation/transfer races before supporting runtime Handoff. |
| Downstream content use | Member input authorization must continue through provider/model egress, tools, memory, knowledge, evaluation, logs, status and native resource reads. A capability grant alone cannot release restricted content. |
| Strict field handling | Prove rejection at supported API boundaries. Pruning alone cannot demonstrate that an unknown governance field reached admission or was rejected. |
| Native scheduling and gate capacity | Prove nonblocking child waits, shared Agent capacity, finalization allowance, and bounded-ledger headroom without gate rotation or lost effect evidence. |

Use the [governance acceptance matrix](execution-contract.md#governance-acceptance-scenarios)
to test these contracts. These obligations refine implementation and evidence
requirements within the approved resolutions. They do not reopen the historical
decision-gate verdict or claim that the in-progress schema work is executable
collaboration. If a proposed implementation changes an accepted policy, record
an amendment before relying on it.

## Dedicated Receiver Draft Security Assessment

Verdict: BLOCKED for privilege grants, receiving deployment and activation.
Disabled design preparation is authorized. This is a source-grounded draft
assessment, not independent security-owner approval or a live permission audit.
The [dedicated receiver proposal](execution-contract.md#disabled-dedicated-receiver-design)
preserves accepted gates and the installed unconditional task-write denial.

| Threat and boundary | Activation risk | Required control and open evidence |
| --- | --- | --- |
| Spoofing and privilege elevation through shared identity | High if the default ServiceAccount gains protected access | Existing candidate Deployments select the shared default identity. Independently approve dedicated ServiceAccount and live Pod UIDs, protect writer Pod/token/exec/key custody, and prove wrong-identity and replacement denials. No new permissions were granted. |
| Tampering and replay at native persistence | High if a controller or receiver gets a blanket exemption | Independently authorize exact CREATE/UPDATE receipts, verify old and proposed state, preserve immutable scope/history and UID/resourceVersion CAS, and deny unrelated ConfigMap writes by the writer. Raw-record and full-object v2 receipt binding are tested with injected authorization. Live writer authorization, positive installed v2 acceptance, API defaulting and key isolation remain unfinished. |
| Information disclosure across polling and recovery | High if discovery or expired initiation is treated as content authority | Preserve current per-read authorization and exact retained owner. Qualify TLS peers, required extensions, receiver binding and bounded maintenance without fresh initiation or credential forwarding. Existing injected SDK tests do not qualify deployment. |
| Repudiation and duplicate effect attribution | High if a new process becomes another effect owner | Keep RuntimeApproval and retained activation ownership unchanged. Prove durable evidence and no resend after committed send failure, lost acknowledgement and restart in both stores. Real member provenance and deployed qualification remain open. |
| Denial of service through writer or retention bypass | Medium before executable bounds are proven | Enforce bounded receipt/task decoding, authorization deadlines, shared budgets and 7-day content/90-day replay retention. A new receiver must not reset allowances, erase tombstones or treat uncertain effects as complete. |

The [Kubernetes RBAC reference](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
states that top-level CREATE cannot be restricted by resource name, default
ServiceAccount permissions apply to every Pod selecting that identity, and
namespace edit permissions can expose other ServiceAccounts through Pod creation.
The [AKS identity reference](https://learn.microsoft.com/en-us/azure/aks/concepts-identity)
distinguishes Kubernetes authentication/authorization from Azure resource and
workload identity. Cluster reachability and Ready services establish no receiver
authority or native runtime conformance.

Azure policy, live role assignments and independent security-owner review were
not validated. Deferred Azure and Microsoft documentation tools were unavailable,
so the public references above were fetched directly. No compliance or deployment
clearance is claimed. Recorded receipt and runtime/schema approval, installed
identity/admission acceptance, both-backend interoperability and operator approval
remain prerequisites. Tasks 1 through 5 remain open and collaboration stays disabled.

The [isolated receipt contract evidence](execution-contract.md#isolated-native-task-mutation-receipt-contract)
covers bounded canonical decoding, domain-separated signatures, exact UPDATE
prior-record binding and shared history/version checks. It grants no principal
permissions and does not qualify the complete ConfigMap representation, actual
writer authorization or signing-key custody. The unconditional task-write guard
is unchanged. This implementation evidence does not change the BLOCKED verdict.

The [full ConfigMap representation](execution-contract.md#proposed-full-configmap-receipt-representation)
now has an unwired source implementation. Its separate v2 receipt binds a strictly
whitelisted metadata envelope and exact raw-task digest, with prior UID/version
and both prior digests for UPDATE. Component tests cover metadata/raw-field
tampering, expired retained signatures, current-authority revocation and missing
or v1 receipt denial. Installed checks confirm unconditional task-write denial,
not positive v2 acceptance. API-assigned metadata/defaulting, genuine writer
authentication and signing-key custody still require installed proof and
independent review. The [source evidence and AKS hold](execution-contract.md#full-object-receipt-source-evidence-and-aks-hold)
record the tested boundary and revalidated default-ServiceAccount candidates.
The user's finalization request authorizes implementation work but supplies no
independent security-owner approval. The BLOCKED activation verdict is unchanged.

The [2026-11-22 writer and admission code review](security-review-code.md)
preserves its original findings and records the
[implementation follow-up](security-review-code.md#implementation-follow-up).
The parser defect is repaired, and the public writer/recovery source tests pass.
The durable CREATE marker now has real PostgreSQL and real API-server CAS
evidence. The high finding remains open for authenticated transport/startup,
positive installed task-writer admission, all-writer admission selection,
deployment custody and native receiver provenance. The installed task-write
denial and BLOCKED activation verdict remain in place. This technical review
supplies no independent security-owner or operator approval.

## Recommended Remedy Update

The [execution contract](execution-contract.md) and
[Proposed ADR-0029](../../../adr/0029-workflow-execution-boundaries.md) now specify
the three recommended remedies: an operator-bound approved-revision executor,
explicit data bindings with retained provenance, and one-gate final dispatch
admission with honest stop/drain semantics. These are concrete design resolutions
prepared under delegated technical judgment while the user was unavailable.
They are not recorded as explicit user approval or completed implementation.

The original source observations below remain valid. Interpret their design
questions through the new contract. Remaining gates are ADR acceptance, exact
policy and record schemas, implementation, and executable acceptance evidence.

The [disabled persistence foundation](execution-contract.md#disabled-persistence-foundation-2026-11-16)
now supplies typed state transitions and PostgreSQL/Kubernetes adapters for the
dispatch gate. This is partial implementation of the third remedy. The gate is
not wired into effect paths or protected by new installation policy. Kubernetes
tests use a fake client, so real API-server and authorization acceptance remain
open. The original review verdict is not upgraded by this source-only progress.

## Findings And Disposition

| Severity | Finding | Disposition |
| --- | --- | --- |
| High | Initial dispatch lacks a defined executor authority | Design remedy specified: operator-bound revision approval, separate initiation capability/route, exact target policy, and original initiator attribution. ADR acceptance, policy schema, implementation and tests remain open. |
| High | Mixed Workflow boundaries can skip existing communication checks | Design remedy specified: explicit bindings, retained lineage, restricted templates, and protected result publication. Binding schema, implementation and cross-surface tests remain open. |
| High | Cancellation and handoff need effect-plane fencing | Design remedy specified: final CommitDispatch and stop share one bounded gate while plane claims retain ownership. Store schema, all-plane integration and race/crash evidence remain open. |
| High | Cyclic visits can collide in the proposed step identity | Corrected the identity to include a persisted activation and graph iteration. Added a repeated-visit acceptance case. |
| Medium | Reconcile instructions stop the maintenance needed to finish cancellation | Corrected new-execution versus maintenance behavior and deletion ordering. Added it to the required stop/fencing acceptance. |
| High | Unresolved decision gates prevent a feature-implementation readiness claim | Preserved proposals as proposals and made readiness phase-specific. No reviewer or user approval was inferred. |

### Initial dispatch authority

The [gateway source validation](../../../../internal/agentgateway/server.go)
requires a current AgentIdentity and a Running AgentInstance bound to its active
session. A Workflow controller starting the first member cannot satisfy that
contract by supplying only a run ID or Agent reference.

The implementation must distinguish authenticated control-plane initiation from
real member-to-member delegation. Do not weaken existing Agent validation,
fabricate a source AgentInstance, or issue a wildcard member capability to solve
the bootstrap problem. The execution contract selects a narrowly scoped
operator-bound executor path with explicit revocation semantics. Approve its
policy schema before dispatcher implementation.

### Mixed Workflow boundaries

The [inbound-edge authorizer](../../../../internal/controller/workflow_reconciler.go)
skips sources and targets without `agentRef`, and its interface represents one
namespace. A collaboration step therefore needs explicit policy integration.
The [input-template path](../../../../internal/controller/workflow_dataflow.go)
also makes prior step outputs available by step name, so edge-only checks cannot
establish every data-release boundary.

The execution contract defines explicit source-to-recipient bindings and
conservative provenance for normal Agent, Tool, and collaboration data. A
finalizer's identity cannot erase worker restrictions. Qualify generic resource
reads and native RBAC exposure as well as dedicated artifact APIs, while keeping
legacy graph-only definitions compatible.

### Stop and handoff fencing

The [gateway source contract](../../../../internal/agentgateway/server.go) checks
active AgentInstance and session bindings, which do not by themselves establish
that a particular run activation or owner epoch is still allowed to act.

Cancellation or an owner transfer must prevent new effects at all applicable
capability boundaries. Restricting only peer delegation leaves model, tool, and
data operations available to a still-running old session. An already claimed
effect cannot be erased by an epoch increment. Define the transfer's quiescent
boundary and keep recipient activation blocked while a required prior effect is
uncertain. Read, cancel, settle, and recovery operations require a distinct
authorized maintenance path. The selected ordering is final dispatch admission,
not an unsupported transaction across run state and plane claims. CommitDispatch
and stop use one activation record. A racing plane claim that loses admission
cannot contact the target. A dispatch committed before stop remains in flight
and blocks conclusive drain or handoff while its outcome is uncertain. Test both
orderings and every described crash window in both persistence backends.

### Cyclic activation identity

The [Workflow engine](../../../../internal/controller/workflow_reconciler.go)
supports repeated step execution and identifies graph iterations on child jobs.
The original proposed collaboration key used run UID, step, and attempt without
the graph visit. A second visit could match old results, sessions, or receipts.

The plan now requires a persisted activation ID incorporating the scheduled graph
visit and retains cumulative parent allowances. Test a genuine WorkflowRun with
at least two collaboration visits and late first-visit callbacks after the second
visit starts.

### Cancellation maintenance

The original reconcile description refused work for a deleting or
cancellation-requested run while later requiring status retrieval and settlement.
The plan now forbids new execution but continues authorized maintenance.

The [current deletion path](../../../../internal/controller/workflow_run_reconciler.go)
also marks a nonterminal run failed and deletes locally owned jobs. Collaboration
must enter its maintenance and tracked-child cleanup path before inheriting that
behavior. Finalizer removal cannot be evidence that remote effects have stopped.

## Attack Surface And Trust Boundaries

| Surface | Data and exposure | Required control |
| --- | --- | --- |
| Workflow and WorkflowRun API | User input, strategy configuration, member references, and lifecycle commands | Operation-scoped control-plane authorization and discriminated admission |
| Workflow executor to first member | Initiator authority crossing into member namespaces | Exact target/run/activation binding and a reviewed executor principal |
| Member-to-member A2A | Task content, context, artifacts, and nested proposals | Actual member identity, destination grants, live communication policy, and bounded scope |
| Mixed steps and templates | Output content crossing Agent, Tool, and collaboration boundaries | Provenance, recipient checks, and explicit data-release semantics |
| Runtime capability gateways | Paid inference and protected tool or data effects | Exact claims, run/owner eligibility, existing approvals, and conservative uncertainty |
| Persistent task and content stores | Task IDs, ownership, allocations, protected content, and replay evidence | Conditional writes, restricted writers/readers, retention, and schema-safe records |
| Lifecycle and maintenance operations | Cancellation, polling, settlement, cleanup, and recovery | Run-scoped access without granting new execution authority |

### STRIDE assessment

| Threat class | Assessment and required evidence |
| --- | --- |
| Spoofing | Executor initiation and member delegation must remain distinguishable. Test forged source identity, same-named foreign Agents, and copied run headers. |
| Tampering | Bind task content, activation, target, attempt, and owner epoch. Test changed retries, old-visit callbacks, and transfer races. |
| Repudiation | Attribute both original initiator and acting principal. Retain invocation, approval, effect, and settlement references across restart and deletion. |
| Information disclosure | Check content release through templates and composed steps as well as A2A operations. Test cross-run reads and worker-content provenance. |
| Denial of service | Enforce cumulative allowances, member capacity, finite buffers, retention, and maintenance rate limits. Waiting and cancelled runs cannot hold unbounded compute. |
| Elevation of privilege | A new step type, finalizer, approved task, or owner epoch cannot bypass an effect grant or revive stopped work. Verify each affected gateway. |

## Implementation Entry Conditions

Contract and baseline work may begin without enabling execution. It includes
writing the reviewed contracts, selecting and testing the protocol/SDK profile,
and establishing existing-behavior baselines with deterministic isolated fixtures.

Before a dependent feature slice begins, record the corresponding decision-gate
outcomes in the [plan](plan.md#decision-gates). In particular:

- API work needs exact schema, validation ceilings, strategy configuration, and
  compatible lifecycle projections.
- Storage work needs record layout, conditional transition invariants, writer
  protection, crash recovery, content authorization, and retention contracts.
- Dispatcher and gateway work needs initial executor authority, mixed-boundary
  rules, protocol mapping, cross-namespace migration, and cross-plane eligibility.
- Strategy work needs completion, required/optional result, transfer, and retry
  semantics plus the runtime capability prerequisites.
- Activation needs the complete four-strategy matrix, affected security checks,
  packaging, migration and rollback evidence, and separate operator approval.

Existing gateway and recovery ADRs remain authoritative. ADR-0029 proposes the
authority, release, and dispatch-admission design. Review it and the remaining
lifecycle/protocol decisions before dependent code is treated as implementation-
ready. This review does not accept the ADR or replace a security owner's approval.

## Review Summary

- Confirmed direction: extend Workflow and retain WorkflowRun, with all four
  strategies and cross-namespace managed-Agent membership.
- Corrected planning defects: activation identity and stop/maintenance ordering.
- Recommended remedies specified: operator-bound initiation, explicit
  mixed-boundary release, and a final dispatch-admission fence across effect
  planes. Implementation and acceptance findings remain open.
- Assumption retained: trusted A2A serving adapters mediate managed Agents.
  External federation remains outside the first release.
- Next decision: review Proposed ADR-0029 and the concrete execution contract,
  settle the remaining schemas and profile choices, and obtain executable
  compatibility evidence. Documentation does not establish implementation approval.

No application-code vulnerability, passing runtime behavior, production safety,
or complete protocol conformance is claimed from this planning review alone.
