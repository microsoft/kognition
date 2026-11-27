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

### Steer the team: guidance and member descriptions

A model that is told only the aliases it may reach can legitimately answer alone.
Two optional, author-written fields tell the team how you want the task resolved:

- `collaboration.guidance` (at most 2000 characters, multi-line) is sent to every
  member as trusted instructions, for example the order of work or what each member
  should check.
- `members[].description` (at most 500 characters, one line) states what a member is
  good at. A member sees its own description as its role and the descriptions of the
  members it may involve when it chooses whom to delegate to. An ExternalAgent member
  without a description is described by the skills pinned in its ExternalAgent
  `card.skills`; the live Agent Card is not read for this.

Both fields only inform the members' choices. They never grant or widen an
interaction: `allowedInteractions` and `AgentCommunicationPolicy` remain the only
authority, and a member can still decide not to delegate. Control characters are
rejected, and descriptions are collapsed to one line before they enter a prompt.

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
- `input` must be non-blank.
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
against `limits.maxTokens`, including the member call that accepts or rejects a
handoff. If a member reports no usable count, the controller
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
when a member is renamed. A **Team guidance** section edits `guidance`, and each
member has a **Description** field. In standalone mode the editor is read-only because
collaboration needs the Kubernetes executor. Server-side admission remains the
authority. Each member has a **Kind** selector (Agent or External agent); an
external member offers the ExternalAgents of the Workflow namespace, reports
missing or duplicate references as problems and warns that prompt data leaves
the platform.

## External agents

A team member can be a remote agent that Kognition does not run, reached over
A2A. Set `externalAgentRef` instead of `agentRef` on the member. Exactly one of
the two is allowed, `serviceRef` is not allowed with an external agent, and the
ExternalAgent must be in the Workflow namespace. An external member cannot
delegate to an Agent member; the router and runtime refuse that interaction.
It also cannot be the Supervisor coordinator, the Parallel synthesizer or the
Peer finalizer; admission rejects that, so a local Agent always decides the result.

```yaml
members:
  - name: reviewer
    externalAgentRef: { name: partner-compliance }
```

Two resources describe the remote side:

- `ExternalAgent` names the endpoint (`host:port`, a fully qualified DNS name, no
  IP address), pins the Agent Card by `card.digest` (`sha256:` plus 64 lowercase
  hex characters) and refers to a SecretRef holding the bearer token through
  `credentialRef`. Optional `limits` bound the calls. The spec is immutable; to
  change it, create a new ExternalAgent. The workflow author never sees the
  token; only the controller transport resolves it.
- `ExternalAgentGrant` authorizes the `invoke` verb for one Workflow
  (`workflowRef`) or a selector of Workflows (`workflowSelector`) on one
  ExternalAgent. A live grant is required at every send, so deleting it stops
  new sends immediately.

The digest is the SHA-256 of the RFC 8785 (JCS) form of the Agent Card as the
endpoint serves it, with `signatures` removed. The card is fetched for each call
without the token, and the call fails if the digest differs.

[samples/workflows/external-agent.yaml](../samples/workflows/external-agent.yaml)
shows a Parallel team with one external member. Replace its placeholder
endpoint, digest and token Secret before applying.

### Enable external agents

```yaml
k8sController:
  workflowExecutor:
    enabled: true
    collaboration:
      enabled: true
      externalAgents:
        enabled: true
        egressAllowList:
          - agents.partner.example.com:443
```

The allow-list takes `host:port` or `*.domain:port` entries (the wildcard never
matches the bare domain). An empty list permits nothing. It reaches the
controller and the webhook as `KOGNITION_EXTERNAL_AGENT_ALLOWLIST`, and is
enforced at admission, in the ExternalAgent status reconciler and in the
transport, which also refuses private and link-local addresses. The switch is
`KOGNITION_EXTERNAL_AGENTS_ENABLED`. External agents need Kubernetes mode and
collaboration.

### What leaves the platform

- The member prompt, and the data it carries, is sent to the remote endpoint.
  The remote operator is outside Kognition's identity, metering, tool and audit
  boundary.
- The reply is untrusted input. It is passed to later members as data, not as
  instructions.
- A send is written to the audit log (communication plane) before it is made.
- If the outcome of a send is unknown (a timeout, a dropped connection, or a
  reply that is only a Message with no task result), the send is treated as
  ambiguous and is never repeated automatically.
- The concurrent-call cap is local to each controller process; it is not shared
  between replicas.

## Failure and outcomes

Before the first dispatch, the controller fails the step if the resolved input
is blank or exceeds 8 KiB.

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
- External agents are team members only. A standalone external step in a
  Workflow is not supported, and an ExternalAgent is limited to the namespace
  of the Workflow that references it.
- The allow-list is a host-name check in admission and in the transport. Kubernetes
  NetworkPolicy cannot match DNS names, so the policy only limits egress to the
  allow-listed ports.
- `cr_state_metrics` does not include ExternalAgent or ExternalAgentGrant.
- This design has not had an independent security-owner review. The earlier
  [plan and reviews](development-plans/features/workflow-collaboration/plan.md)
  describe a different, unbuilt receiver design.
