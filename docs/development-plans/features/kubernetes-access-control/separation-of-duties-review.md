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
