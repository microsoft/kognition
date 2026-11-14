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
| Install on Kubernetes | [Kubernetes setup](getting-started/kubernetes.md) | Install the platform and verify controller readiness. |
| Install on Azure Kubernetes Service | [AKS setup](getting-started/aks.md) | Use the same fresh-install workflow with AKS authentication and registry configuration. |
| Run an agent on a prepared installation | [First agent](getting-started/first-agent.md) | Declare an Agent and grant, invoke it and inspect the result. |
| Build an integration | [Guides and samples](guides/README.md) | Find the maintained guide and sample for a specific capability. |
| Operate or troubleshoot a deployment | [Operations](operations/README.md) | Find security, recovery, observability and diagnostic guidance. |
| Contribute | [Contributing](../CONTRIBUTING.md) | Build, test and submit a scoped change. |

## Learn the model

Read [the resource model](concepts/resource-model.md) before combining capabilities.
An Agent definition, a running instance and a successful invocation are different
objects and observations. A ready control plane does not prove that a provider,
grant or workload is configured correctly.

## Command shells

Run examples from the repository root unless a guide says otherwise. Choose
one shell's block for each step; alternatives perform the same operation.

| Environment | Command conventions |
| --- | --- |
| Linux/macOS with Bash | `./bin/kognitionctl`, `python3`, `export NAME=value`, and `\` for line continuation. Bash arrays use `"${target[@]}"`; run these blocks in Bash even when your login shell is zsh. |
| Windows with PowerShell 7 | `.\bin\kognitionctl.exe`, `python`, `$env:NAME = 'value'`, and a backtick for line continuation. PowerShell arrays use `@target`. |
| Shared helpers | `.ps1` helpers require [PowerShell 7](https://learn.microsoft.com/powershell/scripting/install/installing-powershell) on every OS. From Bash, use `pwsh -NoProfile -File ./path/to/helper.ps1`; script parameters still use a single hyphen. |

Use GNU Make for `make` examples. macOS users can use the installed GNU Make or
`gmake` when their package manager installs that name. Sample installers also
require `powershell-yaml`; install the pinned module in PowerShell 7:

```powershell
Install-Module powershell-yaml -RequiredVersion 0.4.12 -Scope CurrentUser
```

**Bash (Linux/macOS):**

```bash
pwsh -NoProfile -Command 'Install-Module powershell-yaml -RequiredVersion 0.4.12 -Scope CurrentUser'
```

Commands labeled `sh` contain no shell-specific syntax and work in Bash and
PowerShell. Quoted placeholders such as `'<resource-group>'` must be replaced
before execution. Keep kubeconfigs, environment files and captured installation
state private on every platform.

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
