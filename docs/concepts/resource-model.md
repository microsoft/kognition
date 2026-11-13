---
title: Resource model
description: Distinguish configuration, execution, authorization and observed state.
---

# Resource model

Kognition resources use `apiVersion`, `kind`, `metadata`, `spec` and `status`.
Authors declare desired state in `spec`; the control plane records observations
and execution results in `status`.

```yaml
apiVersion: kognition.dev/v1alpha1
kind: Agent
metadata:
  name: example-agent
  namespace: team-a
spec:
  modelRef:
    name: approved-chat-model
```

This fragment illustrates a model reference. It is not a complete installation,
provider binding or authorization policy.

## Configuration and execution

| Resource | Role |
| --- | --- |
| Agent | Describes an agent's model, instructions, runtime and optional capabilities. |
| AgentInstance | Represents an agent execution instance and its session state. |
| AgentJob | Coordinates requested agent work and its execution attempts. |
| Workflow | Defines orchestration steps and dependencies. |
| WorkflowRun | Records a particular workflow execution. |
| Model and ModelClass | Bind a configured model and classify its provider capabilities. |
| Prompt | Stores reusable instructions referenced by workloads. |

An Agent definition is not itself proof of a successful invocation. An accepted
resource write, a healthy runtime, an approved action and a completed request are
different observations.

## Capabilities and authorization

- **Models:** Model references select a model; ModelGrant resources govern access.
- **Tools:** ToolClass and Tool resources describe capabilities; grants and
  policies control who can invoke them.
- **Memory:** classes, claims and volumes describe storage; grants and gateway
  capabilities govern access.
- **Knowledge:** classes, bases, claims and index resources describe ingestion and
  retrieval configuration and execution.
- **Identity and governance:** AgentIdentity, roles, bindings, communication
  policies, quotas and approval resources constrain workloads.

A reference does not automatically grant access. Runtime authentication,
capability scope and provider credential isolation remain separate concerns.

## Reconciliation and status

Controllers repeatedly compare desired and observed state. Some operations
complete asynchronously or depend on an external provider.

When diagnosing a problem:

1. Confirm the selected backend and namespace.
2. Inspect the resource's status and conditions.
3. Inspect related execution resources and events.
4. Check the relevant grant, policy and provider configuration.
5. Preserve failed attempts before applying a recovery procedure.

Do not replace immutable execution evidence or strip finalizers to make a status
look healthy.

## Namespaces and scope

Namespaced resources belong to a workload namespace. Cluster-scoped classes
provide shared configuration. Cross-namespace access needs the documented
authorization path; matching names do not establish a trust relationship.

Use the [canonical Go registrations](../../api/v1alpha1/register.go) and
[Kubernetes API definitions](../../api/k8s/v1alpha1) for the current resource
surface. The [resource interaction map](../resource-interactions.md) provides
reviewed relationships; its recorded scope and date apply.

Continue with [First agent](../getting-started/first-agent.md), or use the
[reference index](../reference/README.md) for a specific subsystem.
