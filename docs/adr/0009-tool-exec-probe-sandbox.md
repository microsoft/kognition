# ADR-0009: Tool Exec Probe Sandbox

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** Kognition core team

## Context

`ToolHealthProbe.type: exec` already validates a direct argv and applies a
probe timeout, but production controller assemblies do not configure an
`ExecProbeRunner`. Executing the command in a controller process would grant a
Tool author command execution in the control-plane container. ToolClass only
describes an execution tier and resource limits; it currently does not identify
a trusted image or an execution target.

Kubernetes and standalone modes must preserve the same security boundary. An
exec health probe must execute in an OCI sandbox approved by the ToolClass, not
in the controller process and not in a user-selected arbitrary Pod.

## Decision

The first production implementation supports exec probes for ToolClasses with
`executionTier: pod` and a class-owned `sandboxImage` only.

1. The ToolClass administrator configures `sandboxImage`. A Tool cannot supply
   or override the image used for an exec probe.
2. Each probe starts a one-shot OCI sandbox with the ToolHealthProbe command as
   direct argv. Shell parsing, interactive TTYs, and inherited credentials are
   prohibited.
3. Kubernetes creates a short-lived Job in the Tool namespace. The Job runs
   without a service-account token, uses restrictive security settings, has an
   active deadline derived from the probe timeout, and is removed after the
   probe.
4. Standalone invokes Docker Engine to start an equivalent short-lived
   container with no network, a read-only root filesystem, dropped Linux
   capabilities, no-new-privileges, and limits derived from Tool sandbox
   configuration.
5. The controller accepts a probe result only from a sandbox it created for
   the exact Tool. Kubernetes verifies the generated Job labels before reading
   status. Standalone receives the process result directly from Docker.
6. `process`, `sidecar`, and `vm` execution tiers remain fail-closed for exec
   probes until they define equivalent trusted target resolvers. HTTP, TCP, and
   MCP ping probes retain their current behavior.

## Alternatives Considered

1. **Run argv in the controller process.** Rejected because it violates the
   Tool sandbox boundary and exposes the controller service account and local
   filesystem to Tool-controlled commands.
2. **Exec into a user-selected Pod or container.** Rejected because a Tool
   author could target a privileged or unrelated workload. A selector alone
   cannot provide the required ownership and UID guarantees.
3. **Use a long-lived sandbox target recorded in Tool status.** Rejected for
   the initial slice because target replacement, stale UID handling, and
   lifecycle cleanup add state without benefiting periodic health probes.

## Consequences

**Positive.**

- Exec probes preserve OCI isolation in standalone and Kubernetes modes.
- ToolClass administrators approve the executable image.
- Probe execution has a deterministic lifetime and cleanup path.
- Direct argv and restrictive security context reduce command injection and
  privilege exposure.

**Negative.**

- Each probe creates a short-lived container or Job.
- Standalone exec probes require Docker Engine availability.
- Existing ToolClasses need an explicit `sandboxImage` before exec probes can
  become Ready.
- Additional execution tiers require their own trusted target designs.

## Related

- [ADR-0007: Agent Gateway fabric](0007-agent-gateway-fabric.md)
- [docs/security.md](../security.md)
- [BACKLOG.md](../../BACKLOG.md)