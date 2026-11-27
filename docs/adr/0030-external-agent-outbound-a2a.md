# ADR-0030: ExternalAgent outbound A2A trust boundary

- Status: Proposed.
- Date: 2026-11-18.
- Scope: how Workflow collaboration members and steps call agents that Kognition
  does not run, using the A2A protocol.
- Approval boundary: the feature owner approved the v1 scope on 2026-11-18
  (static endpoint, pinned card digest, bearer-token credential, egress
  allow-list). This record stays Proposed until the open decisions in the
  [plan](../development-plans/features/external-agent/plan.md) are resolved and
  the independent security owner reviews it. It does not authorise production use.

## Context

Collaboration members are `Agent` resources run as `AgentJob` turns under the
platform's identity, model gateway and metering
([ADR-0029](0029-workflow-execution-boundaries.md), the
[collaboration guide](../workflow-collaboration.md)). Users also need members that
run elsewhere. A2A is the interoperability protocol for that. A remote agent
differs from a local one in every control the platform relies on: Kognition does
not issue its identity, meter its model calls, bound its tools or inspect its
code, and it receives data outside the cluster.

## Priorities and requirements

1. Prevent unauthorised data release and network reach, including SSRF.
2. Preserve exact effect ownership: a remote task is never silently repeated.
3. Keep secrets away from workflow authors and from every persisted record.
4. Bound cost, time and size even when the remote is dishonest.
5. Reuse existing collaboration, ledger, grant and Secret mechanisms; add no
   server or listener.

## Alternatives considered

| Option | Boundary visible and enforceable | Effect ownership | Secret handling | Cost and compatibility |
| --- | --- | --- | --- | --- |
| Separate `ExternalAgent` resource with a client-only transport (chosen) | Meets: every control attaches to one explicit kind | Meets with persisted remote handles | Meets: credential read only by the transport | One CRD, one grant, one transport |
| Local loopback `Agent` that proxies to the remote | Does not meet: hides the boundary behind a trusted-looking Agent and inherits its grants | Partial | Partial: the proxy runtime holds the secret | Lower API surface, higher hidden risk |
| `externalUrl` field on `Agent` | Does not meet: every Agent consumer must handle a non-running agent | Partial | Partial | Breaks scheduling, probes and scaling assumptions |
| Treat the remote as a `Tool` | Partial: tool grants fit, but a tool call is not an agent task with cancel and handles | Partial | Meets | Misleading semantics for delegation and cancel |
| Open endpoint and trust the fetched Agent Card | Does not meet: a swapped card changes capability and credentials target | Meets | Does not meet | Lowest setup cost |
| Add mTLS, OAuth and card refresh in v1 | Meets more | Meets | Meets more | Large first release; delays value and review |

## Proposed decision

1. Introduce a namespaced `ExternalAgent` kind with a required `protocol` field.
   v1 accepts `A2A` only; MCP remains a `Tool`.
2. v1 uses a static `host:port` endpoint over TLS, a pinned Agent Card digest
   (fail closed on mismatch), a bearer credential through `SecretRef`, finite
   limits, and an operator egress allow-list enforced by admission and the
   transport. NetworkPolicy additionally limits non-443 allow-listed ports to
   public addresses; port 443 is already open to the controller for the
   Kubernetes API and CPI, so it relies on the admission and transport checks.
3. Referencing and invoking an `ExternalAgent` needs an explicit grant,
   re-checked at each send. Cross-namespace use needs a `CrossNamespaceGrant`.
4. A client-only A2A transport implements the existing `driver.Transport`.
   Remote task handles are persisted before observation. An ambiguous send is
   never resent. Cancel maps to the remote cancel call.
5. Only the declared step input is sent, within a byte limit. The reply is
   untrusted data and is never executed, used as an instruction to the platform,
   or used to widen permissions.
6. Usage is charged from the remote's `kognition.dev/usage` when present, capped
   at the allowance, and otherwise at the full allowance.
7. mTLS, OAuth and token exchange, card refresh and signature verification, data
   classification, long-running tasks and additional protocols are later,
   separately approved work ([EPIC-0024](../../BACKLOG.md)).

## Consequences

- Positive: the trust boundary is one explicit resource with one grant, one
  allow-list and one audit trail; later security features attach to it without
  changing the Workflow API.
- Positive: no new listener, receiver or identity issuer.
- Negative: v1 has no mutual authentication, automatic rotation or card refresh.
  A compromised bearer token or partner endpoint is limited to the declared limits
  but is not otherwise detected.
- Negative: external usage is self-reported or fully charged; cost can be
  overstated.
- Negative: an uncertain remote task blocks that member until its handle
  resolves or the timeout ends.
- Compatibility: existing collaboration definitions are unchanged. The feature is
  disabled by default behind a Helm flag.

## Acceptance and follow-up

Before activation: executable evidence for SSRF and rebinding refusal, card
mismatch refusal, secret non-disclosure, grant revocation mid-run, restart without
resend, hostile reply handling and usage capping, across all four strategies and
one live interop run (see the plan's test strategy). Independent security
approval is separate.
