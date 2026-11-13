# Kognition quickstart

Choose a deployment path, install the platform, then configure a first Agent.

| Path | Guide |
| --- | --- |
| Local development | [Standalone setup](docs/getting-started/local.md). |
| Kubernetes | [Kubernetes setup](docs/getting-started/kubernetes.md). |
| Azure Kubernetes Service | [AKS setup](docs/getting-started/aks.md). |

Kubernetes bootstrap ends at verified inactive admission; it does not start the
controller. Workloads require separately approved activation with the documented
acceptance evidence. Production activation remains blocked by its outstanding
gates. The installer does not provision providers or install samples.

After installation, follow [First agent](docs/getting-started/first-agent.md) to
declare the Agent and model grant, invoke a turn and inspect the result.

For a specific capability, use [Guides and samples](docs/guides/README.md).
Use [Operations](docs/operations/README.md) for diagnostics, upgrades and recovery.
