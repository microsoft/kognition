# ADR-0008: Model Gateway Invocation Boundary

- **Status:** Accepted
- **Date:** 2026-08-20
- **Deciders:** Kognition core team

## Context

Kubernetes agent runtimes currently receive a Model provider endpoint and
credential environment variables from the dispatcher. The runtime creates a
provider-specific LangChain client and calls the provider directly. Standalone
chat checks `ModelGrant` and `AgentPolicy` before it dispatches through a CPI
driver, while Kubernetes checks them only before the AgentInstance is
dispatched. A grant or policy change after dispatch can therefore leave an
already-running runtime with direct provider access.

The Tool Gateway and memory policy gateway establish the required data-plane
pattern: short-lived capabilities, live policy evaluation at the execution
boundary, durable results, and no direct target credential in runtime Pods.
Model inference needs the same boundary, but differs from Tool invocation:
model calls are multi-turn, selected from the Agent's declared Model rather
than after a tool-selection exchange, and provider protocols vary.

## Decision

Kognition will add a shared Model Gateway core. Standalone paths call the core
in process. Kubernetes runtimes call a Model Gateway HTTP service and do not
receive provider endpoints or provider credentials when the gateway is
enabled.

1. The dispatcher issues a short-lived Model session capability for the active
   namespace, Agent, automatic AgentIdentity, AgentInstance UID, session, and
   exact Model resource version. The capability has a distinct JWT audience
   from Tool and memory capabilities.
2. The runtime receives the Model Gateway URL, logical Model ID, and the
   per-session capability in the authenticated `/invoke` body. Session values
   do not enter the shared Deployment Pod template.
3. The Model Gateway verifies the capability and live AgentIdentity and
   AgentInstance session, loads the live Model revision, and evaluates
   `ModelGrant` plus enforced `AgentPolicy` immediately before each provider
   request. Revocation therefore applies to an active runtime session.
4. The gateway owns upstream credential resolution and provider access. Runtime
   Pods receive neither the provider endpoint nor credential material in
   gateway mode. Model session capabilities are non-transferable to another
   Agent, instance, session, or Model revision.
5. Gateway responses use the provider's OpenAI-compatible chat-completions
   contract for the first implementation slice. The gateway preserves safe
   provider error classes and usage values, while trace and audit attributes
   attach Kognition workload and Model identity.
6. The first slice supports only `openai-compatible` chat Models with an
   API-key `SecretRef`. Azure/Foundry token exchange, Anthropic, Ollama,
   Bedrock, embeddings, provider-specific streaming, global rate limiting,
   durable model receipts, and cost settlement are subsequent slices behind
   this boundary.

## Alternatives Considered

1. **Keep direct runtime-to-provider calls.** Rejected because policy and grant
   revocation cannot be enforced at inference time and provider credentials
   remain present in agent Pods.
2. **Reuse the Tool Gateway protocol.** Rejected because Tool capabilities are
   exact single-invocation grants after model tool selection. Model inference
   needs a session-bound capability and a provider request contract.
3. **Use a generic external AI proxy as the authorization boundary.** Rejected
   because it cannot verify Kognition AgentInstance identity, Model revision,
   `ModelGrant`, or `AgentPolicy`. It may be an upstream provider target behind
   the Model Gateway.

## Consequences

**Positive.**

- Kubernetes and standalone inference use one authorization and execution
  contract.
- A live policy change can revoke model access before the next inference turn.
- Provider endpoints and credentials leave agent runtime Pods in gateway mode.
- Model traces, usage, audit, and later cost settlement gain one ownership
  point.

**Negative.**

- Kubernetes inference adds a network hop.
- The initial OpenAI-compatible adapter does not cover every declared provider.
- The Model Gateway needs its own deployment lifecycle, key rotation, RBAC,
  resilience policy, and conformance suite.

## Related

- [ADR-0005: Secretless outbound provider auth](0005-secretless-provider-auth.md)
- [ADR-0007: Agent Gateway fabric](0007-agent-gateway-fabric.md)
- [docs/security.md](../security.md)