# Workflow Collaboration Execution Contract

> **Superseded in part.** The receiver, task-plane and receipt mechanisms in this
> contract were replaced by AgentJob-based member turns with an owner-referenced
> ledger ConfigMap. See the [workflow collaboration guide](../../../workflow-collaboration.md).
> Strategy semantics, limits and validation rules remain accurate.

- Status: implementation contract under Accepted ADR-0029 and the feature-owner
   approved decision gates. Disabled persistence and standalone execution-policy
   foundations are implemented.
   Production activation and independent security-owner approval remain separate.
- Date: 2026-11-16. Governance and strategy elaboration: 2026-11-18.
- Authority: [ADR-0029](../../../adr/0029-workflow-execution-boundaries.md) records
   the alternatives and accepted decision. The
   [approved gate resolutions](decision-gate-resolutions.md) govern exact scope,
   limits, the native-runtime release profile, and implementation entry order.
   Existing accepted effect-ownership and recovery ADRs remain authoritative.
- Scope: resolve initiation authority, mixed-step data release, and cross-plane
  stop and handoff fencing from the [readiness review](security-review-architecture.md).

## Selected Design And Limits

Use an operator-bound Workflow executor, explicit data bindings with retained
provenance, and a final dispatch-admission fence shared with stop transitions.
Keep WorkflowRun as the public execution resource. Do not introduce AgentPool,
AgentPoolRun, an artificial member Agent, or another exact-effect execution owner.

The operator-bound authority model was accepted by the feature owner on
2026-11-17 through ADR-0029 and gates 2, 3, and 11. That acceptance permits disabled
implementation and does not replace independent security-owner or activation
approval. The original submitter's credentials are never retained. Revoking
submit permission prevents new runs.
Stopping an accepted run requires cancellation, revocation of its execution
binding or destination authority, or expiry of its bounded execution authority.

The contracts below make the design findings actionable within the approved
resolutions. Gate 13 selects A2A 1.0 over gRPC and the official Go v2 SDK subject
to a passing compatibility spike. Do not select another binding, relax the
approved limits, or claim runtime support without the prescribed evidence.
Implementation details introduced below describe enforcement responsibilities.
They do not silently amend an approved gate or establish executable acceptance.

## Governance And Trust Enforcement

### Distinct meanings of trust

Use the [reference terminology](../../../architecture/reference/glossary.md).
A team is a bounded collaboration, not a shared authorization principal.

| Concept | Meaning at the collaboration boundary | It cannot establish |
| --- | --- | --- |
| Workload identity | Verified principal and immutable Agent, instance, session, or executor binding | Permission for every advertised skill or target |
| Authorization | Current right to invoke the exact service, release the exact data, or perform the requested effect | Answer correctness or broad permission inherited from membership |
| TrustProfile | Incarnation-bound behavioral evidence used by the existing autonomy resolver | A transferable team trust score or an authorization grant |
| AutonomyClass | Current action constraints and approval policy resolved for the acting Agent | A numeric score to average across members or a replacement for target grants |
| RuntimeApproval | Exact-action decision and existing protected-effect execution lifecycle | General approval of a conversation, a changed action, or a worker's later tool calls |
| Result validation | Trusted schema, provenance, completeness, and declared quality checks | Proof that a signed or schema-valid result is factually correct |

Resolve the acting Agent's effective class through the existing
[runtime autonomy machinery](../../../../internal/runtimeapproval/gate3.go) and
[incarnation-bound profile contract](../../../../api/v1alpha1/autonomy.go).
Do not implement a separate collaboration trust calculator. Required live
identity, policy, trust, and evidence checks fail closed when unavailable.
Retain the existing resolver's supported behavior when a profile is absent.

### Pinned, live, and retained state

| State class | Treatment |
| --- | --- |
| Workflow revision, admitted members and services, goal, role map, data bindings, and execution scope | Pin the accepted configuration and immutable identities. A change requires the reviewed revision/continuation procedure, not silent substitution. |
| Grants, executor validity, effective Agent autonomy, policy freshness, target readiness, deadline, and remaining capacity | Re-evaluate at the applicable new-effect boundary. A snapshot is not permanent permission. |
| Decisions, exact claims, dispatch admission, invocation identity, outcomes, and usage | Retain authoritative facts. Neither policy changes nor result expiry create a new execution right. |

Place immutable run/task bindings in trusted execution context at creation.
Keep dynamic eligibility and owner state in the gate. Do not repeatedly mutate
Agent definitions or approval-bound instance configuration merely to carry
progress metadata, and do not equate mutable status versions with a new workload
incarnation.

### Delegation permits specialization without transferring privilege

The operator-approved task contract limits the service operation, purpose,
recipients, permitted data, effect scope, deadline, and remaining allowance.
Each member then acts under its own grants and current autonomy constraints.
All applicable restrictions compose by intersection and budgets cannot expand.

Do not union the members' permissions into a team capability. Also do not require
every coordinator to possess every specialist's direct ToolGrant. The coordinator
needs authority to request the bounded specialist operation, and the specialist
needs its own authority for the effects that operation entails. The capability
to perform those effects is never returned to the coordinator.

For example, a reporting coordinator may request a scoped inventory analysis
from an operations specialist without receiving the specialist's infrastructure
credentials. That authorization does not permit a production deletion request.
A specialist's valid tool permissions do not authorize a task purpose or target
that the approved delegation contract excludes.

Enforce this distinction through trusted operation metadata and target/argument
constraints, not a model's explanation that an action is relevant. If the task
requires broader scope, stop for a separately authorized scope decision or new
run. An exact-action human approval cannot itself enlarge the authorization set.

### Checks for each operation

| Stage | Required checks and durable evidence |
| --- | --- |
| Interpret proposal | Supported structured operation/version, role, actual authenticated actor, parent task, activation, attempt, owner epoch, and exact operation ID |
| Establish task scope | Approved Workflow revision and execution binding, allowed member/interaction, permitted service operation and purpose, and no authority expansion |
| Authorize release | Declared data binding, current source restrictions and recipient access, qualified namespace policy, provenance, and content bounds |
| Resolve action policy | Actual acting member's effective AutonomyClass and applicable policies, target-specific authorization, and exact-action approval where required |
| Prepare effect | Stable request/target digest, parent reservation, plane receipt, and required durable audit evidence |
| Claim and admit | Existing plane owner, live actor/target/authority, activation eligibility, trusted time, then the final CommitDispatch fence |
| Retain outcome | Exact task/result binding, validated artifact references, reported usage, effect disposition, and idempotent audit/accounting settlement |

An A2A Agent Card, a claimed skill, successful discovery, model-selected routing,
or a valid result signature cannot skip a stage. Declaration or signature can
establish protocol metadata or origin, not permission or semantic safety.

### Approval and trust composition

- Workflow edge approval, RuntimeApproval, and AutonomyReview have distinct
   purposes. Edge approval opens a scheduled transition. RuntimeApproval concerns
   one protected action. AutonomyReview concerns a trust/autonomy change.
- A coordinator's approval to delegate to a specialist does not approve the
   specialist's tool calls, memory writes, or further delegations. Evaluate each
   actual actor at its enforcement boundary using the existing plane contract.
- Preserve task-level restrictions through delegation even when the recipient
   has a higher autonomy class. Do not derive a team class from the maximum,
   minimum, or average numeric levels. Different action types have different
   constraints and a single scalar cannot represent their intersection.
- A trust demotion triggers current-policy reevaluation before subsequent
   effects. If policy now requires approval, park the exact new action. If it
   denies execution, stop that branch's new effects. Do not silently substitute
   another member or identity to bypass the restriction.
- Preserve existing pending, rejected, expired, claimed, and terminal approval
   records. A policy or trust change uses the established stale-binding and
   reconciliation rules, not an edit that turns the same decision into approval.
   A new invocation ID or recipient cannot automatically evade a prior denial.
- Trust changes cannot erase a claimed effect, invalidate its factual outcome,
   or authorize replay. Required uncertain work blocks conclusive completion.
   Unaffected branches may continue only within their approved independent scope.
- TrustProfile writes and promotion evidence remain outside member control.
   Members cannot self-report an authority increase through a proposal, artifact,
   memory entry, or team-completion message.

### Non-Agent initiation approval bridge

The operator-bound executor has no Agent TrustProfile or AgentInstance. The
current Gate3 request and RuntimeApproval Agent references are Agent-specific.
Do not pass the executor or destination Agent as a fabricated source to satisfy
that interface, and do not treat interface incompatibility as permission to
bypass exact initiation governance.

Before enabling initiation, implement a distinct typed executor-action
authorization/review input under the approved execution binding. It must bind the
actual executor, original initiator, Workflow revision, target, action digest,
activation, and expiry. Reuse the established exact-effect decision, claim, audit,
and uncertainty principles without changing the meaning of legacy Agent fields.
The precise additive decision-record representation needs its own schema and
compatibility review. Until supported, initiation is an unsupported capability
and makes no target call. The destination's later actions still use its own
normal Agent-based autonomy resolver.

This is an implementation boundary within gate 2, not a second authority model
or a new accepted approval exception. Any proposed change to accepted action
ownership or policy semantics must follow the ADR/amendment process first.

### Evidence and adversarial content

- Record original initiator, actual acting member/executor, recipient, task
   ancestry, activation/epoch, policy and trust references, approval, effect
   ownership, dispatch admission, outcome, and accounting as distinct facts.
- Use existing normalized Trust Decision contracts for their supported planes.
   Define and version any additional collaboration event before making it input
   to trust scoring. A transport acceptance event is not successful task completion.
- Do not credit every member with the same successful action or score retries as
   additional successes. Trust and accounting consume stable invocation evidence.
- Treat peer text, descriptions, external documents, and artifacts as untrusted
   content. Only a schema-valid proposal from the authorized role can request a
   control operation, and trusted policy still decides whether it may proceed.
- Keep operator instructions separate from peer content. Do not use prompt
   instructions as the enforcement boundary, and do not capture hidden reasoning
   to justify audit completeness. Protect the bounded evidence actually needed.
- Human review previews use the bound target, permitted parameter view, purpose,
   source restrictions, and known uncertainty. Rendering a preview must not fetch
   remote content or invoke a model/tool. Approval of changed input needs a new
   exact operation, not mutation of the old approval.

### Downstream data use and memory

Releasing content to a member does not authorize every destination accessible to
that member. Propagate its trusted restrictions to model requests, tool inputs,
knowledge queries, memory operations, evaluation tasks, exports, and logs.
At each egress boundary, check the selected destination and permitted data use as
well as the member's capability grant. A model/provider change is not an implicit
authorization to disclose previously admitted context to that new destination.

Do not mount every member's memory into a shared context or infer MemoryGrant
from Workflow membership. Existing MemoryClaim access, lifetime, purpose, and
content restrictions remain applicable. Shared run memory, if explicitly
configured, requires each actual member's authorized access and separate write
provenance. Long-term storage of a short-lived peer result needs an authorized
retention purpose and cannot silently extend the approved content lifetime.

Memory and knowledge content remain untrusted input. Retrieval does not promote
stored instructions into operator policy, executable control operations, or
trusted behavioral evidence. Preserve contributor lineage when a retrieved value
is included in an artifact. Cryptographic integrity and semantic correctness
remain different properties.

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

The native configuration surface uses private ConfigMaps in an explicitly
configured operator namespace. Reserve the name prefix
`kognition-workflow-executor-binding-` and internal-record label value
`workflow-executor-binding`. These records are not public Kognition resource
kinds. Only authenticated members of separately configured operator groups can
create, update, or delete them. Reject configured executor subjects even when
they appear in an operator group. Missing protection configuration denies writes.
Inspect both current and prior metadata so removing the label cannot bypass the
guard. Namespace and actor come from authenticated admission context.
Persist the actual operator's mutation attempt before allowing the write. Missing
or unavailable audit durability denies administration.
Characterization must cover ordinary authors, executor substitution, changed or
removed labels, wrong namespace, deletion, and the configured operator path.
The validating webhook's installed coverage and RBAC remain qualification gates.

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

## Strategy Safety And Runtime State

The following elaborates gates 4 through 7 without introducing additional
strategies or a second execution authority. The four handlers propose bounded
operations against shared task, approval, effect, payload, and accounting stores.
Only trusted execution code commits state changes.

### Common task and operation contract

Each accepted child task records its goal/purpose binding, actual caller,
recipient and role, parent task, selected Agent/AgentService revisions, permitted
input/result bindings, required/optional disposition, deadlines, child allowance,
activation/attempt/epoch, and exact operation identity. A member can complete only
its addressed task. Its result cannot change the run's initial goal, members,
authority, owner, required-task set, or result-publication policy.

