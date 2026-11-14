# Kognition quickstart

Choose a deployment path, install the platform, then configure a first Agent.

| Path | Guide |
| --- | --- |
| Local development | [Standalone setup](docs/getting-started/local.md). |
| Kubernetes | [Kubernetes setup](docs/getting-started/kubernetes.md). |
| Azure Kubernetes Service | [AKS setup](docs/getting-started/aks.md). |

The Kubernetes fresh installer installs and verifies admission and database
components before starting the controller. It refuses to overwrite an existing
release and does not provision model providers or install samples.

After installation, follow [First agent](docs/getting-started/first-agent.md) to
declare the Agent and model grant, invoke a turn and inspect the result.

For a specific capability, use [Guides and samples](docs/guides/README.md).
Use [Operations](docs/operations/README.md) for diagnostics, upgrades and recovery.
