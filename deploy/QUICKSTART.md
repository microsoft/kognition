# Deployment reference

Select the guide for the deployment you intend to run:

| Deployment | Guide | Completion check |
| --- | --- | --- |
| Standalone local development | [Standalone setup](../docs/getting-started/local.md) | Authenticated API access and successful database migration. |
| New Kubernetes installation | [Kubernetes setup](../docs/getting-started/kubernetes.md) | Admission, migrations and platform services pass checks; the controller rollout is ready. |
| New Azure Kubernetes Service installation | [AKS setup](../docs/getting-started/aks.md) | The same fresh-install checks complete using the selected AKS kubeconfig and registry. |
| Existing release, upgrade or partial installation | [Installation and recovery](../docs/features/knowledge-provider-execution/installation-recovery-contract.md) | The reviewed transition and applicable activation/recovery procedure complete. |
| Manually controlled Argo CD deployment | [Guarded GitOps](GITOPS.md) | The documented manual sync, verification and activation procedure completes. |

## New Kubernetes releases

[bootstrap.ps1](../bootstrap.ps1), `make quickstart`, `make.ps1 quickstart` and the
[AKS helper](aks/Install-RuntimeBase.ps1) use the shared fresh installer. It checks
release absence, captures the target and artifacts, installs admission and database
components, verifies their readiness and security behavior, then starts the
controller and checks its rollout.

The temporary zero-replica controller stage is part of startup, not the expected
finished result. Existing release history or conflicting workloads are refused;
the fresh path does not reset or adopt an installation.

Prepare cluster dependencies and image availability before running it. Providers,
credentials for inference and sample workloads are configured separately.
Follow [Kubernetes setup](../docs/getting-started/kubernetes.md) for executable
commands and configuration.

## Standalone mode

The standalone mode exposes the first-party API server instead of Kubernetes CRDs.
Use [Standalone Compose](../docs/standalone-compose.md) for credential generation,
database migration, health checks and runtime limitations.

Standalone Helm deployment is a separate advanced configuration. Its chart
settings are in [values.yaml](helm/kognition/values.yaml); configure API
authentication, TLS, networking and persistent data according to the
[security guide](../docs/security.md). A healthy API does not prove a provider
invocation succeeds.

## Images and chart configuration

- [Root Dockerfile](../Dockerfile): Go control-plane and gateway image targets.
- [Agent runtime Dockerfile](../runtime/agent/Dockerfile): native and optional
  AutoGen profiles.
- [Ingester Dockerfile](../runtime/ingester/Dockerfile): KnowledgeBase ingestion.
- [Chart values](helm/kognition/values.yaml): component, credential and network
  configuration.
- [Distribution notice preparation](../tools/distribution_notices.py): legal
  files retained when packaging charts and other artifacts.

Use the image version and digests for the chart you are installing. Publish or
load images through the target cluster's normal registry process; do not infer
availability from a successful local build.

## Samples and observability

After installation, follow [First agent](../docs/getting-started/first-agent.md)
before combining capabilities. Consult the [sample matrix](../docs/sample-matrix.md)
for dependencies and dated validation scope.

[Observability](../docs/observability.md) documents metrics and opt-in tracing.
Configure installation-scoped observability changes through the supported
deployment workflow; do not apply an unrelated Helm upgrade over an active
Kubernetes release.

## Upgrades and recovery

Retain the original installation state, chart and executable. Review existing work,
database backups, credentials and retention requirements before changing a release.
Use the [installation/recovery contract](../docs/features/knowledge-provider-execution/installation-recovery-contract.md),
[drain procedure](../docs/features/knowledge-provider-execution/drain-verification-contract.md)
and applicable [activation transaction](../docs/features/knowledge-provider-execution/activation-transaction-contract.md).

Do not delete persistent volumes or force finalizers to turn an uncertain
installation into a successful-looking retry.
