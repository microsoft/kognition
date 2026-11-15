---
title: Overview
description: Understand Kognition's purpose, deployment modes and current limitations.
---

# Overview

Kognition provides a resource-oriented control plane for AI agents. Applications
declare what an agent may use and how work should execute. Controllers reconcile
that configuration, coordinate execution and expose status for inspection.

## When it fits

Consider Kognition when you need:

- Declarative Agent and Workflow configuration with observable lifecycle state.
- Explicit access to models, tools, memory and knowledge.
- Namespace boundaries, grants, quotas and approval policies around execution.
- A common resource model for standalone development and Kubernetes integration.

Kognition does not supply a model or eliminate the need to operate its
dependencies. Providers, credentials, data stores, networking and installation
approval remain operator responsibilities.

## Deployment modes

| Mode | Control plane | Appropriate starting point |
| --- | --- | --- |
| Standalone | Kognition HTTP API and controllers backed by PostgreSQL. | [Local development](getting-started/local.md). |
| Kubernetes | Kubernetes custom resources, admission webhooks and controllers. | [Kubernetes setup](getting-started/kubernetes.md). |

The modes share resource concepts but differ at infrastructure and runtime
boundaries. Do not infer complete deployment parity from a shared schema or a
passing unit suite. See the [standalone runtime limitations](standalone-compose.md#opt-in-http-runtime)
before choosing an external runtime.

## How work moves through the platform

1. An operator prepares provider bindings, credentials and access policies.
2. An author declares an Agent, its model reference and optional capabilities.
3. Admission validates the request against the configured policies.
4. Controllers reconcile resources and create or coordinate execution.
5. Runtime and gateway components enforce the relevant capability boundaries.
6. Resource status, events and configured observability expose the outcome.

Use the [resource model](concepts/resource-model.md) to distinguish configuration,
execution and authorization resources.

## Maturity and boundaries

- The project is under active development; compatibility can change.
- The Kubernetes installer verifies admission and database readiness before
  starting the controller. Existing releases use the separate upgrade/recovery
  procedure.
- Sample manifests illustrate configuration and may contain environment-specific
  bindings. Review and adapt them before use.
- Provider calls can incur charges and transmit inputs to the configured provider.
- OpenTelemetry trace export is disabled by default. Operators choose any export
  endpoint; Kognition does not configure a Microsoft trace destination.

For the architectural motivation, read the [Kognition manifest](kognition-manifest.md).
For current operational requirements, follow [Operations](operations/README.md).
