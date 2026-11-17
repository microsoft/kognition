# Workflow Collaboration Execution Contract

- Status: recommended design under Proposed ADR-0029. A disabled persistence
   foundation is implemented. Full execution and activation are not approved.
- Date: 2026-11-16.
- Authority: [ADR-0029](../../../adr/0029-workflow-execution-boundaries.md) records
  the alternatives and proposed decision. Existing accepted effect-ownership
  and recovery ADRs remain authoritative.
- Scope: resolve initiation authority, mixed-step data release, and cross-plane
  stop and handoff fencing from the [readiness review](security-review-architecture.md).

## Selected Design And Limits

Use an operator-bound Workflow executor, explicit data bindings with retained
provenance, and a final dispatch-admission fence shared with stop transitions.
Keep WorkflowRun as the public execution resource. Do not introduce AgentPool,
AgentPoolRun, an artificial member Agent, or another exact-effect execution owner.

The operator-bound authority model is the recommended choice made while the user
was unavailable and had delegated technical decisions. It is not recorded as an
explicit user selection or an accepted security decision. The original submitter's
credentials are never retained. Revoking submit permission prevents new runs.
Stopping an accepted run requires cancellation, revocation of its execution
binding or destination authority, or expiry of its bounded execution authority.

The contracts below make the three design findings actionable. SDK selection,
full strategy schemas, operational ceilings, backend migrations, and the remaining
[plan gates](plan.md#decision-gates) are separate prerequisites.

## Workflow Initiation Authority

### Three distinct principals

| Principal | Authority and attribution |
| --- | --- |
| Submitter | Authenticated caller allowed to use an approved Workflow revision and create its run. Retained as original initiator, not used as a reusable credential. |
| Workflow executor | Operator-bound non-Agent workload principal permitted to initiate exact targets for that Workflow revision. The trusted controller presents its verified workload identity. |
| Member Agent | Actual AgentIdentity, AgentInstance UID, active session, activation, attempt, and owner epoch. Used for member-originated delegation and protected effects. |

Permission to author a Workflow, use its executor, invoke a target, reference a
foreign Agent, and read resulting content are distinct. A controller's service
identity is authentication, not an unrestricted grant to start every Agent.

### Operator-owned execution binding

Maintain a protected execution binding containing:

- Workflow namespace and immutable UID, approved revision and canonical digest.
- Executor's canonical workload principal and permitted controller incarnations
  or attestation profile.
- Explicit target namespaces, Agent and serving identities, allowed operation
  kinds, and configured authority duration and limits.
- Allowed submitters or a separately evaluated permission to use this Workflow.
- Policy version, enabled/revoked state, and attribution of the approving
  operator.

Use operator-managed configuration and protected backend records, not an
author-writable Workflow field that selects an arbitrary ServiceAccount. The
first implementation does not need another public resource kind. Specify and
test the binding's administrator-only configuration surface before coding it.
Editing a Workflow creates a new revision that needs a matching approved binding.
Recreating a same-named Workflow cannot inherit the old binding.

Kubernetes evaluates explicit `use` permission on the Workflow and `invoke`
permission on the exact Agent for the bound executor through SubjectAccessReview.
These are proposed custom authorization verbs on the named resource, not claims
that native A2A subresources already exist. Resource names and namespaces must be
bounded. The controller does not receive user impersonation rights.

Standalone mode needs equivalent typed Workflow-use and Agent-invocation policy
evaluation for canonical principals. Extend the administrator-protected
ControlPlaneRole model with separately typed operation and target rules. Its
current RuntimeApproval-only rules, wildcard fields, or approval permissions must
not implicitly grant Workflow execution. Keep default deny and deny precedence.
An anonymous identity or shared runtime bearer token is insufficient.

### Initiation sequence

1. Authenticate submission and record the original identity from trusted request
   context. Kubernetes uses authenticated admission context and protected
   admission evidence, not caller annotations. Bind the persisted run UID before
   dispatch. Specify protection against forged, replayed, or edited evidence.
2. Resolve the Workflow revision and approved execution binding. Evaluate the
   submitter's permission to use that exact configuration. Persist an acceptance
   record with the run UID, binding version, expiry, and definition digest.
3. Before each new initiation, re-evaluate the live binding, executor identity,
   target `invoke` authorization, foreign-reference grant, target readiness, and
   remaining run allowance. An expired or revoked binding cannot be refreshed by
   changing the submitter or replaying the acceptance record.
4. Issue a short-lived, exact initiation capability with a distinct type and
   audience from Agent delegation. Bind run UID, activation, attempt, logical
   task, input digest, target UIDs/revision, executor, binding version, and expiry.
5. Accept it only at a separately authenticated Workflow-initiation route using
   the shared invocation core. Preserve the existing Agent delegation validator.
   A member-session capability cannot enter this route and an initiation
   capability cannot invoke general Agent, Tool, Model, or Memory operations.
6. Create the target's legitimate local execution/session through the reviewed
   AgentService adapter. It receives fresh member-scoped capabilities and the
   exact run context. No source AgentInstance is fabricated for the executor.

Apply the same contract to initial coordinators, parallel workers, peer entries,
and controller-initiated synthesizers. A member-selected child task instead uses
that real member's current identity and communication authority. Recovered
controllers must recover the same operation binding rather than mint a new task.

Audit original initiator, acting executor or Agent, execution-binding version,
run/activation/task, target, authorization evidence, and disposition. Do not log
capabilities or content merely to demonstrate attribution.

## Explicit Data Release And Mixed Steps

### Data is not a dependency edge

For new collaboration-bearing Workflow definitions, require an explicit dataflow
contract covering inputs and the selected published result. Existing graph-only
definitions retain their behavior. The exact additive schema remains an API gate,
with these required fields for each input binding:

- Source run-input field or source step, output/artifact identity, and optional
  validated structured-field selector.
- Destination step and exact member alias or declared recipient set.
- Purpose/input name, supported media type, schema/size bounds, and treatment of
  absent optional data.

Dependency edges still schedule execution. They neither release all predecessor
data nor authorize a recipient. A data reference must also have a valid execution
dependency or a deliberately supported run-input origin. Reject future-step and
ambiguous cyclic references. Cyclic reads name the applicable completed
activation explicitly, not whichever same-named result was written last.

### Authoritative provenance and access

Each protected value or artifact has trusted lineage containing its run and
activation, producer principal, contributing input origins, and content-access
policy references. Gateways and the executor assign it. Member text cannot
replace it or declare it public.

Use conservative propagation: a derived output inherits the union of input
origins and all applicable restrictions, plus its actual producer. A finalizer's
summary does not remove a worker's restrictions. Automatic declassification and
model-selected removal of provenance are outside the first release.

Allow a release only when all of the following hold:

1. The accepted definition contains the binding and authorizes the exact
   destination or recipient set.
2. The source and result belong to the intended run, activation, and attempt.
3. The destination has content-read authority for every inherited restriction.
4. Current cross-namespace grants and source-to-recipient communication policy
   permit the actual Agent origins where applicable. Executor-produced input
   uses the separate executor/content policy rather than an invented Agent.
5. Target schema, media type, and size constraints pass.

Tool output retains its actual tool identity and invoking principal. ToolGrant
authorizes invocation but is not a blanket right to publish its result. A
controller operation, synthesizer, or new member cannot relabel restricted
content as unrestricted run input.

### Template and publication enforcement

- Build the template context only from data bindings authorized for the exact
  receiving step/member. Never render from all accumulated step outputs and then
  attempt to filter the resulting string.
- Validate new definitions using the Go template AST and a bounded supported
  reference subset. Reject dynamic or computed access to `.Steps` that cannot
  be resolved against explicit bindings. Do not use text matching as a parser.
- Apply the rule to ordinary Agent and Tool steps inside the new definition as
  well as collaboration steps. Otherwise a template can route around a strict
  collaboration edge.
- Publish an explicit selected result rather than the incidental last sink
  output. Run readers need authorization for the published content. Permission
  to see status or to submit/cancel a run does not grant every artifact read.
- Keep protected payloads behind authorized references. Do not copy them into
  generic WorkflowRun, AgentJob, AgentInstance, event, or audit fields that a
  broader native Kubernetes get/list/watch permission exposes. Update all
  relevant serializers and UI projections as part of this feature.
- Direct Kubernetes readers remain subject to native RBAC. If any supported
  profile stores plaintext in an object, its native readers are inside that
  content's trust boundary. Do not claim finer payload secrecy without the
  protected-reference path and its installed acceptance evidence.

Provenance is an enforcement and attribution mechanism for platform-carried data,
not proof that an LLM has identified all semantic dependencies or cannot disclose
information it already possesses. Runtime isolation and gateway policy remain
necessary.

## Stop, Handoff, And Effect Admission

### Separate effect ownership from permission to dispatch

RuntimeApproval or the plane's existing ledger continues to own exact effect
execution and replay. Add a mandatory final dispatch-admission decision after
that claim and immediately before the target call. This is a related safety
check in the existing pre-target protocol, not a second owner allowed to replay
or reassign the operation.

This explicitly refines the earlier phrase "stop prevents every later claim."
Independent Kubernetes objects cannot make a run-state check and a plane claim
one transaction. A plane claim may win while stop is racing, but it does not
authorize target contact without dispatch admission. The atomic promise is:
after stop wins, no new final dispatch admission can commit for that epoch.

### One bounded activation gate

Maintain one protected gate per run UID, step activation, and attempt:

- Mode Active, Quiescing, Stopping, Stopped, or OutcomeUncertain.
- Monotonic owner epoch and allowed acting members for that epoch.
- Fixed authority/budget binding and monotonic counters.
- A finite set of exact effect entries with binding digest, plane owner reference,
  claimant incarnation, owner epoch, and Registered, DispatchCommitted, Settled,
  Aborted, or Uncertain state.
- Configured finite entry/encoded-byte ceilings bounded by administrator limits.
  Reject excess work before adding a record rather than exceeding API object
  limits or silently evicting an unfinished entry.

Persist final effect bindings and replay fences before any supported compaction.
The first implementation can retain the finite activation ledger until its
approved retention boundary. It does not need an unbounded per-run CAS object or
an unsafe active-slot eviction protocol.

PostgreSQL uses a conditional update under a locked activation row, with the
effect-entry transition and gate-mode predicate in the same transaction.
Kubernetes uses a protected control-namespace object containing gate mode, epoch,
and bounded effect entries, updated by one resourceVersion compare-and-swap.
A Lease, cached read, separate-object status patch, or eventually consistent watch
does not supply this atomicity. Use a typed store capability, not generic
read-then-write code in every gateway.

### Effect sequence

1. Revalidate exact actor, run/activation/attempt/epoch, live policy, target,
   authority deadline, and effect allowance. Register the exact effect binding
   in the gate while it is Active. Registration alone cannot execute anything.
2. Complete current approval, quota preparation, receipt preparation, and
   write-ahead evidence requirements. Pending approval carries no dispatch
   admission and cannot revive an old epoch.
3. Acquire the existing exact plane execution claim. A changed binding conflicts.
   The effect has one owner even if the run is now stopping.
4. The same claimant calls `CommitDispatch` with the exact plane-claim reference.
   The store atomically requires Active mode, matching owner epoch, matching
   registered binding, and no existing conflicting disposition. Record
   DispatchCommitted in the same gate write.
5. Recheck exact plane claimant ownership and contact the target with no unrelated
   work. The operation must carry its stable invocation and target bindings.
6. Persist the plane outcome, accounting, and evidence under existing rules.
   Settle the gate entry idempotently from that exact retained outcome.

No target contact is allowed before step 4, including model calls, Tool calls,
memory mutations, knowledge operations, and nested A2A submissions. Qualify reads
that disclose protected content as effects requiring eligibility as well. A
runtime without the required trusted bindings is unsupported for collaboration.

### Stop and transfer sequence

1. Persist cancellation or handoff intent and authenticate the requested
   lifecycle operation.
2. Atomically change the gate from Active to Stopping or Quiescing, fence its
   epoch, and record the accepted stop/transfer operation. Repeated matching
   requests return that same state.
3. Reject every later CommitDispatch for the old epoch. A stale claimant must
   resolve the current gate on conflict rather than retry with a refreshed epoch.
4. For Registered entries that never reached DispatchCommitted, retain durable
   no-dispatch evidence. Abort preparations or finalize a claimed-but-not-dispatched
   failure only through the supported plane protocol and rightful owner. Consume
   the invocation identity. Never reopen the claim or invent a human decision.
5. For DispatchCommitted entries, request cooperative cancellation and reconcile
   their actual outcomes. They remain admitted in-flight operations even when
   their network call starts after stop won the gate write.
6. Mark Stopped or activate a handoff recipient only after every relevant prior
   entry has a conclusive disposition and required settlement. If any dispatch
   outcome is unknown, retain OutcomeUncertain and do not activate the recipient.

Cancellation acknowledgement has two meanings that APIs and UI must distinguish:
the stop request was durably accepted, and execution was conclusively drained.
Do not report the second when only the first occurred. This design does not
guarantee that no external effect can happen after a cancellation request.

### Crash and race outcomes

| Ordering or crash | Required outcome |
| --- | --- |
| Stop wins before registration | No entry and no new target call. |
| Stop wins after registration but before plane claim | Plane preparation can be aborted. A racing claim still cannot commit dispatch. |
| Plane claim wins, then stop wins before CommitDispatch | Preserve the owner and finalize known no-dispatch failure under the exact protocol. No target call. |
| CommitDispatch wins, then stop | Treat the operation as in flight. Drain or retain uncertainty before acknowledged stop or recipient activation. |
| Crash after registration, before dispatch commit | The frozen gate proves no dispatch was admitted. Reconcile only with the existing claimant/recovery rules. |
| Crash after dispatch commit, before target acknowledgement | Outcome is potentially uncertain. Do not infer no effect or redispatch because a lease expired. |
| Crash after known plane outcome, before gate settlement | Reconcile the retained exact result without another target call. |
| Stale approval, callback, or checkpoint after epoch change | Reject new dispatch and task completion from that epoch while allowing authorized evidence reconciliation. |

A conservative uncertain outcome is acceptable. A claim timeout is not proof
that the target was never contacted. Recovery never transfers a claimed effect
to a new executor simply because the activation gate exists.

### Maintenance and all-plane enforcement

Authorize status retrieval, cancellation, retained-result settlement, and recovery
separately from execution. Maintenance accepts an exact known task/effect
reference and cannot mint a new executable capability or alter the run scope.

All entry points for an affected plane must use the final admission check,
including controller-side invocations, external workers, and legacy transport
adapters carrying collaboration context. Unbound calls from a collaboration
session fail closed. Existing non-collaboration sessions retain their contract.
Network egress and credential placement must prevent runtime bypass of gateways.

## Delivery And Acceptance

Implement in the following dependency order, with execution disabled until the
supported matrix is qualified:

1. Review ADR-0029 and the selected authority policy. Define the protected binding,
   lifecycle operations, finite limits, and backend record schemas.
2. Add characterization tests for original invocation, Workflow dataflow, and
   exact-effect replay. Define typed store contracts and barrier-controlled race
   tests before adding target calls.
3. Implement and protect the activation gate in both stores. Demonstrate every
   crash/race outcome above and rejected unauthorized writes/deletes.
4. Add initiation authorization and exact capabilities. Test a real first-member
   start, wrong executor, arbitrary-ServiceAccount selection, changed revision,
   revoked binding, foreign target, and copied capability.
5. Add explicit data bindings and protected payload access across every relevant
   API, template, native resource, worker, and UI projection.
6. Integrate final dispatch admission in each supported gateway and controller
   invocation path. A partially integrated runtime/profile remains disabled.
7. Qualify Supervisor, Parallel, Handoff, and Peer through the shared machinery,
   including repeated cyclic activations and cross-namespace members.

Required acceptance includes:

- Independent and concurrent admission-versus-stop tests in PostgreSQL and a real
  Kubernetes API server. Force both write orderings using test barriers.
- A claimed operation losing CommitDispatch with zero fixture target calls, plus
  a committed operation losing acknowledgement with no automatic replay.
- Late old-owner tool/model/data requests after handoff intent and after a new
  epoch, including capabilities issued before the transition.
- Pending approval completing after stop without target execution.
- Known-result settlement after controller or gateway restart without inference.
- Template, mixed-edge, list/watch, raw-resource, and artifact-read attempts that
  cannot expose content outside approved bindings and native trust boundaries.
- Submitter credential expiry without credential retention, executor revocation
  stopping new admissions, and exact original/acting-principal audit attribution.

## Resolution Status

| Finding | Design disposition | Remaining evidence |
| --- | --- | --- |
| Initial executor authority | Concrete operator-bound model and distinct initiation path specified | ADR acceptance, protected policy schema, implementation and authorization tests |
| Mixed-step data release | Explicit bindings, conservative lineage, restricted templates, and protected publication specified | Schema agreement and cross-surface content-access acceptance |
| Stop and handoff race | Typed activation state machine and PostgreSQL/Kubernetes persistence adapters implemented as an unwired foundation | Plane-claim verification, all-plane integration, durable effect recovery, record protection, real API-server acceptance, and installed enforcement |

### Disabled persistence foundation, 2026-11-16

- [Typed gate contract and transitions](../../../../internal/store/workflow_activation.go)
   bind run UID, step activation, attempt, actor digest, effect digest, and exact
   plane-claim reference. Expected-version mutations serialize transitions.
- Only a newly persisted CommitDispatch returns `DispatchGranted`. Matching
   retained commits return observation without permission to contact the target.
   A lost acknowledgement must therefore preserve uncertainty rather than grant
   replay. This store does not itself authenticate a plane claim or caller.
- Stop and quiescence advance the owner epoch. Unknown committed outcomes block
   drain, and explicit resolution retains a proof digest. There is deliberately
   no recipient-activation or epoch-reopening method in this foundation.
- Finite effect and encoded-byte ceilings reserve worst-case space for stop,
   exact claim metadata, settlement, and evidence. Filling registration capacity
   must not prevent maintenance. These conservative internal ceilings do not
   establish final public schema defaults or operational limits.
- [PostgreSQL adapter](../../../../internal/store/postgres/workflow_activation.go)
   uses a row lock for mutation and a transaction-scoped activation-key lock for
   creation. It stores a private, unregistered `WorkflowActivationRecord` in the
   existing resource table. No migration or public API kind is introduced.
   Retained soft-deleted records block recreation.
- [Kubernetes adapter](../../../../internal/k8s/controller/workflow_activation_store.go)
   is explicitly constructed with a reader, writer, control namespace, and trusted
   clock boundary. It uses one ConfigMap update with resourceVersion CAS. Production
   callers must supply an uncached reader and conservative healthy-clock time.
   The adapter is not constructed by any controller or gateway assembly.
- [Shared backend tests](../../../../internal/store/workflowactivationtest/contract.go)
   exercise both stop/dispatch orderings, replay denial, immutable binding,
   namespace and activation isolation, and simultaneous mutations through two
   store instances. PostgreSQL uses its existing disposable embedded database.
   Kubernetes coverage currently uses controller-runtime's fake client.
- Focused tests additionally cover malformed records, expiry, finite capacity,
   uncertainty resolution, concurrent database creation, and a Kubernetes write
   whose successful persistence is followed by a lost acknowledgement.

The following focused command passed on 2026-11-16 with Go 1.27.4 on Windows:

```powershell
go test -race -p 1 ./internal/store ./internal/k8s/controller ./internal/store/postgres -run 'Test.*WorkflowActivation' -count=1 -timeout=180s -v
```

All three packages completed successfully with the race detector enabled. The
PostgreSQL package started and stopped its disposable PostgreSQL 16.9 fixture.
The Kubernetes package used the fake client, including concurrent store updates
and lost-acknowledgement recovery. These receipts cover the named activation
tests only, not a full package suite or installed Kubernetes behavior.

No capability issuer, gateway, Workflow dispatcher, public schema, chart, RBAC,
or admission policy enables collaboration through this foundation. Before use,
protect Kubernetes gate creation/update/deletion, qualify a real API server,
verify exact plane-claim ownership at the trusted boundary, and integrate the
gate across every supported effect path. Database-level writers and Kubernetes
administrators remain inside the storage trust boundary.

The three security findings remain open for complete implementation and
acceptance. This progress does not establish A2A interoperability, safe handoff
of a live member, production cancellation, or activation readiness.
