---
title: Guides and samples
description: Find task-oriented guidance and the source material for each capability.
---

# Guides and samples

Complete [First agent](../getting-started/first-agent.md) before combining
capabilities. Existing detailed guides and samples remain in the repository;
their links open the canonical source rather than a second maintained copy.

| Task | Guide | Sample or reference |
| --- | --- | --- |
| Watch an end-to-end Studio example | [Build, govern and verify an assistant](video-tutorial.md) | Captioned video, chapters, transcript and related written guides. |
| Build and inspect resources visually | [Kognition Studio](../studio.md) | [CLI reference](../reference/cli.md). |
| Add model invocation behavior | [Model invocation profiles](../model-invocation-profiles.md) | [Foundry Agent sample](../../samples/foundry-agent/manifests.yaml). |
| Choose a runtime binding | [Runtime binding profiles](../runtime-binding-profiles.md) | [Standalone runtime limits](../standalone-compose.md#opt-in-http-runtime). |
| Ground an agent in documents | [Knowledge bases](../knowledge-base.md) and [providers](../knowledge-providers.md) | [Knowledge base sample](../../samples/knowledge-base/README.md). |
| Use persistent agent memory | [Memory walkthrough](../../samples/memory-in-action/README.md) | [Memory types](../../samples/memory-all-types/README.md). |
| Add tools | [Tools walkthrough](../../samples/tools-in-action/README.md) | [Workflow tool sandbox](../../samples/workflows/README.md). |
| Orchestrate work | [Workflow samples](../../samples/workflows/README.md) | [Multi-agent workflow](../../samples/multi-agent-workflow/manifests.yaml). |
| Run a team of agents in one step | [Workflow collaboration](../workflow-collaboration.md) | [Collaboration sample](../../samples/workflows/collaboration.yaml). |
| Apply governance | [Governance examples](../../samples/governance/README.md) | [Security guidance](../security.md). |
| Evaluate an agent | [Evaluation sample](../../samples/evaluation/README.md) | [Resource reference](../reference/README.md). |

## Review a sample before applying it

- Verify its provider endpoints, model names, namespace and required credentials.
- Read the [sample matrix](../sample-matrix.md) for its installation profile and
  dated validation scope.
- In Kubernetes mode, use a running installation and one of its enrolled workload
  namespaces. Do not install samples during an upgrade or recovery operation.
- A successful ingestion or ready Agent does not prove retrieval or a complete
  workflow succeeded.
- Keep any failed execution evidence. Avoid repeating a paid invocation while
  its result or accounting is uncertain.

The [legacy tutorial](../../TUTORIAL.md) remains available as a broad resource
walkthrough. Where it differs from an operational contract, follow the current
installation, activation, runtime and security guidance.
