---
title: Reference
description: Locate exact CLI, resource, configuration and architecture definitions.
---

# Reference

Use reference material for exact fields and configuration. Use
[getting started](../getting-started/README.md) for a learning sequence and
[Operations](../operations/README.md) for deployment procedures.

| Surface | Authoritative source |
| --- | --- |
| CLI usage and backend selection | [CLI reference](cli.md); use the built binary's `--help` for the installed revision. |
| Standalone API resources | [Go API definitions](../../api/v1alpha1) and [registrations](../../api/v1alpha1/register.go). |
| Kubernetes resources | [Kubernetes API definitions](../../api/k8s/v1alpha1) and [Helm CRDs](../../deploy/helm/kognition/crds). |
| Resource relationships | [Reviewed interaction map](../resource-interactions.md). |
| Helm configuration | [Chart values](../../deploy/helm/kognition/values.yaml). |
| Native and external runtime bindings | [Runtime binding profiles](../runtime-binding-profiles.md). |
| Provider invocation contracts | [Model invocation profiles](../model-invocation-profiles.md). |
| Knowledge provider behavior | [Knowledge providers](../knowledge-providers.md). |
| Metrics and tracing configuration | [Observability](../observability.md). |
| Architecture and rationale | [Reference architecture](../architecture/reference/README.md) and [ADRs](../adr/README.md). |

The API is currently `kognition.dev/v1alpha1`. Use the definitions shipped with
the installed revision rather than inferring a supported kind or field from an
older example. Schema compatibility does not establish runtime or deployment
parity.

## Specifications and historical records

[PROJECT.md](../../PROJECT.md) describes the project specification;
[BACKLOG.md](../../BACKLOG.md) tracks work. Neither is an assertion that every
described capability is shipped or qualified.

[Development plans and reviews](../development-plans/README.md) preserve
implementation context and scoped evidence. They are intentionally separate from
the website's default reader journey.