The structured `delegate`, `handoff`, `complete`, and `finalize` operations are
Kognition coordination payloads under the approved A2A extension. `complete`
reports a child task result. `finalize` requests activation-level completion.
Neither is an unmediated member write to WorkflowRun status. Authenticate the
role and validate the resulting artifacts before a conditional completion write.
Natural-language declarations such as "approved" or "task finished" have no
control-plane meaning.

Resolve the existing member aliases and optional `serviceRef` from the typed
[collaboration API](../../../../api/v1alpha1/workflow_collaboration.go). Do not
replace it with an untyped strategy configuration, a new membership resource,
or a model-supplied endpoint. Current in-progress API fields are not evidence
that execution, capability handling, or protected dataflow is implemented.

Do not treat a transport retry as a new task, an A2A context as shared authority,
or one remote server's task ID as a global identifier. Bind every inbound result
to its known sender and recorded target task. Unknown-origin, old-activation,
old-attempt, mismatched-context, or stale-owner results cannot complete new work.

### Scheduling and capacity

Represent child waits, human waits, retry timers, and capacity waits durably.
A2A submission returns an acknowledged task, not a blocking recursive call that
retains a database lock or a worker's only execution slot until every descendant
finishes. Release or suspend compute only at runtime-supported safe boundaries.
If the native adapter cannot safely support the required wait/resume semantics,
fail capability qualification rather than reconstructing context by replaying
unknown paid/model/tool effects.

Schedule against shared per-Agent capacity across runs. Account for a member
appearing in multiple tasks or approved aliases, and reserve enough capacity and
allowance for declared joins/finalization. A runnable task awaiting capacity is
not peer quiescence. Detect closed task-wait cycles separately from an active
remote task, human decision, or scheduled retry that can still make progress.

The approved eight-member and message/turn ceilings do not remove store limits.
Review worst-case effect-entry and encoded-byte demand against the bounded gate.
Expose any tighter admitted resource allowance before execution, and reject or
backpressure new work before its registration would exhaust maintenance space.
Never rotate to a fresh gate, reset counters, or evict unresolved entries to
manufacture capacity. A capacity failure must preserve earlier effects and their
settlement obligations.

### Supervisor

- The coordinator owns decomposition and proposes approved child requests.
   Specialists complete their addressed work and return scoped artifacts. A
   worker does not acquire coordinator authority by requesting delegation.
- Accept a delegation only when recipient, operation, input release, ancestry
   depth, capacity, and parent allocation are permitted. Requests to broadcast
   full history or reuse another member's credentials fail independently of the
   coordinator's trust score.
- Keep required tasks explicit in the trusted ledger. A failed required task
   cannot become optional because the coordinator omitted it from a summary.
- Reserve allowance for the coordinator's finalization work. Accept only the
   coordinator's authorized finalize proposal and declared result contract.

### Parallel

- Start only declared branches, each with its own session, data bindings, and
   child allowance. A branch's outputs and model context are not implicitly
   available to siblings.
- All required branches must succeed. Optional branch failure is a conclusive
   task disposition recorded in the manifest, not permission to discard uncertain
   effects, unresolved data disclosure, or unsettled required accounting.
- The join waits for the dispositions needed to publish a conclusive outcome.
   It can abort optional work not yet dispatched, but it cannot call an unknown
   remote outcome an optional failure in order to finish early.
- If configured, the synthesizer receives only approved branch inputs and
   finalizes the result under its own authority and approval requirements.
   Otherwise trusted join code produces the declared manifest without an
   undeclared model call. No any-success or quorum fallback is allowed.

### Handoff

- Retain one conversation owner and a persisted transfer identity. Bind the
   proposed recipient, filtered-context digest, source epoch, and proposed
   successor epoch. The recipient cannot acknowledge a different transfer or
   upgrade its authority during acceptance.
- Preparation is an explicitly authorized operation, not a maintenance bypass.
   If obtaining recipient acceptance requires A2A submission or another effect,
   admit it while the old gate is Active and record it before quiescing. After
   quiescing, only authorized observation of that known preparation may continue.
- The serving adapter must keep the recipient passive until transfer commit.
   A normal SendMessage that starts arbitrary recipient inference is not a
   non-executing readiness check. Reject unsupported preparation semantics.
- Quiesce the current owner, deny new old-epoch admission, and establish required
   prior effect and settlement outcomes. Claimed or dispatch-committed work does
   not disappear because an acknowledgement was received.
- Commit recipient identity, accepted context binding, successor epoch, and
   active-owner state in one conditional transition on the same activation gate.
   Cancellation racing that transition must win or lose on that gate. An accepted
   stop prevents recipient activation and remains visible to recovery.
- Preserve cumulative counters, all prior invocation/effect identities, and
   transfer history. Do not simulate handoff by creating a fresh activation gate
   with reset allowances or by overwriting the immutable participant definition.

