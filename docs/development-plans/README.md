# Development Plans and Review Records

This directory contains implementation planning and development-tracking
material. Plans describe proposed work; baselines, reviews and acceptance records
apply only to their recorded source and environment. They do not establish the
current product contract or release readiness.

## Contents

| Area | Contents |
| --- | --- |
| [Refactoring](refactoring/repository-refactor-plan.md) | Repository-boundary plan, dependency/test baselines and the companion test-ownership inventory |
| [Feature work](features/) | Implementation plans, security reviews, acceptance reports, finding dispositions and backlog tracking, grouped by feature |
| [Workflow collaboration](features/workflow-collaboration/plan.md) | Implemented (AgentJob design, see the [guide](../workflow-collaboration.md)); the plan is retained as history and is partly superseded |
| [Review records](reviews/) | Historical implementation/backlog reviews and closure ledgers |

These files remain versioned so contributors can follow implementation decisions
and reproduce development work. Preserve historical evidence scope and failure
results when updating a plan.

Demo preparation documents, including video-production plans, recording
notes and the presenter storyboard, are local-only and excluded from Git.
Runnable samples, recording helpers and regression tests remain versioned.

## Documentation That Stays Outside This Directory

- User and operator guides, including [bootstrapping](../bootstrapping.md),
  [security](../security.md) and [Studio](../studio.md).
- Current [installation/recovery](../features/knowledge-provider-execution/installation-recovery-contract.md),
  [activation](../features/knowledge-provider-execution/activation-transaction-contract.md),
  [bootstrap verification](../features/knowledge-provider-execution/bootstrap-verification-contract.md)
  and [drain verification](../features/knowledge-provider-execution/drain-verification-contract.md)
  contracts.
- [Architecture references](../architecture/reference/README.md) and
  [Architecture Decision Records](../adr/README.md).
- Contributor instructions such as [testing](../testing.md) and
  [change-based validation](../development-gates.md).

Link to the current guides/contracts for supported behavior and to this directory
for implementation context. Moving a record does not change its authority or
approve proposed work.
