# ADR-0019: Runtime Adapter Transport

- Status: Accepted by workspace user on 2026-10-03.
- Traces: OPEN-DIFF-2, OPEN-RC-1, FR-0600, FR-0602, US-0042, T-0147, T-0148, T-0149, T-0150.

## Context

AutoGen adapter logic is tested only with a fake AgentClient. The user chose a
real Python HTTP bridge over a new generated gRPC worker integration.

## Options

| Option | Benefit | Cost or limitation |
| --- | --- | --- |
| Python HTTP bridge | Fits existing AgentClient boundary and deployment toolchain | Must implement authenticated lifecycle, bounded events and complete state recovery |
| Generated gRPC plugin worker | Fits the language-neutral host protocol | Broader SDK/worker transport work and deployment validation |

## Decision

Use the real AutoGen SDK in a dedicated Python worker and a Go HTTP AgentClient.
Bind sessions to the authenticated Agent/workspace and pass only reviewed runtime
configuration. No arbitrary per-request endpoint, executable, model credentials,
serialized component factory or tool code may become authority through the bridge.
Operator-owned configuration controls upstream models and tool implementations.

The bridge supports create, bounded event streaming, cancellation, health,
checkpoint and resume. Checkpoint state must preserve actual AutoGen history and
tool state; cancellation, credit backpressure and terminal outcomes must meet the
Runtime contract. Readiness requires the SDK and configured dependencies, not
only an HTTP listener. Non-loopback traffic requires verified TLS and scoped
authentication; credentials must not appear in payload logs or events.

The current native/Python runtime is preserved. runtimeClass selection, image
build/publish, both-mode wiring and real-worker conformance are part of completion.
Mock-provider tests exercise a real SDK worker and cannot be labeled live provider
acceptance. Do not claim a second production runtime from fake events alone.

## Verification

Test real SDK lifecycle/event translation, missing/wrong credentials, namespace
binding, invalid/oversized input, stream cancellation, busy checkpoints, fresh
worker state restore, tool-effect replay and request deadlines. Deployed acceptance
uses a reviewed isolated target; no automatic installation changes are authorized.
