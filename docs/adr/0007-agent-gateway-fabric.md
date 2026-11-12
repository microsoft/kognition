# ADR-0007: Agent Gateway Fabric

- **Status:** Accepted
- **Date:** 2026-08-19
- **Deciders:** Kognition core team

## Context

Kognition has an external `IntentGateway` and a runtime memory policy gateway.
`IntentGateway` accepts north-south goals and routes them to Kognition
resources. The memory gateway authorizes one target-specific data-plane path.

Runtime agent tool calls currently execute directly from Python agent pods to
OpenAPI or MCP endpoints. Controller-side workflow tool steps use a separate
Go path that enforces `ToolGrant`, `AgentRole`, quota, tracing, and execution
limits. This creates inconsistent governance and observability between the
standalone and Kubernetes execution paths.

Enterprise deployments need a common invocation boundary that can attach a
stable invocation identity, evaluate live policy, reserve budget, emit audit
and trace data, and invoke the target protocol. The boundary must preserve the
same policy semantics in standalone and Kubernetes modes.

## Decision

Kognition will evolve as an Agent Gateway fabric with separate ingress,
invocation, and target-specific gateway responsibilities.

1. `IntentGateway` remains the north-south entry point for external goals.
2. A shared Tool Gateway core becomes the first Agent Gateway vertical slice.
   It evaluates existing `ToolGrant`, `AgentRole`, enforced `AgentPolicy`, and
   workspace tool-call quota before protocol execution. It emits the existing
   trace and audit contracts around the invocation.
3. Standalone chat and controller workflow tool calls use the shared Tool
   Gateway core in process. Kubernetes agent runtimes call a Tool Gateway HTTP
   service using a short-lived, target-bound capability. These transports have
   identical authorization, quota, audit, and result semantics.
4. Tool capabilities bind namespace, Agent, automatic AgentIdentity,
   AgentInstance immutable UID, active session, tool identity, permitted verb,
   invocation ID, audience, and expiry. The gateway re-evaluates live grants
   and roles for every request, so revocation applies immediately.
   Because model tool selection happens after dispatch, the dispatcher first
   issues a distinct, non-executable session capability. The runtime exchanges
   it for one exact Tool capability after selecting a tool. The exchange
   validates the live runtime session, current Tool resource version, and
   `ToolGrant`/`AgentRole`/enforced `AgentPolicy`; it neither reserves quota nor
   records an invocation. The exact capability is then revalidated at invoke
   time, where quota reservation and the write-ahead audit record occur before
   execution. A session capability cannot invoke a tool directly.
5. The Tool Gateway owns protocol execution for OpenAPI and MCP tools. Runtime
   pods no longer receive a direct executable tool endpoint when the gateway is
   enabled. The gateway remains subject to endpoint safety validation, timeout,
   response-size, and trace-propagation controls already implemented by the
   tool executor. Standalone and Kubernetes HTTP callers receive the same
   normalized result contract for success, replay, receipt conflict, rejection,
   timeout, circuit rejection, quota denial, and post-execution audit failure.
   A post-execution audit failure retains the Tool output with a safe error
   code so callers can distinguish an uncertain audit state from a skipped Tool.
6. Kubernetes Tool Gateway replicas coordinate per-Tool concurrency and circuit
   state through control-namespace `coordination.k8s.io/v1` Leases. Holder
   entries expire after a bounded execution hold, state mutations use
   optimistic concurrency, and Kubernetes API coordination failures reject new
   execution rather than bypass a configured resilience policy. Standalone
   mode retains its in-process coordinator because it has one gateway process.
7. Model egress and agent-to-agent delegation will reuse the invocation
   envelope and capability pattern after the Tool Gateway is operating in both
   modes. They are not part of this first implementation slice.

## Consequences

**Positive.**

- Runtime and controller-originated tool calls enforce the same declarative
  grants, roles, quotas, and policies.
- A capability cannot be replayed for a different agent, session, or tool.
- Dynamic model-selected tool calls retain exact tool and invocation binding
   without giving a runtime pod a wildcard executable capability.
- Audit, cost attribution, and traces have a common invocation boundary.
- Kubernetes NetworkPolicy can restrict runtimes to the Tool Gateway instead
  of arbitrary tool endpoints.
- The design reuses the proven memory capability and live-policy model rather
  than introducing a second authorization engine.

**Negative.**

- Tool invocation gains a network hop in Kubernetes mode.
- The gateway needs its own availability, capability-key rotation, RBAC, and
  deployment lifecycle.
- Direct in-cluster tools must be reachable from the gateway and explicitly
  opt into private-network access where required.

## Alternatives Considered

1. **Continue direct runtime-to-tool calls.** Rejected because grants, roles,
   audit durability, quotas, and endpoint governance cannot be guaranteed at
   the data plane.
2. **Put all agent, tool, model, and memory traffic behind one generic proxy.**
   Rejected because target-specific authorization and execution behavior would
   be coupled into a large service. The memory gateway already demonstrates
   that specialization keeps data-plane policy clear.
3. **Use only an external API or AI gateway.** Rejected because an external
   proxy does not understand Kognition resources, AgentInstance identity,
   `ToolGrant`, `AgentRole`, workflow state, or autonomy policy. It may remain
   an outer edge or model-provider layer.

## Related

- [ADR-0006: Runtime memory policy gateway](0006-runtime-memory-policy-gateway.md)
- [docs/security.md](../security.md)
- [docs/resource-interactions.md](../resource-interactions.md)