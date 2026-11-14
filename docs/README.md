---
title: Kognition documentation
description: Learn the resource model, choose a deployment mode, and operate Kognition within its documented boundaries.
---

# Kognition documentation

Kognition is a declarative orchestration platform for AI agents. Declare agents,
models, tools, memory, knowledge and governance policies as resources; the control
plane reconciles their desired state and records execution status.

Kognition is under active development. APIs and deployment contracts can change
before a stable release. Tests and dated acceptance records apply to their
recorded scope; they do not establish production readiness.

## Start with your goal

| Your goal | Start here | Expected outcome |
| --- | --- | --- |
| Understand whether Kognition fits | [Overview](overview.md) | Understand the platform boundary and deployment options. |
| Explore a local control plane | [Local development](getting-started/local.md) | Prepare an authenticated standalone development installation. |
| Prepare a Kubernetes installation | [Kubernetes setup](getting-started/kubernetes.md) | Understand inactive bootstrap and separately approved activation. |
| Prepare an Azure Kubernetes Service installation | [AKS setup](getting-started/aks.md) | Review AKS prerequisites and the installer's authentication constraints. |
| Run an agent on a prepared installation | [First agent](getting-started/first-agent.md) | Declare an Agent and grant, invoke it and inspect the result. |
| Watch a Studio walkthrough | [Video tutorial](guides/video-tutorial.md) | Build and govern a support assistant, with captions, chapters and a transcript. |
| Build an integration | [Guides and samples](guides/README.md) | Find the maintained guide and sample for a specific capability. |
| Operate or troubleshoot a deployment | [Operations](operations/README.md) | Find security, recovery, observability and diagnostic guidance. |
| Contribute | [Contributing](../CONTRIBUTING.md) | Build, test and submit a scoped change. |

## Learn the model

Read [the resource model](concepts/resource-model.md) before combining capabilities.
An Agent definition, a running instance and a successful invocation are different
objects and observations. A ready control plane does not prove that a provider,
grant or workload is configured correctly.

## Find exact behavior

- [CLI reference](reference/cli.md): explicit backend selection, authentication
  and common commands.
- [Reference index](reference/README.md): resource definitions, configuration and
  architecture sources.
- [Documentation map](content-map.md): current reader paths and the distinction
  between product guidance, implementation contracts and historical records.

Existing repository guides remain the source of detailed operational contracts.
Links to material outside the initial website open the corresponding repository
page. The site does not publish development records or local preparation files
automatically.
