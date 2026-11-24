---
title: Workflow collaboration
description: Run a bounded team of Agents as one Workflow step with the Supervisor, Parallel, Handoff or Peer strategy.
---

# Workflow collaboration

A `collaboration` step runs a small team of Agents on one task inside a
Workflow. The team follows one of four strategies, stays within finite limits and
returns one result to the Workflow. The feature is available in Kubernetes mode
and is disabled by default.

## How it works

Each member turn is an ordinary `AgentJob` named `wfcj-<hash>` in the Workflow
namespace. A member therefore uses the existing agent dispatcher, runtime
credentials, model gateway and token metering; collaboration adds no separate
receiver, credential or network path.

The controller keeps the run state in a ledger ConfigMap named `wfc-<hash>`. The
ledger is owned by the Workflow or WorkflowRun and is garbage collected with it.
It records turns, messages, delegations, token use and the final outcome, and it
lets the controller resume after a restart without repeating a completed turn.

Admission restricts what a collaboration controller may write to those ledger
records. Cancelling or deleting the WorkflowRun stops the step and abandons
outstanding member jobs.

## Strategies

| Strategy | Behavior | Required block |
| --- | --- | --- |
| `Supervisor` | The coordinator delegates to specialists and produces the final result. | `supervisor.coordinator` |
| `Parallel` | Branch members run concurrently. An optional synthesizer combines their results. | `parallel.branches`, optional `parallel.synthesizer` |
| `Handoff` | One member owns the task and can transfer ownership. | `handoff.entryMember`, `handoff.maxHandoffs` |
| `Peer` | The finalizer starts the task and exchanges messages with its peers. Only the finalizer can complete the step. | `peer.finalizer` |

Exactly the block matching `strategy` must be present.

## How members collaborate

Every member runs as an AgentJob. The prompt names the members that member may
reach (its outgoing `allowedInteractions`) and the JSON reply that delegates or
hands off to one of them. Members reach each other only through this protocol:
the AgentService `delegate_to_agent` tool is withheld from collaboration runs, so
the interactions, limits and ledger cannot be bypassed.

Each delegation or handoff edge must also be allowed by an
`AgentCommunicationPolicy` (default deny). `allowedInteractions` narrows the
policy but never widens it. The sample defines the policies it needs.

## Define a step

```yaml
steps:
  - name: review-request
    type: collaboration
    input: "{{ .Input }}"
    timeout: 10m
    collaboration:
      strategy: Parallel
      protocol: A2A
      members:
        - name: analyst
          agentRef: { name: wfteam-analyst }
        - name: reviewer
          agentRef: { name: wfteam-reviewer }
        - name: writer
          agentRef: { name: wfteam-writer }
      parallel:
        branches:
          - member: analyst
          - member: reviewer
        synthesizer: writer
      limits:
        maxTurns: 4
        maxMessages: 16
        maxDelegations: 1
        maxDelegationDepth: 1
        maxParallelTasks: 2
        maxTokens: 32768
```

[samples/workflows/collaboration.yaml](../samples/workflows/collaboration.yaml)
contains runnable Parallel and Supervisor workflows and their member Agents.

## Rules enforced at admission

- `timeout` is required, greater than zero and at most 24 hours. `retry`,
  `agentRef` and `toolRef` are not allowed on the step.
- `protocol` is `A2A`.
- A team has 2 to 8 members. Aliases are lowercase DNS labels of at most 32
  characters, starting with a letter. **Members must be distinct Agents.**
- `allowedInteractions` (`from`/`to` member aliases) is required for Supervisor,
  Handoff and Peer, and must be empty for Parallel. Self and duplicate
  interactions are rejected.
- The Supervisor coordinator, the Handoff entry member and the Peer finalizer
  (which starts the run) need at least one
  outgoing interaction. `handoff.maxHandoffs` is between 1 and
  `limits.maxDelegations`.
- Parallel needs 2 to 8 branches, each a distinct member. The synthesizer must not
  also be a branch.
- Every limit is required: `maxTurns` (1-100), `maxMessages` (1-500),
  `maxDelegations` (1-100), `maxDelegationDepth` (1-5), `maxParallelTasks`
  (1-8) and `maxTokens` (greater than zero).

## Token accounting

Member runtimes report usage in the result artifact metadata under
`kognition.dev/usage` as `{"totalTokens": n}`. The controller sums reported usage
against `limits.maxTokens`. If a member reports no usable count, the controller
charges the whole remaining allowance rather than assuming zero, so an
unmetered member cannot bypass the limit. Model gateway metering of the member
Agent still applies independently.

## Enable collaboration

Set the Helm value on a release that already enables the Workflow executor:

```yaml
k8sController:
  workflowExecutor:
    enabled: true
    collaboration:
      enabled: true
```

Without it, admission rejects `collaboration` steps. Enabling it grants the
controller update on ConfigMaps in managed namespaces, restricted by admission to
collaboration ledgers. The chart's Custom Resource Definitions are install-only;
apply updated CRDs with `kubectl apply` when upgrading an existing cluster.

## Studio

In Kubernetes mode, Kognition Studio edits a collaboration step in its **Team**
step editor. It supports all four strategies, members, interactions and limits,
shows the admission rules as problems before deployment and updates references
when a member is renamed. In standalone mode the editor is read-only because
collaboration needs the Kubernetes executor. Server-side admission remains the
authority.

## Failure and outcomes

A step fails with a reason such as `MemberFailed`, `MemberRejected`,
`MemberRequestedInput`, `InvalidPayload` or `NoResult`, or when it reaches a
limit or its timeout. An optional Parallel branch may fail without failing the
step. Completed turns stay recorded in the ledger.

## Limits of the current design

- Member turns use the member Agent's existing runtime invocation. The wire
  exchange between the controller and a member is an AgentJob, not a native A2A
  network connection between members.
- Members cannot be the same Agent twice in a team.
- A member cannot ask a human for input; such a request fails the step.
- This design has not had an independent security-owner review. The earlier
  [plan and reviews](development-plans/features/workflow-collaboration/plan.md)
  describe a different, unbuilt receiver design.
