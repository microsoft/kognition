---
title: Choose your starting path
description: Select local development or Kubernetes setup before running a first agent.
---

# Choose your starting path

Start with an explicitly selected environment. The local development path and the
guarded Kubernetes path have different prerequisites and approval boundaries.

| Path | Prepare | Continue when |
| --- | --- | --- |
| [Local development](local.md) | Docker Compose, local credentials, database migrations and the CLI. | The local API is healthy and authenticated resource access works. |
| [Kubernetes setup](kubernetes.md) | A reviewed static kubeconfig, available images, chart artifacts and the installation plan. | Inactive bootstrap and separately approved activation have completed for the exact target. |
| [AKS setup](aks.md) | AKS access, a supported static kubeconfig, Cilium and registry access for the nodes. | The same installation and separate activation requirements have been met. |

If you already have access to a prepared installation and an approved model,
continue to [First agent](first-agent.md).

## Prerequisites by task

| Task | Tools |
| --- | --- |
| Build the CLI or Go services | The Go version declared in [go.mod](../../go.mod). |
| Run the local development stack | Docker with Compose, and PowerShell 7 for the documented credential preparation. |
| Operate Kubernetes mode | Compatible kubectl and Helm, an explicit reviewed kubeconfig, and the prerequisites in the installation contract. |
| Work on the Python runtimes | Python and the relevant runtime dependency manifests. |
| Build protobuf bindings | Buf and the repository generator configuration. |
| Build the documentation | Node.js 24 and npm; see [Contributing](../../CONTRIBUTING.md#documentation). |

## Before making a provider call

- Confirm the target installation and workload namespace.
- Use a provider binding and credentials approved for that environment.
- Confirm which data leaves the deployment and which provider receives it.
- Review expected cost and the applicable grants and quotas.
- Keep secrets out of manifests committed to Git, shell arguments and shared logs.

Review infrastructure and provider costs before provisioning resources or making
provider calls.
