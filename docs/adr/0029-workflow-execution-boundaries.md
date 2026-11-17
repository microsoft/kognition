# ADR-0029: Workflow Execution Boundaries

- Status: Accepted.
- Date: 2026-11-16. Accepted: 2026-11-17.
- Scope: Workflow collaboration initiation, content release, and stop/handoff
  dispatch admission.
- Approval boundary: accepted by the feature owner on 2026-11-17 together with
  the [decision gate resolutions](../development-plans/features/workflow-collaboration/decision-gate-resolutions.md)
  (gates 2, 3, and 11). This permits dependent implementation behind a disabled
  feature. Independent security-owner approval, executable acceptance evidence,
  and operator approval remain required before production activation.

## Context

Workflow collaboration will reuse WorkflowRun while supporting four strategies
and cross-namespace Agent references. The existing member gateway validates a
Running source AgentInstance. A controller initiating the first member has no
such source. Workflow edge authorization skips steps without `agentRef`, and
templates can read accumulated outputs. Checking a run's status before a separate
effect-claim write cannot atomically fence cancellation in Kubernetes.

See the [readiness review](../development-plans/features/workflow-collaboration/security-review-architecture.md)
and [detailed execution contract](../development-plans/features/workflow-collaboration/execution-contract.md).

## Priorities And Requirements

1. Prevent unauthorized target contact and data release. Neither a new step type,
   copied run ID, finalizer, nor controller identity may bypass target policy.
2. Preserve exact effect ownership and conservative recovery. Existing claimed
   operations cannot be reissued or reassigned through a new run mechanism.
3. Support durable automation with honest cross-backend stop semantics. Do not
   retain human credentials or claim multi-object Kubernetes transactions.
4. Bound implementation and operational cost. Reuse WorkflowRun and existing
   gateways, with finite protected records instead of another public run system.

## Alternatives Considered

Each assessment follows the priority order above.

| Domain and option | Unauthorized access | Effect ownership | Durable/cross-backend behavior | Cost and compatibility |
| --- | --- | --- | --- | --- |
| Initiation using an operator-bound Workflow executor | Meets with revision-bound approval and exact target policy | Meets with existing plane claims | Meets without human credential retention | Requires explicit use/invoke policy and a separate capability type |
| Initiation under submitter permissions for every action | Can meet with live identity and target checks | Meets | Partial, because accepted runs depend on continuing human authority | Requires a second identity-refresh and revocation model |
| Controller impersonates a member or bypasses its validator | Does not meet, as source attribution and scope become ambiguous | Partial at best | Easy to start but unsafe to recover | Low initial cost with unacceptable trust-boundary changes |
| Explicit input bindings with retained lineage | Meets within the documented platform and native RBAC trust boundaries | Meets | Deterministic across restart and backends | Requires restricted new-definition templates and protected payload references |
| Dependency edges and finalizer-only provenance | Does not meet for template reads or contributed worker content | Meets for effect claims only | Simple but loses data-release meaning | Lowest initial cost, insufficient authorization |
| Single-gate final dispatch admission plus drain | Meets after the stop's admission fence, with earlier committed dispatch treated as in flight | Meets, as the plane remains the sole effect owner | Meets using row transactions or one-object CAS | Adds a bounded ledger and a related pre-target store operation |
| Read run state and then independently claim an effect | Does not meet under a stop/claim race | Preserves ownership but not the stop boundary | Does not meet Kubernetes atomicity | Small implementation with a security race |
| Serialize every plane claim and stop in one backend transaction | Can meet | Can meet | Partial, because Kubernetes has no general multi-object transaction | Unacceptable cross-backend divergence or broad ownership rewrite |

The user delegated technical decisions while unavailable. The operator-bound
authority model is a recommendation under that delegation, not an explicit user
selection of new security semantics.

## Proposed Decision

- Bind approved Workflow revisions to operator-managed executor authority.
  Submission requires permission to use that binding. Re-evaluate the executor,
  target, grants, expiry, and live binding for new initiation. Retain the original
  submitter as attribution without storing their credentials.
- Use explicit source-to-recipient data bindings for new collaboration-bearing
  definitions, conservative lineage propagation, and content-authorized result
  publication. Preserve legacy graph-only behavior.
- Register exact effects in a bounded activation gate. Keep existing plane claims
  as sole effect ownership. Require a final CommitDispatch on the same gate that
  serializes stop and handoff before any target contact.
- Distinguish accepted stop requests from drained execution. A committed dispatch
  can still produce an effect after stop is requested. Recipient activation waits
  for conclusive old-owner outcomes and required settlement.
- Keep maintenance available without new execution authority. Pending approval,
  lease expiry, and an epoch change cannot authorize replay or erase uncertainty.

This satisfies the first two priorities without inventing member identity or a
second effect owner. One-object admission serializes the stop boundary on both
backends, satisfying the third priority. Existing run and gateway reuse limits
the additional public surface under the fourth priority.

This supplements [ADR-0014](0014-runtime-autonomy-gate-completion-and-recovery.md)
with a directly related pre-target eligibility check for collaboration effects.
It does not supersede its preparation, approval, effect ownership, or recovery
rules. SDK and public schema choices are outside this decision.

## Consequences

- Positive: explicit initiation attribution, bounded data release, and testable
  cancellation/handoff semantics without relying on Kubernetes multi-object CAS.
- Positive: no AgentPoolRun, fabricated AgentInstance, retained human credential,
  or new path to replay an uncertain effect.
- Negative: protected execution bindings, typed standalone authorization, payload
  references, gate storage, and integration across every supported effect plane
  are required before activation.
- Negative: conservative provenance can deny otherwise useful sharing, and an
  uncertain admitted effect can block handoff until evidence resolves it.
- Compatibility: accepted non-collaboration behavior stays unchanged. New
  collaboration definitions cannot run under partially integrated profiles.

## Acceptance And Follow-Up

Before dependent implementation, approve this decision and the detailed contract's
policy/schema choices. Demonstrate both stop/dispatch orderings, claimed-but-not-
dispatched cleanup, uncertain committed recovery, explicit dataflow isolation,
and executor revocation under the supported backend/runtime matrix. A successful
documentation review does not satisfy those executable acceptance obligations.
