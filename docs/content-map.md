---
title: Documentation map
description: See which documents guide readers, define operational contracts or preserve development history.
---

# Documentation map

Kognition documentation has one authored source for each topic. The initial site
publishes a curated reader journey; detailed existing guides remain canonical
repository documents until individually integrated.

## Reader-facing content

| Section | Audience | Source and authority |
| --- | --- | --- |
| [Home](README.md) and [Overview](overview.md) | New readers | Purpose, maturity and deployment boundaries. |
| [Get started](getting-started/README.md) | Developers and operators | Local, Kubernetes and AKS installation paths, followed by a first-agent walkthrough. |
| [Resource model](concepts/resource-model.md) | Agent authors and integrators | Configuration, execution, authorization and observed state. |
| [Guides](guides/README.md) | Capability users | Curated links to maintained subsystem guides and samples. |
| [Video tutorial](guides/video-tutorial.md) | Agent authors | Dated Studio walkthrough with captions, chapter navigation and a searchable transcript. Current operational contracts remain authoritative. |
| [Operations](operations/README.md) | Operators | Index of current installation, activation, recovery and diagnostic contracts. |
| [Reference](reference/README.md) | Implementers | CLI behavior and exact API/configuration sources. |
| [Contribution policy](../CONTRIBUTING.md#documentation) | Contributors | Authoring and content validation. |

## Material with a distinct authority

| Material | How to use it |
| --- | --- |
| [Installation and recovery](features/knowledge-provider-execution/installation-recovery-contract.md), [bootstrap verification](features/knowledge-provider-execution/bootstrap-verification-contract.md), [activation](features/knowledge-provider-execution/activation-transaction-contract.md) and [drain verification](features/knowledge-provider-execution/drain-verification-contract.md) | Authoritative installation, activation, recovery and enforcement contracts. Getting-started guides explain these boundaries without replacing their evidence or approval requirements. |
| [Architecture references](architecture/reference/README.md) and [ADRs](adr/README.md) | Design structure and decision rationale; read status and scope. |
| [Testing](testing.md) and [development gates](development-gates.md) | Contributor validation requirements. |
| [Development plans and review records](development-plans/README.md) | Historical implementation context and scoped evidence, not current product certification. |
| [Project specification](../PROJECT.md) and [backlog](../BACKLOG.md) | Intended design and work tracking, not a shipped-feature checklist. |
| [Legacy tutorial](../TUTORIAL.md) and [quickstart](../QUICKSTART.md) | Existing links remain valid; prefer the new reader paths and current operational contracts. |

## Publication boundary

The website's explicit page list controls publication. Adding a Markdown file
somewhere in the repository does not publish it.

- Keep ignored release-preparation material, local evidence and credentials out
  of both the site and its build artifacts.
- Keep development records out of the default product navigation.
- Use repository links for source files and guides not yet published on the site.
- Preserve existing paths when adding navigation. Split a long document only
  with reviewed links or compatibility pointers.
- Do not add analytics, external search services or embedded tracking by default.
- Tutorial media comes from checksum-pinned Release assets during trusted
  publication builds. It is served by the same Pages site, with the site's
  existing access controls; the MP4 is not stored in Git history.
