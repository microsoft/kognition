# Separation-of-duties review record (EPIC-0025, US-0117)

Findings and residuals from the hardening tasks. Each entry states what was
verified in source, what changed, and what is accepted.

## T-0450 Self-approval

| Decision path | Submitter evidence | Result |
| --- | --- | --- |
| `RuntimeApproval` raised by the workflow executor (`spec.executorAction.initiator`) | The authenticated submitter (`tokenreview` issuer and username), bound into the approval by the exact-action binding | **Closed.** `store.ErrSelfApproval` rejects a decision whose actor equals the initiator, in the Kubernetes store and the PostgreSQL store. The decision endpoints answer `403`. The approval stays pending and unchanged, and a different approver can still decide it. |
| `RuntimeApproval` raised by an agent runtime | None. The approval records the Agent, instance and session, not a human | **Residual.** There is no person to compare. A person who started a chat turn can hold `approver` and decide an approval it triggered. Separate the roles between subjects. |
| `ApprovalDecision` workflow gate vote | None durable on the `WorkflowRun`. The submission receipt exists only for protected submissions and lives for one minute | **Residual.** `spec.approver` is bound to the caller, but the caller is not compared with the run submitter. Closing this needs a durable submitter record on the run, which is not part of stack 1. |

Operator guidance for the residuals: do not bind `kognition-consumer` and
`kognition-approver` to the same subject where independence matters, and use the
recommended bundles in the plan, which keep them apart (Diego versus Business
approver).

Evidence: `TestIsSelfDecision`, `TestKubernetesRuntimeApprovalInitiatorCannotDecideItsOwnAction`,
`TestRuntimeApprovalDecisionHandlerMapsSelfApprovalToForbidden` and
`TestWriteRuntimeApprovalStoreErrorMapsSelfApprovalToForbidden`. Each was written
first and failed before the check existed. A forged identity against a real API
server is verified in the kind tier (T-0459).

## T-0451 Controller-mediated creation paths

Rule applied (plan principle 6): bounded controller-owned provisioning that
follows from an object the author may create is allowed. A user-selected
definition or delegated capability must not exceed what the author could select
directly.

| Author object | What the controller creates | Verdict |
| --- | --- | --- |
| `MemoryClaim` | A `MemoryVolume` in the same namespace, deterministic name, owner reference to the claim ([memory_reconciler.go](../../../../internal/controller/memory_reconciler.go)). No `MemoryGrant`. | **Bounded provisioning.** Allowed. |
| `KnowledgeBaseIndexSchedule` | `KnowledgeBaseIndexRun` children in the same namespace for the `KnowledgeBase` it names; the curator role already holds both kinds ([kb_indexschedule_reconciler.go](../../../../internal/controller/kb_indexschedule_reconciler.go)). | **Bounded provisioning.** Allowed. |
| `AgentAutoscaler` | Changes `spec.replicas` of the same-namespace `Agent` it names ([agent_autoscaler_reconciler.go](../../../../internal/controller/agent_autoscaler_reconciler.go)). | **Accepted.** `AgentAutoscaler` and `Agent` are both `config`, so no shipped role can scale an Agent it could not already edit. A custom role that grants the autoscaler without the Agent would gain scaling authority; operators must not split them. |
| `Evaluation` and `EvaluationRun` | `EvaluationRun`, then target and judge `AgentJob`s in the same namespace ([evaluation_reconciler.go](../../../../internal/controller/evaluation_reconciler.go)). | **Accepted named exception.** Evaluating is part of authoring, so the builder holds `evaluationruns` create. The spend is the evaluation's own and is bounded by its dataset. |
| Workflow collaboration steps | `AgentJob`s for the members the workflow admits, limited by `allowedInteractions` and communication policy ([transport.go](../../../../internal/collaboration/jobtransport/transport.go)). | **By design.** `use` on a Workflow authorizes running its steps. Execution is under the controller, not the submitter, so `invoke` on each member is not checked for the submitter. The workflow author's definition is the boundary. Recorded so operators treat editing a Workflow as granting its reachable Agents. |
| `Solution` with a `target` | `AgentJob` for an Agent or AgentService, or `WorkflowRun` for a Workflow, chosen by the author ([solution_reconciler.go](../../../../internal/controller/solution_reconciler.go), `compileTargetExecution`). Before T-0469 the run started with no `create` check on `agentjobs` or `workflowruns` and no `use` or `invoke` check. | **Finding, fixed (T-0469).** A builder, who cannot submit work, could start any Agent or Workflow in the namespace through a Solution. Admission now authorizes the requesting user, see below. |
| `Solution` generated resources | `Agent`, `Tool`, `Workflow`, `MemoryClaim`, and an `AgentPolicy` fixed to `enforce: true` over the Solution's own agents. | **Bounded provisioning.** The first four are `config` the builder may create. The policy only restricts. |

Not adopted from the survey: it called `AgentAutoscaler` an escalation, but both kinds
are `config` and the builder holds both, so there is no escalation in the shipped
roles. It also classed workflow collaboration as a finding; it is the intended
meaning of `use`.

Open, no change here: the `WorkflowRun` the Solution controller creates carries no
protected-submission annotation, so it is also outside the `use` check (G7, T-0467).
That is now covered for Solution targets by the admission check below.

### T-0469 Solution target authorization

When a `Solution` sets `spec.target` the validating webhook asks the cluster, for
the requesting user, whether that user could start the target directly:

| Target | Required |
| --- | --- |
| `Agent` | `create` on `agentjobs`, verb `invoke` on that Agent |
| `AgentService` | `create` on `agentjobs`, verb `invoke` on the Agent the service names (the service must exist) |
| `Workflow` | `create` on `workflowruns`, verb `use` on that Workflow |

The check runs on create and on any update that changes the target or goal, so the
controller's own metadata updates are not re-reviewed. It fails closed: an
unavailable or erroring `SubjectAccessReview`, an anonymous user, an unresolvable
AgentService and an unsupported kind are all denied. `resourceNames` scoping applies.

Consequences for operators:

- A user, group or GitOps service account that applies a `Solution` with a target
  needs the rights above. `kognition-builder` alone is not enough; add
  `kognition-consumer` (or equivalent) for the same subject.
- The intent gateway creates Solutions for its own authenticated callers. Its
  service account is an operator-bound identity and is exempt through
  `KOGNITION_SOLUTION_TARGET_EXEMPT_SERVICE_ACCOUNTS`, set by the chart. Its callers'
  authority is the gateway's own authentication, not this check.
- `Solution` is now routed to the validating webhook, so the existing Solution
  spec validator also runs in Kubernetes mode.

Evidence: unit tests for the hook and the reviewer, a handler test, a chart test,
and a real-API-server test (`TestSolutionTargetIsAuthorizedAgainstRealRBAC`) in which
the shipped builder is denied, the shipped consumer is allowed in its own namespace
and denied in another, and `resourceNames` limits the workflow. The kind tier
(T-0459) exercises the deployed webhook.
