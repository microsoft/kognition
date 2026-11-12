# ADR-0010: Stdio Tool Invocation Sandbox

- **Status:** Accepted
- **Date:** 2026-08-24
- **Deciders:** Kognition core team

## Context

T-0212 still lacks direct stdio and Shell Tool invocation. Running a process
from the Tool Gateway or controller process would expose that workload's
credentials, filesystem, and network. Letting the Tool Gateway create
cross-namespace Jobs would also give a public-facing authorization component
broad workload-creation permission.

The existing `ToolClass.spec.sandboxImage` establishes a cluster-admin-owned
OCI image boundary for exec health probes. It does not identify a trusted
stdio server launch argv or provide a durable request lifecycle for tool
invocations. Both standalone and Kubernetes modes need the same class-owned
launch contract, resource limits, output limits, cancellation behavior, and
auditable result.

## Decision

Direct stdio invocation will use a durable `ToolInvocationRun` request that
the controller reconciles into a one-shot sandbox.

1. `ToolClass` will own both the OCI `sandboxImage` and a direct
   `stdioCommand` argv. A Tool and an invocation input cannot select either.
2. The Tool Gateway will authorize, quota-check, and durably record the
   invocation before creating a namespaced `ToolInvocationRun`. It will wait
   only for that request's terminal status and return its normalized output.
3. In Kubernetes mode, the controller will verify the Tool, ToolClass,
   resource version, and request ownership before creating a labelled Job.
   The Job will use the same non-root, no-token, no-egress, read-only,
   capability-dropped posture as exec health probes.
4. In standalone mode, the controller will reconcile the same request through
   the Docker Engine API using the same image, argv, UID, no-network, read-only
   root filesystem, capability, PID, CPU, memory, timeout, and bounded-output
   limits.
5. The sandbox process will receive and return newline-delimited MCP JSON-RPC
   over stdio. The runner will perform initialization, send `tools/call`,
   correlate the response ID, and reject oversized, malformed, or unrelated
   output.
6. Shell tools remain a stdio MCP-server use case. Agent input is passed as
   `tools/call` arguments to the class-owned server, never interpolated into a
   controller, gateway, Docker, or Kubernetes command line.

## Alternatives Considered

1. **Start stdio processes in the Tool Gateway.** Rejected because Tool input
   could execute in a network-facing service with credentials and broad
   access to protected resources.
2. **Give the Tool Gateway permission to create Jobs.** Rejected because a
   capability-verifying gateway would gain cross-namespace workload creation
   and cleanup authority, increasing the blast radius of a gateway defect.
3. **Use a caller-selected Pod or command.** Rejected because a Tool author
   could select a privileged workload or inject a launch command outside the
   class-owned trust boundary.

## Consequences

**Positive.**

- Both modes use a single request lifecycle and identical sandbox contract.
- The controller retains workload creation authority and validates the
  class-owned image and launcher before execution.
- Gateway authorization, quota, receipt, audit, cancellation, and result
  handling remain durable and observable.

**Negative.**

- A new request resource, reconciler, conversions, CRD, and status lifecycle
  are required.
- Each stdio call creates a short-lived sandbox, which has startup overhead.
- Interactive, long-lived stdio sessions remain out of scope for the first
  implementation.

## Related

- [ADR-0009: Tool exec probe sandbox](0009-tool-exec-probe-sandbox.md)
- [docs/security.md](../security.md)
- [BACKLOG.md](../../BACKLOG.md)