The disabled foundation now includes the opt-in owner-transfer gate primitives
described in the [November 18 implementation update](#disabled-handoff-gate-extension-2026-11-18).
Authenticated orchestration, real passive-recipient preparation, the task/session
ledger, and all-plane enforcement remain unimplemented. A successfully drained
gate alone does not grant the next owner execution authority.

### Peer

- Members may request work only along approved interactions and within their
   addressed task scope. Discovery of a useful peer does not add it to membership
   or grant it the run's inputs. Do not introduce implicit shared history.
- Record initial tasks and their input recipients explicitly. The finalizer is
   the completion principal, not a reason to grant every peer the same context
   or to infer that all members are runnable at admission.
- Only the configured finalizer may propose activation completion. Trusted
   execution code verifies all required task dispositions, authorized artifacts,
   remaining effect obligations, and the finalizer's current role before the
   single conditional completion write.
- Quiescence with no runnable task, no pending human input or approval, and no
   acceptable final result is `PeerQuiescence`. Work awaiting a known remote task
   is not quiescent. Competing finalizer responses cannot produce two results.
- Voting, score averaging, and members endorsing each other's instructions do
   not supply permission or the approved completion condition.

### Completion and publication barrier

For every strategy, result readiness, task success, effect disposition, evidence
delivery, accounting settlement, and content release are separate facts.
Successful completion requires the declared result checks and known required
obligations. Optional branch semantics never authorize erasing uncertainty.
An independent evaluation, if declared, uses an authorized task and its own
reserved allowance. It is not an implicit free model invocation.

Use the approved outer mappings: success is `Ready`, failure is `Failed`, waits
and uncertainty stay nonterminal with conditions. `Terminated`/`Cancelled` still
needs the public-consumer compatibility review required by gate 15. Return an
explicit unsupported-capability or unavailable result when a required runtime,
authority, payload, or review path cannot enforce these rules.

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

1. Apply Accepted ADR-0029 and the approved gate resolutions. Detail the protected
   binding, typed executor-action review, lifecycle operations, and backend record
   schemas within the selected authority model and finite ceilings.
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

### Validation and implementation entry

Gate 1 requires invalid or unknown governance configuration to be rejected.
Schema pruning alone can discard a field before a validating webhook observes
it. Prove strict validation at supported authoring/API boundaries with negative
tests against the real API server, including misspelled strategy, authority,
and data-release fields. Document native-client behavior explicitly. Do not
describe a successful create after pruning as evidence of unknown-field rejection.
Any mechanism that requires different approved schema behavior goes through a
recorded gate amendment rather than a silent admission exception.

Preserve the approved implementation entry order: feature-branch work with
consent for moving existing edits, characterization first, then dependency-ordered
disabled implementation. This design task creates no branch, moves no existing
changes, modifies no accepted ADR, and enables no runtime. Source schemas in
progress are inputs to design review, not passing transport or security receipts.

### Governance acceptance scenarios

These are required behavioral proofs for the design above. Their presence in
this document does not mean they have passed. Extend shared fixtures and the
native-runtime qualification rather than substituting model self-evaluation.

| Scenario | Required observable outcome |
| --- | --- |
| Coordinator requests a valid specialist task without the specialist's direct ToolGrant | The exact delegated operation can proceed only with invocation authority and the specialist's own effect grants. No credential or executable tool capability returns to the coordinator. |
| Coordinator requests an out-of-scope operation from a highly trusted specialist | Reject before target contact despite valid specialist identity, a high trust score, or a persuasive rationale. |
| Specialist policy changes from autonomous to approval-required during a run | The next applicable action is parked or rejected under current policy. Existing decisions and effect identities are retained, and another member cannot transparently bypass the restriction. |
| Binding authorizer receives a non-Agent executor initiation | Use the reviewed typed executor path, or reject unsupported initiation. No fake AgentInstance, target-identity substitution, or skipped governance check is accepted. |
| Synthesizer rewrites a restricted worker result | The derived artifact retains inherited restrictions. Unauthorized outer publication, status/list/watch access, logs, model egress, and memory retention remain denied. |
| Optional branch loses acknowledgement after target dispatch | Preserve the uncertain operation and its accounting. The join cannot classify it as a harmless optional failure or report conclusive success. |
| Recipient acknowledges handoff while old work is unresolved | Recipient stays passive. Quiescence, drain, cancellation priority, and conditional owner transfer precede its first executable request. |
| Cancellation races recipient activation | One gate transition wins. A winning stop prevents activation, and a winning activation still preserves prior claims and the new epoch's cancellation obligations. |
| Peer is idle while a known remote task or human approval is outstanding | Keep the appropriate durable wait. Do not emit PeerQuiescence or a successful result. |
| Peers form a closed wait cycle or contend for one Agent's worker capacity | Apply the declared progress/deadline policy without holding recursive worker slots, resetting scope, or fabricating completion. |
| A repeated callback, old activation result, or duplicate transport send arrives | No duplicate completion, accounting record, trust credit, capability issuance, or target execution is created. |
| Content or replay evidence reaches its approved retention boundary | Apply gate 14's distinct retention rules after conclusive run end. Do not age out unresolved effects or turn missing content into permission to re-execute. |

The release matrix remains the approved native Go runtime, both stores, and A2A
1.0 gRPC polling profile, with all four strategies or none. The gate 13 SDK
compatibility spike precedes transport implementation. Independent security-owner
review and installation-specific enforcement evidence precede production use.

| Finding | Design disposition | Remaining evidence |
| --- | --- | --- |
| Initial executor authority | Operator-bound model and distinct initiation path accepted by the feature owner | Protected binding and typed executor-action decision representation, implementation, and authorization tests |
| Mixed-step data release | Explicit bindings, conservative lineage, restricted templates, and protected publication accepted by the feature owner | Binding/payload schema implementation and cross-surface content-access acceptance |
| Stop and handoff race | Typed activation state machine and PostgreSQL/Kubernetes persistence adapters implemented as an unwired foundation | Plane-claim verification, all-plane integration, durable effect recovery, record protection, real API-server acceptance, and installed enforcement |

### Standalone Execution Policy Foundation, 2026-11-20

The administrator-protected ControlPlaneRole API now accepts a separate
`executionRules` collection. Legacy RuntimeApproval rules retain their JSON
shape and cannot grant execution authority.

| Operation | Required exact scope |
| --- | --- |
| `Workflow/use` | Canonical principal, workspace, Workflow name and UID, lowercase SHA-256 revision digest |
| `Agent/invoke` | Canonical principal, workspace, Agent name and UID |

Execution rules use explicit `allow` or `deny`, default deny, and deny precedence
across matching roles. They reject wildcards, unknown operations, missing
incarnation bindings, and more than 128 rules per role. Every authorization reads
current roles and bindings. Policy-store failures and malformed execution rules
deny authorization rather than retain a previously successful result.

The existing separate OIDC administrator group protects creation and updates.
Runtime credentials and Workflow authors cannot administer these rules. These
permissions do not approve an exact action, choose an executor, authorize content
release, establish target readiness, or bypass the execution binding.

Race-enabled tests passed for `api/v1alpha1`, `internal/controlplaneauth`, and
`internal/apiserver` on the root Go 1.26.11 toolchain. The standalone authorizer
measured 97.6 percent statement coverage, with 100 percent for the new execution
methods and execution-rule validator. Tests cover API-managed permission updates,
revocation, immutable UIDs, revision changes, principal and namespace mismatch,
deny precedence, rule limits, malformed policy, and unavailable policy stores.
This evidence does not establish Kubernetes authorization or collaboration
execution. No dispatcher or gateway consumes these permissions yet.

### Disabled Executor Binding And Dispatch Foundation, 2026-11-20

The backend-neutral runtime-approval package now defines a typed
`WorkflowExecutorAction` and a distinct keyed action-binding format/domain.
Its exact binding includes canonical executor and initiator principals, Workflow
revision, protected binding reference/version, Agent and AgentService incarnations,
activation, attempt, owner epoch, operation, transient payload, and expiry.
Malformed identities, duplicate JSON keys, unbounded payloads, expired authority,
and substitutions are rejected. Retained verification supports evidence after
expiry and key rotation without granting new execution authority. A MAC binds
the action and does not authenticate the workload or establish current permission.

Existing Agent RuntimeApprovals can now carry an optional `workflowContext` inside
their existing action MAC. Legacy action vectors remain unchanged when absent.
Canonical and Kubernetes types, conversion, deep copies, and the checked-in CRD
preserve this context. Changed or removed activation context does not authenticate
as the original action. PostgreSQL and Kubernetes-adapter create/read/replay tests
preserve the context and reject a changed owner epoch.

`WorkflowDispatchGate` supplies the shared final dispatch primitive for the
existing RuntimeApproval Tool, Memory, and Communication planes. It requires a
retained exact approval, signed action, healthy clock, live eligibility evaluator,
registered effect, and rightful execution claimant. Only a new atomic
CommitDispatch grants one target call. It rechecks live eligibility and ownership
after commit and preserves committed state when the target is not called or its
acknowledgement is uncertain. It does not settle plane outcomes or accounting.

Gate3 rejects collaboration context without a configured dispatch boundary and
registers the signed effect before returning an executable Ready result. Actor
membership uses immutable Agent/identity/instance incarnations and session,
separately from the action's mutable configuration and authorization bindings.

Race tests cover concurrent dispatch, both stop orderings, pending approval after
stop, claimant replacement, revoked authority, unavailable dependencies, changed
payloads, and lost commit/target acknowledgements without replay. Nine affected
package suites passed with `-race`. The separate focused PostgreSQL persistence
test passed against its disposable database. The combined runtime-approval and
standalone-authorizer profile measured 87.8 percent statement coverage.

The platform chart was rebuilt and its complete source/notice parity check passed.
The full traceability run still reports an observability-preflight ordering failure
in pre-existing Makefile changes, which this implementation does not modify.

This is a disabled foundation. Production gateways do not consume the dispatch
primitive yet. Model and Knowledge enforcement, protected executor decision
persistence and initiation, payload release, conclusive settlement/recovery,
native A2A task execution, all four strategies, installed authorization evidence,
and independent security-owner approval remain outstanding. The Workflow planner
and Studio continue to reject collaboration execution.

### Executor-Action Schema Review Proposal, 2026-11-20

Status: historical schema-review proposal. The disabled representation described
below is implemented, as recorded in the subsequent progress section.
Independent schema and security acceptance remain outstanding.

Retain RuntimeApproval as the single exact-effect decision and execution owner.
Propose an additive actor discriminator on RuntimeApprovalSpec with a typed
`executorAction` binding. Legacy records without the discriminator keep their
Agent semantics. Executor records must never populate Agent, AgentIdentity,
AgentInstance, session, TrustProfile, or AutonomyClass fields with invented values.

| Binding | Required content |
| --- | --- |
| Principals | Actual canonical executor and original authenticated initiator, without credentials |
| Workflow | Namespace, name, immutable UID, and accepted revision digest |
| Execution authority | Protected operator binding identity and version, operation, finite scope, and expiry |
| Activation | Run UID, step, activation ID, attempt, and owner epoch |
| Exact action | Stable invocation ID, exact Agent and AgentService incarnations, and keyed canonical action binding |

Use a distinct typed executor-action evaluator rather than the Agent-based Gate3
request. The operator binding explicitly determines whether the exact initiation
requires review. An approved decision cannot expand Workflow-use, target-invoke,
content-release, deadline, or allowance restrictions. Recheck live authority after
review and immediately before claim and final dispatch admission.

Compatibility review must establish strict Agent/executor discrimination,
unchanged legacy serialization and action bindings, independent keyed binding
domains, protected administration and initiator evidence, durable decision audit,
claim ownership, replay denial, uncertainty, expiry, and guarded upgrade/rollback.
Both backends must pass the same decision and claim contracts. Existing approval
clients must reject unsupported executor records rather than treating absent
Agent fields as an authorization exemption.

Production initiation assembly and all-plane dispatch integration remain
implementation work. Production activation
also requires the complete native-runtime/backend/strategy matrix and independent
security-owner approval specified above.

### Disabled Executor Authorization Progress, 2026-11-21

The additive executor RuntimeApproval representation is implemented in canonical
and native types, keyed bindings, both-store decision/claim persistence, quota
ownership, and terminal tombstones. Legacy Agent records retain their existing
semantics. Implementation does not constitute the outstanding independent schema,
security, or installed-runtime acceptance.

The executor coordinator now composes current initiation policy with the retained
exact decision. An earlier NotRequired decision cannot dispatch after current
policy requires approval or denies execution. The live authority validator checks
the exact operator binding and run acceptance through dedicated reader interfaces,
including executor identity, target policy, bounded time, and cancellation.
PostgreSQL and native private binding and acceptance stores now exist. Generic
PostgreSQL resource, history, event, and watch methods reject their reserved kinds.
The authorizer also checks activation effect, byte, delegation, and expiry limits
against the exact authority and acceptance.

PostgreSQL derives the executor from trusted request context and reads current
exact Agent/invoke policy. Kubernetes uses a dedicated live resource reader,
dedicated-audience TokenReview, configured principal and UID, and named
Agent/invoke SubjectAccessReview. Both adapters require the exact ready native
Agent and pinned AgentService revision. Foreign targets require independent Agent
and AgentService reference grants in addition to named invocation permission.
Native authentication independently checks the ServiceAccount UID, bound-token
pod extras, and the uncached Ready pod UID. Both manager modes configure uncached
record readers. The main manager assembles explicit executor authentication from
the independently read pod and ServiceAccount identities and a bounded credential
file. Installed API-server tests now exercise real pod-bound tokens and named
authorization reviews. They do not establish deployed workload qualification.

`KOGNITION_WORKFLOW_EXECUTOR_PRINCIPALS` requires explicit canonical identities.
Standalone configuration requires enabled authentication and the configured OIDC
issuer. Native principals must identify exact valid ServiceAccounts. Native
binding administration additionally requires explicit
`KOGNITION_WORKFLOW_BINDING_ADMIN_GROUPS` without wildcard or broad authenticated
or ServiceAccount groups. An executor cannot administer bindings even when it
belongs to an operator group.

Native executor configuration requires `KOGNITION_WORKFLOW_EXECUTOR_POD_NAME` and
an absolute `KOGNITION_WORKFLOW_EXECUTOR_TOKEN_FILE` path. The file must contain a
pod-bound projected token for the `kognition.dev/workflow-executor` audience.
The verifier rereads the bounded file at each authentication boundary and checks
the actual TokenReview result. The opt-in Helm profile now mounts a rotating
600-second projected token and grants create-only TokenReview and
SubjectAccessReview permissions plus get of the named controller ServiceAccount.
Missing prerequisites fail closed. Standalone verified controller-incarnation
assembly remains unfinished.

The native profile remains off by default. It requires explicit
`k8sController.replicas >= 1`, `webhook.enabled=true`, `webhook.failurePolicy=Fail`,
`webhook.port=9443`, one exact controller TokenReview principal, and bounded
independent administrator groups. WorkflowRun admission and private execution
record admission route to a dedicated Service selecting the existing controller.
Ordinary resource admission retains its separate workload. The private-record
match condition covers protected names, generated names, and old/new labels.
Ordinary ConfigMaps do not depend on executor webhook availability, including
controller clock and key initialization. The state policy permits only tightly
shaped immutable submission and acceptance creates by the controller.

The cert-manager certificate includes the dedicated workflow-webhook Service
names. Operators supplying TLS themselves must include that Service's short,
namespace-qualified, `.svc`, and `.svc.cluster.local` DNS names in the certificate
SANs. These prerequisites do not enable collaboration or approve deployment.

Dispatch rechecks approval expiry and an explicit claimant lease before commit
and before target contact. Expiry after commit preserves the committed effect
without authorizing replay. Legacy claims without an explicit lease retain their
existing semantics. Focused regressions reproduced policy, time, and lease
bypasses before their fixes and passed afterward. Backend tests cover executor
substitution, shared-token rejection, live revocation, and changed target revisions.

The native binding configuration admission check now enforces operator groups,
executor exclusion, namespace/name integrity, prior-label protection, and durable
operator audit. Unconfigured protection or failed audit storage denies writes.
It validates the bounded binding payload and exact live Workflow definition and
is registered in both native manager modes. Protected native reads verify the
actual ConfigMap UID/version, hashed physical identity, payload shape, and
immutability. Unconfigured private writes remain denied. The configured native
path permits only immutable creates by the independently verified bound workload,
with signed submission evidence or exact acceptance derivation. Updates and
deletes remain denied, including operator writes.

Native mutation performs read-only submission preflight because kube-apiserver
clears caller-selected resource UIDs there. Validation seals a one-minute private
receipt against the actual server-assigned WorkflowRun UID, separately retaining
AdmissionUID and complete authenticated UserInfo. Public receipt annotations and
protected dry-run submission are rejected. Finalization independently rechecks
source Workflow/use, exact binding and run, current bound workload, and healthy
time. Retained initiator evidence does not become executor credentials.

The main manager assembles this service only with explicit executor configuration
and registers an acceptance-only WorkflowRun reconciler. The public reconciliation
regression verifies immutable acceptance persistence, unchanged public run state,
zero AgentJobs, and no repeat source authorization for retained acceptance after
source-use revocation. This reconciler does not dispatch collaboration effects.

Standalone submission commits the actual-UID public WorkflowRun and immutable
acceptance together in a PostgreSQL Serializable transaction. The final exact
Workflow/use check reads bounded live policy in that transaction. Unknown fields,
duplicate input, initiator substitution, and replay cannot create an acceptance.
Revoked submit permission leaves no public orphan. Final permission denial and
policy unavailability return HTTP 403 and 503 respectively. Retained initiator
attribution is not reused as credentials for accepted dispatch.

The standalone binding API independently reads the pinned live Workflow before
audit or persistence for enabled bindings. Its HTTP regression reproduced a
missing Workflow being accepted by a permissive storage backend. The corrected
boundary rejects missing, recreated, revision-changed, and digest-mismatched
definitions without binding or audit writes. Revocation remains available after
Workflow deletion and advances the retained disabled binding. The focused
regression and full API-server suite passed with race detection and standard vet.

Live initiation now resolves the exact declared collaboration step from the
pinned Workflow definition, derives its complete member set and finite limits,
and rechecks each member's exact Agent/service configuration digest, readiness,
reference grants, and invocation authority. Both stores connect this preflight
to the live authorizer. A selected step resolves its complete declared member set
within the Workflow-wide target allowlist. Targets belonging to another step do
not become members of the selected step. The composed-step regression proves
selected-target acceptance and other-step target denial. The full shared suite
passed in 4.800 seconds, focused native executor checks passed in 6.962 seconds,
and the real PostgreSQL protected executor regression passed in 80.160 seconds
with all four revocation/acceptance orderings. All used race detection and standard
vet. These checks do not qualify strategy execution. Member-to-member communication
and delegation integration remain unfinished.

Communication policy evaluation now validates the entire bounded policy set
before allowing delivery. Malformed JSON, duplicate or unknown fields, foreign
policy identities, invalid references, and excess data fail closed rather than
being skipped or hidden by an earlier allow. Evaluation requests at most 513
rows and rejects more than 512 policies, individual rows above 1 MiB, or total
data above 8 MiB. The reproducing isolation regression failed before the fix and
the focused communication-policy tests passed afterward with `-race`. Local
source wildcards still cannot authorize a foreign namesake without an explicit
namespace-qualified source and independent reference grant.

The earlier nine-suite runtime, authorization, admission, API, controller, webhook,
and command baseline passed with `-race`. The focused real PostgreSQL protected
record test also passed with `-race`, including authenticated HTTP capability
issuance, undeclared-step rejection, Pending review, durable decision audit,
actual preparation and claim, fresh dispatch, lost target acknowledgment, and
non-replay. Its HTTP authenticator, controller attestation, and target are
fixtures. This does not qualify a production A2A adapter.

The latest full native webhook, controller, controller command, and RuntimeApproval
suites passed with race detection and standard vet enabled. The real PostgreSQL
protected-record regression passed in 129.240 seconds, including all four actual
binding and Workflow/use revocation orderings against atomic run acceptance.
The tests synchronize on PostgreSQL's recursive lock graph. Revocation-first
leaves no public run or private acceptance, and acceptance-first preserves the
exact run UID and original initiator. Both deny new post-revocation submissions.
The standalone binding API regression passed in 5.314 seconds
and the full API-server suite passed in 7.399 seconds. The full chart regression
suite passed 546 checks, and the chart notice freshness check passed for six charts.

The installed native submission test uses a real kube-apiserver, etcd, chart CRDs,
HTTPS admission, API-issued bound JWTs, distinct ServiceAccount and pod UIDs, real
TokenReview/SAR, and executor-authenticated private record writes. It installs the
rendered chart state policy and private-record match condition. Under race it
proves signed receipt and exact acceptance persistence, protected dry-run without
evidence writes, operator create/update/delete denial, and rejection of wrong
audience, unbound tokens, and unready pods. With the private webhook unreachable,
ordinary bootstrap ConfigMaps succeed while protected names, labels, and generated
names fail closed. The expanded lifecycle passed in 23.097 seconds, and the full
native webhook integration suite passed in 29.036 seconds with race detection and
standard vet enabled.
The focused chart regression switch `-WorkflowExecutorOnly` passed 31 checks.
The original lifecycle authority, keys, clock, and root submitter remain fixtures.
Manually setting
a pod Ready in envtest does not prove scheduling, kubelet operation, deployed
token rotation, Service/TLS routing, or production runtime behavior.

Additional installed tests use API-issued operator and submitter certificates,
actual named Workflow/use permissions, operator-owned bindings, the pod-bound
executor, and durable ConfigMap audit. They reject forged operator attribution,
executor administration, physical deletion, and stale Workflow revisions.
Disabling a binding remains possible after source deletion, and retained audit
identifies the actual operator. Four channel-controlled cases prove that completed
binding or source-use revocation before acceptance CREATE prevents private
acceptance and new submissions. Completed acceptance before revocation retains
the exact run and original submitter. These native cases qualify those orderings
at the installed admission boundary, not cross-resource transaction atomicity.

Production code assembles native receipt acceptance but does not yet assemble an
executable collaboration path. Selected tasks 1 through 5 are not closed.
Standalone production controller attestation and initiation authorization are now
assembled under the explicit bound-token profile described below. Qualified A2A
transport, actual member interaction and delegation enforcement, remaining
revocation interleavings, cumulative accounting and retention, and deployed profile
qualification remain required. Protected dataflow, all-plane dispatch and settlement, native A2A task
execution, and Supervisor/Parallel/Handoff/Peer qualification are also unfinished.
Collaboration stays disabled in admission, planning, and Studio. No deployment,
activation, commit, or push is established by this evidence.

### Standalone Bound-Token Attestation And Initiation

- `KOGNITION_WORKFLOW_EXECUTOR_ATTESTATION_PROFILE=kubernetes-bound-token-v1`
   explicitly requires enabled OIDC authentication, the dedicated executor audience,
   exact workload principals, and an absolute explicit kubeconfig or in-cluster
   configuration. The adapter verifies the same token with TokenReview and an
   uncached Running/Ready Pod and ServiceAccount read. Their UIDs must be distinct.
   A private request-only callback revalidates the actual controller incarnation.
   Claims and ordinary authenticated callers cannot acquire this proof.
- An installed test verifies actual API-issued JWT signatures, audience and bound
   identity, distinct rotation, and retained readiness/cancellation denial. It passed
   in 24.466 seconds, and the full native webhook integration passed in 22.200 seconds
   with race detection and standard vet. Pod readiness remains manually supplied in
   envtest. This does not qualify deployed kubelet rotation, runtime, or TLS routing.
- Production initiation consumes that private proof and reuses the PostgreSQL live
   authority reader, RuntimeApproval owner, activation owner, and trusted clock. Clock
   reads have a five-second shutdown-cancelable bound. Approval expiry is clamped to
   the signed action's explicit expiry within the existing 24-hour ceiling. The
   default service stays unavailable. Configured issuance and resolution deny
   claim-only controllers, and target dispatch stays unavailable without qualified
   transport. The full auth suite passed in 5.750 seconds. The command, shared
   RuntimeApproval, and ordinary API suites passed in 4.629, 6.519, and 3.353 seconds
   respectively with race detection and standard vet. These results do not include
   the separate API PostgreSQL fanout integration fixture or independent approval.

### Unwired A2A Adapter And Task Observations, 2026-11-22

- The [executor adapter](../../../../internal/runtimeapproval/workflow_executor_a2a.go)
   uses the root dependency `github.com/a2aproject/a2a-go/v2@v2.9.0` for A2A 1.0
   gRPC SendMessage and GetTask. It requires explicit connection, authentication
   parameters, pinned Agent/service references, and task-journal callbacks. A fresh
   existing initiation commit is the only path to SendMessage. Raw JSON parts
   preserve the exact signed bytes, including integers above the protobuf numeric
   precision limit. A retained commit cannot resend.
- Credential acquisition is followed by re-verification of the exact initiation
   capability, original controller incarnation, live action/activation, current
   signed approval, exact committed effect and claim, fencing epoch, and deadline.
   Regressions reproduced target contact after credential-time revocation or stop
   and exposed a controller replacement missed by a plain current-identity read.
   The repair denies all five credential-time authority/claim changes before RPC
   and preserves the committed no-replay outcome. All sixteen adapter scenarios
   and the calibrated retry control passed. After formatting, full RuntimeApproval,
   ordinary API, command, and native-controller suites passed in 4.061, 5.555,
   5.636, and 28.316 seconds with race detection and standard vet.
- Fresh dispatch now transports the verified one-minute capability in the single
   `kognition-workflow-executor-initiation` metadata value. The receiving verifier
   bounds decoded content to 8 KiB and requires canonical base64 and typed JSON.
   Missing, duplicate, oversized, ambiguous, tampered, expired, or incorrectly
   bound evidence fails closed. Caller credentials and outgoing context cannot
   supply this header, and polling cannot reuse it. Twenty dispatch/poll scenarios
   and sixteen capability-verification cases pass, including the calibrated
   four-attempt ordinary SDK control and one-send lost-acknowledgment case.
- The read-only receiving resolver derives the action from the protected approval
   selected by exact workspace and invocation. It verifies the signed payload,
   pinned target/service, current executor/controller, live action/activation,
   and existing committed claim. It accepts only a new-task user message with
   one exact raw JSON part. Caller task/context references, metadata, extensions,
   filenames, push configuration, and unsigned response options cannot expand
   scope. It checks the payload bound before copying and returns independent
   payload bytes. It creates no task, effect, dispatch grant, or ownership transfer.
- Deterministic receiving regressions reproduced controller replacement and
   capability expiry during live authorization. The resolver now bounds those
   reads by the remaining capability lifetime and reverifies the capability after
   them. Both regressions pass after repair. All thirty-three receiving-scope
   cases pass, and the existing gRPC fixture checks the actual request decoded by
   the official SDK before task execution. This fixture still uses an in-memory
   task store and injected identity callbacks without qualified TLS peer identity.
- After formatting these changes, full RuntimeApproval, ordinary API, API-server
   command, and native-controller suites passed in 6.991, 6.287, 8.618, and 24.046
   seconds with race detection and standard vet. Editor diagnostics were clean.
   The earlier installed signed-token and bound-token attestation check also
   completed successfully. These checks do not qualify production receiving
   identity, durable runtime assembly, or deployment.
- SendMessage disables the gRPC retry buffer. The [behavior tests](../../../../internal/runtimeapproval/workflow_executor_a2a_test.go)
   include an independent control that makes four ordinary SDK calls under the
   configured retry policy. The gated adapter still makes exactly one call when
   acknowledgment is lost after receiving-side task creation. Reconnect recovery
   uses an authoritative retained task/context pair and GetTask only. Missing task
   identity and failed retention remain uncertain and do not authorize resend.
- Coordinator retention verifies the signed retained approval, exact action
   digest, activation binding, committed effect identity, and claim. It stores an
   immutable bounded observation on the existing communication effect. Observation
   cannot grant dispatch or transfer ownership. Registration reserves worst-case
   encoded space for the observation and lifecycle maintenance. Stop, settlement,
   and drain retain it. Caller snapshots do not share observation pointers.
- The shared PostgreSQL and Kubernetes-adapter contract verifies immutable
   observation, claim and scope mismatch denial, stale versions, non-replay, and
   retained evidence after drain. The latest combined real PostgreSQL protected
   executor and activation tests passed in 85.551 seconds, including all four
   revocation/acceptance orderings. PostgreSQL shut down cleanly. Kubernetes contract
   coverage uses a fake client and does not establish installed task persistence.
- After formatting, full store, RuntimeApproval, ordinary API, API-server command,
   and native-controller suites passed in 3.190, 6.105, 15.954, 5.464, and 19.557
   seconds respectively with race detection and standard vet. Editor diagnostics
   were clean. The Linux/amd64 source vulnerability scan covered 49 modules with
   Go 1.26.11 and reported no findings. The refreshed Go notice inventory records
   legal texts for 113 modules with zero unresolved texts. Distribution clearance
   and independent security-owner approval remain pending.
- This adapter is not wired to production dispatch, which remains unavailable.
   The sender-side fixture still uses its in-memory task store. The new private
   [PostgreSQL receiving store](../../../../internal/store/postgres/workflow_a2a_task_store.go)
   implements the official SDK task-store interface with an explicit exact-action
   scope, pinned authenticated-owner digest, initial invocation, byte/history bounds,
   full task state, strict row-version updates, immutable task/context identity,
   terminal-state protection, and retained deletion evidence. It creates no effect
   grant and rejects task listing. Generic CRUD, revisions, events, and watches
   cannot reach the private task kind.
- The [PostgreSQL receiving regressions](../../../../internal/store/postgres/runtime_approval_store_test.go)
   restart the actual SDK gRPC server and connection with a new store adapter.
   GetTask preserves task/context identity, artifact, completed status, and exact
   raw JSON payload without another execution. They also cover concurrent version
   updates, content/history bounds, scope mismatch, ownership loss before writes,
   cancellation, terminal immutability, and tombstone recreation denial. These
   tests and existing protected-record and activation contracts passed in 83.920
   seconds with race detection and standard vet. PostgreSQL stopped cleanly.
   After formatting, full RuntimeApproval, ordinary API, and API-server command
   suites passed in 3.445, 7.533, and 5.541 seconds. The receiving authenticator
   is injected test evidence and this store remains unwired.
- The [shared SDK task contract](../../../../internal/runtimeapproval/workflow_a2a_task_store.go)
   now owns task/scope validation, exact authenticated-owner checks, full bounded
   state, terminal protection, and optimistic SDK versions for both backends.
   PostgreSQL retains its private rows, advisory create locks, row locks, and
   database versions. The extraction passed all three PostgreSQL receiving tests
   in 90.272 seconds. The new [native adapter](../../../../internal/k8s/controller/workflow_a2a_task_store.go)
   uses strict private ConfigMap reads and resource-version CAS, reserves envelope
   capacity, and does not add a dispatch grant. Its tests synchronize both writers
   after reading the same ConfigMap version and require exactly one success and
   one conflict. Lost authorization before create or update leaves no task or
   version change. Malformed identity, version, label, extra/trailing content,
   foreign owners, and changed action/run scopes are denied.
- The [shared SDK restart test](../../../../internal/store/workflowa2atest/contract.go)
   now runs against both backends. Native reconstruction, ownership, synchronized
   CAS, and SDK server restart passed in 7.615 seconds. The same three PostgreSQL
   receiving tests passed in 79.991 seconds and PostgreSQL stopped cleanly. Each
   backend recovers exact task/context, completed status, artifact, and raw payload
   with one execution. Native persistence uses a fake client, not an installed
   receiving writer or a deployed workload.
- The shared history-integrity regression reproduced replacement of the original
   request payload and message context. Updates now snapshot the proposed task
   and reject truncation or modification of any retained history prefix. Valid
   appends remain supported, and subsequently appended messages are also protected.
   All five native receiving tests passed in 4.875 seconds. All four real
   PostgreSQL receiving tests passed in 109.595 seconds after formatting, including
   history protection and official SDK restart recovery. PostgreSQL stopped cleanly.
- An explicit duplicate-initiation regression reproduced two member executions
   after SDK server restart even though the durable store rejected the second
   task creation. The SDK invokes its executor before event-driven task creation.
   The unwired receiving handler now persists the initial scoped task before
   SDK dispatch, pins the protected owner and generated task/context, and checks
   the signed action again after creation and at executor entry. It forwards
   parent cancellation into the SDK's detached execution context. Creation or
   authorization failure never deletes evidence or authorizes resend.
- Seven real-resolver handler cases verify qualified execution and denial after
   post-create revocation, stop, controller replacement, expiry, claim replacement,
   or SDK task-load revocation. The task-load case first reproduced execution
   after revocation and passes with the executor-entry guard. Denied execution
   retains the initial submitted task and cannot replay after authority recovery.
   These cases use protected signed records with injected identity and persistence
   fixtures, not a qualified receiving workload or TLS identity.
- The shared SDK restart contract rejects wrong owners, foreign workspaces, and
   caller task/context continuation. Two independent handlers sharing one native
   record accept exactly one concurrent initiation and invoke the member once.
   Scoped polling bounds history, and cancellation, listing, streaming,
   subscription, push configuration, and extended cards fail closed. The original
   native duplicate regression passes after repair, the expanded native SDK checks
   pass in 4.234 seconds, and the preceding real PostgreSQL SDK restart checks
   passed in 92.603 seconds with clean database shutdown. After formatting the
   executor-entry and concurrency additions, full RuntimeApproval, native
   controller, ordinary API, and API-server command suites passed in 7.889,
   23.146, 5.369, and 5.153 seconds with race detection and standard vet. All four
   real PostgreSQL receiving tests passed in 76.947 seconds and PostgreSQL stopped
   cleanly. Runtime assembly and deployed qualification remain unfinished.
- The unwired scope factory derives activation, invocation, action digest, and
   owner from signed protected initiation evidence. Live protected authority
   supplies the task's message and byte bounds without caller-selected limits.
   Limits must match the accepted execution digest before and after live checks.
   Four protected-limit cases and seven scope cases pass. The receiving SDK
   fixture now uses the derived scope and authenticated task owner in nine cases,
   including post-create executor-authentication loss and limit changes. Each
   denied initiation retains its submitted record and rejects replay.
- The unwired task authenticator verifies the signed retained payload, exact
   protected owner and scope, current executor and approved controller, approved
   limits, and existing committed, uncertain, or settled effect. Twelve owner
   checks cover capability-free authentication after initiation expiry, payload
   snapshot isolation, changed scope, missing identity, and cancellation. A
   controller replacement during effect lookup first returned an owner in
   4.640 seconds. Rechecking live authority after lookup makes the same check
   pass in 6.965 seconds. It creates no grant and does not permit new dispatch.
   This authenticator still requires active execution authority and unexpired
   action evidence. It does not qualify stop, revoked-authority, or long-term
   maintenance recovery. All identity callbacks and persistence in this receiving
   SDK fixture are injected, not deployed receiver or TLS evidence. After
   formatting, full RuntimeApproval, native controller, ordinary API, and command
   suites passed in 9.661, 25.921, 6.106, and 4.626 seconds with race detection
   and standard vet.
- The unwired receiving assembly derives and authenticates the protected scope
   before constructing a backend. It rejects missing dependencies, backend errors,
   nil or mismatched stores, missing persistence, and authority revocation during
   construction. It enforces the protected authenticator even when the backend
   factory supplies another authenticator and reverifies scope afterward. Eight
   construction cases prove no task persistence or member execution occurs during
   assembly. A deadline regression rejected unbounded construction, and the repair
   bounds the factory by the signed initiation's remaining lifetime with an
   independent verified payload snapshot. Thirty-eight focused test events pass.
   The final formatted full RuntimeApproval suite passed in 8.754 seconds with
   race detection and standard vet. Native controller, ordinary API, and command
   consumers compiled successfully afterward. Those final compile checks ran no
   tests and do not replace the preceding full consumer results. The backend
   factory, runtime routing, installed writer, real receiving identity, TLS,
   extension enforcement, and maintenance recovery remain unqualified.
- Sender polling now reauthorizes the exact live activation after the SDK network
   response and confirms the original controller incarnation before returning
   task content. Three official gRPC SDK cases reproduced completed-task release
   after in-flight authority revocation, Workflow stop, and controller replacement
   in 4.820 seconds. The same cases passed in 5.072 seconds after the repair. Each
   denial follows exactly one poll and never resends initiation. Final formatted
   full RuntimeApproval, native controller, ordinary API, and command suites
   passed in 9.955, 25.140, 8.565, and 6.377 seconds with race detection and
   standard vet. This fixture uses injected authority
   and insecure loopback transport and does not qualify a deployed receiver,
   stopped-run recovery, or a new grant.
- The unwired polling reconstruction path accepts a retained protected scope and
   signed payload with fixed Agent and AgentService references. It authenticates
   the live source, exact owner, limits, and retained effect before opening the
   backend and again afterward. Construction has a five-second bound independent
   of initiation lifetime. A backend-supplied authenticator cannot replace the
   protected one. The resulting SDK handler has no delegate executor and rejects
   initiation, mutation, and unsupported methods while polling the same retained
   task and context after initiation capability expiry. A focused reconstruction
   check and eleven denial cases passed in 5.013 seconds. Scope, payload, owner,
   limits, foreign receiver, missing or malformed backend, and late revocation
   cannot change task evidence or bypass authentication. After formatting, full
   RuntimeApproval, native controller, ordinary API, and command suites passed in
   9.911, 26.957, 7.870, and 7.437 seconds with race detection and standard vet.
   The same reconstructed handler also passed an official A2A 1.0 gRPC client
   check without initiation metadata. Polling retained the exact task, context,
   state, and signed payload, and rejected initiation left the record unchanged.
   That focused wire check and the eleven denials passed in 4.755 seconds. The
   final formatted full RuntimeApproval suite passed in 9.589 seconds with race
   detection and standard vet. This fixture uses injected source authentication,
   in-memory persistence, and insecure loopback transport. It does not qualify a
   deployed receiving workload or TLS identity.
   This path still requires active authority and unexpired action evidence. It
   creates no new grant, does not discover tasks, and does not qualify stopped,
   revoked, long-term, or deployed receiving-runtime recovery.
- The expanded Linux/amd64 source vulnerability scan covered 106 modules and
   Go 1.26.11. It found zero reachable symbol vulnerabilities and zero imported
   package vulnerabilities. It reported the required-module advisory
   [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) for the unmaintained
   `golang.org/x/crypto/openpgp` package in `golang.org/x/crypto@v0.58.0`.
   Neither selected production package imports the affected package. This result
   does not establish that every dependency is clear or provide an independent
   security-owner disposition or distribution clearance.
- Native task request names, generated names, and old/new private labels are
   reserved by both the handler and Helm private-webhook selector. Unqualified
   task writes are denied even to operators. The installed API-server test verifies
   unavailable-webhook rejection and healthy deny-by-default enforcement; it passed
   in 25.061 seconds, including the four acceptance/revocation orderings. The
   controller create-only exemption is unchanged and grants no task write path.
   Full RuntimeApproval, native controller, ordinary API, command, and integration
   webhook suites passed in 6.463, 22.354, 5.976, 4.985, and 26.707 seconds after
   formatting. All 552 Helm regressions passed.
- Receiving stores and task scopes remain unwired. These results do not qualify
   installed native task persistence, production receiving-runtime recovery, workload authentication,
   TLS peer identity, required-extension enforcement, or deployed interoperability.
   Current live polling requires active authorization and does not qualify recovery
   across stop or uncertainty. Durable receiving runtime, all four strategies,
   actual member interaction/delegation, cumulative accounting, retention, and
   deployed qualification remain required. Tasks 1 through 5 remain open and
   collaboration stays disabled.

### Unwired Per-RPC Receiving And Retained Lookup

- Sender polling carries a canonical, bounded
   `kognition-workflow-executor-task-locator` containing the retained activation,
   action digest, invocation, task, and context. This untrusted descriptor selects
   a candidate record and grants no authority. The parser binds workspace and task
   to the SDK request, rejects ambiguous or noncanonical metadata, and returns a
   zero locator on error. Credential parameters and outgoing contexts cannot
   inject either protected action header. Eighteen parser cases and twenty-seven
   dispatch/poll scenarios passed in 8.012 seconds. The formatted locator change
   passed full RuntimeApproval, native controller, ordinary API, and command suites
   in 12.417, 23.148, 8.356, and 6.899 seconds with race detection and standard vet.
- The [per-RPC receiving handler](../../../../internal/runtimeapproval/workflow_executor_a2a.go)
   defers signed initiation construction until `SendMessage`. It requires genuine
   private live controller proof before approval or task lookup and preserves
   configured source restrictions at downstream receiving checks. `GetTask` has a
   five-second bound and rejects initiation metadata. It derives source, fixed
   Agent/AgentService, owner, activation and limits from protected approval and
   authority before reading a bounded candidate. After reading, it verifies the
   retained JSON payload against its signed binding, exact action digest, task and
   context, committed effect and current authority before returning content.
   Unsupported methods remain denied. Polling creates no initiation capability,
   new owner, task mutation or execution.
- The [official SDK receiving check](../../../../internal/runtimeapproval/workflow_executor_a2a_test.go)
   combines paired strict gRPC server options with the private workload-proof
   implementation, using injected token verification/TokenReview, fake Kubernetes
   objects, an in-memory durable-record fixture and insecure loopback transport.
   It sends the exact signed JSON once and polls the same completed task, artifact
   and context after initiation capability expiry. Ten denials cover ambiguous
   locators, foreign action/context, initiation on polling, revoked authority,
   stopped activation, revocation during read, tampered payload and an unready
   controller. Claim-only identities cannot reach the backend. Every denial has
   a restored successful control, and polling preserves record bytes with one
   create and one execution overall. The first test run exposed fixture state
   leakage because stopping also changes the fencing epoch. Restoring the whole
   activation repaired fixture isolation, not a production authorization defect.
   The focused check passed in 6.724 seconds after downstream source enforcement.
   Full formatted authentication, RuntimeApproval, native controller, ordinary API
   and command suites passed in 8.182, 14.830, 25.546, 8.583 and 7.686 seconds with
   race detection and standard vet.
- A separate invocation in the same SDK check revokes private Pod readiness during
   backend construction. Its already committed send fails before task creation or
   execution. After readiness restoration, signature verification succeeds and
   the retry reaches CommitDispatch, which rejects it without a fresh grant or
   second contact. The entire retained activation remains unchanged. The first
   follow-up failed in 6.284 seconds because the test incorrectly expected a
   successful retained-dispatch result instead of the gate's intentional conflict.
   Correcting that fixture expectation passed the same check in 9.011 seconds.
   The final formatted full RuntimeApproval suite passed in 12.627 seconds with
   race detection and standard vet. Production dispatch behavior was unchanged.
- The handler is not registered in production. Native task writes remain denied.
   This evidence does not qualify a real receiving workload, TLS identity,
   extensions, native member execution, stopped/revoked maintenance recovery or
   deployed interoperability. Active authority and unexpired action evidence
   remain required. Independent security-owner approval, strategies, cumulative
   accounting and retention remain unfinished. Tasks 1 through 5 remain open and
   collaboration stays disabled.

### Disabled Dedicated Receiver Design

Status: draft for review. The user authorized preparation of this disabled design
after read-only discovery of existing receiving candidates. This is not approval
of a principal, RBAC grant, schema amendment, deployment or activation. The
accepted runtime profile and independent security-owner requirements are unchanged.

Read-only discovery used context `kognition` and the running cluster `kognition`
in resource group `rg-kognition-aks-eus2`. The initial DNS failure was transient
and its cause is unestablished. No cluster, DNS or credential configuration changed.

| Existing candidate in `kognition-work` | Discovered immutable service identity | Current Deployment |
| --- | --- | --- |
| `research-service`, referencing `research-agent` | `ea6d3f76-6d91-4e1d-9ede-6cf6aec667a8` | `agent-research-agent`, two available replicas |
| `summarizer-service`, referencing `summarizer-agent` | `bc0ee5ce-8ee1-476b-a149-7c36b16cb1ae` | `agent-summarizer-agent`, one available replica |

Both Deployment templates omit `serviceAccountName`, selecting the namespace's
shared `default` identity, and expose port 8080. Both reference runtime image
digest `sha256:41a1e0c40fc3d5a6f08f8c6639bc53a9b70bd7bcc63b56a5711567b78695b3bc`.
These observations establish candidates, not approved receiver or writer scope.
The [AgentService schema](../../../../api/v1alpha1/service.go) defines a port and
status endpoint, without the queried `spec.type` or `spec.protocol` fields.
Their absent values therefore establish no transport capability. The checked-in
[worker Dockerfile](../../../../runtime/agent/Dockerfile) starts Python, including
its dependency profile named `native`. Image-to-source provenance and the deployed
entrypoint were not established. Neither Ready status nor port 8080 qualifies
native Go execution or A2A 1.0 over gRPC.

Proposed enforcement responsibilities:

| Boundary | Required implementation before qualification |
| --- | --- |
| Dedicated receiver | One independently bound native Go receiving identity per admitted Agent/AgentService. Preserve the existing services and Deployments. Do not grant the shared default identity protected task access. Resolve and approve actual Agent, service, ServiceAccount and Pod UIDs before wiring. |
| Member provenance | Use the genuine supported AgentInstance lifecycle and approved Agent/runtime binding. A dedicated transport process cannot manufacture an AgentInstance, relabel a Workflow executor, or qualify an existing Python worker as native Go. |
| Protected writer | A separate control-plane task writer authenticates each receiver and independently validates the proposed operation against protected approval, accepted run, original owner, target and committed effect. Receiver pods hold no writer signing keys and receive no direct native task-record mutation permissions. Protect writer Pod creation, token issuance, exec and key custody from member-controlled namespaces and identities. Do not forward initiator or controller bearer credentials to the writer. This writer is not another exact-effect owner. |
| Native admission | Verify a bounded, versioned, domain-separated mutation receipt issued only after writer authorization. Bind namespace, exact record name, CREATE or UPDATE, body digest, original scope, actual writer ServiceAccount UID and live Pod UID. UPDATE also binds the prior record UID, resourceVersion and content digest. Missing or invalid receipts deny. |
| Mutation integrity | CREATE retains the exact signed initial payload before execution. UPDATE preserves scope, invocation, task, context, original history prefix and terminal outcome, with the existing UID/resourceVersion CAS. Admission validates old and proposed state independently and rejects replay onto another record or version. Receipt metadata must have one reviewed representation compatible with strict record decoding. |
| Permission isolation | Ordinary receivers, operators and legacy controllers remain denied on protected task writes. Kubernetes RBAC cannot restrict CREATE by `resourceNames`. Bound the writer's namespace and verbs, and make its admission guard also deny unrelated ConfigMap writes by that writer. No wildcard grants, DELETE or generic controller exemption. |
| Serving and recovery | Register only qualified authenticated unary SendMessage/GetTask with verified receiving identity, TLS peers and required extensions. Preserve capability-free polling, current disclosure authorization and no-resend semantics. Stopped, revoked and uncertain maintenance needs a separately qualified read-only existing-owner path, not fresh initiation authority. |

Exact resource names, API shapes, signing-key custody and receipt encoding remain
review inputs. Define the digest's exact versioned input, including raw task bytes
and security-relevant metadata, while excluding only the designated receipt field
and explicitly allowed API-assigned metadata. Bound receipt expiry and reject
unknown fields or alternate encodings. Avoid self-referential signatures and
account for API defaulting without allowing scope or payload substitution.
No key, ServiceAccount, RoleBinding, listener, chart default or live
resource is created by this draft. The current
[native adapter](../../../../internal/k8s/controller/workflow_a2a_task_store.go)
has no task mutation receipt persistence or admission integration, and installed
[admission](../../../../internal/k8s/webhook/webhook.go) still denies native task
writes unconditionally. The submission writer's CREATE-only receipt exception
does not authorize receiving task CREATE or UPDATE. Persistence for PostgreSQL
must enforce equivalent ownership and mutation rules without a Kubernetes receipt.

Qualification remains blocked until these checks pass:

1. Record approved receiver and writer bindings, native image provenance,
   genuine AgentInstance registration, receipt schema and independent review.
   Changes to an accepted gate require a recorded amendment before implementation.
2. Exercise actual API-signed receiver and writer Pod credentials, rotation,
   wrong audience, wrong ServiceAccount UID, Pod replacement, deletion and readiness
   loss, including revocation during authorization and admission.
3. Prove one authorized initial CREATE and legal owner UPDATE through installed
   admission. Reject unsigned writes, default identity, operator or controller
   spoofing, foreign scope, altered payload/history/context, changed record UID,
   stale version and replay. Concurrent updates require one success and one CAS
   conflict, with retained bytes unchanged by every denied write.
4. Prove both backend restart and lost-acknowledgement behavior through deployed
   official SDK gRPC with real TLS and extensions. A committed send denied before
   persistence remains committed and cannot cause another contact or execution.
   Observation and maintenance cannot create a new owner or dispatch grant.
5. Qualify genuine member interactions and delegation, all four strategies,
   cumulative accounting and the approved 7-day content and 90-day replay retention,
   then obtain independent security-owner and operator activation approval.

These are acceptance requirements, not new passing evidence. Tasks 1 through 5
remain open and collaboration stays disabled. The existing denial and no-resend
regressions remain authoritative while this proposal awaits review.

The [draft security assessment](security-review-architecture.md#dedicated-receiver-draft-security-assessment)
records open trust-boundary risks. The
[Kubernetes RBAC reference](https://kubernetes.io/docs/reference/access-authn-authz/rbac/#referring-to-resources)
confirms the CREATE limitation, and the
[AKS identity concepts](https://learn.microsoft.com/en-us/azure/aks/concepts-identity)
separate Kubernetes authorization from Azure resource and workload identity.
Successful cluster discovery establishes none of those permission grants.

### Approved Task CREATE Replay Guard Amendment

- Status: approved by the feature owner in this implementation conversation.
- Scope: disabled writer/admission implementation. Installation and independent
   security-owner/operator activation approvals remain separate.
- Store one bounded, single-use CREATE admission marker on the existing committed
   communication effect. Bind its exact task observation and receipt digest to the
   existing claim, owner, action and activation epoch. Consume it with the existing
   activation-version CAS before admitting the native task CREATE.
- A consumed marker denies every subsequent CREATE, including identical receipt
   replay, receipt replacement and record recreation. Missing acknowledgement or
   a crash after marker consumption leaves the committed outcome unresolved.
   Preserve evidence and do not resend or create another owner.
- Marker admission grants no dispatch, target contact, owner transfer or effect
   registration. Reject uncommitted, stopped, expired, foreign and stale-claim
   attempts without modifying the activation. Keep the marker through settlement
   and the approved replay-retention lifetime.
- Qualification requires actual installed positive writer admission in addition
   to store-level single-use and concurrent-CAS tests. Task UPDATE continues to
   require fresh owner authorization, signed old-object integrity and actual
   prior UID/resourceVersion binding.

### Isolated Native Task Mutation Receipt Contract

The user authorized this isolated contract implementation. It does not approve
principal bindings, RBAC grants, an admission exemption, deployment or activation.
The unwired [receipt service](../../../../internal/runtimeapproval/workflow_a2a_task_receipt.go)
requires explicit authorization callbacks before and after key reads, a healthy
clock and a five-second request bound. Its canonical receipt is at most 8 KiB,
uses a separate HMAC domain and expires within one minute. It binds exact mutation
scope, operation, task/context, body digest and distinct dedicated writer
ServiceAccount and Pod UIDs. UPDATE also binds prior UID, resourceVersion and digest.

- Record derivation bounds and validates SDK task bytes before producing a
   mutation. The SHA-256 input is the UTF-8 domain
   `kognition.dev/workflow-a2a/task-record/native/v1`, one NUL byte and the exact
   raw record bytes. Unknown, duplicate and alternate fields deny. CREATE requires
   version one, Submitted state and one initial history message.
- Derivation and the shared task store reuse the same transition checks for
   exact scope/task/context, version increment, immutable history prefix and
   terminal-state protection. Invalid input returns no mutation evidence.
- The two focused [receipt regressions](../../../../internal/runtimeapproval/workflow_executor_a2a_test.go)
   initially pass in 3.016 seconds after correcting the SDK ContentParts fixture
   type. That compile failure was not a behavioral RED. Independent byte snapshots
   now verify both digest input and retained-input preservation. The strengthened
   focused tests pass in 5.231 seconds, followed by the formatted full
   RuntimeApproval suite in 16.377 seconds. All five native receiving adapter tests
   pass in 3.411 seconds and all four real PostgreSQL receiving tests pass in
   94.237 seconds with clean fixture shutdown. Race detection and standard vet
   are enabled. The native adapter uses fake Kubernetes persistence, and the
   shared SDK restart fixtures use injected authentication and loopback transport.

Authorization in these receipt tests is injected. Record derivation grants no
authority and does not authenticate the original signed payload or full ConfigMap
metadata. The digest is a validated raw-record primitive, not the reviewed complete
ConfigMap receipt representation. Live writer authorization, key custody, metadata
binding and installed admission remain unfinished. Native task writes remain
unconditionally denied. Tasks 1 through 5 remain open and collaboration stays disabled.

### Proposed Full ConfigMap Receipt Representation

Status: unwired source implementation under the user's subsequent request to
finalize implementation and prepare the existing AKS installation. Independent
security review, installed full-object acceptance and activation approval remain
open. This source work adds no principal, RBAC grant, admission exception or
deployed resource, and does not amend an accepted execution gate.

Use annotation `kognition.dev/workflow-a2a-task-mutation-receipt` as the sole
receipt location. Its value is the canonical receipt JSON string, without base64
or additional whitespace, bounded to 8 KiB after UTF-8 decoding. The receipt
annotation is excluded from the object digest to avoid a self-referential
signature. No other annotation is permitted on proposed task records.

The proposed full-object receipt uses format
`kognition-native-workflow-a2a-task-mutation-v2` and HMAC domain
`kognition.dev/workflow-a2a/task-mutation/native/v2`. Keep every existing mutation
field and add `mutation.configMapDigest`. UPDATE also adds
`mutation.prior.configMapDigest` alongside its existing UID, resourceVersion and
body digest. The unwired v1 receipt remains a raw-record component and must never
authorize full-object admission. Mixing v1/v2 fields or accepting v1 as a fallback
denies. Existing authorization, key validation and one-minute expiry rules remain.

Derive the digest envelope below from the actual admission object, not a caller's
digest or supplied envelope. Placeholders show values, not new resource names.

```json
{
  "apiVersion": "v1",
  "kind": "ConfigMap",
  "namespace": "receiving",
  "name": "kognition-workflow-a2a-task-<existing-scope-hash>",
  "labels": {
    "kognition.dev/internal-record": "workflow-a2a-task"
  },
  "immutable": false,
  "dataKey": "task.json",
  "taskRecordDigest": "<existing domain-separated exact-byte body digest>"
}
```

Canonicalize this typed envelope with the existing `CanonicalizeJSON` algorithm,
with a 1 KiB encoded bound. `configMapDigest` is lowercase hexadecimal SHA-256 of
the UTF-8 domain `kognition.dev/workflow-a2a/task-configmap/native/v1`, one NUL byte
and the canonical envelope bytes. The existing raw-record digest binds exact
`task.json` bytes, so the envelope does not copy or double-encode the whole task.
Validate the raw record and its transition before constructing this envelope.

| Object field | Required representation or denial rule |
| --- | --- |
| API version, kind, namespace and name | Exactly v1/ConfigMap and the protected scope's namespace and existing record-name derivation. No generateName, foreign namespace or caller-selected alias. |
| Labels | Exactly one label, `kognition.dev/internal-record=workflow-a2a-task`. No additional labels. |
| Annotations | Exactly the designated receipt annotation on the proposed write. An old persisted object must also have a retained receipt of the same full-object format. |
| Data and BinaryData | Exactly one UTF-8 text entry, `task.json`. No BinaryData or other text keys. Preserve the existing raw-record and task/history bounds. UTF-8 data-key plus value bytes must also fit the native 1 MiB ConfigMap data limit. The receipt's separate annotation budget does not consume or enlarge the existing 4096-byte task-envelope reserve. |
| Immutable | Nil and explicit false normalize to false. True denies. |
| Owner references, finalizers and deletion fields | Empty owner references and finalizers, with no deletion timestamp or deletion grace period. Unexpected values deny rather than disappear from the digest. |
| UID, resourceVersion, creationTimestamp and managedFields | Excluded from the digest as API-maintained fields. CREATE uses the actual admission operation with no prior object. UPDATE requires nonempty old UID/resourceVersion and exact equality in the proposed object, and the new receipt binds those old values explicitly. Never use field-manager names or timestamps as authority. |
| Unknown or alternate fields | Reject duplicate JSON fields, unknown fields, alternate receipt encodings and unreviewed security-relevant metadata. Admission validation precedes projection into the typed envelope. |

For UPDATE, derive the old envelope and both old digests from the actual
`AdmissionRequest.oldObject` and compare them with the new receipt's `prior` fields.
Preserve old UID and resourceVersion in the proposed write for real API CAS.
Validate the retained old receipt's signature and object binding without treating
its expired per-write authorization as fresh authority. The new UPDATE receipt
requires current live original-owner and writer authorization and an unexpired
per-write lifetime. Neither old nor new receipt authorizes execution or resend.

The writer must derive CREATE/UPDATE from protected state and actual authenticated
writer credentials. Admission independently derives the expected mutation from
the real object, prior object and live verified writer, then compares every field
before accepting. User-supplied writer identity, operation, scope or digests are
never trusted. Concurrent CREATE retains one record, and concurrent UPDATE permits
one CAS success. A receipt does not permit DELETE, recreation after retained replay
evidence, or unrelated ConfigMap writes by the writer.

Review gates before admission wiring or deployment:

1. Independently approve the annotation, separate v2 receipt fields/domains, metadata whitelist,
   default normalization, byte budgets and old-receipt verification semantics.
2. Confirm validating-admission API-assigned metadata behavior and test exact
   oldObject/proposed UID/resourceVersion handling through the actual API server.
3. Prove altered labels, annotations, immutable state, owner references, finalizers,
   data keys, scope and prior digests all deny without changing retained bytes.
4. Prove duplicate/unknown raw fields deny before typed projection, and receipt
   replay, expired UPDATE, writer replacement and concurrent CAS outcomes fail closed.
5. Retain live writer/key-custody, installed admission, both-backend qualification
   and independent security-owner/operator approval as separate open requirements.

### Full-Object Receipt Source Evidence and AKS Hold

- [Full-object derivation and verification](../../../../internal/runtimeapproval/workflow_a2a_task_configmap.go)
   now implement the representation above behind the disabled boundary. Derivation
   accepts an absent receipt only while preparing unsigned proposed bytes. Final
   verification requires a canonical v2 receipt on the proposed object and a
   signed, object-bound v2 retained receipt for UPDATE. No v1 admission fallback
   exists. Raw ConfigMap JSON is bounded to 3 MiB, native data to 1 MiB, the digest
   envelope to 1 KiB and the receipt to 8 KiB. Existing task/history bounds remain.
- [Receipt behavior checks](../../../../internal/runtimeapproval/workflow_executor_a2a_test.go)
   verify the independently specified canonical envelope, nil/false normalization,
   metadata and raw-field tampering, annotation bounds, v1 fallback denial,
   prior UID/resourceVersion and digest conflicts, forged retained signatures,
   writer replacement, key-read revocation and expired fresh UPDATE denial.
   An expired retained receipt remains integrity evidence only. Verification
   preserves the independently captured initial, retained and proposed bytes.
- Final formatted checks passed on Go `1.26.11` with `-race`, `-p 1` and standard
   vet: the full RuntimeApproval suite in 13.481 seconds and all five native A2A
   adapter tests in 11.292 seconds. The two installed API-server admission tests
   passed in 25.838 seconds and confirm the existing unconditional native task
   denial. All four real PostgreSQL receiving tests passed in 102.898 seconds,
   including SDK restart, history protection and concurrent-version checks. The
   embedded server shut down cleanly and released its fixture listener. These
   checks do not prove positive v2 admission or deployed receiver qualification.
- Read-only revalidation in AKS context `kognition` still finds
   `agent-research-agent` and `agent-summarizer-agent` in `kognition-work` without
   an explicit ServiceAccount. Both retain runtime image digest
   `sha256:41a1e0c40fc3d5a6f08f8c6639bc53a9b70bd7bcc63b56a5711567b78695b3bc`.
   Availability is not identity or native A2A conformance evidence. No cluster
   resource, credential or installation mechanism was changed.
- Source security assessment finds bounded structured decoding, authenticated
   digest/domain separation and revalidation around external key reads. The
   authorization callback remains injected, and full-object API defaulting,
   genuine receiving writer proof, signing-key custody, TLS/extensions, runtime
   member execution, recovery, four strategies, accounting and retention require
   qualification. Azure and GitHub security-alert tools were unavailable, so no
   live cloud compliance or remote security clearance is claimed.

Native task writes remain denied. Tasks 1 through 5 remain open, and collaboration
stays disabled. Independent security-owner and operator activation approvals
cannot be supplied by the implementation agent.

### Writer Admission Remediation Evidence, 2026-11-22

- The user-approved CREATE marker is now tested through the shared activation
   contract on both real PostgreSQL and a real Kubernetes API server. Independent
   clients observe retained admission evidence across settlement, rejected replay
   and exactly one successful concurrent consumption. The marker grants neither
   dispatch nor a new owner. This is cross-client persistence evidence, not a
   process-restart qualification.
- Case-sensitive full-object parsing rejects ambiguous ConfigMap fields. A
   subsequent public writer regression exposed canonical integer exponents in
   receipts. Bounded exact expansion repairs decoding without changing signed
   bytes, domains, scope or integer limits. Fractional and overflowing inputs deny.
- The source writer and actual-request admission authorizer use distinct
   writer/receiver identities, retained original-owner authority and disjoint
   key material. Public tests cover signed CREATE, legal UPDATE after old-receipt
   expiry, non-consuming preflight, lost-acknowledgement no-resend and unchanged
   bytes on denied writes. Credential and admission fixtures do not establish
   deployed key custody or positive installed task-writer admission.
- The [review follow-up](security-review-code.md#implementation-follow-up)
   records the latest source, real API-server CAS and real PostgreSQL checks.
   Authenticated transport/startup, positive installed task-writer admission,
   all-writer ConfigMap admission selection, deployment custody, native receiver
   provenance and independent activation approvals remain required. Default task
   denial and disabled collaboration remain in effect on the installation.
   Tasks 1 through 5 remain open. No AKS resource changed.

### Unwired gRPC Caller Admission, 2026-11-22

- The opt-in [gRPC boundary](../../../../internal/auth/grpc.go) admits only
   `SendMessage` and `GetTask` with exactly one bounded bearer credential and
   a live private controller-incarnation proof. Authentication uses the same
   signed OIDC token for dedicated-audience TokenReview and uncached Pod and
   ServiceAccount checks. Missing authentication fails closed, claim-only identity
   is insufficient, and authentication diagnostics do not escape. A five-second
   authentication bound does not replace the original task request context.
- Paired server options install both unary authentication and unconditional
   streaming denial. An official SDK wire regression first reproduced anonymous
   streaming execution with a unary-only guard in 3.320 seconds. The same check
   passed in 4.667 seconds after adding the stream interceptor. The ordinary
   permissive gRPC interceptor is unchanged.
- The [authentication checks](../../../../internal/auth/grpc_test.go) include
   25 live-proof and credential cases. Three added cases reproduced execution with
   vertical-tab, DEL, or Unicode whitespace padding in 4.615 seconds. Standard
   Unicode whitespace and control classification makes the same cases pass in
   7.131 seconds. The official SDK fixture also verifies authenticated send and
   capability-free polling, exact task identity, zero execution on rejected
   credentials, unsupported methods, and polling denial after controller
   revocation. It uses injected authentication, insecure loopback transport, and
   the SDK's in-memory task store, not a qualified receiving deployment.
- The existing [installed token lifecycle](../../../../internal/k8s/webhook/envtest_integration_test.go)
   now applies the unary boundary to actual API-signed bound JWTs for both methods.
   It preserves the distinct live Pod UID and ServiceAccount UID, accepts real
   token rotation, rejects API-audience and unbound tokens, and rejects both
   original and rotated credentials after readiness revocation with zero handler
   calls. The focused installed lifecycle passed in 25.125 seconds. Full
   authentication, RuntimeApproval, ordinary API, command, and installed webhook
   suites passed in 6.818, 11.950, 7.153, 16.011, and 25.905 seconds after initial
   formatting with race detection and standard vet. After the credential repair,
   all five full suites passed in 6.612, 10.822, 5.622, 5.891, and 26.525 seconds
   with race detection and standard vet. The final command completed and its
   terminal was cleaned up.
- These options are unwired and authenticate callers only. They do not grant
   task ownership or qualify receiving workload identity, TLS, native task writes,
   required extensions, stopped or revoked maintenance recovery, runtime routing,
   member execution, strategies, accounting, retention, or deployment. Independent
   security-owner approval is absent. Tasks 1 through 5 remain open and the
   collaboration profile remains disabled.

### Disabled persistence foundation, 2026-11-16

- [Typed gate contract and transitions](../../../../internal/store/workflow_activation.go)
   bind run UID, step activation, attempt, actor digest, effect digest, and exact
   plane-claim reference. Expected-version mutations serialize transitions.
- Only a newly persisted CommitDispatch returns `DispatchGranted`. Matching
   retained commits return observation without permission to contact the target.
   A lost acknowledgement must therefore preserve uncertainty rather than grant
   replay. This store does not itself authenticate a plane claim or caller.
- In the original November 16 foundation, stop and quiescence advanced the owner
   epoch, unknown outcomes blocked drain, and explicit resolution retained a proof
   digest. That original slice had no recipient-activation method. The opt-in
   [November 18 extension](#disabled-handoff-gate-extension-2026-11-18) adds a
   guarded ownership transition without changing effect ownership or replay.
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

### Disabled handoff gate extension, 2026-11-18

- An activation can opt into single-owner behavior through `InitialOwner` and
   a finite `MaxHandoffs`. The owner must belong to the immutable admitted actor
   set. Existing records without these fields retain their multi-actor behavior.
- `ProposeHandoff` records the source owner and epoch, recipient, filtered-context
   digest, and stable transfer ID. Only one unfinished proposal is retained at a
   time. Reusing an ID with different bindings conflicts.
- `AcceptHandoff` records the exact recipient acknowledgement digest while the
   source is still active. Acknowledgement does not activate the recipient or
   grant permission to execute a target operation.
- The source may quiesce only its accepted proposal. `ActivateHandoff` requires
   the exact source, recipient, context and acknowledgement, a quiescing/drained
   gate, a live deadline, and conclusive settlement of all retained effects.
   The conditional write changes the owner without replacing the activation,
   removing effects, resetting limits, or discarding prior transfer records.
- New registration and dispatch require the current owner for an opted-in gate.
   An old owner cannot borrow the successor epoch. A retained successful transfer
   returns observation only, not a second `OwnerActivated` or dispatch grant.
- Cancellation supersedes quiescence even if the old handoff was already drained.
   It advances the fence, retains the original `QuiesceID`, and prevents that
   handoff from controlling drain or activating the recipient. Unknown effects
   remain explicitly uncertain. A matching cancellation retry is a no-op.
- Encoded-capacity checks reserve space for bounded transfer history and both
   lifecycle IDs before accepting effects. Transfer history consumes the admitted
   finite allowance rather than creating fresh scope after each ownership change.
- [Core handoff tests](../../../../internal/store/workflow_handoff_test.go) cover
   passive acknowledgement, unsettled-effect denial, old-owner denial, exact
   recipient/context checks, cancellation, expiry, retained history, and limits.
   The shared store suite additionally exercises cancel-before-transfer,
   transfer-before-cancel, and concurrent cancellation/activation and drain races.

These are trusted-store primitives, not new authenticated endpoints. The gate
does not validate workload credentials, issue A2A capabilities, retrieve protected
context, or authenticate a supplied plane claim. The typed executor/member
boundary and all-plane integration must do that before these operations are
reachable. No controller, gateway, installation policy, or public feature switch
is enabled by this extension. Kubernetes adapter tests remain fake-client
coverage until real API-server acceptance is separately recorded.

No capability issuer, gateway, Workflow dispatcher, public schema, chart, RBAC,
or admission policy enables collaboration through this foundation. Before use,
protect Kubernetes gate creation/update/deletion, qualify a real API server,
verify exact plane-claim ownership at the trusted boundary, and integrate the
gate across every supported effect path. Database-level writers and Kubernetes
administrators remain inside the storage trust boundary.

The three security findings remain open for complete implementation and
acceptance. This progress does not establish A2A interoperability, safe handoff
of a live member, production cancellation, or activation readiness.

### Collaboration Runtime, Governed Transport, And Authority Endpoint (2026-11-23)

This section records unwired, disabled source. It does not close the three
security findings or the selected tasks and does not replace the independent
security-owner approval.

- Layers (all under `internal/collaboration`): the pure strategy engine and
  codec; `driver` (ledger CAS, `Begin`/`Step`/`Stop`/`Abandon`); `a2atransport`
  (all four strategies over real A2A gRPC); `driver/k8sledger` (ConfigMap
  ledger `wfc-<hash>` plus durable `<name>-handles` ConfigMap); `runner`
  (the controller-facing `WorkflowCollaborationRunner`); and `governed` (the
  transport that routes every member call through the executor initiation
  path).
- Principals: the controller's authenticated executor attests every dispatch
  and the Initiator is the workflow principal. A member is only the genuine
  Agent and AgentService behind an authenticated connection, and
  `Member.Alias` must equal the recipient. The requester is signed provenance
  inside the payload, not an authenticated principal. Members never call each
  other directly; the engine enforces the delegation edges.
- Invocation identity: `InvocationID` is `wfc-` plus the hex SHA-256 of the
  domain, workspace, run UID, step, activation, attempt and dispatch ID
  (68 characters, deterministic). It is both the A2A `Message.ID` and the
  executor-action idempotency key. The owner epoch is excluded, so a collision
  under a different epoch fails closed.
- Payload: `kognition.collaboration.dispatch/v1` JSON in one
  `application/json` raw part (dispatch ID, kind, task, turn, epoch,
  recipient, requester, allowance, allowed operations, prompt). It is
  deterministic and frozen: allowed operations are fixed at intent and a
  cancel carries `Message.Target`. The worker unwraps `prompt`; member
  operations return through artifacts.
- Expiry: the action `ExpiresAt` is the clock plus `Window` (default 12h) on
  first use, then the retained approval's value. Polling stops after the
  window; a stuck dispatch is settled through `Abandon`.
- Usage: workers MUST report usage in artifact metadata
  `kognition.dev/usage` (`{"totalTokens":n}`). Member-reported usage is capped
  at the allowance; the model gateway remains the authoritative meter.
- Authority endpoint: `runtimeapproval.WorkflowA2AReceiverAuthority` lets a
  receiver in another process obtain its task scope and per-operation
  authentication from the controller that holds the approvals, keys and
  activations. `Initiate` verifies one relayed initiation and returns the
  protected scope without echoing the payload; `Locate` resolves a retained
  task from its locator; `Authenticate` re-proves a scope, with the retained
  payload for a whole-action proof and without it for a first read. The
  authority is pinned to one Agent and AgentService, requires a limit
  authorizer, and runs an injected `authorize` that must prove the live
  receiver workload serves that target.
- Transport: the controller serves
  `/v1/workflow-a2a-authority/{initiate,locate,authenticate}` (POST only,
  receiver-audience bearer token re-authenticated by TokenReview and live
  Pod/ServiceAccount, strict JSON, bounded bodies, 10s timeout). The client is
  https-only, never retries and verifies that the answer matches the request.
- Receiver: `runtimeapproval.NewWorkflowA2ARemoteReceiver` is the A2A request
  handler of a separate receiver process. `SendMessage` relays the
  initiation capability and the raw payload to the authority and authenticates
  every task-store operation through the authority with the receiver's own
  payload; `GetTask` locates the scope from the locator, reads the task and
  re-proves the whole action with the retained payload before releasing
  content. It requires a `Caller` check that proves the A2A caller is the
  controller, rejects ambient or foreign metadata, serves no cancel, list,
  subscribe or push operation and rebuilds the supplied store so a factory
  cannot drop the authenticator. It holds no approval, key or activation.
  Member work runs under a context deadline equal to the authority-verified
  action expiry, so remote work stops by itself (no governed cancel exists); a
  verdict without an expiry is refused before any execution.
- Caller verifier: `auth.NewWorkflowA2AControllerCaller(namespace,
  serviceAccount, reviews, reader).Verify` is the receiver's `Caller`. It rereads
  the bearer from the incoming gRPC metadata on every call and performs a live
  TokenReview with the dedicated audience `kognition.dev/workflow-a2a-controller`
  (never the executor audience, so a member-controlled receiver cannot replay the
  controller's executor token against the apiserver). It pins the exact
  ServiceAccount subject and requires a Ready pod whose UID differs from the
  ServiceAccount UID, so a replaced controller fails immediately. Evidence is
  fixture-level: fake TokenReview and client, no installed token or TLS.
- Model capability: the receiver never holds a signing key. The authority
  route `/v1/workflow-a2a-authority/capability` re-derives the action from
  the relayed payload, pins target, service, digest, activation and invocation,
  runs `authorize`, and asks an injected `WorkflowA2AModelCapabilityIssuer`
  (`WithModelCapabilityIssuer`) to mint a capability for the verified task. The
  capability string is validated and the scope is authenticated again before it
  is returned; any failure returns no capability. The receiver passes the
  verified execution (`WorkflowA2AVerifiedExecutionFromContext`: scope, payload
  and expiry) to the member executor, and the executor resolves the capability
  only for its own A2A task ID. No issuer implementation is assembled yet.
- Receiver binary: `cmd/kognition-workflow-a2a-receiver` runs
  `internal/workflowreceiver`. It serves one member Agent as a gRPC A2A server
  over TLS (certificate reloaded per handshake) that allows only `SendMessage`
  and `GetTask`, no streams, bounded messages and concurrency. Each call is
  admitted only for the live controller (`Caller`), then relayed to the authority
  with the receiver-audience token; task writes go through the task writer with
  the writer-audience token; both tokens are projected files reread per call and
  must be distinct. Controller endpoints are https origins with no credentials or
  path, with an optional sole CA and no proxy or redirect. Model execution uses
  `agentworker.ModelConfigFromEnv`, which needs no control token, and refuses
  tools, knowledge, memory and delegation. Configuration is
  `KOGNITION_WORKFLOW_CONTROLLER_NAMESPACE`,
  `KOGNITION_WORKFLOW_CONTROLLER_SERVICE_ACCOUNT` (not `default`),
  `KOGNITION_WORKFLOW_AUTHORITY_URL`, `KOGNITION_WORKFLOW_TASK_WRITER_URL`,
  `KOGNITION_WORKFLOW_RECEIVER_TOKEN_FILE`, `KOGNITION_WORKFLOW_WRITER_TOKEN_FILE`,
  `KOGNITION_WORKFLOW_RECEIVER_TLS_CERT_FILE`,
  `KOGNITION_WORKFLOW_RECEIVER_TLS_KEY_FILE`, and optionally
  `KOGNITION_WORKFLOW_CONTROLLER_CA_FILE`, `KOGNITION_WORKFLOW_RECEIVER_LISTEN`
  and `KOGNITION_WORKFLOW_RECEIVER_MAX_INPUT_BYTES`, plus the model variables
  of the native worker. Evidence is fixture-level: a real TLS gRPC server and
  official A2A client over loopback with a stub authority, a fake TokenReview and
  a fake Kubernetes client; it covers caller refusal, capability binding to the
  verified task, a refused capability failing the task without execution,
  unsupported methods, dependency and configuration refusal. Not deployed, not
  wired, no image.
- Final-task allowance: a Final task that can still fan out receives half of
  the remaining tokens and keeps the rest for its work; a Final task that
  cannot fan out receives all of them.
- Open: no native assembly in `cmd/kognition-k8s-controller`; the controller
  must still mount a projected ServiceAccount token with the dedicated audience
  and attach it as the Bearer, and the concrete model-capability issuer behind
  the authority route (bound to the member Agent, the task ID and the action
  expiry) is not assembled; no receiver image, chart workload, ServiceAccount or
  RBAC and no installed TLS evidence; the Communication Gateway has no
  A2A dispatch-bound capability to back `authorize`; remote cancel is
  unsupported in the governed path (cancel settles only when the target task
  is terminal, and a lost acknowledgement is resolved only by `Abandon`); a
  pending approval makes `Step` return an error; `MaxEffects` must be at least
  `MaxMessages`; cyclic workflows are rejected while collaboration is on.


### Collaboration Assembly And Chart Profile (2026-11-23)

This supersedes the "no native assembly" and "no model-capability issuer" parts
of the Open item above. Everything remains disabled by default and fixture-level;
nothing is deployed and tasks 1 through 5 and the High finding stay open.

- Model capability: the dispatcher issues the member capability from the Running
  AgentInstance's `Status.SessionID` with a `ModelSessionWorkflowTask` claim. The
  Model Gateway `authorize` path already validates that instance, session and
  Ready identity, so no gateway code changed. A gateway test now proves a
  task-bearing capability is accepted, both trust decisions validate against the
  schema and carry the member invocation, and a replaced instance session denies
  the same capability.
- Governed executor factory: `governed.NewExecutorFactory` validates the run,
  acceptance, authority and digests, then the approved step and members, then
  creates the activation, and derives one receiver host per member from the Ready
  AgentService endpoint. `NewTLSReceiverDialer` pins the supplied CA and caches
  connections.
- Controller assembly: `cmd/kognition-k8s-controller` builds the runner only when
  `KOGNITION_WORKFLOW_COLLABORATION_ENABLED=true`, and then requires the native
  executor identity, approval keys and admission webhooks. Optional settings are
  `_RECEIVER_PORT` (default 8443), `_CLUSTER_DOMAIN`, and required
  `_RECEIVER_CA_FILE` and `_TOKEN_FILE` (absolute paths; the token is a projected
  file with audience `kognition.dev/workflow-a2a-controller`). Setting any of them
  without the enable flag is a startup error.
- Chart: `k8sController.workflowExecutor.collaboration` (disabled) renders those
  variables, a projected token volume, the receiver CA Secret mount, a controller
  Role `update` on ConfigMaps in managed namespaces, and two state-admission
  variables. `workflowActivation` admits only controller CREATE/UPDATE of
  `kognition-workflow-activation-<64 hex>` records in the installation namespace
  (single label, one `activation.json` of at most 256 KiB, no metadata extras).
  `collaborationLedger` admits only controller CREATE/UPDATE of `wfc-<24 hex>` and
  `wfc-<24 hex>-handles` records in managed namespaces (exactly the three runner
  labels, no annotations, owners or finalizers, a `state` entry of at most 1 MiB).
  Deletes are not admitted, so ledgers are not garbage collected without owner
  references. Enabling requires the native executor and the receiver CA Secret.
- Still open: the receiver authority handler is pinned to one Agent and one
  receiver, so it cannot serve a multi-member team (single-receiver profile
  conflict); a Workflow UID versus WorkflowRun UID mismatch makes the factory fail
  closed in production; the dispatcher does not yet launch a per-Agent receiver
  Pod, Service, certificate or token; the controller NetworkPolicy egress to
  receivers is not rendered.

#### AKS Admission Validation (2026-11-23)

- The new state policy was compiled and behaviour-tested on the existing AKS
  cluster as an isolated scratch copy (bound only to labelled scratch namespaces
  and the impersonated controller ServiceAccount). It was not the live release
  policy and nothing from the release was changed. Persisted type-check status
  was empty (clean); 27 of 27 cases behaved as expected (6 allowed, 21 denied),
  plus four follow-ups (1,000,000-character state allowed, non-hex ledger name
  denied, `-handlesx` denied, `-handles` allowed). The 1 MiB state bound cannot
  be exercised because the API server rejects such objects first.
- The validation exposed a defect already present in live revision 10: the
  `workflow-records` webhook match condition evaluated `request.name` without
  `has()`. A collection DELETE (as issued by the namespace controller) carries no
  name, so the condition errored and the fail-closed policy denied it, leaving
  namespaces that contain ConfigMaps in `Terminating`. An isolated scratch
  webhook reproduced the failure with the old condition and showed the guarded
  condition allowing collection delete while reserved names remain routed to the
  guard. The chart source is fixed with a regression assertion (Helm regression
  595 passed, 0 failed; archive parity passes). The fix is not deployed.

## AKS Rollout Evidence (2026-11-22)

- Controller image `kognition-k8s-controller:aks-wfc-20261122`
  (`sha256:54df996cc018ac9692836ec2c20a22760048329c16c8219b201a6d9225058c8e`)
  is deployed to the existing AKS installation; Helm revision 10 is `deployed`
  (revision 8 carried the image/CRD/webhook rollout; revision 5 is the
  last-known-good rollback point).
- The 54 CRDs were applied with `kubectl` (chart CRDs are install-only) and the
  webhook certificate was rotated with the workflow webhook SANs.
- Live checks: all `kognition*` Deployments ready with no restarted pods; the
  workflow webhook TLS path works; the `workflow-records` webhook denies
  unqualified writes to protected workflow ConfigMaps and allows ordinary
  ConfigMaps; a `WorkflowRun` server-side dry-run is accepted.
- Revisions 6 and 7 remain `failed` in Helm history. Revision 6 used
  `--force-conflicts`, invalid for this client-side-apply release. Revision 7
  hung on the `kb-pgvector-init` hook: its pod lacked the
  `app.kubernetes.io/part-of: kognition` label the kb-pgvector NetworkPolicy
  requires. The chart now sets the label (regression-tested); revision 9 ran
  with hooks enabled and the hook completed.
- The controller logs `events.k8s.io` forbidden errors for namespace `default`
  (events about cluster-scoped KnowledgeBaseClass objects). RBAC was not widened.
  Source now skips events for cluster-scoped objects; the running image predates
  that change, so the log noise stops after the next controller image rollout.
- `k8sController.workflowExecutor.bindingAdminGroups` is the Entra group object ID
  `5f0d8020-43bd-44ba-ab94-edc228dfe551` (`kognition-workflow-binding-admins`,
  created by `deploy/aks/Initialize-BindingAdminGroup.ps1`; revision 10). This
  existing cluster is not Entra-integrated (local admin, no `aadProfile`), so no
  user presents that group and binding administration remains unusable here until
  Entra integration is enabled. Clusters from the Bicep foundation are integrated.
- The ACR is private again (`publicNetworkAccess=Disabled`, no IP rules).

This is deployment evidence only. The independent security approval is still
outstanding; the three security findings and tasks 1 through 5 remain open, and
this does not approve production activation.
